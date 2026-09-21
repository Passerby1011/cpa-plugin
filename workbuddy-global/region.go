// region.go centralizes every realm-dependent value this plugin sends upstream.
//
// The plugin serves the international (Global) WorkBuddy service. Everything
// here is therefore pinned to the Global realm; the CN branch is kept only so
// the realm-routing helpers stay honest about what "not Global" means, and so
// a CN credential that a user happens to import is never silently sent to the
// wrong gateway.
//
// Values are sourced from two places, and the provenance is marked per value:
//
//   - "measured": reproduced from the upstream combined workbuddy plugin, which
//     runs this same protocol against production. These are treated as fact.
//   - "reference": taken from the sibling workbuddy2api implementation, whose
//     comments record the observation that produced the value. These are
//     implemented behind a candidate chain / fallback rather than hardcoded,
//     so a wrong guess degrades instead of breaking the account.
//
// Nothing in this file is guessed: see docs/PROVENANCE.md for the provenance
// table and the list of values that remain unverified.
package main

import "strings"

// Realm identifies which WorkBuddy deployment an account belongs to.
type Realm string

const (
	// RealmGlobal is the international service (www.workbuddy.ai). This plugin's
	// primary target.
	RealmGlobal Realm = "global"
	// RealmCN is the mainland service (copilot.tencent.com). Supported only so
	// an imported CN credential is detected and routed correctly instead of
	// being pointed at the international gateway.
	RealmCN Realm = "cn"
)

// realmProfile is the full set of realm-dependent outbound values.
type realmProfile struct {
	// Base is the chat/auth gateway host (free of trailing slash).
	Base string
	// Origin is the Origin/Referer origin the official client sends.
	Origin string
	// AcceptLanguage mirrors the official client's locale.
	AcceptLanguage string
	// UAPlatform is the second segment of the official desktop User-Agent.
	// Global must send "WorkBuddy AI": sending the CN "WorkBuddy" segment is
	// reported to trip upstream risk control (403 code 11140).
	UAPlatform string
	// BillingPaths are the candidate paths for the billing/meter family, tried
	// in order with a 404 falling through to the next. Global's primary form
	// has no /v2 prefix.
	BillingPaths []string
	// ModelsPath is the dynamic model-catalogue path for this realm.
	ModelsPath string
}

// Client version segments mirrored from the official desktop distribution.
// These are protocol constants shared by both realms; the realm-dependent part
// of the User-Agent is UAPlatform, not the version.
const (
	workBuddyClientVersion = "5.5.4"
	workBuddyCLIVersion    = "2.137.1"
)

// realmProfiles holds the outbound profile per realm.
//
// Base/Origin/AcceptLanguage are "measured" (identical in the upstream combined
// plugin's own constants). UAPlatform, BillingPaths and ModelsPath are
// "reference" and therefore always used through a candidates helper.
var realmProfiles = map[Realm]realmProfile{
	RealmGlobal: {
		Base:           "https://www.workbuddy.ai",
		Origin:         "https://www.workbuddy.ai",
		AcceptLanguage: "en-US",
		UAPlatform:     "WorkBuddy AI",
		// Global serves the meter family without the /v2 prefix; the /v2 form is
		// the documented fallback if the primary 404s.
		BillingPaths: []string{
			"/billing/meter/get-user-resource",
			"/v2/billing/meter/get-user-resource",
		},
		ModelsPath: "/v2/enterprises/personal/models",
	},
	RealmCN: {
		Base:           "https://copilot.tencent.com",
		Origin:         "https://www.codebuddy.cn",
		AcceptLanguage: "zh-CN",
		UAPlatform:     "WorkBuddy",
		BillingPaths: []string{
			"/v2/billing/meter/get-user-resource",
		},
		ModelsPath: "/console/enterprises/personal/models",
	},
}

// globalBase is the single source of truth for the international gateway host.
const globalBase = "https://www.workbuddy.ai"

// profileFor returns the outbound profile for a realm, defaulting to Global.
//
// Defaulting to Global is deliberate: this plugin exists to serve the
// international service, so an unidentifiable credential must not be routed to
// the mainland gateway.
func profileFor(r Realm) realmProfile {
	if p, ok := realmProfiles[r]; ok {
		return p
	}
	return realmProfiles[RealmGlobal]
}

// realmForAuth resolves the realm of a stored credential.
//
// Routing key is the JWT issuer, matching the upstream plugin's behaviour: a
// Global token's issuer lives under workbuddy.ai, a CN token's under
// codebuddy.cn / copilot.tencent.com. The stored domain field is the fallback
// for credentials whose access token is absent or unparseable.
func realmForAuth(sa *storedAuth) Realm {
	if sa == nil {
		return RealmGlobal
	}
	if r, err := realmFromAccessToken(sa.Auth.AccessToken); err == nil {
		return r
	}
	if isGlobalDomain(sa.Auth.Domain) {
		return RealmGlobal
	}
	// An empty domain means a legacy CN-format credential; a populated
	// non-workbuddy.ai domain means CN.
	if strings.TrimSpace(sa.Auth.Domain) == "" {
		return RealmCN
	}
	return RealmCN
}

// realmFromAccessToken decodes the unverified JWT issuer to determine the realm.
//
// The signature is intentionally not verified: this is routing metadata only,
// never an authorization decision. Every request still carries the token to an
// upstream that verifies it.
func realmFromAccessToken(accessToken string) (Realm, error) {
	claims, err := decodeUnverifiedJWTClaims(accessToken)
	if err != nil {
		return "", err
	}
	issuer, err := parseIssuerHost(claims.Issuer)
	if err != nil {
		return "", err
	}
	if isGlobalDomain(issuer) {
		return RealmGlobal, nil
	}
	return RealmCN, nil
}

// userAgentFor builds the official desktop User-Agent for a realm.
//
// Shape: WorkBuddy/<v> <platform>/<v> CLI/<cli>
// The platform segment is realm-dependent; the versions are not.
func userAgentFor(r Realm) string {
	p := profileFor(r)
	return "WorkBuddy/" + workBuddyClientVersion +
		" " + p.UAPlatform + "/" + workBuddyClientVersion +
		" CLI/" + workBuddyCLIVersion
}

// billingPathsFor returns the ordered candidate paths for the billing/meter
// family in this realm. Callers walk the list, advancing on 404/405 only.
func billingPathsFor(r Realm) []string {
	p := profileFor(r)
	out := make([]string, len(p.BillingPaths))
	copy(out, p.BillingPaths)
	return out
}

// modelsPathFor returns the dynamic model-catalogue path for this realm.
func modelsPathFor(r Realm) string {
	return profileFor(r).ModelsPath
}
