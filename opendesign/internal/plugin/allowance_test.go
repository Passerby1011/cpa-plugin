package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/opendesign/internal/allowance"
)

// fakeControlPlane answers the three control-plane reads a scripted way and
// records every call, so a test asserts what was ASKED, not only what came back.
type fakeControlPlane struct {
	mu sync.Mutex
	// urls and auths are every request this plane saw, in order.
	urls  []string
	auths []string
	// bodies maps a path suffix onto an answer.
	bodies map[string]fakePlaneReply
}

type fakePlaneReply struct {
	status int
	body   string
}

func newFakeControlPlane() *fakeControlPlane {
	return &fakeControlPlane{bodies: map[string]fakePlaneReply{}}
}

func (f *fakeControlPlane) on(suffix string, status int, body string) *fakeControlPlane {
	f.bodies[suffix] = fakePlaneReply{status: status, body: body}
	return f
}

// rawCaller is the host callback the bridge drives. The plugin never opens a
// socket of its own, so this is where "the control plane" is stubbed in.
func (f *fakeControlPlane) rawCaller() RawCaller {
	return func(method string, payload []byte) ([]byte, error) {
		if method != "host.http.do" {
			// Everything else (log, auth list) is accepted as an empty OK
			// envelope: this test is about the allowance reads.
			return okEnvelopeBytes(map[string]any{}), nil
		}
		var req struct {
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.urls = append(f.urls, req.URL)
		auth := ""
		if values, ok := req.Headers["Authorization"]; ok && len(values) > 0 {
			auth = values[0]
		}
		f.auths = append(f.auths, auth)
		bodies := f.bodies
		best := ""
		for suffix := range bodies {
			if strings.Contains(req.URL, suffix) && len(suffix) > len(best) {
				best = suffix
			}
		}
		f.mu.Unlock()

		reply := fakePlaneReply{status: 404, body: `{"error":"no scripted answer"}`}
		if best != "" {
			reply = bodies[best]
		}
		return okEnvelopeBytes(pluginapi.HTTPResponse{
			StatusCode: reply.status,
			Body:       []byte(reply.body),
		}), nil
	}
}

func (f *fakeControlPlane) count(suffix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, u := range f.urls {
		if strings.Contains(u, suffix) {
			n++
		}
	}
	return n
}

func (f *fakeControlPlane) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.urls)
}

// okEnvelopeBytes wraps a payload in a successful host envelope.
func okEnvelopeBytes(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabiEnvelopeWire{OK: true, Result: raw})
	return out
}

// pluginabiEnvelopeWire is the host envelope's wire shape, declared here so the
// fakes do not depend on the SDK's exact struct tags.
type pluginabiEnvelopeWire struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// accountConfig is a two-account config: one signed-in account (both keys) and
// one pasted API key (runtime key only).
const accountConfig = `api-keys:
  - value: "sk-amr-runtimeA"
    control: "vela_ctrl_controlA"
    label: "Account A"
    email: "a@example.com"
  - value: "sk-amr-pasted"
    label: "Pasted key"
`

// managerWithPlane registers the two-account config and wires the fake plane in.
func managerWithPlane(t *testing.T, plane *fakeControlPlane) *Manager {
	t.Helper()
	m := NewManager(NewHostBridge(plane.rawCaller()))
	body, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(accountConfig)})
	if _, err := m.HandleCall("plugin.register", body); err != nil {
		t.Fatalf("register: %v", err)
	}
	// The registration reads the model catalog at the fake plane too; the
	// answers for those calls are irrelevant here, so the allowance routes are
	// scripted after it.
	return m
}

// managementGet asks one GET route and returns its decoded answer.
func managementGet(t *testing.T, m *Manager, base, suffix string) managementResponseBody {
	t.Helper()
	req, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: base + "/plugins/" + ProviderID + suffix})
	env := decodeTestEnvelope(t, mustHandle(t, m, req))
	if !env.OK {
		t.Fatalf("%s: envelope: %+v", suffix, env.Error)
	}
	var resp managementResponseBody
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("%s: response: %v", suffix, err)
	}
	return resp
}

// managementPost asks one POST route with a JSON body.
func managementPost(t *testing.T, m *Manager, base, suffix string, body any) managementResponseBody {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   base + "/plugins/" + ProviderID + suffix,
		Body:   raw,
	})
	env := decodeTestEnvelope(t, mustHandle(t, m, req))
	if !env.OK {
		t.Fatalf("%s: envelope: %+v", suffix, env.Error)
	}
	var resp managementResponseBody
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("%s: response: %v", suffix, err)
	}
	return resp
}

