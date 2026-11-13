package allowance

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeControl is a scripted control plane. Every call is recorded (so a test
// can count it) and answered from a table keyed by URL suffix, which is what
// makes "the second call did not go to the network" an assertion rather than a
// guess.
type fakeControl struct {
	mu sync.Mutex
	// calls is every URL this "plane" was asked for, in order.
	calls []string
	// auth is every Authorization header it was sent.
	auth []string
	// reply maps a URL suffix onto the answer.
	reply map[string]fakeReply
}

type fakeReply struct {
	status int
	body   string
}

func newFakeControl() *fakeControl { return &fakeControl{reply: map[string]fakeReply{}} }

func (f *fakeControl) on(suffix string, status int, body string) *fakeControl {
	f.reply[suffix] = fakeReply{status: status, body: body}
	return f
}

// transport is the injected Transport.
func (f *fakeControl) transport() Transport {
	return func(_ context.Context, _ string, u string, headers map[string]string, _ []byte) (int, []byte, error) {
		f.mu.Lock()
		f.calls = append(f.calls, u)
		f.auth = append(f.auth, headers["Authorization"])
		f.mu.Unlock()
		// Longest suffix first: "/billing/preflight" must not be read as
		// "/billing/summary" or as the bare "/api/v1/billing".
		var best string
		for suffix := range f.reply {
			if strings.HasSuffix(u, suffix) && len(suffix) > len(best) {
				best = suffix
			}
		}
		if best == "" {
			return 404, []byte(`{"error":"no scripted answer"}`), nil
		}
		r := f.reply[best]
		return r.status, []byte(r.body), nil
	}
}

func (f *fakeControl) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeControl) calledWith(suffix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, u := range f.calls {
		if strings.Contains(u, suffix) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// the three endpoints
// ---------------------------------------------------------------------------

// Endpoint 1: /api/v1/workspaces/current carries the active workspace id and the
// personal tier. Both spellings of the id are accepted, because the plane has
// answered both.
func TestWorkspaceReadParsesTheActiveWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want Workspace
	}{
		{"activeWorkspaceId", `{"activeWorkspaceId":"ws1","personalMembershipTier":"go"}`, Workspace{ID: "ws1", Tier: "go"}},
		{"workspaceId", `{"workspaceId":"ws2","personalMembershipTier":"pro"}`, Workspace{ID: "ws2", Tier: "pro"}},
	} {
		fake := newFakeControl().on("/api/v1/workspaces/current", 200, tc.body)
		got, err := WorkspaceOf(context.Background(), fake.transport(), "https://amr-api.example", "vela_ctrl_ws")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: workspace = %+v, want %+v", tc.name, got, tc.want)
		}
		if fake.count() != 1 {
			t.Errorf("%s: calls = %d, want 1", tc.name, fake.count())
		}
		if auth := fake.auth[0]; auth != "Bearer vela_ctrl_ws" {
			t.Errorf("%s: authorization = %q, want the control key", tc.name, auth)
		}
	}
}

// A workspace body with no id is a readable failure, not an empty workspace:
// without an id there is no preflight to read, and answering "" would send the
// caller to /workspaces//billing/preflight.
func TestWorkspaceWithoutAnActiveIDIsAReadableError(t *testing.T) {
	fake := newFakeControl().on("/api/v1/workspaces/current", 200, `{"personalMembershipTier":"go"}`)
	_, err := WorkspaceOf(context.Background(), fake.transport(), "https://amr-api.example", "vela_ctrl_ws")
	if err == nil {
		t.Fatal("a workspace with no active id must not be reported as a workspace")
	}
	if !strings.Contains(err.Error(), "no active workspace") {
		t.Errorf("error = %q, want a message naming the missing workspace", err)
	}
	if strings.Contains(err.Error(), "vela_ctrl_ws") {
		t.Errorf("error leaked the control key: %q", err)
	}
}

