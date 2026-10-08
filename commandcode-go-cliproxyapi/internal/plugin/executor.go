// Non-stream and stream execution paths (FR-005/FR-006/FR-007, arch §4/§5
// steps 5-8): resolve the public model ID against the catalog snapshot,
// translate the inbound payload to the record's upstream protocol, call
// upstream through the host HTTP callbacks, and translate back to the
// client protocol. Request authentication is selected by CPA.

package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/adapter/chatcompletions"
	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/adapter/gocli"
	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/adapter/messages"
	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/adapter/responses"
	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/adapter/shared"
	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/catalog"
	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/config"
	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/errclass"
)

// executorRequest mirrors rpcExecutorRequest: the SDK embeds
// pluginapi.ExecutorRequest untagged, so its fields marshal under Go field
// names ("Model", "SourceFormat", "OriginalRequest", "Stream"). StreamID is
// the DOWNSTREAM host-allocated id for executor.execute_stream emissions;
// ids returned by DoStream are UPSTREAM and never interchangeable (§4).
type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID string `json:"stream_id,omitempty"`
}

// resolvedExecution carries everything both execution paths need after
// model/account resolution succeeded.
type resolvedExecution struct {
	cfg     config.Config
	rec     catalog.ModelRecord
	account config.Account
	// pool is the pool the account was acquired from. The lease belongs to
	// that pool, not to whatever pool the manager holds when the request
	// finishes: a reconfigure replaces the manager's pool while requests are
	// still in flight, and returning their leases to the new pool would pin
	// the old pool's in-flight counter forever (permanently capping those
	// credentials) while decrementing a pool that never granted them.
	pool *poolState
	// goCli routes this request over the CLI transport (/alpha/generate)
	// instead of the documented Provider API. It is derived from the
	// selected account's mode, so a Go-plan credential never reaches a
	// surface that refuses it with upgrade_required.
	goCli bool
	// status is the upstream HTTP status, recorded so the streaming path can
	// release the account (with the right cooldown) when the pump finishes.
	status int
	// released guards the once-only release: a request has several failure
	// exits plus the completion path, and releasing twice would decrement
	// another request's in-flight count.
	released bool
}

// releaseAccount returns the account to the pool it was acquired from. status
// is the upstream HTTP status (0 when the request never reached upstream); the
// pool maps it to a cooldown so a bad or throttled credential stops being
// picked. The once-guard matters because a request has several failure exits
// (URL/envelope build, transport error, non-2xx) plus the completion path.
func (m *Manager) releaseAccount(res *resolvedExecution, status int) {
	if res == nil || res.released {
		return
	}
	res.released = true
	if res.pool == nil || res.account.Credential == "" {
		return
	}
	cooldown := cooldownFor(status)
	if status == 429 && cooldown == 0 {
		cooldown = 60 * time.Second
	}
	res.pool.release(res.account.Credential, cooldown, time.Now())
}

// resolveExecution resolves the requested model against the snapshot and picks
// an account from the plugin's own pool. A non-nil second return is a
// ready-made failure envelope.
//
// The credential no longer comes from the host: with no AuthProvider
// capability there is no host auth record to read, and the ModelRouter path
// passes no auth anyway. The pool is the single source of credentials.
func (m *Manager) resolveExecution(req executorRequest) (*resolvedExecution, []byte) {
	m.mu.RLock()
	cfg, mgr, pool := m.cfg, m.mgr, m.pool
	m.mu.RUnlock()

	accounts := m.poolAccounts(cfg)
	if len(accounts) == 0 {
		// Registered but not configured. Say so plainly instead of failing
		// with a confusing "no routable catalog" further down.
		return nil, classEnvelope(&errclass.Error{
			Class:      errclass.ClassAuth,
			Message:    "commandcode plugin is registered but not configured: add an account credential in the plugin configuration",
			StatusCode: http.StatusUnauthorized,
		})
	}

	var rec catalog.ModelRecord
	var found bool
	if mgr != nil && req.Model != "" {
		rec, found = mgr.Lookup(req.Model)
	}
	if !found {
		return nil, classEnvelope(&errclass.Error{
			Class:      errclass.ClassInvalidModel,
			Message:    "model not in routable catalog",
			StatusCode: http.StatusNotFound,
		})
	}

	account := pool.acquire(accounts, cfg.Pool, time.Now())
	if account == nil {
		// Every account is cooling down or at its concurrency cap.
		return nil, classEnvelope(&errclass.Error{
			Class:      errclass.ClassAuth,
			Message:    "no commandcode account available: all are cooling down or at their concurrency limit",
			StatusCode: http.StatusServiceUnavailable,
		})
	}
	debugTrace("executor account selected mode=%s label=%s model=%s upstream_model=%s route=%s",
		account.Mode, account.Label, req.Model, rec.UpstreamID, rec.Protocol)
	// The lease is bound to the pool it was taken from (see resolvedExecution),
	// so releaseAccount never has to re-read m.pool.
	return &resolvedExecution{cfg: cfg, rec: rec, account: *account, pool: pool, goCli: account.Mode == config.TransportGoCLI}, nil
}

