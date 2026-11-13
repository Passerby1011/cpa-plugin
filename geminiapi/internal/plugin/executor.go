package plugin

// Execution: route a request by its model-id namespace, call the backend
// through the host transport, and stream back.
//
// # Cancellation, idle timeout, total timeout
//
// These are three DIFFERENT budgets and conflating them breaks real traffic:
//
//   - request-timeout: the whole request. Generous (30m by default), because an
//     AI Studio turn legitimately thinks for minutes.
//   - stream-idle-timeout: the gap between two CHUNKS. A productive stream is
//     never killed by this, however long it runs; a silently stalled one is
//     stopped long before the 30m total.
//   - client cancellation: when the downstream client disconnects, CPA cancels
//     the request context. That cancel must reach the backend, otherwise the
//     backend keeps a Google request (and an account slot) alive for nobody.
//     The host transport takes the context, so cancellation propagates for free
//     — this code must simply not replace it with context.Background().

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/geminiapi/internal/backend"
	"github.com/hex-ci/cpa-plugin/geminiapi/internal/catalog"
	"github.com/hex-ci/cpa-plugin/geminiapi/internal/config"
)

// executorRequest mirrors rpcExecutorRequest: the SDK embeds
// pluginapi.ExecutorRequest untagged, so its fields marshal under Go field
// names. StreamID is the DOWNSTREAM host-allocated id used for emissions.
type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID string `json:"stream_id,omitempty"`
}

// streamTimeout bounds one downstream emission / close, so a wedged consumer
// surfaces as an error instead of parking a producer goroutine.
var streamTimeout = 5 * time.Second

// handleExecute implements executor.execute (non-streaming).
func (m *Manager) handleExecute(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}
	return m.execute(req)
}

// handleExecuteStream implements executor.execute_stream.
func (m *Manager) handleExecuteStream(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}
	return m.executeStream(req)
}

// prepare resolves the model and the backend setup for a request.
func (m *Manager) prepare(ctx context.Context, req executorRequest) (catalog.Record, backend.Setup, int, error) {
	ns, bare, ok := catalog.ResolveNamespace(req.Model)
	if !ok {
		return catalog.Record{}, backend.Setup{}, http.StatusBadRequest, catalog.ModelIDError(req.Model)
	}
	if ns != config.NamespaceAIStudio {
		// gemini-web is its own card: this provider must not silently answer
		// for a namespace whose backend does not exist yet.
		return catalog.Record{}, backend.Setup{}, http.StatusBadRequest,
			fmt.Errorf("namespace %q is not served by this build; only %s is", ns, config.NamespaceAIStudio)
	}

	m.mu.RLock()
	cfg := m.cfg
	result := m.catalog
	mgr := m.backendMgr
	external := m.skipSupervision
	m.mu.RUnlock()

	rec, found := result.Resolve(bare)
	if !found {
		rec, found = result.Resolve(req.Model)
	}
	if !found {
		// A model the catalog does not know is refused with a clear reason.
		// Passing it through would send an id the backend may or may not
		// accept, and a wrong model on a paid account is worse than an error.
		return catalog.Record{}, backend.Setup{}, http.StatusNotFound,
			fmt.Errorf("model %q is not in the published catalog; refresh the model list on the plugin page, or check models.allow/deny", req.Model)
	}

	setup, err := m.setupFor(ctx, cfg, mgr, external)
	if err != nil {
		return catalog.Record{}, backend.Setup{}, http.StatusBadGateway, err
	}
	return rec, setup, 0, nil
}

// execute runs a non-streaming request.
func (m *Manager) execute(req executorRequest) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), m.requestTimeout())
	defer cancel()
	return m.executeWithContext(ctx, req)
}

