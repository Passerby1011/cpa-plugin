package gocli

import (
	"encoding/json"
	"testing"
)

// TestGoCLINeverSendsTheOffState pins the CLI transport's share of the reasoning
// contract.
//
// WHY: the Provider API's off-state word ("off") was measured there and nowhere
// else. CommandCode's /alpha/generate has NOT been measured with it, and the
// whole reason the plugin refuses to guess is that a wrong value there fails
// EVERY request that asks to turn thinking off - the Go plan would be unusable
// for exactly the clients that care most. So the off-state is dropped on this
// leg (the model keeps its own default) while ordinary levels still pass
// through untouched.
func TestGoCLINeverSendsTheOffState(t *testing.T) {
	base := `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`

	for _, effort := range []string{"none", "off", "NONE", "Off"} {
		body := `{"model":"deepseek/deepseek-v4.1-flash","reasoning_effort":"` + effort + `","messages":[{"role":"user","content":"hi"}]}`
		env, err := BuildEnvelope([]byte(body), Options{})
		if err != nil {
			t.Fatalf("BuildEnvelope(%q): %v", effort, err)
		}
		params := envelopeParams(t, env)
		if _, ok := params["reasoning_effort"]; ok {
			t.Errorf("effort %q was forwarded to /alpha/generate as %v; the CLI surface is unmeasured for it",
				effort, params["reasoning_effort"])
		}
	}

	// Ordinary levels still go through: dropping the off-state must not turn
	// into dropping every reasoning control.
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
		body := `{"model":"deepseek/deepseek-v4.1-flash","reasoning_effort":"` + effort + `","messages":[{"role":"user","content":"hi"}]}`
		env, err := BuildEnvelope([]byte(body), Options{})
		if err != nil {
			t.Fatalf("BuildEnvelope(%q): %v", effort, err)
		}
		if got := envelopeParams(t, env)["reasoning_effort"]; got != effort {
			t.Errorf("effort %q reached /alpha/generate as %v", effort, got)
		}
	}

	// An absent effort stays absent rather than being invented.
	env, err := BuildEnvelope([]byte(base), Options{})
	if err != nil {
		t.Fatalf("BuildEnvelope: %v", err)
	}
	if _, ok := envelopeParams(t, env)["reasoning_effort"]; ok {
		t.Error("an absent reasoning_effort was invented on the CLI surface")
	}
}

// envelopeParams returns the envelope's params block.
func envelopeParams(t *testing.T, env []byte) map[string]any {
	t.Helper()
	var decoded struct {
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(env, &decoded); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, env)
	}
	if decoded.Params == nil {
		t.Fatalf("envelope has no params block: %s", env)
	}
	return decoded.Params
}
