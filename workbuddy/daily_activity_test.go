package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// globalAuth builds a Global-realm personal credential.
func globalAuth() *storedAuth {
	return &storedAuth{
		Auth:    storedTokens{AccessToken: "tok-123", Domain: "www.workbuddy.ai"},
		Account: storedAccount{UID: "uid-abc"},
	}
}

func TestDailyActivityEligibility(t *testing.T) {
	cases := []struct {
		name string
		sa   *storedAuth
		want bool
	}{
		{"global personal", globalAuth(), true},
		{"nil", nil, false},
		{"no token", &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}, Account: storedAccount{UID: "u"}}, false},
		{"no uid", &storedAuth{Auth: storedTokens{AccessToken: "t", Domain: "www.workbuddy.ai"}}, false},
		{"cn account", &storedAuth{Auth: storedTokens{AccessToken: "t", Domain: "www.codebuddy.cn"}, Account: storedAccount{UID: "u"}}, false},
		{"empty domain (legacy cn)", &storedAuth{Auth: storedTokens{AccessToken: "t"}, Account: storedAccount{UID: "u"}}, false},
		{"global enterprise", &storedAuth{Auth: storedTokens{AccessToken: "t", Domain: "www.workbuddy.ai"}, Account: storedAccount{UID: "u", EnterpriseID: "ent-1"}}, false},
		{"subdomain global", &storedAuth{Auth: storedTokens{AccessToken: "t", Domain: "eu.workbuddy.ai"}, Account: storedAccount{UID: "u"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dailyActivityEligible(tc.sa); got != tc.want {
				t.Fatalf("dailyActivityEligible = %v, want %v", got, tc.want)
			}
		})
	}
}

// The web channel authenticates with exactly two credential headers and must NOT
// carry the desktop X-IDE-* fingerprint: the desktop-identity request does not
// earn the reward (measured upstream), the web one does.
func TestWebHeadersCarryOnlyBearerAndUserID(t *testing.T) {
	h := webHeadersFor(globalAuth())
	if h["Authorization"] != "Bearer tok-123" {
		t.Fatalf("Authorization = %q", h["Authorization"])
	}
	if h["X-User-Id"] != "uid-abc" {
		t.Fatalf("X-User-Id = %q", h["X-User-Id"])
	}
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-ide-") || strings.EqualFold(k, "X-Client-Platform") {
			t.Fatalf("web headers must not carry desktop fingerprint header %q", k)
		}
	}
	if h["Origin"] != webOriginGlobal || h["Referer"] != webOriginGlobal+"/app" {
		t.Fatalf("Origin/Referer must be the web origin, got %q / %q", h["Origin"], h["Referer"])
	}
}

// queueWebConversation must send the measured request body shape: prompt, model,
// conversationOrigin and the weixinpay plugin entry.
func TestQueueWebConversationBodyShape(t *testing.T) {
	var gotBody map[string]any
	var gotPath, gotAuth, gotUID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-User-Id")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"id":"conv-1"}}`))
	}))
	defer srv.Close()
	defer setWebBase(srv.URL)()

	id, err := queueWebConversation(globalAuth(), "Hi")
	if err != nil {
		t.Fatalf("queueWebConversation: %v", err)
	}
	if id != "conv-1" {
		t.Fatalf("conversation id = %q, want conv-1", id)
	}
	if gotPath != "/console/as/conversations/" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok-123" || gotUID != "uid-abc" {
		t.Fatalf("auth headers = %q / %q", gotAuth, gotUID)
	}
	if gotBody["prompt"] != "Hi" {
		t.Fatalf("prompt = %v", gotBody["prompt"])
	}
	if gotBody["model"] != dailyActivityModel {
		t.Fatalf("model = %v", gotBody["model"])
	}
	if gotBody["conversationOrigin"] != "workbuddy-app" {
		t.Fatalf("conversationOrigin = %v", gotBody["conversationOrigin"])
	}
	plugins, ok := gotBody["plugins"].([]any)
	if !ok || len(plugins) != 1 {
		t.Fatalf("plugins = %#v", gotBody["plugins"])
	}
	plugin, _ := plugins[0].(map[string]any)
	if plugin["name"] != "weixinpay" || plugin["marketplace"] != "codebuddy-builtin" {
		t.Fatalf("plugin entry = %#v", plugin)
	}
}

func TestQueueWebConversationRejectsNonZeroCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":40101,"msg":"unauthorized","data":null}`))
	}))
	defer srv.Close()
	defer setWebBase(srv.URL)()

	if _, err := queueWebConversation(globalAuth(), "Hi"); err == nil {
		t.Fatal("expected an error for a non-zero console code")
	}
}

