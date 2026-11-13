package allowance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hex-ci/cpa-plugin/opendesign/internal/config"
)

// The allowance read's budget is its own and much shorter than the model plane's:
// an interactive card must fail fast instead of inheriting a five-minute request
// budget.
func TestReadBudgetIsIndependentOfTheRequestTimeout(t *testing.T) {
	if readTimeout != 30*time.Second {
		t.Errorf("readTimeout = %v, want 30s (the reference implementation's budget)", readTimeout)
	}
	if readTimeout >= config.DefaultRequestTimeout {
		t.Errorf("readTimeout (%v) must be shorter than the model request timeout (%v)", readTimeout, config.DefaultRequestTimeout)
	}
}

// A control plane that never answers must not hold the card: the read is bounded
// by its own context, and a caller's earlier deadline wins.
func TestAStalledControlPlaneEndsTheRead(t *testing.T) {
	stalled := func(ctx context.Context, _ string, _ string, _ map[string]string, _ []byte) (int, []byte, error) {
		<-ctx.Done()
		return 0, nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	start := time.Now()
	got := Read(ctx, stalled, Request{ControlURL: "https://amr-api.example", ControlKey: "vela_ctrl_a", WorkspaceID: "ws1"})
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("the read took %v, so it is not bounded by its own budget", elapsed)
	}
	if got.Refused {
		t.Errorf("a stalled plane was reported as a refused sign-in")
	}
	if got.SignIn != SignInKept {
		t.Errorf("sign_in = %q, want kept", got.SignIn)
	}
	if got.Error == "" {
		t.Errorf("a stalled read must say so, not answer an empty snapshot")
	}
	if !got.Readable {
		t.Errorf("a failed read must still be reported as a readable account")
	}
}

// A workspace id that would reshape the path is escaped, so the request can only
// reach the endpoint it was meant for.
func TestWorkspaceIDsArePathEscaped(t *testing.T) {
	fake := newFakeControl().on("/billing/preflight", 200, `{"codingPlan":{"eligible":true,"windows":[]}}`)
	fake.on("/api/v1/billing/summary", 200, `{"membershipTier":"go"}`)
	Read(context.Background(), fake.transport(), Request{
		ControlURL:  "https://amr-api.example",
		ControlKey:  "vela_ctrl_a",
		WorkspaceID: "ws/../evil",
	})
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, u := range fake.calls {
		if !strings.Contains(u, "/api/v1/workspaces/") {
			continue
		}
		if strings.Contains(u, "ws/../evil") {
			t.Errorf("a workspace id reshaped the request path: %s", u)
		}
		if !strings.Contains(u, "ws%2F..%2Fevil") {
			t.Errorf("the workspace id was not escaped: %s", u)
		}
	}
}
