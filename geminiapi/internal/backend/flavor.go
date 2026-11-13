package backend

// Flavor: which backend binary this Manager supervises.
//
// # Why a flavor exists at all
//
// This plugin hosts TWO different OpenAI-compatible backends, and they are not
// interchangeable in any respect that matters:
//
//	           gemini-web (gemini-web2api-go)   ai-studio (AIStudio2API)
//	release    backend-v4.28.0-magpie.1        studio-v0.3.5-magpie.1
//	asset      a raw platform binary            an archive (.tar.gz / .zip)
//	argv       --config <json> --db <db>        -open-ui=false
//	config     config.json host/port + db       config.json v1 + .env
//	secrets    secrets.json apiKey/adminToken   config.json apiKey/admin creds
//	admin      /admin/api (bearer token)        /api (session cookie)
//	accounts   POST /admin/api/cookies          POST /api/accounts (browser login)
//	catalog    OpenAI shape: id only            Gemini shape: capabilities map
//
// Everything that differs is carried by one Flavor value, so a Manager is ALWAYS
// bound to exactly one backend. Two namespaces therefore get two Managers, two
// data directories, two ports, two pid files, two process groups — sharing one
// would mean one backend answering for the other namespace's models, which is
// the defect this split exists to remove.
//
// The zero value is deliberately INVALID: defaulting to a flavor would silently
// reproduce that defect.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// FlavorID is the stable identifier of a backend flavor. It equals the model
// namespace it serves, which is what makes "one namespace, one backend"
// checkable by a single comparison.
type FlavorID string

const (
	// FlavorGeminiWeb is gemini-web2api-go: the Gemini Web (browser) backend.
	FlavorGeminiWeb FlavorID = "gemini-web"
	// FlavorAIStudio is AIStudio2API: the AI Studio / Gemini API backend.
	FlavorAIStudio FlavorID = "ai-studio"
)

// Additional file names. The shared ones (config.json, backend.pid,
// backend.log) are declared in pin.go.
const (
	envFileName  = ".env"
	authDirName  = "auth"
	packageDir   = "package"
	binarySubDir = "bin"
)

// Flavor is one pinned backend build family plus how to operate it.
type Flavor struct {
	// ID is the flavor and namespace name.
	ID FlavorID
	// BinaryName is the executable's base name (no extension).
	BinaryName string
	// ReleaseVersion is the pinned release version (upstream's own string).
	ReleaseVersion string
	// ReleaseTag is the GitHub release tag the assets hang under.
	ReleaseTag string
	// ReleaseRepo is the owner/repo publishing those assets.
	ReleaseRepo string
	// Pins holds the verified asset (and digest) per platform.
	Pins map[string]Pin
	// Archive reports whether Pins name an ARCHIVE that must be unpacked
	// before launching. The digest then covers the archive, and
	// BinarySHA256 covers the executable inside it: both are checked, so a
	// tampered archive cannot smuggle a different binary past the gate.
	Archive bool
	// BinarySHA256 is the digest of the extracted executable, per platform.
	// Empty for a raw (non-archive) asset, whose Pins digest IS the binary's.
	BinarySHA256 map[string]string
	// DefaultPort is where a fresh install starts listening. The two
	// flavors pick different defaults so a first run of both does not
	// collide before any port has been recorded.
	DefaultPort int
}

// Label is the operator-facing name of the backend, used in error text.
func (f Flavor) Label() string {
	switch f.ID {
	case FlavorAIStudio:
		return "AIStudio2API"
	case FlavorGeminiWeb:
		return "gemini-web2api-go"
	default:
		return string(f.ID)
	}
}

// Valid reports whether this value identifies a real backend.
func (f Flavor) Valid() bool {
	return (f.ID == FlavorGeminiWeb || f.ID == FlavorAIStudio) && len(f.Pins) > 0 && f.BinaryName != ""
}

// ReleaseBaseURL is where this flavor's assets are downloaded from.
func (f Flavor) ReleaseBaseURL() string {
	return "https://github.com/" + f.ReleaseRepo + "/releases/download/" + f.ReleaseTag + "/"
}

// ReleasePageURL is the human-facing release page, used in error text.
func (f Flavor) ReleasePageURL() string {
	return "https://github.com/" + f.ReleaseRepo + "/releases/tag/" + f.ReleaseTag
}

// PlatformKey maps GOOS/GOARCH to this package's pin-table key.
func PlatformKey(goos, goarch string) string {
	return strings.ToLower(strings.TrimSpace(goos)) + "/" + strings.ToLower(strings.TrimSpace(goarch))
}