// Endpoints 2 and 3: the preflight and the summary. Every field the card shows
// is read here, and nothing is invented when the plane omits it.
func TestPreflightAndSummaryAreParsed(t *testing.T) {
	fake := newFakeControl().
		on("/billing/preflight", 200, `{
			"balanceUsd": "1.5000",
			"codingPlan": {
				"eligible": true,
				"tier": "go",
				"windows": [
					{"durationSeconds": 604800, "usedCredits": "16251", "limitCredits": "150000", "resetsAt": "2026-10-18T06:36:14.000Z"},
					{"durationSeconds": 18000, "usedCredits": "16251", "limitCredits": "50000", "resetsAt": "2026-10-11T11:36:14.000Z"}
				]
			}
		}`).
		on("/api/v1/billing/summary", 200, `{
			"membershipTier": "go",
			"subscriptionStatus": "active",
			"subscriptionCancelAtPeriodEnd": false,
			"subscriptionCurrentPeriodEnd": "2026-11-11T06:36:14.000Z",
			"balanceUsd": "1.5000"
		}`)

	got := Read(context.Background(), fake.transport(), Request{ControlURL: "https://amr-api.example", ControlKey: "vela_ctrl_ws", WorkspaceID: "ws1"})
	if got.Error != "" {
		t.Fatalf("error = %q", got.Error)
	}
	if !got.Readable {
		t.Errorf("a clean read must be reported as readable")
	}
	if got.SignIn != SignInKept {
		t.Errorf("sign_in = %q, want %q", got.SignIn, SignInKept)
	}
	if got.Plan != "Design Plan Go" {
		t.Errorf("plan = %q, want %q", got.Plan, "Design Plan Go")
	}
	if got.Balance != "$1.50" {
		t.Errorf("balance = %q, want $1.50", got.Balance)
	}
	if got.Until != "2026-11-11T06:36:14.000Z" {
		t.Errorf("until = %q", got.Until)
	}
	if got.Renew != "auto" {
		t.Errorf("renew = %q, want auto", got.Renew)
	}
	if got.Status != "active" {
		t.Errorf("status = %q, want active", got.Status)
	}
	if len(got.Windows) != 2 {
		t.Fatalf("windows = %+v, want 2", got.Windows)
	}
	w := got.Windows[0]
	if w.Name != "5 hours" || w.UsedPercent != 32.5 || w.DurationSeconds != 18000 {
		t.Errorf("first window = %+v, want the 5-hour one at 32.5%%", w)
	}
	if w.Display != "16.3k / 50k credits" {
		t.Errorf("display = %q", w.Display)
	}
	if w.ResetsAt != "2026-10-11T11:36:14.000Z" {
		t.Errorf("resets_at = %q", w.ResetsAt)
	}
	if second := got.Windows[1]; second.Name != "7 days" || second.UsedPercent != 10.8 {
		t.Errorf("second window = %+v, want the 7-day one at 10.8%%", second)
	}
	if fake.calledWith("/billing/preflight") != 1 || fake.calledWith("/api/v1/billing/summary") != 1 {
		t.Errorf("calls = %v, want one preflight and one summary", fake.calls)
	}
	for _, u := range fake.calls {
		if strings.Contains(u, url.PathEscape("")) && strings.Contains(u, "workspaces//") {
			t.Errorf("a request was built with an empty workspace id: %s", u)
		}
	}
}

// ---------------------------------------------------------------------------
// what a refusal means
// ---------------------------------------------------------------------------

// 401/403 from the preflight is the SIGN-IN being refused, not a hiccup: the
// account is marked expired so the operator is sent to re-authenticate, and the
// message must not carry the key.
func TestRefusedSignInIsMarkedNotReportedAsAGenericError(t *testing.T) {
	for _, status := range []int{401, 403} {
		fake := newFakeControl().on("/billing/preflight", status, `{"error":"unauthenticated"}`)
		got := Read(context.Background(), fake.transport(), Request{ControlURL: "https://amr-api.example", ControlKey: "vela_ctrl_secret", WorkspaceID: "ws1"})
		if !got.Refused {
			t.Errorf("%d: refused = false, want the reading to be marked refused", status)
		}
		if got.SignIn != SignInExpired {
			t.Errorf("%d: sign_in = %q, want %q", status, got.SignIn, SignInExpired)
		}
		if got.Error != MessageRefused {
			t.Errorf("%d: error = %q, want %q", status, got.Error, MessageRefused)
		}
		if strings.Contains(got.Error, "vela_ctrl_secret") {
			t.Errorf("%d: the refusal carries the control key", status)
		}
	}
}