// handleExecute implements executor.execute (non-stream). Stream-flagged
// requests are routed to the stream path instead of rejected.
func (m *Manager) handleExecute(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}

	// Optional global in-flight cap (off unless max-inflight > 0).
	releaseInflight, admitted := m.acquireInflight(m.inflightMax())
	if !admitted {
		return inflightRejection(), nil
	}
	defer releaseInflight()
	debugTrace("executor invoked model=%s source_format=%s stream=%t original_body_%s payload_%s", req.Model, req.SourceFormat, req.Stream, debugBodyMeta(req.OriginalRequest), debugBodyMeta(req.Payload))
	if req.Stream {
		return m.executeStream(req)
	}
	res, failEnv := m.resolveExecution(req)
	if res == nil {
		return failEnv, nil
	}
	sessionID, eErr := resolveCommandCodeSessionID(req)
	if eErr != nil {
		m.releaseAccount(res, 0)
		return classEnvelope(eErr), nil
	}
	debugTrace("executor session mode=%s source_format=%s x_commandcode_session=%s fallback=%t", "non-stream", req.SourceFormat, sessionID, sessionID == emptyCommandCodeSessionID)
	if res.goCli {
		m.announceDeviceIfDue(context.Background(), res.cfg.Device, res.cfg.BaseURL, res.account.Credential)
		return m.handleExecuteGoCLI(req, res, sessionID)
	}
	upstreamBody, eErr := buildUpstreamRequest(res.rec.Protocol, res.rec.UpstreamID, req.SourceFormat, req.OriginalRequest, res.rec.Thinking)
	if eErr != nil {
		m.releaseAccount(res, 0)
		return classEnvelope(eErr), nil
	}

	url := catalog.JoinUpstreamURL(res.cfg.BaseURL, res.rec.EndpointPath)
	debugTrace("executor resolved public_model=%s upstream_model=%s route=%s url=%s mode=%s", req.Model, res.rec.UpstreamID, res.rec.Protocol, url, res.account.Mode)
	debugTrace("executor sending non-stream url=%s body_len=%d", url, len(upstreamBody))
	ctx, cancel := context.WithTimeout(context.Background(), res.cfg.RequestTimeout)
	defer cancel()
	headers := upstreamAuthHeaders(res.rec.Protocol, res.account.Credential, sessionID)
	resp, err := m.bridge.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     url,
		Headers: headers,
		Body:    upstreamBody,
	})
	if err != nil {
		m.releaseAccount(res, 0)
		debugTrace("executor non-stream network error: %v", err)
		return classEnvelope(errclass.FromNetwork(err)), nil
	}
	m.releaseAccount(res, resp.StatusCode)
	debugTrace("executor received non-stream status=%d body_len=%d", resp.StatusCode, len(resp.Body))
	if resp.StatusCode >= 400 {
		return classEnvelope(shared.UpstreamStatusError(resp.StatusCode, resp.Body)), nil
	}
	// Parse/envelope guard only; true OOM prevention belongs to the host transport's byte cap.
	if int64(len(resp.Body)) > res.cfg.MaxResponseBytes {
		return classEnvelope(errclass.Translation("response exceeds max-response-bytes")), nil
	}
	converted, eErr := convertNonStream(res.rec.Protocol, req.SourceFormat, resp.StatusCode, resp.Body)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: converted, Headers: resp.Headers}), nil
}

func buildUpstreamRequest(route catalog.Route, upstreamModel, sourceFormat string, sourceBody []byte, ts *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	switch route {
	case catalog.RouteChatCompletions:
		return chatcompletions.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts)
	case catalog.RouteMessages:
		return messages.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts)
	case catalog.RouteResponses:
		return responses.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts)
	}
	return nil, errclass.Translation("unsupported route")
}

// goCLIHeaderOptions renders the wire identity for one go-cli request from the
// device config. Centralised so the header set and the envelope's config block
// are derived from the SAME source: a request whose x-project-slug disagrees
// with its config.workingDir is exactly the kind of inconsistency that makes a
// client look automated.
//
// The identity layer is one switch, not two: when device.enabled is false the
// project slug is dropped here as well, so turning it off actually falls back
// to the pre-identity behaviour instead of leaving half of it on.
func goCLIHeaderOptions(cfg config.Config, sessionID string) gocli.HeaderOptions {
	opts := gocli.HeaderOptions{SessionID: sessionID}
	if cfg.Device.Enabled {
		opts.ProjectDir = cfg.Device.EffectiveProjectDir()
	} else {
		// Identity layer off: drop the fabricated project slug entirely.
		opts.OmitProjectSlug = true
	}
	return opts
}

