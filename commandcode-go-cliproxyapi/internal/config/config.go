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

// DefaultDeviceProjectDir mirrors gocli.DefaultProjectDir. It is duplicated
// here rather than imported: config is a dependency of the adapter chain
// (catalog -> config, chatcompletions -> catalog), so importing gocli would
// create a cycle. The values are pinned equal by TestDeviceProjectDirMatchesGocli.
const DefaultDeviceProjectDir = `C:\Users\dev\projects\app`

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
	// Static is a model-id list served when the live /models catalog cannot
	// be fetched — notably when the pool has no provider-mode account, since
	// the Provider API (and therefore /models) refuses Go-plan keys. Empty
	// means no static fallback.
	Static []string
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
	Accounts         []Account
	Pool             Pool
	Catalog          Catalog
	Protocols        Protocols
	RouteOverrides   map[string]RouteOverride
	Models           ModelFilter
	Device           Device
	AllowHTTP        bool
	RequestTimeout   time.Duration
	MaxResponseBytes int64
	// Pending is true when no usable credential is configured. Such a config
	// is VALID to register with (so the host publishes the metadata and the
	// Management Center can show its config form), but the plugin serves no
	// models and refuses execution until a key is supplied.
	Pending bool
}

// Device controls the fabricated device identity a go-cli request presents.
//
// Why this is configurable at all: CommandCode's upstream risk controls expect
// requests to come from the vendor CLI running on a real machine. One pool of
// credentials all announcing the same anonymous host is a pattern; a stable,
// per-credential device is what a real deployment looks like. The reference
// implementation (MAXeaglet/commandcode-proxy) established the derivation this
// plugin reproduces.
type Device struct {
	// Enabled turns the identity layer on. It defaults to true: the whole
	// point of the go-cli transport is to look like a client the upstream
	// expects, and an identity-less client is the anomaly.
	Enabled bool
	// ProjectDir is the fabricated working directory. It feeds both the
	// envelope's config.workingDir and the x-project-slug header, so the two
	// can never disagree about which machine this is.
	ProjectDir string
	// IdentitySalt shifts which fake machine a credential maps to. Empty is
	// the reference's default. It is the escape hatch for a credential whose
	// device has been flagged: changing the salt is allowed, but it is
	// deliberately a separate knob from the API key.
	IdentitySalt string
}

// EffectiveProjectDir returns the fabricated project directory, falling back to
// the gocli package's default so a zero-value config still produces the same
// identity the reference does.
func (d Device) EffectiveProjectDir() string {
	if strings.TrimSpace(d.ProjectDir) != "" {
		return d.ProjectDir
	}
	return DefaultDeviceProjectDir
}

// TransportMode selects which upstream surface an account talks to.
//
//	provider -> {base-url}/chat/completions etc. Documented Provider API.
//	            Available to GOAT / Pro / Max / Team / Provider plans.
//	go-cli   -> {base-url-root}/alpha/generate, the CLI's own envelope.
//	            The only surface a Go-plan credential can use, because Go is
//	            the one plan the Provider API refuses ("upgrade_required").
type TransportMode string

const (
	// TransportProvider is the documented OpenAI/Anthropic-compatible surface.
	TransportProvider TransportMode = "provider"
	// TransportGoCLI is the CLI transport used by Go-plan credentials.
	TransportGoCLI TransportMode = "go-cli"
)

// Valid reports whether the mode is one this plugin implements.
func (m TransportMode) Valid() bool {
	return m == TransportProvider || m == TransportGoCLI
}

// Account is one credential plus the surface it must be sent to. A pool of
// these replaces the old flat api-keys list: Go and Provider credentials need
// different upstream envelopes, so the mode cannot be inferred from the key
// alone and has to be declared.
type Account struct {
	// Label is display-only, shown on the quota page and in logs.
	Label string
	// Mode selects the upstream transport. Empty defaults to provider.
	Mode TransportMode
	// Credential is the CommandCode API key (a "user_..." string).
	Credential string
	// Disabled keeps the entry in config but out of rotation.
	Disabled bool
}

// Pool controls how accounts are chosen per request.
type Pool struct {
	// Strategy is "sticky" (prefer the last healthy account) or
	// "round-robin". Empty defaults to sticky.
	Strategy string
	// MaxConcurrencyPerAccount caps in-flight requests per credential.
	// Zero means unlimited. The reference implementations default to one
	// in-flight request per account, which also keeps a single credential
	// from looking like a burst to upstream risk controls.
	MaxConcurrencyPerAccount int
}

const (
	PoolStrategySticky     = "sticky"
	PoolStrategyRoundRobin = "round-robin"
)

