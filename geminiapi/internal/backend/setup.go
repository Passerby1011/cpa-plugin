// Package backend: platform-neutral process primitives.
//
// Everything that differs per OS lives in process_windows.go and
// process_unix.go; this file holds the shared contract and the data-directory
// files the backend reads.
package backend

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func isWindows() bool { return osPathSeparator == '\\' }

// osPathSeparator is '\\' on Windows and '/' elsewhere; imported as a constant
// to avoid a runtime.GOOS check in a hot path.
const osPathSeparator = os.PathSeparator

// Setup is how to reach a configured backend: its origin, OpenAI base URL and
// the credentials its own files hold.
type Setup struct {
	// Origin is the scheme://host:port root the backend serves (no /v1).
	Origin string `json:"origin"`
	// BaseURL is Origin + "/v1", the OpenAI-compatible root.
	BaseURL string `json:"baseURL"`
	// APIKey authenticates OpenAI-compatible calls. It is the backend's key,
	// not a Google cookie, and it lives only in the data directory.
	APIKey string `json:"apiKey"`
	// AdminToken authenticates the backend's own /admin/api. Required for
	// account import and probe; never logged, never placed in a response.
	AdminToken string `json:"adminToken"`
	// Version is the version recorded when the setup was written.
	Version string `json:"version"`
}

// BackendConfig is the subset of the backend's config.json this plugin reads
// and writes. The backend owns the file, so unknown keys must survive a
// rewrite: ConfigFile keeps the raw document and only patches known fields.
type BackendConfig struct {
	Host           string `json:"host"`
	Port           int    `json:"port"`
	LogRequests    bool   `json:"log_requests"`
	MultiTurn      bool   `json:"multi_turn"`
	TemporaryChat  bool   `json:"temporary_chat"`
	extra          map[string]json.RawMessage
	Sourced        bool
	SourceFilePath string
}

// Secrets is the backend's secrets.json: its API key and admin token.
type Secrets struct {
	APIKey     string `json:"apiKey"`
	AdminToken string `json:"adminToken"`
}

// randomToken returns a URL-safe random token. 32 bytes of entropy is what the
// backend's own setup generates for adminToken; the API key gets the same
// treatment because it is a bearer credential.
func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// Paths is the file layout of one data directory.
type Paths struct {
	DataDir    string
	Binary     string
	ConfigFile string
	Secrets    string
	Database   string
	PIDFile    string
	LogFile    string
	Accounts   string
}

// PathsFor resolves the layout of a data directory.
func PathsFor(dataDir string) Paths {
	return Paths{
		DataDir:    dataDir,
		Binary:     installedPath(dataDir),
		ConfigFile: filepath.Join(dataDir, ConfigFileName),
		Secrets:    filepath.Join(dataDir, SecretsFileName),
		Database:   filepath.Join(dataDir, DatabaseFileName),
		PIDFile:    filepath.Join(dataDir, PIDFileName),
		LogFile:    filepath.Join(dataDir, LogFileName),
		Accounts:   filepath.Join(dataDir, "accounts.jsonl"),
	}
}

// EnsureDataFiles creates config.json and secrets.json when missing and
// returns the resulting setup. Existing files are kept, except that a config
// bound to anything but 127.0.0.1 is refused: the backend carries Google
// cookies, so it must never listen on a routable address.
//
// preferredPort is used only when config.json does not exist yet. An existing
// port is authoritative — the operator (or a previous port-conflict resolution)
// chose it.
func EnsureDataFiles(dataDir string, preferredPort int) (Setup, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return Setup{}, fmt.Errorf("create data directory %s: %w", dataDir, err)
	}
	paths := PathsFor(dataDir)

	cfg, err := readOrCreateConfig(paths.ConfigFile, preferredPort)
	if err != nil {
		return Setup{}, err
	}
	if cfg.Host != "127.0.0.1" {
		return Setup{}, fmt.Errorf("%s is bound to host %q; this plugin requires 127.0.0.1 because the backend holds Google account cookies", paths.ConfigFile, cfg.Host)
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return Setup{}, fmt.Errorf("invalid port %d in %s", cfg.Port, paths.ConfigFile)
	}

	secrets, err := readOrCreateSecrets(paths.Secrets)
	if err != nil {
		return Setup{}, err
	}
	origin := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	return Setup{
		Origin:     origin,
		BaseURL:    origin + "/v1",
		APIKey:     secrets.APIKey,
		AdminToken: secrets.AdminToken,
		Version:    ReleaseVersion,
	}, nil
}

