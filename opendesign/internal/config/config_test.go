package config

import (
	"strings"
	"testing"
	"time"
)

// A config that says nothing gets the documented defaults, and the model plane
// and control plane are two distinct hosts (they are in reality, and a config
// that conflated them would send the device sign-in to the wrong service).
func TestDefaultsNameBothPlanes(t *testing.T) {
	cfg, err := Load([]byte("api-keys: [\"sk-amr-placeholder\"]\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BaseURL != "https://amr-link.open-design.ai/v1" {
		t.Errorf("base-url = %q", cfg.BaseURL)
	}
	if cfg.ControlURL != "https://amr-api.open-design.ai" {
		t.Errorf("control-url = %q", cfg.ControlURL)
	}
	if cfg.BaseURL == cfg.ControlURL {
		t.Errorf("the model plane and the control plane must not be the same host")
	}
	if cfg.RequestTimeout != 5*time.Minute {
		t.Errorf("request-timeout = %v", cfg.RequestTimeout)
	}
	if !cfg.ModelPrefix.Enabled || cfg.ModelPrefix.Value != "opendesign" {
		t.Errorf("model-prefix = %+v", cfg.ModelPrefix)
	}
	if cfg.Pool.Strategy != "sticky" {
		t.Errorf("pool.strategy = %q", cfg.Pool.Strategy)
	}
	if cfg.PublicID("glm-5.3-flash") != "opendesign/glm-5.3-flash" {
		t.Errorf("public id = %q", cfg.PublicID("glm-5.3-flash"))
	}
}

// allow-http keeps the meaning it has in this repository's other plugins: plain
// http is refused unless opted in, and a credential can never ride in the URL.
func TestAllowHTTPGate(t *testing.T) {
	httpCfg := "api-keys: [\"sk-amr-placeholder\"]\nbase-url: http://localhost:8080/v1\n"
	if _, err := Load([]byte(httpCfg)); err == nil {
		t.Fatalf("an http base-url must be refused without allow-http")
	}
	allowed := httpCfg + "allow-http: true\n"
	cfg, err := Load([]byte(allowed))
	if err != nil {
		t.Fatalf("allow-http: true must permit http: %v", err)
	}
	if cfg.BaseURL != "http://localhost:8080/v1" {
		t.Errorf("base-url = %q", cfg.BaseURL)
	}
	if !cfg.AllowHTTP {
		t.Errorf("allow-http not recorded")
	}

	// A URL carrying userinfo, a query or a fragment is refused even with
	// allow-http on: a credential must never be smuggled into an upstream URL.
	for _, bad := range []string{
		"https://user:pass@amr-link.open-design.ai/v1",
		"https://amr-link.open-design.ai/v1?key=abc",
		"https://amr-link.open-design.ai/v1#frag",
	} {
		body := "api-keys: [\"sk-amr-placeholder\"]\nbase-url: " + bad + "\nallow-http: true\n"
		if _, err := Load([]byte(body)); err == nil {
			t.Errorf("base-url %q must be refused", bad)
		}
	}
}

// The two key families are checked: a control key cannot serve a model request,
// and an entry carrying only one is refused rather than contributing nothing.
func TestCredentialPrefixValidation(t *testing.T) {
	// A control key in the runtime slot is the mistake this catches: it would
	// sign every model request with a key the model plane refuses.
	body := "api-keys:\n  - value: \"vela_ctrl_something\"\n"
	if _, err := Load([]byte(body)); err == nil || !strings.Contains(err.Error(), "control key") {
		t.Fatalf("a control key in the runtime slot must be refused, got %v", err)
	}
	// A control key under its own field must carry its prefix.
	badControl := "api-keys:\n  - value: \"sk-amr-placeholder\"\n    control: \"not-a-control-key\"\n"
	if _, err := Load([]byte(badControl)); err == nil {
		t.Fatalf("a control key without vela_ctrl- must be refused")
	}
	// The documented pair loads and keeps both keys.
	good := "api-keys:\n  - value: \"sk-amr-placeholder\"\n    control: \"vela_ctrl_control\"\n    label: \"work\"\n"
	cfg, err := Load([]byte(good))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	accounts := cfg.EffectiveAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d", len(accounts))
	}
	if accounts[0].RuntimeKey != "sk-amr-placeholder" || accounts[0].ControlKey != "vela_ctrl_control" {
		t.Errorf("both keys must be kept: %+v", accounts[0])
	}
	if accounts[0].Label != "work" {
		t.Errorf("label = %q", accounts[0].Label)
	}
}

