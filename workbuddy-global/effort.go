// effort.go downgrades a caller's reasoning_effort to one the target model
// actually accepts.
//
// Why the downgrade is necessary rather than optional: the upstream validates
// the tier against the model's own list and answers 400 for a tier the model
// does not offer (measured). A client that hardcodes "high" — the common case,
// since most models do offer it — breaks as soon as it targets a model whose
// list omits it, and the request is lost rather than served at a nearby tier.
//
// The tiers form one ordered scale; the requested tier keeps its meaning when
// mapped onto the model's sublist:
//
//	requested tier not in the model's list  -> highest model tier <= requested
//	all model tiers above the requested one -> lowest model tier
//	unknown model / no tier list / unset    -> passthrough untouched
//
// "Passthrough" is deliberate: with no metadata there is no evidence the tier
// is rejected, and rewriting on a guess would change behaviour for every model
// the catalogue does not describe.
package main

import (
	"fmt"
	"strings"
	"sync"
)

// effortRank orders the tiers the upstream recognises. The scale is ordinal,
// not numeric: a downgrade picks a neighbour by position, never by arithmetic
// on names.
var effortRank = map[string]int{
	"off":     0,
	"minimal": 1,
	"low":     2,
	"medium":  3,
	"high":    4,
	"xhigh":   5,
	"max":     6,
}

// normalizeReasoningEffort returns the tier to send for a model whose accepted
// tiers are `supported`. requested is the caller's tier; an empty supported
// list, an unknown model, or an unrecognised tier name all pass the request
// through unchanged.
func normalizeReasoningEffort(requested string, supported []string) string {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "" || len(supported) == 0 {
		return requested
	}
	requestedRank, ok := effortRank[requested]
	if !ok {
		// A tier the plugin does not know (a future upstream tier, or a typo)
		// is forwarded as-is: without a rank there is nothing to compare, and
		// silently replacing it would be inventing a decision.
		return requested
	}

	best := ""
	bestRank := -1
	lowest := ""
	lowestRank := -1
	for _, tier := range supported {
		tier = strings.ToLower(strings.TrimSpace(tier))
		rank, ok := effortRank[tier]
		if !ok {
			continue
		}
		if rank <= requestedRank && rank > bestRank {
			best, bestRank = tier, rank
		}
		if lowestRank == -1 || rank < lowestRank {
			lowest, lowestRank = tier, rank
		}
	}
	if bestRank >= 0 {
		return best
	}
	if lowestRank >= 0 {
		// Every accepted tier sits above the request: the caller asked for
		// less thinking than the model can do, so give it the least it has.
		return lowest
	}
	// The model's list held no tier this plugin understands; leave the request
	// alone rather than guessing among unranked values.
	return requested
}

// applyEffortDowngradeInPlace rewrites reasoning_effort on the outgoing body
// when the target model's tiers do not include it. Both spellings clients use
// are handled (top-level reasoning_effort and reasoning.effort).
//
// A model with no tier metadata is left untouched — see the package comment.
func applyEffortDowngradeInPlace(obj map[string]any, supported []string) bool {
	if len(supported) == 0 || obj == nil {
		return false
	}
	changed := false
	if requested, ok := obj["reasoning_effort"].(string); ok && strings.TrimSpace(requested) != "" {
		if next := normalizeReasoningEffort(requested, supported); !strings.EqualFold(strings.TrimSpace(requested), next) {
			logEffortDowngrade("reasoning_effort", requested, next, supported)
			obj["reasoning_effort"] = next
			changed = true
		}
	}
	if reasoning, ok := obj["reasoning"].(map[string]any); ok {
		if requested, ok := reasoning["effort"].(string); ok && strings.TrimSpace(requested) != "" {
			if next := normalizeReasoningEffort(requested, supported); !strings.EqualFold(strings.TrimSpace(requested), next) {
				logEffortDowngrade("reasoning.effort", requested, next, supported)
				reasoning["effort"] = next
				changed = true
			}
		}
	}
	return changed
}

// effortLogDedup throttles downgrade logs to one line per (field, requested,
// resolved) combination. A downgrade is routine (any client hardcoding "high"
// hits it on a single-tier model), so an un-throttled log would flood main.log
// on a busy gateway; but a silent rewrite would make "did the mapping run?"
// unanswerable from the logs — which is exactly how a wrong mapping hides.
var effortLogDedup sync.Map

func logEffortDowngrade(field, requested, resolved string, supported []string) {
	key := field + "|" + strings.ToLower(strings.TrimSpace(requested)) + "|" + resolved
	if _, seen := effortLogDedup.LoadOrStore(key, struct{}{}); seen {
		return
	}
	hostLogf("info", fmt.Sprintf("workbuddy-global effort downgrade: %s %q -> %q (model supports %v)",
		field, requested, resolved, supported))
}

// modelEffortsForRequest resolves the accepted tiers for a model id from the
// current runtime snapshot. It returns nil when the catalogue has no record of
// the model, which the downgrade reads as "leave the request alone".
func modelEffortsForRequest(authID, modelID string) []string {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	snapshot := currentModelRuntime().snapshotForAuthID(authID)
	for i := range snapshot.Models {
		if !strings.EqualFold(snapshot.Models[i].ID, modelID) {
			continue
		}
		if snapshot.Models[i].Thinking == nil {
			return nil
		}
		return snapshot.Models[i].Thinking.Levels
	}
	return nil
}