// buildGoCLIRequest renders the CLI-transport (/alpha/generate) envelope for
// one request. The envelope is built from an OpenAI chat-completions body, so
// a non-OpenAI source is translated first — the same body the provider route
// would have sent, just wrapped for the CLI surface instead.
func buildGoCLIRequest(res *resolvedExecution, req executorRequest, sessionID string) ([]byte, *errclass.Error) {
	body, eErr := buildUpstreamRequest(catalog.RouteChatCompletions, res.rec.UpstreamID, req.SourceFormat, req.OriginalRequest, res.rec.Thinking)
	if eErr != nil {
		return nil, eErr
	}
	// The fabricated device must be the SAME one the headers announce. When
	// the identity layer is off the envelope keeps its static defaults.
	envelopeOpts := gocli.Options{
		// Thread identity is derived from the resolved session so the same
		// client session maps onto one upstream thread across turns.
		ThreadID: gocli.NormalizeThreadID(sessionID),
		// With no system prompt the CLI substitute is a single space;
		// otherwise upstream injects a multi-thousand-token default prompt.
		SystemPlaceholder: true,
	}
	if res.cfg.Device.Enabled {
		projectDir := res.cfg.Device.EffectiveProjectDir()
		envelopeOpts.WorkingDir = projectDir
		envelopeOpts.Environment = gocli.DeviceEnvironment()
	}
	envelope, err := gocli.BuildEnvelope(body, envelopeOpts)
	if err != nil {
		return nil, errclass.Translation("failed to build CLI envelope: " + err.Error())
	}
	return envelope, nil
}

// handleExecuteGoCLI serves a non-streaming client over the CLI transport.
// The CLI endpoint is always a stream, so the whole response is consumed and
// aggregated into one completion before translation back to the client. The
// aggregation is checked: only a stream that carries its finish event is a
// completed answer, and a truncated body fails classified.
func (m *Manager) handleExecuteGoCLI(req executorRequest, res *resolvedExecution, sessionID string) ([]byte, error) {
	url, eErr := generateURL(res.cfg.BaseURL)
	if eErr != nil {
		m.releaseAccount(res, 0)
		return classEnvelope(eErr), nil
	}
	upstreamBody, eErr := buildGoCLIRequest(res, req, sessionID)
	if eErr != nil {
		m.releaseAccount(res, 0)
		return classEnvelope(eErr), nil
	}
	debugTrace("executor sending non-stream url=%s body_len=%d mode=go-cli", url, len(upstreamBody))
	// Transparent retry of a pre-first-byte transport flip. The whole response
	// is buffered before anything reaches the client, so a failed attempt was
	// never observed downstream and redoing it cannot duplicate content. Only
	// transport-level drops qualify; an upstream status (429/503/...) is a
	// deliberate signal and is never retried here.
	policy := newRetryPolicy(res.cfg.Retry.Max, res.cfg.Retry.BaseBackoff)
	var resp pluginapi.HTTPResponse
	var err error
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			time.Sleep(policy.backoffFor(attempt - 1))
		}
		ctx, cancel := context.WithTimeout(context.Background(), res.cfg.RequestTimeout)
		resp, err = m.bridge.Do(ctx, pluginapi.HTTPRequest{
			Method:  http.MethodPost,
			URL:     url,
			Headers: gocli.GenerateHeadersWithOptions(res.account.Credential, goCLIHeaderOptions(res.cfg, sessionID)),
			Body:    upstreamBody,
		})
		cancel()
		if err == nil {
			break
		}
		debugTrace("executor go-cli non-stream network error attempt=%d: %v", attempt, err)
		if attempt > policy.max || !isRetryableTransportFlip(err.Error()) {
			m.releaseAccount(res, 0)
			return classEnvelope(errclass.FromNetwork(err)), nil
		}
		debugTrace("executor go-cli non-stream retrying after transport flip attempt=%d/%d", attempt, policy.max+1)
	}
	m.releaseAccount(res, resp.StatusCode)
	debugTrace("executor go-cli received non-stream status=%d body_len=%d", resp.StatusCode, len(resp.Body))
	if resp.StatusCode >= 400 {
		return classEnvelope(goCLIUpstreamError(resp.StatusCode, resp.Body)), nil
	}
	if int64(len(resp.Body)) > res.cfg.MaxResponseBytes {
		return classEnvelope(errclass.Translation("response exceeds max-response-bytes")), nil
	}
	// The checked aggregation reports whether the CLI stream ended without its
	// finish event. A truncated stream is a dropped connection, not an answer:
	// fail it rather than hand the client a half response dressed up as a
	// completed completion.
	//
	// Classification is UpstreamFallback (ClassUpstream, retryable) because a
	// non-stream response is fully buffered before anything reaches the
	// client, so the partial body was never delivered: re-execution cannot
	// duplicate content, and the failure is an upstream server/connection
	// fault with no HTTP status of its own. Malformed frames and upstream
	// error events keep the classification ConvertNonStreamChecked derives
	// (Translation / FromStatus), which is strictly more specific than the
	// truncation fallback.
	assembled, truncated, eErr := gocli.ConvertNonStreamChecked(resp.Body)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	if truncated {
		debugTrace("executor go-cli non-stream truncated body_len=%d", len(resp.Body))
		return classEnvelope(errclass.UpstreamFallback("upstream CLI stream ended without a finish event")), nil
	}
	// A completed stream that carried nothing visible is a silent failure: the
	// client would cache an empty answer as a successful one. Reported as a
	// rate-limit so a retrying SDK backs off and re-asks instead of treating it
	// as the model's answer.
	//
	// Order matters: truncation is checked first, because when the upstream
	// never sent its finish event the missing finish is the root cause and the
	// empty output is only a symptom.
	if reason := gocli.ZeroOutputReason(resp.Body); reason != "" {
		debugTrace("executor go-cli non-stream zero output body_len=%d", len(resp.Body))
		return classEnvelope(errclass.FromStatus(http.StatusTooManyRequests, reason)), nil
	}
	converted, eErr := chatcompletions.ConvertNonStreamResponse(req.SourceFormat, http.StatusOK, assembled)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: converted, Headers: resp.Headers}), nil
}

