// Package config loads and validates the geminiapi plugin configuration from
// the plugins.configs.geminiapi node of CPA's config.yaml.
//
// The plugin serves ONE provider id ("geminiapi") but two model namespaces:
// model ids are published as "<namespace>/<model>", where the namespace is
// "ai-studio" (this card) or "gemini-web" (downstream card). A namespace that
// is backed by an external, already-running service is expressed by
// "<namespace>.base-url": when it is set the plugin does NOT manage that
// backend, it only proxies.
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
	// DefaultNamespace is the namespace whose backend this card manages.
	DefaultNamespace = NamespaceAIStudio

	// DefaultRequestTimeout is the base per-request deadline. AI Studio turns
	// can think for minutes; 30m matches the card's requirement and outlasts
	// the backend's own upstream retries.
	DefaultRequestTimeout = 30 * time.Minute
	// DefaultStreamIdleTimeout bounds the gap between upstream chunks, NOT the
	// whole stream: a long-but-productive reasoning stream must not be killed,
	// while a silently stalled one must not hold a stream forever. Gemini can
	// stay quiet for a while before its first token, so this is generous.
	DefaultStreamIdleTimeout = 5 * time.Minute
	// DefaultModelTimeout bounds catalog/account/health reads.
	DefaultModelTimeout = 15 * time.Second

	// DefaultPort is the backend's own default port; the manager picks the
	// next free one when it is taken and persists the choice.
	DefaultPort = 2048

	// DefaultContextLimit / DefaultOutputLimit are used only when the backend
	// does not report per-model limits.
	DefaultContextLimit = int64(1000000)
	DefaultOutputLimit  = int64(8192)
)

// AIStudio is the configuration of the ai-studio namespace.
type AIStudio struct {
	// BaseURL points at an external, already-running OpenAI-compatible
	// backend (for example an AIStudio2API instance the operator manages
	// themselves). When set, the plugin never installs, starts or stops a
	// backend for this namespace: it only proxies. Empty means the plugin
	// manages its own local backend.
	BaseURL string
	// APIKey is the external backend's API key. Ignored (and refused) when
	// BaseURL is empty, because a managed backend's key lives in the plugin's
	// data directory.
	APIKey string
	// Proxy is the proxy the backend uses to reach Google. It is NOT the
	// proxy the plugin uses to reach the backend: localhost traffic must not
	// go through a proxy. Empty means the backend keeps its saved pool.
	Proxy string
	// AllowImageModels exposes image-generation models. CPA has no image
	// generation route, so a chat request naming one of these models can only
	// fail; it is off by default and the README says why.
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
func (a AIStudio) Managed() bool { return a.BaseURL == "" }

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
	// DataDir holds the managed backend's binary, config, secrets, accounts
	// and log. Defaults to a directory under the user's local app data.
	DataDir string
	// SyncProxy is the proxy used to download the backend binary and to reach
	// the backend's own admin API. It is never used for 127.0.0.1 traffic.
	SyncProxy string
	// RequestTimeout is the base per-request deadline.
	RequestTimeout time.Duration
	// StreamIdleTimeout is the independent stream-silence budget.
	StreamIdleTimeout time.Duration
	// ModelTimeout bounds catalog / account / health reads.
	ModelTimeout time.Duration
	Models       ModelFilter
	AIStudio     AIStudio
	// Namespaces lists the model namespaces this deployment publishes. It is
	// always ai-studio for this card; gemini-web is added by its own card.
	Namespaces []string
}

// DefaultDataDir is where a managed backend lives when data-dir is unset.
func DefaultDataDir() string {
	if dir := strings.TrimSpace(os.Getenv("GEMINIAPI_DATA_DIR")); dir != "" {
		return filepath.Clean(dir)
	}
	base, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base, _ = os.UserHomeDir()
	}
	return filepath.Join(base, "cpa-plugin", "geminiapi")
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
	AIStudio struct {
		BaseURL          string `yaml:"base-url"`
		APIKey           string `yaml:"api-key"`
		Proxy            string `yaml:"proxy"`
		AllowImageModels bool   `yaml:"allow-image-models"`
		ContextLimit     int64  `yaml:"context-limit"`
		OutputLimit      int64  `yaml:"output-limit"`
		Port             int    `yaml:"port"`
	} `yaml:"ai-studio"`
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
		AIStudio: AIStudio{
			BaseURL:          strings.TrimSpace(raw.AIStudio.BaseURL),
			APIKey:           strings.TrimSpace(raw.AIStudio.APIKey),
			Proxy:            strings.TrimSpace(raw.AIStudio.Proxy),
			AllowImageModels: raw.AIStudio.AllowImageModels,
			ContextLimit:     raw.AIStudio.ContextLimit,
			OutputLimit:      raw.AIStudio.OutputLimit,
			Port:             raw.AIStudio.Port,
		},
		Namespaces: []string{NamespaceAIStudio},
	}
	if cfg.DataDir == "" {
		cfg.DataDir = DefaultDataDir()
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
	if cfg.AIStudio.BaseURL != "" {
		normalized, err := NormalizeBaseURL(cfg.AIStudio.BaseURL)
		if err != nil {
			return Config{}, fmt.Errorf("invalid ai-studio.base-url: %w", err)
		}
		cfg.AIStudio.BaseURL = normalized
		if cfg.AIStudio.APIKey == "" {
			return Config{}, fmt.Errorf("ai-studio.base-url is set but ai-studio.api-key is empty: an external backend needs its own API key")
		}
		if err := checkHeaderSafe("ai-studio.api-key", cfg.AIStudio.APIKey); err != nil {
			return Config{}, err
		}
	} else if cfg.AIStudio.APIKey != "" {
		// A managed backend's key is generated into the data directory. Accepting
		// one here would silently mislead: the operator would expect their key to
		// be used, and the backend would keep rejecting it.
		return Config{}, fmt.Errorf("ai-studio.api-key is only valid together with ai-studio.base-url; a plugin-managed backend generates its own key in data-dir/secrets.json")
	}
	if cfg.AIStudio.Proxy != "" {
		if _, err := ValidateProxy(cfg.AIStudio.Proxy); err != nil {
			return Config{}, fmt.Errorf("invalid ai-studio.proxy: %w", err)
		}
	}
	if cfg.AIStudio.ContextLimit < 0 || cfg.AIStudio.OutputLimit < 0 {
		return Config{}, fmt.Errorf("ai-studio.context-limit and ai-studio.output-limit must not be negative")
	}
	if cfg.AIStudio.OutputLimit > 0 && cfg.AIStudio.ContextLimit > 0 && cfg.AIStudio.OutputLimit > cfg.AIStudio.ContextLimit {
		return Config{}, fmt.Errorf("ai-studio.output-limit (%d) cannot exceed ai-studio.context-limit (%d)", cfg.AIStudio.OutputLimit, cfg.AIStudio.ContextLimit)
	}
	if cfg.AIStudio.Port < 0 || cfg.AIStudio.Port > 65535 {
		return Config{}, fmt.Errorf("ai-studio.port must be between 1 and 65535, got %d", cfg.AIStudio.Port)
	}
	if cfg.AIStudio.BaseURL != "" && cfg.AIStudio.Port != 0 {
		return Config{}, fmt.Errorf("ai-studio.port cannot be set together with ai-studio.base-url: the plugin does not manage an external backend")
	}
	return cfg, nil
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
// a value containing a newline: it ends up in an Authorization-free request
// header and in the backend's proxy pool.
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
