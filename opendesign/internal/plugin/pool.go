// Package plugin — per-credential selection and cooldowns.
//
// The pool is what makes several accounts useful: a refused or throttled
// credential steps aside for a while instead of being retried forever, and the
// next request goes to one that is healthy. "Healthy" here means "has not been
// refused recently" — this plugin has no quota numbers of its own to compare,
// because only an account sign-in can read a plan.
package plugin

import (
	"strings"
	"sync"
	"time"

	"github.com/hex-ci/cpa-plugin/opendesign/internal/config"
)

// cooldowns map an upstream status onto how long a credential steps aside.
//
// The numbers matter more than they look: a 401/403 is the key being refused,
// which does not fix itself, so the cooldown is long enough that the account
// stops being chosen but not permanent — the operator re-signs in and the pool
// retries it. A 429 is a throttle, which does fix itself.
var cooldowns = map[int]time.Duration{
	401: 30 * time.Minute,
	403: 30 * time.Minute,
	402: 10 * time.Minute,
	429: 2 * time.Minute,
	500: 30 * time.Second,
	502: 30 * time.Second,
	503: 60 * time.Second,
	504: 60 * time.Second,
}

// cooldownFor is the cooldown one status earns; 0 means "no penalty".
func cooldownFor(status int) time.Duration {
	if d, ok := cooldowns[status]; ok {
		return d
	}
	if status >= 500 {
		return 30 * time.Second
	}
	return 0
}

// lease is one acquired credential.
type lease struct {
	account config.Account
}

// poolState tracks cooldowns and per-credential concurrency.
type poolState struct {
	mu sync.Mutex
	// until is when a credential becomes usable again.
	until map[string]time.Time
	// inFlight counts requests currently held per credential.
	inFlight map[string]int
	// last is the credential the previous request used, for sticky strategy.
	last string
	// cursor advances the round-robin choice.
	cursor int
}

func newPoolState() *poolState {
	return &poolState{until: map[string]time.Time{}, inFlight: map[string]int{}}
}

// retain drops the state of credentials that are no longer configured. A
// reconfigure must not keep a cooldown for a key the operator removed.
func (p *poolState) retain(accounts []config.Account) {
	want := make(map[string]struct{}, len(accounts))
	for _, a := range accounts {
		want[strings.TrimSpace(a.RuntimeKey)] = struct{}{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for key := range p.until {
		if _, ok := want[key]; !ok {
			delete(p.until, key)
		}
	}
	for key := range p.inFlight {
		if _, ok := want[key]; !ok {
			delete(p.inFlight, key)
		}
	}
	if _, ok := want[p.last]; !ok {
		p.last = ""
	}
}

// acquire picks a credential, or nil when every one is cooling down or at its
// concurrency cap.
func (p *poolState) acquire(accounts []config.Account, pool config.Pool, now time.Time) *lease {
	if len(accounts) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	usable := make([]config.Account, 0, len(accounts))
	for _, a := range accounts {
		key := strings.TrimSpace(a.RuntimeKey)
		if key == "" {
			continue
		}
		if until, cooling := p.until[key]; cooling && now.Before(until) {
			continue
		}
		if pool.MaxConcurrencyPerAccount > 0 && p.inFlight[key] >= pool.MaxConcurrencyPerAccount {
			continue
		}
		usable = append(usable, a)
	}
	if len(usable) == 0 {
		return nil
	}
	chosen := usable[0]
	switch pool.Strategy {
	case "round-robin":
		if p.cursor >= len(usable) {
			p.cursor = 0
		}
		chosen = usable[p.cursor]
		p.cursor = (p.cursor + 1) % len(usable)
	default: // sticky
		for _, a := range usable {
			if strings.TrimSpace(a.RuntimeKey) == p.last {
				chosen = a
				break
			}
		}
	}
	key := strings.TrimSpace(chosen.RuntimeKey)
	p.inFlight[key]++
	p.last = key
	return &lease{account: chosen}
}

// release returns a credential, applying a cooldown for a status that earned
// one. It is idempotent per lease: a request has several failure exits plus its
// completion path, and releasing twice would free another request's slot.
func (p *poolState) release(l *lease, status int, now time.Time) {
	if l == nil || l.account.RuntimeKey == "" {
		return
	}
	key := strings.TrimSpace(l.account.RuntimeKey)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inFlight[key] > 0 {
		p.inFlight[key]--
	}
	if d := cooldownFor(status); d > 0 {
		p.until[key] = now.Add(d)
	}
}

// markCooling puts a credential aside for d without a request having been made —
// used when a catalog read proves the key is refused.
func (p *poolState) markCooling(runtimeKey string, d time.Duration, now time.Time) {
	key := strings.TrimSpace(runtimeKey)
	if key == "" || d <= 0 {
		return
	}
	p.mu.Lock()
	p.until[key] = now.Add(d)
	p.mu.Unlock()
}
