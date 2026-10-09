package gocli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/hex-ci/cpa-plugin/commandcode/internal/errclass"
)

// testModel is the model name threaded through every converter under test: the
// synthesized envelope must report the CALLER's model, not a plugin placeholder.
const testModel = "deepseek-v4.1-flash"

// cliFrame renders one SSE frame in the shape the CLI transport emits: a
// single data: line followed by a blank separator line.
func cliFrame(payload string) string {
	return "data: " + payload + "\n\n"
}

// chunkView is one decoded openai-target chunk: the outer envelope, its
// single choice, and (when present) the delta object.
type chunkView struct {
	chunk  map[string]any
	choice map[string]any
	delta  map[string]any
}

// parseOpenAIChunk decodes one bare openai-target event payload and
// asserts the converter's fixed chunk identity.
func parseOpenAIChunk(t *testing.T, raw []byte) chunkView {
	t.Helper()
	var chunk map[string]any
	if err := json.Unmarshal(raw, &chunk); err != nil {
		t.Fatalf("openai-target event is not a JSON chunk: %v (%q)", err, raw)
	}
	if chunk["id"] != cliChunkID || chunk["object"] != "chat.completion.chunk" || chunk["model"] != testModel {
		t.Fatalf("chunk identity wrong: %v", chunk)
	}
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("chunk must carry exactly one choice: %v", chunk["choices"])
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		t.Fatalf("choice is not an object: %v", choices[0])
	}
	delta, _ := choice["delta"].(map[string]any)
	return chunkView{chunk: chunk, choice: choice, delta: delta}
}

// TestConverterFeedMultiEventStream feeds one realistic CLI stream
// (reasoning deltas, text deltas, a tool call, and a usage-carrying
// finish plus the terminal sentinel) and checks that every frame reaches
// the client exactly once, in arrival order.
func TestConverterFeedMultiEventStream(t *testing.T) {
	stream := cliFrame(`{"type":"reasoning-start"}`) +
		cliFrame(`{"type":"reasoning-delta","text":"We "}`) +
		cliFrame(`{"type":"reasoning-delta","text":"think."}`) +
		cliFrame(`{"type":"reasoning-end"}`) +
		cliFrame(`{"type":"text-delta","text":"Hello "}`) +
		cliFrame(`{"type":"text-delta","text":"world"}`) +
		cliFrame(`{"type":"cache-write-tokens","tokens":12}`) +
		cliFrame(`{"type":"tool-call","toolCallId":"call_1","toolName":"lookup","input":{"query":"go"}}`) +
		cliFrame(`{"type":"finish","finishReason":"tool_calls","totalUsage":{"inputTokens":11,"outputTokens":7,"inputTokenDetails":{"cacheReadTokens":3},"outputTokenDetails":{"reasoningTokens":4}}}`) +
		cliFrame("[DONE]")

	c := NewConverter("openai", testModel)
	events, done, eErr := c.Feed([]byte(stream))
	if eErr != nil {
		t.Fatalf("Feed: %v", eErr)
	}
	if !done {
		t.Fatal("the terminal [DONE] must report the stream done")
	}
	if len(events) != 6 {
		t.Fatalf("want 6 chunks (2 reasoning + 2 text + 1 tool + 1 finish), got %d", len(events))
	}

	want := []struct{ kind, value string }{
		{"reasoning", "We "},
		{"reasoning", "think."},
		{"text", "Hello "},
		{"text", "world"},
		{"tool-call", "call_1"},
		{"finish", "tool_calls"},
	}
	for i, w := range want {
		v := parseOpenAIChunk(t, events[i])
		switch w.kind {
		case "reasoning":
			if v.delta["reasoning_content"] != w.value {
				t.Errorf("event %d reasoning_content = %v, want %q", i, v.delta["reasoning_content"], w.value)
			}
		case "text":
			if v.delta["content"] != w.value {
				t.Errorf("event %d content = %v, want %q", i, v.delta["content"], w.value)
			}
		case "tool-call":
			calls, _ := v.delta["tool_calls"].([]any)
			if len(calls) != 1 {
				t.Fatalf("event %d must carry exactly one tool call: %v", i, v.delta["tool_calls"])
			}
			call, _ := calls[0].(map[string]any)
			fn, _ := call["function"].(map[string]any)
			if call["index"] != float64(0) || call["id"] != w.value || call["type"] != "function" ||
				fn["name"] != "lookup" || fn["arguments"] != `{"query":"go"}` {
				t.Errorf("event %d tool call wrong: %v", i, call)
			}
		case "finish":
			if v.choice["finish_reason"] != w.value {
				t.Errorf("event %d finish_reason = %v, want %q", i, v.choice["finish_reason"], w.value)
			}
			usage, _ := v.chunk["usage"].(map[string]any)
			if usage == nil {
				t.Fatalf("event %d finish chunk has no usage: %v", i, v.chunk)
			}
			if usage["prompt_tokens"] != float64(11) || usage["completion_tokens"] != float64(7) || usage["total_tokens"] != float64(18) {
				t.Errorf("event %d usage counters wrong: %v", i, usage)
			}
			pd, _ := usage["prompt_tokens_details"].(map[string]any)
			cd, _ := usage["completion_tokens_details"].(map[string]any)
			if pd["cached_tokens"] != float64(3) {
				t.Errorf("event %d cached_tokens = %v, want 3", i, pd["cached_tokens"])
			}
			if cd["reasoning_tokens"] != float64(4) {
				t.Errorf("event %d reasoning_tokens = %v, want 4", i, cd["reasoning_tokens"])
			}
		}
	}

	if late, done2, eErr2 := c.Feed([]byte(cliFrame(`{"type":"text-delta","text":"late"}`))); eErr2 != nil || !done2 || len(late) != 0 {
		t.Fatalf("feed after done must be a no-op: events=%d done=%v err=%v", len(late), done2, eErr2)
	}
}

