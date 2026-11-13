package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hex-ci/cpa-plugin/commandcode/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestPlanGateReturnsOnBudgetWhenOneReadHangs pins the two halves of the
// deadline contract at once:
//
//   - BEHAVIOUR: an account whose read never answers must not stretch the gate
//     past its own budget, and the accounts that DID answer inside it still
//     decide what is published: a known tier installs the gate, and the hung
//     account is fail-open so it neither hides models nor erases the answer the
//     readable account gave.
//   - SAFETY: the hung read keeps running after the gate has returned. It must
//     not write anything the gate reads. The pre-fix code shared one results[i]
//     slot between an abandoned goroutine and this function, which -race
//     reports as a data race on this exact shape (the abandoned read was the
//     reported writer, the accesses loop the reported reader). A channel send
//     per read cannot alias, so a late read cannot reach state this function
//     reads.
//
// Deterministic on purpose: the hanging read ignores its context entirely, so
// the only way the gate returns is its own budget expiring, on every run.
func TestPlanGateReturnsOnBudgetWhenOneReadHangs(t *testing.T) {
	store := newAuthStore()
	for _, cred := range []string{"user_fast", "user_slow"} {
		record, _ := json.Marshal(authRecordCredential{
			Type: ProviderID, APIKey: cred, Mode: string(config.TransportGoCLI),
		})
		store.save(authFileNameFromHash(authKeyHash(cred)), ProviderID, record)
	}

	// release unblocks the hanging reads so their goroutines finish inside the
	// test binary; -race flushes reports only for completed executions.
	release := make(chan struct{})
	// answered is read from several concurrent reads; an atomic keeps the
	// fixture itself race-free so -race reports only what is under test.
	var answered atomic.Int32
	responder := func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return store.call(method, payload)
		}
		var req struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(payload, &req)
		if strings.HasSuffix(req.URL, "/alpha/billing/subscriptions") {
			// The FIRST subscription read answers at once; every later one
			// hangs past the gate's budget and ignores its context, like a
			// host bridge that has stopped answering.
			if answered.Add(1) == 1 {
				return hostOK(pluginapi.HTTPResponse{
					StatusCode: http.StatusOK,
					Body:       []byte(`{"success":true,"data":{"planId":"individual-go-v1"}}`),
				}), nil
			}
			<-release
			return hostErr("timeout", "no answer"), nil
		}
		return hostOK(pluginapi.HTTPResponse{
			StatusCode: http.StatusOK,
			Body:       []byte(`{"credits":{"purchasedCredits":0,"freeCredits":0}}`),
		}), nil
	}

	m := NewManager(NewHostBridge(responder))
	if err := m.refreshAuthCredentials(context.Background(), m.bridge); err != nil {
		t.Fatalf("refreshAuthCredentials: %v", err)
	}
	cfg := config.Config{BaseURL: "https://api.commandcode.ai"}

	start := time.Now()
	gate := m.buildPlanGate(context.Background(), cfg, m.bridge)
	elapsed := time.Since(start)
	close(release)
	if elapsed > 8*time.Second {
		t.Fatalf("buildPlanGate took %v; it must return on its own budget, not on the slowest read", elapsed)
	}
	// The Go tier that answered before the deadline still gates: the hung
	// account is fail-open, so it neither hides models nor erases what the
	// readable account told us.
	if gate == nil {
		t.Fatal("no gate installed although one account's Go plan was readable inside the budget")
	}
	if !gate("deepseek/deepseek-v4.1-flash") {
		t.Error("a Go-tier model was hidden from the readable Go account")
	}
	if gate("claude-haiku-5-5") {
		t.Error("claude-haiku-5-5 (minimum GOAT) was published although the only readable plan is Go")
	}
}