// ReadSetup returns the saved setup without creating anything. ok is false when
// config.json or secrets.json is missing or malformed, which is the normal
// state before a first install.
func ReadSetup(dataDir string) (Setup, bool) {
	paths := PathsFor(dataDir)
	cfgRaw, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return Setup{}, false
	}
	var cfg BackendConfig
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
		return Setup{}, false
	}
	if cfg.Host != "127.0.0.1" || cfg.Port <= 0 || cfg.Port > 65535 {
		return Setup{}, false
	}
	secRaw, err := os.ReadFile(paths.Secrets)
	if err != nil {
		return Setup{}, false
	}
	var secrets Secrets
	if err := json.Unmarshal(secRaw, &secrets); err != nil {
		return Setup{}, false
	}
	if strings.TrimSpace(secrets.APIKey) == "" || strings.TrimSpace(secrets.AdminToken) == "" {
		return Setup{}, false
	}
	origin := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	return Setup{
		Origin:     origin,
		BaseURL:    origin + "/v1",
		APIKey:     secrets.APIKey,
		AdminToken: secrets.AdminToken,
		Version:    ReleaseVersion,
	}, true
}

// SetPort rewrites config.json's port, preserving every other key the backend
// wrote. It is how a port-conflict resolution is persisted.
func SetPort(dataDir string, port int) error {
	paths := PathsFor(dataDir)
	cfg, err := readOrCreateConfig(paths.ConfigFile, port)
	if err != nil {
		return err
	}
	cfg.Port = port
	cfg.Host = "127.0.0.1"
	return writeConfig(paths.ConfigFile, cfg)
}

// readOrCreateConfig reads config.json, creating a loopback-only default when
// it does not exist.
func readOrCreateConfig(path string, preferredPort int) (BackendConfig, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		var cfg BackendConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return BackendConfig{}, fmt.Errorf("parse %s: %w", path, err)
		}
		if err := json.Unmarshal(raw, &map[string]json.RawMessage{}); err != nil {
			return BackendConfig{}, fmt.Errorf("parse %s: %w", path, err)
		}
		var extra map[string]json.RawMessage
		if uerr := json.Unmarshal(raw, &extra); uerr == nil {
			delete(extra, "host")
			delete(extra, "port")
			delete(extra, "log_requests")
			delete(extra, "multi_turn")
			delete(extra, "temporary_chat")
			cfg.extra = extra
		}
		cfg.Sourced = true
		cfg.SourceFilePath = path
		// A missing host in an old file means the backend's own default.
		if cfg.Host == "" {
			cfg.Host = "127.0.0.1"
		}
		return cfg, nil
	}
	if !os.IsNotExist(err) {
		return BackendConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	port := preferredPort
	if port <= 0 {
		port = DefaultPortValue
	}
	cfg := BackendConfig{
		Host:          "127.0.0.1",
		Port:          port,
		LogRequests:   false,
		MultiTurn:     false,
		TemporaryChat: true,
	}
	if err := writeConfig(path, cfg); err != nil {
		return BackendConfig{}, err
	}
	return cfg, nil
}

// writeConfig writes config.json, merging the preserved unknown keys back in.
func writeConfig(path string, cfg BackendConfig) error {
	doc := map[string]any{
		"host":           cfg.Host,
		"port":           cfg.Port,
		"log_requests":   cfg.LogRequests,
		"multi_turn":     cfg.MultiTurn,
		"temporary_chat": cfg.TemporaryChat,
	}
	for key, value := range cfg.extra {
		doc[key] = value
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

// readOrCreateSecrets creates secrets.json when missing. Both values are
// generated, never derived from the operator's environment, so two
// installations cannot share a bearer credential.
func readOrCreateSecrets(path string) (Secrets, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		var s Secrets
		if err := json.Unmarshal(raw, &s); err != nil {
			return Secrets{}, fmt.Errorf("parse %s: %w", path, err)
		}
		if strings.TrimSpace(s.APIKey) == "" || strings.TrimSpace(s.AdminToken) == "" {
			return Secrets{}, fmt.Errorf("%s is missing apiKey or adminToken", path)
		}
		return s, nil
	}
	if !os.IsNotExist(err) {
		return Secrets{}, fmt.Errorf("read %s: %w", path, err)
	}
	apiKey, err := randomToken()
	if err != nil {
		return Secrets{}, err
	}
	adminToken, err := randomToken()
	if err != nil {
		return Secrets{}, err
	}
	s := Secrets{APIKey: apiKey, AdminToken: adminToken}
	raw, err = json.MarshalIndent(s, "", "  ")
	if err != nil {
		return Secrets{}, fmt.Errorf("encode %s: %w", path, err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return Secrets{}, fmt.Errorf("write %s: %w", path, err)
	}
	return s, nil
}
