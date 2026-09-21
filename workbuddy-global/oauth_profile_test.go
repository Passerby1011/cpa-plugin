package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestCLIProfilePreservesCurrentOAuthStateRequest(t *testing.T) {
	req, err := buildAuthStateRequest(oauthProfileForMode(oauthClientModeCLI))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPost || req.URL.String() != endpointAuthState {
		t.Fatalf("CLI state request = %s %s", req.Method, req.URL)
	}
	if got := req.Header.Get("User-Agent"); got != userAgentFor(RealmCN) {
		t.Fatalf("CLI User-Agent = %q, want %q", got, userAgentFor(RealmCN))
	}
	// The CLI login profile deliberately sends no Origin/Referer: only the
	// desktop profiles mirror browser decoration. Asserting emptiness pins that
	// contract so a future refactor does not silently start sending them.
	if got := req.Header.Get("Origin"); got != "" {
		t.Fatalf("CLI Origin = %q, want empty", got)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "{}" {
		t.Fatalf("CLI state body = %q, want {}", body)
	}
}

func TestWorkBuddyProfileBuildsDesktopStateRequest(t *testing.T) {
	req, err := buildAuthStateRequest(oauthProfileForMode(oauthClientModeWorkBuddy))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPost || req.URL.String() != profileFor(RealmCN).Base+pluginAuthStatePath+"?platform=workbuddy" {
		t.Fatalf("desktop state request = %s %s", req.Method, req.URL)
	}
	for key, want := range map[string]string{
		"User-Agent":           userAgentFor(RealmCN),
		"Origin":               profileFor(RealmCN).Origin,
		"Referer":              profileFor(RealmCN).Origin + "/",
		"X-No-Authorization":   "true",
		"X-No-User-Id":         "true",
		"X-No-Enterprise-Id":   "true",
		"X-No-Department-Info": "true",
	} {
		if got := req.Header.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "{}" {
		t.Fatalf("desktop state body = %q, want {}", body)
	}
}

func TestWorkBuddyAIProfileBuildsInternationalStateRequest(t *testing.T) {
	req, err := buildAuthStateRequest(oauthProfileForMode(oauthClientModeWorkBuddyAI))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPost || req.URL.String() != profileFor(RealmGlobal).Base+pluginAuthStatePath+"?platform=workbuddy-ai" {
		t.Fatalf("international state request = %s %s", req.Method, req.URL)
	}
	for key, want := range map[string]string{
		"User-Agent":           userAgentFor(RealmGlobal),
		"Origin":               profileFor(RealmGlobal).Origin,
		"Referer":              profileFor(RealmGlobal).Origin + "/",
		"X-No-Authorization":   "true",
		"X-No-User-Id":         "true",
		"X-No-Enterprise-Id":   "true",
		"X-No-Department-Info": "true",
	} {
		if got := req.Header.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// TestAuthEndpointsFollowProfileGateway pins the rule that token polling and
// the account lookup stay on the gateway that issued the state.
func TestAuthEndpointsFollowProfileGateway(t *testing.T) {
	cases := []struct {
		mode  string
		base  string
		state string
	}{
		{oauthClientModeCLI, profileFor(RealmCN).Base, profileFor(RealmCN).Base + pluginAuthStatePath + "?platform=CLI"},
		{oauthClientModeWorkBuddy, profileFor(RealmCN).Base, profileFor(RealmCN).Base + pluginAuthStatePath + "?platform=workbuddy"},
		{oauthClientModeWorkBuddyAI, profileFor(RealmGlobal).Base, profileFor(RealmGlobal).Base + pluginAuthStatePath + "?platform=workbuddy-ai"},
	}
	for _, tc := range cases {
		profile := oauthProfileForMode(tc.mode)
		if profile.stateURL != tc.state {
			t.Errorf("%s stateURL = %q, want %q", tc.mode, profile.stateURL, tc.state)
		}
		tokenReq, err := buildAuthTokenRequest(profile, "s1")
		if err != nil {
			t.Fatal(err)
		}
		if want := tc.base + pluginAuthTokenPath + "s1"; tokenReq.URL.String() != want {
			t.Errorf("%s token URL = %q, want %q", tc.mode, tokenReq.URL, want)
		}
		accountReq, err := buildLoginAccountRequest(profile, "s1", "tok")
		if err != nil {
			t.Fatal(err)
		}
		if want := tc.base + pluginLoginAccountPath + "s1"; accountReq.URL.String() != want {
			t.Errorf("%s account URL = %q, want %q", tc.mode, accountReq.URL, want)
		}
		if got := accountReq.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("%s account authorization = %q", tc.mode, got)
		}
	}
}

func TestDecorateDesktopAuthURLPreservesBrowserQuery(t *testing.T) {
	got, err := decorateDesktopAuthURL("https://example.test/login?state=s", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("state") != "s" || u.Query().Get("version") != "5.3.14" || u.Query().Get("loginSessionId") != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("query = %v", u.Query())
	}
}

// TestDecorateDesktopAuthURLOmitsEmptySessionID covers the international
// desktop, whose login page has no loginSessionId parameter.
func TestDecorateDesktopAuthURLOmitsEmptySessionID(t *testing.T) {
	got, err := decorateDesktopAuthURL("https://www.workbuddy.ai/login?state=s", "")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("state") != "s" || u.Query().Get("version") != "5.3.14" {
		t.Fatalf("query = %v", u.Query())
	}
	if u.Query().Has("loginSessionId") {
		t.Fatalf("loginSessionId must be absent, query = %v", u.Query())
	}
}

func TestDesktopLoginKeepsStartProfileAndSessionAcrossReconfigure(t *testing.T) {
	oldFeatures := featureRuntime.Load()
	desktop, err := parseFeatureRuntime([]byte("oauth_client_mode: workbuddy\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(desktop)
	t.Cleanup(func() { featureRuntime.Store(oldFeatures) })

	oldProxy := currentProxyState()
	proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})
	t.Cleanup(func() { proxyState.Store(oldProxy) })

	oldClient := sharedHTTPClient()
	var requests []*http.Request
	sharedClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Clone(req.Context()))
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			resp := testHTTPResponse(req, `{"code":0,"data":{"state":"desktop-login-snapshot","authUrl":"https://example.test/login?state=original"}}`)
			resp.Header.Set("Set-Cookie", "oauth-session=desktop; Path=/")
			return resp, nil
		case "/v2/plugin/auth/token":
			return testHTTPResponse(req, `{"code":0,"data":{"accessToken":"access","refreshToken":"refresh","expiresIn":3600}}`), nil
		case "/v2/plugin/login/account":
			return testHTTPResponse(req, `{"code":0,"data":{"uid":"desktop-user"}}`), nil
		default:
			return nil, &url.Error{Op: "unexpected", URL: req.URL.String(), Err: io.EOF}
		}
	})}
	t.Cleanup(func() { sharedClient = oldClient })

	startRaw, err := handleStartLogin(nil)
	if err != nil {
		t.Fatal(err)
	}
	var startEnvelope envelope
	if err := json.Unmarshal(startRaw, &startEnvelope); err != nil {
		t.Fatal(err)
	}
	var start pluginapi.AuthLoginStartResponse
	if err := json.Unmarshal(startEnvelope.Result, &start); err != nil {
		t.Fatal(err)
	}
	browserURL, err := url.Parse(start.URL)
	if err != nil {
		t.Fatal(err)
	}
	loginSessionID := browserURL.Query().Get("loginSessionId")
	if browserURL.Query().Get("state") != "original" || browserURL.Query().Get("version") != "5.3.14" || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(loginSessionID) {
		t.Fatalf("desktop browser URL = %q", start.URL)
	}
	loginState, ok := loginStates.Load("desktop-login-snapshot")
	if !ok {
		t.Fatal("login context was not stored")
	}
	lc := loginState.(*loginCtx)
	if lc.profile.mode != oauthClientModeWorkBuddy || lc.loginSessionID != loginSessionID {
		t.Fatalf("login context = %#v", lc)
	}
	t.Cleanup(func() { loginStates.Delete("desktop-login-snapshot") })

	cli, err := parseFeatureRuntime([]byte("oauth_client_mode: cli\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(cli)
	if _, err := handlePollLogin(mustJSON(map[string]any{"state": "desktop-login-snapshot"})); err != nil {
		t.Fatal(err)
	}

	if len(requests) != 3 {
		t.Fatalf("OAuth requests = %d, want 3", len(requests))
	}
	for _, req := range requests {
		if got := req.Header.Get("User-Agent"); got != userAgentFor(RealmCN) {
			t.Errorf("%s User-Agent = %q", req.URL.Path, got)
		}
		if got := req.Header.Get("Origin"); got != profileFor(RealmCN).Origin {
			t.Errorf("%s Origin = %q", req.URL.Path, got)
		}
		if got := req.Header.Get("Referer"); got != profileFor(RealmCN).Origin+"/" {
			t.Errorf("%s Referer = %q", req.URL.Path, got)
		}
	}
	for _, req := range requests[1:] {
		if got := req.Header.Get("Cookie"); got != "oauth-session=desktop" {
			t.Errorf("%s Cookie = %q, want oauth-session=desktop", req.URL.Path, got)
		}
	}
	for _, req := range requests[:2] {
		for _, key := range []string{"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
			if got := req.Header.Get(key); got != "true" {
				t.Errorf("%s %s = %q, want true", req.URL.Path, key, got)
			}
		}
	}
	account := requests[2]
	if got := account.Header.Get("Authorization"); got != "Bearer access" {
		t.Errorf("account authorization = %q", got)
	}
	if got := account.Header.Get("X-No-Authorization"); got != "" {
		t.Errorf("account X-No-Authorization = %q, want empty with bearer token", got)
	}
	for _, key := range []string{"X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		if got := account.Header.Get(key); got != "true" {
			t.Errorf("account %s = %q, want true", key, got)
		}
	}
}

