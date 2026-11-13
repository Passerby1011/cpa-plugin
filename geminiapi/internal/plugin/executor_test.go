package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/geminiapi/internal/backend"
	"github.com/hex-ci/cpa-plugin/geminiapi/internal/catalog"
	"github.com/hex-ci/cpa-plugin/geminiapi/internal/config"
)

// scriptedHost is a fake host callback surface. It answers the callbacks the
// plugin actually uses and records what was sent, which is how the "no cookie
// in a response / no cookie in a log" assertions are made.
type scriptedHost struct {
	mu sync.Mutex
	// calls records method names in order.
	calls []string
	// bodies records the payload of each call, keyed by method (last wins).
	bodies map[string][][]byte
	// httpDo answers host.http.do / do_stream.
	httpDo func(req hostHTTPReq) (int, http.Header, []byte, error)
	// streamChunks are emitted in order by host.http.stream_read.
	streamChunks []hostStreamReadResp
	// streamIndex is the read cursor.
	streamIndex int
	// authFiles is returned by host.auth.list.
	authFiles []pluginapi.HostAuthFileEntry
	// failAuthSave makes host.auth.save fail.
	failAuthSave bool
}

func newScriptedHost() *scriptedHost {
	return &scriptedHost{bodies: map[string][][]byte{}}
}

func (h *scriptedHost) call(method string, payload []byte) ([]byte, error) {
	h.mu.Lock()
	h.calls = append(h.calls, method)
	h.bodies[method] = append(h.bodies[method], payload)
	fn := h.httpDo
	idx := h.streamIndex
	files := h.authFiles
	failSave := h.failAuthSave
	h.mu.Unlock()

	_ = idx

	switch method {
	case pluginabi.MethodHostLog:
		return okEnvelope(struct{}{}), nil
	case pluginabi.MethodHostHTTPDo:
		var req hostHTTPReq
		_ = json.Unmarshal(payload, &req)
		if fn == nil {
			return nil, errString("no http handler")
		}
		status, headers, body, err := fn(req)
		if err != nil {
			return nil, err
		}
		return okEnvelope(pluginapi.HTTPResponse{StatusCode: status, Headers: headers, Body: body}), nil
	case pluginabi.MethodHostHTTPDoStream:
		var req hostHTTPReq
		_ = json.Unmarshal(payload, &req)
		if fn == nil {
			return nil, errString("no http handler")
		}
		status, headers, body, err := fn(req)
		if err != nil {
			return nil, err
		}
		_ = body
		return okEnvelope(hostStreamStartResp{StatusCode: status, Headers: headers, StreamID: "upstream-1"}), nil
	case pluginabi.MethodHostHTTPStreamRead:
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.streamIndex >= len(h.streamChunks) {
			return okEnvelope(hostStreamReadResp{Done: true}), nil
		}
		chunk := h.streamChunks[h.streamIndex]
		h.streamIndex++
		_ = idx
		return okEnvelope(chunk), nil
	case pluginabi.MethodHostHTTPStreamClose, pluginabi.MethodHostStreamClose, pluginabi.MethodHostStreamEmit:
		return okEnvelope(struct{}{}), nil
	case pluginabi.MethodHostAuthList:
		return okEnvelope(struct {
			Files []pluginapi.HostAuthFileEntry `json:"files"`
		}{Files: files}), nil
	case pluginabi.MethodHostAuthSave:
		if failSave {
			return ErrEnvelope("auth_save_failed", "nope"), nil
		}
		return okEnvelope(struct{}{}), nil
	}
	return nil, errString("unexpected host callback " + method)
}

// errString keeps the fake honest: a transport error instead of a silent nil.
type errString string

func (e errString) Error() string { return string(e) }

func (h *scriptedHost) count(method string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.calls {
		if m == method {
			n++
		}
	}
	return n
}

func (h *scriptedHost) lastBody(method string) []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	entries := h.bodies[method]
	if len(entries) == 0 {
		return nil
	}
	return entries[len(entries)-1]
}

