// retry_after_test.go pins the header parsing rules. The three header forms
// below were observed on the reference gateway; the reject cases exist because
// a wrong wait time is worse than no wait time — the operator reads these
// numbers to decide whether to wait or re-login.
package main

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	hdr := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}

	cases := []struct {
		name   string
		h      http.Header
		wantOK bool
		want   time.Duration
	}{
		{"retry-after seconds", hdr("Retry-After", "90"), true, 90 * time.Second},
		{"retry-after zero padded", hdr("Retry-After", " 007 "), true, 7 * time.Second},
		{"retry-after-ms", hdr("Retry-After-Ms", "1500"), true, 1500 * time.Millisecond},
		{"retry-after wins over ms", hdr("Retry-After", "30", "Retry-After-Ms", "1500"), true, 30 * time.Second},

		// HTTP-date form: not parsed on purpose (see parseRetryAfter).
		{"retry-after http-date ignored", hdr("Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT"), false, 0},
		{"retry-after garbage ignored", hdr("Retry-After", "soon"), false, 0},
		{"retry-after negative ignored", hdr("Retry-After", "-5"), false, 0},
		{"retry-after zero ignored", hdr("Retry-After", "0"), false, 0},
		// Sanity cap: a multi-day value is garbage, not a real wait.
		{"retry-after beyond cap ignored", hdr("Retry-After", "604800"), false, 0},

		{"epoch seconds", hdr("X-Ratelimit-Reset", epochIn(t, 45*time.Second, false)), true, 0}, // duration checked separately
		{"no headers", hdr(), false, 0},
		{"nil header", nil, false, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tc.h)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				if got != 0 {
					t.Fatalf("failed parse must return 0, got %v", got)
				}
				return
			}
			switch tc.name {
			case "epoch seconds":
				// Epoch values are computed from now, so assert the window
				// rather than an exact value.
				if got < 40*time.Second || got > 50*time.Second {
					t.Fatalf("epoch parse = %v, want ~45s", got)
				}
			default:
				if got != tc.want {
					t.Fatalf("duration = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// epochIn renders an instant N from now as the header expects: seconds for
// near-term values, milliseconds for the 12+ digit form.
func epochIn(t *testing.T, d time.Duration, millis bool) string {
	t.Helper()
	at := time.Now().Add(d)
	if millis {
		return itoa(at.UnixMilli())
	}
	return itoa(at.Unix())
}

func TestParseRetryAfterEpochMilliseconds(t *testing.T) {
	h := http.Header{}
	h.Set("X-Ratelimit-Reset", epochIn(t, 30*time.Second, true))
	got, ok := parseRetryAfter(h)
	if !ok {
		t.Fatal("millisecond epoch form should parse")
	}
	if got < 25*time.Second || got > 35*time.Second {
		t.Fatalf("millisecond epoch parse = %v, want ~30s", got)
	}
}

// A stale epoch (in the past) must not produce a negative wait.
func TestParseRetryAfterPastEpochRejected(t *testing.T) {
	h := http.Header{}
	h.Set("X-Ratelimit-Reset", itoa(time.Now().Add(-time.Hour).Unix()))
	if _, ok := parseRetryAfter(h); ok {
		t.Fatal("past epoch must not parse")
	}
}

func TestRetryAfterHintFormat(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "59")
	if got := retryAfterHint(h); got != "60s" {
		t.Fatalf("retryAfterHint = %q, want %q (rounded up)", got, "60s")
	}
	if got := retryAfterHint(http.Header{}); got != "" {
		t.Fatalf("empty headers must render empty, got %q", got)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
