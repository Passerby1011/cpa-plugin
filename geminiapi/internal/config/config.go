// Package config loads and validates the geminiapi plugin configuration from
// the plugins.configs.geminiapi node of CPA's config.yaml.
//
// The plugin serves ONE provider id ("geminiapi") but TWO model namespaces:
// model ids are published as "<namespace>/<model>", where the namespace is
// "ai-studio" (AIStudio2API) or "gemini-web" (gemini-web2api-go). Each
// namespace has its own backend block and, when it is backed by an external
// service, its own base-url: a namespace with base-url set is not managed by
// this plugin, it is only proxied.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Namespaces are the model-id prefixes served by this single provider. A
// provider id is a single value per .so (executor.identifier), so two backends
// are two namespaces of one provider rather than two providers.
const (
	NamespaceAIStudio  = "ai-studio"
	NamespaceGeminiWeb = "gemini-web"
)

const (
	// DefaultNamespace is the namespace an operator means when they name none.
	DefaultNamespace = NamespaceAIStudio

	// DefaultRequestTimeout is the base per-request deadline. Both backends
	// can think for minutes; 30m matches the card's requirement and outlasts
	// the backend's own upstream retries.
	DefaultRequestTimeout = 30 * time.Minute
	// DefaultStreamIdleTimeout bounds the gap between upstream chunks, NOT the
	// whole stream: a long-but-productive reasoning stream must not be killed,
	// while a silently stalled one must not hold a stream forever.
	DefaultStreamIdleTimeout = 5 * time.Minute
	// DefaultModelTimeout bounds catalog/account/health reads.
	DefaultModelTimeout = 15 * time.Second

	// DefaultPortAIStudio / DefaultPortGeminiWeb are the two backends' own
	// defaults. gemini-web2api-go publishes 2048; AIStudio2API also defaults to
	// 2048, so this plugin starts the second backend one port up: two fresh
	// installs on one machine must not race for the same port. A port already
	// recorded in a data directory always wins afterwards.
	DefaultPortAIStudio  = 2049
	DefaultPortGeminiWeb = 2048

	// DefaultContextLimit / DefaultOutputLimit are used only when the backend
	// does not report per-model limits (gemini-web2api-go never does).
	DefaultContextLimit = int64(1000000)
	DefaultOutputLimit  = int64(8192)
)

// NamespaceConfig is the configuration of ONE namespace's backend.
type NamespaceConfig struct {
	// BaseURL points at an external, already-running OpenAI-compatible
	// backend. When set, the plugin never installs, starts or stops a backend
	// for this namespace: it only proxies. Empty means the plugin manages its
	// own local backend.
	BaseURL string
	// APIKey is the external backend's API key. Ignored (and refused) when
	// BaseURL is empty, because a managed backend's key lives in the plugin's
	// data directory.
	APIKey string
	// Proxy is the proxy the BACKEND uses to reach Google. It is NOT the proxy
	// the plugin uses to reach the backend: localhost traffic must not go
	// through a proxy. Empty means the backend keeps its saved pool
	// (gemini-web) or its saved .env value (AI Studio).
	Proxy string
	// AllowImageModels exposes image-generation models. Only AI Studio
	// publishes any; CPA has no image generation route, so a chat request
	// naming one can only fail. Off by default, and the README says why.
	AllowImageModels bool
	// ContextLimit and OutputLimit override the per-model limits the backend
	// reports. Zero keeps the backend's values.
	ContextLimit int64
	OutputLimit  int64
	// Port is the port a NEW managed backend is started on. A port already
	// recorded in the data directory wins, so an operator's choice survives.
	Port int
}

// Managed reports whether the plugin owns this namespace's backend process.
func (n NamespaceConfig) Managed() bool { return n.BaseURL == "" }

// ModelFilter restricts which discovered models are published and routable.
//
//   - Allow empty -> every discovered model is eligible.
//   - Allow set   -> only listed models are eligible.
//   - Deny        -> always wins.
//
// '*' matches any run of characters including none. An entry may match either
// the bare upstream id ("gemini-2.5-pro") or the published id
// ("ai-studio/gemini-2.5-pro"), so an operator can copy whichever they see.
type ModelFilter struct {
	Allow []string
	Deny  []string
}

