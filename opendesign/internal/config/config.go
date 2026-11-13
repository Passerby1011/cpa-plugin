// Package config loads and validates the opendesign plugin configuration.
//
// The plugin faces two Open Design planes, both named here once:
//
//		base-url     the model plane (amr-link), where every model request goes at
//		             the account's runtime key (sk-amr-…)
//		control-url  the Vela control plane (amr-api), where the device sign-in is
//		             opened and where the account's plan allowance is read at its
//	            control key (vela_ctrl_…)
//
// allow-http keeps the same meaning it has in this repository's other plugins:
// plain http is refused unless the operator opts in, and a URL carrying a
// query, a fragment, or userinfo is never accepted — an upstream URL comes from
// configuration, and a credential must never be smuggled into it.
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

// Defaults. Every value here is what a config that says nothing gets.
const (
	DefaultBaseURL          = "https://amr-link.open-design.ai/v1"
	DefaultControlURL       = "https://amr-api.open-design.ai"
	DefaultModelPrefix      = "opendesign"
	DefaultRequestTimeout   = 5 * time.Minute
	DefaultMaxResponseBytes = int64(67108864) // 64 MiB
	DefaultPoolStrategy     = "sticky"
)

// ModelPrefix is the client-facing id prefix. Enabled by default: the public
// id is "opendesign/<upstream id>", which is what the host routes on and what a
// client sees in /v1/models.
type ModelPrefix struct {
	Enabled bool
	Value   string
}

// ModelFilter restricts which discovered models are published and routable.
//
// Semantics (identical to this repository's commandcode plugin):
//   - Allow empty -> every model is eligible.
//   - Allow set   -> only listed models are eligible.
//   - Deny        -> always wins.
//
// An entry may contain '*', which matches any run of characters including
// none, so `deny: ["claude-*"]` drops a family without listing every model.
// A pattern with '*' matches case-insensitively; an entry without one stays an
// exact, case-sensitive comparison.
//
// Entries match either the upstream id ("glm-5.3-flash") or the client-facing
// id with the prefix applied ("opendesign/glm-5.3-flash").
type ModelFilter struct {
	Allow []string
	Deny  []string
}

// IsEmpty reports whether the filter would keep every model.
func (f ModelFilter) IsEmpty() bool {
	return len(f.Allow) == 0 && len(f.Deny) == 0
}

// Excludes reports whether a model identified by upstreamID and publicID is
// filtered out.
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
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		if GlobMatch(want, upstreamID) || GlobMatch(want, publicID) {
			return true
		}
	}
	return false
}

// GlobMatch reports whether pattern matches name. '*' matches any run of
// characters, including none. A pattern without '*' is compared exactly and
// case-sensitively; once it carries a wildcard it is matched case-insensitively,
// because a hand-written family pattern is written from memory and model ids
// differ in case across providers.
//
// Iterative with backtracking (no regexp): this runs once per discovered model
// while the catalog snapshot is built, so it must not allocate.
func GlobMatch(pattern, name string) bool {
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
	if 'A' <= b && b <= 'Z' {
		b += 'a' - 'A'
	}
	return a == b
}

// Pool controls how one credential is chosen per request.
type Pool struct {
	// Strategy is "sticky" (prefer the last healthy credential) or
	// "round-robin".
	Strategy string
	// MaxConcurrencyPerAccount bounds concurrent requests per credential; 0
	// means unlimited.
	MaxConcurrencyPerAccount int
}

// Account is one credential in the pool.
//
// RuntimeKey rides model requests; ControlKey is optional and only an account
// sign-in produces one. An account with no ControlKey is a pasted API key: its
// models are still discovered at the runtime key, but its plan allowance
// cannot be read, because the control plane does not take an API key.
type Account struct {
	Label      string
	RuntimeKey string
	ControlKey string
	// Email/UID are known only for accounts created by the device sign-in.
	Email string
	UID   string
}

type Config struct {
	BaseURL          string
	ControlURL       string
	ModelPrefix      ModelPrefix
	APIKeys          []Account
	Accounts         []Account
	Pool             Pool
	Models           ModelFilter
	AllowHTTP        bool
	RequestTimeout   time.Duration
	TimeoutSet       bool
	MaxResponseBytes int64
	// Pending is true when no usable credential is configured. Such a config
	// is valid to register with (so the Management Center renders the config
	// form), but the plugin serves no models and refuses execution.
	Pending bool
}

// EffectiveAccounts returns the config's credentials, api-keys folded in after
// accounts, deduplicated by runtime key.
func (c Config) EffectiveAccounts() []Account {
	seen := make(map[string]struct{}, len(c.Accounts)+len(c.APIKeys))
	out := make([]Account, 0, len(c.Accounts)+len(c.APIKeys))
	for _, a := range append(append([]Account{}, c.Accounts...), c.APIKeys...) {
		key := strings.TrimSpace(a.RuntimeKey)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, a)
	}
	return out
}

