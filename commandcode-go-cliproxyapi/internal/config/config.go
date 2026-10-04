// Package config loads and validates the commandcode plugin configuration
// per spec 04 (configuration) and spec 05 §2 (HTTPS/timeouts).
package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults (spec 04 §2/§4).
const (
	DefaultBaseURL          = "https://api.commandcode.ai/provider/v1"
	DefaultModelPrefix      = "commandcode"
	DefaultRefreshInterval  = 15 * time.Minute
	DefaultRequestTimeout   = 5 * time.Minute
	DefaultMaxResponseBytes = int64(67108864) // 64 MiB
)

type ModelPrefix struct {
	Enabled bool
	Value   string
}

type APIKey struct {
	Value string
}

type Catalog struct {
	RefreshInterval       time.Duration
	StaleWhileUnavailable bool
}

type Protocols struct {
	ChatCompletions bool
	Messages        bool
	Responses       bool
}

type RouteOverride struct {
	Protocol string `yaml:"protocol"`
	Endpoint string `yaml:"endpoint"`
}

// ModelFilter restricts which discovered models are published and routable.
//
// Semantics (deliberately simple, exact-match only):
//   - Allow empty  -> every discovered model is eligible.
//   - Allow set    -> only listed models are eligible.
//   - Deny         -> always wins: a denied model is excluded even if allowed.
//
// Entries match either the upstream id ("deepseek/deepseek-v4.1-flash") or
// the client-facing id with the prefix applied
// ("commandcode/deepseek/deepseek-v4.1-flash"), so an operator can copy
// whichever id they see in the panel or in /v1/models.
//
// Filtering happens while the catalog snapshot is built, so the excluded
// models disappear everywhere at once: /v1/models, model.static,
// model.for_auth, the host model registry, and executor lookup.
type ModelFilter struct {
	Allow []string
	Deny  []string
}

// IsEmpty reports whether the filter would keep every model.
func (f ModelFilter) IsEmpty() bool {
	return len(f.Allow) == 0 && len(f.Deny) == 0
}

// Excludes reports whether a model identified by upstreamID and publicID is
// filtered out by this config.
func (f ModelFilter) Excludes(upstreamID, publicID string) bool {
	if f.matches(f.Deny, upstreamID, publicID) {
		return true
	}
	if len(f.Allow) == 0 {
		return false
	}
	return !f.matches(f.Allow, upstreamID, publicID)
}

func (f ModelFilter) matches(list []string, upstreamID, publicID string) bool {
	for _, want := range list {
		if want == "" {
			continue
		}
		if want == upstreamID || want == publicID {
			return true
		}
	}
	return false
}

type Config struct {
	BaseURL          string
	CatalogURL       string
	ModelPrefix      ModelPrefix
	APIKeys          []APIKey
	Catalog          Catalog
	Protocols        Protocols
	RouteOverrides   map[string]RouteOverride
	Models           ModelFilter
	AllowHTTP        bool
	RequestTimeout   time.Duration
	MaxResponseBytes int64
	// Pending is true when api-keys is still empty. Such a config is
	// VALID to register with (so the host publishes the metadata and the
	// Management Center can show its config form), but the plugin serves
	// no models and refuses execution until keys are supplied. The strict
	// Load still rejects an empty key list, so a plugin that IS configured
	// keeps every existing validation guarantee.
	Pending bool
}

type rawModelFilter struct {
	Allow []string `yaml:"allow"`
	Deny  []string `yaml:"deny"`
}

// rawConfig mirrors the YAML shape; pointer fields distinguish "unset"
// (apply default) from explicitly-set values including "" (validate as-is).
// Unknown fields are ignored (host may pass extra keys).
type rawConfig struct {
	BaseURL          *string                  `yaml:"base-url"`
	CatalogURL       *string                  `yaml:"catalog-url"`
	ModelPrefix      rawPrefix                `yaml:"model-prefix"`
	APIKeys          []rawKey                 `yaml:"api-keys"`
	Catalog          rawCatalog               `yaml:"catalog"`
	Protocols        rawProtocols             `yaml:"protocols"`
	RouteOverrides   map[string]RouteOverride `yaml:"route-overrides"`
	Models           rawModelFilter           `yaml:"models"`
	AllowHTTP        bool                     `yaml:"allow-http"`
	RequestTimeout   *string                  `yaml:"request-timeout"`
	MaxResponseBytes *int64                   `yaml:"max-response-bytes"`
}

type rawPrefix struct {
	Enabled *bool   `yaml:"enabled"`
	Value   *string `yaml:"value"`
}

