package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The allowance read over a REAL socket.
//
// The other tests inject a transport function, which proves the shaping and the
// decisions but says nothing about the host-bridge call itself. This one drives
// the same path through a real http.Client against an httptest server, so the
// request the bridge builds (method, URL, Authorization) is exercised end to end.
// It is still a fake control plane: this machine has no Open Design account, so
// the real amr-api is NOT covered by any test here.
func TestAllowanceRefreshOverRealHTTP(t *testing.T) {
	type seen struct {
		mu    sync.Mutex
		auths []string
		paths []string
	}
	var got seen

	mux := http.NewServeMux()
	handle := func(path string, body string) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			got.mu.Lock()
			got.auths = append(got.auths, r.Header.Get("Authorization"))
			got.paths = append(got.paths, r.URL.Path)
			got.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
	}
	handle("/api/v1/workspaces/current", `{"activeWorkspaceId":"wsA","personalMembershipTier":"go"}`)
	handle("/api/v1/workspaces/wsA/billing/preflight", `{
		"balanceUsd":"2.4000",
		"codingPlan":{"eligible":true,"windows":[
			{"durationSeconds":604800,"usedCredits":"90000","limitCredits":"150000","resetsAt":"2026-10-18T06:36:14Z"},
			{"durationSeconds":18000,"usedCredits":"42000","limitCredits":"50000","resetsAt":1791718574000}
		]}
	}`)
	handle("/api/v1/billing/summary", `{
		"membershipTier":"go","subscriptionStatus":"active",
		"subscriptionCancelAtPeriodEnd":true,
		"subscriptionCurrentPeriodEnd":"2026-11-11T06:36:14Z","balanceUsd":"2.4000"
	}`)
	server := httptest.NewServer(mux)
	defer server.Close()

	// The host bridge, with a RawCaller that performs real HTTP instead of a
	// scripted answer: everything below the plugin's own code is the network.
	raw := func(method string, payload []byte) ([]byte, error) {
		if method != "host.http.do" {
			return okEnvelopeBytes(map[string]any{}), nil
		}
		var req struct {
			Method  string              `json:"method"`
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
			Body    []byte              `json:"body"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		hreq, err := http.NewRequest(req.Method, req.URL, nil)
		if err != nil {
			return nil, err
		}
		for k, values := range req.Headers {
			for _, v := range values {
				hreq.Header.Add(k, v)
			}
		}
		resp, err := http.DefaultClient.Do(hreq)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body := make([]byte, 0, 4096)
		buf := make([]byte, 4096)
		for {
			n, errRead := resp.Body.Read(buf)
			body = append(body, buf[:n]...)
			if errRead != nil {
				break
			}
		}
		return okEnvelopeBytes(pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Body: body}), nil
	}
	m := NewManager(NewHostBridge(raw))
	cfgYAML := "control-url: \"" + server.URL + "\"\nallow-http: true\n" + `api-keys:
  - value: "sk-amr-runtimeA"
    control: "vela_ctrl_controlA"
    label: "Account A"
    email: "a@example.com"
`
	body, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(cfgYAML)})
	if _, err := m.HandleCall("plugin.register", body); err != nil {
		t.Fatalf("register: %v", err)
	}

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
	var read allowanceRead
	if err := json.Unmarshal(resp.Body, &read); err != nil {
		t.Fatalf("read: %v (%s)", err, resp.Body)
	}
	if read.Card.Usage == nil {
		t.Fatalf("no reading: %s", resp.Body)
	}
	usage := read.Card.Usage
	if usage.Plan != "Design Plan Go" || usage.Balance != "$2.40" {
		t.Errorf("plan/balance = %q/%q", usage.Plan, usage.Balance)
	}
	if usage.Renew != "off" {
		t.Errorf("renew = %q, want off (the summary says it cancels at the period's end)", usage.Renew)
	}
	if usage.Until != "2026-11-11T06:36:14.000Z" {
		t.Errorf("until = %q", usage.Until)
	}
	if len(usage.Windows) != 2 {
		t.Fatalf("windows = %+v", usage.Windows)
	}
	if usage.Windows[0].Name != "5 hours" || usage.Windows[0].UsedPercent != 84 {
		t.Errorf("first window = %+v, want 5 hours at 84%%", usage.Windows[0])
	}
	// The epoch-millisecond resetsAt and the RFC3339 one land on the same shape.
	if usage.Windows[0].ResetsAt != "2026-10-11T11:36:14.000Z" {
		t.Errorf("epoch-millisecond resetsAt = %q", usage.Windows[0].ResetsAt)
	}
	if usage.Windows[1].ResetsAt != "2026-10-18T06:36:14.000Z" {
		t.Errorf("RFC3339 resetsAt = %q", usage.Windows[1].ResetsAt)
	}

	// Every control-plane request carried the CONTROL key, and the runtime key
	// never left the model plane.
	got.mu.Lock()
	defer got.mu.Unlock()
	if len(got.paths) != 3 {
		t.Errorf("control-plane calls = %v, want the workspace, the preflight and the summary", got.paths)
	}
	for _, auth := range got.auths {
		if auth != "Bearer vela_ctrl_controlA" {
			t.Errorf("a control-plane read was authorized with %q", auth)
		}
		if strings.Contains(auth, "sk-amr") {
			t.Errorf("the runtime key was sent to the control plane: %q", auth)
		}
	}
}
