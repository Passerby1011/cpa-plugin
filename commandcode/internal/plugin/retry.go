package plugin

import (
	"strings"
	"time"
)

// retryPolicy is the resolved retry configuration for one attempt loop.
type retryPolicy struct {
	// max is the number of RETRIES, so max+1 attempts in total. 0 disables.
	max         int
	baseBackoff time.Duration
}

// newRetryPolicy resolves the configured retry block. A negative max is treated
// as 0 (disabled) rather than panicking.
func newRetryPolicy(max int, base time.Duration) retryPolicy {
	if max < 0 {
		max = 0
	}
	if base <= 0 {
		base = 400 * time.Millisecond
	}
	return retryPolicy{max: max, baseBackoff: base}
}

// enabled reports whether any retry is permitted.
func (p retryPolicy) enabled() bool { return p.max > 0 }

// backoffFor is how long to wait before attempt number n (1-based). The
// reference waits base*n, i.e. the FIRST retry waits one step, the second waits
// two. This spreads a burst of simultaneous failures instead of retrying them
// in lockstep.
func (p retryPolicy) backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	return p.baseBackoff * time.Duration(attempt)
}

// isRetryableTransportFlip reports whether an error is a TRANSPORT-LEVEL
// upstream drop that is safe to retry, as opposed to a semantic error the
// upstream deliberately sent.
//
// The distinction matters more than the retry itself: 429 and 503 are the
// upstream telling the caller to back off or to expect a different answer, and
// retrying those internally would swallow a signal the client needs to see (and
// would multiply load during exactly the incident it is meant to relieve).
//
// Recognised flip signatures mirror the reference implementation; the list is
// intentionally a subset of Node's undici errors translated to what the Go
// host surfaces.
func isRetryableTransportFlip(msg string) bool {
	m := strings.ToLower(msg)
	if m == "" {
		return false
	}
	// Never retry an idle-watchdog abort: that is OUR signal asking the client
	// to reduce context, not an upstream fault.
	if strings.Contains(m, "idle") || strings.Contains(m, "watchdog") {
		return false
	}
	for _, sig := range []string{
		"terminated",
		"econnreset",
		"econnrefused",
		"epipe",
		"etimedout",
		"und_err_socket",
		"socket hang up",
		"other side closed",
		"connection reset by peer",
		"unexpected eof",
		"broken pipe",
		"fetch failed",
		"server closed",
		"connection closed",
		// Go's net package surfaces the same faults with different spellings
		// than Node's errno names above. Without these the Go host's own
		// "connection refused"/"i/o timeout" drops would fall through as
		// non-retryable and the mechanism would silently miss real flips.
		"connection refused",
		"i/o timeout",
		"no such host",
		"network is unreachable",
		"connection attempt failed",
	} {
		if strings.Contains(m, sig) {
			return true
		}
	}
	return false
}

// streamEndedWithoutFinish builds the classified error for a CLI stream that
// ended before its finish event. Shared by the streaming and non-streaming
// paths so both describe the same fault identically.
func streamEndedWithoutFinish() string {
	return "upstream CLI stream ended without a finish event"
}