// executeWithContext is the testable core: the caller owns the context so both
// the timeout path and the cancellation path can be exercised.
func (m *Manager) executeWithContext(ctx context.Context, req executorRequest) ([]byte, error) {
	rec, setup, status, err := m.prepare(ctx, req)
	if err != nil {
		return classEnvelope(status, err), nil
	}
	doer := m.httpDoer()
	if doer == nil {
		return classEnvelope(http.StatusBadGateway, fmt.Errorf("host transport is unavailable")), nil
	}

	body, err := m.rewriteBody(req, rec)
	if err != nil {
		return classEnvelope(http.StatusBadRequest, err), nil
	}
	resp, err := doer.Do(ctx, backend.HTTPRequest{
		Method:  http.MethodPost,
		URL:     setup.BaseURL + "/chat/completions",
		Headers: backendHeaders(setup.APIKey, req.Headers),
		Body:    body,
	})
	if err != nil {
		if ctx.Err() != nil {
			// The client went away (or the total budget lapsed): nothing to
			// retry, and the upstream call is already cancelled by ctx.
			return classEnvelope(499, fmt.Errorf("request cancelled before the backend answered")), nil
		}
		return classEnvelope(http.StatusBadGateway, fmt.Errorf("the local backend could not be reached: %s", redact(err))), nil
	}
	if resp.StatusCode >= 400 {
		return upstreamErrorEnvelope(resp.StatusCode, resp.Body, setup), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: resp.Body,
		Headers: responseHeaders(resp.Headers),
	}), nil
}

// executeStream runs a streaming request.
//
// Pre-first-byte failures (bad model, unreachable backend, upstream HTTP >= 400)
// return as error envelopes so CPA can fail over before anything was emitted.
// On success the pump runs in a goroutine and this returns immediately, so the
// host can start draining downstream without a buffer deadlock.
func (m *Manager) executeStream(req executorRequest) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), m.requestTimeout())
	rec, setup, status, err := m.prepare(ctx, req)
	if err != nil {
		cancel()
		return classEnvelope(status, err), nil
	}
	doer := m.httpDoer()
	if doer == nil {
		cancel()
		return classEnvelope(http.StatusBadGateway, fmt.Errorf("host transport is unavailable")), nil
	}
	bridge, okBridge := doer.(*HostBridge)
	if !okBridge || bridge == nil {
		cancel()
		return classEnvelope(http.StatusBadGateway, fmt.Errorf("host streaming transport is unavailable")), nil
	}
	body, err := m.rewriteBody(req, rec)
	if err != nil {
		cancel()
		return classEnvelope(http.StatusBadRequest, err), nil
	}

	stream, err := bridge.DoStream(ctx, backend.HTTPRequest{
		Method:  http.MethodPost,
		URL:     setup.BaseURL + "/chat/completions",
		Headers: backendHeaders(setup.APIKey, req.Headers),
		Body:    body,
	})
	if err != nil {
		cancel()
		if ctx.Err() != nil {
			return classEnvelope(499, fmt.Errorf("request cancelled before the backend answered")), nil
		}
		return classEnvelope(http.StatusBadGateway, fmt.Errorf("the local backend could not be reached: %s", redact(err))), nil
	}
	if stream.StatusCode >= 400 {
		cancel()
		var errBody []byte
		if stream.ID != "" {
			payload, errMsg, _, readErr := bridge.StreamRead(stream.ID)
			if readErr == nil {
				if len(payload) > 0 {
					errBody = payload
				} else if errMsg != "" {
					errBody = []byte(errMsg)
				}
			}
			_ = bridge.StreamClose(stream.ID)
		}
		return upstreamErrorEnvelope(stream.StatusCode, errBody, setup), nil
	}

	// Success: the pump owns the context deadline from here on.
	go m.pumpStream(ctx, cancel, bridge, req.StreamID, stream.ID, req.Model, rec.UpstreamID)
	return okEnvelope(struct{}{}), nil
}