// EffectiveAccounts returns the accounts eligible for selection.
//
// This is the single normalization point for credentials: explicit accounts
// come first, then any legacy flat api-keys entries are folded in as
// provider-mode accounts. Doing it here (rather than only at parse time) keeps
// the runtime correct for a Config built programmatically — tests, or a future
// caller that constructs one directly.
func (c Config) EffectiveAccounts() []Account {
	out := make([]Account, 0, len(c.Accounts)+len(c.APIKeys))
	seen := make(map[string]struct{}, len(c.Accounts)+len(c.APIKeys))

	for _, a := range c.Accounts {
		cred := strings.TrimSpace(a.Credential)
		if cred == "" {
			continue
		}
		// A disabled entry is remembered so the same credential cannot sneak
		// back in through the legacy api-keys list.
		seen[cred] = struct{}{}
		if a.Disabled {
			continue
		}
		if a.Mode == "" {
			a.Mode = TransportProvider
		}
		a.Credential = cred
		out = append(out, a)
	}
	for _, k := range c.APIKeys {
		cred := strings.TrimSpace(k.Value)
		if cred == "" {
			continue
		}
		if _, dup := seen[cred]; dup {
			continue
		}
		seen[cred] = struct{}{}
		out = append(out, Account{Mode: TransportProvider, Credential: cred})
	}
	return out
}

type rawModelFilter struct {
	Allow []string `yaml:"allow"`
	Deny  []string `yaml:"deny"`
}

// rawDevice mirrors the device config block. Enabled is a pointer so "unset"
// (apply the default, which is on) is distinguishable from an explicit false.
type rawDevice struct {
	Enabled      *bool   `yaml:"enabled"`
	ProjectDir   *string `yaml:"project-dir"`
	IdentitySalt *string `yaml:"identity-salt"`
}

// rawConfig mirrors the YAML shape; pointer fields distinguish "unset"
// (apply default) from explicitly-set values including "" (validate as-is).
// Unknown fields are ignored (host may pass extra keys).
type rawConfig struct {
	BaseURL          *string                  `yaml:"base-url"`
	CatalogURL       *string                  `yaml:"catalog-url"`
	ModelPrefix      rawPrefix                `yaml:"model-prefix"`
	APIKeys          []rawKey                 `yaml:"api-keys"`
	Accounts         []rawAccount             `yaml:"accounts"`
	Pool             rawPool                  `yaml:"pool"`
	Catalog          rawCatalog               `yaml:"catalog"`
	Protocols        rawProtocols             `yaml:"protocols"`
	RouteOverrides   map[string]RouteOverride `yaml:"route-overrides"`
	Models           rawModelFilter           `yaml:"models"`
	Device           rawDevice                `yaml:"device"`
	AllowHTTP        bool                     `yaml:"allow-http"`
	RequestTimeout   *string                  `yaml:"request-timeout"`
	MaxResponseBytes *int64                   `yaml:"max-response-bytes"`
}

// rawAccount is one pool entry. Like rawKey it accepts a bare credential
// string, so a hand-written `accounts: ["user_xxx"]` works as well as the
// full object form the Management Center produces.
type rawAccount struct {
	Label      string        `yaml:"label"`
	Mode       TransportMode `yaml:"mode"`
	Credential string        `yaml:"credential"`
	Value      string        `yaml:"value"`
	Key        string        `yaml:"key"`
	APIKey     string        `yaml:"api_key"`
	Disabled   bool          `yaml:"disabled"`
}

// UnmarshalYAML accepts a scalar (the credential itself) or a mapping.
func (a *rawAccount) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		if node.Tag != "!!str" {
			return fmt.Errorf("accounts entry must be a string or an object")
		}
		a.Credential = node.Value
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("accounts entry must be a string or an object")
	}
	type plain rawAccount
	var p plain
	if err := node.Decode(&p); err != nil {
		return fmt.Errorf("accounts entry must be a string or an object")
	}
	*a = rawAccount(p)
	return nil
}

// credential resolves whichever key spelling was used, in a fixed precedence.
func (a rawAccount) resolveCredential() string {
	for _, candidate := range []string{a.Credential, a.Value, a.Key, a.APIKey} {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}
	return ""
}

type rawPool struct {
	Strategy                 string `yaml:"strategy"`
	MaxConcurrencyPerAccount *int   `yaml:"max-concurrency-per-account"`
}

type rawPrefix struct {
	Enabled *bool   `yaml:"enabled"`
	Value   *string `yaml:"value"`
}