// acquireInflight admits one request against the optional global in-flight cap.
//
// The cap exists for the "bare proxy, no upstream limiter" deployment: without
// it, N concurrent requests each buffer a full response and the process can be
// OOM-killed. It is OFF by default (max <= 0), matching the reference, because
// concurrency control normally belongs to the reverse proxy in front (nginx
// limit_conn), which is the only layer that knows what the host can take.
//
// The returned release function is idempotent: a request has several exit paths
// and a double release would free a slot another request is using. Streaming
// callers must pass it to the pump goroutine, because the handler returns long
// before the stream finishes.
func (m *Manager) acquireInflight(max int) (release func(), ok bool) {
	if max <= 0 {
		return func() {}, true
	}
	if m.inflight.Add(1) > int64(max) {
		m.inflight.Add(-1)
		return func() {}, false
	}
	var once sync.Once
	return func() { once.Do(func() { m.inflight.Add(-1) }) }, true
}

// inflightMax reads the configured cap. Read under the same lock that guards
// cfg so a reconfigure cannot be observed half-applied.
func (m *Manager) inflightMax() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.MaxInflight
}

// inflightRejection is the envelope a request past the cap receives: a 503 with
// Retry-After semantics, so a client SDK backs off and retries rather than
// treating it as a permanent failure.
func inflightRejection() []byte {
	return classEnvelope(&errclass.Error{
		Class:      errclass.ClassUpstream,
		Message:    "commandcode: too many requests in flight; retry shortly",
		StatusCode: http.StatusServiceUnavailable,
		Retryable:  true,
	})
}

// generateURL resolves the CLI endpoint, folding the adapter's plain error
// into the classified envelope vocabulary used by the executor.
func generateURL(baseURL string) (string, *errclass.Error) {
	url, err := gocli.GenerateURL(baseURL)
	if err != nil {
		return "", errclass.Translation(err.Error())
	}
	return url, nil
}

// goCLIUpstreamError classifies an upstream CLI-transport error response.
//
// The CLI surface has its own status vocabulary: a quota wall arrives as 402,
// an overloaded backend as 503. Passing those straight to a client SDK means a
// payment wall reads as "bad request" and a retryable overload reads as a
// generic 502, so the SDK neither backs off nor retries. The reference
// translates them (402 -> 429, 403 -> 401, 422 -> 400, 500 -> 502, ...); this
// applies the same translation BEFORE classification, so both the client-facing
// status and the error class agree with what the client will actually observe.
//
// The provider transport keeps calling shared.UpstreamStatusError directly: it
// already speaks the client's status vocabulary.
func goCLIUpstreamError(status int, body []byte) *errclass.Error {
	return shared.UpstreamStatusError(gocli.MapUpstreamStatus(status).Status, body)
}

func upstreamAuthHeaders(route catalog.Route, key, sessionID string) http.Header {
	var h http.Header
	if route == catalog.RouteMessages {
		h = messages.AuthHeaders(key)
	} else {
		// Chat Completions and Responses endpoints are OpenAI-style bearer.
		h = chatcompletions.AuthHeaders(key)
	}
	h.Set("x-commandcode-session", sessionID)
	return h
}