// A bare scalar key is accepted, because the Management Center's array editor
// produces exactly that when a key is typed into it.
func TestBareScalarKeyLoads(t *testing.T) {
	cfg, err := Load([]byte("api-keys: [\"sk-amr-placeholder\"]\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	accounts := cfg.EffectiveAccounts()
	if len(accounts) != 1 || accounts[0].RuntimeKey != "sk-amr-placeholder" {
		t.Fatalf("accounts = %+v", accounts)
	}
	if accounts[0].ControlKey != "" {
		t.Errorf("a pasted key has no control key, got %q", accounts[0].ControlKey)
	}
}

// ${VAR} expansion reaches credentials only, and the key is trimmed.
func TestEnvExpansionInCredential(t *testing.T) {
	t.Setenv("OPENDESIGN_TEST_KEY", "sk-amr-from-env")
	cfg, err := Load([]byte("api-keys: [\"${OPENDESIGN_TEST_KEY}\"]\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	accounts := cfg.EffectiveAccounts()
	if len(accounts) != 1 || accounts[0].RuntimeKey != "sk-amr-from-env" {
		t.Fatalf("accounts = %+v", accounts)
	}
}

// Duplicate keys collapse, and an empty one is dropped rather than pooled.
func TestEffectiveAccountsDeduplicates(t *testing.T) {
	cfg := Config{
		Accounts: []Account{{RuntimeKey: "sk-amr-a", Label: "first"}},
		APIKeys:  []Account{{RuntimeKey: "sk-amr-a", Label: "dup"}, {RuntimeKey: "  "}},
	}
	accounts := cfg.EffectiveAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v", accounts)
	}
	if accounts[0].Label != "first" {
		t.Errorf("the accounts entry must win over the api-keys duplicate, label = %q", accounts[0].Label)
	}
}

// Registration tolerates an empty credential set (so the Management Center can
// render the config form) but a live config does not.
func TestPendingVersusLiveCredentialRequirement(t *testing.T) {
	if _, err := Load([]byte("base-url: https://amr-link.open-design.ai/v1\n")); err == nil {
		t.Fatalf("a live config with no credential must be refused")
	}
	cfg, err := LoadForRegistration([]byte(""))
	if err != nil {
		t.Fatalf("LoadForRegistration: %v", err)
	}
	if !cfg.Pending {
		t.Errorf("a credential-free registration config must be marked Pending")
	}
	if cfg.BaseURL != DefaultBaseURL || cfg.ControlURL != DefaultControlURL {
		t.Errorf("defaults must still apply: %+v", cfg)
	}
}

// GlobMatch is the filter primitive: '*' matches any run including none, a pattern
// without '*' is exact and case-sensitive, one with '*' is case-insensitive.
func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"claude-*", "claude-haiku-5.5", true},
		{"claude-*", "CLAUDE-Haiku-5.5", true}, // wildcard patterns fold case
		{"claude-*", "glm-5.3-flash", false},
		{"*flash", "glm-5.3-flash", true},
		{"*flash*", "glm-5.3-flashx", true},
		{"glm-*", "glm-5.3-flash", true},
		{"*", "anything", true},
		{"*", "", true},
		{"glm-5.3-flash", "glm-5.3-flash", true},
		{"glm-5.3-flash", "GLM-5.3-Flash", false}, // exact patterns stay case-sensitive
		{"glm-5.3-flash", "glm-5.3-flashx", false},
		{"deepseek-v4.1-*", "deepseek-v4.1-flash-eco", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
	}
	for _, tc := range cases {
		if got := GlobMatch(tc.pattern, tc.name); got != tc.want {
			t.Errorf("GlobMatch(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

// The filter matches the upstream id AND the prefixed public id, and deny wins
// over allow for the same model.
func TestModelFilterDenyWinsAndMatchesBothIds(t *testing.T) {
	filter := ModelFilter{Deny: []string{"opendesign/claude-*"}}
	if !filter.Excludes("claude-haiku-5.5", "opendesign/claude-haiku-5.5") {
		t.Errorf("the prefixed id must be matchable")
	}
	if filter.Excludes("glm-5.3-flash", "opendesign/glm-5.3-flash") {
		t.Errorf("an unrelated model was excluded")
	}

	both := ModelFilter{Allow: []string{"glm-*", "claude-haiku-5.5"}, Deny: []string{"claude-*"}}
	if !both.Excludes("claude-haiku-5.5", "opendesign/claude-haiku-5.5") {
		t.Errorf("deny must win over allow")
	}
	if both.Excludes("glm-5.3-flash", "opendesign/glm-5.3-flash") {
		t.Errorf("an allowed model was excluded")
	}

	// An empty allow publishes everything.
	if (ModelFilter{}).Excludes("any-model", "opendesign/any-model") {
		t.Errorf("an empty filter must exclude nothing")
	}
	// A set allow publishes only what it names.
	set := ModelFilter{Allow: []string{"glm-5.3-flash"}}
	if !set.Excludes("deepseek-v4-pro", "opendesign/deepseek-v4-pro") {
		t.Errorf("an unlisted model must be excluded when allow is set")
	}
}

// Validation failures must name the knob, so the operator knows what to fix, and
// must never echo a credential.
func TestInvalidConfigNamesTheKnobWithoutEchoingKeys(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{"api-keys: [\"sk-amr-secret-value\"]\nrequest-timeout: 0s\n", "request-timeout"},
		{"api-keys: [\"sk-amr-secret-value\"]\nrequest-timeout: nonsense\n", "request-timeout"},
		{"api-keys: [\"sk-amr-secret-value\"]\npool:\n  strategy: random\n", "pool.strategy"},
		{"api-keys: [\"sk-amr-secret-value\"]\npool:\n  max-concurrency-per-account: -1\n", "max-concurrency-per-account"},
		{"api-keys: [\"sk-amr-secret-value\"]\nmax-response-bytes: 0\n", "max-response-bytes"},
		{"api-keys: [\"sk-amr-secret-value\"]\nmodel-prefix:\n  enabled: true\n  value: \"\"\n", "model-prefix.value"},
		{"api-keys: \"not-a-list\"\n", "decode config"},
	}
	for _, tc := range cases {
		_, err := Load([]byte(tc.body))
		if err == nil {
			t.Errorf("config %q must be refused", tc.body)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("error %q must name %q", err.Error(), tc.want)
		}
		if strings.Contains(err.Error(), "secret-value") {
			t.Errorf("the error leaked the key: %q", err.Error())
		}
	}
}

// The prefix can be disabled, and then the public id is the upstream id.
func TestDisabledPrefixPublishesBareIds(t *testing.T) {
	body := "api-keys: [\"sk-amr-placeholder\"]\nmodel-prefix:\n  enabled: false\n  value: \"opendesign\"\n"
	cfg, err := Load([]byte(body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ModelPrefix.Enabled {
		t.Errorf("the prefix must be disabled")
	}
	if cfg.PublicID("glm-5.3-flash") != "glm-5.3-flash" {
		t.Errorf("public id = %q, want the bare upstream id", cfg.PublicID("glm-5.3-flash"))
	}
}

// An explicit request-timeout is recorded as set, so a caller can tell an
// operator's choice from the default.
func TestRequestTimeoutSetFlag(t *testing.T) {
	body := "api-keys: [\"sk-amr-placeholder\"]\nrequest-timeout: 90s\n"
	cfg, err := Load([]byte(body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RequestTimeout != 90*time.Second {
		t.Errorf("timeout = %v", cfg.RequestTimeout)
	}
	if !cfg.TimeoutSet {
		t.Errorf("TimeoutSet must be true for an explicit value")
	}
}
