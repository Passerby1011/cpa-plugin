// oauth.go implements the AuthProvider login flow: browser-driven OAuth via
// CodeBuddy's login endpoints (CN and Global), login state polling, and token
// refresh. Each login flow gets an isolated cookie jar so multi-account flows
// never cross-contaminate session state.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type authRefreshRequestWire struct {
	pluginapi.AuthRefreshRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// newLoginClient builds an isolated client with its own cookie jar and binds it
// to the routing snapshot active when the login starts.
func newLoginClient() (*http.Client, error) {
	state := currentProxyState()
	if state.mode == proxyModeBlocked || state.mode == proxyModeExplicit && state.client == nil {
		return nil, blockedProxyError()
	}
	transport := sharedHTTPClient().Transport
	if state.mode == proxyModeExplicit {
		transport = state.client.Transport
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout:       30 * time.Second,
		Transport:     transport,
		Jar:           jar,
		CheckRedirect: rejectHTTPRedirect,
	}, nil
}

func loginClientForCurrentRouting(lc *loginCtx) (*http.Client, error) {
	if lc == nil || lc.client == nil {
		return nil, fmt.Errorf("login client unavailable")
	}
	routing := currentProxyState()
	if routing.mode == proxyModeBlocked || routing.mode == proxyModeExplicit && routing.client == nil {
		return nil, blockedProxyError()
	}
	client := *lc.client
	client.CheckRedirect = rejectHTTPRedirect
	if routing.mode == proxyModeExplicit {
		client.Transport = routing.client.Transport
	}
	return &client, nil
}

// doJSONRequest sends req and parses the {code,msg,data} envelope, returning the
// inner data payload. httpStatus is the upstream code.
func doJSONRequest(client *http.Client, req *http.Request) (json.RawMessage, int, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		// Redirects: Go's client follows them for GET, but a 3xx that lands
		// here (e.g. POST 307/308 not re-sent, or a new upstream gateway) would
		// otherwise surface as a misleading JSON "parse failed".
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, truncateRedacted(env.Msg, 120))
	}
	return env.Data, resp.StatusCode, nil
}

type oauthRequestProfile struct {
	mode      string
	base      string
	stateURL  string
	userAgent string
	origin    string
}

// desktopAppVersion is the WorkBuddy desktop app version the login flow
// advertises — both in the desktop User-Agent and in the sign-in page's
// `version` query parameter.
//
// The CN sign-in page gates its 企业微信 (WeCom) entry on it: its
// signin-callback chunk hides WeCom when platform is workbuddy and the
// version compares below "5.8.0" (the literal is Gt="5.8.0", compared
// segment by segment). The 5.3.14 this flow used to send was below the gate,
// so enterprise users never saw a WeCom login at all. Kept equal to the app
// version inside clientUA, which the rest of the plugin already advertises.
const desktopAppVersion = "5.8.3"

// oauthProfileForMode returns the login request profile for one client mode.
// Desktop logins are region split: workbuddy uses the CN gateway with
// platform=workbuddy, workbuddy-ai the international gateway with
// platform=workbuddy-ai. Everything downstream keys off the token response's
// domain field, so the profile only has to pick the right gateway.
//
// The UA must carry a full `WorkBuddy/<version>` segment: the login page's
// host is chosen from it upstream (a versionless or curl UA falls back to
// copilot.tencent.com instead of www.workbuddy.cn), and the version inside
// the segment doubles as the WeCom gate above.
func oauthProfileForMode(mode string) oauthRequestProfile {
	switch mode {
	case oauthClientModeWorkBuddy:
		return oauthRequestProfile{
			mode:      oauthClientModeWorkBuddy,
			base:      upstreamBaseCN,
			stateURL:  upstreamBaseCN + pluginAuthStatePath + "?platform=workbuddy",
			userAgent: clientUA,
			origin:    "https://www.workbuddy.cn",
		}
	case oauthClientModeWorkBuddyAI:
		return oauthRequestProfile{
			mode:      oauthClientModeWorkBuddyAI,
			base:      upstreamBaseGlobal,
			stateURL:  upstreamBaseGlobal + pluginAuthStatePath + "?platform=workbuddy-ai",
			userAgent: clientUA,
			origin:    originRefererGlobal,
		}
	}
	return oauthRequestProfile{
		mode:      oauthClientModeCLI,
		base:      upstreamBaseCN,
		stateURL:  endpointAuthState,
		userAgent: clientUA,
		origin:    originReferer,
	}
}

