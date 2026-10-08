package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// activityEligible encodes the measured scope rules: the growth subsystem
// serves CN personal accounts only. Enterprise accounts answer 403 and Global
// accounts answer 500, so both must be skipped before any upstream call.
func TestActivityEligible(t *testing.T) {
	cases := []struct {
		name string
		sa   *storedAuth
		want bool
	}{
		{
			name: "CN personal is eligible",
			sa: &storedAuth{
				Auth:    storedTokens{AccessToken: "tok", Domain: "www.codebuddy.cn"},
				Account: storedAccount{UID: "u1"},
			},
			want: true,
		},
		{
			name: "CN enterprise is not eligible (403 growth only for personal)",
			sa: &storedAuth{
				Auth:    storedTokens{AccessToken: "tok", Domain: "www.codebuddy.cn"},
				Account: storedAccount{UID: "u1", EnterpriseID: "ent-1"},
			},
			want: false,
		},
		{
			name: "Global is not eligible (streak 500 / travel empty)",
			sa: &storedAuth{
				Auth:    storedTokens{AccessToken: "tok", Domain: "www.workbuddy.ai"},
				Account: storedAccount{UID: "u1"},
			},
			want: false,
		},
		{
			name: "empty domain behaves as CN and is eligible",
			sa: &storedAuth{
				Auth:    storedTokens{AccessToken: "tok"},
				Account: storedAccount{UID: "u1"},
			},
			want: true,
		},
		{
			name: "missing uid is skipped",
			sa: &storedAuth{
				Auth: storedTokens{AccessToken: "tok", Domain: "www.codebuddy.cn"},
			},
			want: false,
		},
		{
			name: "missing token is skipped",
			sa: &storedAuth{
				Auth:    storedTokens{Domain: "www.codebuddy.cn"},
				Account: storedAccount{UID: "u1"},
			},
			want: false,
		},
		{name: "nil auth is skipped", sa: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := activityEligible(tc.sa); got != tc.want {
				t.Errorf("activityEligible = %v, want %v", got, tc.want)
			}
		})
	}
}

// The event must carry every field the client sends. userId in particular is
// load-bearing: the upstream accepts the POST and silently discards the event
// when it is missing, so a stripped-down payload would look successful while
// never advancing the streak.
func TestChatRequestEventShape(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	event := newChatRequestEvent("uid-123", "conv-1", "conv-1-r1", now)

	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}

	required := []string{
		"eventCode", "timestamp", "reportDelay", "mode", "conversationId", "requestId",
		"inputLength", "requestModelId", "requestModelName", "isPlan",
		"isAutoExecuteTerminal", "isAutoModify", "codebaseEnable", "maxToken",
		"maxSteps", "temperature", "maxRetries", "mentionContexts", "knowledgeId",
		"knowledgeName", "codebaseId", "mentionContextCount", "command", "expertId",
		"recommendId", "skillId", "skillCount", "totalCount", "fileUri", "presentAt",
		"traceId", "rootRequestId", "parentConversationId", "agentName", "agentType",
		"userId",
	}
	for _, key := range required {
		if _, ok := decoded[key]; !ok {
			t.Errorf("event is missing field %q", key)
		}
	}

	if decoded["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode = %v", decoded["eventCode"])
	}
	if decoded["userId"] != "uid-123" {
		t.Errorf("userId = %v; it is required or the upstream drops the event", decoded["userId"])
	}
	if decoded["conversationId"] != "conv-1" || decoded["requestId"] != "conv-1-r1" {
		t.Errorf("ids = %v / %v", decoded["conversationId"], decoded["requestId"])
	}
	// The array fields must serialize as [] rather than null.
	for _, key := range []string{"mentionContexts", "knowledgeId", "knowledgeName"} {
		v, ok := decoded[key].([]any)
		if !ok || v == nil {
			t.Errorf("%s should be an empty array, got %#v", key, decoded[key])
		}
	}
}

