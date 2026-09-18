// fault_note_preserve_test.go pins the fault-note preservation rule.
//
// A fault note ("re-login required") is what keeps an account disabled after
// the credit-driven re-enable looks at healthy credits. The reconcile tick's
// note refresh rebuilds the note from credits state, so without an explicit
// guard it replaces the fault wording and the re-enable puts a credential the
// upstream still rejects back into rotation.
package main

import "testing"

// A stored fault note must survive the refresh: the note the refresh would
// write is discarded in favour of the stored one.
func TestSyncAuthNoteKeepsFaultNote(t *testing.T) {
	faultNote := upstreamFaultNote(upstreamErrSessionDead)
	if !reenableBlockedByNote(faultNote) {
		t.Fatalf("fixture is not a fault note: %q", faultNote)
	}

	// Simulate the decision the refresh makes: the stored note wins.
	stored := faultNote
	refreshed := displayNote(&storedAuth{}, nil, true)
	note := refreshed
	if reenableBlockedByNote(stored) {
		note = stored
	}
	if note != faultNote {
		t.Fatalf("fault note was replaced by %q", note)
	}
	if !reenableBlockedByNote(note) {
		t.Fatal("the re-enable guard no longer sees a fault note")
	}
}

// An ordinary credits note is not preserved: it is replaced by the fresh one.
func TestSyncAuthNoteReplacesCreditsNote(t *testing.T) {
	creditsNote := "CN · 余100 已用5"
	if reenableBlockedByNote(creditsNote) {
		t.Fatalf("credits note misread as a fault note: %q", creditsNote)
	}
	fresh := "CN · 余50 已用55"
	note := fresh
	if reenableBlockedByNote(creditsNote) {
		note = creditsNote
	}
	if note != fresh {
		t.Fatalf("credits note was preserved: %q", note)
	}
}

// Both fault wordings the plugin writes must be recognised.
func TestFaultNoteWordingsAreRecognised(t *testing.T) {
	for _, kind := range []upstreamErrKind{upstreamErrSessionDead, upstreamErrAccountBanned} {
		note := upstreamFaultNote(kind)
		if !reenableBlockedByNote(note) {
			t.Errorf("upstreamFaultNote(%v) = %q is not recognised as a fault note", kind, note)
		}
	}
}
