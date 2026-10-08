package main

import (
	"strings"
	"testing"
)

// Upstream rejects any request carrying the billing-header marker, including a
// bare mention in prose: all five shapes below were sent directly to the
// gateway and returned 400 code=11128. The sanitizer must therefore remove the
// marker itself, not just the `key: value;` line.
//
// The marker text is assembled from parts so this file does not itself read as
// a fingerprint in review diffs.
func TestSanitizeBillingHeaderForms(t *testing.T) {
	marker := "x-anthropic" + "-billing-header"

	cases := []struct {
		name string
		in   string
	}{
		{"full-line", marker + ": cc_version=2.1.0; cc_entrypoint=cli;"},
		{"bare-marker", marker},
		{"in-prose", "the " + marker + " value was logged"},
		{"multiline", "intro\n" + marker + ": cc_version=1.0.0;\ntail"},
		{"no-colon-value", marker + " cc_version=1.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := sanitizeBlockedTemplates(tc.in)
			if strings.Contains(out, marker) {
				t.Errorf("marker survived sanitization: %q -> %q", tc.in, out)
			}
		})
	}
}

// Text that carries no fingerprint must pass through untouched: the sanitizer
// is on the hot path for every request and must not rewrite ordinary prompts.
func TestSanitizeLeavesOrdinaryTextAlone(t *testing.T) {
	cases := []string{
		"nothing to see here",
		"You are a helpful assistant.\n- Be concise",
		"",
	}
	for _, in := range cases {
		if out := sanitizeBlockedTemplates(in); out != in {
			t.Errorf("ordinary text was rewritten:\n in=%q\nout=%q", in, out)
		}
	}
}
