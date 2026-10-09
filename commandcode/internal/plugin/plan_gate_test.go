package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hex-ci/cpa-plugin/commandcode/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// planGateBridge answers the two host calls the plan gate needs: the auth store
// (via authStore) and host.http.do, which readBillingAccess uses to fetch the
// account's subscription and credits. Credits are answered too so the account's
// tier is genuinely known - an unknown tier is skipped, not gated.
func planGateBridge(store *authStore, planID string) func(string, []byte) ([]byte, error) {
	return func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return store.call(method, payload)
		}
		var req struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(payload, &req)
		switch {
		case strings.Contains(req.URL, "subscriptions"):
			body, _ := json.Marshal(map[string]any{
				"success": true,
				"data":    map[string]any{"planId": planID},
			})
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body}), nil
		case strings.Contains(req.URL, "credits"):
			body, _ := json.Marshal(map[string]any{
				"credits": map[string]any{"purchasedCredits": 0, "freeCredits": 0},
			})
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body}), nil
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusNotFound}), nil
	}
}

// gateFor builds a manager whose pool has exactly one credential - the one
// stored in the auth record - and returns its installed plan gate.
func gateFor(t *testing.T, store *authStore, planID string) func(string) bool {
	t.Helper()
	m := NewManager(NewHostBridge(planGateBridge(store, planID)))
	if err := m.refreshAuthCredentials(context.Background(), m.bridge); err != nil {
		t.Fatalf("refreshAuthCredentials: %v", err)
	}
	cfg := config.Config{BaseURL: "https://api.commandcode.ai"}
	return m.buildPlanGate(context.Background(), cfg, m.bridge)
}

// TestPlanGateCoversKeysAddedFromTheQuotaPage pins the operator-visible bug: a
// Go key added from the quota page lives in an auth record, never in the
// config. A gate built only from cfg.Accounts sees an empty pool, installs
// nothing, and the plugin then advertises every upstream model - so picking a
// GOAT-only one fails with 403 MODEL_NOT_IN_PLAN even though the table says
// otherwise. The gate must judge the union, like the request path already does.
func TestPlanGateCoversKeysAddedFromTheQuotaPage(t *testing.T) {
	store := newAuthStore()
	record, _ := json.Marshal(authRecordCredential{
		Type: ProviderID, ID: "commandcode-key-go", Label: "go",
		APIKey: "user_goplan", Mode: string(config.TransportGoCLI),
	})
	store.save(authFileNameFromHash(authKeyHash("user_goplan")), ProviderID, record)

	gate := gateFor(t, store, "individual-go-v1")
	if gate == nil {
		t.Fatal("no gate installed: a pool of one Go key from the auth store must be gated, " +
			"otherwise every upstream model is published and GOAT-only picks 403 upstream")
	}
	// A Go-tier model stays; a model this plan cannot run is dropped.
	if !gate("deepseek/deepseek-v4.1-flash") {
		t.Error("a Go-tier model was hidden from a Go account")
	}
	if gate("claude-haiku-5-5") {
		t.Error("claude-haiku-5-5 (minimum GOAT) was published to a Go account")
	}
}

// TestPlanGateStillCoversConfigAccounts pins that the pre-existing source keeps
// working - the union must not regress the config path.
func TestPlanGateStillCoversConfigAccounts(t *testing.T) {
	store := newAuthStore() // deliberately empty: the key is only in the config
	m := NewManager(NewHostBridge(planGateBridge(store, "individual-go-v1")))

	cfg := config.Config{
		BaseURL: "https://api.commandcode.ai",
		Accounts: []config.Account{
			{Label: "go", Mode: config.TransportGoCLI, Credential: "user_configgo"},
		},
	}
	gate := m.buildPlanGate(context.Background(), cfg, m.bridge)
	if gate == nil {
		t.Fatal("no gate installed for a config-declared Go key")
	}
	if gate("claude-haiku-5-5") {
		t.Error("claude-haiku-5-5 (minimum GOAT) was published to a Go account")
	}
}

// TestPlanGateIsSkippedWhenNoPlanIsReadable keeps the honest fail-open: an
// account whose tier cannot be read must not silently hide every model.
func TestPlanGateIsSkippedWhenNoPlanIsReadable(t *testing.T) {
	store := newAuthStore()
	record, _ := json.Marshal(authRecordCredential{
		Type: ProviderID, APIKey: "user_unreadable", Mode: string(config.TransportGoCLI),
	})
	store.save(authFileNameFromHash(authKeyHash("user_unreadable")), ProviderID, record)

	// A bridge that refuses the billing calls: the tier stays unknown.
	refuse := func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusTooManyRequests}), nil
		}
		return store.call(method, payload)
	}
	m := NewManager(NewHostBridge(refuse))
	if err := m.refreshAuthCredentials(context.Background(), m.bridge); err != nil {
		t.Fatalf("refreshAuthCredentials: %v", err)
	}
	cfg := config.Config{BaseURL: "https://api.commandcode.ai"}
	if gate := m.buildPlanGate(context.Background(), cfg, m.bridge); gate != nil {
		t.Error("a gate was installed from unknowns only; a pool whose plan cannot be read " +
			"must publish everything rather than hide it on a guess")
	}
}

// TestPlanGateRespectsItsTotalBudget pins the failure that made a quota-page
// key add leave the deployment with NO models: the gate reads every account's
// plan over the host HTTP bridge, and when the host never answers those reads
// used to run sequentially under a per-read timeout far longer than the
// refresh budget the caller had given. The gate then consumed the whole
// refresh, the catalog fetch was cut off, and the model list came back empty.
//
// The reads are now concurrent and capped as a group, so the gate always
// returns inside its own budget and an unreadable pool degrades to the honest
// "publish everything" answer instead of starving the fetch.
func TestPlanGateRespectsItsTotalBudget(t *testing.T) {
	store := newAuthStore()
	// Three go-cli credentials: sequential reads would need 3 full timeouts.
	for _, cred := range []string{"user_a", "user_b", "user_c"} {
		record, _ := json.Marshal(authRecordCredential{
			Type: ProviderID, APIKey: cred, Mode: string(config.TransportGoCLI),
		})
		store.save(authFileNameFromHash(authKeyHash(cred)), ProviderID, record)
	}

	// A host that never answers: every billing read blocks until its context
	// is done, then reports failure - the worst case for a budget.
	blocking := func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return store.call(method, payload)
		}
		time.Sleep(20 * time.Second) // never answered within any budget here
		return hostErr("timeout", "no answer"), nil
	}

	m := NewManager(NewHostBridge(blocking))
	if err := m.refreshAuthCredentials(context.Background(), m.bridge); err != nil {
		t.Fatalf("refreshAuthCredentials: %v", err)
	}
	cfg := config.Config{BaseURL: "https://api.commandcode.ai"}

	start := time.Now()
	gate := m.buildPlanGate(context.Background(), cfg, m.bridge)
	elapsed := time.Since(start)

	// Generous ceiling: the point is that it is bounded by the gate's own
	// budget, not by accounts * per-read timeout.
	if elapsed > 8*time.Second {
		t.Fatalf("buildPlanGate took %v with an unresponsive host; want it bounded by its own budget", elapsed)
	}
	if gate != nil {
		t.Error("a gate was built from a pool whose plans all timed out; an unreadable pool must not hide anything")
	}
}
