package backend

// The data-directory contract each backend has, and the argv/env this plugin
// will launch a verified binary with.
//
// # Why this is a closed shape
//
// `os/exec` on a user-supplied path is the single most dangerous thing a plugin
// can do. Here the path comes from Install() (digest-verified) and the argument
// vector is built from constants plus values that must already be a loopback
// host and an integer port — never from a request body, a header, or an
// operator-provided command line.

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
	// AdminToken authenticates gemini-web's /admin/api. AI Studio uses a
	// session cookie instead and leaves this empty.
	AdminToken string `json:"adminToken"`
	// AdminUser / AdminPassword authenticate AI Studio's /api: it has no
	// bearer token, it has a login form. Both live in its config.json.
	AdminUser     string `json:"adminUser,omitempty"`
	AdminPassword string `json:"adminPassword,omitempty"`
	// Flavor names the backend this setup reaches. A setup with an empty
	// flavor is not usable: it would let one namespace's request travel
	// through the other backend.
	Flavor FlavorID `json:"flavor"`
	// Version is the version recorded when the setup was written.
	Version string `json:"version"`
}

// Port returns the setup's port.
func (s Setup) Port() int { return portFromOrigin(s.Origin) }

// Secrets is gemini-web2api-go's secrets.json: its API key and admin token.
type Secrets struct {
	APIKey     string `json:"apiKey"`
	AdminToken string `json:"adminToken"`
}

// StudioConfig is AIStudio2API's config.json. It is version-tagged by upstream
// ("version": 1) and carries both the OpenAI key and the admin credentials the
// service's login form checks.
type StudioConfig struct {
	Version       int    `json:"version"`
	Port          int    `json:"port"`
	APIKey        string `json:"apiKey"`
	AdminUsername string `json:"adminUsername"`
	AdminPassword string `json:"adminPassword"`
}

// randomToken returns a URL-safe random token. 32 bytes of entropy is what
// gemini-web's own setup generates for its admin token; the API key gets the
// same treatment because it is a bearer credential.
func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// studioAPIKey returns a key shaped like the one AIStudio2API generates
// itself ("as_" + 32 random bytes), so the value is recognisable in a log line
// and cannot be confused with gemini-web's key.
func studioAPIKey() (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	return "as_" + token, nil
}

// --- gemini-web: config.json + secrets.json + gemini.db ---

// EnsureGeminiWebFiles creates gemini-web2api-go's config.json and secrets.json
// when missing and returns the resulting setup. Existing files are kept, except
// that a config bound to anything but 127.0.0.1 is refused: the backend carries
// Google cookies, so it must never listen on a routable address.
//
// preferredPort is used only when config.json does not exist yet. An existing
// port is authoritative — the operator (or a previous port-conflict
// resolution) chose it.
func EnsureGeminiWebFiles(dataDir string, preferredPort int) (Setup, error) {
	flavor := geminiWebFlavor
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return Setup{}, fmt.Errorf("create data directory %s: %w", dataDir, err)
	}
	layout := flavor.Layout(dataDir)

	cfg, err := readOrCreateGeminiWebConfig(layout.Config, preferredPort)
	if err != nil {
		return Setup{}, err
	}
	if cfg.Host != "127.0.0.1" {
		return Setup{}, fmt.Errorf("%s is bound to host %q; this plugin requires 127.0.0.1 because the backend holds Google account cookies", layout.Config, cfg.Host)
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return Setup{}, fmt.Errorf("invalid port %d in %s", cfg.Port, layout.Config)
	}
	secrets, err := geminiWebSecrets(layout.Secrets)
	if err != nil {
		return Setup{}, err
	}
	return geminiWebSetup(cfg.Port, secrets), nil
}

// ReadGeminiWebSetup returns gemini-web's saved setup without creating anything.
// ok is false when config.json or secrets.json is missing or malformed, which
// is the normal state before a first install.
func ReadGeminiWebSetup(dataDir string) (Setup, bool) {
	layout := geminiWebFlavor.Layout(dataDir)
	raw, err := os.ReadFile(layout.Config)
	if err != nil {
		return Setup{}, false
	}
	var cfg geminiWebConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Setup{}, false
	}
	if cfg.Host != "127.0.0.1" || cfg.Port <= 0 || cfg.Port > 65535 {
		return Setup{}, false
	}
	secRaw, err := os.ReadFile(layout.Secrets)
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
	return geminiWebSetup(cfg.Port, secrets), true
}