// pumpStream reads the upstream stream and forwards it downstream until the
// backend closes, the idle guard trips, the total deadline lapses, or the
// client disconnects.
//
// Every exit path closes BOTH streams, exactly once: a leaked upstream stream
// holds a backend worker, and a leaked downstream stream hangs the client.
func (m *Manager) pumpStream(ctx context.Context, cancel context.CancelFunc, bridge *HostBridge, downstreamID, upstreamID, publicID, upstreamModel string) {
	defer cancel()
	if m.bridge != nil {
		m.bridge.inFlight.Add(1)
		defer m.bridge.inFlight.Done()
	}

	var closeOnce sync.Once
	closeAll := func(errMsg string) {
		closeOnce.Do(func() {
			_ = bridge.StreamClose(upstreamID)
			_ = bridge.CloseDownstream(downstreamID, errMsg)
		})
	}
	defer closeAll("")

	idle := newIdleGuard(m.streamIdleTimeout(), func() {
		// Tripping the guard closes the upstream stream, which unblocks the
		// read loop that is parked inside the host.
		_ = bridge.StreamClose(upstreamID)
	})
	defer idle.Stop()

	firstChunk := true
	for {
		payload, errMsg, done, err := bridge.StreamRead(upstreamID)
		// Any return from a read is progress: a chunk, a clean close, or a
		// transport error all reset the silence budget.
		idle.Rearm()

		if idle.Tripped() {
			// Reported as a retryable failure so the client's own SDK backs off,
			// rather than this proxy replaying a request the backend may still
			// be working on.
			closeAll(fmt.Sprintf("upstream stream idle timeout after %s", m.streamIdleTimeout()))
			return
		}
		if ctx.Err() != nil {
			closeAll("client cancelled the request")
			return
		}
		if err != nil {
			closeAll(redact(err))
			return
		}
		if errMsg != "" {
			closeAll(truncate(errMsg, 240))
			return
		}
		if done {
			closeAll("")
			return
		}
		if len(payload) == 0 {
			continue
		}
		if firstChunk {
			firstChunk = false
			// The backend may echo the model we sent (the bare upstream id).
			// The client asked for the namespaced id, so rewrite the first
			// chunks' model field back to what it requested. Only the model
			// field is touched; the SSE framing is passed through verbatim.
			payload = rewriteStreamModel(payload, upstreamModel, publicID)
		}
		if emitErr := bridge.Emit(downstreamID, payload); emitErr != nil {
			// The consumer is gone; stop reading rather than buffering a
			// response nobody will receive.
			closeAll(redact(emitErr))
			return
		}
	}
}

// rewriteStreamModel rewrites "model":"<upstream>" to the public id inside an
// SSE chunk. It is deliberately conservative: a payload that is not the shape
// it expects is returned unchanged, because dropping a chunk would corrupt the
// response.
func rewriteStreamModel(payload []byte, upstreamID, publicID string) []byte {
	if upstreamID == "" || publicID == "" || upstreamID == publicID {
		return payload
	}
	needle := `"model":"` + upstreamID + `"`
	if !strings.Contains(string(payload), needle) {
		return payload
	}
	return []byte(strings.ReplaceAll(string(payload), needle, `"model":"`+publicID+`"`))
}

// streamIdleTimeout reads the independent idle budget.
func (m *Manager) streamIdleTimeout() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cfg.StreamIdleTimeout > 0 {
		return m.cfg.StreamIdleTimeout
	}
	return config.DefaultStreamIdleTimeout
}

// rewriteBody replaces the client's model id with the backend's own id.
//
// The published id carries the namespace, the backend expects the bare model.
// Everything else in the body is forwarded untouched: the backend is
// OpenAI-compatible, so translating it would only lose fields this plugin does
// not know about.
func (m *Manager) rewriteBody(req executorRequest, rec catalog.Record) ([]byte, error) {
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("the request carries no body")
	}
	out, err := rewriteModelID(body, rec.UpstreamID)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// backendHeaders builds the outbound headers: the backend's key, a JSON
// content type, and nothing else from the inbound request except fields that
// matter (a client's own Authorization must NOT be forwarded — it is CPA's key,
// not the backend's).
func backendHeaders(apiKey string, inbound http.Header) http.Header {
	out := http.Header{}
	out.Set("Content-Type", "application/json")
	out.Set("Accept", "application/json")
	if apiKey != "" {
		out.Set("Authorization", "Bearer "+apiKey)
	}
	// OpenAI's optional tracing / idempotency hints are worth passing through.
	for _, name := range []string{"X-Request-Id", "OpenAI-Beta", "Idempotency-Key"} {
		if value := inbound.Get(name); value != "" {
			out.Set(name, value)
		}
	}
	return out
}

