package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaultsAreUsable(t *testing.T) {
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load(nil): %v", err)
	}
	if cfg.DataDir == "" {
		t.Fatal("data-dir did not default to anything")
	}
	if cfg.RequestTimeout != 30*time.Minute {
		t.Fatalf("request-timeout = %s, want 30m", cfg.RequestTimeout)
	}
	if cfg.StreamIdleTimeout != DefaultStreamIdleTimeout {
		t.Fatalf("stream-idle-timeout = %s", cfg.StreamIdleTimeout)
	}
	if cfg.AIStudio.Managed() == false {
		t.Fatal("an empty config must mean a plugin-managed backend")
	}
	if len(cfg.Namespaces) != 1 || cfg.Namespaces[0] != NamespaceAIStudio {
		t.Fatalf("namespaces = %v", cfg.Namespaces)
	}
}

// TestStreamIdleTimeoutIsIndependentOfRequestTimeout is the acceptance
// criterion "流空闲超时独立可配": the two budgets move separately.
func TestStreamIdleTimeoutIsIndependentOfRequestTimeout(t *testing.T) {
	cfg, err := Load([]byte(`
request-timeout: 45m
stream-idle-timeout: 90s
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RequestTimeout != 45*time.Minute {
		t.Fatalf("request-timeout = %s", cfg.RequestTimeout)
	}
	if cfg.StreamIdleTimeout != 90*time.Second {
		t.Fatalf("stream-idle-timeout = %s, the two budgets must not be coupled", cfg.StreamIdleTimeout)
	}
}

func TestLoadRejectsBadDuration(t *testing.T) {
	if _, err := Load([]byte("request-timeout: soon\n")); err == nil {
		t.Fatal("a non-duration request-timeout was accepted")
	}
	if _, err := Load([]byte("stream-idle-timeout: 1s\n")); err == nil {
		t.Fatal("a sub-5s stream idle budget was accepted")
	}
}

func TestLoadRejectsTooShortRequestTimeout(t *testing.T) {
	if _, err := Load([]byte("request-timeout: 5s\n")); err == nil {
		t.Fatal("a 5s request-timeout was accepted")
	}
}

func TestLoadParsesExternalBackend(t *testing.T) {
	cfg, err := Load([]byte(`
ai-studio:
  base-url: http://127.0.0.1:3000/v1/
  api-key: external-key
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AIStudio.Managed() {
		t.Fatal("an external base-url must disable management")
	}
	if strings.HasSuffix(cfg.AIStudio.BaseURL, "/") {
		t.Fatalf("base-url kept a trailing slash: %q", cfg.AIStudio.BaseURL)
	}
}

// TestLoadRequiresAnAPIKeyForAnExternalBackend: an external backend has its own
// key, and without it every call would 401 with no clue why.
func TestLoadRequiresAnAPIKeyForAnExternalBackend(t *testing.T) {
	_, err := Load([]byte("ai-studio:\n  base-url: http://127.0.0.1:3000/v1\n"))
	if err == nil {
		t.Fatal("an external base-url without api-key was accepted")
	}
	if !strings.Contains(err.Error(), "api-key") {
		t.Fatalf("error does not name the missing key: %v", err)
	}
}

// TestLoadRefusesAnAPIKeyForAManagedBackend: the managed backend generates its
// own key, so accepting one here would silently mislead.
func TestLoadRefusesAnAPIKeyForAManagedBackend(t *testing.T) {
	_, err := Load([]byte("ai-studio:\n  api-key: mine\n"))
	if err == nil {
		t.Fatal("api-key without base-url was accepted")
	}
	if !strings.Contains(err.Error(), "base-url") {
		t.Fatalf("error does not explain the pairing rule: %v", err)
	}
}

// TestLoadRefusesPlainHTTPOffLoopback: the backend holds Google cookies, so a
// remote plaintext origin would expose them.
func TestLoadRefusesPlainHTTPOffLoopback(t *testing.T) {
	_, err := Load([]byte("ai-studio:\n  base-url: http://backend.example.com/v1\n  api-key: k\n"))
	if err == nil {
		t.Fatal("a plain-http non-loopback base-url was accepted")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("error does not explain the loopback rule: %v", err)
	}
}

func TestLoadAcceptsHTTPSOffLoopback(t *testing.T) {
	if _, err := Load([]byte("ai-studio:\n  base-url: https://backend.example.com/v1\n  api-key: k\n")); err != nil {
		t.Fatalf("an https remote base-url was refused: %v", err)
	}
}

func TestLoadRejectsBaseURLWithQueryOrUserinfo(t *testing.T) {
	for _, bad := range []string{
		"http://127.0.0.1:3000/v1?x=1",
		"http://user:pass@127.0.0.1:3000/v1",
		"http://127.0.0.1:3000/v1#frag",
	} {
		if _, err := Load([]byte("ai-studio:\n  base-url: " + bad + "\n  api-key: k\n")); err == nil {
			t.Fatalf("base-url %q was accepted", bad)
		}
	}
}

func TestLoadValidatesProxySchemes(t *testing.T) {
	for _, good := range []string{"http://127.0.0.1:7890", "https://p:443", "socks5://127.0.0.1:1080", "socks5h://p:1080"} {
		if _, err := Load([]byte("proxy: " + good + "\n")); err != nil {
			t.Fatalf("proxy %q was refused: %v", good, err)
		}
	}
	for _, bad := range []string{"ftp://p:21", "127.0.0.1:7890", "http://p:1\nhttp://q:2"} {
		if _, err := Load([]byte("proxy: \"" + strings.ReplaceAll(bad, "\n", "\\n") + "\"\n")); err == nil {
			t.Fatalf("proxy %q was accepted", bad)
		}
	}
}

// TestLoadRefusesPortWithExternalBackend: the port belongs to the managed
// backend; setting it for an external one is a configuration mistake.
func TestLoadRefusesPortWithExternalBackend(t *testing.T) {
	_, err := Load([]byte("ai-studio:\n  base-url: http://127.0.0.1:3000/v1\n  api-key: k\n  port: 2048\n"))
	if err == nil {
		t.Fatal("port alongside base-url was accepted")
	}
}

func TestLoadValidatesLimits(t *testing.T) {
	if _, err := Load([]byte("ai-studio:\n  context-limit: 100\n  output-limit: 200\n")); err == nil {
		t.Fatal("output-limit greater than context-limit was accepted")
	}
	if _, err := Load([]byte("ai-studio:\n  context-limit: -1\n")); err == nil {
		t.Fatal("a negative context-limit was accepted")
	}
}

func TestLoadParsesModelFilter(t *testing.T) {
	cfg, err := Load([]byte(`
models:
  allow:
    - "ai-studio/gemini-2.5-pro"
    - "  "
  deny:
    - "*-flash"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Models.Allow) != 1 {
		t.Fatalf("allow = %v, blank entries must be dropped", cfg.Models.Allow)
	}
	if !cfg.Models.Excludes("gemini-2.5-flash", "ai-studio/gemini-2.5-flash") {
		t.Fatal("the deny pattern did not match")
	}
}

// --- filter semantics ---

func TestFilterDenyAlwaysWins(t *testing.T) {
	f := ModelFilter{Allow: []string{"gemini-*"}, Deny: []string{"gemini-2.5-flash"}}
	if !f.Excludes("gemini-2.5-flash", "ai-studio/gemini-2.5-flash") {
		t.Fatal("an allowed AND denied model was not excluded")
	}
	if f.Excludes("gemini-2.5-pro", "ai-studio/gemini-2.5-pro") {
		t.Fatal("an allowed model was excluded")
	}
}

func TestFilterEmptyKeepsEverything(t *testing.T) {
	var f ModelFilter
	if f.Excludes("anything", "ai-studio/anything") {
		t.Fatal("an empty filter excluded a model")
	}
	if !f.IsEmpty() {
		t.Fatal("IsEmpty reported false for an empty filter")
	}
}

func TestFilterMatchesPublicID(t *testing.T) {
	f := ModelFilter{Allow: []string{"ai-studio/gemini-2.5-pro"}}
	if f.Excludes("gemini-2.5-pro", "ai-studio/gemini-2.5-pro") {
		t.Fatal("an allow entry naming the public id did not match")
	}
}

func TestFilterExactEntriesAreCaseSensitive(t *testing.T) {
	f := ModelFilter{Deny: []string{"gemini-2.5-pro"}}
	if !f.Excludes("gemini-2.5-pro", "ai-studio/gemini-2.5-pro") {
		t.Fatal("an exact deny did not match")
	}
	if f.Excludes("Gemini-2.5-Pro", "ai-studio/Gemini-2.5-Pro") {
		t.Fatal("an exact (non-wildcard) entry matched with different case")
	}
}

func TestFilterWildcardIsCaseInsensitive(t *testing.T) {
	f := ModelFilter{Deny: []string{"Gemini-*-flash"}}
	if !f.Excludes("gemini-2.5-flash", "ai-studio/gemini-2.5-flash") {
		t.Fatal("a wildcard entry did not match case-insensitively")
	}
}

func TestGlobMatchSemantics(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"*", "", true},
		{"*", "anything", true},
		{"a*b", "ab", true},
		{"a*b", "axxxb", true},
		{"a*b", "axxx", false},
		{"*-flash", "gemini-2.5-flash", true},
		{"*-flash", "gemini-2.5-pro", false},
		{"exact", "exact", true},
		{"exact", "Exact", false},
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.name); got != tc.want {
			t.Fatalf("globMatch(%q, %q) = %t, want %t", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestValidateProxyRejectsNewlines(t *testing.T) {
	if _, err := ValidateProxy("http://p:1\nX-Injected: 1"); err == nil {
		t.Fatal("a proxy URL containing a newline was accepted (header injection)")
	}
}

func TestValidateProxyAcceptsEmpty(t *testing.T) {
	got, err := ValidateProxy("   ")
	if err != nil || got != "" {
		t.Fatalf("empty proxy = %q, %v", got, err)
	}
}

func TestNormalizeBaseURLStripsTrailingSlash(t *testing.T) {
	got, err := NormalizeBaseURL("http://127.0.0.1:2048/v1///")
	if err != nil {
		t.Fatalf("NormalizeBaseURL: %v", err)
	}
	if got != "http://127.0.0.1:2048/v1" {
		t.Fatalf("got %q", got)
	}
}
