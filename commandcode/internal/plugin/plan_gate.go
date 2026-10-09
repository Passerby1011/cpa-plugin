package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/hex-ci/cpa-plugin/commandcode/internal/config"
	"github.com/hex-ci/cpa-plugin/commandcode/internal/planfilter"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// planGateRefreshTimeout bounds the two account reads behind the plan filter.
// They are best-effort: a failure yields an unknown tier, which the filter
// treats as "show everything" rather than guessing a tier and hiding models.
const planGateRefreshTimeout = 15 * time.Second

// planGateTotalTimeout bounds reading EVERY account's plan in one gate build.
//
// The per-read timeout bounds one read; this bounds all of them. Without it a
// pool of N accounts could spend N*15s inside a refresh that the caller has
// budgeted for registerRefreshTimeout (10s) - which is exactly what happened:
// a quota-page key add builds the gate inside a bounded refresh, the unbounded
// parent let the reads overrun it, and the catalog fetch was cut off
// ("context deadline exceeded") leaving the deployment with NO models at all.
//
// Five seconds rather than the full budget because a failed plan read is
// harmless by design: AccessFor(nil, ...) fails open, so an account that could
// not be read in time keeps candidate access and is never hidden. Being late is
// far worse than being imprecise here.
const planGateTotalTimeout = 5 * time.Second

// buildPlanGate derives the model filter from the pool's billing state.
//
// # WHY THE FILTER EXISTS
//
// The live catalog returns every model the platform serves, with no plan
// metadata. A pool whose account cannot call most of them should not advertise
// them: the operator would pick a model, get a 403, and have no way to tell
// which of the 87 were actually theirs.
//
// The tier comes from /alpha/billing/subscriptions and the on-demand balance
// from /alpha/billing/credits, both read per account. Every account in the pool
// contributes: a model is kept when ANY of them can run it, because the pool -
// not any single account - serves the request.
//
// Returns nil when no gate should be installed, which publishes everything.
// That is the honest outcome when the pool is empty or when every tier is
// unknown: filtering on a guess would hide models the operator can run.
func (m *Manager) buildPlanGate(parent context.Context, cfg config.Config, bridge *HostBridge) func(string) bool {
	if bridge == nil {
		return nil
	}
	// Gate on the same pool the request path uses - poolAccounts, NOT
	// cfg.Accounts. A key added from the quota page lives in a CPA auth record,
	// and a gate that reads only the config silently ends up with no access to
	// judge, publishing every model exactly as if no gate existed. That is how
	// a Go key added from the page was still offered GOAT-only models and
	// failed with 403 MODEL_NOT_IN_PLAN upstream.
	//
	// Only go-cli accounts carry a plan worth reading: an api-keys entry is a
	// bare provider credential with no account identity behind it, so a pool
	// built from those has nothing to gate on and publishing everything is the
	// correct outcome, not a degraded one.
	var readable []config.Account
	for _, a := range m.poolAccounts(cfg) {
		if a.Credential != "" && a.Mode == config.TransportGoCLI {
			readable = append(readable, a)
		}
	}
	if len(readable) == 0 {
		return nil
	}

	// Read every account CONCURRENTLY and under one total budget. Sequentially
	// they add up: N accounts * planGateRefreshTimeout can exceed the caller's
	// whole refresh budget, and a plan read has no business eating it.
	gctx, cancelGate := context.WithTimeout(parent, planGateTotalTimeout)
	defer cancelGate()

	type readResult struct {
		tier     *planfilter.SubscriptionPlan
		onDemand float64
	}
	results := make([]readResult, len(readable))
	var wg sync.WaitGroup
	for i, a := range readable {
		wg.Add(1)
		go func(i int, credential string) {
			defer wg.Done()
			tier, onDemand, _ := readBillingAccess(gctx, bridge, cfg.BaseURL, credential)
			results[i] = readResult{tier: tier, onDemand: onDemand}
		}(i, a.Credential)
	}
	// Bound the WAIT too, not just the reads: a bridge that ignores the context
	// would otherwise stall the gate for as long as the slowest read, and this
	// runs inside the refresh that builds the model list.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-gctx.Done():
	}

	accesses := make([]*planfilter.BillingAccess, 0, len(results))
	anyKnown := false
	for _, r := range results {
		if r.tier != nil {
			anyKnown = true
		}
		// A nil tier yields AccessFor's fail-open entry, so an unreadable
		// account is never the reason a model disappears.
		accesses = append(accesses, planfilter.AccessFor(r.tier, r.onDemand))
	}

	if len(accesses) == 0 {
		return nil
	}
	if !anyKnown {
		// Every account's plan was unreadable. A gate built only from unknowns
		// would still publish everything, so skip installing one. This is only
		// worth a log when there WAS an account to read - a pool that simply
		// has no accounts is the ordinary "publish everything" case.
		if len(readable) > 0 && bridge != nil {
			_ = bridge.Log("warn", "plan filter skipped: no account plan could be read; publishing every model", nil)
		}
		return nil
	}

	kept := func(upstreamID string) bool {
		return planfilter.ModelVisibleForAnyAccount(upstreamID, accesses)
	}
	return kept
}

// readBillingAccess reads one credential's plan tier and on-demand credits.
// Both calls are independent and individually optional: a missing subscription
// leaves the tier unknown (nil) while a present credit balance still unlocks
// every model, exactly as the vendor's own access rule does.
func readBillingAccess(ctx context.Context, bridge *HostBridge, baseURL, key string) (*planfilter.SubscriptionPlan, float64, error) {
	base, errBase := AccountAPIBase(baseURL)
	if errBase != nil {
		return nil, 0, errBase
	}
	ctx, cancel := context.WithTimeout(ctx, planGateRefreshTimeout)
	defer cancel()

	get := func(path string) ([]byte, error) {
		resp, errDo := bridge.Do(ctx, pluginapi.HTTPRequest{
			Method: http.MethodGet,
			URL:    base + path,
			Headers: http.Header{
				"Authorization": []string{"Bearer " + key},
				"Accept":        []string{"application/json"},
				"User-Agent":    []string{"commandcode"},
			},
		})
		if errDo != nil {
			return nil, fmt.Errorf("request failed")
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("rejected (%d)", resp.StatusCode)
		}
		return resp.Body, nil
	}

	var plan *planfilter.SubscriptionPlan
	if raw, errSub := get(accountSubscriptionPath); errSub == nil {
		var sub accountSubscription
		if json.Unmarshal(raw, &sub) == nil && sub.Success {
			plan = planfilter.SubscriptionPlanFor(sub.Data.PlanID)
		}
	}

	onDemand := 0.0
	if raw, errCr := get(accountCreditsPath); errCr == nil {
		var credits accountCredits
		if json.Unmarshal(raw, &credits) == nil {
			onDemand = credits.Credits.PurchasedCredits + credits.Credits.FreeCredits
		}
	}
	return plan, onDemand, nil
}
