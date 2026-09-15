package main

import (
	"sync"
	"testing"
	"time"
)

func TestScheduledActionsAtKeepaliveOnlyHour(t *testing.T) {
	runCheckin, runActivity, runTravel, runGrowthTasks, runKeepalive := scheduledActionsFor(time.Date(2026, 8, 18, 22, 0, 0, 0, time.Local))
	if runCheckin {
		t.Fatal("22:00 keepalive tick must not run auto check-in")
	}
	if runActivity {
		t.Fatal("22:00 keepalive tick must not run the activity report")
	}
	if runTravel {
		t.Fatal("22:00 keepalive tick must not run the travel loop")
	}
	if runGrowthTasks {
		t.Fatal("22:00 keepalive tick must not run the growth task centre")
	}
	if !runKeepalive {
		t.Fatal("22:00 should run keepalive")
	}
}

func TestScheduledActionsAtCheckinHour(t *testing.T) {
	runCheckin, runActivity, runTravel, runGrowthTasks, runKeepalive := scheduledActionsFor(time.Date(2026, 8, 18, 21, 0, 0, 0, time.Local))
	if !runCheckin {
		t.Fatal("21:00 should run auto check-in")
	}
	if runActivity {
		t.Fatal("21:00 should not run the activity report")
	}
	if !runTravel {
		t.Fatal("21:00 should run the travel loop (collect a trip started at 09:00)")
	}
	if runGrowthTasks {
		t.Fatal("21:00 should not run the growth task centre")
	}
	if runKeepalive {
		t.Fatal("21:00 should not run keepalive")
	}
}

func TestScheduledActionsAtActivityHour(t *testing.T) {
	runCheckin, runActivity, runTravel, runGrowthTasks, runKeepalive := scheduledActionsFor(time.Date(2026, 8, 18, 10, 0, 0, 0, time.Local))
	if runCheckin {
		t.Fatal("10:00 should not run auto check-in")
	}
	if !runActivity {
		t.Fatal("10:00 should run the activity report")
	}
	if runTravel {
		t.Fatal("10:00 should not run the travel loop")
	}
	if !runGrowthTasks {
		t.Fatal("10:00 should run the growth task centre")
	}
	if runKeepalive {
		t.Fatal("10:00 should not run keepalive")
	}
}

// 01:00 exists for the night-owl task, which only counts events reported
// between 23:00 and 08:00 CST. Nothing else is scheduled at that hour.
func TestScheduledActionsAtGrowthTasksNightHour(t *testing.T) {
	runCheckin, runActivity, runTravel, runGrowthTasks, runKeepalive := scheduledActionsFor(time.Date(2026, 8, 18, 1, 0, 0, 0, time.Local))
	if !runGrowthTasks {
		t.Fatal("01:00 should run the growth task centre (night-owl window)")
	}
	if runCheckin || runActivity || runTravel || runKeepalive {
		t.Fatalf("01:00 should only run the growth task centre, got checkin=%v activity=%v travel=%v keepalive=%v",
			runCheckin, runActivity, runTravel, runKeepalive)
	}
}

// The wake-up timer must account for every configured slot, otherwise a tick
// scheduled for the activity or travel hour is simply never reached.
func TestNextCheckinTimeCoversEverySlot(t *testing.T) {
	// 09:30 → the next slot is 10:00 (activity).
	got := nextCheckinTime(time.Date(2026, 8, 18, 9, 30, 0, 0, time.Local))
	want := time.Date(2026, 8, 18, 10, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("nextCheckinTime(09:30) = %v, want %v", got, want)
	}
	// 22:30 → today's slots have passed; the next one is 01:00 tomorrow, the
	// night-owl slot of the growth task centre.
	got = nextCheckinTime(time.Date(2026, 8, 18, 22, 30, 0, 0, time.Local))
	want = time.Date(2026, 8, 19, 1, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("nextCheckinTime(22:30) = %v, want %v", got, want)
	}
}

func TestWithCheckinLockSerializesByAuthIndex(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		withCheckinLock("auth-1", func() {
			close(entered)
			<-release
		})
	}()
	<-entered
	locked := make(chan struct{})
	go func() {
		withCheckinLock("auth-1", func() { close(locked) })
	}()
	select {
	case <-locked:
		t.Fatal("second lock holder entered before first released")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("second lock holder did not enter after release")
	}
}