const emptyCommandCodeSessionID = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// resolveCommandCodeSessionID applies the FR-012/AC-H precedence: CPA's canonical
// identity, an explicit inbound session header, then the existing content hash.
func resolveCommandCodeSessionID(req executorRequest) (string, *errclass.Error) {
	if sid, ok := req.Metadata["canonical_session_id"].(string); ok && sid != "" {
		return sid, nil
	}
	for _, name := range []string{
		"X-Session-Affinity",
		"X-Commandcode-Session",
		"X-Session-Id",
		"X-Claude-Code-Session-Id",
		"Session-Id",
	} {
		if sid := req.Headers.Get(name); sid != "" {
			return sid, nil
		}
	}
	return deriveCommandCodeSessionID(req.SourceFormat, req.OriginalRequest)
}

// deriveCommandCodeSessionID hashes the model-visible content of the initial user
// turn before translation (FR-012/AC-H). Metadata is excluded;
// valid requests without a user turn use the fixed empty-input digest.
func deriveCommandCodeSessionID(sourceFormat string, originalRequest []byte) (string, *errclass.Error) {
	var content strings.Builder
	appendParts := func(raw json.RawMessage, target string) *errclass.Error {
		parts, eErr := shared.DecodeStringOrParts(raw, target)
		if eErr != nil {
			return eErr
		}
		for _, part := range parts {
			if part.ImageURL != "" {
				content.WriteString(part.ImageURL)
			} else {
				content.WriteString(part.Text)
			}
		}
		return nil
	}

	switch sourceFormat {
	case "openai":
		var req shared.ChatCompletionsRequest
		if err := json.Unmarshal(originalRequest, &req); err != nil {
			return "", errclass.Translation("malformed openai request JSON: " + err.Error())
		}
		started := false
		for _, msg := range req.Messages {
			if !started {
				if msg.Role == "system" || msg.Role == "developer" {
					continue
				}
				if msg.Role != "user" {
					break
				}
				started = true
			} else if msg.Role != "user" {
				break
			}
			if eErr := appendParts(msg.Content, "/v1/chat/completions"); eErr != nil {
				return "", eErr
			}
		}
	case "claude":
		req, eErr := shared.DecodeClaudeMessages(originalRequest)
		if eErr != nil {
			return "", eErr
		}
		started := false
		for _, msg := range req.Messages {
			if !started {
				if msg.Role != "user" {
					continue
				}
				started = true
			} else if msg.Role != "user" {
				break
			}
			if msg.Content != "" {
				content.WriteString(msg.Content)
			}
			for _, block := range msg.Blocks {
				switch block.Kind {
				case "text":
					content.WriteString(block.Text)
				case "image":
					content.WriteString(block.URL)
				case "tool_result":
					text, eErr := shared.ToolResultText(block.Result, "tool messages carry text only")
					if eErr != nil {
						return "", eErr
					}
					content.WriteString(text)
				}
			}
		}
	case "openai-response":
		var req shared.ResponsesRequest
		if err := json.Unmarshal(originalRequest, &req); err != nil {
			return "", errclass.Translation("malformed openai-response request JSON: " + err.Error())
		}
		items, eErr := req.DecodeInputItems()
		if eErr != nil {
			return "", eErr
		}
		started := false
		for _, item := range items {
			isUserMessage := item.Role == "user" && (item.Type == "message" || item.Type == "")
			if !started {
				if !isUserMessage {
					continue
				}
				started = true
			} else if !isUserMessage {
				break
			}
			if eErr := appendParts(item.Content, "/v1/responses"); eErr != nil {
				return "", eErr
			}
		}
	default:
		return "", shared.UnsupportedFormat(sourceFormat, "CommandCode session derivation")
	}

	digest := sha256.Sum256([]byte(content.String()))
	return hex.EncodeToString(digest[:]), nil
}

// convertNonStream routes one upstream response to its adapter's uniform
// translator: every adapter owns status classification (>=400 → §7
// classified errors), native passthrough, and cross-format conversion for
// all client formats.
func convertNonStream(route catalog.Route, sourceFormat string, status int, body []byte) ([]byte, *errclass.Error) {
	switch route {
	case catalog.RouteChatCompletions:
		return chatcompletions.ConvertNonStreamResponse(sourceFormat, status, body)
	case catalog.RouteMessages:
		return messages.ConvertNonStreamResponse(sourceFormat, status, body)
	case catalog.RouteResponses:
		return responses.ConvertNonStreamResponse(sourceFormat, status, body)
	}
	return nil, errclass.Translation("unsupported route")
}