// rawKey tolerates both shapes a key entry can arrive in:
//
//	api-keys:
//	  - value: "cc_xxx"     # the documented object form
//	  - "cc_xxx"            # the bare string the Management Center's array
//	                        # field actually produces when you type a key
//
// The panel renders api-keys as a generic JSON array, so the natural thing to
// type is a plain string. Accepting only the object form made saving a key
// fail reconfigure with "invalid YAML structure", which the host renders as
// "not registered / not effective" — i.e. the plugin looked broken right after
// the user did exactly what the UI asked.
type rawKey struct {
	Value string `yaml:"value"`
}

// UnmarshalYAML accepts a scalar (the key itself) or a mapping with a value
// field, and also tolerates the other common spellings for that field.
func (k *rawKey) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag != "!!str" {
			return fmt.Errorf("api-keys entry must be a string or a {value: ...} object")
		}
		k.Value = node.Value
		return nil
	case yaml.MappingNode:
		var obj struct {
			Value string `yaml:"value"`
			Key   string `yaml:"key"`
		}
		if err := node.Decode(&obj); err != nil {
			return fmt.Errorf("api-keys entry must be a string or a {value: ...} object")
		}
		if strings.TrimSpace(obj.Value) != "" {
			k.Value = obj.Value
			return nil
		}
		k.Value = obj.Key
		return nil
	default:
		return fmt.Errorf("api-keys entry must be a string or a {value: ...} object")
	}
}

type rawCatalog struct {
	RefreshInterval       *string   `yaml:"refresh-interval"`
	StaleWhileUnavailable *bool     `yaml:"stale-while-unavailable"`
	Static                *[]string `yaml:"static"`
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
	accounts := make([]Account, 0, len(raw.Accounts)+len(keys))
	for _, a := range raw.Accounts {
		credential := os.ExpandEnv(a.resolveCredential())
		mode := a.Mode
		if mode == "" {
			mode = TransportProvider
		}
		accounts = append(accounts, Account{
			Label:      strings.TrimSpace(a.Label),
			Mode:       mode,
			Credential: credential,
			Disabled:   a.Disabled,
		})
	}
	// Legacy flat api-keys entries fold into provider-mode accounts, so an
	// existing config keeps working without being rewritten. They come after
	// any explicit accounts, and a credential already listed as an account is
	// not duplicated.
	seenCredential := make(map[string]struct{}, len(accounts))
	for _, a := range accounts {
		if a.Credential != "" {
			seenCredential[a.Credential] = struct{}{}
		}
	}
	for _, k := range keys {
		if k.Value == "" {
			continue
		}
		if _, dup := seenCredential[k.Value]; dup {
			continue
		}
		seenCredential[k.Value] = struct{}{}
		accounts = append(accounts, Account{Mode: TransportProvider, Credential: k.Value})
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
		APIKeys:  keys,
		Accounts: accounts,
		Pool: Pool{
			Strategy:                 poolStrategy(raw.Pool.Strategy),
			MaxConcurrencyPerAccount: orDefault(raw.Pool.MaxConcurrencyPerAccount, 0),
		},
		Catalog: Catalog{
			RefreshInterval:       refreshInterval,
			StaleWhileUnavailable: orDefault(raw.Catalog.StaleWhileUnavailable, true),
			Static:                staticModelList(raw.Catalog.Static),
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
		Device: Device{
			Enabled:      orDefault(raw.Device.Enabled, true),
			ProjectDir:   strings.TrimSpace(orDefault(raw.Device.ProjectDir, "")),
			IdentitySalt: orDefault(raw.Device.IdentitySalt, ""),
		},
		AllowHTTP:        raw.AllowHTTP,
		RequestTimeout:   requestTimeout,
		MaxResponseBytes: orDefault(raw.MaxResponseBytes, DefaultMaxResponseBytes),
	}
	c.Pending = allowPending && len(c.EffectiveAccounts()) == 0
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
// when the caller allowed an empty credential list.
func (c Config) validateForLoad(allowPending bool) error {
	if allowPending && len(c.EffectiveAccounts()) == 0 {
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
	// Per-entry checks run BEFORE the presence check so a specific problem
	// (a key that expanded to empty, a duplicate) reports its own message and
	// index instead of the generic "no credential configured".
	if err := c.validateCommon(); err != nil {
		return err
	}
	if len(c.EffectiveAccounts()) == 0 {
		return fmt.Errorf("accounts: at least one credential is required")
	}
	return nil
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

// poolStrategy normalizes the strategy name, defaulting to sticky.
func poolStrategy(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case PoolStrategyRoundRobin:
		return PoolStrategyRoundRobin
	default:
		return PoolStrategySticky
	}
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

// staticModelList normalizes the configured static catalog model ids: trims,
// drops empties and exact duplicates, keeping first-seen order. Nil when the
// list is unset, so the catalog falls back to the live fetch alone.
func staticModelList(in *[]string) []string {
	if in == nil {
		return nil
	}
	return normalizeModelList(*in)
}