// geminiWebSetup assembles the setup for a port and a secrets pair.
func geminiWebSetup(port int, secrets Secrets) Setup {
	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	return Setup{
		Origin:     origin,
		BaseURL:    origin + "/v1",
		APIKey:     secrets.APIKey,
		AdminToken: secrets.AdminToken,
		Flavor:     FlavorGeminiWeb,
		Version:    geminiWebFlavor.ReleaseVersion,
	}
}

// SetGeminiWebPort rewrites gemini-web's config.json port, preserving every
// other key the backend wrote. It is how a port-conflict resolution is
// persisted.
func SetGeminiWebPort(dataDir string, port int) error {
	layout := geminiWebFlavor.Layout(dataDir)
	cfg, err := readOrCreateGeminiWebConfig(layout.Config, port)
	if err != nil {
		return err
	}
	cfg.Port = port
	cfg.Host = "127.0.0.1"
	return writeGeminiWebConfig(layout.Config, cfg)
}

// GeminiWebConfig is gemini-web2api-go's config.json: the subset this plugin
// reads and writes. The backend owns the file, so unknown keys must survive a
// rewrite — geminiWebConfig keeps the raw document and patches only known
// fields.
type geminiWebConfig struct {
	Host           string `json:"host"`
	Port           int    `json:"port"`
	LogRequests    bool   `json:"log_requests"`
	MultiTurn      bool   `json:"multi_turn"`
	TemporaryChat  bool   `json:"temporary_chat"`
	extra          map[string]json.RawMessage
	Sourced        bool
	SourceFilePath string
}

// readOrCreateGeminiWebConfig reads gemini-web's config.json, creating a
// loopback-only default when it does not exist.
func readOrCreateGeminiWebConfig(path string, preferredPort int) (geminiWebConfig, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		var cfg geminiWebConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return geminiWebConfig{}, fmt.Errorf("parse %s: %w", path, err)
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
		return geminiWebConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	port := preferredPort
	if port <= 0 {
		port = geminiWebFlavor.DefaultPort
	}
	cfg := geminiWebConfig{
		Host:          "127.0.0.1",
		Port:          port,
		LogRequests:   false,
		MultiTurn:     false,
		TemporaryChat: true,
	}
	if err := writeGeminiWebConfig(path, cfg); err != nil {
		return geminiWebConfig{}, err
	}
	return cfg, nil
}

