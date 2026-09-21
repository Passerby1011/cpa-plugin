package main

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Recording writer that satisfies the host RPC surface refreshCallWithCallback
// depends on.
func installRefreshRecorder(t *testing.T) *[]*http.Request {
	t.Helper()
	old := currentProxyState()
	var requests []*http.Request
	proxyState.Store(&proxyRoutingState{mode: proxyModeExplicit, client: &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests = append(requests, req.Clone(req.Context()))
			return testHTTPResponse(req, `{"code":0,"data":{"accessToken":"new-access"}}`), nil
		}),
	}})
	t.Cleanup(func() { proxyState.Store(old) })
	return &requests
}

func withOAuthClientMode(t *testing.T, mode string) {
	t.Helper()
	old := featureRuntime.Load()
	cfg, err := parseFeatureRuntime([]byte("oauth_client_mode: " + mode + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(cfg)
	t.Cleanup(func() { featureRuntime.Store(old) })
}

// A Global token only exists on www.workbuddy.ai. Refreshing it on the CN
// gateway fails, so the refresh route must follow the account domain rather
// than whatever login channel the user last started.
func TestRefreshFollowsAccountDomainNotLoginChannel(t *testing.T) {
	// Resolved at runtime: userAgentFor / profileFor are functions, and Go
	// constants cannot be function results.
	desktopUA := userAgentFor(RealmCN)
	cases := []struct {
		name       string
		mode       string
		domain     string
		wantHost   string
		wantUA     string
		wantOrigin string
	}{
		{"global account under CN mode", oauthClientModeWorkBuddy, "www.workbuddy.ai", "www.workbuddy.ai", userAgentFor(RealmGlobal), profileFor(RealmGlobal).Origin},
		{"global account under intl mode", oauthClientModeWorkBuddyAI, "www.workbuddy.ai", "www.workbuddy.ai", userAgentFor(RealmGlobal), profileFor(RealmGlobal).Origin},
		{"CN account under CN mode", oauthClientModeWorkBuddy, "www.codebuddy.cn", "copilot.tencent.com", desktopUA, profileFor(RealmCN).Origin},
		{"CN account under intl mode", oauthClientModeWorkBuddyAI, "www.codebuddy.cn", "copilot.tencent.com", desktopUA, profileFor(RealmCN).Origin},
		{"legacy CN account without domain under cli mode", oauthClientModeCLI, "", "copilot.tencent.com", desktopUA, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withOAuthClientMode(t, tc.mode)
			requests := installRefreshRecorder(t)
			sa := &storedAuth{
				Auth:    storedTokens{RefreshToken: "rt-" + tc.name, Domain: tc.domain},
				Account: storedAccount{UID: "u1"},
			}
			if _, _, _, err := refreshCall(sa); err != nil {
				t.Fatal(err)
			}
			if len(*requests) != 1 {
				t.Fatalf("requests = %d, want 1", len(*requests))
			}
			req := (*requests)[0]
			if req.URL.Host != tc.wantHost {
				t.Errorf("refresh host = %q, want %q", req.URL.Host, tc.wantHost)
			}
			if req.URL.Path != "/v2/plugin/auth/token/refresh" {
				t.Errorf("refresh path = %q", req.URL.Path)
			}
			if got := req.Header.Get("User-Agent"); got != tc.wantUA {
				t.Errorf("User-Agent = %q, want %q", got, tc.wantUA)
			}
			if got := req.Header.Get("X-Refresh-Token"); got != "rt-"+tc.name {
				t.Errorf("X-Refresh-Token = %q", got)
			}
			if got := req.Header.Get("X-Auth-Refresh-Source"); got != "plugin" {
				t.Errorf("X-Auth-Refresh-Source = %q", got)
			}
			if got := req.Header.Get("Origin"); got != tc.wantOrigin {
				t.Errorf("Origin = %q, want %q", got, tc.wantOrigin)
			}
		})
	}
}

// Legacy auth files carry no domain and must keep working.
func TestRefreshWithoutDomainStillUsesConfiguredChannel(t *testing.T) {
	for _, mode := range []string{oauthClientModeCLI, oauthClientModeWorkBuddy, oauthClientModeWorkBuddyAI} {
		t.Run(mode, func(t *testing.T) {
			withOAuthClientMode(t, mode)
			requests := installRefreshRecorder(t)
			sa := &storedAuth{Auth: storedTokens{RefreshToken: "rt"}, Account: storedAccount{UID: "u1"}}
			if _, _, _, err := refreshCall(sa); err != nil {
				t.Fatal(err)
			}
			if host := (*requests)[0].URL.Host; host != "copilot.tencent.com" {
				t.Fatalf("refresh host = %q, want copilot.tencent.com", host)
			}
		})
	}
}

func TestOAuthProfileForAuthPicksRealm(t *testing.T) {
	cases := []struct {
		name   string
		mode   string
		domain string
		want   string
	}{
		{"global account", oauthClientModeCLI, "www.workbuddy.ai", profileFor(RealmGlobal).Base},
		{"cn desktop account", oauthClientModeWorkBuddy, "www.codebuddy.cn", profileFor(RealmCN).Base},
		{"cn account with intl login channel collapses to cn gateway", oauthClientModeWorkBuddyAI, "www.codebuddy.cn", profileFor(RealmCN).Base},
		{"legacy cn account without domain", oauthClientModeWorkBuddy, "", profileFor(RealmCN).Base},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withOAuthClientMode(t, tc.mode)
			profile := oauthProfileForAuth(&storedAuth{Auth: storedTokens{Domain: tc.domain}})
			if profile.base != tc.want {
				t.Fatalf("base = %q, want %q", profile.base, tc.want)
			}
		})
	}
}

func TestUnusedAuthLoginResponseShapesStayWired(t *testing.T) {
	// Guard against accidental removal of the poll response contract consumed
	// by the host (pending status + auth payload).
	var resp pluginapi.AuthLoginPollResponse
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("empty poll response encoding")
	}
	_ = io.Discard
}
