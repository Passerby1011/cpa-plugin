package main

import (
	"strings"
	"testing"
)

// Does the plugin's own frame pipeline preserve a NON-EMPTY reasoning_content?
//
// The upstream provably emits it (measured: 342 chars for a plain request), but
// an end-to-end call through the host delivered 0 chars to the client. This
// isolates the plugin's leg of that path: feed a real upstream frame through
// cleanChunkJSON and see whether the thinking survives.
func TestCleanChunkPreservesNonEmptyReasoning(t *testing.T) {
	frame := `{"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Let me think about the ball."}}]}`

	out := cleanChunkJSON(frame)
	if !strings.Contains(out, "reasoning_content") {
		t.Errorf("non-empty reasoning_content was stripped: %q", out)
	}
	if !strings.Contains(out, "Let me think about the ball.") {
		t.Errorf("reasoning text was lost: %q", out)
	}
}

// Empty noise fields must still be dropped (that is the existing contract).
func TestCleanChunkDropsEmptyReasoning(t *testing.T) {
	frame := `{"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"","content":"hi"}}]}`

	out := cleanChunkJSON(frame)
	if strings.Contains(out, "reasoning_content") {
		t.Errorf("empty reasoning_content should be dropped as noise: %q", out)
	}
	if !strings.Contains(out, `"hi"`) {
		t.Errorf("meaningful content was dropped: %q", out)
	}
}

// The aggregating (non-streaming) path must put the thinking back on the
// message. Upstream frames arrive as separate deltas, so this also proves the
// deltas are joined rather than the last one winning.
func TestAggregateCompletionKeepsReasoning(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","model":"deep-model","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Step one. "}}]}`,
		`data: {"id":"c1","model":"deep-model","choices":[{"index":0,"delta":{"reasoning_content":"Step two."}}]}`,
		`data: {"id":"c1","model":"deep-model","choices":[{"index":0,"delta":{"content":"0.05"}}]}`,
		`data: {"id":"c1","model":"deep-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	raw, err := aggregateCompletion(strings.NewReader(sse), "deep-model")
	if err != nil {
		t.Fatalf("aggregateCompletion: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "Step one. ") || !strings.Contains(got, "Step two.") {
		t.Errorf("reasoning deltas were not joined: %s", got)
	}
	if !strings.Contains(got, "0.05") {
		t.Errorf("content was lost: %s", got)
	}
}
