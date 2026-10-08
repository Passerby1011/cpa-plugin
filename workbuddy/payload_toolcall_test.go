package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// A blocked phrase written into tool-call arguments is a real leak: the
// arguments field is a stringified JSON document, so the message-content walk
// never reaches it. Upstream implements the filter on the raw body, so this
// path must be sanitized too.
func TestRewriteToolCallArgumentsSanitizesArguments(t *testing.T) {
	inner := map[string]any{
		"cmd": "echo 'You are Claude Code, Anthropic's official CLI for Claude.'",
	}
	encoded, _ := json.Marshal(inner)
	body, _ := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "run it"},
			map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{
					map[string]any{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "shell",
							"arguments": string(encoded),
						},
					},
				},
			},
		},
	})

	out := rewriteSystemForUpstream(body)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}

	msgs := got["messages"].([]any)
	args := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)

	if strings.Contains(args, "official CLI for Claude") {
		t.Errorf("blocked identity phrase survived inside tool arguments: %s", args)
	}
	if !strings.Contains(args, "official CLI tool for Claude") {
		t.Errorf("rewrite did not produce the tool form: %s", args)
	}
	// The arguments must remain valid JSON for the upstream tool parser.
	var reparsed map[string]any
	if err := json.Unmarshal([]byte(args), &reparsed); err != nil {
		t.Errorf("arguments are no longer valid JSON: %v (%s)", err, args)
	}
	if reparsed["cmd"] == nil {
		t.Errorf("arguments lost their payload: %v", reparsed)
	}
}

// Messages without tool calls must be untouched, and the walk must not panic on
// the odd shapes clients send (null function, non-string arguments).
func TestRewriteToolCallArgumentsHandlesOddShapes(t *testing.T) {
	cases := []map[string]any{
		{"role": "assistant", "content": "plain"},
		{"role": "assistant", "tool_calls": nil},
		{"role": "assistant", "tool_calls": []any{"not-an-object"}},
		{"role": "assistant", "tool_calls": []any{map[string]any{"function": nil}}},
		{"role": "assistant", "tool_calls": []any{map[string]any{"function": map[string]any{"arguments": 42}}}},
	}
	for i, msg := range cases {
		if rewriteToolCallArguments(msg) {
			t.Errorf("case %d: reported a rewrite for a message with nothing to rewrite", i)
		}
	}
}