// writeGeminiWebConfig writes gemini-web's config.json, merging the preserved
// unknown keys back in.
func writeGeminiWebConfig(path string, cfg geminiWebConfig) error {
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

// geminiWebSecrets creates secrets.json when missing. Both values are
// generated, never derived from the operator's environment, so two installations
// cannot share a bearer credential.
func geminiWebSecrets(path string) (Secrets, error) {
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

// --- ai-studio: config.json + .env ---

// EnsureStudioFiles creates AIStudio2API's config.json and .env when missing and
// returns the resulting setup.
//
// The two files exist because upstream reads them differently: config.json is the
// service's own versioned document (port, OpenAI key, admin credentials), while
// .env holds the settings its generation service actually starts with (listen
// address, auth-state directory, preview port, model routing). Writing only one
// of them leaves the service either unreachable or running with a port nobody
// recorded.
func EnsureStudioFiles(dataDir string, preferredPort int) (Setup, error) {
	flavor := aiStudioFlavor
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return Setup{}, fmt.Errorf("create data directory %s: %w", dataDir, err)
	}
	layout := flavor.Layout(dataDir)
	if err := os.MkdirAll(layout.AuthDir, 0o700); err != nil {
		return Setup{}, fmt.Errorf("create auth directory %s: %w", layout.AuthDir, err)
	}

	cfg, err := readOrCreateStudioConfig(layout.Config, preferredPort)
	if err != nil {
		return Setup{}, err
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return Setup{}, fmt.Errorf("invalid port %d in %s", cfg.Port, layout.Config)
	}
	if err := ensureStudioEnv(layout, cfg); err != nil {
		return Setup{}, err
	}
	return studioSetup(cfg), nil
}

// ReadStudioSetup returns AI Studio's saved setup without creating anything.
func ReadStudioSetup(dataDir string) (Setup, bool) {
	layout := aiStudioFlavor.Layout(dataDir)
	raw, err := os.ReadFile(layout.Config)
	if err != nil {
		return Setup{}, false
	}
	cfg, err := decodeStudioConfig(raw, layout.Config)
	if err != nil {
		return Setup{}, false
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return Setup{}, false
	}
	if strings.TrimSpace(cfg.APIKey) == "" || strings.TrimSpace(cfg.AdminUsername) == "" || strings.TrimSpace(cfg.AdminPassword) == "" {
		return Setup{}, false
	}
	return studioSetup(cfg), true
}

// SetStudioPort rewrites AI Studio's config.json and .env port so a
// port-conflict resolution survives both a service restart and a reload.
func SetStudioPort(dataDir string, port int) error {
	layout := aiStudioFlavor.Layout(dataDir)
	cfg, err := readOrCreateStudioConfig(layout.Config, port)
	if err != nil {
		return err
	}
	cfg.Port = port
	if err := writeStudioConfig(layout.Config, cfg); err != nil {
		return err
	}
	return ensureStudioEnv(layout, cfg)
}

// studioSetup assembles the setup for AI Studio's config.
func studioSetup(cfg StudioConfig) Setup {
	origin := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	return Setup{
		Origin:        origin,
		BaseURL:       origin + "/v1",
		APIKey:        cfg.APIKey,
		AdminUser:     cfg.AdminUsername,
		AdminPassword: cfg.AdminPassword,
		Flavor:        FlavorAIStudio,
		Version:       aiStudioFlavor.ReleaseVersion,
	}
}

// readOrCreateStudioConfig reads AI Studio's config.json, creating upstream's
// default document when it does not exist.
func readOrCreateStudioConfig(path string, preferredPort int) (StudioConfig, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		return decodeStudioConfig(raw, path)
	}
	if !os.IsNotExist(err) {
		return StudioConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	port := preferredPort
	if port <= 0 {
		port = aiStudioFlavor.DefaultPort
	}
	if port == geminiWebFlavor.DefaultPort {
		// Both backends default to 2048 upstream. Starting AI Studio there
		// while gemini-web already holds it would spawn a service that
		// cannot bind, so a fresh AI Studio install moves up one.
		port = aiStudioFlavor.DefaultPort
	}
	apiKey, err := studioAPIKey()
	if err != nil {
		return StudioConfig{}, err
	}
	password, err := randomToken()
	if err != nil {
		return StudioConfig{}, err
	}
	cfg := StudioConfig{
		Version:       1,
		Port:          port,
		APIKey:        apiKey,
		AdminUsername: "geminiapi",
		AdminPassword: password,
	}
	if err := writeStudioConfig(path, cfg); err != nil {
		return StudioConfig{}, err
	}
	return cfg, nil
}

// decodeStudioConfig parses AI Studio's config.json, filling the admin username
// when upstream left it out (upstream defaults it to "magpie").
func decodeStudioConfig(raw []byte, path string) (StudioConfig, error) {
	var cfg StudioConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return StudioConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Version == 0 {
		return StudioConfig{}, fmt.Errorf("%s is missing its version field; refusing to treat an unknown document as AI Studio config", path)
	}
	if cfg.AdminUsername == "" {
		cfg.AdminUsername = "magpie"
	}
	return cfg, nil
}

// writeStudioConfig persists AI Studio's config.json with owner-only
// permissions.
func writeStudioConfig(path string, cfg StudioConfig) error {
	if cfg.Version == 0 {
		cfg.Version = 1
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// ensureStudioEnv writes the .env the service's generation process reads.
//
// Only the keys this plugin owns are written; anything else the admin page has
// saved is preserved, because upstream treats .env as the operator's file.
//
// PREVIEW_LISTEN_ADDR is set explicitly and moved off the service port: upstream
// listens on BOTH addresses and exits at startup when either is taken, so
// leaving it to a guess is what turns a port conflict into "the service started
// and immediately died".
func ensureStudioEnv(layout Layout, cfg StudioConfig) error {
	values := readEnvFile(layout.EnvFile)

	values["LISTEN_ADDR"] = fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	values["AISTUDIO_AUTH_STATES"] = layout.AuthDir
	values["PROXY_API_KEY"] = cfg.APIKey
	values["ADMIN_AUTH_ENABLED"] = "true"
	values["ADMIN_USERNAME"] = cfg.AdminUsername
	values["ADMIN_PASSWORD"] = cfg.AdminPassword
	values["AUTO_START"] = "true"
	// The managed build runs upstream's own browser-free generation path.
	values["WAA_BACKEND"] = "spidermonkey"
	values["UPSTREAM_CHANNELS"] = "playground"
	values["BUILD_NATIVE_NONSTREAM"] = "false"

	// A preview address already pointing somewhere else is kept; otherwise it
	// is pinned one above the service port.
	if preview := listenPort(values["PREVIEW_LISTEN_ADDR"]); preview == 0 || preview == cfg.Port {
		next := cfg.Port + 1
		if next > 65535 {
			next = cfg.Port - 1
		}
		values["PREVIEW_LISTEN_ADDR"] = fmt.Sprintf("127.0.0.1:%d", next)
	}
	return writeEnvFile(layout.EnvFile, values)
}

// envKeyOrder is upstream's own ordering for the keys this plugin writes, so a
// hand-edited file keeps a familiar shape.
var envKeyOrder = []string{
	"AISTUDIO_AUTH_STATES", "LISTEN_ADDR", "PREVIEW_LISTEN_ADDR", "PROXY_API_KEY",
	"ADMIN_AUTH_ENABLED", "ADMIN_USERNAME", "ADMIN_PASSWORD", "AUTO_START",
	"WAA_BACKEND", "UPSTREAM_CHANNELS", "BUILD_NATIVE_NONSTREAM",
}

// readEnvFile parses a .env into values. Values are kept verbatim unless they
// are quoted; a line without '=' is ignored rather than treated as an error,
// because the admin page may write comments.
func readEnvFile(path string) map[string]string {
	out := map[string]string{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx <= 0 {
			continue
		}
		out[strings.TrimSpace(line[:idx])] = unquoteEnvValue(strings.TrimSpace(line[idx+1:]))
	}
	return out
}

// unquoteEnvValue strips one layer of matching single or double quotes.
func unquoteEnvValue(value string) string {
	if len(value) >= 2 {
		if value[0] == '\'' && value[len(value)-1] == '\'' {
			return value[1 : len(value)-1]
		}
		if value[0] == '"' && value[len(value)-1] == '"' {
			var decoded string
			if err := json.Unmarshal([]byte(value), &decoded); err == nil {
				return decoded
			}
			return value[1 : len(value)-1]
		}
	}
	return value
}

// writeEnvFile persists .env, writing the keys this plugin owns in upstream's
// order and then everything else that was there, with owner-only permissions.
func writeEnvFile(path string, values map[string]string) error {
	var builder strings.Builder
	written := map[string]bool{}
	for _, key := range envKeyOrder {
		if value, ok := values[key]; ok {
			builder.WriteString(key + "=" + quoteEnvValue(value) + "\n")
			written[key] = true
		}
	}
	// Preserve unrecognised keys the admin page may have added, in a stable
	// order so a rewrite does not churn the file.
	for key, value := range values {
		if written[key] {
			continue
		}
		builder.WriteString(key + "=" + quoteEnvValue(value) + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return os.Chmod(path, 0o600)
}

// quoteEnvValue quotes a value that would otherwise break the parser.
func quoteEnvValue(value string) string {
	if value == "" {
		return ""
	}
	if strings.ContainsAny(value, " \t\r\n#\"'") {
		return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
	}
	return value
}

// listenPort extracts the port of a "host:port" listen address, 0 when absent.
func listenPort(addr string) int {
	addr = strings.TrimSpace(addr)
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return 0
	}
	port := 0
	for _, r := range addr[idx+1:] {
		if r < '0' || r > '9' {
			return 0
		}
		port = port*10 + int(r-'0')
	}
	if port <= 0 || port > 65535 {
		return 0
	}
	return port
}

// --- launch ---

// EnsureDataFiles dispatches to the flavor's own file setup, so a caller holding
// a Flavor does not have to know which backend writes which files.
func (f Flavor) EnsureDataFiles(dataDir string, preferredPort int) (Setup, error) {
	switch f.ID {
	case FlavorAIStudio:
		return EnsureStudioFiles(dataDir, preferredPort)
	case FlavorGeminiWeb:
		return EnsureGeminiWebFiles(dataDir, preferredPort)
	default:
		return Setup{}, fmt.Errorf("unknown backend flavor %q", f.ID)
	}
}

// ReadSetup dispatches to the flavor's own reader.
func (f Flavor) ReadSetup(dataDir string) (Setup, bool) {
	switch f.ID {
	case FlavorAIStudio:
		return ReadStudioSetup(dataDir)
	case FlavorGeminiWeb:
		return ReadGeminiWebSetup(dataDir)
	default:
		return Setup{}, false
	}
}

// SetPort persists a port choice for this flavor.
func (f Flavor) SetPort(dataDir string, port int) error {
	switch f.ID {
	case FlavorAIStudio:
		return SetStudioPort(dataDir, port)
	case FlavorGeminiWeb:
		return SetGeminiWebPort(dataDir, port)
	default:
		return fmt.Errorf("unknown backend flavor %q", f.ID)
	}
}

// LaunchArgs builds the exact argv for this flavor's backend.
//
// The flags are what each upstream documents and nothing else is passed, so no
// flag can be smuggled in from configuration:
//
//   - gemini-web2api-go takes its config and database paths explicitly.
//   - AIStudio2API takes its settings from the .env beside it in the working
//     directory and reads config.json from there; the only switch this plugin
//     passes is to suppress its desktop UI, which has no meaning on a headless
//     gateway.
func (f Flavor) LaunchArgs(dataDir string) ([]string, error) {
	layout := f.Layout(dataDir)
	switch f.ID {
	case FlavorAIStudio:
		if layout.Config == "" || layout.EnvFile == "" {
			return nil, fmt.Errorf("AI Studio config and .env paths are required")
		}
		if _, err := os.Stat(layout.EnvFile); err != nil {
			return nil, fmt.Errorf("AI Studio .env is missing (%s): refusing to launch a service with no recorded port", layout.EnvFile)
		}
		return []string{"-open-ui=false"}, nil
	case FlavorGeminiWeb:
		if layout.Config == "" || layout.Database == "" {
			return nil, fmt.Errorf("backend config and database paths are required")
		}
		return []string{"--config", layout.Config, "--db", layout.Database}, nil
	default:
		return nil, fmt.Errorf("unknown backend flavor %q", f.ID)
	}
}

// ServiceEnv is the plugin-owned environment AIStudio2API is started with.
//
// # Why the service must be given these in its ENVIRONMENT
//
// Upstream reads them from the process environment FIRST and only falls back to
// .env. An inherited variable that is PRESENT BUT EMPTY is not "unset": the
// service logs `Invalid setting value, starting with the default` and then uses
// 2048/2050 — which is exactly what a real run showed (`.env` said one port, the
// service listened on the defaults, and the health probe hit the right port of
// nothing). So the values are taken from the SAME .env this function reads, and
// passed explicitly: one source of truth, delivered by the channel upstream
// actually honours.
//
// gemini-web takes no settings this way, so it gets an empty map.
func (f Flavor) ServiceEnv(setup Setup, dataDir string) map[string]string {
	if f.ID != FlavorAIStudio {
		return nil
	}
	layout := f.Layout(dataDir)
	values := readEnvFile(layout.EnvFile)
	out := make(map[string]string, len(values))
	for _, key := range []string{
		"LISTEN_ADDR", "PREVIEW_LISTEN_ADDR", "AISTUDIO_AUTH_STATES", "PROXY_API_KEY",
		"ADMIN_AUTH_ENABLED", "ADMIN_USERNAME", "ADMIN_PASSWORD", "AUTO_START",
		"WAA_BACKEND", "UPSTREAM_CHANNELS", "BUILD_NATIVE_NONSTREAM",
	} {
		if value, ok := values[key]; ok && strings.TrimSpace(value) != "" {
			out[key] = value
		}
	}
	// The .env can be hand-edited; the port the plugin resolved is authoritative
	// for reachability, so it wins when it disagrees.
	if port := setup.Port(); port > 0 {
		out["LISTEN_ADDR"] = fmt.Sprintf("127.0.0.1:%d", port)
	}
	if preview := listenPort(out["PREVIEW_LISTEN_ADDR"]); preview == 0 || preview == setup.Port() {
		delete(out, "PREVIEW_LISTEN_ADDR")
	}
	return out
}

// LaunchEnv is this flavor's child environment.
//
// # Why credentials travel in the environment
//
// The API key and admin credentials go through the environment rather than
// argv: argv is visible in the process list to every user on the machine, and
// these are bearer credentials for an account pool.
//
// # Why the host's proxy is removed
//
// HTTP(S)_PROXY is deliberately REMOVED: the host may have a proxy configured
// for its own outbound traffic, and a proxy handed to the backend would silently
// route localhost calls (the plugin's own /v1 requests) through it. AI Studio
// takes its own PROXY from .env, so the variable this plugin sets there is the
// one that governs its outbound traffic.
func (f Flavor) LaunchEnv(setup Setup, extra map[string]string) []string {
	blocked := map[string]bool{
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true,
		"http_proxy": true, "https_proxy": true, "all_proxy": true,
	}
	out := make([]string, 0, len(os.Environ())+8)
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
	if f.ID == FlavorGeminiWeb {
		// gemini-web2api-go reads its two credentials from the environment.
		out = append(out, "API_KEY="+setup.APIKey, "ADMIN_TOKEN="+setup.AdminToken)
	}
	// Upstream settings imported from the host's environment must not leak in:
	// for AI Studio they are delivered from its own .env via ServiceEnv, and
	// leaving an inherited variable set to "" is NOT equivalent to unsetting it
	// (upstream logs "Invalid setting value, starting with the default" and
	// listens on 2048 instead of the resolved port).
	for _, name := range studioServiceEnvKeys() {
		out = append(out, name+"=")
	}
	for name, value := range extra {
		// Last one wins for a repeated name in the child environment, which is
		// why the blanks above are emitted first.
		out = append(out, name+"="+value)
	}
	return out
}

// studioServiceEnvKeys are the variables AIStudio2API reads, which must be empty
// in the child so its own .env governs.
func studioServiceEnvKeys() []string {
	return []string{
		"LISTEN_ADDR", "PREVIEW_LISTEN_ADDR", "PROXY_API_KEY", "AISTUDIO_AUTH_STATES",
		"ADMIN_AUTH_ENABLED", "ADMIN_USERNAME", "ADMIN_PASSWORD", "PROXY",
	}
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

// --- gemini-web shorthands ---

// Paths is gemini-web2api-go's file layout, kept as a named struct because the
// existing supervisor and its tests refer to it by field.
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

// PathsFor resolves gemini-web2api-go's layout of a data directory.
func PathsFor(dataDir string) Paths {
	layout := geminiWebFlavor.Layout(dataDir)
	return Paths{
		DataDir:    dataDir,
		Binary:     layout.Binary,
		ConfigFile: layout.Config,
		Secrets:    layout.Secrets,
		Database:   layout.Database,
		PIDFile:    layout.PIDFile,
		LogFile:    layout.LogFile,
		Accounts:   layout.ImportedAccounts,
	}
}

// EnsureDataFiles creates gemini-web2api-go's data files.
func EnsureDataFiles(dataDir string, preferredPort int) (Setup, error) {
	return EnsureGeminiWebFiles(dataDir, preferredPort)
}

// ReadSetup returns gemini-web2api-go's saved setup.
func ReadSetup(dataDir string) (Setup, bool) { return ReadGeminiWebSetup(dataDir) }

// SetPort rewrites gemini-web2api-go's config.json port.
func SetPort(dataDir string, port int) error { return SetGeminiWebPort(dataDir, port) }

// LaunchArgs builds gemini-web2api-go's exact argv.
func LaunchArgs(paths Paths) ([]string, error) {
	if paths.ConfigFile == "" || paths.Database == "" {
		return nil, fmt.Errorf("backend config and database paths are required")
	}
	return []string{"--config", paths.ConfigFile, "--db", paths.Database}, nil
}

// LaunchEnv is gemini-web2api-go's child environment.
func LaunchEnv(secrets Secrets, extra map[string]string) []string {
	return geminiWebFlavor.LaunchEnv(Setup{APIKey: secrets.APIKey, AdminToken: secrets.AdminToken}, extra)
}
