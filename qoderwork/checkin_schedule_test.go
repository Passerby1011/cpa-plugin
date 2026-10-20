// checkin_schedule_test.go pins the auto check-in schedule to the shape of
// Qoder CN's daily claim window.
//
// Upstream opens exactly one window per day at 10:00 (UTC+8) and keeps it open
// until 10:00 the next day; a missed window cannot be back-filled. The plugin
// used to fire its first slot at 09:00 — an hour before the new round existed —
// so that tick always reported the previous day's state and the actual claim
// only ever happened on the 21:00 tick. These tests fail if the first slot
// drifts back in front of the window.
package main

import (
	"testing"
	"time"
)

func localTime(hour, min int) time.Time {
	return time.Date(2026, 10, 20, hour, min, 0, 0, time.Local)
}

func TestFirstCheckinSlotFollowsTheClaimWindowOpen(t *testing.T) {
	if len(checkinHours) == 0 {
		t.Fatal("no check-in hours configured")
	}
	first := checkinHours[0]
	slot := checkinSlotAt(localTime(0, 0), first)
	windowOpen := time.Date(2026, 10, 20, 10, 0, 0, 0, time.Local)
	if slot.Before(windowOpen) {
		t.Fatalf("first check-in slot %s fires before the 10:00 claim window opens (%s)", slot.Format("15:04"), windowOpen.Format("15:04"))
	}
	// Still comfortably inside the window, not hours later.
	if slot.After(windowOpen.Add(30 * time.Minute)) {
		t.Fatalf("first check-in slot %s is more than 30m after the window opens", slot.Format("15:04"))
	}
}

func TestNextCheckinTimeSkipsTodaysPassedSlot(t *testing.T) {
	// 10:30 is past the 10:05 slot but before 21:00.
	next := nextCheckinTime(localTime(10, 30))
	want := time.Date(2026, 10, 20, 21, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Fatalf("nextCheckinTime(10:30) = %s, want %s", next.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestNextCheckinTimeRollsToTomorrowAfterAllSlots(t *testing.T) {
	// 23:00 is past every check-in slot and past the 22:00 keepalive.
	next := nextCheckinTime(localTime(23, 0))
	want := time.Date(2026, 10, 21, 10, 5, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Fatalf("nextCheckinTime(23:00) = %s, want %s", next.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestNextCheckinTimePrefersTheEarlierSlot(t *testing.T) {
	// 09:00 is before the first check-in slot and before the keepalive, so the
	// 10:05 claim must win over the 22:00 keepalive.
	next := nextCheckinTime(localTime(9, 0))
	want := time.Date(2026, 10, 20, 10, 5, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Fatalf("nextCheckinTime(09:00) = %s, want %s", next.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}