// PublicID renders the client-facing model id for one upstream id.
func (c Config) PublicID(upstreamID string) string {
	if c.ModelPrefix.Enabled && strings.TrimSpace(c.ModelPrefix.Value) != "" {
		return c.ModelPrefix.Value + "/" + upstreamID
	}
	return upstreamID
}

// rawConfig mirrors the YAML shape; pointer fields distinguish "unset" (apply
// the default) from explicitly-set values, including an empty string.
type rawConfig struct {
	BaseURL          *string        `yaml:"base-url"`
	ControlURL       *string        `yaml:"control-url"`
	ModelPrefix      rawPrefix      `yaml:"model-prefix"`
	APIKeys          []rawKey       `yaml:"api-keys"`
	Accounts         []rawKey       `yaml:"accounts"`
	Pool             rawPool        `yaml:"pool"`
	Models           rawModelFilter `yaml:"models"`
	AllowHTTP        bool           `yaml:"allow-http"`
	RequestTimeout   *string        `yaml:"request-timeout"`
	MaxResponseBytes *int64         `yaml:"max-response-bytes"`
}

type rawPrefix struct {
	Enabled *bool   `yaml:"enabled"`
	Value   *string `yaml:"value"`
}

type rawPool struct {
	Strategy                 string `yaml:"strategy"`
	MaxConcurrencyPerAccount *int   `yaml:"max-concurrency-per-account"`
}

type rawModelFilter struct {
	Allow []string `yaml:"allow"`
	Deny  []string `yaml:"deny"`
}

// rawKey is one pool entry. It accepts the documented object form
// ({"value": "sk-amr-…", "control": "vela_ctrl_…", "label": "work"}) and a bare
// scalar, because the Management Center's array editor produces a plain string
// when a key is typed into it. `key`/`api_key` are accepted as spellings of
// "value" so a credential copied out of another plugin's config still loads.
type rawKey struct {
	Value      string `yaml:"value"`
	RuntimeKey string `yaml:"runtime-key"`
	ControlKey string `yaml:"control-key"`
	Control    string `yaml:"control"`
	Label      string `yaml:"label"`
	Key        string `yaml:"key"`
	APIKey     string `yaml:"api_key"`
	Email      string `yaml:"email"`
	UID        string `yaml:"uid"`
}

// UnmarshalYAML accepts a scalar (the runtime key itself) or a mapping.
func (k *rawKey) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag != "!!str" {
			return fmt.Errorf("an api-keys entry must be a string or an object")
		}
		k.Value = node.Value
		return nil
	case yaml.MappingNode:
		type plain rawKey
		var p plain
		if err := node.Decode(&p); err != nil {
			// Never echo the value: a malformed entry may carry a key.
			return fmt.Errorf("an api-keys entry must be a string or an object")
		}
		*k = rawKey(p)
		return nil
	default:
		return fmt.Errorf("an api-keys entry must be a string or an object")
	}
}