// isDesktopOAuthMode reports the plugin's desktop-client login profiles, which
// share the browser decoration and the X-No-* header treatment.
func isDesktopOAuthMode(mode string) bool {
	return mode == oauthClientModeWorkBuddy || mode == oauthClientModeWorkBuddyAI
}

func applyOAuthProfileHeaders(req *http.Request, profile oauthRequestProfile) {
	commonHeaders(req)
	if !isDesktopOAuthMode(profile.mode) {
		return
	}
	req.Header.Set("User-Agent", profile.userAgent)
	req.Header.Set("Origin", profile.origin)
	req.Header.Set("Referer", profile.origin+"/")
}

func applyAnonymousOAuthHeaders(req *http.Request, profile oauthRequestProfile) {
	applyOAuthProfileHeaders(req, profile)
	if !isDesktopOAuthMode(profile.mode) {
		return
	}
	req.Header.Set("X-No-Authorization", "true")
	req.Header.Set("X-No-User-Id", "true")
	req.Header.Set("X-No-Enterprise-Id", "true")
	req.Header.Set("X-No-Department-Info", "true")
}

// authEndpointFor joins one gateway-relative plugin OAuth path with the
// profile's gateway. Logins must poll the gateway that issued the state —
// mixing them (CN state on www.workbuddy.ai) silently yields a different
// account, so the base always travels with the profile.
func authEndpointFor(profile oauthRequestProfile, path string) string {
	return profile.base + path
}

func buildAuthStateRequest(profile oauthRequestProfile) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, profile.stateURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	applyAnonymousOAuthHeaders(req, profile)
	return req, nil
}

func buildAuthTokenRequest(profile oauthRequestProfile, state string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodGet, authEndpointFor(profile, pluginAuthTokenPath)+state, nil)
	if err != nil {
		return nil, err
	}
	applyAnonymousOAuthHeaders(req, profile)
	return req, nil
}

func buildLoginAccountRequest(profile oauthRequestProfile, state, accessToken string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodGet, authEndpointFor(profile, pluginLoginAccountPath)+state, nil)
	if err != nil {
		return nil, err
	}
	applyOAuthProfileHeaders(req, profile)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if isDesktopOAuthMode(profile.mode) {
		req.Header.Set("X-No-User-Id", "true")
		req.Header.Set("X-No-Enterprise-Id", "true")
		req.Header.Set("X-No-Department-Info", "true")
	}
	return req, nil
}

// buildLoginAccountsRequest builds GET /v2/plugin/accounts — the person's own
// account list (personal, team, 「WorkBuddy 教育公益版」…), used to name the
// enterprise a sign-in bound to when /login/account left the name out.
func buildLoginAccountsRequest(profile oauthRequestProfile, accessToken, domain string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodGet, authEndpointFor(profile, pluginAccountsPath), nil)
	if err != nil {
		return nil, err
	}
	applyOAuthProfileHeaders(req, profile)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	domain = strings.TrimSpace(domain)
	if domain == "" {
		// The official client falls back to the API host when the token
		// carried no domain (domainOf: a.domain || new URL(api(site)).host).
		if u, err := url.Parse(profile.base); err == nil {
			domain = u.Host
		}
	}
	if domain != "" {
		req.Header.Set("X-Domain", domain)
	}
	return req, nil
}

// loginAccountsResponse is one entry of /v2/plugin/accounts. Only the fields
// the enterprise choice reads are decoded; the upstream sends more.
type loginAccountsResponse struct {
	Accounts []accountData `json:"accounts"`
}

// enterpriseRef is the enterprise a sign-in belongs to: an id and, when
// WorkBuddy names one, its display name.
type enterpriseRef struct {
	ID   string
	Name string
}

