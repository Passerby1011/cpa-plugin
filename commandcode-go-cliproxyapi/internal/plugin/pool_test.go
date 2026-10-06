package plugin

import (
	"strconv"
	"testing"
	"time"

	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/config"
)

// The tests below pin the two pool strategies at the state-machine level.
//
// Sticky is deliberately *process*-scoped: acquire() has no session handle, so
// the strongest property it can guarantee is "keep using the credential this
// process used last, until it becomes unusable". These tests assert exactly
// that and nothing stronger (no per-session affinity).

// poolAccounts builds n accounts with credentials acct-0..acct-(n-1).
func poolAccounts(n int) []config.Account {
	accounts := make([]config.Account, n)
	for i := range accounts {
		accounts[i] = config.Account{
			Label:      "acct",
			Credential: "acct-" + strconv.Itoa(i),
		}
	}
	return accounts
}

// acquireOnce grabs one account and releases it immediately, mimicking the
// success path in executor.go. It fails the test when the pool is exhausted.
func acquireOnce(t *testing.T, pool *poolState, accounts []config.Account, cfg config.Pool, now time.Time) string {
	t.Helper()
	account := pool.acquire(accounts, cfg, now)
	if account == nil {
		t.Fatalf("acquire at %s returned nil, want an account", now.Format(time.RFC3339Nano))
	}
	credential := account.Credential
	pool.release(credential, 0, now)
	return credential
}

// TestStickyReusesLastUsedAccount pins the core sticky property: with a single
// usable account, repeated acquires must keep returning that account. The
// advancement of `now` between calls matters — a last-used-ordering bug shows
// up here, because an LRU pick would start rotating as soon as the other
// account ages out of cooldown.
func TestStickyReusesLastUsedAccount(t *testing.T) {
	accounts := poolAccounts(1)
	pool := newPoolState()
	cfg := config.Pool{Strategy: config.PoolStrategySticky}
	now := time.Unix(0, 0)

	for i := 0; i < 5; i++ {
		got := acquireOnce(t, pool, accounts, cfg, now)
		if got != "acct-0" {
			t.Fatalf("acquire #%d = %q, want acct-0", i+1, got)
		}
		now = now.Add(500 * time.Millisecond)
	}
	if st := pool.state("acct-0"); st.inFlight != 0 {
		t.Fatalf("inFlight = %d after 5 acquire/release cycles, want 0", st.inFlight)
	}
}

// TestStickyReusesHealthiestAcrossSiblings is the two-account form of the same
// property: acct-1 was used last (and acct-0 is out of cooldown but idle), so
// sticky must hand back acct-1 rather than rotating to the coldest account.
func TestStickyReusesHealthiestAcrossSiblings(t *testing.T) {
	accounts := poolAccounts(2)
	pool := newPoolState()
	cfg := config.Pool{Strategy: config.PoolStrategySticky}

	// Warm acct-1 last so it is the most recently used credential.
	pool.state("acct-0").lastUsed = time.Unix(10, 0)
	pool.state("acct-1").lastUsed = time.Unix(20, 0)

	if got := acquireOnce(t, pool, accounts, cfg, time.Unix(30, 0)); got != "acct-1" {
		t.Fatalf("first acquire = %q, want acct-1 (the last-used account)", got)
	}
	// Still sticky once that request has finished and time has moved on.
	if got := acquireOnce(t, pool, accounts, cfg, time.Unix(31, 0)); got != "acct-1" {
		t.Fatalf("second acquire = %q, want acct-1 (sticky, not rotation)", got)
	}
}

// TestStickyFallsOverToHealthySiblingWhenCoolingDown pins that a cooled-down
// account is skipped and the other usable account takes over.
func TestStickyFallsOverToHealthySiblingWhenCoolingDown(t *testing.T) {
	accounts := poolAccounts(2)
	pool := newPoolState()
	cfg := config.Pool{Strategy: config.PoolStrategySticky}
	now := time.Unix(0, 0)

	first := acquireOnce(t, pool, accounts, cfg, now)
	pool.release(first, time.Minute, now) // simulated upstream failure

	now = now.Add(time.Second)
	if got := acquireOnce(t, pool, accounts, cfg, now); got == first {
		t.Fatalf("acquire = %q while that account is cooling down, want the sibling", got)
	}

	// Once the cooldown expires the sibling is still the most recently used,
	// so sticky stays on it rather than flapping back.
	now = now.Add(2 * time.Minute)
	if got := acquireOnce(t, pool, accounts, cfg, now); got == first {
		t.Fatalf("acquire = %q after cooldown expiry, want sticky to hold on the sibling", got)
	}
}

// TestStickySwitchesAtConcurrencyCap pins the second failover trigger: the
// preferred account is at max-concurrency-per-account and still in flight.
func TestStickySwitchesAtConcurrencyCap(t *testing.T) {
	accounts := poolAccounts(2)
	pool := newPoolState()
	cfg := config.Pool{Strategy: config.PoolStrategySticky, MaxConcurrencyPerAccount: 1}
	now := time.Unix(0, 0)

	first := pool.acquire(accounts, cfg, now)
	if first == nil {
		t.Fatal("first acquire = nil, want an account")
	}
	if got := pool.acquire(accounts, cfg, now); got == nil {
		t.Fatal("second acquire = nil, want the sibling account")
	} else if got.Credential == first.Credential {
		t.Fatalf("second acquire = %q, want a different account once the first is at the cap", got.Credential)
	}

	// Both are at the cap now, so the pool must report exhaustion.
	if got := pool.acquire(accounts, cfg, now); got != nil {
		t.Fatalf("third acquire = %q, want nil with both accounts at the cap", got.Credential)
	}

	// Freeing a slot makes that credential selectable again.
	pool.release(first.Credential, 0, now)
	if got := pool.acquire(accounts, cfg, now); got == nil || got.Credential != first.Credential {
		t.Fatalf("acquire after release = %v, want %q", got, first.Credential)
	}
}

// TestStickyPrefersNeverUsedThenMostRecent pins the boot path: with no history
// every account is equally "last used", so selection is by config order, and
// after one has served it keeps serving.
func TestStickyPrefersNeverUsedThenMostRecent(t *testing.T) {
	accounts := poolAccounts(3)
	pool := newPoolState()
	cfg := config.Pool{Strategy: config.PoolStrategySticky}
	now := time.Unix(0, 0)

	if got := acquireOnce(t, pool, accounts, cfg, now); got != "acct-0" {
		t.Fatalf("cold-start acquire = %q, want acct-0 (config order)", got)
	}
	for i := 0; i < 3; i++ {
		now = now.Add(time.Second)
		if got := acquireOnce(t, pool, accounts, cfg, now); got != "acct-0" {
			t.Fatalf("acquire #%d = %q, want acct-0 to keep serving", i+2, got)
		}
	}
}

// TestRoundRobinRotatesStrictly pins that the round-robin branch is untouched
// by the sticky fix: consecutive acquires walk the list and then wrap.
func TestRoundRobinRotatesStrictly(t *testing.T) {
	accounts := poolAccounts(3)
	pool := newPoolState()
	cfg := config.Pool{Strategy: config.PoolStrategyRoundRobin}
	now := time.Unix(0, 0)

	want := []string{"acct-0", "acct-1", "acct-2", "acct-0", "acct-1", "acct-2"}
	for i, expected := range want {
		got := acquireOnce(t, pool, accounts, cfg, now)
		if got != expected {
			t.Fatalf("acquire #%d = %q, want %q", i+1, got, expected)
		}
		now = now.Add(time.Second)
	}
}