func TestQueueWebConversationRequiresID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	defer srv.Close()
	defer setWebBase(srv.URL)()

	if _, err := queueWebConversation(globalAuth(), "Hi"); err == nil {
		t.Fatal("expected an error when the response carries no conversation id")
	}
}

func TestWebConversationSessionRequiresLinkAndToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sandbox not provisioned yet: no link/token.
		_, _ = w.Write([]byte(`{"code":0,"data":{"sessionId":"s1","cwd":"/workspace"}}`))
	}))
	defer srv.Close()
	defer setWebBase(srv.URL)()

	if _, _, _, _, err := webConversationSession(globalAuth(), "conv-1"); err == nil {
		t.Fatal("expected an error when link/token are missing")
	}
}

func TestWebConversationSessionReadsSandbox(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/conv-1/session") {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"link":"https://sandbox.example/x","token":"sb-token","sessionId":"sess-9","cwd":"/w"}}`))
	}))
	defer srv.Close()
	defer setWebBase(srv.URL)()

	link, token, sessionID, cwd, err := webConversationSession(globalAuth(), "conv-1")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if link != "https://sandbox.example/x" || token != "sb-token" || sessionID != "sess-9" || cwd != "/w" {
		t.Fatalf("got link=%q token=%q session=%q cwd=%q", link, token, sessionID, cwd)
	}
}