// newTestManager builds a Manager with prepared catalogs and an EXTERNAL
// backend per namespace, so no download or spawn is involved.
func newTestManager(t *testing.T, host *scriptedHost) *Manager {
	t.Helper()
	m := NewManager(NewHostBridge(host.call))
	aiStudioCatalog, err := catalog.Build([]backend.Model{
		{
			ID:                         "gemini-2.5-pro",
			Capabilities:               map[string]any{"chat_model": true, "thinking": true},
			SupportedGenerationMethods: []string{"generateContent"},
			InputTokenLimit:            1000000,
			OutputTokenLimit:           8192,
		},
	}, catalog.BuildOptions{Namespace: catalog.AIStudio})
	if err != nil {
		t.Fatalf("build the ai-studio fixture catalog: %v", err)
	}
	geminiWebCatalog, err := catalog.Build([]backend.Model{
		{ID: "gemini-2.5-pro", Object: "model", OwnedBy: "google"},
		{ID: "gemini-3-pro", Object: "model", OwnedBy: "google"},
		{ID: "gemini-2.5-pro-thinking", Object: "model", OwnedBy: "google"},
	}, catalog.BuildOptions{Namespace: catalog.GeminiWeb})
	if err != nil {
		t.Fatalf("build the gemini-web fixture catalog: %v", err)
	}
	m.mu.Lock()
	m.cfg = config.Config{
		DataDir:           t.TempDir(),
		RequestTimeout:    config.DefaultRequestTimeout,
		StreamIdleTimeout: config.DefaultStreamIdleTimeout,
		ModelTimeout:      config.DefaultModelTimeout,
		Namespaces:        []string{config.NamespaceAIStudio, config.NamespaceGeminiWeb},
		AIStudio:          config.NamespaceConfig{BaseURL: "http://127.0.0.1:2049/v1", APIKey: "ai-studio-key"},
		GeminiWeb:         config.NamespaceConfig{BaseURL: "http://127.0.0.1:2048/v1", APIKey: "gemini-web-key"},
	}
	m.externals = map[string]bool{config.NamespaceAIStudio: true, config.NamespaceGeminiWeb: true}
	m.catalogs = map[string]catalog.BuildResult{
		config.NamespaceAIStudio:  aiStudioCatalog,
		config.NamespaceGeminiWeb: geminiWebCatalog,
	}
	m.mu.Unlock()
	return m
}

// managedTestManager marks one namespace as plugin-managed and hands the
// supervisor resolution to a fake Setup, so a management route can be exercised
// without a data directory or a binary.
func managedTestManager(t *testing.T, host *scriptedHost, namespace, apiKey string, adminToken string) *Manager {
	t.Helper()
	m := newTestManager(t, host)
	m.mu.Lock()
	m.externals[namespace] = false
	m.backends[namespace] = &backend.Manager{}
	m.cfg.AIStudio.BaseURL = ""
	if namespace == config.NamespaceAIStudio {
		m.cfg.AIStudio.APIKey = ""
	}
	m.resolveSetup = func(_ context.Context, ns string) (backend.Setup, error) {
		origin := "http://127.0.0.1:2049"
		if ns == config.NamespaceGeminiWeb {
			origin = "http://127.0.0.1:2048"
		}
		return backend.Setup{
			Origin:     origin,
			BaseURL:    origin + "/v1",
			APIKey:     apiKey,
			AdminToken: adminToken,
			Flavor:     backend.FlavorID(ns),
		}, nil
	}
	m.mu.Unlock()
	return m
}

// --- routing ---

// TestExecuteRoutesAISStudioNamespace is the acceptance criterion
// "ai-studio/ 前缀路由": the backend receives its own model id, not the prefixed
// one.
func TestExecuteRoutesAISStudioNamespace(t *testing.T) {
	host := newScriptedHost()
	var seenURL string
	var seenBody []byte
	host.httpDo = func(req hostHTTPReq) (int, http.Header, []byte, error) {
		seenURL = req.URL
		seenBody = req.Body
		return http.StatusOK, http.Header{"Content-Type": []string{"application/json"}},
			[]byte(`{"id":"x","object":"chat.completion","model":"gemini-2.5-pro","choices":[{"message":{"role":"assistant","content":"hi"}}]}`), nil
	}
	m := newTestManager(t, host)

	req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model:           "ai-studio/gemini-2.5-pro",
		Payload:         []byte(`{"model":"ai-studio/gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`),
		OriginalRequest: []byte(`{"model":"ai-studio/gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`),
		Headers:         http.Header{"Authorization": []string{"Bearer cpa-client-key"}},
	}}
	raw, err := m.executeWithContext(t.Context(), req)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	env := decodeTestEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("execution failed: %s", envelopeErrorText(env))
	}

	if !strings.Contains(seenURL, "/v1/chat/completions") {
		t.Fatalf("upstream URL = %q, want the backend's /v1/chat/completions", seenURL)
	}
	var outbound map[string]any
	if err := json.Unmarshal(seenBody, &outbound); err != nil {
		t.Fatalf("outbound body is not JSON: %v", err)
	}
	if outbound["model"] != "gemini-2.5-pro" {
		t.Fatalf("outbound model = %v: the backend must receive its bare model id", outbound["model"])
	}
	if outbound["messages"] == nil {
		t.Fatal("the request body lost its messages field")
	}
}

