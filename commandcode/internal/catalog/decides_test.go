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

// THE REAL RISK: the flag is the *secondary* signal. Command Code's `/models`
// list does not name Jev at all (the reference implementation adds it from its
// own config), so an upstream - or a `catalog.static` table written from
// `/v1/models` output - that lists it plainly carries NO `decides` field. Keying
// the exclusion off the field alone would then publish exactly the model this
// check exists to keep out, which is why the id is matched too.
func TestDecisionModelIDsAreExcludedWithoutAnyDecidesFlag(t *testing.T) {
	m := New(testCfg(), nil)
	m.swap([]rawModel{
		{ID: "glm-5.3"},
		// No Decides field at all: this is what a plain listing looks like.
		{ID: "typesafe/jev"},
		{ID: "typesafe/jev-latest"},
		{ID: "typesafe/Jev"},
		{ID: "commandcode/typesafe/jev"},
		// A different owner's "jev" is a different family and must survive:
		// excluding by bare substring would silently drop a chat model.
		{ID: "someoneelse/jev"},
		{ID: "jev"},
		{ID: "jev-pro-chat"},
	})

	var kept []string
	for _, model := range m.Models() {
		kept = append(kept, model.UpstreamID)
	}
	for _, unwanted := range []string{
		"typesafe/jev", "typesafe/jev-latest", "typesafe/Jev", "commandcode/typesafe/jev",
	} {
		for _, id := range kept {
			if strings.EqualFold(id, unwanted) {
				t.Fatalf("decision model %q published as a chat model without a decides flag", unwanted)
			}
		}
	}
	for _, id := range []string{"typesafe/jev", "typesafe/jev-latest", "typesafe/Jev", "commandcode/typesafe/jev"} {
		if _, ok := m.Lookup(id); ok {
			t.Fatalf("decision model %q is routable without a decides flag", id)
		}
	}
	// The unrelated models must still be routable - over-exclusion is the
	// other way this check fails, and a bare "jev" substring match would do it.
	for _, want := range []string{"glm-5.3", "someoneelse/jev", "jev", "jev-pro-chat"} {
		if _, ok := m.Lookup(want); !ok {
			t.Fatalf("unrelated model %q was excluded; the id rule is too broad", want)
		}
	}
	// Same diagnostic as the flagged path: one explanation for one cause.
	for _, entry := range m.Unsupported() {
		if strings.HasPrefix(strings.ToLower(entry.UpstreamID), "typesafe/jev") &&
			!strings.Contains(entry.Reason, "decides") {
			t.Fatalf("%q excluded with reason %q, want the decides explanation",
				entry.UpstreamID, entry.Reason)
		}
	}
}

// The flag must survive decoding - an entry that SAYS it decides has to be
// recognised even under an id the rule does not know - and a plain entry must
// not be mistaken for one.
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