// Pin returns this flavor's asset and digest for a GOOS/GOARCH pair.
func (f Flavor) Pin(goos, goarch string) (Pin, error) {
	key := PlatformKey(goos, goarch)
	pin, ok := f.Pins[key]
	if !ok {
		return Pin{}, fmt.Errorf("no pinned %s %s build for %s/%s; this plugin supports linux, windows and darwin on amd64/arm64",
			f.Label(), f.ReleaseVersion, goos, goarch)
	}
	if len(pin.SHA256) != 64 {
		return Pin{}, fmt.Errorf("pin for %s/%s on %s is malformed: digest must be 64 hex chars", f.Label(), f.ReleaseVersion, key)
	}
	return pin, nil
}

// CurrentPin is Pin for this process's platform.
func (f Flavor) CurrentPin() (Pin, error) { return f.Pin(runtime.GOOS, runtime.GOARCH) }

// ExpectedBinaryDigest is the digest the extracted executable must hash to.
//
// For a raw binary asset the archive digest IS the binary digest; for an
// archive the release manifest publishes a separate "binarySha256", because
// unpacking a zip changes the hash of the bytes on disk.
func (f Flavor) ExpectedBinaryDigest(goos, goarch string) (string, error) {
	if !f.Archive {
		pin, err := f.Pin(goos, goarch)
		if err != nil {
			return "", err
		}
		return pin.SHA256, nil
	}
	key := PlatformKey(goos, goarch)
	digest, ok := f.BinarySHA256[key]
	if !ok || len(digest) != 64 {
		return "", fmt.Errorf("no pinned %s binary digest for %s; refusing to launch an unpinned executable", f.Label(), key)
	}
	return digest, nil
}

// Layout is the file layout of one data directory for one flavor.
type Layout struct {
	DataDir string
	// BinaryDir holds the installed executable.
	BinaryDir string
	Binary    string
	Config    string
	// Secrets is gemini-web's secrets.json (API key + admin token).
	Secrets string
	// EnvFile is AI Studio's .env (the settings its service actually reads).
	EnvFile string
	// Database is gemini-web's SQLite account store.
	Database string
	// AuthDir is AI Studio's auth-state directory.
	AuthDir string
	// ImportedAccounts records accounts the plugin imported, for a backend
	// whose pool lives in a browser profile. Counts only, never material.
	ImportedAccounts string
	PIDFile          string
	LogFile          string
}

// Layout resolves the layout of a data directory for this flavor.
//
// The two backends keep their state in genuinely different shapes: gemini-web
// reads a JSON config plus a SQLite database and takes its credentials from
// secrets.json, while AI Studio reads config.json plus a .env and keeps its
// Google auth state in a directory. Sharing one layout would mean handing each
// backend files it does not understand, so this switch is the whole point.
func (f Flavor) Layout(dataDir string) Layout {
	out := Layout{
		DataDir:          dataDir,
		PIDFile:          filepath.Join(dataDir, PIDFileName),
		LogFile:          filepath.Join(dataDir, LogFileName),
		Config:           filepath.Join(dataDir, ConfigFileName),
		ImportedAccounts: filepath.Join(dataDir, "accounts.jsonl"),
	}
	switch f.ID {
	case FlavorAIStudio:
		// AIStudio2API is shipped as a package directory and reads settings
		// from .env; its auth state lives in an auth/ tree.
		out.BinaryDir = filepath.Join(dataDir, packageDir)
		out.Binary = filepath.Join(out.BinaryDir, executableName(f.BinaryName))
		out.EnvFile = filepath.Join(dataDir, envFileName)
		out.AuthDir = filepath.Join(dataDir, authDirName)
	default:
		out.BinaryDir = filepath.Join(dataDir, binarySubDir)
		out.Binary = filepath.Join(out.BinaryDir, executableName(f.BinaryName))
		out.Secrets = filepath.Join(dataDir, SecretsFileName)
		out.Database = filepath.Join(dataDir, DatabaseFileName)
	}
	return out
}

// executableName appends the platform extension to a binary base name.
func executableName(base string) string {
	if isWindows() {
		return base + ".exe"
	}
	return base
}

// DataDirFor is where a namespace's data directory lives under the plugin's
// configured root. Each flavor gets its OWN subdirectory: two backends writing
// the same config.json and holding the same pid file would fight over both.
func DataDirFor(root string, id FlavorID) string {
	if strings.TrimSpace(root) == "" {
		return ""
	}
	return filepath.Join(root, string(id))
}

// EnsureDataDir creates a flavor's data directory with owner-only permissions.
func EnsureDataDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("data directory is not configured")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data directory %s: %w", dir, err)
	}
	return nil
}
