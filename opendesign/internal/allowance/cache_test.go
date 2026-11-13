package allowance

import (
	"context"
	"testing"
	"time"
)

// The workspace is read once and reused for the TTL: a refresh that re-read it
// would cost an extra round-trip per account on every card refresh.
func TestWorkspaceIsCachedForTenMinutes(t *testing.T) {
	fake := newFakeControl().on("/api/v1/workspaces/current", 200,
		`{"activeWorkspaceId":"ws1","personalMembershipTier":"go"}`)
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	cache := NewWorkspaceCache(10*time.Minute, func() time.Time { return now })

	first, err := cache.Get(context.Background(), fake.transport(), "https://amr-api.example", "vela_ctrl_a")
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if first.ID != "ws1" {
		t.Errorf("workspace = %+v", first)
	}
	if n := fake.calledWith("/workspaces/current"); n != 1 {
		t.Fatalf("calls after the first read = %d, want 1", n)
	}

	// Just inside the TTL: no network.
	now = now.Add(9 * time.Minute)
	if _, err := cache.Get(context.Background(), fake.transport(), "https://amr-api.example", "vela_ctrl_a"); err != nil {
		t.Fatalf("cached read: %v", err)
	}
	if n := fake.calledWith("/workspaces/current"); n != 1 {
		t.Errorf("calls after a read inside the TTL = %d, want still 1", n)
	}

	// Past the TTL: one more read.
	now = now.Add(2 * time.Minute)
	if _, err := cache.Get(context.Background(), fake.transport(), "https://amr-api.example", "vela_ctrl_a"); err != nil {
		t.Fatalf("expired read: %v", err)
	}
	if n := fake.calledWith("/workspaces/current"); n != 2 {
		t.Errorf("calls after the TTL expired = %d, want 2", n)
	}
	if cache.Len() != 1 {
		t.Errorf("cache holds %d entries, want 1", cache.Len())
	}
}

// A failed read must not be cached: one hiccup would otherwise keep an account
// unreadable for the whole TTL, showing an error the operator cannot clear.
func TestAFailedWorkspaceReadIsNotCached(t *testing.T) {
	fake := newFakeControl().on("/api/v1/workspaces/current", 503, `{"error":"unavailable"}`)
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	cache := NewWorkspaceCache(10*time.Minute, func() time.Time { return now })

	if _, err := cache.Get(context.Background(), fake.transport(), "https://amr-api.example", "vela_ctrl_a"); err == nil {
		t.Fatal("a 503 workspace read must fail")
	}
	if cache.Len() != 0 {
		t.Errorf("a failed read was cached (%d entries)", cache.Len())
	}

	// The plane recovers; the next call must reach it, not answer from a cache.
	fake.on("/api/v1/workspaces/current", 200, `{"activeWorkspaceId":"ws1"}`)
	if _, err := cache.Get(context.Background(), fake.transport(), "https://amr-api.example", "vela_ctrl_a"); err != nil {
		t.Fatalf("read after recovery: %v", err)
	}
	if n := fake.calledWith("/workspaces/current"); n != 2 {
		t.Errorf("calls = %d, want the recovery read to have gone out", n)
	}
}

// The cache is keyed by the key's digest, so two accounts never share a
// workspace and the map holds no key material.
func TestWorkspaceCacheIsKeyedByDigest(t *testing.T) {
	fake := newFakeControl()
	fake.on("/api/v1/workspaces/current", 200, `{"activeWorkspaceId":"ws1"}`)
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	cache := NewWorkspaceCache(10*time.Minute, func() time.Time { return now })

	for _, key := range []string{"vela_ctrl_a", "vela_ctrl_b"} {
		if _, err := cache.Get(context.Background(), fake.transport(), "https://amr-api.example", key); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
	}
	if cache.Len() != 2 {
		t.Errorf("two accounts share a cache entry: %d", cache.Len())
	}
	cache.mu.Lock()
	for stored := range cache.entries {
		if stored == "vela_ctrl_a" || stored == "vela_ctrl_b" {
			t.Errorf("the cache stores a key verbatim")
		}
	}
	cache.mu.Unlock()

	// Forget drops exactly one account's entry.
	cache.Forget("vela_ctrl_a")
	if cache.Len() != 1 {
		t.Errorf("after Forget the cache holds %d entries, want 1", cache.Len())
	}
}

// An account with no control key is refused before any request is built: there
// is nothing to authorize with.
func TestWorkspaceReadWithoutAControlKeyMakesNoRequest(t *testing.T) {
	fake := newFakeControl().on("/api/v1/workspaces/current", 200, `{"activeWorkspaceId":"ws1"}`)
	if _, err := WorkspaceOf(context.Background(), fake.transport(), "https://amr-api.example", ""); err == nil {
		t.Fatal("a read with no control key must fail")
	}
	if fake.count() != 0 {
		t.Errorf("calls = %d, want none", fake.count())
	}
}
