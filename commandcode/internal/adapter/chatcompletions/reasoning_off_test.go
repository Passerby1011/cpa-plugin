package chatcompletions

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// offModels are the five ids measured to ACCEPT reasoning_effort "off" and to
// actually stop thinking (0 reasoning tokens; GOAT key, 2026-10-23).
var offModels = []string{
	"deepseek/deepseek-v4-pro",
	"deepseek/deepseek-v4-flash",
	"deepseek/deepseek-v4-flash-vision-exp",
	"deepseek/deepseek-v4.1-flash",
	"deepseek/deepseek-v4.1-flash-fast",
}

// refuseOffModels are the measured ids that REFUSE "off" outright ("does not
// support reasoning_effort \"off\"").
var refuseOffModels = []string{"deepseek/deepseek-v4-flash-fast", "moonshotai/Kimi-K3"}

// TestOffCapableTableMatchesTheMeasuredSet pins the table itself, so a model
// added to (or dropped from) the table without a measurement fails here rather
// than in production as a 400.
func TestOffCapableTableMatchesTheMeasuredSet(t *testing.T) {
	for _, id := range offModels {
		if !SupportsReasoningOff(id) {
			t.Errorf("%s was measured to take reasoning_effort \"off\" but is not in the table", id)
		}
	}
	for _, id := range refuseOffModels {
		if SupportsReasoningOff(id) {
			t.Errorf("%s refuses \"off\" upstream but is in the table", id)
		}
	}
	if got := len(OffCapableModelIDs()); got != len(offModels) {
		t.Errorf("table holds %d models, want exactly the %d measured ones: %v",
			got, len(offModels), OffCapableModelIDs())
	}
}

// TestOffStateGoesAsOffForModelsThatTakeIt pins the positive half across all
// three client formats, for both client spellings: OpenAI's "none" and a
// client's literal "off" both reach the Provider API as "off".
func TestOffStateGoesAsOffForModelsThatTakeIt(t *testing.T) {
	for _, model := range offModels {
		for _, effort := range []string{"none", "off", "OFF", "NONE"} {
			t.Run(model+"/"+effort, func(t *testing.T) {
				// openai source: forwarded verbatim except for the off-state.
				got := buildAndDecode(t, model, "openai", `{
					"model":"client-name","stream":false,
					"reasoning_effort":"`+effort+`",
					"messages":[{"role":"user","content":"1+1=?"}]}`)
				if got["reasoning_effort"] != "off" {
					t.Fatalf("openai %q -> reasoning_effort = %v, want \"off\"", effort, got["reasoning_effort"])
				}
			})
		}
	}
}

// TestOffStateIsOmittedForModelsThatRefuseIt pins the negative half: a model
// that refuses "off" gets NO reasoning_effort field rather than a guaranteed
// 400. "none" is doubly wrong for those (the Provider API does not know the
// option at all), so dropping it is the only honest translation.
func TestOffStateIsOmittedForModelsThatRefuseIt(t *testing.T) {
	for _, model := range refuseOffModels {
		for _, effort := range []string{"none", "off"} {
			t.Run(model+"/"+effort, func(t *testing.T) {
				got := buildAndDecode(t, model, "openai", `{
					"model":"client-name","stream":false,
					"reasoning_effort":"`+effort+`",
					"messages":[{"role":"user","content":"hi"}]}`)
				if _, ok := got["reasoning_effort"]; ok {
					t.Fatalf("refusing model was sent reasoning_effort = %v", got["reasoning_effort"])
				}
			})
		}
	}
}

// TestOrdinaryLevelsPassThroughUntouched pins that the rewrite is targeted:
// levels the upstream accepts broadly must not be rewritten, and neither must
// an absent field.
func TestOrdinaryLevelsPassThroughUntouched(t *testing.T) {
	for _, model := range []string{"deepseek/deepseek-v4.1-flash", "deepseek/deepseek-v4-flash-fast", "moonshotai/Kimi-K3"} {
		for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
			got := buildAndDecode(t, model, "openai", `{
				"model":"client-name","stream":false,
				"reasoning_effort":"`+effort+`",
				"messages":[{"role":"user","content":"hi"}]}`)
			if got["reasoning_effort"] != effort {
				t.Errorf("%s/%s: reasoning_effort = %v, want it untouched", model, effort, got["reasoning_effort"])
			}
		}
		got := buildAndDecode(t, model, "openai", `{"model":"client-name","messages":[{"role":"user","content":"hi"}]}`)
		if _, ok := got["reasoning_effort"]; ok {
			t.Errorf("%s: an absent effort was invented: %v", model, got["reasoning_effort"])
		}
	}
}