// classEnvelope renders a classified failure as the wire error envelope;
// ToEnvelopeError redacts and propagates Retryable/HTTPStatus host-side.
func classEnvelope(e *errclass.Error) []byte {
	wire := errclass.ToEnvelopeError(e)
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &wire})
	return out
}

// streamConverter is the common shape of the four adapters' stream
// converters: feed one upstream SSE chunk, get translated client events.
type streamConverter interface {
	Feed(chunk []byte) (events [][]byte, done bool, eErr *errclass.Error)
}

// Compile-time proof the close-without-terminal Flush seam (F5) picks up
// both converters that can hold a deferred terminal.
var (
	_ interface{ Flush() [][]byte } = (*chatcompletions.StreamConverter)(nil)
	_ interface{ Flush() [][]byte } = (*messages.StreamConverter)(nil)
)

func newStreamConverter(route catalog.Route, sourceFormat string, goCli bool) streamConverter {
	if goCli {
		// The CLI transport always answers in its own event vocabulary
		// regardless of the model's catalog route; gocli translates onto the
		// chat-completions kernel and then into sourceFormat.
		return gocli.NewConverter(sourceFormat)
	}
	switch route {
	case catalog.RouteMessages:
		return messages.NewStreamConverter(sourceFormat)
	case catalog.RouteResponses:
		return responses.NewStreamConverter(sourceFormat)
	}
	return chatcompletions.NewStreamConverter(sourceFormat)
}

// handleExecuteStream implements executor.execute_stream (FR-006, §7).
// It decodes once and delegates to executeStream so a stream-flagged
// request arriving via executor.execute is never parsed twice.
func (m *Manager) handleExecuteStream(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}
	debugTrace("executor stream invoked model=%s source_format=%s stream=%t original_body_%s payload_%s", req.Model, req.SourceFormat, req.Stream, debugBodyMeta(req.OriginalRequest), debugBodyMeta(req.Payload))
	return m.executeStream(req)
}

// executeStream runs the already-decoded stream execution: pre-first-byte errors
// (invalid request, unroutable model, upstream HTTP >=400) return immediately as
// error envelopes so CPA can failover pre-emission.
// On success (upstream HTTP 200 OK), the reading and emitting pump loop runs in
// a background goroutine and executeStream returns okEnvelope immediately so the
// host can start draining chunks to the downstream client without buffer deadlocks.
func (m *Manager) executeStream(req executorRequest) ([]byte, error) {
	res, failEnv := m.resolveExecution(req)
	if res == nil {
		return failEnv, nil
	}
	sessionID, eErr := resolveCommandCodeSessionID(req)
	if eErr != nil {
		m.releaseAccount(res, 0)
		return classEnvelope(eErr), nil
	}
	debugTrace("executor session mode=%s source_format=%s x_commandcode_session=%s fallback=%t", "stream", req.SourceFormat, sessionID, sessionID == emptyCommandCodeSessionID)
	// Optional global in-flight cap. Acquired BEFORE the upstream is opened
	// and released by the pump, which outlives this handler.
	releaseInflight, admitted := m.acquireInflight(res.cfg.MaxInflight)
	if !admitted {
		m.releaseAccount(res, 0)
		return inflightRejection(), nil
	}
	if res.goCli {
		m.announceDeviceIfDue(context.Background(), res.cfg.Device, res.cfg.BaseURL, res.account.Credential)
		return m.executeStreamGoCLI(req, res, sessionID, releaseInflight)
	}
	upstreamBody, eErr := buildUpstreamRequest(res.rec.Protocol, res.rec.UpstreamID, req.SourceFormat, req.OriginalRequest, res.rec.Thinking)
	if eErr != nil {
		releaseInflight()
		m.releaseAccount(res, 0)
		return classEnvelope(eErr), nil
	}

	url := catalog.JoinUpstreamURL(res.cfg.BaseURL, res.rec.EndpointPath)
	debugTrace("executor sending stream url=%s body_len=%d", url, len(upstreamBody))
	ctx, cancel := context.WithTimeout(context.Background(), res.cfg.RequestTimeout)
	defer cancel()
	st, _, id, err := m.bridge.DoStream(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     url,
		Headers: upstreamAuthHeaders(res.rec.Protocol, res.account.Credential, sessionID),
		Body:    upstreamBody,
	})
	debugTrace("executor stream DoStream status=%d upstreamID=%s err=%v", st, id, err)
	if err != nil {
		releaseInflight()
		m.releaseAccount(res, 0)
		return classEnvelope(errclass.FromNetwork(err)), nil
	}
	if st >= 400 {
		releaseInflight()
		m.releaseAccount(res, st)
		var body []byte
		if id != "" {
			payload, errMsg, _, readErr := m.bridge.StreamRead(id)
			if readErr == nil {
				if len(payload) > 0 {
					body = payload
				} else if errMsg != "" {
					body = []byte(errMsg)
				}
			}
			_ = m.bridge.StreamClose(id)
		}
		return classEnvelope(shared.UpstreamStatusError(st, body)), nil
	}
	// Success: the account stays in-flight for the whole stream so
	// pool.max-concurrency-per-account actually bounds concurrency; the pump
	// releases it once, when the stream finishes.
	res.status = st

	downID := req.StreamID
	if m.bridge != nil {
		m.bridge.inFlight.Add(1)
	}
	go func() {
		if m.bridge != nil {
			defer m.bridge.inFlight.Done()
		}
		m.pumpStream(downID, id, res, req.SourceFormat, releaseInflight)
	}()
	return okEnvelope(struct{}{}), nil
}

