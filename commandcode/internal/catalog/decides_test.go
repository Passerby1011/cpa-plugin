package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

// Jev (typesafe/jev) is a DECISION model: it answers typed questions with
// probabilities and holds no conversation. The reference implementation
// (magpie packages/commandcode, commit 0135b7b) lists it marked `decides` and
// offers it through its own `/v1/systemone` - a surface CPA does not have.
//
// Feasibility verdict, verified against CLIProxyAPI v7.2.138:
//   - sdk/pluginapi/types.go Capabilities has no "decides"/classifier slot
//     (ModelRegistrar, ModelProvider, AuthProvider, FrontendAuthProvider,
//     Scheduler, ModelRouter, Executor, the four translators, the two
//     interceptors, RequestLifecyclePlugin, ResponseInterceptor,
//     StreamChunkInterceptor, ThinkingApplier, UsagePlugin, CommandLinePlugin,
//     ManagementAPI - nothing for a decision model);
//   - internal/api/server_routes.go registers no `/systemone` (and no
//     `/provider/v1/*`) route, so there is nowhere to serve a question;
//   - the client-facing `/v1/models` answer is filtered to
//     id/object/created/owned_by, so a `decides` marker cannot even be
//     advertised.
//
// Therefore: NOT exposed, and defensively kept OUT of the chat list - a
// decision model offered as a chat model would accept a conversation it cannot
// hold. These tests pin that decision so a later "helpful" addition cannot
// quietly publish it.
func TestDecisionModelsAreExcludedFromTheChatCatalog(t *testing.T) {
	m := New(testCfg(), nil)
	m.swap([]rawModel{
		{ID: "glm-5.3"},
		{ID: "typesafe/jev", Decides: true},
		{ID: "typesafe/jev-latest", Decides: true, DisplayName: "Jev"},
	})

	models := m.Models()
	if len(models) != 1 || models[0].UpstreamID != "glm-5.3" {
		t.Fatalf("routable models = %+v, want only the chat model", models)
	}
	for _, id := range []string{"typesafe/jev", "typesafe/jev-latest", "commandcode/typesafe/jev"} {
		if _, ok := m.Lookup(id); ok {
			t.Fatalf("decision model %q is routable; the executor would send it a conversation", id)
		}
	}
	// Excluded but EXPLAINED: a silently missing model is a diagnosis nobody
	// can make, and the reason has to name the actual cause.
	unsup := m.Unsupported()
	found := false
	for _, entry := range unsup {
		if strings.HasPrefix(entry.UpstreamID, "typesafe/jev") {
			found = true
			if !strings.Contains(entry.Reason, "decides") {
				t.Fatalf("decision model excluded with reason %q, want it to name the missing decides capability", entry.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("decision models absent from diagnostics too: %+v", unsup)
	}
}

// The field must survive decoding - an entry that SAYS it decides has to be
// recognised - and a plain entry must not be mistaken for one.
func TestDecidesIsDecodedNotGuessed(t *testing.T) {
	// Only well-typed values are covered here on purpose: a wrongly-typed one
	// fails the whole entry decode, which is this catalog's (pre-existing)
	// tolerance policy for EVERY field - "id": 5 behaves the same way. Widening
	// tolerance for this one field alone would be a different change.
	for _, tc := range []struct {
		json    string
		decides bool
	}{
		{`{"id":"typesafe/jev","decides":true}`, true},
		{`{"id":"glm-5.3"}`, false},
		{`{"id":"glm-5.3","decides":false}`, false},
	} {
		var entry rawModel
		if err := json.Unmarshal([]byte(tc.json), &entry); err != nil {
			t.Fatalf("decode %s: %v", tc.json, err)
		}
		if entry.Decides != tc.decides {
			t.Fatalf("%s decoded decides=%t, want %t", tc.json, entry.Decides, tc.decides)
		}
	}
}
