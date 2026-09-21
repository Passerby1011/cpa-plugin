package main

import (
	"encoding/json"
	"testing"
)

// The host rewrites an auth file from the plugin's returned Metadata map: any
// user-owned key the plugin omits is dropped from disk. weight is the one the
// user noticed losing, so pin the whole preservation contract.
func TestRefreshMetadataPreservesUserConfiguredFields(t *testing.T) {
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "at", RefreshToken: "rt", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "u1", Nickname: "nick"},
	}
	hostMetadata := map[string]any{
		"type":          "workbuddy-global",
		"note":          "stale note from host",
		"disabled":      false,
		"weight":        float64(7),
		"priority":      float64(3),
		"proxy_url":     "socks5://127.0.0.1:7897",
		"prefix":        "wb",
		"websockets":    true,
		"request_retry": float64(2),
	}
	ad := toAuthDataForRefresh(sa, hostMetadata)

	if got, want := ad.Metadata["weight"], float64(7); got != want {
		t.Errorf("weight = %#v, want %#v", got, want)
	}
	if got, want := ad.Metadata["priority"], float64(3); got != want {
		t.Errorf("priority = %#v, want %#v", got, want)
	}
	if got, want := ad.Metadata["proxy_url"], "socks5://127.0.0.1:7897"; got != want {
		t.Errorf("proxy_url = %#v, want %#v", got, want)
	}
	if got, want := ad.Metadata["prefix"], "wb"; got != want {
		t.Errorf("prefix = %#v, want %#v", got, want)
	}
	if got, want := ad.Metadata["websockets"], true; got != want {
		t.Errorf("websockets = %#v, want %#v", got, want)
	}
	// Plugin-owned keys stay authoritative: the stale host note is replaced by
	// the live credits summary.
	if got := ad.Metadata["note"]; got == "stale note from host" {
		t.Errorf("note was not refreshed: %#v", got)
	}
	if got := ad.Metadata["type"]; got != providerName {
		t.Errorf("type = %#v, want %q", got, providerName)
	}
	if got, ok := ad.Metadata["disabled"]; !ok || got != false {
		t.Errorf("disabled = %#v, want false", got)
	}
	// FileName/ID stay empty so the host keeps the original path identity.
	if ad.FileName != "" || ad.ID != "" {
		t.Errorf("refresh identity must stay empty: fileName=%q id=%q", ad.FileName, ad.ID)
	}
}

func TestRefreshWithoutHostMetadataStillBuildsAuthData(t *testing.T) {
	sa := &storedAuth{Auth: storedTokens{AccessToken: "at"}, Account: storedAccount{UID: "u1"}}
	ad := toAuthDataForRefresh(sa, nil)
	if ad.Metadata["type"] != providerName {
		t.Fatalf("metadata = %#v", ad.Metadata)
	}
	if _, ok := ad.Metadata["weight"]; ok {
		t.Fatal("weight must not be invented when the host sent none")
	}
}

func TestParseAuthCarrierKeepsOnlyUserOwnedKeys(t *testing.T) {
	raw := []byte(`{
		"type":"workbuddy-global","provider":"workbuddy-global","logo":"l","disabled":false,
		"note":"CN · 余1 已用2 池3",
		"weight":5,"priority":1,"proxy_url":"http://p:1","prefix":"wb",
		"headers":{"X-Test":"1"},"request_retry":3,"websockets":true,
		"auth":{"accessToken":"secret","refreshToken":"secret"},"account":{"uid":"u1"}
	}`)
	carrier := parseAuthMetadataCarrier(raw)
	for _, key := range []string{"weight", "priority", "proxy_url", "prefix", "headers", "request_retry", "websockets", "note"} {
		if _, ok := carrier[key]; !ok {
			t.Errorf("carrier lost %q", key)
		}
	}
	for _, key := range []string{"auth", "account", "type", "provider", "logo", "disabled"} {
		if _, ok := carrier[key]; ok {
			t.Errorf("carrier must not carry %q", key)
		}
	}
}

// A re-parse of the same file must be idempotent: the host rewrites the file
// from what the plugin returns, so feeding that output back keeps the values.
func TestParseAuthCarrierRoundTripsThroughAuthData(t *testing.T) {
	raw := []byte(`{"type":"workbuddy-global","note":"n","weight":9,"proxy_url":"http://p:2","auth":{"accessToken":"at","refreshToken":"rt"},"account":{"uid":"u1"}}`)
	carrier := parseAuthMetadataCarrier(raw)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatal(err)
	}
	ad := toAuthDataOpts(sa, nil, false, carrier)
	encoded, err := json.Marshal(ad.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	second := parseAuthMetadataCarrier(encoded)
	if second["weight"] != float64(9) || second["proxy_url"] != "http://p:2" {
		t.Fatalf("second pass lost user config: %#v", second)
	}
}