// TestExecuteRejectsNonsenseButServesBothNamespaces pins the routing rule that
// replaced "gemini-web is another card's problem": both namespaces are served,
// each by its own backend, and each rejects a model it never published.
func TestExecuteRoutesEachNamespaceToItsOwnBackend(t *testing.T) {
	host := newScriptedHost()
	seen := map[string]string{}
	host.httpDo = func(req hostHTTPReq) (int, http.Header, []byte, error) {
		seen[req.URL] = string(req.Body)
		return http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}, []byte(`{"ok":true}`), nil
	}
	m := newTestManager(t, host)

	for _, model := range []string{"ai-studio/gemini-2.5-pro", "gemini-web/gemini-2.5-pro"} {
		raw, err := m.executeWithContext(t.Context(), executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
			Model:   model,
			Payload: []byte(`{"model":"` + model + `"}`),
		}})
		if err != nil {
			t.Fatalf("execute %s: %v", model, err)
		}
		env := decodeTestEnvelope(t, raw)
		if !env.OK {
			t.Fatalf("execute %s failed: %s", model, envelopeErrorText(env))
		}
	}
	// Each namespace reached ITS OWN backend port: ai-studio 2049, gemini-web
	// 2048. Routing one namespace's request at the other's port is the defect
	// this split exists to remove.
	for url := range seen {
		switch {
		case strings.HasPrefix(url, "http://127.0.0.1:2049"):
			if !strings.Contains(seen[url], `"model":"gemini-2.5-pro"`) {
				t.Fatalf("ai-studio request body = %s", seen[url])
			}
		case strings.HasPrefix(url, "http://127.0.0.1:2048"):
		default:
			t.Fatalf("unexpected backend URL %q", url)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("backend calls = %d, want one per namespace: %v", len(seen), seen)
	}
}

// TestExecuteRefusesAModelOutsideItsNamespaceCatalog: a gemini-web-only model
// must not be answered by ai-studio, even though the id is plausible.
func TestExecuteRefusesAModelOutsideItsNamespaceCatalog(t *testing.T) {
	host := newScriptedHost()
	m := newTestManager(t, host)
	raw, err := m.executeWithContext(t.Context(), executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model:   "ai-studio/gemini-3-pro",
		Payload: []byte(`{"model":"ai-studio/gemini-3-pro"}`),
	}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	env := decodeTestEnvelope(t, raw)
	if env.OK {
		t.Fatal("a model published only by gemini-web was executed under ai-studio")
	}
	if !strings.Contains(envelopeErrorText(env), "ai-studio") {
		t.Fatalf("error does not name the namespace whose catalog was searched: %s", envelopeErrorText(env))
	}
	if host.count(pluginabi.MethodHostHTTPDo) != 0 {
		t.Fatal("the plugin called a backend for a model it never published")
	}
}

// TestExecuteRejectsNamespaceLessModel: an unprefixed id must be refused with an
// explanation, not guessed into a namespace.
func TestExecuteRejectsNamespaceLessModel(t *testing.T) {
	host := newScriptedHost()
	m := newTestManager(t, host)
	raw, err := m.executeWithContext(t.Context(), executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model:   "gemini-2.5-pro",
		Payload: []byte(`{"model":"gemini-2.5-pro"}`),
	}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	env := decodeTestEnvelope(t, raw)
	if env.OK {
		t.Fatal("a namespace-less model was executed")
	}
	if !strings.Contains(envelopeErrorText(env), "ai-studio/") {
		t.Fatalf("error does not show the expected prefix: %s", envelopeErrorText(env))
	}
}

