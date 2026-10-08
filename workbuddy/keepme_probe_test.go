package main

import (
	"fmt"
	"os"
	"testing"
)

// Why did "keep me" lose its content? Report booleans/lengths only.
func TestProbeWhyKeepMeLost(t *testing.T) {
	in := "keep me"
	has := hasFingerprint(in)
	out := sanitizeBlockedTemplates(in)

	var b string
	b += fmt.Sprintf("input=%q\n", in)
	b += fmt.Sprintf("hasFingerprint=%v\n", has)
	b += fmt.Sprintf("output_len=%d output_empty=%v\n", len(out), out == "")
	// Does either regex match ordinary text?
	b += fmt.Sprintf("billingRE_match=%v\n", sanitizeBillingHeaderRE.MatchString(in))
	b += fmt.Sprintf("entrypointRE_match=%v\n", sanitizeCCEntrypointRE.MatchString(in))
	b += fmt.Sprintf("keyvalueRE_match=%v\n", sanitizeCCKeyValueRE.MatchString(in))
	_ = os.WriteFile("/tmp/keepme_probe.txt", []byte(b), 0o644)
}