// executeStreamGoCLI runs a streaming client over the CLI transport. The
// upstream is a plain SSE stream; pumpStream translates it with the gocli
// converter. Pre-first-byte failures (URL, envelope, HTTP >=400) still return
// as envelopes so CPA can failover before any emission.
func (m *Manager) executeStreamGoCLI(req executorRequest, res *resolvedExecution, sessionID string, releaseInflight func()) ([]byte, error) {
	url, eErr := generateURL(res.cfg.BaseURL)
	if eErr != nil {
		releaseInflight()
		m.releaseAccount(res, 0)
		return classEnvelope(eErr), nil
	}
	upstreamBody, eErr := buildGoCLIRequest(res, req, sessionID)
	if eErr != nil {
		releaseInflight()
		m.releaseAccount(res, 0)
		return classEnvelope(eErr), nil
	}
	debugTrace("executor sending stream url=%s body_len=%d mode=go-cli", url, len(upstreamBody))
	ctx, cancel := context.WithTimeout(context.Background(), res.cfg.RequestTimeout)
	defer cancel()
	st, _, id, err := m.bridge.DoStream(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     url,
		Headers: gocli.GenerateHeadersWithOptions(res.account.Credential, goCLIHeaderOptions(res.cfg, sessionID)),
		Body:    upstreamBody,
	})
	debugTrace("executor go-cli stream DoStream status=%d upstreamID=%s err=%v", st, id, err)
	if err != nil {
		m.releaseAccount(res, 0)
		return classEnvelope(errclass.FromNetwork(err)), nil
	}
	if st >= 400 {
		m.releaseAccount(res, st)
		var body []byte
		if id != "" {
			payload, errMsg, _, readErr := m.bridge.StreamRead(id)
			if readErr == nil {
				if len(payload) > 0 {
					body = payload
				} else if errMsg != "" {
					body = []byte(errMsg)
				}
			}
			_ = m.bridge.StreamClose(id)
		}
		return classEnvelope(goCLIUpstreamError(st, body)), nil
	}
	// Success: the account stays in-flight for the whole stream (see
	// executeStream); the pump releases it exactly once at the end.
	res.status = st

	downID := req.StreamID
	if m.bridge != nil {
		m.bridge.inFlight.Add(1)
	}
	go func() {
		if m.bridge != nil {
			defer m.bridge.inFlight.Done()
		}
		m.pumpStream(downID, id, res, req.SourceFormat, releaseInflight)
	}()
	return okEnvelope(struct{}{}), nil
}

