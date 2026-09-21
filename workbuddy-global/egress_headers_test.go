package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The official client marks every backend call with X-CodeBuddy-Request: 1 and
// sends Accept-Language per realm. Both are mirrored here so the outbound shape
// stays close to the official one; neither existed before.
func TestBackendHeadersSendClientMarkerAndLanguage(t *testing.T) {
	cases := []struct {
		name     string
		domain   string
		wantLang string
	}{
		{"CN account", "www.codebuddy.cn", profileFor(RealmCN).AcceptLanguage},
		{"Global account", "www.workbuddy.ai", profileFor(RealmGlobal).AcceptLanguage},
		{"empty domain falls back to CN", "", profileFor(RealmCN).AcceptLanguage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
			backendHeaders(req, &storedAuth{Auth: storedTokens{Domain: tc.domain}})

			if got := req.Header.Get("X-CodeBuddy-Request"); got != "1" {
				t.Errorf("X-CodeBuddy-Request = %q, want %q", got, "1")
			}
			if got := req.Header.Get("Accept-Language"); got != tc.wantLang {
				t.Errorf("Accept-Language = %q, want %q", got, tc.wantLang)
			}
		})
	}
}

// The billing path builds its own headers and never calls commonHeaders, so it
// needs the same two values set explicitly.
func TestBillingHeadersSendClientMarkerAndLanguage(t *testing.T) {
	for _, tc := range []struct {
		domain   string
		wantLang string
	}{
		{"www.codebuddy.cn", profileFor(RealmCN).AcceptLanguage},
		{"www.workbuddy.ai", profileFor(RealmGlobal).AcceptLanguage},
	} {
		req := httptest.NewRequest(http.MethodGet, "https://example.com/v2/billing", nil)
		billingHeaders(req, &storedAuth{
			Auth:    storedTokens{AccessToken: "tok", Domain: tc.domain},
			Account: storedAccount{UID: "uid"},
		})

		if got := req.Header.Get("X-CodeBuddy-Request"); got != "1" {
			t.Errorf("domain %q: X-CodeBuddy-Request = %q, want %q", tc.domain, got, "1")
		}
		if got := req.Header.Get("Accept-Language"); got != tc.wantLang {
			t.Errorf("domain %q: Accept-Language = %q, want %q", tc.domain, got, tc.wantLang)
		}
	}
}

// The refresh request is assembled by hand, so it must carry the marker too —
// otherwise one leg of the account lifecycle looks different from the rest.
func TestTokenRefreshRequestSendsClientMarkerAndLanguage(t *testing.T) {
	for _, tc := range []struct {
		domain   string
		wantLang string
	}{
		{"www.codebuddy.cn", profileFor(RealmCN).AcceptLanguage},
		{"www.workbuddy.ai", profileFor(RealmGlobal).AcceptLanguage},
	} {
		req, err := buildTokenRefreshRequest(oauthRequestProfile{mode: oauthClientModeWorkBuddy},
			&storedAuth{
				Auth:    storedTokens{RefreshToken: "rt", Domain: tc.domain},
				Account: storedAccount{UID: "uid"},
			})
		if err != nil {
			t.Fatalf("buildTokenRefreshRequest: %v", err)
		}
		if got := req.Header.Get("X-CodeBuddy-Request"); got != "1" {
			t.Errorf("domain %q: X-CodeBuddy-Request = %q, want %q", tc.domain, got, "1")
		}
		if got := req.Header.Get("Accept-Language"); got != tc.wantLang {
			t.Errorf("domain %q: Accept-Language = %q, want %q", tc.domain, got, tc.wantLang)
		}
	}
}

// acceptLanguageFor must stay nil-safe: header helpers are called from paths
// where the account may not have been resolved.
func TestAcceptLanguageForNilAuth(t *testing.T) {
	if got := acceptLanguageFor(nil); got != profileFor(RealmGlobal).AcceptLanguage {
		t.Errorf("acceptLanguageFor(nil) = %q, want %q", got, profileFor(RealmGlobal).AcceptLanguage)
	}
}
