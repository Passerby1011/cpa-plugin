package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/hex-ci/cpa-plugin/commandcode/internal/config"
	"github.com/hex-ci/cpa-plugin/commandcode/internal/planfilter"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// planGateRefreshTimeout bounds the two account reads behind the plan filter.
// They are best-effort: a failure yields an unknown tier, which the filter
// treats as "show everything" rather than guessing a tier and hiding models.
const planGateRefreshTimeout = 15 * time.Second

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
	var accesses []*planfilter.BillingAccess
	anyKnown := false

	// Only go-cli accounts carry a plan worth reading: an api-keys entry is a
	// bare provider credential with no account identity behind it, so a pool
	// built from those has nothing to gate on and publishing everything is the
	// correct outcome, not a degraded one.
	for _, a := range cfg.Accounts {
		if a.Credential == "" || a.Mode != config.TransportGoCLI {
			continue
		}
		tier, onDemand, errRead := readBillingAccess(parent, bridge, cfg.BaseURL, a.Credential)
		if errRead != nil {
			// Unknown account: keep it in the pool as a fail-open entry so its
			// possible access is still honoured, and note why.
			accesses = append(accesses, &planfilter.BillingAccess{})
			continue
		}
		if tier != nil {
			anyKnown = true
		}
		accesses = append(accesses, planfilter.AccessFor(tier, onDemand))
	}

	if len(accesses) == 0 {
		return nil
	}
	if !anyKnown {
		// Every account's plan was unreadable. A gate built only from unknowns
		// would still publish everything, so skip installing one. This is only
		// worth a log when there WAS an account to read - a pool that simply
		// has no accounts is the ordinary "publish everything" case.
		if len(cfg.Accounts) > 0 && bridge != nil {
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