// TestConverterFeedDoneSentinel pins the terminal sentinel contract: a
// stream without a finish event stays open until [DONE] arrives.
func TestConverterFeedDoneSentinel(t *testing.T) {
	tests := []struct {
		name       string
		frames     []string
		wantEvents int
		wantDone   bool
	}{
		{name: "text then sentinel", frames: []string{cliFrame(`{"type":"text-delta","text":"hi"}`), cliFrame("[DONE]")}, wantEvents: 1, wantDone: true},
		{name: "sentinel alone", frames: []string{cliFrame("[DONE]")}, wantEvents: 0, wantDone: true},
		{name: "text without sentinel", frames: []string{cliFrame(`{"type":"text-delta","text":"hi"}`)}, wantEvents: 1, wantDone: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewConverter("openai", testModel)
			var got [][]byte
			done := false
			for _, frame := range tc.frames {
				evs, d, eErr := c.Feed([]byte(frame))
				if eErr != nil {
					t.Fatalf("Feed: %v", eErr)
				}
				got = append(got, evs...)
				done = d
			}
			if len(got) != tc.wantEvents {
				t.Errorf("events = %d, want %d", len(got), tc.wantEvents)
			}
			if done != tc.wantDone {
				t.Errorf("done = %v, want %v", done, tc.wantDone)
			}
		})
	}
}

// TestConverterFeedSplitEventAcrossFeeds cuts one event across two Feed
// calls - inside a multi-byte rune - and checks the buffered bytes are
// rejoined without losing or duplicating anything.
func TestConverterFeedSplitEventAcrossFeeds(t *testing.T) {
	const text = "\u8de8\u5305\u5b8c\u6574" // four 3-byte runes
	stream := cliFrame(`{"type":"reasoning-delta","text":"r1"}`) +
		cliFrame(`{"type":"text-delta","text":"`+text+`"}`) +
		cliFrame("[DONE]")
	cut := strings.Index(stream, "\u5305")
	if cut < 0 {
		t.Fatal("test fixture lost its multibyte marker")
	}
	cut++ // split inside the rune so the buffered bytes must be rejoined

	c := NewConverter("openai", testModel)
	first, done, eErr := c.Feed([]byte(stream[:cut]))
	if eErr != nil {
		t.Fatalf("first Feed: %v", eErr)
	}
	if done {
		t.Fatal("first Feed cannot be done: the second event is incomplete")
	}
	if len(first) != 1 {
		t.Fatalf("first Feed must complete the first event only, got %d", len(first))
	}
	if v := parseOpenAIChunk(t, first[0]); v.delta["reasoning_content"] != "r1" {
		t.Fatalf("first event wrong: %v", v.delta)
	}

	second, done2, eErr2 := c.Feed([]byte(stream[cut:]))
	if eErr2 != nil {
		t.Fatalf("second Feed: %v", eErr2)
	}
	if !done2 {
		t.Fatal("second Feed completes the split event and reaches [DONE]")
	}
	if len(second) != 1 {
		t.Fatalf("second Feed must complete exactly the split event, got %d", len(second))
	}
	if v := parseOpenAIChunk(t, second[0]); v.delta["content"] != text {
		t.Fatalf("split event lost content: %v", v.delta)
	}
}

