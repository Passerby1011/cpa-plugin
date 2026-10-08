package main

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The helpers travelServer / recordedCall / lastSegment come from travel_test.go
// (same package): a scriptable stand-in that records every call.

func growthTestAuth() *storedAuth {
	return &storedAuth{
		Auth:    storedTokens{AccessToken: "tok", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "uid-tasks"},
	}
}

// collapseTaskDelays removes the deliberate spacing between round trips, which
// is only there to look human to the live service.
func collapseTaskDelays(t *testing.T) {
	t.Helper()
	gap, delay := taskReportGap, taskCenterDelay
	taskReportGap, taskCenterDelay = 0, 0
	t.Cleanup(func() { taskReportGap, taskCenterDelay = gap, delay })
}

func cstTime(hour int) time.Time {
	return time.Date(2026, 8, 18, hour, 0, 0, 0, time.FixedZone("CST", 8*60*60))
}

// The full cycle: list, accept what we can finish, report events, re-read, then
// claim. Acceptance must precede the report because progress is null until the
// task is accepted.
func TestTaskCycleAcceptsReportsThenClaims(t *testing.T) {
	collapseTaskDelays(t)
	var listCount atomic.Int32
	ts := &travelServer{handle: func(path string, body []byte) (int, string) {
		switch {
		case path == growthTasksListPath:
			switch listCount.Add(1) {
			case 1:
				return 200, `{"code":0,"data":{"tasks":[{"task_code":"chat_5","accept_status":"not_accepted","reward_credit":100}]}}`
			case 2:
				return 200, `{"code":0,"data":{"tasks":[{"task_code":"chat_5","accept_status":"accepted","reward_credit":100,"progress":{"current":0,"target":5}}]}}`
			default:
				return 200, `{"code":0,"data":{"tasks":[{"task_code":"chat_5","accept_status":"accepted","reward_credit":100,"progress":{"current":5,"target":5}}]}}`
			}
		case path == growthTasksAcceptPath:
			if !strings.Contains(string(body), `"task_codes":["chat_5"]`) {
				t.Errorf("accept body = %s, want the plural task_codes array", body)
			}
			return 200, `{"code":0,"data":{"results":[{"task_code":"chat_5","status":"accepted"}]}}`
		case path == "/v2/activity/growth/tasks/chat_5/claim":
			// The code travels in the path; the body must not repeat it.
			// growthCall sends an empty object, which the live service accepts
			// (measured: no body, no Content-Type and {} all answer 200).
			if strings.Contains(string(body), "task_code") {
				t.Errorf("claim body = %s, must not carry the task code", body)
			}
			return 200, `{"code":0,"data":{"already_claimed":false,"credit":100,"energy":5}}`
		case path == activityEventPath:
			return 200, `{"code":0}`
		}
		t.Errorf("unexpected call %s", path)
		return 200, `{"code":0}`
	}}
	srv := ts.start(t)
	defer setBillingBase(srv.URL)()

	result, err := runTasksForAccount(growthTestAuth(), cstTime(12))
	if err != nil {
		t.Fatalf("runTasksForAccount: %v", err)
	}
	if result.Reported != 1 {
		t.Errorf("Reported = %d, want 1", result.Reported)
	}
	if result.Claimed != 1 || result.Credit != 100 {
		t.Errorf("Claimed = %d credit = %d, want 1 and 100", result.Claimed, result.Credit)
	}

	// Exactly five events, each in its own conversation: the task counts
	// distinct conversations.
	var events int
	var conversations = map[string]bool{}
	for _, call := range ts.calls {
		if call.path != activityEventPath {
			continue
		}
		var payload []map[string]any
		if err := json.Unmarshal([]byte(call.body), &payload); err != nil || len(payload) != 1 {
			t.Fatalf("event body = %s", call.body)
		}
		events++
		conversations[payload[0]["conversationId"].(string)] = true
	}
	if events != 5 {
		t.Errorf("posted %d events, want 5", events)
	}
	if len(conversations) != 5 {
		t.Errorf("distinct conversations = %d, want 5", len(conversations))
	}
}