// A 500 is NOT a refusal: the credential is still good, so marking it expired
// would send the operator to redo a working sign-in.
func TestAServerErrorDoesNotMarkTheSignIn(t *testing.T) {
	fake := newFakeControl().on("/billing/preflight", 503, `{"error":"unavailable"}`)
	got := Read(context.Background(), fake.transport(), Request{ControlURL: "https://amr-api.example", ControlKey: "vela_ctrl_c", WorkspaceID: "ws1"})
	if got.Refused {
		t.Errorf("a 503 was reported as a refused sign-in")
	}
	if got.SignIn == SignInExpired {
		t.Errorf("a 503 marked the sign-in expired")
	}
	if !strings.Contains(got.Error, "503") {
		t.Errorf("error = %q, want the status named", got.Error)
	}
}

// ---------------------------------------------------------------------------
// shaping rules
// ---------------------------------------------------------------------------

// The three spellings of resetsAt the plane has been seen to send.
func TestResetsAtAcceptsThreeSpellings(t *testing.T) {
	// 2026-10-11T11:36:14Z
	const want = "2026-10-11T11:36:14.000Z"
	for _, tc := range []struct{ name, body string }{
		{"rfc3339", `{"windows":[{"durationSeconds":18000,"usedCredits":10,"limitCredits":100,"resetsAt":"2026-10-11T11:36:14.000Z"}]}`},
		{"epoch seconds", `{"windows":[{"durationSeconds":18000,"usedCredits":10,"limitCredits":100,"resetsAt":1791718574}]}`},
		{"epoch milliseconds", `{"windows":[{"durationSeconds":18000,"usedCredits":10,"limitCredits":100,"resetsAt":1791718574000}]}`},
	} {
		var plan Plan
		if err := json.Unmarshal([]byte(tc.body), &plan); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		windows := Allowances(plan)
		if len(windows) != 1 {
			t.Fatalf("%s: windows = %+v", tc.name, windows)
		}
		if windows[0].ResetsAt != want {
			t.Errorf("%s: resets_at = %q, want %q", tc.name, windows[0].ResetsAt, want)
		}
	}
}

// A window with nothing to divide is dropped: a limit of zero would render as
// an empty bar, which reads as "consumed nothing" rather than "no allowance".
func TestWindowsWithoutALimitAreDropped(t *testing.T) {
	var plan Plan
	if err := json.Unmarshal([]byte(`{"windows":[
		{"durationSeconds":18000,"usedCredits":"3","limitCredits":"0"},
		{"durationSeconds":604800,"usedCredits":"0","limitCredits":"-1"},
		{"durationSeconds":900,"usedCredits":"3","limitCredits":"none"},
		{"durationSeconds":3600,"usedCredits":"3","limitCredits":"12"}
	]}`), &plan); err != nil {
		t.Fatalf("%v", err)
	}
	windows := Allowances(plan)
	if len(windows) != 1 {
		t.Fatalf("windows = %+v, want only the one with a usable limit", windows)
	}
	if windows[0].DurationSeconds != 3600 || windows[0].UsedPercent != 25 {
		t.Errorf("window = %+v, want the 1-hour one at 25%%", windows[0])
	}
}

// The list is shortest first, whatever order the plane listed them in.
func TestWindowsAreSortedShortestFirst(t *testing.T) {
	var plan Plan
	if err := json.Unmarshal([]byte(`{"windows":[
		{"durationSeconds":604800,"usedCredits":"1","limitCredits":"10"},
		{"durationSeconds":86400,"usedCredits":"1","limitCredits":"10"},
		{"durationSeconds":18000,"usedCredits":"1","limitCredits":"10"}
	]}`), &plan); err != nil {
		t.Fatalf("%v", err)
	}
	windows := Allowances(plan)
	if len(windows) != 3 {
		t.Fatalf("windows = %+v", windows)
	}
	for i := 1; i < len(windows); i++ {
		if windows[i-1].DurationSeconds > windows[i].DurationSeconds {
			t.Fatalf("windows are not sorted: %+v", windows)
		}
	}
	if windows[0].Name != "5 hours" {
		t.Errorf("first = %q, want the shortest window", windows[0].Name)
	}
}

