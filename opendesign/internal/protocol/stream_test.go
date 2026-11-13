package protocol

import (
	"strings"
	"testing"
)

// feedAll runs every frame of an upstream stream through a decoder, collecting
// the canonical deltas.
func feedAll(t *testing.T, schema Schema, frames []string) []Delta {
	t.Helper()
	dec := NewStreamDecoder(schema)
	var out []Delta
	for _, frame := range frames {
		deltas, err := dec.Feed([]byte(frame))
		if err != nil {
			t.Fatalf("feed %q: %v", frame, err)
		}
		out = append(out, deltas...)
	}
	return out
}

// The headline contract: thinking text must survive every crossing, so a client
// on one schema talking to a model declared on another still sees the thought.
func TestThinkingSurvivesEveryCrossing(t *testing.T) {
	cases := []struct {
		name    string
		up      Schema
		down    Schema
		frames  []string
		wantKey string
	}{
		{
			name: "claude -> chat",
			up:   SchemaClaude,
			down: SchemaChat,
			frames: []string{
				`data: {"type":"message_start","message":{"id":"msg_1","model":"m"}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"ponder"}}`,
				`data: {"type":"content_block_stop","index":0}`,
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`,
				`data: {"type":"content_block_stop","index":1}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
			},
			wantKey: `"reasoning_content":"ponder"`,
		},
		{
			name: "responses -> chat",
			up:   SchemaResponses,
			down: SchemaChat,
			frames: []string{
				`data: {"type":"response.created","response":{"id":"resp_1","model":"m"}}`,
				`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"delta":"ponder"}`,
				`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"delta":"answer"}`,
			},
			wantKey: `"reasoning_content":"ponder"`,
		},
		{
			name: "chat -> claude",
			up:   SchemaChat,
			down: SchemaClaude,
			frames: []string{
				`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"reasoning_content":"ponder"},"finish_reason":null}]}`,
				`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":null}]}`,
			},
			wantKey: `"thinking":"ponder"`,
		},
		{
			name: "chat -> responses",
			up:   SchemaChat,
			down: SchemaResponses,
			frames: []string{
				`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"reasoning_content":"ponder"},"finish_reason":null}]}`,
				`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":null}]}`,
			},
			wantKey: `"type":"response.reasoning_summary_text.delta"`,
		},
		{
			name: "claude -> responses",
			up:   SchemaClaude,
			down: SchemaResponses,
			frames: []string{
				`data: {"type":"message_start","message":{"id":"msg_1","model":"m"}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"ponder"}}`,
				`data: {"type":"content_block_stop","index":0}`,
			},
			wantKey: `"type":"response.reasoning_summary_text.delta"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deltas := feedAll(t, tc.up, tc.frames)
			enc := NewStreamEncoder(tc.down)
			var out strings.Builder
			for _, f := range enc.Start("", "m", 0) {
				out.WriteString(string(f))
			}
			for _, d := range deltas {
				if d.Kind == DeltaStop || d.Kind == DeltaUsage {
					continue
				}
				for _, f := range enc.Emit(d) {
					out.WriteString(string(f))
				}
			}
			if !strings.Contains(out.String(), tc.wantKey) {
				t.Errorf("thinking was lost crossing %s -> %s\nwant substring: %s\ngot:\n%s",
					tc.up, tc.down, tc.wantKey, out.String())
			}
		})
	}
}

// Answer text must survive the tactic thinking does, and must not be doubled by
// a "done" event repeating what the deltas already carried.
func TestAnswerTextIsNotDoubled(t *testing.T) {
	frames := []string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m"}}`,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"answer"}`,
		`data: {"type":"response.output_text.done","item_id":"msg_1","output_index":0,"text":"answer"}`,
	}
	deltas := feedAll(t, SchemaResponses, frames)
	var text strings.Builder
	for _, d := range deltas {
		if d.Kind == DeltaText {
			text.WriteString(d.Text)
		}
	}
	if text.String() != "answer" {
		t.Errorf("text = %q, want exactly one copy", text.String())
	}
}

// A chat encoder must close its stream with a finish_reason and [DONE], which is
// what a chat client waits for.
func TestChatEncoderClosesWithDone(t *testing.T) {
	enc := NewStreamEncoder(SchemaChat)
	frames := enc.Finish(StopEnd, &Usage{Input: 3, Output: 2})
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2 (the finish chunk and [DONE])", len(frames))
	}
	if string(frames[1]) != "[DONE]" {
		t.Errorf("last frame = %q", frames[1])
	}
	if !strings.Contains(string(frames[0]), `"finish_reason":"stop"`) {
		t.Errorf("finish chunk = %s", frames[0])
	}
}

// A Claude encoder must close every content block before it reports the stop
// reason: Anthropic clients abort on a message_delta with a block still open.
func TestClaudeEncoderClosesBlocksBeforeStop(t *testing.T) {
	enc := NewStreamEncoder(SchemaClaude)
	var out strings.Builder
	for _, f := range enc.Start("msg_1", "m", 0) {
		out.WriteString(string(f))
	}
	for _, f := range enc.Emit(Delta{Kind: DeltaThinking, Text: "ponder", Index: 0}) {
		out.WriteString(string(f))
	}
	for _, f := range enc.Emit(Delta{Kind: DeltaText, Text: "answer", Index: 1}) {
		out.WriteString(string(f))
	}
	for _, f := range enc.Finish(StopEnd, &Usage{Input: 3, Output: 2}) {
		out.WriteString(string(f))
	}
	body := out.String()
	if strings.Count(body, "content_block_stop") < 2 {
		t.Errorf("every block must be closed:\n%s", body)
	}
	lastStop := strings.LastIndex(body, "message_delta")
	lastBlockStop := strings.LastIndex(body, "content_block_stop")
	if lastStop < 0 || lastBlockStop < 0 || lastBlockStop > lastStop {
		t.Errorf("a block was still open at message_delta:\n%s", body)
	}
}

// A tool call crossing chat -> Claude must become a tool_use block with the
// arguments as input_json_delta fragments.
func TestToolCallCrossesIntoClaudeToolUse(t *testing.T) {
	frames := []string{
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"SF\"}"}}]},"finish_reason":null}]}`,
	}
	deltas := feedAll(t, SchemaChat, frames)
	enc := NewStreamEncoder(SchemaClaude)
	var out strings.Builder
	for _, f := range enc.Start("msg_1", "m", 0) {
		out.WriteString(string(f))
	}
	for _, d := range deltas {
		if d.Kind == DeltaStop || d.Kind == DeltaUsage {
			continue
		}
		for _, f := range enc.Emit(d) {
			out.WriteString(string(f))
		}
	}
	body := out.String()
	if !strings.Contains(body, `"type":"tool_use"`) || !strings.Contains(body, `"name":"get_weather"`) {
		t.Errorf("tool_use block missing:\n%s", body)
	}
	if !strings.Contains(body, `"partial_json":"{\"city\":"`) {
		t.Errorf("argument fragment missing:\n%s", body)
	}
}