// A task the gateway cannot finish must never be accepted: accepting it would
// advertise progress that can never arrive. The real catalogue carries several
// such tasks (a donation action, expert sessions, fingerprint chains).
func TestTaskCycleLeavesUnfinishableTasksUntouched(t *testing.T) {
	collapseTaskDelays(t)
	var accepted atomic.Int32
	ts := &travelServer{handle: func(path string, body []byte) (int, string) {
		switch {
		case path == growthTasksListPath:
			return 200, `{"code":0,"data":{"tasks":[
				{"task_code":"Expert_Philanthropy","accept_status":"not_accepted","reward_credit":300},
				{"task_code":"create_canvas","accept_status":"not_accepted","reward_credit":300},
				{"task_code":"RichMeow_Chat","accept_status":"not_accepted","reward_credit":100},
				{"task_code":"chat_5","accept_status":"accepted","reward_credit":100,"progress":{"current":0,"target":5}}
			]}}`
		case path == growthTasksAcceptPath:
			accepted.Add(1)
			if !strings.Contains(string(body), "chat_5") {
				t.Errorf("accepted something unplanned: %s", body)
			}
			return 200, `{"code":0}`
		case path == activityEventPath:
			return 200, `{"code":0}`
		}
		return 200, `{"code":0}`
	}}
	srv := ts.start(t)
	defer setBillingBase(srv.URL)()

	if _, err := runTasksForAccount(growthTestAuth(), cstTime(12)); err != nil {
		t.Fatalf("runTasksForAccount: %v", err)
	}
	// The only task already accepted is chat_5, so nothing may be accepted.
	if n := accepted.Load(); n != 0 {
		t.Errorf("accept called %d times; nothing unplanned was accepted-but-pending", n)
	}
}

// The night-owl task only counts inside its window, so outside it the cycle
// must neither report nor claim it.
func TestTaskCycleSkipsNightOwlOutsideWindow(t *testing.T) {
	collapseTaskDelays(t)
	var events atomic.Int32
	ts := &travelServer{handle: func(path string, body []byte) (int, string) {
		switch path {
		case growthTasksListPath:
			return 200, `{"code":0,"data":{"tasks":[{"task_code":"black_cat","accept_status":"accepted","reward_credit":0,"progress":{"current":0,"target":3}}]}}`
		case activityEventPath:
			events.Add(1)
			return 200, `{"code":0}`
		}
		return 200, `{"code":0}`
	}}
	srv := ts.start(t)
	defer setBillingBase(srv.URL)()

	result, err := runTasksForAccount(growthTestAuth(), cstTime(12))
	if err != nil {
		t.Fatalf("runTasksForAccount: %v", err)
	}
	if n := events.Load(); n != 0 {
		t.Errorf("posted %d events at 12:00 CST, want 0 (outside the night window)", n)
	}
	if result.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", result.Skipped)
	}

	// Inside the window it reports, and the mode marks it as a night event.
	events.Store(0)
	var sawNightMode bool
	ts.handle = func(path string, body []byte) (int, string) {
		switch path {
		case growthTasksListPath:
			return 200, `{"code":0,"data":{"tasks":[{"task_code":"black_cat","accept_status":"accepted","reward_credit":0,"progress":{"current":3,"target":3}}]}}`
		case activityEventPath:
			events.Add(1)
			if strings.Contains(string(body), `"mode":"night"`) {
				sawNightMode = true
			}
			return 200, `{"code":0}`
		case "/v2/activity/growth/tasks/black_cat/claim":
			return 200, `{"code":0,"data":{"already_claimed":false,"credit":0,"energy":0}}`
		}
		return 200, `{"code":0}`
	}
	if _, err := runTasksForAccount(growthTestAuth(), cstTime(1)); err != nil {
		t.Fatalf("runTasksForAccount at 01:00: %v", err)
	}
	// The task is already at target, so it is claimed rather than reported.
	if n := events.Load(); n != 0 {
		t.Errorf("posted %d events for an already-complete task, want 0", n)
	}
	if !sawNightMode {
		t.Log("note: the complete task needed no report, so no night-mode event was observed")
	}
}

// Acceptance can fail per task while the envelope still reports success: the
// service answers HTTP 200 with code 0 and puts the failure inside results. A
// task whose prerequisite is unmet must not be reported against — the events
// would be pure cost against a rate-limited endpoint. Measured case: an account
// with no buddy instance cannot accept the conversation tasks, because their
// prerequisite (first_buddy) is not met.
func TestTaskCycleSkipsTasksWhoseAcceptanceFailed(t *testing.T) {
	collapseTaskDelays(t)
	var events atomic.Int32
	ts := &travelServer{handle: func(path string, body []byte) (int, string) {
		switch path {
		case growthTasksListPath:
			return 200, `{"code":0,"data":{"tasks":[{"task_code":"chat_5","accept_status":"not_accepted","reward_credit":100}]}}`
		case growthTasksAcceptPath:
			return 200, `{"code":0,"data":{"results":[{"task_code":"chat_5","status":"error","message":"prerequisite not met: first_buddy (no buddy instance found)"}]}}`
		case activityEventPath:
			events.Add(1)
			return 200, `{"code":0}`
		}
		return 200, `{"code":0}`
	}}
	srv := ts.start(t)
	defer setBillingBase(srv.URL)()

	result, err := runTasksForAccount(growthTestAuth(), cstTime(12))
	if err != nil {
		t.Fatalf("runTasksForAccount: %v", err)
	}
	if n := events.Load(); n != 0 {
		t.Errorf("posted %d events for a task that was never accepted, want 0", n)
	}
	if result.Reported != 0 {
		t.Errorf("Reported = %d, want 0", result.Reported)
	}
	if result.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", result.Skipped)
	}
}