// The ACP drive is the part that makes the conversation count: open the SSE
// channel (Acp-Connection-Id), then initialize / session/load / session/prompt,
// then wait for the console to report completed.
func TestACPTurnDrivesConversationToCompletion(t *testing.T) {
	var methods []string
	// Shared with the console handler: flips once session/prompt is delivered.
	var (
		promptedMu sync.Mutex
		prompted   bool
	)
	// Sandbox server: SSE channel + JSON-RPC posts.
	sandbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if r.Header.Get("Accept") != "text/event-stream" {
				t.Errorf("SSE GET Accept = %q", r.Header.Get("Accept"))
			}
			if r.Header.Get("Authorization") != "Bearer sb-token" {
				t.Errorf("SSE GET Authorization = %q", r.Header.Get("Authorization"))
			}
			w.Header().Set("Acp-Connection-Id", "conn-1")
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// One update event then hold the stream open briefly.
			_, _ = w.Write([]byte("data: {\"method\":\"session/update\",\"params\":{\"update\":{\"sessionUpdate\":\"agent_message_chunk\"}}}\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(120 * time.Millisecond)
		case http.MethodPost:
			if r.Header.Get("Acp-Connection-Id") != "conn-1" {
				t.Errorf("POST Acp-Connection-Id = %q", r.Header.Get("Acp-Connection-Id"))
			}
			var msg struct {
				Method string `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&msg)
			methods = append(methods, msg.Method)
			if msg.Method == "session/prompt" {
				promptedMu.Lock()
				prompted = true
				promptedMu.Unlock()
			}
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer sandbox.Close()

	// Console server: status flips to completed once the sandbox received
	// session/prompt.
	// The sessionId deliberately DIFFERS from the conversation id: status is a
	// property of the conversation, so polling the session id must be treated as
	// a wrong resource. Returning a status for any path would hide that bug.
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/session") {
			_, _ = w.Write([]byte(`{"code":0,"data":{"link":"` + sandbox.URL + `","token":"sb-token","sessionId":"sess-9","cwd":"/w"}}`))
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/conv-1") {
			t.Errorf("status polled at %q; must poll the conversation id (/conv-1), not the session id", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"msg":"not found"}`))
			return
		}
		promptedMu.Lock()
		done := prompted
		promptedMu.Unlock()
		status := "CREATING"
		if done {
			status = "completed"
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"status":"` + status + `"}}`))
	}))
	defer console.Close()
	defer setWebBase(console.URL)()

	// Collapse the wait so the test is fast.
	oldTimeout, oldPoll := acpTurnTimeout, acpPollInterval
	acpTurnTimeout, acpPollInterval = 3*time.Second, 20*time.Millisecond
	defer func() { acpTurnTimeout, acpPollInterval = oldTimeout, oldPoll }()

	sa := globalAuth()
	link, token, sessionID, cwd, err := webConversationSession(sa, "conv-1")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	chunks, events, err := acpTurn(sa, "conv-1", link, token, sessionID, cwd, "Hi")
	if err != nil {
		t.Fatalf("acpTurn: %v", err)
	}
	if chunks < 1 || events < 1 {
		t.Fatalf("chunks=%d events=%d, want at least one of each", chunks, events)
	}
	want := []string{"initialize", "session/load", "session/prompt"}
	if len(methods) != len(want) {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
	for i := range want {
		if methods[i] != want[i] {
			t.Fatalf("methods = %v, want %v", methods, want)
		}
	}
}

func TestACPTurnRejectsMissingConnectionID(t *testing.T) {
	sandbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No Acp-Connection-Id header → the drive cannot proceed.
		w.WriteHeader(http.StatusOK)
	}))
	defer sandbox.Close()

	if _, _, err := acpTurn(globalAuth(), "conv-1", sandbox.URL, "tok", "s", "/w", "Hi"); err == nil {
		t.Fatal("expected an error when the SSE channel returns no Acp-Connection-Id")
	}
}

func TestACPTurnRejectsUnusableLink(t *testing.T) {
	if _, _, err := acpTurn(globalAuth(), "conv-1", "not-a-url", "tok", "s", "/w", "Hi"); err == nil {
		t.Fatal("expected an error for an unusable sandbox link")
	}
}

// One reward per account per calendar day: a second sweep must not re-drive an
// account that already succeeded today.
func TestDailyActivityDeduplicatesWithinDay(t *testing.T) {
	uid := "dedupe-uid"
	clearDailyActivityState()
	if dailyActivityDoneToday(uid) {
		t.Fatal("fresh uid must not be marked done")
	}
	rememberDailyActivity(uid)
	if !dailyActivityDoneToday(uid) {
		t.Fatal("uid should be marked done after a successful reward")
	}
	if dailyActivityDoneToday("") {
		t.Fatal("empty uid must never be considered done")
	}
}

// The runner must refuse a CN account outright rather than calling the Global
// web console API with it.
func TestRunDailyActivityRejectsCNAccount(t *testing.T) {
	cn := &storedAuth{Auth: storedTokens{AccessToken: "t", Domain: "www.codebuddy.cn"}, Account: storedAccount{UID: "u"}}
	res := runDailyActivityForAccount(cn)
	if ok, _ := res["ok"].(bool); ok {
		t.Fatalf("CN account must not be driven through the Global web channel: %#v", res)
	}
}

func TestStringifyAndFirstStringField(t *testing.T) {
	if stringify(float64(0)) != "0" || stringify("x") != "x" || stringify(nil) != "" {
		t.Fatalf("stringify mismatch: %q %q %q", stringify(float64(0)), stringify("x"), stringify(nil))
	}
	m := map[string]any{"link": "", "endpoint": "https://e"}
	if got := firstStringField(m, "link", "endpoint"); got != "https://e" {
		t.Fatalf("firstStringField = %q", got)
	}
}

// Regression: the manual button (and POST /daily-activity) must work with the
// auto toggle OFF — that is the default on a fresh install. The original bug
// ran the scheduled entry point, which returns immediately unless
// daily_activity_auto is on, so the button silently did nothing.
func TestManualDailyActivityIgnoresAutoToggle(t *testing.T) {
	clearDailyActivityState()
	// Toggle explicitly off (the default), then run the manual path.
	dailyActivityAutoMu.Lock()
	old := dailyActivityAuto
	dailyActivityAuto = false
	dailyActivityAutoMu.Unlock()
	defer func() {
		dailyActivityAutoMu.Lock()
		dailyActivityAuto = old
		dailyActivityAutoMu.Unlock()
	}()

	// The robust way to tell "gated" from "ran but found no accounts" without a
	// host bridge: probe the gate directly. listDailyActivityTargets is the
	// shared selection step both paths run after the gate.
	scheduledRan, _ := dailyActivitySweepAllowed(false)
	if scheduledRan {
		t.Fatal("scheduled sweep must be gated while the toggle is off")
	}
	manualRan, _ := dailyActivitySweepAllowed(true)
	if !manualRan {
		t.Fatal("manual sweep must NOT be gated by daily_activity_auto")
	}
}

// The scheduled sweep must remain gated: an off toggle means no attempts at all.
func TestScheduledDailyActivityRespectsToggle(t *testing.T) {
	dailyActivityAutoMu.Lock()
	old := dailyActivityAuto
	dailyActivityAuto = false
	dailyActivityAutoMu.Unlock()
	defer func() {
		dailyActivityAutoMu.Lock()
		dailyActivityAuto = old
		dailyActivityAutoMu.Unlock()
	}()

	allowed, _ := dailyActivitySweepAllowed(false)
	if allowed {
		t.Fatal("toggle off must refuse the scheduled sweep")
	}
	if got := runAutoDailyActivity(false); got.Attempts != 0 || got.Claimed != 0 {
		t.Fatalf("toggle off must produce no attempts, got %#v", got)
	}
}