// resolveEnterprise decides which enterprise (if any) the account a sign-in
// returned belongs to, mirroring the reference implementation's enterpriseOf():
//
//   - /login/account's own enterpriseId + enterpriseName win outright.
//   - A named id without a name takes the name the account list has for that
//     id (same uid).
//   - No id at all: an account that isn't personal is a team account and the
//     list says which — the uid's only entry of that type, else the only one
//     it last signed in to, else the uid's only enterprise entry.
//   - A personal account never asks: it has no enterprise by definition.
//
// The list call is best-effort. A failure (or an unparsable body) degrades to
// "id known, name unknown" — it must never fail the login: the credential is
// already valid at this point, and the enterprise id it carries is what
// billing needs.
func resolveEnterprise(client *http.Client, profile oauthRequestProfile, acct accountData, accessToken, domain string) enterpriseRef {
	own := strings.TrimSpace(acct.EnterpriseID)
	acctType := strings.ToLower(strings.TrimSpace(acct.Type))
	// Personal (or untyped with no enterprise): nothing to resolve, no request.
	if own == "" && (acctType == "" || acctType == "personal") {
		return enterpriseRef{}
	}
	if own != "" && strings.TrimSpace(acct.EnterpriseName) != "" {
		return enterpriseRef{ID: own, Name: strings.TrimSpace(acct.EnterpriseName)}
	}
	req, err := buildLoginAccountsRequest(profile, accessToken, domain)
	if err != nil {
		return enterpriseRef{ID: own}
	}
	data, _, err := doJSONRequest(client, req)
	if err != nil {
		return enterpriseRef{ID: own}
	}
	var list loginAccountsResponse
	if err := json.Unmarshal(data, &list); err != nil {
		return enterpriseRef{ID: own}
	}
	nameOf := func(entry accountData) string { return strings.TrimSpace(entry.EnterpriseName) }
	mine := make([]accountData, 0, len(list.Accounts))
	for _, entry := range list.Accounts {
		if entry.UID != acct.UID || strings.TrimSpace(entry.EnterpriseID) == "" {
			continue
		}
		mine = append(mine, entry)
	}
	if own != "" {
		// The id is settled; the list only supplies the name, and only when
		// it has an entry for exactly this enterprise.
		for _, entry := range mine {
			if strings.TrimSpace(entry.EnterpriseID) == own {
				return enterpriseRef{ID: own, Name: nameOf(entry)}
			}
		}
		return enterpriseRef{ID: own}
	}
	// No id: the account list decides, by the same priority the official
	// client's mergeCurrentAccountDetails uses.
	sameType := make([]accountData, 0, len(mine))
	lastLogin := make([]accountData, 0, len(mine))
	for _, entry := range mine {
		if strings.EqualFold(strings.TrimSpace(entry.Type), acctType) {
			sameType = append(sameType, entry)
		}
		if entry.LastLogin {
			lastLogin = append(lastLogin, entry)
		}
	}
	var pick *accountData
	switch {
	case len(sameType) == 1:
		pick = &sameType[0]
	case len(lastLogin) == 1:
		pick = &lastLogin[0]
	case len(mine) == 1:
		pick = &mine[0]
	}
	if pick == nil {
		return enterpriseRef{}
	}
	return enterpriseRef{ID: strings.TrimSpace(pick.EnterpriseID), Name: nameOf(*pick)}
}

func buildTokenRefreshRequest(profile oauthRequestProfile, sa *storedAuth) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, endpointTokenRefreshFor(sa), nil)
	if err != nil {
		return nil, err
	}
	applyOAuthProfileHeaders(req, profile)
	if profile.mode == oauthClientModeWorkBuddy {
		req.Header.Set("X-No-Authorization", "true")
		req.Header.Set("X-No-User-Id", "true")
		req.Header.Set("X-No-Department-Info", "true")
		if sa.Account.EnterpriseID == "" {
			req.Header.Set("X-No-Enterprise-Id", "true")
		}
	}
	req.Header.Set("X-Refresh-Token", sa.Auth.RefreshToken)
	if sa.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
	// The official client marks every backend call the same way; the refresh
	// path builds its own request and never passes through commonHeaders.
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("Accept-Language", acceptLanguageFor(sa))
	return req, nil
}

