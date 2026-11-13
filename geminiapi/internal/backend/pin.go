// Package backend manages the local OpenAI-compatible backends this plugin
// owns: pinned version + sha256, download, verification, spawn, health probe,
// crash restart, and teardown of the whole process group.
//
// # Two backends, two supervisors
//
// This plugin hosts TWO upstream backends that are not interchangeable:
// gemini-web2api-go (the Gemini Web browser backend) and AIStudio2API (the
// AI Studio backend). Each is described by a Flavor (flavor.go): its own pin
// table, release tag, argv, config shape, admin surface and account import
// path. A Manager is bound to exactly ONE Flavor, so the two namespaces get two
// Managers with separate data directories, ports, pid files and process groups.
//
// # Security boundary
//
// The plugin never executes a path it has not verified. Every launch is
// preceded by a sha256 check against a compile-time pin (and, for an archive
// asset, a second check on the extracted executable), and every kill is
// preceded by a PID **identity** check (PID alone is not an identity: a live
// PID may belong to an unrelated process after a crash or a reboot).
//
// # Why the binaries are not shipped
//
// Each backend is tens of megabytes per platform and the plugin is a single .so
// per platform. Shipping every platform's binary inside every .so would
// multiply the artifact size for no benefit, and bundling cookies or account
// state is out of the question. Everything here therefore runs at first use,
// into a data directory the operator can point anywhere with `data-dir`.
package backend

import (
	"fmt"
	"runtime"
	"strings"
)

// The gemini-web2api-go release this plugin is pinned to. These constants are
// the ONLY source of truth for what may be downloaded and executed: a manifest
// served over the network is never trusted, so a compromised release page
// cannot redirect the plugin at an unverified binary. The AI Studio flavor
// keeps its own copy of the same idea in flavors.go.
//
// Source: BUKOWSKIREAL/magpie-gemini-web lib/backend-release.json (commit
// 4d9c20f), tag backend-v4.28.0-magpie.1.
const (
	ReleaseVersion   = "4.28.0-magpie.1"
	ReleaseTag       = "backend-v4.28.0-magpie.1"
	ReleaseRepo      = "BUKOWSKIREAL/magpie-gemini-web"
	BinaryName       = "gemini-web2api-go"
	UpstreamVersion  = "4.28.0"
	ConfigFileName   = "config.json"
	SecretsFileName  = "secrets.json"
	DatabaseFileName = "gemini.db"
	PIDFileName      = "backend.pid"
	LogFileName      = "backend.log"
)

// Pin is one platform's asset and digest.
type Pin struct {
	// Asset is the release asset file name.
	Asset string
	// SHA256 is the lowercase hex digest of the exact asset bytes.
	SHA256 string
}

// Flavors is the registry of every backend this plugin knows how to supervise.
// A FlavorID missing here is refused rather than guessed at.
var Flavors = map[FlavorID]Flavor{
	FlavorGeminiWeb: geminiWebFlavor,
	FlavorAIStudio:  aiStudioFlavor,
}

// FlavorFor returns the flavor with this id.
func FlavorFor(id FlavorID) (Flavor, error) {
	flavor, ok := Flavors[FlavorID(strings.TrimSpace(string(id)))]
	if !ok {
		return Flavor{}, fmt.Errorf("unknown backend flavor %q; this plugin serves %s and %s",
			id, FlavorAIStudio, FlavorGeminiWeb)
	}
	return flavor, nil
}

// Pins holds gemini-web2api-go's verified asset for every platform this plugin
// can run on. A platform missing here is refused rather than guessed at.
//
// It is an alias of the flavor's own table so a supervisor and a test can never
// disagree about the digest that gates a launch.
var Pins = geminiWebPins

// PinFor returns gemini-web2api-go's pin for a GOOS/GOARCH pair.
func PinFor(goos, goarch string) (Pin, error) { return geminiWebFlavor.Pin(goos, goarch) }

// CurrentPin is gemini-web2api-go's PinFor this process's platform.
func CurrentPin() (Pin, error) { return geminiWebFlavor.CurrentPin() }

// DownloadURL is the exact release URL for one gemini-web pin. It is derived
// from the pin table, never from a response body.
func DownloadURL(pin Pin) string { return geminiWebFlavor.ReleaseBaseURL() + pin.Asset }

// ReleasePageURL is gemini-web2api-go's human-facing release page, used in
// error text so an operator can see what was expected.
func ReleasePageURL() string { return geminiWebFlavor.ReleasePageURL() }

// platformLabel is a short human-readable platform name for error text.
func platformLabel() string { return runtime.GOOS + "/" + runtime.GOARCH }

// platformGOOS / platformGOARCH name this process's platform for pin lookups.
func platformGOOS() string   { return runtime.GOOS }
func platformGOARCH() string { return runtime.GOARCH }