// TestConverterFeedErrorEvent checks that an error event is always
// surfaced as a classified errclass error, never as a silent empty
// stream, and that events already delivered before the error survive.
func TestConverterFeedErrorEvent(t *testing.T) {
	tests := []struct {
		name       string
		prefix     string
		payload    string
		wantClass  errclass.Class
		wantStatus int
		wantRetry  bool
		wantEvents int
	}{
		{
			name:       "rate limit code",
			payload:    `{"type":"error","error":"rate_limit_exceeded","message":"slow down"}`,
			wantClass:  errclass.ClassRateLimit,
			wantStatus: 429,
			wantRetry:  true,
		},
		{
			name:       "numeric upstream status",
			payload:    `{"type":"error","error":502,"message":"bad gateway"}`,
			wantClass:  errclass.ClassUpstream,
			wantStatus: 502,
			wantRetry:  true,
		},
		{
			name:       "object quota code",
			payload:    `{"type":"error","error":{"type":"quota_exhausted"},"message":"no credits"}`,
			wantClass:  errclass.ClassQuota,
			wantStatus: 429,
			wantRetry:  true,
		},
		{
			name:       "error after a delivered delta",
			prefix:     cliFrame(`{"type":"text-delta","text":"partial"}`),
			payload:    `{"type":"error","error":"internal_error","message":"boom"}`,
			wantClass:  errclass.ClassUpstream,
			wantStatus: 0,
			wantRetry:  true,
			wantEvents: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewConverter("openai", testModel)
			events, done, eErr := c.Feed([]byte(tc.prefix + cliFrame(tc.payload)))
			if eErr == nil {
				t.Fatalf("error event must return a classified error, got %d events, done=%v", len(events), done)
			}
			if len(events) != tc.wantEvents {
				t.Errorf("events before the error = %d, want %d", len(events), tc.wantEvents)
			}
			if eErr.Class != tc.wantClass || eErr.StatusCode != tc.wantStatus || eErr.Retryable != tc.wantRetry {
				t.Errorf("classified error = %+v, want class=%q status=%d retryable=%v",
					eErr, tc.wantClass, tc.wantStatus, tc.wantRetry)
			}
			if strings.TrimSpace(eErr.Message) == "" {
				t.Error("classified error must carry a detail message")
			}
		})
	}
}

// TestCLIBaseAuthority pins base-URL normalization: trailing slashes and
// the documented /provider/v1 suffix are stripped, anything without a
// scheme and host is rejected.
func TestCLIBaseAuthority(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		want    string
		wantErr bool
	}{
		{name: "plain origin", base: "https://api.commandcode.ai", want: "https://api.commandcode.ai"},
		{name: "trailing slash", base: "https://api.commandcode.ai/", want: "https://api.commandcode.ai"},
		{name: "provider v1 suffix", base: "https://api.commandcode.ai/provider/v1", want: "https://api.commandcode.ai"},
		{name: "provider v1 suffix with slash", base: "https://api.commandcode.ai/provider/v1/", want: "https://api.commandcode.ai"},
		{name: "surrounding whitespace", base: "  https://api.commandcode.ai/provider/v1/  ", want: "https://api.commandcode.ai"},
		{name: "gateway subpath preserved", base: "https://gateway.example.com/base/provider/v1", want: "https://gateway.example.com/base"},
		{name: "explicit port preserved", base: "http://127.0.0.1:8317/provider/v1", want: "http://127.0.0.1:8317"},
		{name: "missing scheme", base: "api.commandcode.ai/provider/v1", wantErr: true},
		{name: "missing host", base: "https://", wantErr: true},
		{name: "empty", base: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cliBaseAuthority(tc.base)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("cliBaseAuthority(%q) = %q, want error", tc.base, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("cliBaseAuthority(%q): %v", tc.base, err)
			}
			if got != tc.want {
				t.Errorf("cliBaseAuthority(%q) = %q, want %q", tc.base, got, tc.want)
			}
		})
	}
}