func TestExecuteRejectsModelOutsideCatalog(t *testing.T) {
	host := newScriptedHost()
	m := newTestManager(t, host)
	raw, err := m.executeWithContext(t.Context(), executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model:   "ai-studio/gemini-9.9-nonexistent",
		Payload: []byte(`{"model":"ai-studio/gemini-9.9-nonexistent"}`),
	}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	env := decodeTestEnvelope(t, raw)
	if env.OK {
		t.Fatal("an unknown model was forwarded to the backend")
	}
	if !strings.Contains(envelopeErrorText(env), "is not in the published ai-studio catalog") {
		t.Fatalf("error does not explain the catalog miss: %s", envelopeErrorText(env))
	}
}

// TestExecuteNeverForwardsTheClientAuthorization: the inbound key is CPA's, not
// the backend's; forwarding it would send the wrong credential.
func TestExecuteNeverForwardsTheClientAuthorization(t *testing.T) {
	host := newScriptedHost()
	var seenAuth string
	host.httpDo = func(req hostHTTPReq) (int, http.Header, []byte, error) {
		seenAuth = req.Headers.Get("Authorization")
		return http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}, []byte(`{"ok":true}`), nil
	}
	m := newTestManager(t, host)
	raw, _ := m.executeWithContext(t.Context(), executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model:   "ai-studio/gemini-2.5-pro",
		Payload: []byte(`{"model":"ai-studio/gemini-2.5-pro"}`),
		Headers: http.Header{"Authorization": []string{"Bearer cpa-client-key"}},
	}})
	if env := decodeTestEnvelope(t, raw); !env.OK {
		t.Fatalf("execution failed: %s", envelopeErrorText(env))
	}
	if seenAuth != "Bearer ai-studio-key" {
		t.Fatalf("outbound Authorization = %q, want the ai-studio backend's own key", seenAuth)
	}
}

// TestExecuteReturnsUpstreamErrorAsClientError keeps a backend error visible
// rather than turning it into an opaque 502.
func TestExecuteReturnsUpstreamErrorAsClientError(t *testing.T) {
	host := newScriptedHost()
	host.httpDo = func(req hostHTTPReq) (int, http.Header, []byte, error) {
		return http.StatusTooManyRequests, http.Header{"Content-Type": []string{"application/json"}},
			[]byte(`{"error":{"message":"quota exhausted for this account","code":"rate_limited"}}`), nil
	}
	m := newTestManager(t, host)
	raw, _ := m.executeWithContext(t.Context(), executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model:   "ai-studio/gemini-2.5-pro",
		Payload: []byte(`{"model":"ai-studio/gemini-2.5-pro"}`),
	}})
	env := decodeTestEnvelope(t, raw)
	if env.OK {
		t.Fatal("an upstream 429 was reported as success")
	}
	if env.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", env.Error.HTTPStatus)
	}
	if !strings.Contains(env.Error.Message, "quota exhausted") {
		t.Fatalf("the upstream message was lost: %s", env.Error.Message)
	}
}

// TestExecuteReportsCancellationWithoutCallingUpstream proves the cancel path:
// a cancelled context must not produce an upstream call that nobody waits for.
func TestExecuteReportsCancellationWithoutCallingUpstream(t *testing.T) {
	host := newScriptedHost()
	host.httpDo = func(req hostHTTPReq) (int, http.Header, []byte, error) {
		return 0, nil, nil, errString("context canceled")
	}
	m := newTestManager(t, host)
	ctx, cancel := contextWithTimeout(0)
	cancel()
	raw, _ := m.executeWithContext(ctx, executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model:   "ai-studio/gemini-2.5-pro",
		Payload: []byte(`{"model":"ai-studio/gemini-2.5-pro"}`),
	}})
	env := decodeTestEnvelope(t, raw)
	if env.OK {
		t.Fatal("a cancelled request was reported as success")
	}
	if env.Error.Code != "cancelled" {
		t.Fatalf("code = %q, want cancelled", env.Error.Code)
	}
}

// --- streaming ---

