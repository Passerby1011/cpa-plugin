package gocli

import (
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
)

// strictUUID matches a real RFC 4122 UUID: version nibble 1-5 and a variant
// nibble of 8/9/a/b. This is what the upstream enforces - it rejected a merely
// UUID-shaped id with `400 Invalid UUID at "threadId"`.
var strictUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestNormalizeThreadIDIsAStrictUUID is the regression test for the
// intermittent 400.
//
// The derivation used to take group 4 straight from the SHA-1 digest, so its
// first hex was uniformly distributed. A real UUID requires that nibble to be
// 8/9/a/b, which left roughly three of every four sessions invalid: the same
// request failed or succeeded by luck of the hash. A single sample proves
// nothing, so this sweeps many session keys.
func TestNormalizeThreadIDIsAStrictUUID(t *testing.T) {
	bad := 0
	for i := 0; i < 2000; i++ {
		key := "session-" + strings.Repeat("x", i%7) + hex.EncodeToString([]byte{byte(i), byte(i >> 8)})
		got := NormalizeThreadID(key)
		if !strictUUID.MatchString(got) {
			bad++
			if bad <= 5 {
				t.Errorf("NormalizeThreadID(%q) = %q, not a strict UUID", key, got)
			}
		}
	}
	if bad != 0 {
		t.Fatalf("%d of 2000 derived thread ids were not strict UUIDs (upstream rejects these with 400)", bad)
	}
}

// TestNormalizeThreadIDIsDeterministic pins the other half of the contract:
// the same session must always map to the same thread, or upstream loses
// conversation continuity.
func TestNormalizeThreadIDIsDeterministic(t *testing.T) {
	const key = "same-session-key"
	first := NormalizeThreadID(key)
	for i := 0; i < 50; i++ {
		if got := NormalizeThreadID(key); got != first {
			t.Fatalf("derivation is not stable: %q vs %q", got, first)
		}
	}
	if NormalizeThreadID("other-key") == first {
		t.Fatal("different sessions collapsed to the same thread id")
	}
}

// TestNormalizeThreadIDPassesThroughRealUUIDs pins that an already-valid UUID
// is used verbatim, so a client that supplies its own thread id keeps it.
func TestNormalizeThreadIDPassesThroughRealUUIDs(t *testing.T) {
	const real = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	if got := NormalizeThreadID(real); got != real {
		t.Fatalf("a valid UUID was rewritten: %q", got)
	}
	if got := NormalizeThreadID("   "); got != "" {
		t.Fatalf("blank session should yield an empty thread id, got %q", got)
	}
}

// TestThreadIDOmittedWhenNotAUUID pins the envelope rule: a thread id that is
// not a UUID is OMITTED rather than sent malformed.
func TestThreadIDOmittedWhenNotAUUID(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	env, err := BuildEnvelope(body, Options{ThreadID: "not-a-uuid"})
	if err != nil {
		t.Fatalf("BuildEnvelope: %v", err)
	}
	if strings.Contains(string(env), "threadId") {
		t.Fatalf("a malformed threadId was sent: %s", env)
	}
}