// TestConvertNonStream checks the aggregated non-stream completion keeps
// reasoning, text, tool calls in order and carries finish_reason + usage.
func TestConvertNonStream(t *testing.T) {
	stream := cliFrame(`{"type":"reasoning-delta","text":"r1"}`) +
		cliFrame(`{"type":"text-delta","text":"Hello "}`) +
		cliFrame(`{"type":"text-delta","text":"world"}`) +
		cliFrame(`{"type":"tool-call","toolCallId":"call_9","toolName":"lookup","input":"{\"q\":\"x\"}"}`) +
		cliFrame(`{"type":"finish","finishReason":"tool_calls","totalUsage":{"inputTokens":5,"outputTokens":9}}`) +
		cliFrame("[DONE]")

	out, eErr := ConvertNonStream([]byte(stream), testModel)
	if eErr != nil {
		t.Fatalf("ConvertNonStream: %v", eErr)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("result is not JSON: %v (%q)", err, out)
	}
	if resp["object"] != "chat.completion" || resp["id"] != cliChunkID || resp["model"] != testModel {
		t.Fatalf("completion identity wrong: %v", resp)
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("want exactly one choice: %v", resp["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}
	message, _ := choice["message"].(map[string]any)
	if message["role"] != "assistant" || message["content"] != "Hello world" || message["reasoning_content"] != "r1" {
		t.Errorf("message wrong: %v", message)
	}
	calls, _ := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("want exactly one tool call: %v", message["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if call["id"] != "call_9" || call["type"] != "function" || fn["name"] != "lookup" || fn["arguments"] != `{"q":"x"}` {
		t.Errorf("tool call wrong: %v", call)
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(5) || usage["completion_tokens"] != float64(9) || usage["total_tokens"] != float64(14) {
		t.Errorf("usage wrong: %v", usage)
	}
}

// completionMessage decodes one aggregated non-stream completion and
// returns its envelope, its single choice and that choice's message.
func completionMessage(t *testing.T, body []byte) (map[string]any, map[string]any, map[string]any) {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("completion is not JSON: %v (%q)", err, body)
	}
	if resp["object"] != "chat.completion" || resp["id"] != cliChunkID || resp["model"] != testModel {
		t.Fatalf("completion identity wrong: %v", resp)
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("want exactly one choice: %v", resp["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		t.Fatalf("choice is not an object: %v", choices[0])
	}
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		t.Fatalf("choice carries no message: %v", choice)
	}
	return resp, choice, message
}

// TestConvertNonStreamCheckedTruncation pins the truncation contract of the
// non-stream aggregation primitive:
//
//   - finish event present, no [DONE]  -> normal end (finish is the terminal
//     signal);
//   - [DONE] present, no finish        -> truncated;
//   - neither finish nor [DONE]        -> truncated (dropped connection);
//   - no event at all                  -> truncated (empty response);
//   - malformed frame / unparseable tool-call arguments -> classified error,
//     never a silent drop and never a completion carrying broken arguments.
func TestConvertNonStreamCheckedTruncation(t *testing.T) {
	var (
		textFrame      = `{"type":"text-delta","text":"Hello "}`
		moreTextFrame  = `{"type":"text-delta","text":"world"}`
		reasonFrame    = `{"type":"reasoning-delta","text":"thinking"}`
		finishFrame    = `{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":5,"outputTokens":7}}`
		doneFrame      = `[DONE]`
		partialText    = `{"type":"text-delta","text":"partial"}`
		openToolFrame  = `{"type":"tool-call","toolCallId":"call_1","toolName":"lookup","input":{"query":"go"}}`
		nullToolFrame  = `{"type":"tool-call","toolCallId":"call_2","toolName":"lookup","input":null}`
		badArgsFrame   = `{"type":"tool-call","toolCallId":"call_3","toolName":"lookup","input":"{\"a\":1"}`
		unknownFrame   = `{"type":"cache-write-tokens","tokens":3}`
		errorFrame     = `{"type":"error","error":"rate_limit_exceeded","message":"slow down"}`
		halfJSONFrame  = `data: {"type":"text-del`
		textAndFinish  = cliFrame(textFrame) + cliFrame(moreTextFrame) + cliFrame(finishFrame)
		noFinishReason = `{"type":"finish","finishReason":""}`
	)

	tests := []struct {
		name           string
		stream         string
		wantErrClass   errclass.Class
		wantTruncated  bool
		wantContent    string
		wantReasoning  string
		wantFinish     string
		wantToolCalls  int
		wantArgs       string
		wantPrompt     float64
		wantCompletion float64
	}{
		{
			name:           "text plus finish plus done",
			stream:         textAndFinish + cliFrame(doneFrame),
			wantTruncated:  false,
			wantContent:    "Hello world",
			wantFinish:     "stop",
			wantPrompt:     5,
			wantCompletion: 7,
		},
		{
			name:           "text plus finish without done",
			stream:         textAndFinish,
			wantTruncated:  false,
			wantContent:    "Hello world",
			wantFinish:     "stop",
			wantPrompt:     5,
			wantCompletion: 7,
		},
		{
			name:          "text without finish",
			stream:        cliFrame(partialText),
			wantTruncated: true,
			wantContent:   "partial",
			wantFinish:    "stop",
		},
		{
			name:          "done alone",
			stream:        cliFrame(doneFrame),
			wantTruncated: true,
			wantFinish:    "stop",
		},
		{
			name:          "empty stream",
			stream:        "",
			wantTruncated: true,
			wantFinish:    "stop",
		},
		{
			name:          "reasoning delta without finish",
			stream:        cliFrame(reasonFrame),
			wantTruncated: true,
			wantReasoning: "thinking",
			wantFinish:    "stop",
		},
		{
			name:           "reasoning delta with finish",
			stream:         cliFrame(reasonFrame) + cliFrame(finishFrame),
			wantTruncated:  false,
			wantReasoning:  "thinking",
			wantFinish:     "stop",
			wantPrompt:     5,
			wantCompletion: 7,
		},
		{
			name:          "finish with empty reason is a finish",
			stream:        cliFrame(textFrame) + cliFrame(noFinishReason),
			wantTruncated: false,
			wantContent:   "Hello ",
			wantFinish:    "stop",
		},
		{
			name:          "unknown event only",
			stream:        cliFrame(unknownFrame),
			wantTruncated: true,
			wantFinish:    "stop",
		},
		{
			name:          "tool call without finish",
			stream:        cliFrame(openToolFrame),
			wantTruncated: true,
			wantFinish:    "tool_calls",
			wantToolCalls: 1,
			wantArgs:      `{"query":"go"}`,
		},
		{
			name:           "tool call null input defaults to empty object",
			stream:         cliFrame(nullToolFrame) + cliFrame(finishFrame),
			wantTruncated:  false,
			wantFinish:     "stop",
			wantToolCalls:  1,
			wantArgs:       "{}",
			wantPrompt:     5,
			wantCompletion: 7,
		},
		{
			name:         "half json frame fails classified",
			stream:       cliFrame(textFrame) + halfJSONFrame,
			wantErrClass: errclass.ClassTranslation,
		},
		{
			name:         "unclosed tool call arguments fail classified",
			stream:       cliFrame(textFrame) + cliFrame(badArgsFrame) + cliFrame(finishFrame),
			wantErrClass: errclass.ClassTranslation,
		},
		{
			name:         "error event fails classified",
			stream:       cliFrame(partialText) + cliFrame(errorFrame),
			wantErrClass: errclass.ClassRateLimit,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, truncated, eErr := ConvertNonStreamChecked([]byte(tc.stream), testModel)
			if tc.wantErrClass != "" {
				if eErr == nil {
					t.Fatalf("want a classified error, got body %q truncated=%v", body, truncated)
				}
				if eErr.Class != tc.wantErrClass {
					t.Errorf("error class = %q, want %q (%+v)", eErr.Class, tc.wantErrClass, eErr)
				}
				if len(body) != 0 {
					t.Errorf("a failed aggregation must not carry a body: %q", body)
				}
				if strings.TrimSpace(eErr.Message) == "" {
					t.Error("classified error must carry a detail message")
				}
				return
			}
			if eErr != nil {
				t.Fatalf("ConvertNonStreamChecked: %v", eErr)
			}
			if truncated != tc.wantTruncated {
				t.Errorf("truncated = %v, want %v", truncated, tc.wantTruncated)
			}
			resp, choice, message := completionMessage(t, body)
			if choice["finish_reason"] != tc.wantFinish {
				t.Errorf("finish_reason = %v, want %q", choice["finish_reason"], tc.wantFinish)
			}
			if message["content"] != tc.wantContent {
				t.Errorf("content = %v, want %q", message["content"], tc.wantContent)
			}
			gotReasoning, hasReasoning := message["reasoning_content"]
			if tc.wantReasoning == "" {
				if hasReasoning {
					t.Errorf("unexpected reasoning_content = %v", gotReasoning)
				}
			} else if gotReasoning != tc.wantReasoning {
				t.Errorf("reasoning_content = %v, want %q", gotReasoning, tc.wantReasoning)
			}
			calls, _ := message["tool_calls"].([]any)
			if len(calls) != tc.wantToolCalls {
				t.Fatalf("tool_calls = %d, want %d (%v)", len(calls), tc.wantToolCalls, message["tool_calls"])
			}
			if tc.wantArgs != "" {
				call, _ := calls[0].(map[string]any)
				fn, _ := call["function"].(map[string]any)
				if fn["arguments"] != tc.wantArgs {
					t.Errorf("tool call arguments = %v, want %q", fn["arguments"], tc.wantArgs)
				}
			}
			usage, _ := resp["usage"].(map[string]any)
			if usage == nil {
				t.Fatalf("completion carries no usage: %v", resp)
			}
			if usage["prompt_tokens"] != tc.wantPrompt || usage["completion_tokens"] != tc.wantCompletion ||
				usage["total_tokens"] != tc.wantPrompt+tc.wantCompletion {
				t.Errorf("usage = %v, want prompt=%v completion=%v", usage, tc.wantPrompt, tc.wantCompletion)
			}
		})
	}
}

// TestConvertNonStreamLegacyWrapper pins that ConvertNonStream keeps its
// original signature and behaviour: it returns exactly the checked body,
// ignores the truncation flag, and still synthesizes a terminal
// finish_reason so a truncated stream stays a valid completion for callers
// that cannot act on the flag.
func TestConvertNonStreamLegacyWrapper(t *testing.T) {
	tests := []struct {
		name       string
		stream     string
		wantFinish string
	}{
		{
			name:       "complete stream",
			stream:     cliFrame(`{"type":"text-delta","text":"hi"}`) + cliFrame(`{"type":"finish","finishReason":"stop"}`) + cliFrame("[DONE]"),
			wantFinish: "stop",
		},
		{
			name:       "truncated stream keeps a synthesized reason",
			stream:     cliFrame(`{"type":"text-delta","text":"hi"}`),
			wantFinish: "stop",
		},
		{
			name:       "truncated tool call stream keeps tool_calls",
			stream:     cliFrame(`{"type":"tool-call","toolCallId":"call_1","toolName":"lookup","input":null}`),
			wantFinish: "tool_calls",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			legacy, eErr := ConvertNonStream([]byte(tc.stream), testModel)
			if eErr != nil {
				t.Fatalf("ConvertNonStream: %v", eErr)
			}
			checked, _, eErr2 := ConvertNonStreamChecked([]byte(tc.stream), testModel)
			if eErr2 != nil {
				t.Fatalf("ConvertNonStreamChecked: %v", eErr2)
			}
			var legacyResp, checkedResp map[string]any
			if err := json.Unmarshal(legacy, &legacyResp); err != nil {
				t.Fatalf("legacy result is not JSON: %v (%q)", err, legacy)
			}
			if err := json.Unmarshal(checked, &checkedResp); err != nil {
				t.Fatalf("checked result is not JSON: %v (%q)", err, checked)
			}
			// created is a wall-clock stamp; every other field must match.
			delete(legacyResp, "created")
			delete(checkedResp, "created")
			if !reflect.DeepEqual(legacyResp, checkedResp) {
				t.Errorf("ConvertNonStream diverged from ConvertNonStreamChecked:\n legacy=%v\nchecked=%v", legacyResp, checkedResp)
			}
			_, choice, _ := completionMessage(t, legacy)
			if choice["finish_reason"] != tc.wantFinish {
				t.Errorf("finish_reason = %v, want %q", choice["finish_reason"], tc.wantFinish)
			}
		})
	}
}

// TestConvertNonStreamErrorEvent checks the aggregation path fails
// classified on an upstream error event instead of emitting a completion.
func TestConvertNonStreamErrorEvent(t *testing.T) {
	stream := cliFrame(`{"type":"text-delta","text":"partial"}`) +
		cliFrame(`{"type":"error","error":"rate_limit_exceeded","message":"slow down"}`)
	out, eErr := ConvertNonStream([]byte(stream), testModel)
	if eErr == nil {
		t.Fatalf("error event must fail the aggregation, got %q", out)
	}
	if eErr.Class != errclass.ClassRateLimit || eErr.StatusCode != 429 {
		t.Fatalf("classified error = %+v, want rate_limit status 429", eErr)
	}
}

// TestSynthesizedResponsesEchoTheRequestedModel is the regression test for the
// "response model is always commandcode-gocli" bug.
//
// The CLI transport's stream carries no model name, so the plugin used a fixed
// placeholder. Every client then saw a response model that matched neither what
// it requested nor what was sent upstream, and gateways that diff those three
// values (new-api does) raised a "model may have been substituted" warning on
// every single request. The synthesized envelope must carry the REQUESTED name.
func TestSynthesizedResponsesEchoTheRequestedModel(t *testing.T) {
	const requested = "commandcode/deepseek/deepseek-v4.1-flash"
	stream := cliFrame(`{"type":"text-delta","text":"hi"}`) +
		cliFrame(`{"type":"finish","finishReason":"stop"}`) +
		"data: [DONE]\n\n"

	t.Run("non-stream", func(t *testing.T) {
		out, eErr := ConvertNonStream([]byte(stream), requested)
		if eErr != nil {
			t.Fatalf("ConvertNonStream: %v", eErr)
		}
		var resp map[string]any
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatal(err)
		}
		if resp["model"] != requested {
			t.Fatalf("non-stream model = %v, want the requested %q", resp["model"], requested)
		}
		if resp["model"] == cliFallbackModel {
			t.Fatalf("non-stream model is still the placeholder %q", cliFallbackModel)
		}
	})

	t.Run("stream", func(t *testing.T) {
		c := NewConverter("openai", requested)
		events, _, eErr := c.Feed([]byte(stream))
		if eErr != nil {
			t.Fatalf("Feed: %v", eErr)
		}
		if len(events) == 0 {
			t.Fatal("no events emitted")
		}
		seen := 0
		for _, ev := range events {
			payload := strings.TrimPrefix(strings.TrimSpace(string(ev)), "data: ")
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(payload), &chunk) != nil {
				continue
			}
			if _, ok := chunk["model"]; !ok {
				continue
			}
			seen++
			if chunk["model"] != requested {
				t.Fatalf("chunk model = %v, want the requested %q", chunk["model"], requested)
			}
		}
		if seen == 0 {
			t.Fatal("no chunk carried a model field")
		}
	})

	// The placeholder survives only as a genuine fallback: a caller with no
	// model name at all must still get a well-formed, non-empty model.
	t.Run("fallback when the caller supplies no model", func(t *testing.T) {
		out, eErr := ConvertNonStream([]byte(stream), "")
		if eErr != nil {
			t.Fatalf("ConvertNonStream: %v", eErr)
		}
		var resp map[string]any
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatal(err)
		}
		if resp["model"] != cliFallbackModel {
			t.Fatalf("fallback model = %v, want %q", resp["model"], cliFallbackModel)
		}
	})
}