func (m *Manager) pumpStream(downID, upstreamID string, res *resolvedExecution, sourceFormat string, releaseInflight func()) {
	// The in-flight slot is released by the PUMP, not the handler: the handler
	// returns as soon as the stream is opened, so releasing there would free
	// the slot while the response is still being delivered.
	if releaseInflight != nil {
		defer releaseInflight()
	}

	// The account was acquired by resolveExecution and stays in-flight for the
	// whole stream; release it exactly once when the pump ends (cleanly, on
	// truncation, or on any early failure exit below).
	defer m.releaseAccount(res, res.status)

	var closeOnce sync.Once
	closeStreams := func(downErrMsg string) {
		closeOnce.Do(func() {
			_ = m.bridge.StreamClose(upstreamID)
			_ = m.bridge.StreamCloseDownstream(downID, downErrMsg)
		})
	}
	defer closeStreams("")

	var aborted atomic.Bool
	watchdog := time.AfterFunc(res.cfg.RequestTimeout, func() {
		defer func() {
			if r := recover(); r != nil && m.bridge != nil {
				_ = m.bridge.Log("error", "stream watchdog panicked", nil)
			}
		}()
		aborted.Store(true)
		_ = m.bridge.StreamClose(upstreamID)
	})
	defer watchdog.Stop()

	// Idle watchdog: bounds the GAP between upstream reads, not the total
	// request duration. The vendor CLI applies no upstream idle timeout, so a
	// legitimate long reasoning pause can run for minutes and a total-only cap
	// would kill healthy requests; conversely an upstream that has silently
	// stopped sending would otherwise hold the stream (and the account lease)
	// until the much larger request-timeout above. The timer resets on every
	// chunk, so only a genuine silence trips it.
	var idleTripped atomic.Bool
	idleTimer := (*time.Timer)(nil)
	if res.cfg.Watchdog.Enabled && res.cfg.Watchdog.Stream > 0 {
		idleTimer = time.AfterFunc(res.cfg.Watchdog.Stream, func() {
			defer func() {
				if r := recover(); r != nil && m.bridge != nil {
					_ = m.bridge.Log("error", "stream idle watchdog panicked", nil)
				}
			}()
			idleTripped.Store(true)
			_ = m.bridge.StreamClose(upstreamID)
		})
		defer idleTimer.Stop()
	}
	// armIdle restarts the silence budget. Called after every successful read,
	// including reads that carried no payload (the loop may see a heartbeat).
	armIdle := func() {
		if idleTimer != nil {
			idleTimer.Reset(res.cfg.Watchdog.Stream)
		}
	}

	conv := newStreamConverter(res.rec.Protocol, sourceFormat, res.goCli)
	var (
		total          int64
		upstreamClosed bool
		convDone       bool
	)
	for {
		payload, readErrMsg, closed, err := m.bridge.StreamRead(upstreamID)
		debugTrace("executor stream read chunk_len=%d closed=%t readErrMsg=%q err=%v", len(payload), closed, readErrMsg, err)
		upstreamClosed = closed
		// Any read that returns restarts the silence budget: a chunk arrived,
		// a clean close arrived, or a transport error arrived. All three are
		// progress as far as "is the upstream still alive" is concerned.
		armIdle()
		if idleTripped.Load() {
			// Deliberately NOT a retry: a silent upstream is reported to the
			// client as a retryable rate-limit so its own SDK backs off, rather
			// than the proxy replaying a request the upstream may still be
			// working on.
			debugTrace("executor stream idle timeout after %.0fs of silence", res.cfg.Watchdog.Stream.Seconds())
			closeStreams(errclass.Redact("upstream stream idle timeout"))
			return
		}
		if aborted.Load() {
			closeStreams(errclass.Redact("stream exceeded request-timeout"))
			return
		}
		if err != nil {
			closeStreams(errclass.Redact(err.Error()))
			return
		}
		if readErrMsg != "" {
			closeStreams(errclass.Redact(readErrMsg))
			return
		}
		total += int64(len(payload))
		if total > res.cfg.MaxResponseBytes {
			closeStreams(errclass.Redact("stream exceeded max-response-bytes"))
			return
		}
		events, done, convErr := conv.Feed(payload)
		if convErr != nil {
			closeStreams(errclass.Redact(convErr.Message))
			return
		}
		if emitErr := m.emitAll(downID, events); emitErr != nil {
			closeStreams(errclass.Redact(emitErr.Error()))
			return
		}
		convDone = done
		if convDone || upstreamClosed {
			break
		}
	}
	debugTrace("executor stream loop end convDone=%t upstreamClosed=%t", convDone, upstreamClosed)
	if !convDone && upstreamClosed {
		// A CLI stream that ended before its finish event is truncated (a
		// dropped connection). gocli exposes Truncated to say so; the
		// provider converters do not implement it and keep their existing
		// clean-close flush.
		if _, ok := conv.(interface{ Truncated() bool }); ok {
			closeStreams(errclass.Redact("upstream stream ended without a finish event"))
			return
		}
		if flusher, ok := conv.(interface{ Flush() [][]byte }); ok {
			flushed := flusher.Flush()
			if emitErr := m.emitAll(downID, flushed); emitErr != nil {
				closeStreams(errclass.Redact(emitErr.Error()))
				return
			}
		}
	}
	// A CLI stream that reached [DONE] without a finish event is also
	// truncated: fail it rather than report a completed answer.
	if trunc, ok := conv.(interface{ Truncated() bool }); ok && trunc.Truncated() {
		closeStreams(errclass.Redact("upstream stream ended without a finish event"))
		return
	}
}

// emitAll feeds converted events downstream in order. StreamEmit is
// deadline-bounded (emitTimeout), so a host that stops draining the
// downstream stream surfaces here as an error; the pump loop treats that
// as a post-first-byte stream-fatal failure and runs fail(), whose
// Once-closer releases both streams.
func (m *Manager) emitAll(downStreamID string, events [][]byte) error {
	for _, evt := range events {
		if err := m.bridge.StreamEmit(downStreamID, evt); err != nil {
			return err
		}
	}
	return nil
}