// IsEmpty reports whether the filter keeps every model.
func (f ModelFilter) IsEmpty() bool { return len(f.Allow) == 0 && len(f.Deny) == 0 }

// Excludes reports whether a model is filtered out. upstreamID is the id the
// backend reported; publicID is the id this plugin publishes.
func (f ModelFilter) Excludes(upstreamID, publicID string) bool {
	if matchAny(f.Deny, upstreamID, publicID) {
		return true
	}
	if len(f.Allow) == 0 {
		return false
	}
	return !matchAny(f.Allow, upstreamID, publicID)
}

func matchAny(list []string, ids ...string) bool {
	for _, want := range list {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		for _, id := range ids {
			if globMatch(want, id) {
				return true
			}
		}
	}
	return false
}

// globMatch reports whether pattern matches name. '*' matches any run of
// characters, including none. A pattern WITHOUT '*' is an exact,
// case-sensitive comparison (so pre-wildcard entries keep their meaning); a
// pattern with '*' is folded case-insensitively, because a hand-written family
// pattern ("Gemini-*-pro") is written from memory.
func globMatch(pattern, name string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == name
	}
	p, n := 0, 0
	star, mark := -1, 0
	for n < len(name) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, n
			p++
		case p < len(pattern) && asciiEqualFold(pattern[p], name[n]):
			p++
			n++
		case star >= 0:
			mark++
			p, n = star+1, mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

func asciiEqualFold(a, b byte) bool {
	if a == b {
		return true
	}
	if 'A' <= a && a <= 'Z' {
		a += 'a' - 'A'
	}
	if 'B' <= b && b <= 'Z' {
		b += 'a' - 'A'
	}
	return a == b
}

// Config is the validated plugin configuration.
type Config struct {
	// DataDir is the ROOT of the managed backends' state. Each namespace keeps
	// its own subdirectory under it (see DataDirFor), because two backends
	// writing one config.json would overwrite each other.
	DataDir string
	// SyncProxy is the proxy used to download the backend binaries and to reach
	// a backend's own admin surface. It is never used for 127.0.0.1 traffic.
	SyncProxy string
	// RequestTimeout is the base per-request deadline.
	RequestTimeout time.Duration
	// StreamIdleTimeout is the independent stream-silence budget.
	StreamIdleTimeout time.Duration
	// ModelTimeout bounds catalog / account / health reads.
	ModelTimeout time.Duration
	Models       ModelFilter
	// AIStudio and GeminiWeb are the two namespaces' backend settings.
	AIStudio  NamespaceConfig
	GeminiWeb NamespaceConfig
	// Namespaces lists the model namespaces this deployment publishes.
	Namespaces []string
}

// Namespace returns the configuration of one namespace.
func (c Config) Namespace(name string) (NamespaceConfig, error) {
	switch strings.TrimSpace(name) {
	case NamespaceAIStudio:
		return c.AIStudio, nil
	case NamespaceGeminiWeb:
		return c.GeminiWeb, nil
	}
	return NamespaceConfig{}, fmt.Errorf("unknown namespace %q; this plugin serves %s and %s", name, NamespaceAIStudio, NamespaceGeminiWeb)
}

// DataDirRoot is where the managed backends live when data-dir is unset.
func DataDirRoot() string {
	if dir := strings.TrimSpace(os.Getenv("GEMINIAPI_DATA_DIR")); dir != "" {
		return filepath.Clean(dir)
	}
	base, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base, _ = os.UserHomeDir()
	}
	return filepath.Join(base, "cpa-plugin", "geminiapi")
}

// DataDirFor is the state directory of one namespace under the configured root.
//
// The two backends get separate directories because they keep incompatible
// state under the same file names (config.json, backend.pid, backend.log). A
// shared directory would make one backend read the other's config.
func (c Config) DataDirFor(namespace string) string {
	if strings.TrimSpace(c.DataDir) == "" {
		return ""
	}
	return filepath.Join(c.DataDir, namespace)
}