type rawKey struct {
	Value string `yaml:"value"`
}

type rawCatalog struct {
	RefreshInterval       *string `yaml:"refresh-interval"`
	StaleWhileUnavailable *bool   `yaml:"stale-while-unavailable"`
}

type rawProtocols struct {
	ChatCompletions *bool `yaml:"chat-completions"`
	Messages        *bool `yaml:"messages"`
	Responses       *bool `yaml:"responses"`
}

var (
	validProtocols = map[string]bool{"chat-completions": true, "messages": true, "responses": true}
)

// Load decodes YAML, expands ${VAR} references in api-key values only,
// applies defaults, and validates (spec 04 §6). Decode errors never echo
// decoded node values — a malformed entry (e.g. a bare-scalar API key)
// must not leak into the invalid_config envelope the host logs.
func Load(yamlBytes []byte) (Config, error) {
	return load(yamlBytes, false)
}

// LoadForRegistration is Load for the plugin.register path: an EMPTY
// api-keys list is accepted and marked Config.Pending instead of failing
// registration. Without this the host rejects plugin.register with
// "api-keys: at least one key is required", the plugin never appears as
// registered, and the Management Center therefore never renders its
// config form — leaving no way to enter a key from the UI. Every other
// validation (URLs, durations, duplicates among the keys that ARE
// present, route overrides, prefix) still applies, so a misconfigured
// submission is still surfaced as soon as it is saved.
func LoadForRegistration(yamlBytes []byte) (Config, error) {
	return load(yamlBytes, true)
}

func load(yamlBytes []byte, allowPending bool) (Config, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(yamlBytes, &raw); err != nil {
		if n := regexp.MustCompile(`line (\d+)`).FindStringSubmatch(err.Error()); n != nil {
			return Config{}, fmt.Errorf("decode config: invalid YAML structure near line %s", n[1])
		}
		return Config{}, fmt.Errorf("decode config: invalid YAML structure")
	}
	keys := make([]APIKey, len(raw.APIKeys))
	for i, k := range raw.APIKeys {
		keys[i] = APIKey{Value: os.ExpandEnv(k.Value)}
	}
	refreshInterval, err := parseDuration("catalog.refresh-interval", raw.Catalog.RefreshInterval, DefaultRefreshInterval)
	if err != nil {
		return Config{}, err
	}
	requestTimeout, err := parseDuration("request-timeout", raw.RequestTimeout, DefaultRequestTimeout)
	if err != nil {
		return Config{}, err
	}
	if requestTimeout <= 0 {
		return Config{}, fmt.Errorf("request-timeout: must be positive")
	}
	c := Config{
		BaseURL: orDefault(raw.BaseURL, DefaultBaseURL),
		ModelPrefix: ModelPrefix{
			Enabled: orDefault(raw.ModelPrefix.Enabled, true),
			Value:   orDefault(raw.ModelPrefix.Value, DefaultModelPrefix),
		},
		APIKeys: keys,
		Catalog: Catalog{
			RefreshInterval:       refreshInterval,
			StaleWhileUnavailable: orDefault(raw.Catalog.StaleWhileUnavailable, true),
		},
		Protocols: Protocols{
			ChatCompletions: orDefault(raw.Protocols.ChatCompletions, true),
			Messages:        orDefault(raw.Protocols.Messages, true),
			Responses:       orDefault(raw.Protocols.Responses, true),
		},
		RouteOverrides: raw.RouteOverrides,
		Models: ModelFilter{
			Allow: normalizeModelList(raw.Models.Allow),
			Deny:  normalizeModelList(raw.Models.Deny),
		},
		AllowHTTP:        raw.AllowHTTP,
		RequestTimeout:   requestTimeout,
		MaxResponseBytes: orDefault(raw.MaxResponseBytes, DefaultMaxResponseBytes),
		Pending:          allowPending && len(keys) == 0,
	}
	if raw.CatalogURL != nil {
		// Mirror the derived-default trim so an explicit trailing-slash
		// catalog-url cannot double up separators downstream.
		c.CatalogURL = strings.TrimRight(*raw.CatalogURL, "/")
	} else {
		c.CatalogURL = strings.TrimRight(c.BaseURL, "/") + "/models"
	}
	if err := c.validateForLoad(allowPending); err != nil {
		return Config{}, err
	}
	return c, nil
}

// validateForLoad runs the strict validation, or the pending-tolerant one
// when the caller allowed an empty api-keys list.
func (c Config) validateForLoad(allowPending bool) error {
	if allowPending && len(c.APIKeys) == 0 {
		return c.validateWithoutKeys()
	}
	return c.validate()
}

