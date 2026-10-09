package plugin

import (
	"strings"
	"testing"

	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/resources"
)

// TestQuotaPageSpellsOutThePlanType pins the card's plan badge.
//
// The plan must be stated IN WORDS next to the account name, not only carried by
// a bare acronym: the panel lets a credential be labelled "go", which an
// operator reads as the plan. An unknown plan must say 套餐未知 rather than
// render nothing, and a known plan carries its monthly allowance so the number
// can be sanity-checked against the meter below it.
func TestQuotaPageSpellsOutThePlanType(t *testing.T) {
	page := resources.QuotaPage
	for _, marker := range []string{
		`function planBadge(usage) {`,
		`'<span class="badge plan">套餐 ' + esc(usage.plan)`,
		`' · 月额度 ' + num(usage.plan_credits).toFixed(0)`,
		`'<span class="badge unknown">套餐未知</span>'`,
		"plan_credits",
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("quota page does not spell out the plan: missing %q", marker)
		}
	}
}

// TestQuotaPagePlanChipStaysWithinManualRefreshRules re-asserts the page's
// contract around the plan badge: no polling, no timers, no expiry logic may
// creep in with the markup.
func TestQuotaPagePlanChipStaysWithinManualRefreshRules(t *testing.T) {
	page := resources.QuotaPage
	for _, marker := range []string{"setInterval", "setTimeout", "visibilitychange", "TTL", "expiry"} {
		if strings.Contains(page, marker) {
			t.Fatalf("quota page gained automatic behaviour %q", marker)
		}
	}
}