// The report is a batch array carrying the account's UID and the caller's ids.
func TestActivityReportPayload(t *testing.T) {
	var gotPath, gotMethod string
	var sent []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		sent, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()
	defer setBillingBase(srv.URL)()

	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "tok", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "uid-9"},
	}
	if err := reportChatActivity(sa, "conv-7", "conv-7-r3"); err != nil {
		t.Fatalf("reportChatActivity: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if !strings.HasSuffix(gotPath, "/v2/report") {
		t.Errorf("path = %q, want .../v2/report", gotPath)
	}
	var batch []map[string]any
	if err := json.Unmarshal(sent, &batch); err != nil {
		t.Fatalf("payload is not a JSON array: %v (%s)", err, sent)
	}
	if len(batch) != 1 {
		t.Fatalf("batch size = %d, want 1", len(batch))
	}
	if batch[0]["conversationId"] != "conv-7" || batch[0]["requestId"] != "conv-7-r3" {
		t.Errorf("ids = %v / %v", batch[0]["conversationId"], batch[0]["requestId"])
	}
	if batch[0]["userId"] != "uid-9" {
		t.Errorf("userId = %v, want uid-9", batch[0]["userId"])
	}
}

// The streak oracle reads data.streak.days over GET.
func TestFetchGrowthStreakDays(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"streak":{"days":3,"month_total_days":6}}}`))
	}))
	defer srv.Close()
	defer setBillingBase(srv.URL)()

	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "tok", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "uid-9"},
	}
	days, err := fetchGrowthStreakDays(sa)
	if err != nil {
		t.Fatalf("fetchGrowthStreakDays: %v", err)
	}
	if days != 3 {
		t.Errorf("days = %d, want 3", days)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %s, want GET", gotMethod)
	}
	if !strings.HasSuffix(gotPath, "/activity/growth/streak") {
		t.Errorf("path = %q", gotPath)
	}
}

// An upstream failure must surface as an error rather than be read as "no
// streak", otherwise a broken gateway looks like a silent-drop account.
func TestFetchGrowthStreakDaysError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"msg":"internal server error"}`))
	}))
	defer srv.Close()
	defer setBillingBase(srv.URL)()

	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "tok", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "uid-9"},
	}
	if _, err := fetchGrowthStreakDays(sa); err == nil {
		t.Fatal("a 500 must be reported as an error, not as days=0")
	}
}

// A run reports exactly `count` events and spaces them out.
func TestRunActivityForAccountSendsCountEvents(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, raw)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()
	defer setBillingBase(srv.URL)()

	// Collapse the inter-event gap so the test stays fast.
	origGap, origDelay := activityReportGap, activityAccountDelay
	activityReportGap, activityAccountDelay = 0, 0
	defer func() { activityReportGap, activityAccountDelay = origGap, origDelay }()

	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "tok", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "uid-9"},
	}
	if err := runActivityForAccount(sa, 3); err != nil {
		t.Fatalf("runActivityForAccount: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("sent %d events, want 3", len(bodies))
	}
	// All three share one conversationId; each has its own requestId.
	var convIDs, reqIDs []string
	for _, raw := range bodies {
		var batch []map[string]any
		if err := json.Unmarshal(raw, &batch); err != nil || len(batch) != 1 {
			t.Fatalf("bad batch: %v (%s)", err, raw)
		}
		convIDs = append(convIDs, batch[0]["conversationId"].(string))
		reqIDs = append(reqIDs, batch[0]["requestId"].(string))
	}
	for i := 1; i < len(convIDs); i++ {
		if convIDs[i] != convIDs[0] {
			t.Errorf("conversationId drifted within one run: %v", convIDs)
		}
	}
	seen := map[string]bool{}
	for _, id := range reqIDs {
		if seen[id] {
			t.Errorf("duplicate requestId %q", id)
		}
		seen[id] = true
	}
}
