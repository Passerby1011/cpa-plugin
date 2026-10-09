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
		`'<span class="badge plan">套餐 ' + esc(usage.plan) + '</span>'`,
		`'<span class="badge unknown">套餐未知</span>'`,
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

// TestQuotaPageBadgesNeverSplitMidPill pins the card-header layout fix.
//
// The header used to be a plain flex row whose badge group could be squeezed:
// a long plan pill ("套餐 Go · 月额度 10") wrapped its own text onto a second
// line, and with the email winning the row the whole badge group dropped below
// it. Both read as a broken two-layer header. The contract is now: the name
// yields first (basis 0, ellipsised), the badge group never shrinks, and no pill
// may break its text - a crowded card wraps the GROUP instead.
func TestQuotaPageBadgesNeverSplitMidPill(t *testing.T) {
	page := resources.QuotaPage
	for _, marker := range []string{
		".badge{font-size:11px;font-weight:600;padding:3px 10px;border-radius:99px;white-space:nowrap;",
		".card-name{flex:1 1 0;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}",
		".card-badges{display:flex;gap:6px;align-items:center;flex:0 0 auto;max-width:100%;flex-wrap:wrap;justify-content:flex-end;margin-left:auto}",
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("quota page lost the badge layout guard: missing %q", marker)
		}
	}
	// The plan pill must stay short: repeating the monthly allowance here is
	// what made it long enough to squeeze the header in the first place.
	if strings.Contains(page, `' · 月额度 '`) {
		t.Fatal("quota page repeats the monthly allowance inside the plan pill")
	}
}