func TestExecuteStreamForwardsChunksAndRewritesModel(t *testing.T) {
	host := newScriptedHost()
	host.httpDo = func(req hostHTTPReq) (int, http.Header, []byte, error) {
		return http.StatusOK, http.Header{"Content-Type": []string{"text/event-stream"}}, nil, nil
	}
	host.streamChunks = []hostStreamReadResp{
		{Payload: []byte("data: {\"model\":\"gemini-2.5-pro\",\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n")},
		{Payload: []byte("data: {\"model\":\"gemini-2.5-pro\",\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n")},
		{Done: true},
	}
	host.count(pluginabi.MethodHostStreamEmit)

	m := newTestManager(t, host)
	req := executorRequest{StreamID: "downstream-1", ExecutorRequest: pluginapi.ExecutorRequest{
		Model:   "ai-studio/gemini-2.5-pro",
		Payload: []byte(`{"model":"ai-studio/gemini-2.5-pro","stream":true}`),
	}}
	raw, err := m.executeStream(req)
	if err != nil {
		t.Fatalf("executeStream: %v", err)
	}
	env := decodeTestEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("stream start failed: %s", envelopeErrorText(env))
	}

	// The pump runs in the background; wait for it to drain the scripted chunks.
	waitFor(t, func() bool { return host.count(pluginabi.MethodHostStreamClose) >= 1 })

	emitted := host.bodies[pluginabi.MethodHostStreamEmit]
	if len(emitted) != 2 {
		t.Fatalf("emitted %d chunks, want 2", len(emitted))
	}
	var msg struct {
		StreamID string `json:"stream_id"`
		Payload  []byte `json:"payload"`
	}
	if err := json.Unmarshal(emitted[0], &msg); err != nil {
		t.Fatalf("decode emit body: %v", err)
	}
	if msg.StreamID != "downstream-1" {
		t.Fatalf("emit went to %q, want the downstream id", msg.StreamID)
	}
	if !strings.Contains(string(msg.Payload), "ai-studio/gemini-2.5-pro") {
		t.Fatalf("the emitted chunk still carries the bare model id: %s", msg.Payload)
	}
}

func TestExecuteStreamReportsPreBodyUpstreamError(t *testing.T) {
	host := newScriptedHost()
	host.httpDo = func(req hostHTTPReq) (int, http.Header, []byte, error) {
		return http.StatusUnauthorized, http.Header{"Content-Type": []string{"application/json"}}, nil, nil
	}
	host.streamChunks = []hostStreamReadResp{{Payload: []byte(`{"error":{"message":"backend key rejected"}}`)}}
	m := newTestManager(t, host)
	raw, _ := m.executeStream(executorRequest{StreamID: "d1", ExecutorRequest: pluginapi.ExecutorRequest{
		Model:   "ai-studio/gemini-2.5-pro",
		Payload: []byte(`{"model":"ai-studio/gemini-2.5-pro"}`),
	}})
	env := decodeTestEnvelope(t, raw)
	if env.OK {
		t.Fatal("an upstream 401 was reported as a successful stream start")
	}
	if env.Error.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", env.Error.HTTPStatus)
	}
}

// TestIdleGuardTripsOnSilenceAndRearmsOnChunks is the acceptance criterion
// "流空闲超时独立可配": the budget bounds the GAP, not the total.
func TestIdleGuardTripsOnSilenceAndRearmsOnChunks(t *testing.T) {
	tripped := make(chan struct{}, 1)
	guard := newIdleGuard(20*1e6, func() { tripped <- struct{}{} }) // 20ms
	defer guard.Stop()

	// Keep rearming well inside the budget: it must NOT trip.
	for i := 0; i < 10; i++ {
		guard.Rearm()
		sleepMs(8)
	}
	guard.Stop()
	if guard.Tripped() {
		t.Fatal("the idle guard tripped while chunks kept arriving")
	}

	// Now let it lapse.
	tripped2 := make(chan struct{}, 1)
	guard2 := newIdleGuard(15*1e6, func() { tripped2 <- struct{}{} })
	defer guard2.Stop()
	select {
	case <-tripped2:
		if !guard2.Tripped() {
			t.Fatal("the callback fired but the guard does not report it tripped")
		}
	case <-timeAfterMs(400):
		t.Fatal("the idle guard never tripped on silence")
	}
}

func TestIdleGuardDisabledWithZeroBudget(t *testing.T) {
	guard := newIdleGuard(0, func() { t.Fatal("a disabled guard must never trip") })
	defer guard.Stop()
	guard.Rearm()
	if guard.Tripped() {
		t.Fatal("a zero-budget guard reported tripped")
	}
}

// --- envelope helpers ---

func decodeTestEnvelope(t *testing.T, raw []byte) pluginabi.Envelope {
	t.Helper()
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("response is not an envelope: %v (%s)", err, truncate(string(raw), 200))
	}
	return env
}

func envelopeErrorText(env pluginabi.Envelope) string {
	if env.Error == nil {
		return ""
	}
	return env.Error.Message
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		sleepMs(5)
	}
	t.Fatal("condition was not met in time")
}