func (k rawKey) account() Account {
	runtime := firstNonEmpty(k.RuntimeKey, k.Value, k.Key, k.APIKey)
	return Account{
		Label:      strings.TrimSpace(os.ExpandEnv(k.Label)),
		RuntimeKey: os.ExpandEnv(strings.TrimSpace(runtime)),
		ControlKey: os.ExpandEnv(strings.TrimSpace(firstNonEmpty(k.ControlKey, k.Control))),
		Email:      os.ExpandEnv(strings.TrimSpace(k.Email)),
		UID:        os.ExpandEnv(strings.TrimSpace(k.UID)),
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// Load decodes YAML, expands ${VAR} references in credentials only, applies
// defaults, and validates.
func Load(yamlBytes []byte) (Config, error) {
	return load(yamlBytes, false)
}

// LoadForRegistration is Load for plugin.register: an empty credential set is
// accepted and marked Pending instead of failing registration, so the host
// publishes the plugin and the Management Center can render its config form.
// Every other validation still applies.
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

	cfg := Config{
		BaseURL:          DefaultBaseURL,
		ControlURL:       DefaultControlURL,
		ModelPrefix:      ModelPrefix{Enabled: true, Value: DefaultModelPrefix},
		RequestTimeout:   DefaultRequestTimeout,
		MaxResponseBytes: DefaultMaxResponseBytes,
		Pool:             Pool{Strategy: DefaultPoolStrategy},
		AllowHTTP:        raw.AllowHTTP,
	}
	if raw.BaseURL != nil {
		cfg.BaseURL = strings.TrimSpace(*raw.BaseURL)
	}
	if raw.ControlURL != nil {
		cfg.ControlURL = strings.TrimSpace(*raw.ControlURL)
	}
	if raw.ModelPrefix.Enabled != nil {
		cfg.ModelPrefix.Enabled = *raw.ModelPrefix.Enabled
	}
	if raw.ModelPrefix.Value != nil {
		cfg.ModelPrefix.Value = strings.TrimSpace(*raw.ModelPrefix.Value)
	}
	if cfg.ModelPrefix.Enabled && cfg.ModelPrefix.Value == "" {
		return Config{}, fmt.Errorf("model-prefix.value must not be empty when the prefix is enabled")
	}
	if raw.Pool.Strategy != "" {
		cfg.Pool.Strategy = strings.ToLower(strings.TrimSpace(raw.Pool.Strategy))
	}
	if cfg.Pool.Strategy != "sticky" && cfg.Pool.Strategy != "round-robin" {
		return Config{}, fmt.Errorf("pool.strategy must be sticky or round-robin")
	}
	if raw.Pool.MaxConcurrencyPerAccount != nil {
		if *raw.Pool.MaxConcurrencyPerAccount < 0 {
			return Config{}, fmt.Errorf("pool.max-concurrency-per-account must not be negative")
		}
		cfg.Pool.MaxConcurrencyPerAccount = *raw.Pool.MaxConcurrencyPerAccount
	}
	for _, a := range raw.Accounts {
		cfg.Accounts = append(cfg.Accounts, a.account())
	}
	for _, k := range raw.APIKeys {
		cfg.APIKeys = append(cfg.APIKeys, k.account())
	}
	cfg.Models = ModelFilter{
		Allow: trimAll(raw.Models.Allow),
		Deny:  trimAll(raw.Models.Deny),
	}
	if raw.RequestTimeout != nil {
		d, err := time.ParseDuration(strings.TrimSpace(*raw.RequestTimeout))
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("request-timeout must be a positive duration, e.g. 5m")
		}
		cfg.RequestTimeout = d
		cfg.TimeoutSet = true
	}
	if raw.MaxResponseBytes != nil {
		if *raw.MaxResponseBytes <= 0 {
			return Config{}, fmt.Errorf("max-response-bytes must be positive")
		}
		cfg.MaxResponseBytes = *raw.MaxResponseBytes
	}

	for _, u := range []struct {
		name  string
		value string
	}{{"base-url", cfg.BaseURL}, {"control-url", cfg.ControlURL}} {
		if err := validateURL(u.name, u.value, cfg.AllowHTTP); err != nil {
			return Config{}, err
		}
	}
	for i, a := range cfg.Accounts {
		if err := validateCredential(fmt.Sprintf("accounts[%d]", i), a); err != nil {
			return Config{}, err
		}
	}
	for i, a := range cfg.APIKeys {
		if err := validateCredential(fmt.Sprintf("api-keys[%d]", i), a); err != nil {
			return Config{}, err
		}
	}

	cfg.Pending = len(cfg.EffectiveAccounts()) == 0
	if cfg.Pending && !allowPending {
		return Config{}, fmt.Errorf("api-keys: at least one key is required")
	}
	return cfg, nil
}

// validateCredential enforces the vendor's two key prefixes. A runtime key is
// what the model plane takes; a control key alone cannot serve a model request,
// so an entry carrying only a control key is refused rather than silently
// contributing nothing.
func validateCredential(where string, a Account) error {
	if a.RuntimeKey == "" {
		return fmt.Errorf("%s: a runtime key (sk-amr-…) is required", where)
	}
	if strings.HasPrefix(a.RuntimeKey, "vela_ctrl_") {
		return fmt.Errorf("%s: that is a control key (vela_ctrl-…), not the runtime key (sk-amr-…) a model request takes", where)
	}
	if strings.ContainsAny(a.RuntimeKey, " \t\r\n") {
		return fmt.Errorf("%s: the key must not contain whitespace", where)
	}
	if a.ControlKey != "" && !strings.HasPrefix(a.ControlKey, "vela_ctrl_") {
		return fmt.Errorf("%s: control-key must start with vela_ctrl-", where)
	}
	return nil
}

func validateURL(name, raw string, allowHTTP bool) error {
	if raw == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL", name)
	}
	if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
		return fmt.Errorf("%s must use https (set allow-http: true for local testing only)", name)
	}
	if u.User != nil {
		return fmt.Errorf("%s must not carry credentials in the URL", name)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s must not carry a query or a fragment", name)
	}
	if strings.TrimSpace(u.Host) == "" {
		return fmt.Errorf("%s must name a host", name)
	}
	return nil
}

func trimAll(list []string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		if v := strings.TrimSpace(s); v != "" {
			out = append(out, v)
		}
	}
	return out
}
