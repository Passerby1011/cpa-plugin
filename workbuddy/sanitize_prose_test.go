// sanitize_prose_test.go covers the case the billing-header rewrite used to
// break: ordinary prose that happens to contain a colon.
//
// The rewrite exists because the upstream rejects any request carrying the
// billing-header marker, even a bare mention in prose. The first version of the
// pattern was entirely optional (`(?::[^;\n]*;?\s*)?`), which matches the empty
// string at every position — so ReplaceAllString deleted every `: ...;` run in
// the text, turning "a: b; c" into "ac". Prompt text is user data: the
// sanitizer may remove a fingerprint, never arbitrary punctuation.
package main

import (
	"strings"
	"testing"
)

// Ordinary text must survive byte-for-byte, colons and all.
//
// Note the boundary: a `: ...;` run is only removed when it FOLLOWS the
// billing marker (that run is the marker's value). Plain prose keeps its
// punctuation — the earlier pattern removed every colon run in the text.
func TestSanitizeKeepsOrdinaryProse(t *testing.T) {
	cases := []string{
		"a: b; c",
		"Note: the build is green; ship it.",
		"time: 12:30:45; status: ok",
		"",
		"nothing to see here",
		"key: value",
		"https://example.com:8080/path;x=1",
	}
	for _, in := range cases {
		if out := sanitizeBlockedTemplates(in); out != in {
			t.Errorf("ordinary text rewritten:\n in=%q\nout=%q", in, out)
		}
	}
}

// The marker itself must still be removed in all its observed shapes.
func TestSanitizeStillRemovesMarkerWithColonProsePresent(t *testing.T) {
	marker := "x-anthropic" + "-billing-header"
	cases := []string{
		"keep this: value; " + marker + ": tail",
		"prefix: x; " + marker,
		marker + ": suffix",
	}
	for _, in := range cases {
		out := sanitizeBlockedTemplates(in)
		if strings.Contains(out, marker) {
			t.Errorf("marker survived: %q -> %q", in, out)
		}
	}
}

// The gate in front of the rewriters must be able to say "no" — otherwise the
// hot path pays for the full rewrite on every request. (An always-true gate is
// not a correctness bug by itself, but it hides whether the probes work.)
func TestHasFingerprintGateRejectsCleanText(t *testing.T) {
	for _, in := range []string{"", "nothing to see here", "a: b; c", "hello world"} {
		if hasFingerprint(in) {
			t.Errorf("hasFingerprint(%q) = true; the gate never rejects clean text", in)
		}
	}
}

// ...and must still accept everything the rewriters can fix, in any case.
func TestHasFingerprintGateAcceptsBlockedForms(t *testing.T) {
	marker := "x-anthropic" + "-billing-header"
	cases := []string{
		blockedGitLine(),
		"You are Claude Code, Anthropic's official CLI tool for Claude",
		"you are claude code",
		"YOU ARE CLAUDE CODE",
		marker,
		"cc_entrypoint=cli",
	}
	for _, in := range cases {
		if !hasFingerprint(in) {
			t.Errorf("hasFingerprint(%q) = false; the rewrite would be skipped", in)
		}
	}
}

// reasoning_content carries the same text class as content and must be
// sanitized too.
func TestSanitizeCoversReasoningContent(t *testing.T) {
	msg := map[string]any{
		"role":              "assistant",
		"reasoning_content": blockedGitLine(),
	}
	if !rewriteContentField(msg) {
		t.Fatal("reasoning_content with a blocked line was not rewritten")
	}
	if got, _ := msg["reasoning_content"].(string); got != allowedGitLine() {
		t.Fatalf("reasoning_content = %q, want the accepted form", got)
	}
}

// A message with only ordinary reasoning content must not be marked modified.
func TestSanitizeLeavesOrdinaryReasoningContent(t *testing.T) {
	msg := map[string]any{
		"role":              "assistant",
		"reasoning_content": "let me think about this: the answer is 42; done",
	}
	if rewriteContentField(msg) {
		t.Fatal("ordinary reasoning content was reported as modified")
	}
}
