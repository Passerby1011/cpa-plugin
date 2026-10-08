package plugin

import (
	"strings"
	"testing"

	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/resources"
)

// TestQuotaPageSpellsOutThePlanType pins the plan chip.
//
// The card already carried the plan in a 28px round badge, but that badge is a
// bare acronym an operator reads as the account's own label (the panel lets a
// credential be labelled "go"), so the plan was effectively invisible. The chip
// states it in words, and an unknown plan must say so rather than render
// nothing.
func TestQuotaPageSpellsOutThePlanType(t *testing.T) {
	page := resources.QuotaPage
	for _, marker := range []string{
		"credential-plan",
		`"套餐 " + plan`,
		`" · 月额度 "`,
		`"套餐未知"`,
		`planChip.className = "credential-plan"`,
		"plan_credits",
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("quota page does not spell out the plan: missing %q", marker)
		}
	}
}

// TestQuotaPagePlanChipStaysWithinManualRefreshRules re-asserts the page's
// contract after the plan chip was added: no polling, no timers, no expiry
// logic may creep in with the new markup.
func TestQuotaPagePlanChipStaysWithinManualRefreshRules(t *testing.T) {
	page := resources.QuotaPage
	for _, marker := range []string{"setInterval", "setTimeout", "visibilitychange", "TTL", "expiry"} {
		if strings.Contains(page, marker) {
			t.Fatalf("quota page gained automatic behaviour %q", marker)
		}
	}
}