func TestRefreshCallUsesDesktopProfile(t *testing.T) {
	oldFeatures := featureRuntime.Load()
	desktop, err := parseFeatureRuntime([]byte("oauth_client_mode: workbuddy\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(desktop)
	t.Cleanup(func() { featureRuntime.Store(oldFeatures) })

	oldProxy := currentProxyState()
	var got *http.Request
	proxyState.Store(&proxyRoutingState{mode: proxyModeExplicit, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		got = req.Clone(req.Context())
		return testHTTPResponse(req, `{"code":0,"data":{"accessToken":"new-access"}}`), nil
	})}})
	t.Cleanup(func() { proxyState.Store(oldProxy) })

	sa := &storedAuth{
		Auth:    storedTokens{RefreshToken: "refresh-secret"},
		Account: storedAccount{UID: "user-1", EnterpriseID: "enterprise-1"},
	}
	if _, _, _, err := refreshCall(sa); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("refresh sent no request")
	}
	if got.Method != http.MethodPost || got.URL.String() != endpointTokenRefreshFor(sa) {
		t.Fatalf("refresh request = %s %s", got.Method, got.URL)
	}
	for key, want := range map[string]string{
		"User-Agent":            userAgentFor(RealmCN),
		"Origin":                profileFor(RealmCN).Origin,
		"Referer":               profileFor(RealmCN).Origin + "/",
		"X-No-Authorization":    "true",
		"X-No-User-Id":          "true",
		"X-Enterprise-Id":       "enterprise-1",
		"X-Refresh-Token":       "refresh-secret",
		"X-Auth-Refresh-Source": "plugin",
	} {
		if gotValue := got.Header.Get(key); gotValue != want {
			t.Errorf("refresh %s = %q, want %q", key, gotValue, want)
		}
	}
	if gotValue := got.Header.Get("X-No-Enterprise-Id"); gotValue != "" {
		t.Errorf("refresh X-No-Enterprise-Id = %q with enterprise", gotValue)
	}
}
