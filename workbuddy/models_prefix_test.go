package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// A host-side prefix leak: measured on the live gateway, one request reached
// the upstream as `gpt-5.6-luna` on its first attempt and as
// `wb/gpt-5.6-luna` on the retry within the SAME request, and the upstream
// answered 11102 "model service info not found" for the prefixed form.
//
// The plugin cannot read the auth file's prefix on the executor path (the host
// forwards AuthAttributes, not Auth.Prefix), so it strips a leading segment
// only when the remainder is a model it actually serves. That keeps the strip
// self-validating: it can never invent a model name.
func TestResolveUpstreamModelStripsLeakedPrefix(t *testing.T) {
	const authID = "auth-prefix-test"
	store := newModelStore(t.TempDir())
	sa := syntheticStoredAuth(t, workBuddyRealmGlobal)
	// Seed the shared caches the way a completed refresh would, so the roster
	// resolves from the plugin's own store rather than a live call.
	identity, err := modelAuthIdentityFor(authID, sa)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.saveModels(modelCatalogCacheV1{
		SchemaVersion:  modelCacheSchemaVersion,
		IdentitySHA256: identity.sha256(),
		Realm:          workBuddyRealmGlobal,
		FetchedAt:      time.Date(2026, time.September, 15, 1, 2, 3, 0, time.UTC),
		Endpoint:       workBuddyEndpointV3Config,
		Models:         []modelFacts{{ID: "gpt-5.6-luna"}, {ID: "deepseek-v4.1-flash"}},
	}); err != nil {
		t.Fatalf("seed models: %v", err)
	}
	runtime := newModelRuntime(store, func(req *http.Request, gotCallbackID string) (*hostHTTPResponse, error) {
		return &hostHTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    make(http.Header),
			Body:       []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["gpt-5.6-luna","deepseek-v4.1-flash"]}]}}`),
		}, nil
	})
	oldRuntime := activeModelRuntime.Swap(runtime)
	t.Cleanup(func() { activeModelRuntime.Store(oldRuntime) })

	// Populate the roster for this credential the way the host does.
	if _, err := handleModelForAuth(mustJSON(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{
			AuthID:      authID,
			StorageJSON: mustJSON(sa),
			Host:        pluginapi.HostConfigSummary{},
		},
	})); err != nil {
		t.Fatalf("populate roster: %v", err)
	}
	if got := len(runtime.snapshotForAuthID(authID).Models); got != 2 {
		t.Fatalf("roster size = %d, want 2", got)
	}

	cases := []struct {
		name  string
		model string
		want  string
	}{
		{"leaked prefix is stripped", "wb/gpt-5.6-luna", "gpt-5.6-luna"},
		{"bare id is untouched", "gpt-5.6-luna", "gpt-5.6-luna"},
		{"second served model also strips", "wb/deepseek-v4.1-flash", "deepseek-v4.1-flash"},
		{"unknown tail is never invented", "wb/some-unknown-model", "wb/some-unknown-model"},
		{"unrelated namespace is left alone", "vendor/serve-alpha", "vendor/serve-alpha"},
		{"leading slash is not a prefix", "/gpt-5.6-luna", "/gpt-5.6-luna"},
		{"trailing slash is not a prefix", "wb/", "wb/"},
		{"no slash at all", "gpt-5.6-sol", "gpt-5.6-sol"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveUpstreamModel(tc.model, nil, authID); got != tc.want {
				t.Fatalf("resolveUpstreamModel(%q) = %q, want %q", tc.model, got, tc.want)
			}
		})
	}

	// An empty authID has no roster, so nothing may be stripped: the fallback
	// must never guess without evidence.
	if got := resolveUpstreamModel("wb/gpt-5.6-luna", nil, ""); got != "wb/gpt-5.6-luna" {
		t.Fatalf("empty auth id stripped a prefix: %q", got)
	}
}

// The dangerous shape: an upstream model whose OWN id contains a slash, while a
// different served model happens to carry the tail. Stripping there would
// silently answer a request for one model with another, so a slash-bearing ID
// that is itself served must pass through untouched.
//
// Not hypothetical: the catalogue already pairs names this way across
// providers (nvidia/z-ai/glm-5.3-flash alongside glm-5.3-flash), so a future
// workbuddy model adopting a vendor-prefixed name is a normal upstream
// evolution rather than an exotic case.
func TestResolveUpstreamModelKeepsSlashedServedIDs(t *testing.T) {
	const authID = "auth-slashed-id"
	store := newModelStore(t.TempDir())
	sa := syntheticStoredAuth(t, workBuddyRealmGlobal)
	identity, err := modelAuthIdentityFor(authID, sa)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.saveModels(modelCatalogCacheV1{
		SchemaVersion:  modelCacheSchemaVersion,
		IdentitySHA256: identity.sha256(),
		Realm:          workBuddyRealmGlobal,
		FetchedAt:      time.Date(2026, time.September, 15, 1, 2, 3, 0, time.UTC),
		Endpoint:       workBuddyEndpointV3Config,
		// Both the vendor-prefixed id AND the bare id are served models.
		Models: []modelFacts{{ID: "deepseek-ai/deepseek-v4-flash"}, {ID: "deepseek-v4-flash"}},
	}); err != nil {
		t.Fatalf("seed models: %v", err)
	}
	runtime := newModelRuntime(store, func(*http.Request, string) (*hostHTTPResponse, error) {
		return &hostHTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    make(http.Header),
			Body:       []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["deepseek-ai/deepseek-v4-flash","deepseek-v4-flash"]}]}}`),
		}, nil
	})
	oldRuntime := activeModelRuntime.Swap(runtime)
	t.Cleanup(func() { activeModelRuntime.Store(oldRuntime) })

	if _, err := handleModelForAuth(mustJSON(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{
			AuthID:      authID,
			StorageJSON: mustJSON(sa),
			Host:        pluginapi.HostConfigSummary{},
		},
	})); err != nil {
		t.Fatalf("populate roster: %v", err)
	}
	if got := len(runtime.snapshotForAuthID(authID).Models); got != 2 {
		t.Fatalf("roster size = %d, want 2", got)
	}

	for _, model := range []string{"deepseek-ai/deepseek-v4-flash", "deepseek-v4-flash"} {
		if got := resolveUpstreamModel(model, nil, authID); got != model {
			t.Fatalf("served id %q was rewritten to %q — a served model must never be turned into another", model, got)
		}
	}
	// A leaked prefix over the slashed id still resolves: the whole "wb/..." is
	// not served, the tail is.
	if got := resolveUpstreamModel("wb/deepseek-ai/deepseek-v4-flash", nil, authID); got != "deepseek-ai/deepseek-v4-flash" {
		t.Fatalf("leaked prefix over a slashed served id = %q", got)
	}
	// And a leaked prefix over the bare id keeps resolving too.
	if got := resolveUpstreamModel("wb/deepseek-v4-flash", nil, authID); got != "deepseek-v4-flash" {
		t.Fatalf("leaked prefix over a bare served id = %q", got)
	}
}

// A configured model list applies to every credential, including ones whose
// per-auth roster has not been populated yet.
func TestResolveUpstreamModelUsesConfiguredModels(t *testing.T) {
	oldRuntime := activeModelRuntime.Swap(newModelRuntime(nil, func(*http.Request, string) (*hostHTTPResponse, error) {
		return nil, nil
	}))
	t.Cleanup(func() { activeModelRuntime.Store(oldRuntime) })

	features := *currentFeatureRuntime()
	features.configuredModels = []string{"configured-model"}
	oldFeatures := featureRuntime.Swap(&features)
	t.Cleanup(func() { featureRuntime.Store(oldFeatures) })

	if got := resolveUpstreamModel("wb/configured-model", nil, "auth-without-roster"); got != "configured-model" {
		t.Fatalf("configured model prefix not stripped: %q", got)
	}
	if got := resolveUpstreamModel("wb/not-configured", nil, "auth-without-roster"); got != "wb/not-configured" {
		t.Fatalf("unconfigured model was stripped: %q", got)
	}
}
