// retry_after.go parses the upstream's "come back in N seconds" hints so the
// plugin can surface them in logs and the panel.
//
// Why this is only a reporting feature: the host's executor-error bridge
// (internal/pluginhost/rpc_client.go decodeEnvelopeResult) reads exactly two
// fields off the plugin envelope — Message and HTTPStatus — and builds an
// rpcPluginError that exposes only Error() and StatusCode(). The host DOES probe
// upstream errors for a RetryAfter() *time.Duration method
// (sdk/cliproxy/auth/conductor.go retryAfterFromError), but that probe applies
// to errors the host itself constructs, so a plugin-side implementation would
// never be seen. pluginapi.Error carries no duration field either. Passing the
// upstream reset instant through therefore needs a core-CPA change; until then
// the parsed value goes to host.log and the panel.
package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// retryAfterSanityCap bounds how far into the future a header may point. A
// larger value is treated as garbage rather than trusted: the upstream reset
// hints we have seen are minutes to a couple of hours, and a bogus multi-day
// value would misreport the account state to the operator.
const retryAfterSanityCap = 2 * time.Hour

// parseRetryAfter extracts a wait duration from the response headers.
//
// Accepted forms, in priority order:
//
//	Retry-After        integer seconds
//	Retry-After-Ms     integer milliseconds
//	X-Ratelimit-Reset  epoch seconds (10 digits) or epoch milliseconds (12+)
//
// A non-numeric Retry-After (the HTTP-date form) is not parsed: computing the
// remaining time from a clock the upstream chose is not something we can do
// reliably, and guessing is worse than reporting nothing. Values that are zero,
// negative, or beyond retryAfterSanityCap are dropped for the same reason.
func parseRetryAfter(h http.Header) (time.Duration, bool) {
	if h == nil {
		return 0, false
	}
	if d, ok := secondsHeader(h.Get("Retry-After")); ok {
		return d, true
	}
	if d, ok := millisecondsHeader(h.Get("Retry-After-Ms")); ok {
		return d, true
	}
	if d, ok := epochHeader(h.Get("X-Ratelimit-Reset")); ok {
		return d, true
	}
	return 0, false
}

func secondsHeader(raw string) (time.Duration, bool) {
	v, ok := parseIntHeader(raw)
	if !ok {
		return 0, false
	}
	return saneDuration(time.Duration(v) * time.Second)
}

func millisecondsHeader(raw string) (time.Duration, bool) {
	v, ok := parseIntHeader(raw)
	if !ok {
		return 0, false
	}
	return saneDuration(time.Duration(v) * time.Millisecond)
}

// epochHeader handles X-Ratelimit-Reset, whose magnitude tells the unit apart:
// seconds fit in 10 digits, milliseconds need 12 or more.
func epochHeader(raw string) (time.Duration, bool) {
	v, ok := parseIntHeader(raw)
	if !ok || v <= 0 {
		return 0, false
	}
	var at time.Time
	if v >= 1e12 {
		at = time.UnixMilli(v)
	} else {
		at = time.Unix(v, 0)
	}
	return saneDuration(time.Until(at))
}

func parseIntHeader(raw string) (int64, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func saneDuration(d time.Duration) (time.Duration, bool) {
	if d <= 0 || d > retryAfterSanityCap {
		return 0, false
	}
	return d, true
}

// retryAfterHint renders the parsed wait for a log line, or "" when the
// upstream sent nothing usable.
func retryAfterHint(h http.Header) string {
	d, ok := parseRetryAfter(h)
	if !ok {
		return ""
	}
	// Round up to whole seconds: the operator reads this as "about N seconds".
	secs := int64(d/time.Second) + 1
	return strconv.FormatInt(secs, 10) + "s"
}