func decorateDesktopAuthURL(rawURL, loginSessionID string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	query := u.Query()
	// desktopAppVersion, not a literal: the CN sign-in page hides the WeCom
	// entry below 5.8.0 (see the constant's comment).
	query.Set("version", desktopAppVersion)
	// loginSessionId is the CN desktop's telemetry session id; the
	// international login bundle has no such parameter, so it stays CN-only.
	if loginSessionID != "" {
		query.Set("loginSessionId", loginSessionID)
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func handleStartLogin(_ []byte) ([]byte, error) {
	client, err := newLoginClient()
	if err != nil {
		return nil, fmt.Errorf("auth state failed: %w", err)
	}
	mode := oauthClientModeCLI
	if features := currentFeatureRuntime(); features != nil {
		mode = features.oauthClientMode
	}
	profile := oauthProfileForMode(mode)
	stateReq, err := buildAuthStateRequest(profile)
	if err != nil {
		return nil, fmt.Errorf("auth state failed: %w", err)
	}
	data, _, err := doJSONRequest(client, stateReq)
	if err != nil {
		return nil, fmt.Errorf("auth state failed: %w", err)
	}
	var st authStateData
	_ = json.Unmarshal(data, &st)
	if st.State == "" || st.AuthURL == "" {
		return nil, fmt.Errorf("auth state: missing state or authUrl — please restart the login flow")
	}
	loginSessionID := ""
	if profile.mode == oauthClientModeWorkBuddy {
		loginSessionID = randomHex(16)
	}
	if isDesktopOAuthMode(profile.mode) {
		st.AuthURL, err = decorateDesktopAuthURL(st.AuthURL, loginSessionID)
		if err != nil {
			return nil, fmt.Errorf("auth state: invalid authUrl: %w", err)
		}
	}
	loginStates.Store(st.State, &loginCtx{
		client:         client,
		expires:        time.Now().Add(loginTTL),
		profile:        profile,
		loginSessionID: loginSessionID,
	})
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       st.AuthURL,
		State:     st.State,
		ExpiresAt: time.Now().Add(loginTTL).UTC(),
		Metadata:  map[string]any{"logo": pluginLogoURL},
	})
}

func handlePollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return nil, fmt.Errorf("poll: empty state")
	}
	v, ok := loginStates.Load(state)
	if !ok {
		return nil, fmt.Errorf("poll: unknown state (restart login) — the login session was lost; please re-initiate login")
	}
	lc := v.(*loginCtx)
	if time.Now().After(lc.expires) {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: login expired (5 min timeout) — please re-initiate login and complete within 5 minutes")
	}
	profile := lc.profile
	if profile.mode == "" {
		profile = oauthProfileForMode(oauthClientModeCLI)
	}
	pollClient, err := loginClientForCurrentRouting(lc)
	if err != nil {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: %w", err)
	}

	// Single-shot poll per RPC: the host drives the polling cadence.
	// auth/token is the authoritative login-status endpoint: the application
	// layer returns a non-zero code ("login ing") while pending, and code 0
	// with the token bundle once complete. login/account sits behind the
	// openresty gateway and is rejected (401) until login finishes, so probe
	// token first and only fetch account once we hold a bearer.
	tokenReq, err := buildAuthTokenRequest(profile, state)
	if err != nil {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: token request: %w", err)
	}
	tokRaw, status, errTok := doJSONRequest(pollClient, tokenReq)
	if errTok != nil {
		// Transport-level failures and 5xx are real errors, not "still waiting":
		// surface them so the user sees a failure instead of polling until TTL.
		if status == 0 || status >= 500 {
			loginStates.Delete(state)
			return nil, fmt.Errorf("poll: token endpoint error: %w — upstream may be temporarily unavailable; retry in a few minutes", errTok)
		}
		// 4xx / business-code responses mean the login is still pending.
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for login",
		})
	}
	var tok tokenData
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for login",
		})
	}
	pollClient, err = loginClientForCurrentRouting(lc)
	if err != nil {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: %w", err)
	}

	var acct accountData
	accountReq, err := buildLoginAccountRequest(profile, state, tok.AccessToken)
	if err != nil {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: account request: %w", err)
	}
	acctRaw, _, errAcct := doJSONRequest(pollClient, accountReq)
	if errAcct != nil {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: account lookup failed after token success: %w", errAcct)
	}
	if err := json.Unmarshal(acctRaw, &acct); err != nil {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: account lookup parse failed: %w", err)
	}
	// Name the enterprise this sign-in belongs to. Personal accounts answer
	// without a request; everything else may need the account list. Failure
	// here degrades to "id known, name unknown" and never fails the login.
	ent := resolveEnterprise(pollClient, profile, acct, tok.AccessToken, tok.Domain)
	sa, err := buildLoginStoredAuthWithEnterprise(tok, acct, ent)
	if err != nil {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: %w", err)
	}
	loginStates.Delete(state)
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   toAuthDataForLoginPoll(sa),
	})
}

func toAuthDataForLoginPoll(sa *storedAuth) pluginapi.AuthData {
	ad := toAuthData(sa)
	ad.ID = ""
	return ad
}

