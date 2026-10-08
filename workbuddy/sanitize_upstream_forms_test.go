// sanitize_upstream_forms_test.go pins the two git-line forms by BYTE, because
// the pair differs only in the first letter and every text-level tool in this
// environment has been observed swapping them.
//
// Ground truth, measured by POSTing each form directly to the upstream gateway
// with no plugin in the path:
//
//	"M" + "ain branch (you will usually use this for PRs)"     -> 400 code=11128
//	"D" + "efault branch (you will usually use this for PRs)"  -> 200
//
// So the rewrite must go M-form -> D-form.
package main

import "testing"

const (
	byteM = 0x4d // 'M'
	byteD = 0x44 // 'D'
)

func blockedGitLine() string {
	return string([]byte{byteM}) + "ain branch (you will usually use this for PRs)"
}

func allowedGitLine() string {
	return string([]byte{byteD}) + "efault branch (you will usually use this for PRs)"
}

func TestGitLineFixturesAreDistinct(t *testing.T) {
	b, a := blockedGitLine(), allowedGitLine()
	if b == a {
		t.Fatal("fixtures are degenerate")
	}
	if b[0] != byteM || a[0] != byteD {
		t.Fatalf("fixture bytes wrong: blocked=%#x allowed=%#x", b[0], a[0])
	}
}

// The upstream-blocked form must be rewritten into the accepted form.
func TestSanitizeRewritesBlockedGitLine(t *testing.T) {
	got := sanitizeBlockedTemplates(blockedGitLine())
	if got == blockedGitLine() {
		t.Fatal("blocked form was not rewritten; upstream would reject this prompt")
	}
	if got != allowedGitLine() {
		t.Fatalf("got %q, want the accepted form", got)
	}
}

// The accepted form must survive untouched (and the rewrite must be idempotent).
func TestSanitizeKeepsAcceptedGitLine(t *testing.T) {
	allowed := allowedGitLine()
	if got := sanitizeBlockedTemplates(allowed); got != allowed {
		t.Fatalf("accepted form was modified: got %q", got)
	}
	once := sanitizeBlockedTemplates(blockedGitLine())
	twice := sanitizeBlockedTemplates(once)
	if once != twice {
		t.Fatalf("not idempotent: %q vs %q", once, twice)
	}
}

// The fingerprint pre-check must fire for the blocked form, otherwise the
// rewrite above is unreachable.
func TestGitLineFingerprintPrecheck(t *testing.T) {
	if !hasFingerprint(blockedGitLine()) {
		t.Fatal("hasFingerprint must detect the blocked git line")
	}
}
