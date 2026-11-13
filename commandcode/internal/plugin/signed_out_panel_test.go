package plugin

import (
	"strings"
	"testing"

	"github.com/hex-ci/cpa-plugin/commandcode/resources"
)

// TestQuotaPageOffersSignInAgainOnlyWhenExpired pins the panel entry's
// contract: the row offers "重新登录" only for a credential Command Code
// refused EVERYWHERE ("expired"), never for a card that merely failed to read.
// Offering it on every error would send the operator to re-authenticate keys
// that work, which is the failure this whole check exists to prevent.
func TestQuotaPageOffersSignInAgainOnlyWhenExpired(t *testing.T) {
	page := resources.QuotaPage
	for _, marker := range []string{
		`function signedOutBadge(card) {`,
		`function signedOutAction(card) {`,
		`data-signin="`,
		`event.target.closest("[data-signin]")`,
		// The verdict is read from the plugin's own field, never inferred
		// from an HTTP status the page does not even receive.
		`if (typeof result.sign_in === "string") card.sign_in = result.sign_in;`,
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("quota page missing sign-in marker %q", marker)
		}
	}
	// Each entry must key off the SAME verdict, and each is checked in its OWN
	// body: matching the guard anywhere in the file let a mutant that inverted
	// only the action's condition survive, because every other assertion still
	// found the badge's guard elsewhere in the page.
	for _, fn := range []struct{ name, copy string }{
		{"signedOutBadge", "已失效"},
		{"signedOutAction", "重新登录"},
	} {
		head := "function " + fn.name + "(card) {"
		start := strings.Index(page, head)
		if start < 0 {
			t.Fatalf("%s not found", fn.name)
		}
		end := strings.Index(page[start:], "\n  }")
		if end < 0 {
			t.Fatalf("%s body not terminated", fn.name)
		}
		body := page[start : start+end]
		if !strings.Contains(body, `if (card.sign_in !== "expired") return "";`) {
			t.Fatalf("%s does not gate on \"expired\":\n%s", fn.name, body)
		}
		if strings.Contains(body, `card.sign_in === "kept"`) {
			t.Fatalf("%s inverts the verdict (offers the entry to healthy keys):\n%s", fn.name, body)
		}
		if !strings.Contains(body, fn.copy) {
			t.Fatalf("%s does not carry its copy %q", fn.name, fn.copy)
		}
	}
	// The entry must be rendered OUTSIDE the usage branch: an expired card has
	// no usage (there is nothing to read), so a control inside the metered
	// section would never appear exactly when it is needed.
	cardStart := strings.Index(page, "function cardHTML(card)")
	cardEnd := strings.Index(page, "function accountCredit(usage)")
	if cardStart < 0 || cardEnd <= cardStart {
		t.Fatal("cardHTML section not found")
	}
	cardBody := page[cardStart:cardEnd]
	for _, marker := range []string{"signedOutBadge(card)", "signedOutAction(card)"} {
		if !strings.Contains(cardBody, marker) {
			t.Fatalf("cardHTML does not render %q", marker)
		}
	}
	if strings.Contains(cardBody, `badge exceeded" title=`) {
		t.Fatal("the expired badge is inlined instead of going through signedOutBadge")
	}
}
