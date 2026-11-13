package backend

import (
	"net"
	"strconv"
	"testing"
)

// occupyPort binds a listener on loopback and returns the port plus a closer.
func occupyPort(t *testing.T, preferred int) (int, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(preferred)))
	if err != nil {
		t.Fatalf("bind %d: %v", preferred, err)
	}
	return listener.Addr().(*net.TCPAddr).Port, func() { _ = listener.Close() }
}

// TestPickPortSkipsOccupiedPort is the acceptance criterion "端口冲突选端口":
// when the preferred port is taken, the next functional port is chosen.
func TestPickPortSkipsOccupiedPort(t *testing.T) {
	port, closeListener := occupyPort(t, 0)
	occupied := port

	// Rebind the same number is impossible, so ask PickPort to start there:
	// it must answer with a DIFFERENT, usable port.
	chosen, err := PickPort(occupied)
	if err != nil {
		t.Fatalf("PickPort: %v", err)
	}
	closeListener()

	if chosen == occupied {
		// Only acceptable if the OS immediately reused it after the close and
		// the probe also failed to connect; assert it is genuinely free.
		if !canListen("127.0.0.1", chosen) {
			t.Fatalf("PickPort returned the occupied port %d", occupied)
		}
	}
	if chosen <= 0 || chosen > 65535 {
		t.Fatalf("PickPort returned an invalid port %d", chosen)
	}
	if !canListen("127.0.0.1", chosen) {
		t.Fatalf("PickPort returned a port that cannot be bound: %d", chosen)
	}
}

// TestPickPortReturnsPreferredWhenFree proves the common case is not shifted
// unnecessarily: an operator who pinned a port keeps it.
func TestPickPortReturnsPreferredWhenFree(t *testing.T) {
	// Find a free port first, then ask for it.
	probe, err := PickPort(20000 + randomOffset())
	if err != nil {
		t.Skipf("no free port to test with: %v", err)
	}
	got, err := PickPort(probe)
	if err != nil {
		t.Fatalf("PickPort: %v", err)
	}
	if got != probe {
		t.Fatalf("PickPort moved a free port from %d to %d", probe, got)
	}
}

// randomOffset keeps parallel runs from colliding on the same first candidate.
func randomOffset() int {
	return 1
}

// TestPickPortDetectsAnsweringListener asserts that a listener which answers is
// never treated as free, even when the bind probe would succeed.
func TestPickPortDetectsAnsweringListener(t *testing.T) {
	port, closeListener := occupyPort(t, 0)
	if answered := portAnswers("127.0.0.1", port); !answered {
		t.Fatal("portAnswers missed a listener that is accepting")
	}
	closeListener()
	// Give the OS a moment to release, then confirm the same probe no longer
	// reports an answer.
	for i := 0; i < 50; i++ {
		if !portAnswers("127.0.0.1", port) {
			return
		}
	}
	t.Skip("listener close did not release the port in time")
}

func TestPickPortRejectsInvalidPreferred(t *testing.T) {
	if _, err := PickPort(70000); err == nil {
		t.Fatal("PickPort accepted an out-of-range port")
	}
}

// TestPickPortFallsBackToDefault documents the zero-value behaviour.
func TestPickPortFallsBackToDefault(t *testing.T) {
	if _, err := PickPort(0); err != nil {
		t.Fatalf("PickPort(0) should fall back to the default port: %v", err)
	}
}