// responseHeaders keeps only headers a client may see.
func responseHeaders(in http.Header) http.Header {
	out := http.Header{}
	for _, name := range []string{"Content-Type", "X-Request-Id"} {
		if value := in.Get(name); value != "" {
			out.Set(name, value)
		}
	}
	return out
}

// upstreamErrorEnvelope turns a backend error into a client-facing envelope.
// The backend's own message is included because it is about the request (a bad
// model name, an exhausted quota), never about a credential.
func upstreamErrorEnvelope(status int, body []byte, setup backend.Setup) []byte {
	message := upstreamErrorMessage(body)
	if message == "" {
		message = fmt.Sprintf("the local backend returned HTTP %d", status)
	}
	return classEnvelope(status, fmt.Errorf("%s", message))
}

// upstreamErrorMessage extracts an error message from an OpenAI-style body.
func upstreamErrorMessage(body []byte) string {
	var doc struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &doc); err == nil {
		if doc.Error.Message != "" {
			return truncate(doc.Error.Message, 400)
		}
		if doc.Message != "" {
			return truncate(doc.Message, 400)
		}
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	return truncate(trimmed, 400)
}

// classEnvelope wraps an error with its HTTP status so the host can decide
// failover vs. surface.
func classEnvelope(status int, err error) []byte {
	if status <= 0 {
		status = http.StatusBadGateway
	}
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{
		Code:       statusCode(status),
		Message:    err.Error(),
		HTTPStatus: status,
	}})
	return out
}

func statusCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request"
	case http.StatusNotFound:
		return "model_not_found"
	case http.StatusUnauthorized:
		return "unauthenticated"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case 499:
		return "cancelled"
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return "backend_unavailable"
	}
	return "upstream_error"
}

// redact keeps an error's class while dropping anything that could identify a
// credential (a URL is never echoed whole).
func redact(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if idx := strings.Index(msg, "http"); idx >= 0 && strings.Contains(msg, "://") {
		return truncate(msg, 240)
	}
	return truncate(msg, 240)
}

// rewriteModelID is the one JSON rewrite this plugin performs. It is split out
// (and exported to tests) so the "did we change the right field" question has a
// single answer.
func rewriteModelID(body []byte, upstreamID string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("the request body is not a JSON object")
	}
	encoded, err := json.Marshal(upstreamID)
	if err != nil {
		return nil, fmt.Errorf("encode model id: %w", err)
	}
	doc["model"] = encoded
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("re-encode request body: %w", err)
	}
	return out, nil
}

// idleGuard tracks the gap between chunks and cancels the upstream read when it
// lapses. It is a timer that is REARMED on every chunk, which is precisely what
// makes it an idle budget rather than a total budget.
type idleGuard struct {
	mu      sync.Mutex
	budget  time.Duration
	timer   *time.Timer
	tripped bool
}

// newIdleGuard arms a guard. A zero budget disables it.
func newIdleGuard(budget time.Duration, onTrip func()) *idleGuard {
	g := &idleGuard{budget: budget}
	if budget <= 0 {
		return g
	}
	g.timer = time.AfterFunc(budget, func() {
		g.mu.Lock()
		g.tripped = true
		g.mu.Unlock()
		if onTrip != nil {
			onTrip()
		}
	})
	return g
}

// Rearm resets the silence budget after a chunk arrived.
func (g *idleGuard) Rearm() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.timer != nil {
		g.timer.Reset(g.budget)
	}
}

// Tripped reports whether the budget lapsed.
func (g *idleGuard) Tripped() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tripped
}

// Stop cancels the timer.
func (g *idleGuard) Stop() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
}

// parsePositiveInt is used by the panel-facing query parsing.
func parsePositiveInt(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
