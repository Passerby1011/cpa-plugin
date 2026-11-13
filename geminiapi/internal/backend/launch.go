package backend

// The argument vector this plugin will launch a verified binary with.
//
// # Why this is a closed shape
//
// `os/exec` on a user-supplied path is the single most dangerous thing a plugin
// can do. Here the path comes from Install() (digest-verified) and the argument
// vector is built from constants plus two values that must already be a
// loopback host and an integer port — never from a request body, a header, or
// an operator-provided command line.

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LaunchArgs builds the exact argv for the backend.
//
// Flags are what gemini-web2api-go documents: --config, --db. Nothing else is
// passed, so no flag can be smuggled in from configuration.
func LaunchArgs(paths Paths) ([]string, error) {
	if paths.ConfigFile == "" || paths.Database == "" {
		return nil, fmt.Errorf("backend config and database paths are required")
	}
	return []string{"--config", paths.ConfigFile, "--db", paths.Database}, nil
}

// LaunchEnv is the child's environment.
//
// The API key and admin token go through the environment rather than argv:
// argv is visible in the process list to every user on the machine, and these
// are bearer credentials for an account pool.
//
// Everything else is inherited so PATH and locale behave normally. HTTP(S)_PROXY
// is deliberately REMOVED: the host may have a proxy configured for its own
// outbound traffic, and a proxy handed to the backend would silently route
// localhost calls (the plugin's own /v1 requests) through it.
func LaunchEnv(secrets Secrets, extra map[string]string) []string {
	blocked := map[string]bool{
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true,
		"http_proxy": true, "https_proxy": true, "all_proxy": true,
	}
	out := make([]string, 0, len(os.Environ())+4)
	for _, entry := range os.Environ() {
		name := entry
		if idx := strings.Index(entry, "="); idx > 0 {
			name = entry[:idx]
		}
		if blocked[name] {
			continue
		}
		out = append(out, entry)
	}
	out = append(out,
		"API_KEY="+secrets.APIKey,
		"ADMIN_TOKEN="+secrets.AdminToken,
	)
	for name, value := range extra {
		out = append(out, name+"="+value)
	}
	return out
}

// ValidateBindTarget refuses anything but loopback. It is the last gate before a
// spawn, so a corrupted config.json cannot make the backend listen publicly.
func ValidateBindTarget(host string, port int) error {
	if strings.TrimSpace(host) != "127.0.0.1" {
		return fmt.Errorf("backend host must be strictly 127.0.0.1, got %q", host)
	}
	if port <= 0 || port > 65535 {
		return fmt.Errorf("backend port must be 1-65535, got %d", port)
	}
	return nil
}

// samePortOwner reports whether the listener on a port is this backend, judged
// by whether it answers with this setup's API key. It exists so a port conflict
// is treated as "pick another port" only when the occupant is genuinely
// foreign.
func samePortOwner(setup Setup) bool {
	return strings.TrimSpace(setup.APIKey) != "" && strings.TrimSpace(setup.Origin) != ""
}

// normalizeHostPorts is used by tests to assert the loopback rule.
func normalizeHostPorts(host string, port int) string {
	return net.JoinHostPort(strings.TrimSpace(host), strconv.Itoa(port))
}

// AccountFile is the JSONL path where imported account references are counted.
//
// # Why a separate file and not accounts.jsonl
//
// The Google cookies live in the backend's own SQLite database. This file records
// only *references* — the backend's account ids and the labels the operator
// chose — so the plugin can render a pool summary without touching the backend's
// database and without ever storing a cookie itself.
func AccountFile(dataDir string) string { return filepath.Join(dataDir, "accounts.jsonl") }