// A Claude signature delta must reach a Claude client, because the next turn's
// thinking block is refused without it.
func TestSignatureSurvivesToClaudeClient(t *testing.T) {
	frames := []string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"ponder"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}`,
		`data: {"type":"content_block_stop","index":0}`,
	}
	deltas := feedAll(t, SchemaClaude, frames)
	enc := NewStreamEncoder(SchemaClaude)
	var out strings.Builder
	for _, f := range enc.Start("msg_1", "m", 0) {
		out.WriteString(string(f))
	}
	for _, d := range deltas {
		for _, f := range enc.Emit(d) {
			out.WriteString(string(f))
		}
	}
	if !strings.Contains(out.String(), `"signature":"sig-abc"`) {
		t.Errorf("signature lost:\n%s", out.String())
	}
}

// An upstream error event must surface as an error rather than a silently empty
// answer.
func TestUpstreamErrorEventFails(t *testing.T) {
	dec := NewStreamDecoder(SchemaClaude)
	_, err := dec.Feed([]byte(`data: {"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`))
	if err == nil {
		t.Fatal("an upstream error event must fail the stream")
	}
	if !strings.Contains(err.Error(), "overloaded") {
		t.Errorf("error = %v, want the vendor's own words", err)
	}
}

// A Responses stream that ends because of the output cap must report the
// truncation, not a normal completion.
func TestResponsesIncompleteReportsTruncation(t *testing.T) {
	frames := []string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m"}}`,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"partial"}`,
		`data: {"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":3,"output_tokens":2}}}`,
	}
	deltas := feedAll(t, SchemaResponses, frames)
	var stop string
	for _, d := range deltas {
		if d.Kind == DeltaStop {
			stop = d.StopReason
		}
	}
	if stop != StopLength {
		t.Errorf("stop = %q, want %q", stop, StopLength)
	}
}

func TestFramePayloadReadsNamedEvents(t *testing.T) {
	cases := map[string]string{
		"data: {\"type\":\"x\"}":                       `{"type":"x"}`,
		"event: message_start\ndata: {\"type\":\"x\"}": `{"type":"x"}`,
		`{"type":"x"}`:                                 `{"type":"x"}`,
	}
	for frame, want := range cases {
		if got := FramePayload([]byte(frame)); got != want {
			t.Errorf("FramePayload(%q) = %q, want %q", frame, got, want)
		}
	}
}
