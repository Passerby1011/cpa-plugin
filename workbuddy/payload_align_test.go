package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The official CLI's configureThinkingSettings runs OUTSIDE the compatibility
// pipeline it skips for internal domains, so reasoning_summary="auto" and
// verbosity="high" really do go to this gateway. The plugin should send them
// too, but only when the caller is actually asking for thinking.
func TestAlignThinkingFieldsAddsOfficialFieldsWhenThinkingOn(t *testing.T) {
	body := []byte(`{"model":"serve-alpha","reasoning_effort":"high","messages":[]}`)
	if err := json.Unmarshal(prepareUpstreamBody(body, nil, nil, "serve-alpha"), &map[string]any{}); err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(prepareUpstreamBody(body, nil, nil, "serve-alpha"), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["reasoning_summary"] != "auto" {
		t.Fatalf("reasoning_summary = %#v, want \"auto\"", obj["reasoning_summary"])
	}
	if obj["verbosity"] != "high" {
		t.Fatalf("verbosity = %#v, want \"high\"", obj["verbosity"])
	}
	if obj["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %#v, want it preserved", obj["reasoning_effort"])
	}
}

func TestAlignThinkingFieldsRespectsNestedEffort(t *testing.T) {
	body := []byte(`{"model":"serve-alpha","reasoning":{"effort":"max"},"messages":[]}`)
	var obj map[string]any
	if err := json.Unmarshal(prepareUpstreamBody(body, nil, nil, "serve-alpha"), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["reasoning_summary"] != "auto" || obj["verbosity"] != "high" {
		t.Fatalf("nested effort should enable thinking fields: %#v", obj)
	}
}

// A request with no thinking signal must stay untouched: the official client
// only reaches those body mutations on the thinking-enabled path, and adding a
// reasoning_summary would contradict its own "no thinking" shape.
func TestAlignThinkingFieldsLeavesNonThinkingRequestAlone(t *testing.T) {
	body := []byte(`{"model":"serve-alpha","messages":[{"role":"user","content":"hi"}]}`)
	var obj map[string]any
	if err := json.Unmarshal(prepareUpstreamBody(body, nil, nil, "serve-alpha"), &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["reasoning_summary"]; ok {
		t.Fatalf("reasoning_summary must not appear: %#v", obj)
	}
	if _, ok := obj["verbosity"]; ok {
		t.Fatalf("verbosity must not appear: %#v", obj)
	}
}

// Caller-provided values win; the plugin only fills gaps.
func TestAlignThinkingFieldsDoesNotOverwriteCallerValues(t *testing.T) {
	body := []byte(`{"model":"serve-alpha","reasoning_effort":"low","reasoning_summary":"detailed","verbosity":"low","messages":[]}`)
	var obj map[string]any
	if err := json.Unmarshal(prepareUpstreamBody(body, nil, nil, "serve-alpha"), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["reasoning_summary"] != "detailed" {
		t.Fatalf("reasoning_summary = %#v, want caller value preserved", obj["reasoning_summary"])
	}
	if obj["verbosity"] != "low" {
		t.Fatalf("verbosity = %#v, want caller value preserved", obj["verbosity"])
	}
}

// Idempotence: running the chain twice must not change the result.
func TestAlignThinkingFieldsIsIdempotent(t *testing.T) {
	body := []byte(`{"model":"serve-alpha","reasoning_effort":"high","messages":[]}`)
	once := prepareUpstreamBody(body, nil, nil, "serve-alpha")
	twice := prepareUpstreamBody(once, nil, nil, "serve-alpha")

	var a, b map[string]any
	if err := json.Unmarshal(once, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(twice, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("second pass changed the body:\n first: %#v\nsecond: %#v", a, b)
	}
}

// An empty reasoning_effort string is not a thinking signal.
func TestRequestWantsThinkingIgnoresBlankEffort(t *testing.T) {
	cases := []struct {
		name string
		obj  map[string]any
		want bool
	}{
		{"blank effort", map[string]any{"reasoning_effort": "   "}, false},
		{"missing", map[string]any{}, false},
		{"effort", map[string]any{"reasoning_effort": "low"}, true},
		{"nested effort", map[string]any{"reasoning": map[string]any{"effort": "max"}}, true},
		{"nested enabled", map[string]any{"reasoning": map[string]any{"enabled": true}}, true},
		{"nested disabled", map[string]any{"reasoning": map[string]any{"enabled": false}}, false},
		{"summary only", map[string]any{"reasoning_summary": "auto"}, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := requestWantsThinking(tt.obj); got != tt.want {
				t.Fatalf("requestWantsThinking = %v, want %v", got, tt.want)
			}
		})
	}
}