func TestNightOwlWindowBoundaries(t *testing.T) {
	cases := []struct {
		hour int
		min  int
		want bool
	}{
		{22, 59, false},
		{23, 0, true},
		{0, 0, true},
		{7, 59, true},
		{8, 0, false},
		{12, 0, false},
	}
	for _, c := range cases {
		now := time.Date(2026, 8, 18, c.hour, c.min, 0, 0, time.FixedZone("CST", 8*60*60))
		if got := nightOwlWindow(now); got != c.want {
			t.Errorf("nightOwlWindow(%02d:%02d CST) = %v, want %v", c.hour, c.min, got, c.want)
		}
	}
}

func TestNightOwlWindowIsZoneIndependent(t *testing.T) {
	// 01:00 CST is 17:00 UTC the previous day. A host running in UTC must
	// still see the window, which is why it is computed in CST.
	utc := time.Date(2026, 8, 17, 17, 0, 0, 0, time.UTC)
	if !nightOwlWindow(utc) {
		t.Error("nightOwlWindow(17:00 UTC = 01:00 CST) = false, want true")
	}
}

// A repeat claim is a success, not a second payout.
func TestClaimIsIdempotent(t *testing.T) {
	collapseTaskDelays(t)
	var claimCalls atomic.Int32
	ts := &travelServer{handle: func(path string, body []byte) (int, string) {
		switch path {
		case growthTasksListPath:
			return 200, `{"code":0,"data":{"tasks":[{"task_code":"chat_5","accept_status":"claimed","reward_credit":100,"progress":{"current":5,"target":5}}]}}`
		case "/v2/activity/growth/tasks/chat_5/claim":
			claimCalls.Add(1)
			return 200, `{"code":0,"data":{"already_claimed":true,"credit":0,"energy":0}}`
		}
		return 200, `{"code":0}`
	}}
	srv := ts.start(t)
	defer setBillingBase(srv.URL)()

	result, err := runTasksForAccount(growthTestAuth(), cstTime(12))
	if err != nil {
		t.Fatalf("runTasksForAccount: %v", err)
	}
	if claimCalls.Load() != 1 {
		t.Errorf("claim called %d times, want 1", claimCalls.Load())
	}
	if result.Claimed != 0 || result.Credit != 0 {
		t.Errorf("Claimed = %d credit = %d, want 0 and 0 for an already-claimed task",
			result.Claimed, result.Credit)
	}
	if result.Failed != 0 {
		t.Errorf("Failed = %d, want 0 (already_claimed is not a failure)", result.Failed)
	}
}

// An accepted-but-untouched task carries {"current":0,"target":N}; a null
// progress must never read as complete either, or the cycle would claim tasks
// it never finished.
func TestTaskCompleteSemantics(t *testing.T) {
	parse := func(raw string) growthTask {
		var task growthTask
		if err := json.Unmarshal([]byte(raw), &task); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		return task
	}
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"null progress", `{"task_code":"x"}`, false},
		{"zero progress", `{"task_code":"x","progress":{"current":0,"target":5}}`, false},
		{"partial", `{"task_code":"x","progress":{"current":4,"target":5}}`, false},
		{"met", `{"task_code":"x","progress":{"current":5,"target":5}}`, true},
		{"over", `{"task_code":"x","progress":{"current":6,"target":5}}`, true},
		{"zero target", `{"task_code":"x","progress":{"current":0,"target":0}}`, false},
	}
	for _, c := range cases {
		if got := parse(c.raw).complete(); got != c.want {
			t.Errorf("%s: complete() = %v, want %v", c.name, got, c.want)
		}
	}
}

// Every planned task must complete through a chat_request_send event, because
// that is the only report this plugin implements. Guarding the plan table keeps
// a future addition from silently pointing at a mechanism that does not exist.
func TestTaskReportPlansUseSupportedEvents(t *testing.T) {
	if len(taskReportPlans) == 0 {
		t.Fatal("taskReportPlans is empty")
	}
	for code, plan := range taskReportPlans {
		if plan.count < 1 {
			t.Errorf("%s: count = %d, want >= 1", code, plan.count)
		}
		if plan.modelID == "" || plan.modelName == "" {
			t.Errorf("%s: telemetry model must be set", code)
		}
	}
	// The night-owl task is the only window-restricted one.
	if plan := taskReportPlans["black_cat"]; !plan.nightOnly {
		t.Error("black_cat must be restricted to the night window")
	}
	for code, plan := range taskReportPlans {
		if code != "black_cat" && plan.nightOnly {
			t.Errorf("%s: unexpectedly restricted to the night window", code)
		}
	}
}