// rawConfig mirrors the YAML node. Duration-ish values are strings so an
// operator can write "30m" the way the rest of this repo's config does.
type rawConfig struct {
	DataDir           string `yaml:"data-dir"`
	Proxy             string `yaml:"proxy"`
	RequestTimeout    string `yaml:"request-timeout"`
	StreamIdleTimeout string `yaml:"stream-idle-timeout"`
	ModelTimeout      string `yaml:"model-timeout"`
	Models            struct {
		Allow []string `yaml:"allow"`
		Deny  []string `yaml:"deny"`
	} `yaml:"models"`
	AIStudio  rawNamespace `yaml:"ai-studio"`
	GeminiWeb rawNamespace `yaml:"gemini-web"`
}

// rawNamespace is one namespace's YAML node.
type rawNamespace struct {
	BaseURL          string `yaml:"base-url"`
	APIKey           string `yaml:"api-key"`
	Proxy            string `yaml:"proxy"`
	AllowImageModels bool   `yaml:"allow-image-models"`
	ContextLimit     int64  `yaml:"context-limit"`
	OutputLimit      int64  `yaml:"output-limit"`
	Port             int    `yaml:"port"`
}

// Load parses the plugins.configs.geminiapi YAML node.
//
// An empty node is valid: every option has a default, and the plugin registers
// so the Management Center can render its config form. Validation failures are
// reported with the offending key so an operator can fix the YAML rather than
// guess.
func Load(node []byte) (Config, error) {
	var raw rawConfig
	if len(node) > 0 {
		if err := yaml.Unmarshal(node, &raw); err != nil {
			return Config{}, fmt.Errorf("invalid geminiapi config: %w", err)
		}
	}
	cfg := Config{
		DataDir:           strings.TrimSpace(raw.DataDir),
		SyncProxy:         strings.TrimSpace(raw.Proxy),
		RequestTimeout:    DefaultRequestTimeout,
		StreamIdleTimeout: DefaultStreamIdleTimeout,
		ModelTimeout:      DefaultModelTimeout,
		Models: ModelFilter{
			Allow: trimAll(raw.Models.Allow),
			Deny:  trimAll(raw.Models.Deny),
		},
		Namespaces: []string{NamespaceAIStudio, NamespaceGeminiWeb},
	}
	if cfg.DataDir == "" {
		cfg.DataDir = DataDirRoot()
	}
	cfg.DataDir = filepath.Clean(cfg.DataDir)

	var err error
	if cfg.RequestTimeout, err = durationValue("request-timeout", raw.RequestTimeout, DefaultRequestTimeout, time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.StreamIdleTimeout, err = durationValue("stream-idle-timeout", raw.StreamIdleTimeout, DefaultStreamIdleTimeout, 5*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.ModelTimeout, err = durationValue("model-timeout", raw.ModelTimeout, DefaultModelTimeout, time.Second); err != nil {
		return Config{}, err
	}

	if cfg.SyncProxy != "" {
		if _, err := ValidateProxy(cfg.SyncProxy); err != nil {
			return Config{}, fmt.Errorf("invalid proxy: %w", err)
		}
	}

	aiStudio, err := validateNamespace(NamespaceAIStudio, raw.AIStudio)
	if err != nil {
		return Config{}, err
	}
	geminiWeb, err := validateNamespace(NamespaceGeminiWeb, raw.GeminiWeb)
	if err != nil {
		return Config{}, err
	}
	// AI Studio is the only namespace that can publish image models, so an
	// allow-image-models switch on the other one is meaningless: refusing it is
	// better than accepting a setting that does nothing.
	if raw.GeminiWeb.AllowImageModels {
		return Config{}, fmt.Errorf("gemini-web.allow-image-models is not supported: gemini-web2api-go publishes no image models, so only ai-studio can expose them")
	}
	cfg.AIStudio = aiStudio
	cfg.GeminiWeb = geminiWeb
	return cfg, nil
}

// validateNamespace validates one namespace's block.
func validateNamespace(name string, raw rawNamespace) (NamespaceConfig, error) {
	out := NamespaceConfig{
		BaseURL:          strings.TrimSpace(raw.BaseURL),
		APIKey:           strings.TrimSpace(raw.APIKey),
		Proxy:            strings.TrimSpace(raw.Proxy),
		AllowImageModels: raw.AllowImageModels,
		ContextLimit:     raw.ContextLimit,
		OutputLimit:      raw.OutputLimit,
		Port:             raw.Port,
	}
	if out.BaseURL != "" {
		normalized, err := NormalizeBaseURL(out.BaseURL)
		if err != nil {
			return NamespaceConfig{}, fmt.Errorf("invalid %s.base-url: %w", name, err)
		}
		out.BaseURL = normalized
		if out.APIKey == "" {
			return NamespaceConfig{}, fmt.Errorf("%s.base-url is set but %s.api-key is empty: an external backend needs its own API key", name, name)
		}
		if err := checkHeaderSafe(name+".api-key", out.APIKey); err != nil {
			return NamespaceConfig{}, err
		}
	} else if out.APIKey != "" {
		// A managed backend's key is generated into its data directory.
		// Accepting one here would silently mislead: the operator would expect
		// their key to be used, and the backend would keep rejecting it.
		return NamespaceConfig{}, fmt.Errorf("%s.api-key is only valid together with %s.base-url; a plugin-managed backend generates its own key in its data directory", name, name)
	}
	if out.Proxy != "" {
		if _, err := ValidateProxy(out.Proxy); err != nil {
			return NamespaceConfig{}, fmt.Errorf("invalid %s.proxy: %w", name, err)
		}
	}
	if out.ContextLimit < 0 || out.OutputLimit < 0 {
		return NamespaceConfig{}, fmt.Errorf("%s.context-limit and %s.output-limit must not be negative", name, name)
	}
	if out.OutputLimit > 0 && out.ContextLimit > 0 && out.OutputLimit > out.ContextLimit {
		return NamespaceConfig{}, fmt.Errorf("%s.output-limit (%d) cannot exceed %s.context-limit (%d)", name, out.OutputLimit, name, out.ContextLimit)
	}
	if out.Port < 0 || out.Port > 65535 {
		return NamespaceConfig{}, fmt.Errorf("%s.port must be between 1 and 65535, got %d", name, out.Port)
	}
	if out.BaseURL != "" && out.Port != 0 {
		return NamespaceConfig{}, fmt.Errorf("%s.port cannot be set together with %s.base-url: the plugin does not manage an external backend", name, name)
	}
	return out, nil
}

func trimAll(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func durationValue(key, value string, fallback, minimum time.Duration) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: use a Go duration such as 30m or 90s", key, value)
	}
	if d < minimum {
		return 0, fmt.Errorf("invalid %s %q: must be at least %s", key, value, minimum)
	}
	return d, nil
}

// ValidateProxy accepts http, https, socks5 and socks5h proxy URLs and rejects
// a value containing a newline: it ends up in a request header and in the
// backend's proxy pool.
func ValidateProxy(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if strings.ContainsAny(raw, "\r\n") {
		return "", fmt.Errorf("proxy URL must not contain newlines")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid proxy URL: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", fmt.Errorf("proxy URL must start with http://, https://, socks5:// or socks5h://")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("proxy URL must include a host")
	}
	return raw, nil
}

// NormalizeBaseURL validates an OpenAI-compatible base URL and strips a
// trailing slash so "<base>/v1/models" always builds correctly.
//
// A loopback host is required for plain http: the backend carries an account's
// Google cookies, and sending them to a remote plaintext origin would leak
// them. An https remote origin is allowed.
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty base URL")
	}
	if strings.ContainsAny(raw, "\r\n") {
		return "", fmt.Errorf("base URL must not contain newlines")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", fmt.Errorf("base URL must not carry a query, fragment or userinfo")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	switch strings.ToLower(parsed.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHost(parsed.Hostname()) {
			return "", fmt.Errorf("plain http is only allowed for a loopback host (127.0.0.1, ::1, localhost); use https for %q", parsed.Hostname())
		}
	default:
		return "", fmt.Errorf("base URL must start with http:// or https://")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("base URL must include a host")
	}
	return parsed.String(), nil
}

func isLoopbackHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

func checkHeaderSafe(key, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("invalid %s: must not contain newlines", key)
	}
	return nil
}