// TestOffStateAlsoAppliesToTranslatedSources pins that the rule holds for the
// two non-OpenAI client formats too. Those build the request structurally (the
// Claude leg from a thinking budget, the Responses leg from reasoning.effort),
// and before this the off-state reached the upstream in the client's own
// spelling - which the Provider API refuses.
func TestOffStateAlsoAppliesToTranslatedSources(t *testing.T) {
	// claude -> chat: budget 0 with a model that takes "off" must send "off".
	ts := zeroBudgetSupport()
	for _, model := range offModels {
		got := buildAndDecode(t, model, "claude", `{
			"model":"client-name","max_tokens":64,
			"thinking":{"type":"enabled","budget_tokens":0},
			"messages":[{"role":"user","content":"hi"}]}`)
		if got["reasoning_effort"] != "off" {
			t.Errorf("claude/%s: reasoning_effort = %v, want \"off\"", model, got["reasoning_effort"])
		}
	}
	// ... and must be dropped entirely for a model that refuses it.
	for _, model := range refuseOffModels {
		got := buildAndDecode(t, model, "claude", `{
			"model":"client-name","max_tokens":64,
			"thinking":{"type":"enabled","budget_tokens":0},
			"messages":[{"role":"user","content":"hi"}]}`)
		if _, ok := got["reasoning_effort"]; ok {
			t.Errorf("claude/%s: refusing model was sent %v", model, got["reasoning_effort"])
		}
	}
	_ = ts

	// openai-response -> chat: reasoning.effort "none" follows the same rule.
	got := buildAndDecode(t, "deepseek/deepseek-v4-pro", "openai-response", `{
		"model":"client-name","reasoning":{"effort":"none"},
		"input":[{"type":"message","role":"user","content":"hi"}]}`)
	if got["reasoning_effort"] != "off" {
		t.Errorf("openai-response/deepseek-v4-pro: reasoning_effort = %v, want \"off\"", got["reasoning_effort"])
	}
	got = buildAndDecode(t, "moonshotai/Kimi-K3", "openai-response", `{
		"model":"client-name","reasoning":{"effort":"none"},
		"input":[{"type":"message","role":"user","content":"hi"}]}`)
	if _, ok := got["reasoning_effort"]; ok {
		t.Errorf("openai-response/Kimi-K3: refusing model was sent %v", got["reasoning_effort"])
	}
}

// TestReasoningEffortMustBeAString pins the type guard: a non-string effort is
// refused rather than guessed at, so no request silently loses its control.
func TestReasoningEffortMustBeAString(t *testing.T) {
	_, eErr := BuildRequest("deepseek/deepseek-v4-pro", "openai",
		[]byte(`{"model":"m","reasoning_effort":{"level":"off"},"messages":[]}`), nil)
	if eErr == nil {
		t.Fatal("a non-string reasoning_effort was accepted")
	}
	if eErr.Message == "" {
		t.Error("the refusal carries no message")
	}
}

// zeroBudgetSupport is the thinking declaration that lets a client ask for "no
// thinking": zero allowed, and "none" among the offered levels.
func zeroBudgetSupport() *pluginapi.ThinkingSupport {
	return &pluginapi.ThinkingSupport{
		ZeroAllowed: true,
		Levels:      []string{"none", "low", "medium", "high", "max"},
	}
}

// buildAndDecode runs one client body through BuildRequest and returns the
// upstream body as a decoded map.
//
// The thinking capability declares "none" as an offered level with zero budget
// allowed, which is precisely the client state this file is about: the client
// asked for no thinking. The target model - not the declaration - decides
// whether that can be spelled "off" or must be dropped.
func buildAndDecode(t *testing.T, upstreamModel, sourceFormat, body string) map[string]any {
	t.Helper()
	out, eErr := BuildRequest(upstreamModel, sourceFormat, []byte(body), zeroBudgetSupport())
	if eErr != nil {
		t.Fatalf("BuildRequest(%s, %s): %v", upstreamModel, sourceFormat, eErr)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal upstream body: %v (%s)", err, out)
	}
	return m
}
