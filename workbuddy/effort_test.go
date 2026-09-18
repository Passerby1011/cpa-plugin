// effort_test.go pins the reasoning_effort downgrade.
//
// The upstream validates the tier against the model's own list and answers 400
// for a tier the model does not offer, so the mapping rules below decide
// whether a request is served at a nearby tier or lost outright.
package main

import (
	"encoding/json"
	"testing"
)

func TestNormalizeReasoningEffort(t *testing.T) {
	cases := []struct {
		name      string
		requested string
		supported []string
		want      string
	}{
		{
			name:      "exact match passes through",
			requested: "high",
			supported: []string{"low", "high", "max"},
			want:      "high",
		},
		{
			name:      "unsupported tier falls to the highest below it",
			requested: "xhigh",
			supported: []string{"low", "high", "max"},
			want:      "high",
		},
		{
			name:      "all tiers above the request takes the lowest",
			requested: "minimal",
			supported: []string{"high", "max"},
			want:      "high",
		},
		{
			name:      "single-tier model forces that tier",
			requested: "low",
			supported: []string{"high"},
			want:      "high",
		},
		{
			name:      "no tier list passes through",
			requested: "high",
			supported: nil,
			want:      "high",
		},
		{
			name:      "unknown requested tier passes through",
			requested: "turbo",
			supported: []string{"low", "high"},
			want:      "turbo",
		},
		{
			name:      "unranked supported tiers pass the request through",
			requested: "high",
			supported: []string{"weird-tier"},
			want:      "high",
		},
		{
			// "off" ranks below every real tier, so it maps to the model's
			// lowest instead of being dropped.
			name:      "off maps to the lowest supported tier",
			requested: "off",
			supported: []string{"medium", "high"},
			want:      "medium",
		},
		{
			name:      "case and spacing are normalised",
			requested: " HIGH ",
			supported: []string{"high", "max"},
			want:      "high",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeReasoningEffort(tc.requested, tc.supported); got != tc.want {
				t.Fatalf("normalizeReasoningEffort(%q, %v) = %q, want %q",
					tc.requested, tc.supported, got, tc.want)
			}
		})
	}
}

func TestApplyEffortDowngradeInPlace(t *testing.T) {
	t.Run("top-level reasoning_effort is rewritten", func(t *testing.T) {
		obj := map[string]any{"reasoning_effort": "xhigh"}
		if !applyEffortDowngradeInPlace(obj, []string{"low", "high"}) {
			t.Fatal("expected a change")
		}
		if obj["reasoning_effort"] != "high" {
			t.Fatalf("reasoning_effort = %v", obj["reasoning_effort"])
		}
	})

	t.Run("nested reasoning.effort is rewritten", func(t *testing.T) {
		obj := map[string]any{"reasoning": map[string]any{"effort": "max"}}
		if !applyEffortDowngradeInPlace(obj, []string{"medium"}) {
			t.Fatal("expected a change")
		}
		reasoning := obj["reasoning"].(map[string]any)
		if reasoning["effort"] != "medium" {
			t.Fatalf("reasoning.effort = %v", reasoning["effort"])
		}
	})

	t.Run("both spellings are rewritten together", func(t *testing.T) {
		obj := map[string]any{
			"reasoning_effort": "max",
			"reasoning":        map[string]any{"effort": "max"},
		}
		if !applyEffortDowngradeInPlace(obj, []string{"low"}) {
			t.Fatal("expected a change")
		}
		if obj["reasoning_effort"] != "low" {
			t.Fatalf("reasoning_effort = %v", obj["reasoning_effort"])
		}
		if obj["reasoning"].(map[string]any)["effort"] != "low" {
			t.Fatalf("reasoning.effort = %v", obj["reasoning"].(map[string]any)["effort"])
		}
	})

	t.Run("no tier metadata leaves the body untouched", func(t *testing.T) {
		obj := map[string]any{"reasoning_effort": "max"}
		if applyEffortDowngradeInPlace(obj, nil) {
			t.Fatal("nil tiers must not modify the body")
		}
		if obj["reasoning_effort"] != "max" {
			t.Fatalf("reasoning_effort = %v", obj["reasoning_effort"])
		}
	})

	t.Run("supported tier is not rewritten", func(t *testing.T) {
		obj := map[string]any{"reasoning_effort": "high"}
		if applyEffortDowngradeInPlace(obj, []string{"low", "high"}) {
			t.Fatal("a supported tier must not count as a change")
		}
	})

	t.Run("a body without any effort is untouched", func(t *testing.T) {
		obj := map[string]any{"model": "m"}
		if applyEffortDowngradeInPlace(obj, []string{"low"}) {
			t.Fatal("nothing to change")
		}
	})
}

// The downgrade must reach the outgoing body through the pipeline, not only as
// a standalone helper.
func TestPipelineAppliesEffortDowngrade(t *testing.T) {
	body := []byte(`{"model":"serve-alpha","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh"}`)
	out := prepareUpstreamBody(body, nil, nil, "serve-alpha", []string{"low", "high"})
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high", obj["reasoning_effort"])
	}
}