// PublicID returns the client-facing model ID (FR-003).
func PublicID(c Config, upstreamID string) string {
	if c.ModelPrefix.Enabled {
		return c.ModelPrefix.Value + "/" + upstreamID
	}
	return upstreamID
}

func (c Config) validate() error {
	if err := validateURL("base-url", c.BaseURL, c.AllowHTTP); err != nil {
		return err
	}
	if err := validateURL("catalog-url", c.CatalogURL, c.AllowHTTP); err != nil {
		return err
	}
	if len(c.APIKeys) == 0 {
		return fmt.Errorf("api-keys: at least one key is required")
	}
	return c.validateCommon()
}

// validateWithoutKeys runs every check except "at least one key", for the
// pending-registration path where keys are not yet supplied.
func (c Config) validateWithoutKeys() error {
	if err := validateURL("base-url", c.BaseURL, c.AllowHTTP); err != nil {
		return err
	}
	if err := validateURL("catalog-url", c.CatalogURL, c.AllowHTTP); err != nil {
		return err
	}
	return c.validateCommon()
}

// validateCommon holds the checks shared by both validation modes.
func (c Config) validateCommon() error {
	for i, k := range c.APIKeys {
		if k.Value == "" {
			return fmt.Errorf("api-keys[%d].value: expanded to empty", i)
		}
	}
	seen := make(map[string]bool, len(c.APIKeys))
	for _, k := range c.APIKeys {
		if seen[k.Value] {
			return fmt.Errorf("api-keys: duplicate key values are not allowed")
		}
		seen[k.Value] = true
	}
	if c.Catalog.RefreshInterval < time.Minute {
		return fmt.Errorf("catalog.refresh-interval: must be at least 1m")
	}
	if c.MaxResponseBytes <= 0 {
		return fmt.Errorf("max-response-bytes: must be positive")
	}
	for name, o := range c.RouteOverrides {
		if !validProtocols[o.Protocol] {
			return fmt.Errorf("route-overrides[%s].protocol: unsupported protocol %q", name, o.Protocol)
		}
		if o.Endpoint == "" {
			return fmt.Errorf("route-overrides[%s].endpoint: must not be empty", name)
		}
		if !strings.HasPrefix(o.Endpoint, "/") {
			return fmt.Errorf("route-overrides[%s].endpoint: must start with /", name)
		}
	}
	if c.ModelPrefix.Enabled && !validPrefix(c.ModelPrefix.Value) {
		return fmt.Errorf("model-prefix.value: invalid provider-ID characters %q", c.ModelPrefix.Value)
	}
	// A model id listed in a filter must at least look like an id; an entry
	// containing whitespace or a slash-only value is a typo that would
	// silently exclude everything under an allow list.
	for _, group := range []struct {
		name   string
		values []string
	}{{"models.allow", c.Models.Allow}, {"models.deny", c.Models.Deny}} {
		for _, id := range group.values {
			if strings.ContainsAny(id, " 	\n") {
				return fmt.Errorf("%s: entry %q must not contain whitespace", group.name, id)
			}
		}
	}
	return nil
}

func validateURL(name, raw string, allowHTTP bool) error {
	if raw == "" {
		return fmt.Errorf("%s: must not be empty", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid URL", name)
	}
	if u.Scheme == "" || u.Host == "" {
		// Echo only scheme://host — the configured string may embed
		// userinfo credentials (https://user:key@host).
		return fmt.Errorf("%s: invalid URL %s://%s", name, u.Scheme, u.Host)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("%s: must not contain query, fragment, or userinfo", name)
	}
	// Scheme allowlist (spec 05 §2): https always; http only behind
	// allow-http; anything else (ftp://, file://, custom schemes) is
	// rejected at load instead of failing at transport time.
	switch u.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return fmt.Errorf("%s: http scheme requires allow-http", name)
		}
	default:
		return fmt.Errorf("%s: unsupported scheme %q; use https", name, u.Scheme)
	}
	return nil
}

func validPrefix(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if i == 0 && !alnum {
			return false
		}
		if !alnum && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func parseDuration(name string, p *string, def time.Duration) (time.Duration, error) {
	if p == nil {
		return def, nil
	}
	d, err := time.ParseDuration(*p)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration", name)
	}
	return d, nil
}

func orDefault[T any](p *T, def T) T {
	if p != nil {
		return *p
	}
	return def
}

// normalizeModelList trims entries, drops empties and exact duplicates, and
// keeps first-seen order so a saved config round-trips predictably.
func normalizeModelList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		v := strings.TrimSpace(raw)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
