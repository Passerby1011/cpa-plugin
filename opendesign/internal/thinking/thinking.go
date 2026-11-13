// Package thinking maps reasoning controls between the three schemas and onto
// the levels an Open Design model actually declares.
//
// The levels amr-link was measured to take are low/high/max (magpie #141), and
// a model with no levels (GPT-6 Luna) takes reasoning_effort and never thinks.
// So "which control do I send" is a per-model question the catalog answers, and
// this package is the only place that answers it.
//
// The budget <-> level table is the pinned SDK's own (v7.2.138
// internal/thinking/convert.go): fixed thresholds, never proportional
// interpolation, so a caller's budget and a caller's level reach the same
// upstream number here as they do through the host's own executors.
package thinking

import (
	"fmt"
	"strings"
)

// CanonicalLevels is the whole effort space, weakest to strongest. The full
// space is kept (not just the low/high/max a model declares) because a client
// may ask for any of it and the mapping has to reach the nearest level the
// model DOES take. "auto" is deliberately absent: it is a dynamic sentinel,
// not a rankable level.
var CanonicalLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// LevelBudget is the SDK's levelToBudgetMap verbatim: exact per-level budgets,
// no interpolation.
var LevelBudget = map[string]int64{
	"minimal": 512,
	"low":     1024,
	"medium":  8192,
	"high":    24576,
	"xhigh":   32768,
	"max":     128000,
}

// budgetThresholds are the SDK's ConvertBudgetToLevel fixed thresholds
// (ThresholdMinimal/Low/Medium/High = 512/1024/8192/24576).
var budgetThresholds = []struct {
	limit int64
	level string
}{
	{512, "minimal"},
	{1024, "low"},
	{8192, "medium"},
	{24576, "high"},
}

// BudgetToLevel maps a thinking budget onto a canonical level by the SDK's fixed
// thresholds: <=512 minimal, <=1024 low, <=8192 medium, <=24576 high, else
// xhigh. "max" is never derived from a budget.
func BudgetToLevel(budget int64) string {
	for _, t := range budgetThresholds {
		if budget <= t.limit {
			return t.level
		}
	}
	return "xhigh"
}

// ClampToSupported returns level when the model declares it, otherwise the
// nearest declared level by index distance in CanonicalLevels order, preferring
// the WEAKER level on ties (the SDK's clampLevel behaviour). An empty supported
// set returns level unchanged: there is nothing to clamp to, and the caller
// decides whether to send the control at all.
func ClampToSupported(level string, supported []string) string {
	if len(supported) == 0 {
		return level
	}
	for _, s := range supported {
		if strings.EqualFold(s, level) {
			return s
		}
	}
	pos, ok := levelIndex(level)
	if !ok {
		// Not canonical: nothing sensible to measure distance from.
		return supported[0]
	}
	bestIdx, bestDist, bestName := -1, len(CanonicalLevels)+1, supported[0]
	for _, s := range supported {
		idx, found := levelIndex(s)
		if !found {
			continue
		}
		dist := pos - idx
		if dist < 0 {
			dist = -dist
		}
		if dist < bestDist || (dist == bestDist && idx < bestIdx) {
			bestIdx, bestDist, bestName = idx, dist, s
		}
	}
	return bestName
}

func levelIndex(level string) (int, bool) {
	level = strings.ToLower(strings.TrimSpace(level))
	for i, c := range CanonicalLevels {
		if c == level {
			return i, true
		}
	}
	return 0, false
}

// EffortFromBudget maps a Claude thinking budget onto a level the model
// declares.
//
//   - A model with no declared levels returns "" for any budget: it has no
//     level vocabulary, so the caller keeps the budget control instead of
//     inventing a level. (This is what keeps GPT-6 Luna from being handed a
//     reasoning_effort it ignores.)
//   - budget <= 0 means "reasoning off": the weakest declared level is
//     returned only when the model declares "none", otherwise "" so the caller
//     omits the control entirely rather than silently turning thinking on.
//   - An exact match on a declared level's own budget returns THAT level. The
//     threshold mapping below never derives "max" (the SDK's own rule), so
//     without this step a client asking for max's budget of 128000 would come
//     back as "high" — a level it did not ask for, one tier weaker.
//   - Any other budget > 0 derives a level by the SDK thresholds and clamps it
//     to the nearest declared one.
func EffortFromBudget(budget int64, levels []string) string {
	levels = NormalizeLevels(levels)
	if len(levels) == 0 {
		return ""
	}
	if budget <= 0 {
		for _, l := range levels {
			if l == "none" {
				return "none"
			}
		}
		return ""
	}
	// An exact match wins over the threshold table: a budget that IS a level's
	// own number came from that level (this plugin wrote the budget itself when
	// a client asked for a level), so rounding it down loses a tier.
	for _, l := range levels {
		if l == "none" {
			continue
		}
		if b, ok := LevelBudget[l]; ok && b == budget {
			return l
		}
	}
	return ClampToSupported(BudgetToLevel(budget), levels)
}

// BudgetFromEffort maps a declared level onto an Anthropic budget_tokens value.
// ok is false when the level cannot be represented for the model, so the caller
// refuses the request instead of dropping the control.
//
// An effort outside the canonical space is refused rather than clamped to the
// weakest declared level: clamping a value the model has no word for would send a
// budget the client never asked for, which is worse than a stated refusal the
// client can act on. (A canonical-but-undeclared level IS clamped — "medium" on a
// low/high/max model has a nearest neighbour, "turbo" does not.)
func BudgetFromEffort(effort string, levels []string) (int64, bool) {
	e := strings.ToLower(strings.TrimSpace(effort))
	if e == "" {
		return 0, false
	}
	if e == "none" {
		return 0, true
	}
	if _, canonical := levelIndex(e); !canonical {
		return 0, false
	}
	levels = NormalizeLevels(levels)
	if len(levels) > 0 {
		clamped := ClampToSupported(e, levels)
		budget, found := LevelBudget[clamped]
		if !found {
			return 0, false
		}
		return budget, true
	}
	// No declared levels: the upstream is the only authority, so the table value
	// for a level it publishes is forwarded as-is.
	budget, found := LevelBudget[e]
	return budget, found
}

// NormalizeLevels filters a model's declared levels to canonical members, in
// CanonicalLevels order, lowercased. It is deliberately a filter rather than a
// fallback: a model with no levels gets an EMPTY list, which callers read as
// "this model has no level vocabulary" (GPT-6 Luna's measured behaviour).
func NormalizeLevels(levels []string) []string {
	if len(levels) == 0 {
		return nil
	}
	out := make([]string, 0, len(levels))
	for _, canon := range CanonicalLevels {
		for _, l := range levels {
			if strings.EqualFold(strings.TrimSpace(l), canon) {
				out = append(out, canon)
				break
			}
		}
	}
	return out
}

// ThinkingLevels is the levels a model declares, as the plugin publishes them.
// A model with no levels publishes none, which is how a client learns that
// asking for a reasoning level there changes nothing.
func ThinkingLevels(levels []string) []string {
	return NormalizeLevels(levels)
}

// ErrUnsupportedEffort is raised when a client asks for a level the target
// model cannot express AND the value cannot be clamped to one it can. It quotes
// the value exactly as the client wrote it.
func ErrUnsupportedEffort(effort string) error {
	return fmt.Errorf("reasoning effort %q is not supported for this model", effort)
}
