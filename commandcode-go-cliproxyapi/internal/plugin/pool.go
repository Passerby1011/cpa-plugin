package plugin

import (
	"strings"
	"sync"
	"time"

	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/config"
)

// poolState tracks per-credential health so one exhausted or failing account
// does not sink the whole provider.
//
// Deliberately in-memory: a restart resets cooldowns, which is the safe
// direction (an account that was cooled down gets retried once) and keeps the
// plugin free of any persistence the host would have to own.
type poolState struct {
	mu       sync.Mutex
	rrCursor int
	entries  map[string]*accountState
}

type accountState struct {
	// inFlight counts requests currently using this credential.
	inFlight int
	// cooldownUntil is when the credential may be tried again. Zero means
	// it is usable.
	cooldownUntil time.Time
	// lastUsed orders sticky selection: the account touched most recently is
	// the one sticky keeps choosing until it becomes unusable.
	lastUsed time.Time
}

func newPoolState() *poolState {
	return &poolState{entries: make(map[string]*accountState)}
}

func (p *poolState) state(credential string) *accountState {
	st, ok := p.entries[credential]
	if !ok {
		st = &accountState{}
		p.entries[credential] = st
	}
	return st
}

// acquire picks the next usable account and marks it in-flight. It returns nil
// when every candidate is cooled down or at its concurrency cap.
//
// Selection order follows pool.Strategy:
//   - sticky: the most-recently-used usable account, i.e. keep serving from the
//     credential this process used last and only move on when it is cooling
//     down or already at max-concurrency-per-account. This is what keeps a
//     multi-turn conversation on one credential — and therefore on one upstream
//     cache/session — instead of flapping between accounts every request.
//   - round-robin: strict rotation, which spreads load and avoids one
//     credential looking like a burst to upstream risk controls.
//
// The stickiness is deliberately process-scoped, not session-scoped: acquire
// has no session handle to key on. A per-session affinity would need the
// caller to pass a session/stream ID and the state to grow a per-session
// mapping with its own eviction, which is a bigger surface than this pool is
// meant to own. "Prefer the credential used last" already gives most of the
// benefit for the common single-conversation-at-a-time case, so that is all
// this promises; callers must not read it as "requests of one session always
// land on the same account".
//
// With no history at all every credential has a zero lastUsed, so selection
// falls back to config order and the first account becomes the sticky one.
func (p *poolState) acquire(accounts []config.Account, pool config.Pool, now time.Time) *config.Account {
	p.mu.Lock()
	defer p.mu.Unlock()

	limit := pool.MaxConcurrencyPerAccount
	usable := make([]*config.Account, 0, len(accounts))
	for i := range accounts {
		st := p.state(accounts[i].Credential)
		if st.cooldownUntil.After(now) {
			continue
		}
		if limit > 0 && st.inFlight >= limit {
			continue
		}
		usable = append(usable, &accounts[i])
	}
	if len(usable) == 0 {
		return nil
	}

	var picked *config.Account
	if pool.Strategy == config.PoolStrategyRoundRobin {
		start := p.rrCursor % len(usable)
		picked = usable[start]
		p.rrCursor = (start + 1) % len(usable)
	} else {
		picked = usable[0]
		for _, candidate := range usable[1:] {
			// Strictly-after (not Before): the newest lastUsed wins, so the
			// account that served the previous request keeps serving.
			if p.state(candidate.Credential).lastUsed.After(p.state(picked.Credential).lastUsed) {
				picked = candidate
			}
		}
	}

	st := p.state(picked.Credential)
	st.inFlight++
	st.lastUsed = now
	return picked
}

// release marks a request finished. A non-zero cooldown parks the credential
// until then; callers pass zero on success.
func (p *poolState) release(credential string, cooldown time.Duration, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state(credential)
	if st.inFlight > 0 {
		st.inFlight--
	}
	if cooldown > 0 {
		until := now.Add(cooldown)
		if until.After(st.cooldownUntil) {
			st.cooldownUntil = until
		}
	}
}

// cooldownFor maps an upstream failure to how long a credential should rest.
// The values are deliberately modest: they exist to stop hammering a dead or
// throttled account inside one request burst, not to enforce the upstream's
// own rate limits.
func cooldownFor(status int) time.Duration {
	switch {
	case status == 401 || status == 403:
		// Bad key: park it long enough that a human notices before it is
		// retried in a loop.
		return 30 * time.Minute
	case status == 429:
		return 60 * time.Second
	case status >= 500:
		return 15 * time.Second
	default:
		return 0
	}
}

// parseRetryAfter reads a Retry-After header value in either supported form
// (delta seconds or an HTTP date) and bounds it to something sane.
func parseRetryAfter(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := time.ParseDuration(raw + "s"); err == nil && seconds > 0 {
		if seconds > 30*time.Minute {
			return 30 * time.Minute
		}
		return seconds
	}
	if when, err := time.Parse(time.RFC1123, raw); err == nil {
		if d := when.Sub(now); d > 0 {
			if d > 30*time.Minute {
				return 30 * time.Minute
			}
			return d
		}
	}
	return 0
}
