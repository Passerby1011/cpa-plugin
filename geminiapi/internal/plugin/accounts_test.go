package plugin

// Cookie handling and its secrecy boundary.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/geminiapi/internal/backend"
)

// validCookie is a syntactically complete, obviously fake Google cookie.
const validCookie = "SID=abc123; HSID=def456; SSID=ghi789; APISID=jkl012; SAPISID=mno345; __Secure-1PSID=pqr678"

func TestParseCookieHeaderAcceptsRawHeader(t *testing.T) {
	got, err := ParseCookieHeader(validCookie)
	if err != nil {
		t.Fatalf("ParseCookieHeader: %v", err)
	}
	for _, name := range requiredCookies {
		if !strings.Contains(got, name+"=") {
			t.Fatalf("normalized cookie lost %s: %s", name, got)
		}
	}
}

func TestParseCookieHeaderStripsCookieLabel(t *testing.T) {
	for _, prefix := range []string{"cookie: ", "Cookie: ", "COOKIE:"} {
		got, err := ParseCookieHeader(prefix + validCookie)
		if err != nil {
			t.Fatalf("ParseCookieHeader(%q): %v", prefix, err)
		}
		if strings.HasPrefix(strings.ToLower(got), "cookie") {
			t.Fatalf("the label survived normalization: %s", got)
		}
	}
}

func TestParseCookieHeaderAcceptsJSONExport(t *testing.T) {
	export := `[{"name":"SID","value":"abc123","domain":".google.com"},
	           {"name":"HSID","value":"def456","domain":".google.com"},
	           {"name":"SSID","value":"ghi789","domain":".google.com"},
	           {"name":"APISID","value":"jkl012","domain":".google.com"},
	           {"name":"SAPISID","value":"mno345","domain":".google.com"},
	           {"name":"__Secure-1PSID","value":"pqr678","domain":".google.com"},
	           {"name":"NID","value":"should-be-dropped","domain":".example.com"}]`
	got, err := ParseCookieHeader(export)
	if err != nil {
		t.Fatalf("ParseCookieHeader: %v", err)
	}
	if strings.Contains(got, "should-be-dropped") {
		t.Fatal("a cookie from a non-Google domain was kept")
	}
	if !strings.Contains(got, "__Secure-1PSID=pqr678") {
		t.Fatalf("normalized cookie lost __Secure-1PSID: %s", got)
	}
}

func TestParseCookieHeaderAcceptsDevToolsTable(t *testing.T) {
	table := strings.Join([]string{
		"Name\tValue\tDomain\tPath",
		"SID\tabc123\t.google.com\t/",
		"HSID\tdef456\t.google.com\t/",
		"SSID\tghi789\t.google.com\t/",
		"APISID\tjkl012\t.google.com\t/",
		"SAPISID\tmno345\t.google.com\t/",
		"__Secure-1PSID\tpqr678\t.google.com\t/",
		"irrelevant\tzzz\texample.com\t/",
	}, "\n")
	got, err := ParseCookieHeader(table)
	if err != nil {
		t.Fatalf("ParseCookieHeader: %v", err)
	}
	if strings.Contains(got, "zzz") {
		t.Fatal("a non-Google table row was kept")
	}
	if !strings.Contains(got, "SAPISID=mno345") {
		t.Fatalf("table parse lost a required cookie: %s", got)
	}
}

// TestParseCookieHeaderNamesTheMissingCookies is the operator-facing contract:
// a near-miss must say WHICH cookie is missing, not just "invalid".
func TestParseCookieHeaderNamesTheMissingCookies(t *testing.T) {
	partial := "SID=abc123; HSID=def456"
	_, err := ParseCookieHeader(partial)
	if err == nil {
		t.Fatal("an incomplete cookie was accepted")
	}
	for _, missing := range []string{"SSID", "APISID", "SAPISID", "__Secure-1PSID"} {
		if !strings.Contains(err.Error(), missing) {
			t.Fatalf("error does not name %s: %s", missing, err.Error())
		}
	}
}

func TestParseCookieHeaderRejectsEmpty(t *testing.T) {
	if _, err := ParseCookieHeader("   "); err == nil {
		t.Fatal("an empty cookie was accepted")
	}
}

func TestParseCookieHeaderRejectsGarbage(t *testing.T) {
	if _, err := ParseCookieHeader("this is not a cookie at all"); err == nil {
		t.Fatal("garbage was accepted as a cookie")
	}
}

// TestParseCookieHeaderDeduplicatesNames keeps the FIRST value: a duplicate
// later in the pasted text is usually a stale copy from another tab.
func TestParseCookieHeaderDeduplicatesNames(t *testing.T) {
	duplicated := validCookie + "; SID=stale-value"
	got, err := ParseCookieHeader(duplicated)
	if err != nil {
		t.Fatalf("ParseCookieHeader: %v", err)
	}
	if strings.Contains(got, "stale-value") {
		t.Fatal("a duplicate cookie name let a later value win")
	}
	// Count only whole "SID=" pairs (HSID/APISID also end in SID=).
	var sidCount int
	for _, pair := range strings.Split(got, "; ") {
		name, _, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		if name == "SID" {
			sidCount++
			if pair != "SID=abc123" {
				t.Fatalf("SID = %q, want the first value", pair)
			}
		}
	}
	if sidCount != 1 {
		t.Fatalf("duplicate SID survived: %s", got)
	}
}

