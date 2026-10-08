package planfilter

import (
	"math"
	"strings"
)

// BillingAccess is what the filter needs to know about one account.
//
// TierWeight is the account's rank on the plan scale; nil means "unknown"
// (no subscription was read, or the plan id was not recognised). OnDemandCredits
// is purchased + free credit balance: the vendor's own access model grants
// every model while any on-demand credits remain, regardless of plan.
type BillingAccess struct {
	TierWeight      *int
	OnDemandCredits float64
}

// ModelVisibleInPlan reports whether the picker should list modelID for an
// account with this access.
//
// It FAILS OPEN at every uncertainty - unknown access, unknown plan, and a
// model absent from knownPlans all keep the model visible. Hiding a model the
// operator can actually run is the one failure this filter must never cause,
// because the cost is invisible: the model simply is not there to pick. The
// server stays the final gate, so showing one model too many costs a single
// 403 on an explicit request.
func ModelVisibleInPlan(modelID string, access *BillingAccess) bool {
	if access == nil {
		return true
	}
	if access.OnDemandCredits > 0 {
		return true
	}
	if access.TierWeight == nil {
		return true
	}
	weight := *access.TierWeight
	if math.IsNaN(float64(weight)) || math.IsInf(float64(weight), 0) {
		return true
	}
	tier, ok := knownPlans[modelID]
	if !ok {
		return true
	}
	modelWeight, ok := planOrder[tier]
	if !ok {
		return true
	}
	return modelWeight <= weight
}

// ModelVisibleForAnyAccount is the pool-level rule: a model is advertised when
// AT LEAST ONE account can run it.
//
// The pool - not any single account - is what serves a request, so keying the
// list on whichever account rotation happened to reach would make models appear
// and vanish as accounts rotated, and would hide models the operator's other
// accounts could run. An empty or nil pool means "show everything": with no
// accounts there is nothing to filter by, and hiding is the failure mode to
// avoid.
func ModelVisibleForAnyAccount(modelID string, accounts []*BillingAccess) bool {
	if len(accounts) == 0 {
		return true
	}
	for _, a := range accounts {
		if ModelVisibleInPlan(modelID, a) {
			return true
		}
	}
	return false
}

// FilterModels keeps the models any account in the pool can run, preserving
// input order. ids are UPSTREAM model ids.
func FilterModels(ids []string, accounts []*BillingAccess) []string {
	if len(accounts) == 0 {
		return append([]string(nil), ids...)
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if ModelVisibleForAnyAccount(id, accounts) {
			out = append(out, id)
		}
	}
	return out
}

// SubscriptionPlanFor resolves a planId to its facts, longest-prefix first so
// "individual-goat" is not read as "individual-go". An unrecognised plan
// returns nil, which callers must treat as "unknown" (fail open) rather than
// as "lowest tier" - guessing low would hide models the account can run.
func SubscriptionPlanFor(planID string) *SubscriptionPlan {
	id := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(planID, "_", "-")))
	if id == "" {
		return nil
	}
	if p, ok := subscriptionPlans[id]; ok {
		cp := p
		return &cp
	}
	for _, prefix := range subscriptionPlanPrefixes {
		if strings.HasPrefix(id, prefix) {
			cp := subscriptionPlans[prefix]
			return &cp
		}
	}
	return nil
}

// AccessFor derives the filter input from a resolved plan and a credit balance.
// A nil plan (unknown planId) yields nil TierWeight, which fails open.
func AccessFor(plan *SubscriptionPlan, onDemandCredits float64) *BillingAccess {
	if plan == nil {
		return &BillingAccess{OnDemandCredits: onDemandCredits}
	}
	weight := plan.TierWeight
	return &BillingAccess{TierWeight: &weight, OnDemandCredits: onDemandCredits}
}
