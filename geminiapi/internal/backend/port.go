package backend

import (
	"fmt"
	"net"
	"strconv"
	"time"
)

// Port selection.
//
// The backend binds 127.0.0.1 only. When the preferred port is taken the plugin
// walks upward to the next free one and PERSISTS the choice in the data
// directory's config.json, so a restart lands on the same port and any
// operator-facing URL stays valid.

const (
	// portScanWindow bounds the upward search. A machine with a few thousand
	// busy ports should fail loudly rather than scan forever.
	portScanWindow = 200
	// portProbeTimeout keeps a probe from hanging on an odd listener.
	portProbeTimeout = 400 * time.Millisecond
)

// portAnswers reports whether something accepts connections on host:port.
//
// Listening alone is not enough to decide a port is free: on some platforms
// another process can bind 127.0.0.1 to a port already bound on 0.0.0.0 and
// only one of them gets the traffic. A probe that actually connects is the
// only reliable signal, and it is what magpie-gemini-web does too.
var portAnswers = func(host string, port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), portProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// canListen reports whether a listener can be created on host:port.
var canListen = func(host string, port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// PickPort returns preferred when nothing uses it, otherwise the next port that
// is both unbound and unclaimed.
func PickPort(preferred int) (int, error) {
	host := "127.0.0.1"
	if preferred <= 0 {
		preferred = DefaultPortValue
	}
	if preferred > 65535 {
		return 0, fmt.Errorf("invalid preferred port %d", preferred)
	}
	for port := preferred; port <= preferred+portScanWindow && port <= 65535; port++ {
		if portAnswers(host, port) {
			continue
		}
		if !canListen(host, port) {
			continue
		}
		return port, nil
	}
	return 0, fmt.Errorf("no free port in %d-%d on %s; set ai-studio.port to a specific free port", preferred, preferred+portScanWindow, host)
}

// DefaultPortValue is the backend's own default, kept here so backend does not
// import config (config imports nothing from backend, keeping the dependency
// one-way).
const DefaultPortValue = 2048

// PortInUse reports whether something is already answering on the port. The
// manager uses it to distinguish "our backend is up" from "somebody else owns
// the port".
func PortInUse(port int) bool { return portAnswers("127.0.0.1", port) }