func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req authRefreshRequestWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	// Route via host.http.do so request-log captures the refresh call (H2
	// compliance: the old direct sharedHTTPClient() call bypassed host
	// transport policy + logging for the X-Refresh-Token endpoint).
	data, raw2, status, err := refreshCallWithCallback(sa, req.HostCallbackID)
	if err != nil {
		if status >= 400 {
			return nil, fmt.Errorf("refresh rejected (HTTP %d)", status)
		}
		return nil, fmt.Errorf("refresh: %w", err)
	}
	_ = raw2
	var tok tokenData
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("refresh_failed: no accessToken in response — the refresh token may be expired; re-login required")
	}
	sa.Auth.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		sa.Auth.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		sa.Auth.Domain = tok.Domain
	}
	sa.Auth.ExpiresAt = expiryFromExpiresIn(tok.ExpiresIn, sa.Auth.ExpiresAt)
	// No explicit host.auth.save here: the host's auth Manager persists the
	// refreshed credential itself after Refresh returns (conductor.go
	// refreshAuth → m.Update → persist). Writing from the plugin too would
	// double-write the file.
	response, err := okEnvelope(pluginapi.AuthRefreshResponse{Auth: toAuthDataForRefresh(sa, req.Metadata)})
	if err != nil {
		return nil, err
	}
	currentModelRuntime().markAuthNotStarted(req.AuthID)
	return response, nil
}

func buildLoginStoredAuth(tok tokenData, acct accountData) (*storedAuth, error) {
	return buildLoginStoredAuthWithEnterprise(tok, acct, enterpriseRef{})
}

func buildLoginStoredAuthWithEnterprise(tok tokenData, acct accountData, ent enterpriseRef) (*storedAuth, error) {
	if strings.TrimSpace(tok.AccessToken) == "" {
		return nil, fmt.Errorf("token response missing accessToken")
	}
	if strings.TrimSpace(acct.UID) == "" {
		return nil, fmt.Errorf("account lookup missing uid")
	}
	account := storedAccount{
		UID:            acct.UID,
		EnterpriseID:   firstNonEmpty(strings.TrimSpace(ent.ID), acct.EnterpriseID),
		EnterpriseName: firstNonEmpty(strings.TrimSpace(ent.Name), acct.EnterpriseName),
		Nickname:       acct.Nickname,
		Type:           acct.Type,
	}
	return &storedAuth{
		Auth: storedTokens{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			ExpiresAt:    expiryFromExpiresIn(tok.ExpiresIn, 0),
			Domain:       tok.Domain,
		},
		Account: account,
	}, nil
}

func expiryFromExpiresIn(expiresIn, oldExpiry int64) int64 {
	if expiresIn <= 0 {
		return oldExpiry
	}
	return time.Now().Add(time.Duration(expiresIn) * time.Second).Unix()
}

// preserveExpiry reuses the previous token's expiresAt when the refresh
// response omits expiresIn (some CodeBuddy deployments return only the token
// pair). Zero would tell the host the credential is permanently expired and
// trigger a refresh storm on every request.
func preserveExpiry(newExpiry, oldExpiry int64) int64 {
	if newExpiry > 0 {
		return newExpiry
	}
	return oldExpiry
}

// toAuthDataForRefresh returns AuthData with FileName left EMPTY so the CPA
// host backfills the original auth.FileName (auth_provider.go:371).
//
// CPA uses FileName (relative to auth dir) as auth ID. If we set it to
// "workbuddy-<uid>.json" while the original file was "workbuddy.json"
// (legacy single-account name), the host treats it as a rename, writes a
// NEW file, and the old one stays → duplicate auth records.
//
// Returning empty FileName = "keep what you had" → no rename, no dup.
//
// carrier is the host-side Metadata of the record being refreshed: it holds the
// user's own settings (weight, priority, proxy_url, prefix, request_retry, …)
// which the host's plugin-token-storage Save would otherwise overwrite with our
// small Metadata map, dropping them from the auth file on every refresh.
func toAuthDataForRefresh(sa *storedAuth, carrier map[string]any) pluginapi.AuthData {
	ad := toAuthDataOpts(sa, nil, false, carrier)
	ad.FileName = "" // let host backfill original
	ad.ID = ""       // let host compute from path (prevents ID mismatch dupes)
	return ad
}
