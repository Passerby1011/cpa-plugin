package catalog

import (
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/commandcode/internal/config"
)

// TestSupportedEndpointsDriveTheThreeRoutes pins the routing rule that used to
// be missing entirely: CommandCode's /models lists, per model, which API paths
// serve it, and the plugin must call THAT one instead of sending every model to
// chat-completions and letting the upstream answer "not supported on this
// endpoint".
//
// Three branches, one per declared shape, plus the two fallbacks:
//   - a model served only on /messages takes the messages route;
//   - a model served only on /responses takes the responses route;
//   - a model served on /chat/completions takes chat-completions, including
//     when it is ALSO served elsewhere (chat is the preference winner);
//   - an entry with no supported_endpoints keeps the historical chat default;
//   - an entry whose declared endpoints are all unrecognisable also keeps the
//     default rather than inventing one.
func TestSupportedEndpointsDriveTheThreeRoutes(t *testing.T) {
	body := `{"data":[
		{"id":"claude-only","supported_endpoints":["/messages"]},
		{"id":"gpt-only-responses","supported_endpoints":["/responses"]},
		{"id":"chat-only","supported_endpoints":["/chat/completions"]},
		{"id":"chat-and-responses","supported_endpoints":["/responses","/chat/completions"]},
		{"id":"responses-and-messages","supported_endpoints":["/responses","/messages"]},
		{"id":"v1-prefixed","supported_endpoints":["/v1/messages","/v1/chat/completions"]},
		{"id":"trailing-slash","supported_endpoints":["/messages/","/responses/"]},
		{"id":"no-claim"},
		{"id":"empty-claim","supported_endpoints":[]},
		{"id":"unknown-claim","supported_endpoints":["/embeddings","/audio/speech"]}
	]}`
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}}
	m := newManager(testCfg(), fc)
	mustRefresh(t, m)

	want := map[string]Route{
		"claude-only":            RouteMessages,
		"gpt-only-responses":     RouteResponses,
		"chat-only":              RouteChatCompletions,
		"chat-and-responses":     RouteChatCompletions,
		"responses-and-messages": RouteResponses,
		"v1-prefixed":            RouteChatCompletions,
		"trailing-slash":         RouteResponses,
		"no-claim":               RouteChatCompletions,
		"empty-claim":            RouteChatCompletions,
		"unknown-claim":          RouteChatCompletions,
	}
	if len(m.Models()) != len(want) {
		t.Fatalf("got %d routable models, want %d: %+v", len(m.Models()), len(want), m.Unsupported())
	}
	for id, route := range want {
		rec := findModel(t, m.Models(), id)
		if rec.Protocol != route {
			t.Errorf("%s: Protocol = %q, want %q", id, rec.Protocol, route)
		}
		if rec.EndpointPath != route.EndpointPath() {
			t.Errorf("%s: EndpointPath = %q, want %q", id, rec.EndpointPath, route.EndpointPath())
		}
	}
}

// TestSupportedEndpointsLoseToRouteOverride pins the priority order: an
// operator's explicit route-overrides entry still wins over the upstream's own
// claim, because the override exists precisely to correct it (a deployment
// fronting a base-url that serves the model elsewhere).
func TestSupportedEndpointsLoseToRouteOverride(t *testing.T) {
	body := `{"data":[{"id":"claimed-messages","supported_endpoints":["/messages"]}]}`
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}}
	cfg := testCfg()
	cfg.RouteOverrides = map[string]config.RouteOverride{
		"claimed-messages": {Protocol: "chat-completions", Endpoint: "/v1/chat-completions-alias"},
	}
	m := newManager(cfg, fc)
	mustRefresh(t, m)
	rec := findModel(t, m.Models(), "claimed-messages")
	if rec.Protocol != RouteChatCompletions || rec.EndpointPath != "/v1/chat-completions-alias" {
		t.Fatalf("override lost to the catalog claim: %+v", rec)
	}
	// And the override must be visible in diagnostics (spec 04 §5).
	found := false
	for _, w := range m.Warnings() {
		if strings.Contains(w, "claimed-messages") && strings.Contains(w, "override") {
			found = true
		}
	}
	if !found {
		t.Errorf("override not reported in diagnostics: %v", m.Warnings())
	}
}

// TestSupportedEndpointsRespectProtocolKillSwitch pins that the per-model claim
// is still subject to the protocol kill switches: disabling messages must
// exclude a messages-only model (with a diagnostic) instead of quietly routing
// it to chat-completions, which is a surface the upstream did not list for it.
func TestSupportedEndpointsRespectProtocolKillSwitch(t *testing.T) {
	body := `{"data":[
		{"id":"claude-only","supported_endpoints":["/messages"]},
		{"id":"chat-only","supported_endpoints":["/chat/completions"]},
		{"id":"no-claim"}
	]}`
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}}
	cfg := testCfg()
	cfg.Protocols.Messages = false
	m := newManager(cfg, fc)
	mustRefresh(t, m)

	var got []string
	for _, rec := range m.Models() {
		got = append(got, rec.UpstreamID)
	}
	want := []string{"chat-only", "no-claim"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("routable models = %v, want %v (messages-only model must be excluded, not rerouted)", got, want)
	}
	var reason string
	for _, u := range m.Unsupported() {
		if u.UpstreamID == "claude-only" {
			reason = u.Reason
		}
	}
	if !strings.Contains(reason, "protocol messages disabled") {
		t.Errorf("claude-only exclusion reason = %q, want the protocol kill switch named", reason)
	}
}

// TestSupportedEndpointsSeededIntoServedSnapshot drives the claim through
// SeedFrom as well: a reconfigure seeds the previous raw snapshot into a NEW
// manager, and the rebuild must re-resolve routes from the same catalogue
// claims (otherwise a saved config would quietly revert every model to
// chat-completions until the next successful refresh).
func TestSupportedEndpointsSeededIntoServedSnapshot(t *testing.T) {
	body := `{"data":[
		{"id":"claude-only","supported_endpoints":["/messages"]},
		{"id":"chat-only","supported_endpoints":["/chat/completions"]}
	]}`
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}}
	prev := newManager(testCfg(), fc)
	mustRefresh(t, prev)

	next := newManager(testCfg(), &fakeClient{})
	next.SeedFrom(prev)
	if rec := findModel(t, next.Models(), "claude-only"); rec.Protocol != RouteMessages {
		t.Errorf("seeded claude-only kept %q, want %q", rec.Protocol, RouteMessages)
	}
	if rec := findModel(t, next.Models(), "chat-only"); rec.Protocol != RouteChatCompletions {
		t.Errorf("seeded chat-only kept %q, want %q", rec.Protocol, RouteChatCompletions)
	}
}

// TestSupportedEndpointsStaticCatalogStillDefaults pins the static path: a
// configured static model list carries no endpoint claims at all, so every id
// there keeps the chat default. A go-cli-only pool depends on this list, and a
// regression would leave it with models it cannot call.
func TestSupportedEndpointsStaticCatalogStillDefaults(t *testing.T) {
	m := newManager(testCfg(), &fakeClient{})
	m.SeedStatic([]string{"claude-opus-5-5", "deepseek/deepseek-v4.1-flash"})
	if len(m.Models()) != 2 {
		t.Fatalf("static catalog served %d models, want 2", len(m.Models()))
	}
	for _, rec := range m.Models() {
		if rec.Protocol != RouteChatCompletions {
			t.Errorf("%s: static model routed to %q, want the chat default", rec.UpstreamID, rec.Protocol)
		}
	}
}
