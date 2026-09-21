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

// travelServer is a scriptable stand-in for the growth endpoints. It records
// every (method, path, body) so tests can assert the exact call sequence.
type travelServer struct {
	mu     sync.Mutex
	calls  []recordedCall
	handle func(path string, body []byte) (int, string)
}

type recordedCall struct {
	method string
	path   string
	body   string
}

func (s *travelServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.calls = append(s.calls, recordedCall{method: r.Method, path: r.URL.Path, body: string(raw)})
		s.mu.Unlock()
		status, payload := s.handle(r.URL.Path, raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *travelServer) paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.calls))
	for _, c := range s.calls {
		out = append(out, c.method+" "+lastSegment(c.path))
	}
	return out
}

func lastSegment(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func travelTestAuth() *storedAuth {
	return &storedAuth{
		Auth:    storedTokens{AccessToken: "tok", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "uid-travel"},
	}
}

// A fresh account with no cat: agree to the terms, then adopt. Nothing else.
func TestTravelAdoptsWhenNoCat(t *testing.T) {
	resetAdoptionTried()
	ts := &travelServer{handle: func(path string, body []byte) (int, string) {
		switch lastSegment(path) {
		case "info":
			return 200, `{"code":0,"data":{"buddy":null}}`
		case "agreement", "first":
			return 200, `{"code":0,"data":{}}`
		default:
			t.Errorf("unexpected call %s", path)
			return 200, `{"code":0}`
		}
	}}
	srv := ts.start(t)
	defer setBillingBase(srv.URL)()

	outcome, err := runTravelForAccount(travelTestAuth(), time.Now(), false)
	if err != nil {
		t.Fatalf("runTravelForAccount: %v", err)
	}
	if outcome.Action != "adopted" {
		t.Errorf("action = %q, want adopted", outcome.Action)
	}
	got := ts.paths()
	want := []string{"GET info", "POST agreement", "POST first"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// Already has a cat, idle, no trip taken today -> depart.
func TestTravelDepartsWhenIdle(t *testing.T) {
	resetAdoptionTried()
	ts := &travelServer{handle: func(path string, body []byte) (int, string) {
		switch lastSegment(path) {
		case "info":
			return 200, `{"code":0,"data":{"buddy":{"id":7,"name":"cat"}}}`
		case "status":
			return 200, `{"code":0,"data":{"state":"idle","daily_limit_reached":false}}`
		case "depart":
			if !strings.Contains(string(body), `"location_id"`) {
				t.Errorf("depart body missing location_id: %s", body)
			}
			return 200, `{"code":0,"data":{}}`
		default:
			t.Errorf("unexpected call %s", path)
			return 200, `{"code":0}`
		}
	}}
	srv := ts.start(t)
	defer setBillingBase(srv.URL)()

	outcome, err := runTravelForAccount(travelTestAuth(), time.Now(), false)
	if err != nil {
		t.Fatalf("runTravelForAccount: %v", err)
	}
	if outcome.Action != "departed" {
		t.Errorf("action = %q, want departed", outcome.Action)
	}
}

// Idle but today's trip is already used -> nothing to do.
func TestTravelSkipsWhenDailyLimitReached(t *testing.T) {
	resetAdoptionTried()
	ts := &travelServer{handle: func(path string, body []byte) (int, string) {
		switch lastSegment(path) {
		case "info":
			return 200, `{"code":0,"data":{"buddy":{"id":7}}}`
		case "status":
			return 200, `{"code":0,"data":{"state":"idle","daily_limit_reached":true}}`
		default:
			t.Errorf("must not call %s once the daily limit is reached", path)
			return 200, `{"code":0}`
		}
	}}
	srv := ts.start(t)
	defer setBillingBase(srv.URL)()

	outcome, err := runTravelForAccount(travelTestAuth(), time.Now(), false)
	if err != nil {
		t.Fatalf("runTravelForAccount: %v", err)
	}
	if outcome.Action != "skipped" {
		t.Errorf("action = %q, want skipped", outcome.Action)
	}
}

// Arrived -> claim, and the reward must be surfaced.
func TestTravelClaimsArrivedTrip(t *testing.T) {
	resetAdoptionTried()
	ts := &travelServer{handle: func(path string, body []byte) (int, string) {
		switch lastSegment(path) {
		case "info":
			return 200, `{"code":0,"data":{"buddy":{"id":7}}}`
		case "status":
			return 200, `{"code":0,"data":{"state":"arrived","record_id":42,"reward_credit":15}}`
		case "claim":
			if !strings.Contains(string(body), `"record_id":42`) {
				t.Errorf("claim must carry record_id: %s", body)
			}
			return 200, `{"code":0,"data":{"reward_credit":15}}`
		default:
			t.Errorf("unexpected call %s", path)
			return 200, `{"code":0}`
		}
	}}
	srv := ts.start(t)
	defer setBillingBase(srv.URL)()

	outcome, err := runTravelForAccount(travelTestAuth(), time.Now(), false)
	if err != nil {
		t.Fatalf("runTravelForAccount: %v", err)
	}
	if outcome.Action != "claimed" {
		t.Errorf("action = %q, want claimed", outcome.Action)
	}
	if outcome.Reward != 15 {
		t.Errorf("reward = %d, want 15", outcome.Reward)
	}
}

// Arrived without a record id: nothing can be claimed, so do not call claim.
func TestTravelArrivedWithoutRecordIDSkips(t *testing.T) {
	resetAdoptionTried()
	ts := &travelServer{handle: func(path string, body []byte) (int, string) {
		switch lastSegment(path) {
		case "info":
			return 200, `{"code":0,"data":{"buddy":{"id":7}}}`
		case "status":
			return 200, `{"code":0,"data":{"state":"arrived","record_id":0}}`
		default:
			t.Errorf("must not call %s without a record id", path)
			return 200, `{"code":0}`
		}
	}}
	srv := ts.start(t)
	defer setBillingBase(srv.URL)()

	outcome, err := runTravelForAccount(travelTestAuth(), time.Now(), false)
	if err != nil {
		t.Fatalf("runTravelForAccount: %v", err)
	}
	if outcome.Action != "skipped" {
		t.Errorf("action = %q, want skipped", outcome.Action)
	}
}

// A trip in progress must be left alone: neither depart nor claim.
func TestTravelLeavesTravelingTripAlone(t *testing.T) {
	resetAdoptionTried()
	ts := &travelServer{handle: func(path string, body []byte) (int, string) {
		switch lastSegment(path) {
		case "info":
			return 200, `{"code":0,"data":{"buddy":{"id":7}}}`
		case "status":
			return 200, `{"code":0,"data":{"state":"traveling","record_id":9}}`
		default:
			t.Errorf("must not call %s while traveling", path)
			return 200, `{"code":0}`
		}
	}}
	srv := ts.start(t)
	defer setBillingBase(srv.URL)()

	outcome, err := runTravelForAccount(travelTestAuth(), time.Now(), false)
	if err != nil {
		t.Fatalf("runTravelForAccount: %v", err)
	}
	if outcome.Action != "skipped" {
		t.Errorf("action = %q, want skipped", outcome.Action)
	}
}

// An unmeter gate is an expected refusal, not a failure: report a clean skip
// and remember it so the same day is not retried.
func TestTravelAdoptionGateRefusalDebounces(t *testing.T) {
	resetAdoptionTried()
	var firstCalls int
	ts := &travelServer{handle: func(path string, body []byte) (int, string) {
		switch lastSegment(path) {
		case "info":
			return 200, `{"code":0,"data":{"buddy":null}}`
		case "agreement":
			return 200, `{"code":0,"data":{}}`
		case "first":
			firstCalls++
			return 400, `{"code":400,"msg":"first_buddy task not completed yet"}`
		default:
			return 200, `{"code":0}`
		}
	}}
	srv := ts.start(t)
	defer setBillingBase(srv.URL)()
	sa := travelTestAuth()
	now := time.Now()

	outcome, err := runTravelForAccount(sa, now, false)
	if err != nil {
		t.Fatalf("a gate refusal must not be an error: %v", err)
	}
	if outcome.Action != "skipped" {
		t.Errorf("action = %q, want skipped", outcome.Action)
	}
	if firstCalls != 1 {
		t.Fatalf("adopt attempted %d times, want 1", firstCalls)
	}

	// Second run the same day: no adoption attempt at all.
	outcome, err = runTravelForAccount(sa, now, false)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if outcome.Action != "skipped" {
		t.Errorf("action = %q, want skipped", outcome.Action)
	}
	if firstCalls != 1 {
		t.Errorf("adopt retried the same day (%d attempts); the debounce is not working", firstCalls)
	}

	// force=true (used right after a successful activity report) retries.
	if _, err := runTravelForAccount(sa, now, true); err != nil {
		t.Fatalf("forced run: %v", err)
	}
	if firstCalls != 2 {
		t.Errorf("forced run did not retry adoption (%d attempts, want 2)", firstCalls)
	}
}

// The debounce is per-day: a new day retries.
func TestTravelAdoptionDebounceIsPerDay(t *testing.T) {
	resetAdoptionTried()
	sa := travelTestAuth()
	day1 := time.Date(2026, 8, 18, 9, 0, 0, 0, cst)
	markAdoptionTried(sa.Account.UID, day1)
	if !shouldSkipAdoption(sa.Account.UID, day1, false) {
		t.Error("same day must be debounced")
	}
	day2 := day1.Add(24 * time.Hour)
	if shouldSkipAdoption(sa.Account.UID, day2, false) {
		t.Error("next day must not be debounced")
	}
}

// Adoption success clears the debounce so a later gate refusal can re-mark it.
func TestTravelAdoptionSuccessClearsDebounce(t *testing.T) {
	resetAdoptionTried()
	sa := travelTestAuth()
	markAdoptionTried(sa.Account.UID, time.Now())
	clearAdoptionTried(sa.Account.UID)
	if shouldSkipAdoption(sa.Account.UID, time.Now(), false) {
		t.Error("a successful adoption must clear the debounce")
	}
}

// Enterprise and Global accounts must be skipped with no upstream call at all.
func TestRunTravelSkipsIneligibleAccounts(t *testing.T) {
	// activityEligible is the shared gate; assert it here so a future change to
	// either feature cannot silently start calling these endpoints.
	enterprise := &storedAuth{
		Auth:    storedTokens{AccessToken: "tok", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "u1", EnterpriseID: "e1"},
	}
	if activityEligible(enterprise) {
		t.Error("enterprise accounts must be ineligible")
	}
	isGlobal := &storedAuth{
		Auth:    storedTokens{AccessToken: "tok", Domain: "www.workbuddy.ai"},
		Account: storedAccount{UID: "u1"},
	}
	if activityEligible(isGlobal) {
		t.Error("Global accounts must be ineligible")
	}
}

// The CST day helper must not depend on the host's local zone.
func TestTravelDayUsesCST(t *testing.T) {
	// 2026-08-18 16:30 UTC == 2026-08-19 00:30 CST.
	utc := time.Date(2026, 8, 18, 16, 30, 0, 0, time.UTC)
	if got := travelDay(utc); got != "2026-08-19" {
		t.Errorf("travelDay(%v) = %q, want 2026-08-19", utc, got)
	}
	// 2026-08-18 15:30 UTC == 2026-08-18 23:30 CST.
	utc = time.Date(2026, 8, 18, 15, 30, 0, 0, time.UTC)
	if got := travelDay(utc); got != "2026-08-18" {
		t.Errorf("travelDay(%v) = %q, want 2026-08-18", utc, got)
	}
}

// resetAdoptionTried clears the process-wide debounce map between tests.
func resetAdoptionTried() {
	adoptionMu.Lock()
	adoptionTried = map[string]string{}
	adoptionMu.Unlock()
}

// guard against the envelope shape drifting: travelCall unwraps data.
func TestTravelCallUnwrapsEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"state":"idle"}}`))
	}))
	defer srv.Close()
	defer setBillingBase(srv.URL)()

	data, err := travelCall(travelTestAuth(), http.MethodGet, travelStatusPath, nil)
	if err != nil {
		t.Fatalf("travelCall: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out["state"] != "idle" {
		t.Errorf("unwrapped data = %v", out)
	}
}
