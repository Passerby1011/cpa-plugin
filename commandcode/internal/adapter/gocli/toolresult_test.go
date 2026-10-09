package gocli

import (
	"encoding/json"
	"strings"
	"testing"
)

// toolResultBlocks extracts every tool-result block from an envelope's
// messages in order.
func toolResultBlocks(t *testing.T, envelope []byte) []map[string]any {
	t.Helper()
	var env struct {
		Params struct {
			Messages []struct {
				Role    string           `json:"role"`
				Content []map[string]any `json:"content"`
			} `json:"messages"`
		} `json:"params"`
	}
	if err := json.Unmarshal(envelope, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	out := []map[string]any{}
	for _, m := range env.Params.Messages {
		for _, part := range m.Content {
			if part["type"] == "tool-result" {
				out = append(out, part)
			}
		}
	}
	return out
}

// TestToolResultCarriesToolName pins the field the upstream requires and that
// this package used to omit.
//
// A tool message in the OpenAI shape carries only tool_call_id; the CommandCode
// envelope requires the matching tool NAME on the tool-result block, and rejects
// the entire request with 400 when it is undefined:
//
//	Validation error: Invalid option: expected one of "user"|"assistant" at
//	"params.messages[N].role"; ... expected string, received undefined
//
// A plain chat therefore worked while every real agent client (Claude Code,
// Codex) failed, because only they send tool messages.
func TestToolResultCarriesToolName(t *testing.T) {
	body := []byte(`{
	  "model": "deepseek/deepseek-v4-flash",
	  "messages": [
	    {"role": "user", "content": "call the tool"},
	    {"role": "assistant", "content": "calling", "tool_calls": [
	      {"id": "call_1", "type": "function",
	       "function": {"name": "get_weather", "arguments": "{\"city\":\"Hefei\"}"}}
	    ]},
	    {"role": "tool", "tool_call_id": "call_1", "content": "22C"}
	  ]
	}`)

	envelope, err := BuildEnvelope(body, Options{SystemPlaceholder: true})
	if err != nil {
		t.Fatalf("BuildEnvelope: %v", err)
	}
	blocks := toolResultBlocks(t, envelope)
	if len(blocks) != 1 {
		t.Fatalf("tool-result blocks = %d, want 1", len(blocks))
	}
	got, ok := blocks[0]["toolName"].(string)
	if !ok {
		t.Fatalf("toolName is %T, want string (the upstream rejects a non-string)", blocks[0]["toolName"])
	}
	if got != "get_weather" {
		t.Errorf("toolName = %q, want %q (resolved from the assistant tool_call)", got, "get_weather")
	}
	if id, _ := blocks[0]["toolCallId"].(string); id != "call_1" {
		t.Errorf("toolCallId = %q, want call_1", id)
	}
}

// TestToolNameResolvedAcrossMultipleToolCalls pins the prepass: the name for a
// result is only discoverable from an EARLIER assistant message, and several
// calls must not be confused with one another.
func TestToolNameResolvedAcrossMultipleToolCalls(t *testing.T) {
	body := []byte(`{
	  "model": "m",
	  "messages": [
	    {"role": "user", "content": "go"},
	    {"role": "assistant", "content": "", "tool_calls": [
	      {"id": "a1", "type": "function", "function": {"name": "alpha", "arguments": "{}"}},
	      {"id": "b2", "type": "function", "function": {"name": "beta", "arguments": "{}"}}
	    ]},
	    {"role": "tool", "tool_call_id": "b2", "content": "B"},
	    {"role": "tool", "tool_call_id": "a1", "content": "A"}
	  ]
	}`)
	envelope, err := BuildEnvelope(body, Options{})
	if err != nil {
		t.Fatalf("BuildEnvelope: %v", err)
	}
	blocks := toolResultBlocks(t, envelope)
	if len(blocks) != 2 {
		t.Fatalf("tool-result blocks = %d, want 2", len(blocks))
	}
	byID := map[string]string{}
	for _, b := range blocks {
		id, _ := b["toolCallId"].(string)
		name, _ := b["toolName"].(string)
		byID[id] = name
	}
	if byID["a1"] != "alpha" || byID["b2"] != "beta" {
		t.Fatalf("tool names mismatched: %v", byID)
	}
}

// TestToolNameAlwaysAString pins that an unresolvable name still serialises as a
// string. The upstream validates the TYPE: a null or absent field is the exact
// failure this bug was.
func TestToolNameAlwaysAString(t *testing.T) {
	body := []byte(`{
	  "model": "m",
	  "messages": [
	    {"role": "user", "content": "x"},
	    {"role": "tool", "tool_call_id": "orphan", "content": "no matching call"}
	  ]
	}`)
	envelope, err := BuildEnvelope(body, Options{})
	if err != nil {
		t.Fatalf("BuildEnvelope: %v", err)
	}
	blocks := toolResultBlocks(t, envelope)
	if len(blocks) != 1 {
		t.Fatalf("tool-result blocks = %d, want 1", len(blocks))
	}
	if _, ok := blocks[0]["toolName"].(string); !ok {
		t.Fatalf("toolName must always be a string, got %#v", blocks[0]["toolName"])
	}
	// And it must be present in the JSON, not omitted: an absent key is
	// "received undefined" upstream.
	raw, _ := json.Marshal(blocks[0])
	if !strings.Contains(string(raw), `"toolName"`) {
		t.Fatalf("toolName missing from the serialised block: %s", raw)
	}
}