func TestParseAuthUserAcceptsDigitsAndEmpty(t *testing.T) {
	for input, want := range map[string]int{"": 0, "0": 0, "4": 4, "9": 9, " 3 ": 3} {
		got, err := ParseAuthUser(input)
		if err != nil {
			t.Fatalf("ParseAuthUser(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("ParseAuthUser(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestParseAuthUserRejectsOutOfRange(t *testing.T) {
	for _, input := range []string{"10", "-1", "abc", "1.5"} {
		if _, err := ParseAuthUser(input); err == nil {
			t.Fatalf("ParseAuthUser accepted %q", input)
		}
	}
}

// --- the secrecy boundary ---

// TestImportNeverPutsTheCookieInAnyResponse is the acceptance criterion
// "cookie 不落日志断言": neither a success nor a failure response may carry the
// cookie, and it must not appear in a host.log payload either.
func TestImportNeverPutsTheCookieInAnyResponse(t *testing.T) {
	cases := []struct {
		name      string
		httpDo    func(hostHTTPReq) (int, http.Header, []byte, error)
		wantError bool
	}{
		{
			name: "backend accepts and probes ok",
			httpDo: func(req hostHTTPReq) (int, http.Header, []byte, error) {
				switch {
				case strings.HasSuffix(req.URL, "/admin/api/cookies"):
					return http.StatusOK, nil, []byte(`{"id":7}`), nil
				case strings.HasSuffix(req.URL, "/check"):
					return http.StatusOK, nil, []byte(`{"ok":true,"detail":"signed in"}`), nil
				}
				return http.StatusOK, nil, []byte(`{}`), nil
			},
		},
		{
			name: "backend rejects the probe",
			httpDo: func(req hostHTTPReq) (int, http.Header, []byte, error) {
				switch {
				case strings.HasSuffix(req.URL, "/admin/api/cookies"):
					return http.StatusOK, nil, []byte(`{"id":8}`), nil
				case strings.HasSuffix(req.URL, "/check"):
					return http.StatusOK, nil, []byte(`{"ok":false,"detail":"redirected to sign-in"}`), nil
				}
				return http.StatusOK, nil, []byte(`{}`), nil
			},
			wantError: true,
		},
		{
			name: "backend refuses the cookie outright",
			httpDo: func(req hostHTTPReq) (int, http.Header, []byte, error) {
				return http.StatusBadRequest, nil, []byte(`{"error":"invalid cookie"}`), nil
			},
			wantError: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := newScriptedHost()
			host.httpDo = func(req hostHTTPReq) (int, http.Header, []byte, error) {
				// The cookie must only ever travel to /admin/api/... on loopback.
				if strings.Contains(string(req.Body), "SID=") && !strings.Contains(req.URL, "127.0.0.1") {
					t.Fatalf("the cookie was sent to %s", req.URL)
				}
				return tc.httpDo(req)
			}
			m := newTestManager(t, host)
			m.mu.Lock()
			m.cfg.AIStudio.BaseURL = ""
			m.skipSupervision = false
			m.backendMgr = &backend.Manager{}
			m.resolveSetup = func(context.Context) (backend.Setup, error) {
				return backend.Setup{
					Origin:     "http://127.0.0.1:2048",
					BaseURL:    "http://127.0.0.1:2048/v1",
					APIKey:     "backend-key",
					AdminToken: "admin-token",
				}, nil
			}
			m.mu.Unlock()

			body, _ := json.Marshal(importRequest{Cookie: validCookie})
			resp, err := m.importAccountResponse(pluginapi.ManagementRequest{
				Method: http.MethodPost,
				Path:   accountsPath,
				Body:   body,
			})
			if err != nil {
				t.Fatalf("importAccountResponse: %v", err)
			}

			assertNoCookieMaterial(t, resp.Body, "management response")
			// And nothing logged may carry it either.
			for _, payload := range host.bodies["host.log"] {
				assertNoCookieMaterial(t, payload, "host.log payload")
			}
			if tc.wantError && resp.StatusCode == http.StatusOK {
				t.Fatalf("expected a failure, got %d: %s", resp.StatusCode, resp.Body)
			}
		})
	}
}

// assertNoCookieMaterial fails when any of the distinctive cookie values appear.
func assertNoCookieMaterial(t *testing.T, payload []byte, where string) {
	t.Helper()
	text := string(payload)
	for _, secretValue := range []string{"abc123", "def456", "ghi789", "jkl012", "mno345", "pqr678"} {
		if strings.Contains(text, secretValue) {
			t.Fatalf("%s leaks cookie material %q: %s", where, secretValue, truncate(text, 300))
		}
	}
	if strings.Contains(text, "__Secure-1PSID=") {
		t.Fatalf("%s leaks an entire cookie header: %s", where, truncate(text, 300))
	}
}

// TestImportRejectsIncompleteCookieBeforeTouchingTheBackend: validation happens
// first, so a half-pasted cookie never reaches the backend at all.
func TestImportRejectsIncompleteCookieBeforeTouchingTheBackend(t *testing.T) {
	host := newScriptedHost()
	host.httpDo = func(req hostHTTPReq) (int, http.Header, []byte, error) {
		t.Fatalf("the backend was called for an incomplete cookie: %s", req.URL)
		return 0, nil, nil, nil
	}
	m := newTestManager(t, host)
	m.mu.Lock()
	m.cfg.AIStudio.BaseURL = ""
	m.skipSupervision = false
	m.mu.Unlock()

	body, _ := json.Marshal(importRequest{Cookie: "SID=only-one"})
	resp, err := m.importAccountResponse(pluginapi.ManagementRequest{Method: http.MethodPost, Path: accountsPath, Body: body})
	if err != nil {
		t.Fatalf("importAccountResponse: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), "HSID") {
		t.Fatalf("the response does not name the missing cookies: %s", resp.Body)
	}
	assertNoCookieMaterial(t, resp.Body, "rejection response")
}