// Window names follow the vendor's card.
func TestWindowNames(t *testing.T) {
	for _, tc := range []struct {
		seconds any
		want    string
	}{
		{18000, "5 hours"}, {604800, "7 days"}, {3600, "1 hour"},
		{86400, "1 day"}, {900, "15 minutes"}, {0, "Allowance"},
		{"18000", "5 hours"},
	} {
		if got := WindowName(tc.seconds); got != tc.want {
			t.Errorf("WindowName(%v) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}

// Credits are shortened in the three bands, at their exact boundaries.
func TestCreditsBands(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  string
	}{
		{9999, "9999"},    // still whole
		{10000, "10k"},    // the k band opens
		{16251, "16.3k"},  // under 100k keeps a decimal
		{50000, "50k"},    // ...
		{100000, "100k"},  // 100k and up rounds whole
		{150000, "150k"},  // ...
		{999999, "1000k"}, // still k at the boundary
		{1000000, "1M"},   // M band opens
		{2340000, "2.3M"},
		{477, "477"},
		{"16251", "16.3k"}, // strings arrive this way
		{nil, ""},          // nothing to show
	} {
		if got := Credits(tc.value); got != tc.want {
			t.Errorf("Credits(%v) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

// usedPercent rounds to one decimal and clamps at zero; a window it cannot
// meter reports ok == false rather than a confident 0%.
func TestUsedPercent(t *testing.T) {
	for _, tc := range []struct {
		used, limit any
		want        float64
		ok          bool
	}{
		{16251, 50000, 32.5, true},
		{16251, 150000, 10.8, true},
		{-5, 100, 0, true},
		{3, 0, 0, false},
		{3, -1, 0, false},
		{"3", "none", 0, false},
		{nil, nil, 0, false},
	} {
		got, ok := UsedPercent(tc.used, tc.limit)
		if ok != tc.ok {
			t.Errorf("UsedPercent(%v, %v) ok = %v, want %v", tc.used, tc.limit, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("UsedPercent(%v, %v) = %v, want %v", tc.used, tc.limit, got, tc.want)
		}
	}
}

// The plan name is the dashboard's, falling back through the tiers it knows.
func TestPlanName(t *testing.T) {
	for _, tc := range []struct {
		tiers []string
		want  string
	}{
		{[]string{"go", ""}, "Design Plan Go"},
		{[]string{"", "pro"}, "Design Plan Pro"},
		{[]string{"", ""}, "Open Design"},
		{nil, "Open Design"},
	} {
		if got := PlanName(tc.tiers...); got != tc.want {
			t.Errorf("PlanName(%v) = %q, want %q", tc.tiers, got, tc.want)
		}
	}
}

// A plan the account is not eligible for says so, in the reference's words: the
// request draws on the wallet, which is not the same as "no allowance left".
func TestIneligiblePlanIsStated(t *testing.T) {
	fake := newFakeControl().
		on("/billing/preflight", 200, `{"balanceUsd":"4.00","codingPlan":{"eligible":false,"windows":[]}}`).
		on("/api/v1/billing/summary", 200, `{"membershipTier":"free"}`)
	got := Read(context.Background(), fake.transport(), Request{ControlURL: "https://amr-api.example", ControlKey: "vela_ctrl_c", WorkspaceID: "ws1"})
	if got.Error != MessageNoPlan {
		t.Errorf("error = %q, want %q", got.Error, MessageNoPlan)
	}
	if got.Refused {
		t.Errorf("an ineligible plan is not a refused sign-in")
	}
	if got.Balance != "$4.00" {
		t.Errorf("balance = %q, want the wallet balance", got.Balance)
	}
	if !strings.Contains(got.Error, "wallet balance") {
		t.Errorf("the message must say where the requests draw from: %q", got.Error)
	}
}

// noPlanEligible is the plan body an account gets when the vendor does not say
// whether it is eligible: absent is not false.
func TestAbsentEligibilityIsNotIneligibility(t *testing.T) {
	fake := newFakeControl().
		on("/billing/preflight", 200, `{"codingPlan":{"windows":[{"durationSeconds":18000,"usedCredits":1,"limitCredits":10}]}}`).
		on("/api/v1/billing/summary", 200, `{"membershipTier":"go"}`)
	got := Read(context.Background(), fake.transport(), Request{ControlURL: "https://amr-api.example", ControlKey: "vela_ctrl_c", WorkspaceID: "ws1"})
	if got.Error != "" {
		t.Errorf("error = %q, want no message when the vendor did not say", got.Error)
	}
	if len(got.Windows) != 1 {
		t.Errorf("windows = %+v", got.Windows)
	}
}