func mustHandle(t *testing.T, m *Manager, req []byte) []byte {
	t.Helper()
	raw, err := m.HandleCall("management.handle", req)
	if err != nil {
		t.Fatalf("management handle: %v", err)
	}
	return raw
}

// decodeCards reads the GET answer.
func decodeCards(t *testing.T, resp managementResponseBody) allowanceList {
	t.Helper()
	var list allowanceList
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		t.Fatalf("cards: %v (%s)", err, resp.Body)
	}
	return list
}

// ---------------------------------------------------------------------------
// the list route
// ---------------------------------------------------------------------------

// The list is free: no upstream call at all, because a page that read every
// account on load would turn one open into N control-plane requests.
func TestAllowanceListMakesNoUpstreamCall(t *testing.T) {
	plane := newFakeControlPlane()
	plane.on("/workspaces/current", 200, `{"activeWorkspaceId":"wsA","personalMembershipTier":"go"}`)
	m := managerWithPlane(t, plane)
	before := plane.total()

	resp := managementGet(t, m, "/v0/management", "/allowance")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if after := plane.total(); after != before {
		t.Errorf("the list made %d control-plane calls, want none", after-before)
	}

	list := decodeCards(t, resp)
	if list.Accounts != 2 {
		t.Fatalf("accounts = %d, want 2 (%+v)", list.Accounts, list.Cards)
	}
	if list.WithPlan != 1 {
		t.Errorf("with_plan = %d, want 1", list.WithPlan)
	}
	var signedIn, pasted *allowanceCard
	for i := range list.Cards {
		switch list.Cards[i].Label {
		case "Account A":
			signedIn = &list.Cards[i]
		case "Pasted key":
			pasted = &list.Cards[i]
		}
	}
	if signedIn == nil || pasted == nil {
		t.Fatalf("cards = %+v", list.Cards)
	}
	if !signedIn.PlanReadable {
		t.Errorf("the signed-in account is reported as unreadable")
	}
	if pasted.PlanReadable {
		t.Errorf("a pasted API key is reported as readable")
	}
	// An API key says WHY, rather than rendering as an empty card or a zero.
	if pasted.Message != allowance.MessageAPIKey {
		t.Errorf("pasted message = %q, want %q", pasted.Message, allowance.MessageAPIKey)
	}
	// The card id is a digest, never a key.
	for _, card := range list.Cards {
		if strings.Contains(card.ID, "sk-amr") || strings.Contains(card.ID, "vela_ctrl") {
			t.Errorf("card id carries a key: %q", card.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// the refresh route
// ---------------------------------------------------------------------------

// A refresh reads the account at its CONTROL key: the workspace, the preflight
// and the summary, all three authorized by vela_ctrl_… and never by sk-amr-….
func TestAllowanceRefreshReadsAtTheControlKey(t *testing.T) {
	plane := newFakeControlPlane()
	plane.on("/workspaces/current", 200, `{"activeWorkspaceId":"wsA","personalMembershipTier":"go"}`)
	plane.on("/billing/preflight", 200, `{
		"balanceUsd":"1.5000",
		"codingPlan":{"eligible":true,"windows":[
			{"durationSeconds":604800,"usedCredits":"16251","limitCredits":"150000","resetsAt":"2026-10-18T06:36:14.000Z"},
			{"durationSeconds":18000,"usedCredits":"16251","limitCredits":"50000","resetsAt":"2026-10-11T11:36:14.000Z"}
		]}
	}`)
	plane.on("/api/v1/billing/summary", 200, `{
		"membershipTier":"go","subscriptionStatus":"active",
		"subscriptionCancelAtPeriodEnd":false,
		"subscriptionCurrentPeriodEnd":"2026-11-11T06:36:14.000Z","balanceUsd":"1.5000"
	}`)
	m := managerWithPlane(t, plane)

	list := decodeCards(t, managementGet(t, m, "/v0/management", "/allowance"))
	id := ""
	for _, card := range list.Cards {
		if card.Label == "Account A" {
			id = card.ID
		}
	}
	if id == "" {
		t.Fatalf("no card for the signed-in account: %+v", list.Cards)
	}

	resp := managementPost(t, m, "/v0/management", "/allowance", allowanceRequest{ID: id})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s)", resp.StatusCode, resp.Body)
	}
	var read allowanceRead
	if err := json.Unmarshal(resp.Body, &read); err != nil {
		t.Fatalf("read: %v (%s)", err, resp.Body)
	}
	if read.Card.Usage == nil {
		t.Fatalf("a clean refresh carried no reading: %s", resp.Body)
	}
	usage := read.Card.Usage
	if usage.Plan != "Design Plan Go" {
		t.Errorf("plan = %q", usage.Plan)
	}
	if usage.Balance != "$1.50" {
		t.Errorf("balance = %q", usage.Balance)
	}
	if usage.Until != "2026-11-11T06:36:14.000Z" {
		t.Errorf("until = %q", usage.Until)
	}
	if usage.Renew != "auto" {
		t.Errorf("renew = %q", usage.Renew)
	}
	if len(usage.Windows) != 2 {
		t.Fatalf("windows = %+v", usage.Windows)
	}
	if usage.Windows[0].Name != "5 hours" || usage.Windows[0].UsedPercent != 32.5 {
		t.Errorf("first window = %+v", usage.Windows[0])
	}
	if usage.Windows[0].Display != "16.3k / 50k credits" {
		t.Errorf("display = %q", usage.Windows[0].Display)
	}
	if read.Card.SignIn != allowance.SignInKept {
		t.Errorf("sign_in = %q, want kept", read.Card.SignIn)
	}

	// Everything asked for went out at the CONTROL key.
	plane.mu.Lock()
	defer plane.mu.Unlock()
	for i, u := range plane.urls {
		if !strings.Contains(u, "/workspaces") && !strings.Contains(u, "/billing") {
			continue
		}
		if got := plane.auths[i]; got != "Bearer vela_ctrl_controlA" {
			t.Errorf("%s was sent %q, want the control key", u, got)
		}
	}
}

// A pasted API key's refresh answers "not readable" — with a 200, because there
// is nothing wrong; there is simply no control key to read with.
func TestRefreshOfAnAPIKeySaysItCannotBeRead(t *testing.T) {
	plane := newFakeControlPlane()
	m := managerWithPlane(t, plane)

	list := decodeCards(t, managementGet(t, m, "/v0/management", "/allowance"))
	id := ""
	for _, card := range list.Cards {
		if card.Label == "Pasted key" {
			id = card.ID
		}
	}
	if id == "" {
		t.Fatalf("no card for the pasted key: %+v", list.Cards)
	}
	before := plane.total()
	resp := managementPost(t, m, "/v0/management", "/allowance", allowanceRequest{ID: id})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var read allowanceRead
	if err := json.Unmarshal(resp.Body, &read); err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.Card.PlanReadable {
		t.Errorf("a pasted key is reported as readable")
	}
	if read.Card.Usage != nil {
		t.Errorf("a pasted key produced a reading: %+v", read.Card.Usage)
	}
	if read.Card.Message != allowance.MessageAPIKey {
		t.Errorf("message = %q, want %q", read.Card.Message, allowance.MessageAPIKey)
	}
	if after := plane.total(); after != before {
		t.Errorf("reading an API key's allowance made %d upstream calls, want none", after-before)
	}
}

// A 401 from the preflight marks the account, and the mark survives into the
// LIST, so the row keeps offering "sign in again" across page loads.
func TestRefusedControlSignInIsMarkedAndSurvivesTheReload(t *testing.T) {
	plane := newFakeControlPlane()
	plane.on("/workspaces/current", 200, `{"activeWorkspaceId":"wsA"}`)
	plane.on("/billing/preflight", 401, `{"error":"unauthenticated"}`)
	plane.on("/api/v1/billing/summary", 200, `{}`)
	m := managerWithPlane(t, plane)

	list := decodeCards(t, managementGet(t, m, "/v0/management", "/allowance"))
	id := ""
	for _, card := range list.Cards {
		if card.Label == "Account A" {
			id = card.ID
		}
	}
	resp := managementPost(t, m, "/v0/management", "/allowance", allowanceRequest{ID: id})
	var read allowanceRead
	if err := json.Unmarshal(resp.Body, &read); err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.Card.SignIn != allowance.SignInExpired {
		t.Errorf("sign_in = %q, want expired", read.Card.SignIn)
	}
	if read.Card.Message != allowance.MessageRefused {
		t.Errorf("message = %q, want %q", read.Card.Message, allowance.MessageRefused)
	}

	// The reload must still say expired: a mark that only lived inside one
	// response would make a dead account look healthy on the next page load.
	reload := decodeCards(t, managementGet(t, m, "/v0/management", "/allowance"))
	for _, card := range reload.Cards {
		if card.ID != id {
			continue
		}
		if card.SignIn != allowance.SignInExpired {
			t.Errorf("after reload sign_in = %q, want expired", card.SignIn)
		}
	}
}

// A clean read after a refusal clears the mark and reports "renewed", so the
// operator's row stops asking for a sign-in that already happened.
func TestACleanReadClearsTheExpiredMark(t *testing.T) {
	plane := newFakeControlPlane()
	plane.on("/workspaces/current", 200, `{"activeWorkspaceId":"wsA"}`)
	plane.on("/billing/preflight", 401, `{"error":"unauthenticated"}`)
	m := managerWithPlane(t, plane)

	list := decodeCards(t, managementGet(t, m, "/v0/management", "/allowance"))
	id := ""
	for _, card := range list.Cards {
		if card.Label == "Account A" {
			id = card.ID
		}
	}
	managementPost(t, m, "/v0/management", "/allowance", allowanceRequest{ID: id})
	if !m.expiredAllowances.has(id) {
		t.Fatalf("a 401 did not mark the account")
	}

	// The plane recovers.
	plane.on("/billing/preflight", 200, `{"balanceUsd":"2.00","codingPlan":{"eligible":true,"windows":[]}}`)
	plane.on("/api/v1/billing/summary", 200, `{"membershipTier":"go","subscriptionStatus":"active"}`)
	resp := managementPost(t, m, "/v0/management", "/allowance", allowanceRequest{ID: id})
	var read allowanceRead
	if err := json.Unmarshal(resp.Body, &read); err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.Card.SignIn != allowance.SignInRenewed {
		t.Errorf("sign_in = %q, want renewed", read.Card.SignIn)
	}
	if m.expiredAllowances.has(id) {
		t.Errorf("a clean read left the account marked expired")
	}
}

// A 5xx is NOT a refusal: re-authenticating a working account is the mistake
// this split exists to prevent.
func TestAServerErrorDoesNotMarkTheAccount(t *testing.T) {
	plane := newFakeControlPlane()
	plane.on("/workspaces/current", 200, `{"activeWorkspaceId":"wsA"}`)
	plane.on("/billing/preflight", 503, `{"error":"unavailable"}`)
	m := managerWithPlane(t, plane)

	list := decodeCards(t, managementGet(t, m, "/v0/management", "/allowance"))
	id := ""
	for _, card := range list.Cards {
		if card.Label == "Account A" {
			id = card.ID
		}
	}
	resp := managementPost(t, m, "/v0/management", "/allowance", allowanceRequest{ID: id})
	var read allowanceRead
	if err := json.Unmarshal(resp.Body, &read); err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.Card.SignIn == allowance.SignInExpired {
		t.Errorf("a 503 marked the account expired")
	}
	if m.expiredAllowances.has(id) {
		t.Errorf("a 503 marked the account")
	}
	if !strings.Contains(read.Card.Message, "503") {
		t.Errorf("message = %q, want the status named", read.Card.Message)
	}
}

// The second refresh of one account reuses the cached workspace: a click is not
// three round-trips when two will do.
func TestTheSecondRefreshReusesTheCachedWorkspace(t *testing.T) {
	plane := newFakeControlPlane()
	plane.on("/workspaces/current", 200, `{"activeWorkspaceId":"wsA"}`)
	plane.on("/billing/preflight", 200, `{"codingPlan":{"eligible":true,"windows":[]}}`)
	plane.on("/api/v1/billing/summary", 200, `{"membershipTier":"go"}`)
	m := managerWithPlane(t, plane)

	list := decodeCards(t, managementGet(t, m, "/v0/management", "/allowance"))
	id := ""
	for _, card := range list.Cards {
		if card.Label == "Account A" {
			id = card.ID
		}
	}
	for i := 0; i < 2; i++ {
		if resp := managementPost(t, m, "/v0/management", "/allowance", allowanceRequest{ID: id}); resp.StatusCode != http.StatusOK {
			t.Fatalf("refresh %d: status = %d (%s)", i, resp.StatusCode, resp.Body)
		}
	}
	if n := plane.count("/workspaces/current"); n != 1 {
		t.Errorf("workspaces reads = %d, want 1 (the second refresh must use the cache)", n)
	}
	if n := plane.count("/billing/preflight"); n != 2 {
		t.Errorf("preflights = %d, want 2 (every refresh is live)", n)
	}
}

// An ineligible plan is stated in the reference's words and is NOT a refusal.
func TestAnIneligiblePlanIsStatedNotMarked(t *testing.T) {
	plane := newFakeControlPlane()
	plane.on("/workspaces/current", 200, `{"activeWorkspaceId":"wsA"}`)
	plane.on("/billing/preflight", 200, `{"balanceUsd":"4.00","codingPlan":{"eligible":false,"windows":[]}}`)
	plane.on("/api/v1/billing/summary", 200, `{"membershipTier":"free","subscriptionStatus":"none"}`)
	m := managerWithPlane(t, plane)

	list := decodeCards(t, managementGet(t, m, "/v0/management", "/allowance"))
	id := ""
	for _, card := range list.Cards {
		if card.Label == "Account A" {
			id = card.ID
		}
	}
	resp := managementPost(t, m, "/v0/management", "/allowance", allowanceRequest{ID: id})
	var read allowanceRead
	if err := json.Unmarshal(resp.Body, &read); err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.Card.Message != allowance.MessageNoPlan {
		t.Errorf("message = %q, want %q", read.Card.Message, allowance.MessageNoPlan)
	}
	if read.Card.SignIn == allowance.SignInExpired {
		t.Errorf("an ineligible plan is not a refused sign-in")
	}
	if read.Card.Usage == nil || read.Card.Usage.Balance != "$4.00" {
		t.Errorf("the wallet balance must still be shown: %+v", read.Card.Usage)
	}
}

// ---------------------------------------------------------------------------
// guards
// ---------------------------------------------------------------------------

// The refresh is POST-only and the list is GET-only: a POST with no id must not
// silently read every account.
func TestRefreshNeedsAnID(t *testing.T) {
	plane := newFakeControlPlane()
	m := managerWithPlane(t, plane)
	resp := managementPost(t, m, "/v0/management", "/allowance", allowanceRequest{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), "GET") {
		t.Errorf("the refusal must point at the list route: %s", resp.Body)
	}
}

// An unknown id is a stated 404, not an empty 200 a page would render as a
// successful payload with no data.
func TestUnknownAccountIs404(t *testing.T) {
	plane := newFakeControlPlane()
	m := managerWithPlane(t, plane)
	resp := managementPost(t, m, "/v0/management", "/allowance", allowanceRequest{ID: "000000000000"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// A PUT on the allowance route is refused: it is a read surface with two
// methods, not a writable resource.
func TestAllowanceRefusesOtherMethods(t *testing.T) {
	m := NewManager(nil)
	req, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodPut, Path: "/v0/management/plugins/" + ProviderID + "/allowance"})
	env := decodeTestEnvelope(t, mustHandle(t, m, req))
	var resp managementResponseBody
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("response: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

// Every allowance answer is key-free: the panel renders these bytes, and a key
// appearing in one would be a leak to a browser. Both keys are planted in the
// config for this.
func TestAllowanceRoutesNeverCarryAKey(t *testing.T) {
	plane := newFakeControlPlane()
	plane.on("/workspaces/current", 200, `{"activeWorkspaceId":"wsA"}`)
	plane.on("/billing/preflight", 401, `{"error":"unauthenticated"}`)
	m := managerWithPlane(t, plane)

	list := managementGet(t, m, "/v0/management", "/allowance")
	if body := string(list.Body); strings.Contains(body, "vela_ctrl_controlA") || strings.Contains(body, "sk-amr-runtimeA") {
		t.Errorf("the list leaked a key: %s", body)
	}
	list2 := decodeCards(t, list)
	if len(list2.Cards) == 0 {
		t.Fatalf("no cards")
	}
	resp := managementPost(t, m, "/v0/management", "/allowance", allowanceRequest{ID: list2.Cards[0].ID})
	if body := string(resp.Body); strings.Contains(body, "vela_ctrl_controlA") || strings.Contains(body, "sk-amr-runtimeA") {
		t.Errorf("the refresh leaked a key: %s", body)
	}
	// A refusal's message must not quote the credential it was refused for.
	if strings.Contains(string(resp.Body), "Bearer") {
		t.Errorf("the refusal carries a header value: %s", resp.Body)
	}
}

// The panel renders the allowance cards, so the served page must actually carry
// the card code: a panel without it would show accounts and models only, which
// is the gap this feature closes.
func TestPanelRendersAllowanceCards(t *testing.T) {
	m := NewManager(nil)
	registerForTest(t, m, "/v0/management")
	req, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/" + ProviderID + panelResourcePath,
	})
	resp := managementResponseBody{}
	env := decodeTestEnvelope(t, mustHandle(t, m, req))
	if !env.OK {
		t.Fatalf("panel: %+v", env.Error)
	}
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("panel response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("panel status = %d", resp.StatusCode)
	}
	page := string(resp.Body)
	for _, want := range []string{"allowance", "Refresh", "credits", "wallet", "vela_ctrl"} {
		if !strings.Contains(page, want) {
			t.Errorf("the panel does not carry %q", want)
		}
	}
	if strings.Contains(page, "__MANAGEMENT_BASE__") {
		t.Errorf("the panel still carries the base placeholder")
	}
}
