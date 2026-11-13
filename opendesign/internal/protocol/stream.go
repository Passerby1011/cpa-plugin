// Package protocol — streaming.
//
// The three schemas stream in three different frame vocabularies:
//
//	chat completions  one JSON object per data: frame, delta.content,
//	                  delta.reasoning_content, delta.tool_calls, [DONE]
//	Claude messages   named events (message_start, content_block_delta, …)
//	Responses         named events (response.output_text.delta, …)
//
// A stream crosses schemas whenever the client's protocol differs from the
// model's declared API — which is the common case here, because a model is
// declared on the wire it THINKS on rather than on the wire the client speaks.
// So each schema has a decoder (frames -> canonical deltas) and an encoder
// (canonical deltas -> frames), and thinking text survives the crossing because
// it is a first-class delta kind rather than a schema-local field.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// StreamDecoder turns one upstream frame at a time into canonical deltas. A
// frame that carries nothing (a comment, an empty data: line, an unmodelled
// event) returns (nil, nil).
type StreamDecoder interface {
	Feed(frame []byte) ([]Delta, error)
}

// StreamEncoder turns canonical deltas into frames for one client schema. It
// owns the per-stream state a schema needs (which content block index is open,
// whether the message started), so one encoder instance serves one response.
type StreamEncoder interface {
	// Start emits the opening frames for a response.
	Start(id, model string, createdAt int64) [][]byte
	// Emit renders one delta.
	Emit(d Delta) [][]byte
	// Finish closes the response, with a normalized stop reason.
	Finish(stopReason string, usage *Usage) [][]byte
}

// NewStreamDecoder returns the decoder for one schema.
func NewStreamDecoder(schema Schema) StreamDecoder {
	switch schema {
	case SchemaClaude:
		return &claudeStreamDecoder{}
	case SchemaResponses:
		return &responsesStreamDecoder{}
	default:
		return &chatStreamDecoder{}
	}
}

// NewStreamEncoder returns the encoder for one schema.
func NewStreamEncoder(schema Schema) StreamEncoder {
	switch schema {
	case SchemaClaude:
		return &claudeStreamEncoder{blocks: map[int]int{}, kinds: map[int]string{}}
	case SchemaResponses:
		return &responsesStreamEncoder{items: map[string]int{}, started: map[string]bool{}}
	default:
		return &chatStreamEncoder{}
	}
}

// IsDoneFrame reports whether a raw frame terminates a chat-completions stream.
func IsDoneFrame(frame []byte) bool {
	return strings.TrimSpace(string(frame)) == "[DONE]"
}

// FramePayload strips an SSE frame's `data:` prefix and surrounding whitespace.
// A frame may also arrive as a bare JSON object (a decoders' tolerance for
// gateways that do not frame), and then it is returned unchanged.
func FramePayload(frame []byte) string {
	if payload, ok := isDataLine(frame); ok {
		return payload
	}
	trimmed := strings.TrimSpace(string(frame))
	// A named event arrives as "event: x\ndata: {...}"; the payload is the
	// data line inside it.
	for _, line := range strings.Split(trimmed, "\n") {
		if payload, ok := isDataLine([]byte(line)); ok {
			return payload
		}
	}
	return trimmed
}

// ---------------------------------------------------------------------------
// chat completions: frames -> deltas
// ---------------------------------------------------------------------------

type chatStreamDecoder struct{}

// chatThinkingIndex and chatToolBase are canonical part indexes the chat decoder
// assigns, so a schema's content-block numbering (which restarts per block) does
// not collide with a chat delta's own numbering. A thought and the answer are
// always different blocks downstream — an Anthropic client refuses two blocks of
// different types on one index.
const (
	chatThinkingIndex = 0
	chatTextIndex     = 1
	chatToolBase      = 100
)

func (d *chatStreamDecoder) Feed(frame []byte) ([]Delta, error) {
	payload := FramePayload(frame)
	if payload == "" || payload == "[DONE]" {
		return nil, nil
	}
	obj, err := decodeObject([]byte(payload))
	if err != nil {
		// A frame this decoder cannot read is noise (a gateway heartbeat); a
		// hard failure here would abort an otherwise healthy stream.
		return nil, nil
	}
	var deltas []Delta
	if id := stringField(obj, "id"); id != "" {
		deltas = append(deltas, Delta{Kind: DeltaStart, ID: id, Model: stringField(obj, "model"), CreatedAt: intField(obj, "created")})
	}
	var choices []map[string]json.RawMessage
	if raw, ok := obj["choices"]; ok {
		_ = json.Unmarshal(raw, &choices)
	}
	for _, choice := range choices {
		// The chat choice index is a candidate slot, not a content block; the
		// decoder assigns its own canonical indexes below.
		_ = choice
		if raw, ok := choice["delta"]; ok {
			var d map[string]json.RawMessage
			if json.Unmarshal(raw, &d) == nil {
				if think := firstStringField(d, "reasoning_content", "reasoning"); think != "" {
					deltas = append(deltas, Delta{Kind: DeltaThinking, Text: think, Index: chatThinkingIndex})
				}
				if text := stringField(d, "content"); text != "" {
					deltas = append(deltas, Delta{Kind: DeltaText, Text: text, Index: chatTextIndex})
				}
				if rawCalls, ok := d["tool_calls"]; ok {
					var calls []map[string]json.RawMessage
					if json.Unmarshal(rawCalls, &calls) == nil {
						for _, c := range calls {
							callIndex := int(firstIntField(c, "index"))
							started := false
							name := ""
							if fn, ok := c["function"]; ok {
								var f map[string]json.RawMessage
								if json.Unmarshal(fn, &f) == nil {
									if n := stringField(f, "name"); n != "" {
										name, started = n, true
									}
								}
							}
							deltas = append(deltas, Delta{
								Kind:       DeltaToolCall,
								Index:      chatToolBase + callIndex,
								Started:    started,
								ToolCallID: stringField(c, "id"),
								ToolName:   name,
								ToolArgs:   chatToolArgs(c),
							})
						}
					}
				}
			}
		}
		if reason := stringField(choice, "finish_reason"); reason != "" {
			deltas = append(deltas, Delta{Kind: DeltaStop, StopReason: normalizeStop(reason)})
		}
	}
	if raw, ok := obj["usage"]; ok && len(raw) > 0 && string(raw) != "null" {
		var u map[string]json.RawMessage
		if json.Unmarshal(raw, &u) == nil {
			usage := decodeChatUsage(u)
			deltas = append(deltas, Delta{Kind: DeltaUsage, Usage: &usage})
		}
	}
	return deltas, nil
}

func chatToolArgs(call map[string]json.RawMessage) string {
	if fn, ok := call["function"]; ok {
		var f map[string]json.RawMessage
		if json.Unmarshal(fn, &f) == nil {
			return stringField(f, "arguments")
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Claude messages: frames -> deltas
// ---------------------------------------------------------------------------

type claudeStreamDecoder struct {
	started bool
}

func (d *claudeStreamDecoder) Feed(frame []byte) ([]Delta, error) {
	payload := FramePayload(frame)
	if payload == "" {
		return nil, nil
	}
	obj, err := decodeObject([]byte(payload))
	if err != nil {
		return nil, nil
	}
	index := int(intField(obj, "index"))
	switch strings.ToLower(stringField(obj, "type")) {
	case "message_start":
		if msg, ok := obj["message"]; ok {
			var m map[string]json.RawMessage
			if json.Unmarshal(msg, &m) == nil {
				deltas := []Delta{{Kind: DeltaStart, ID: stringField(m, "id"), Model: stringField(m, "model")}}
				if u, ok := m["usage"]; ok {
					var um map[string]json.RawMessage
					if json.Unmarshal(u, &um) == nil {
						usage := decodeClaudeUsage(um)
						deltas = append(deltas, Delta{Kind: DeltaUsage, Usage: &usage})
					}
				}
				return deltas, nil
			}
		}
		return nil, nil
	case "content_block_start":
		if block, ok := obj["content_block"]; ok {
			var b map[string]json.RawMessage
			if json.Unmarshal(block, &b) == nil {
				switch strings.ToLower(stringField(b, "type")) {
				case "text":
					if text := stringField(b, "text"); text != "" {
						return []Delta{{Kind: DeltaText, Text: text, Index: index}}, nil
					}
				case "thinking":
					// A block that opens with text (some gateways send the
					// whole thought here) is emitted as one thinking delta.
					if think := firstStringField(b, "thinking", "text"); think != "" {
						return []Delta{{Kind: DeltaThinking, Text: think, Index: index}}, nil
					}
				case "tool_use":
					return []Delta{{
						Kind:       DeltaToolCall,
						Index:      index,
						Started:    true,
						ToolCallID: stringField(b, "id"),
						ToolName:   stringField(b, "name"),
					}}, nil
				}
			}
		}
		return nil, nil
	case "content_block_delta":
		if delta, ok := obj["delta"]; ok {
			var d map[string]json.RawMessage
			if json.Unmarshal(delta, &d) == nil {
				switch strings.ToLower(stringField(d, "type")) {
				case "text_delta":
					return []Delta{{Kind: DeltaText, Text: stringField(d, "text"), Index: index}}, nil
				case "thinking_delta":
					return []Delta{{Kind: DeltaThinking, Text: firstStringField(d, "thinking", "text"), Index: index}}, nil
				case "signature_delta":
					return []Delta{{Kind: DeltaThinkingSignature, Signature: stringField(d, "signature"), Index: index}}, nil
				case "input_json_delta":
					return []Delta{{Kind: DeltaToolCall, Index: index, ToolArgs: stringField(d, "partial_json")}}, nil
				}
			}
		}
		return nil, nil
	case "message_delta":
		var deltas []Delta
		if delta, ok := obj["delta"]; ok {
			var dm map[string]json.RawMessage
			if json.Unmarshal(delta, &dm) == nil {
				if reason := stringField(dm, "stop_reason"); reason != "" {
					deltas = append(deltas, Delta{Kind: DeltaStop, StopReason: normalizeStop(reason)})
				}
			}
		}
		if u, ok := obj["usage"]; ok {
			var um map[string]json.RawMessage
			if json.Unmarshal(u, &um) == nil {
				usage := decodeClaudeUsage(um)
				deltas = append(deltas, Delta{Kind: DeltaUsage, Usage: &usage})
			}
		}
		return deltas, nil
	case "message_stop", "content_block_stop":
		return nil, nil
	case "error":
		msg := ""
		if e, ok := obj["error"]; ok {
			var em map[string]json.RawMessage
			if json.Unmarshal(e, &em) == nil {
				msg = stringField(em, "message")
			}
		}
		if msg == "" {
			msg = "upstream reported an error"
		}
		return nil, fmt.Errorf("upstream answered an error: %s", truncate(msg, 300))
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// Responses: frames -> deltas
// ---------------------------------------------------------------------------

type responsesStreamDecoder struct {
	seen map[string]bool
}

func (d *responsesStreamDecoder) Feed(frame []byte) ([]Delta, error) {
	payload := FramePayload(frame)
	if payload == "" || payload == "[DONE]" {
		return nil, nil
	}
	obj, err := decodeObject([]byte(payload))
	if err != nil {
		return nil, nil
	}
	kind := strings.ToLower(stringField(obj, "type"))
	switch kind {
	case "response.created", "response.in_progress":
		if r, ok := obj["response"]; ok {
			var rm map[string]json.RawMessage
			if json.Unmarshal(r, &rm) == nil {
				return []Delta{{Kind: DeltaStart, ID: stringField(rm, "id"), Model: stringField(rm, "model"), CreatedAt: intField(rm, "created_at")}}, nil
			}
		}
		return nil, nil
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return []Delta{{Kind: DeltaThinking, Text: stringField(obj, "delta"), Index: int(firstIntField(obj, "output_index"))}}, nil
	case "response.output_text.delta":
		return []Delta{{Kind: DeltaText, Text: stringField(obj, "delta"), Index: int(firstIntField(obj, "output_index"))}}, nil
	case "response.output_item.added":
		if item, ok := obj["item"]; ok {
			var im map[string]json.RawMessage
			if json.Unmarshal(item, &im) == nil && strings.ToLower(stringField(im, "type")) == "function_call" {
				return []Delta{{
					Kind:       DeltaToolCall,
					Index:      int(firstIntField(obj, "output_index")),
					Started:    true,
					ToolCallID: firstStringField(im, "call_id", "id"),
					ToolName:   stringField(im, "name"),
				}}, nil
			}
		}
		return nil, nil
	case "response.function_call_arguments.delta":
		return []Delta{{
			Kind:     DeltaToolCall,
			Index:    int(firstIntField(obj, "output_index")),
			ToolArgs: stringField(obj, "delta"),
		}}, nil
	case "response.completed", "response.incomplete":
		var deltas []Delta
		if r, ok := obj["response"]; ok {
			var rm map[string]json.RawMessage
			if json.Unmarshal(r, &rm) == nil {
				if u, ok := rm["usage"]; ok {
					var um map[string]json.RawMessage
					if json.Unmarshal(u, &um) == nil {
						usage := decodeResponsesUsage(um)
						deltas = append(deltas, Delta{Kind: DeltaUsage, Usage: &usage})
					}
				}
				if strings.EqualFold(stringField(rm, "status"), "incomplete") {
					if details, ok := rm["incomplete_details"]; ok {
						var dm map[string]json.RawMessage
						if json.Unmarshal(details, &dm) == nil {
							if reason := stringField(dm, "reason"); reason != "" {
								deltas = append(deltas, Delta{Kind: DeltaStop, StopReason: normalizeStop(reason)})
							}
						}
					}
				}
			}
		}
		if len(deltas) == 0 {
			deltas = append(deltas, Delta{Kind: DeltaStop, StopReason: StopEnd})
		}
		return deltas, nil
	case "error", "response.failed":
		msg := firstStringField(obj, "message")
		if msg == "" {
			if e, ok := obj["error"]; ok {
				var em map[string]json.RawMessage
				if json.Unmarshal(e, &em) == nil {
					msg = stringField(em, "message")
				}
			}
		}
		if r, ok := obj["response"]; ok && msg == "" {
			var rm map[string]json.RawMessage
			if json.Unmarshal(r, &rm) == nil {
				if e, ok := rm["error"]; ok {
					var em map[string]json.RawMessage
					if json.Unmarshal(e, &em) == nil {
						msg = stringField(em, "message")
					}
				}
			}
		}
		if msg == "" {
			msg = "upstream reported an error"
		}
		return nil, fmt.Errorf("upstream answered an error: %s", truncate(msg, 300))
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// chat completions: deltas -> frames
// ---------------------------------------------------------------------------

type chatStreamEncoder struct {
	id      string
	model   string
	created int64
	started bool
	// toolIndexes maps a canonical tool-call index onto the chat index the
	// client sees, because chat numbers tool calls 0..n per message.
	toolIndexes map[int]int
	nextTool    int
}

func (e *chatStreamEncoder) Start(id, model string, createdAt int64) [][]byte {
	e.id = nonEmpty(id, "chatcmpl-opendesign")
	e.model = model
	e.created = createdAt
	if e.created <= 0 {
		e.created = 1
	}
	e.toolIndexes = map[int]int{}
	e.started = true
	// chat has no opening frame of its own: the first delta carries the role.
	return e.emit(Delta{Kind: DeltaText, Text: "", Index: -1}, true)
}

func (e *chatStreamEncoder) Emit(d Delta) [][]byte {
	switch d.Kind {
	case DeltaStart:
		if d.ID != "" {
			e.id = d.ID
		}
		if d.Model != "" {
			e.model = d.Model
		}
		if d.CreatedAt > 0 {
			e.created = d.CreatedAt
		}
		if !e.started {
			e.started = true
			return e.roleFrame()
		}
		return nil
	case DeltaText:
		if d.Text == "" {
			return nil
		}
		return e.deltaFrame(map[string]any{"content": d.Text}, nil)
	case DeltaThinking:
		if d.Text == "" {
			return nil
		}
		return e.deltaFrame(map[string]any{"reasoning_content": d.Text}, nil)
	case DeltaThinkingSignature:
		// chat completions has nowhere to put a signature; dropping it is the
		// only option and the client does not read one back.
		return nil
	case DeltaToolCall:
		if !e.started {
			e.started = true
		}
		if e.toolIndexes == nil {
			e.toolIndexes = map[int]int{}
		}
		idx, ok := e.toolIndexes[d.Index]
		if !ok || d.Started {
			idx = e.nextTool
			e.nextTool++
			e.toolIndexes[d.Index] = idx
		}
		call := map[string]any{"index": idx}
		if d.ToolCallID != "" || d.Started {
			call["id"] = nonEmpty(d.ToolCallID, fmt.Sprintf("call_opendesign_%d", idx))
			call["type"] = "function"
			call["function"] = map[string]any{"name": d.ToolName, "arguments": ""}
			return e.deltaFrame(map[string]any{"tool_calls": []any{call}}, nil)
		}
		if d.ToolArgs != "" {
			return e.deltaFrame(map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    idx,
					"function": map[string]any{"arguments": d.ToolArgs},
				}},
			}, nil)
		}
		return nil
	}
	// DeltaUsage and DeltaStop close the stream through Finish.
	return nil
}

func (e *chatStreamEncoder) Finish(stopReason string, usage *Usage) [][]byte {
	var frames [][]byte
	finish := "stop"
	switch stopReason {
	case StopLength:
		finish = "length"
	case StopToolUse:
		finish = "tool_calls"
	case StopFiltered:
		finish = "content_filter"
	}
	obj := map[string]any{
		"id":      e.id,
		"object":  "chat.completion.chunk",
		"created": e.created,
		"model":   e.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		prompt, completion, total := usage.Resolved()
		detail := map[string]any{
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
			"total_tokens":      total,
		}
		if usage.Reasoning > 0 {
			detail["completion_tokens_details"] = map[string]any{"reasoning_tokens": usage.Reasoning}
		}
		obj["usage"] = detail
	}
	raw, _ := json.Marshal(obj)
	frames = append(frames, raw, []byte("[DONE]"))
	return frames
}

func (e *chatStreamEncoder) roleFrame() [][]byte {
	return e.deltaFrame(map[string]any{"role": "assistant", "content": ""}, nil)
}

func (e *chatStreamEncoder) deltaFrame(delta map[string]any, finish any) [][]byte {
	obj := map[string]any{
		"id":      nonEmpty(e.id, "chatcmpl-opendesign"),
		"object":  "chat.completion.chunk",
		"created": e.created,
		"model":   e.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	raw, _ := json.Marshal(obj)
	return [][]byte{raw}
}

// emit is Start's helper: it produces the opening chunk, dodging the "no
// content" case by emitting the role frame instead.
func (e *chatStreamEncoder) emit(d Delta, opening bool) [][]byte {
	if opening {
		return e.roleFrame()
	}
	return e.Emit(d)
}

// ---------------------------------------------------------------------------
// Claude messages: deltas -> frames
// ---------------------------------------------------------------------------

type claudeStreamEncoder struct {
	id      string
	model   string
	open    bool
	blocks  map[int]int    // canonical index -> content block index
	kinds   map[int]string // content block index -> current block type
	nextIdx int
	// started tracks whether a block is open, so a block is closed before
	// another one opens (Anthropic requires one open block at a time).
	openBlock int
	hasBlock  bool
	stopSent  bool
}

func (e *claudeStreamEncoder) Start(id, model string, createdAt int64) [][]byte {
	e.id = nonEmpty(id, "msg_opendesign")
	e.model = model
	if e.blocks == nil {
		e.blocks = map[int]int{}
	}
	if e.kinds == nil {
		e.kinds = map[int]string{}
	}
	e.open = true
	body := map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            e.id,
			"type":          "message",
			"role":          "assistant",
			"model":         e.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}
	return [][]byte{claudeEvent("message_start", body)}
}

func (e *claudeStreamEncoder) Emit(d Delta) [][]byte {
	if !e.open {
		// A decoder that never sent a start event still gets a valid stream.
		return append(e.Start(e.id, e.model, 0), e.Emit(d)...)
	}
	switch d.Kind {
	case DeltaStart:
		if d.ID != "" {
			e.id = d.ID
		}
		if d.Model != "" {
			e.model = d.Model
		}
		return nil
	case DeltaText:
		if d.Text == "" {
			return nil
		}
		var frames [][]byte
		frames = append(frames, e.closeBlock()...)
		blockIndex := e.blockFor(d.Index, "text")
		if _, open := e.kinds[blockIndex]; !open {
			// Opening a text block is the block_start frame; closeBlock above
			// only closes what was open, so a fresh block is opened here.
		}
		frames = append(frames, e.openBlockOfType(blockIndex, "text")...)
		frames = append(frames, claudeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": d.Text},
		}))
		return frames
	case DeltaThinking:
		if d.Text == "" {
			return nil
		}
		var frames [][]byte
		blockIndex, isOpen := e.blockForExisting(d.Index, "thinking")
		if !isOpen {
			frames = append(frames, e.closeBlock()...)
			blockIndex = e.blockFor(d.Index, "thinking")
			frames = append(frames, e.openBlockOfType(blockIndex, "thinking")...)
		}
		frames = append(frames, claudeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": d.Text},
		}))
		return frames
	case DeltaThinkingSignature:
		if d.Signature == "" {
			return nil
		}
		blockIndex := e.currentBlock()
		return [][]byte{claudeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]any{"type": "signature_delta", "signature": d.Signature},
		})}
	case DeltaToolCall:
		var frames [][]byte
		blockIndex, isOpen := e.blockForTool(d.Index)
		if !isOpen {
			frames = append(frames, e.closeBlock()...)
			blockIndex = e.nextIndex()
			e.blocks[d.Index] = blockIndex
			e.kinds[blockIndex] = "tool_use"
			e.openBlock, e.hasBlock = blockIndex, true
			frames = append(frames, claudeEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": blockIndex,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    nonEmpty(d.ToolCallID, fmt.Sprintf("toolu_%d", blockIndex)),
					"name":  d.ToolName,
					"input": map[string]any{},
				},
			}))
		}
		if d.ToolArgs != "" {
			frames = append(frames, claudeEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": d.ToolArgs},
			}))
		}
		return frames
	}
	return nil
}

func (e *claudeStreamEncoder) Finish(stopReason string, usage *Usage) [][]byte {
	if e.stopSent {
		return nil
	}
	e.stopSent = true
	var frames [][]byte
	frames = append(frames, e.closeBlock()...)
	stop := claudeStopReason(stopReason)
	body := map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stop,
			"stop_sequence": nil,
		},
	}
	if usage != nil {
		body["usage"] = claudeUsageBody(*usage)
	} else {
		body["usage"] = map[string]any{"output_tokens": 0}
	}
	frames = append(frames, claudeEvent("message_delta", body))
	frames = append(frames, claudeEvent("message_stop", map[string]any{"type": "message_stop"}))
	return frames
}

func (e *claudeStreamEncoder) nextIndex() int {
	idx := e.nextIdx
	e.nextIdx++
	return idx
}

// blockFor returns the content block index for a canonical index, ensuring the
// block is marked open.
func (e *claudeStreamEncoder) blockFor(index int, kind string) int {
	if e.blocks == nil {
		e.blocks = map[int]int{}
	}
	if e.kinds == nil {
		e.kinds = map[int]string{}
	}
	blockIndex, ok := e.blocks[index]
	if !ok {
		blockIndex = e.nextIndex()
		e.blocks[index] = blockIndex
	}
	e.kinds[blockIndex] = kind
	e.openBlock, e.hasBlock = blockIndex, true
	return blockIndex
}

// blockForExisting returns an already-open block of the given kind, if any.
func (e *claudeStreamEncoder) blockForExisting(index int, kind string) (int, bool) {
	if blockIndex, ok := e.blocks[index]; ok && e.kinds[blockIndex] == kind {
		return blockIndex, true
	}
	return 0, false
}

// blockForTool returns an open tool block for a canonical index.
func (e *claudeStreamEncoder) blockForTool(index int) (int, bool) {
	if blockIndex, ok := e.blocks[index]; ok && e.kinds[blockIndex] == "tool_use" {
		e.openBlock = blockIndex
		return blockIndex, true
	}
	return 0, false
}

func (e *claudeStreamEncoder) currentBlock() int {
	if e.hasBlock {
		return e.openBlock
	}
	return 0
}

func (e *claudeStreamEncoder) openBlockOfType(blockIndex int, kind string) [][]byte {
	block := map[string]any{"type": kind}
	switch kind {
	case "text":
		block["text"] = ""
	case "thinking":
		block["thinking"] = ""
	}
	return [][]byte{claudeEvent("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         blockIndex,
		"content_block": block,
	})}
}

// closeBlock closes the block that is open, if any.
func (e *claudeStreamEncoder) closeBlock() [][]byte {
	if !e.hasBlock {
		return nil
	}
	index := e.openBlock
	delete(e.kinds, index)
	e.hasBlock = false
	return [][]byte{claudeEvent("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": index,
	})}
}

func claudeEvent(name string, body any) []byte {
	raw, _ := json.Marshal(body)
	return []byte("event: " + name + "\ndata: " + string(raw) + "\n\n")
}

// ---------------------------------------------------------------------------
// Responses: deltas -> frames
// ---------------------------------------------------------------------------

type responsesStreamEncoder struct {
	id    string
	model string
	// items tracks which output items are open, keyed by canonical index.
	items   map[string]int
	started map[string]bool
	seq     int
	open    bool
	// openKind remembers the open item's type so it is closed before another
	// opens.
	openKind   string
	openItemID string
	openIndex  int
	// reasoningBuf / textBuf accumulate the text a "done" event repeats.
	reasoningBuf strings.Builder
	textBuf      strings.Builder
	finished     bool
}

func (e *responsesStreamEncoder) Start(id, model string, createdAt int64) [][]byte {
	e.id = nonEmpty(id, "resp_opendesign")
	e.model = model
	if e.items == nil {
		e.items = map[string]int{}
	}
	if e.started == nil {
		e.started = map[string]bool{}
	}
	e.open = true
	if createdAt <= 0 {
		createdAt = 1
	}
	body := map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":         e.id,
			"object":     "response",
			"created_at": createdAt,
			"status":     "in_progress",
			"model":      e.model,
			"output":     []any{},
		},
	}
	return [][]byte{responsesEvent("response.created", body, &e.seq)}
}

func (e *responsesStreamEncoder) Emit(d Delta) [][]byte {
	if !e.open {
		return append(e.Start(e.id, e.model, 0), e.Emit(d)...)
	}
	switch d.Kind {
	case DeltaStart:
		if d.ID != "" {
			e.id = d.ID
		}
		if d.Model != "" {
			e.model = d.Model
		}
		return nil
	case DeltaThinking:
		if d.Text == "" {
			return nil
		}
		var frames [][]byte
		if e.openKind != "reasoning" {
			frames = append(frames, e.closeItem()...)
			e.openReasoning()
			frames = append(frames, e.openReasoningFrames()...)
		}
		e.reasoningBuf.WriteString(d.Text)
		frames = append(frames, responsesEvent("response.reasoning_summary_text.delta", map[string]any{
			"type":          "response.reasoning_summary_text.delta",
			"item_id":       e.openItemID,
			"output_index":  e.openIndex,
			"summary_index": 0,
			"delta":         d.Text,
		}, &e.seq))
		return frames
	case DeltaToolCall:
		if d.Started || e.openKind != "function_call" || e.openItemID == "" {
			var frames [][]byte
			if d.Started || e.openKind != "function_call" {
				frames = append(frames, e.closeItem()...)
				e.openCall(d)
				frames = append(frames, responsesEvent("response.output_item.added", map[string]any{
					"type":         "response.output_item.added",
					"output_index": e.openIndex,
					"item": map[string]any{
						"type":      "function_call",
						"id":        e.openItemID,
						"call_id":   nonEmpty(d.ToolCallID, e.openItemID),
						"name":      d.ToolName,
						"arguments": "",
						"status":    "in_progress",
					},
				}, &e.seq))
			}
			if d.ToolArgs != "" {
				frames = append(frames, responsesEvent("response.function_call_arguments.delta", map[string]any{
					"type":         "response.function_call_arguments.delta",
					"item_id":      e.openItemID,
					"output_index": e.openIndex,
					"delta":        d.ToolArgs,
				}, &e.seq))
			}
			return frames
		}
		if d.ToolArgs != "" {
			return [][]byte{responsesEvent("response.function_call_arguments.delta", map[string]any{
				"type":         "response.function_call_arguments.delta",
				"item_id":      e.openItemID,
				"output_index": e.openIndex,
				"delta":        d.ToolArgs,
			}, &e.seq)}
		}
		return nil
	case DeltaText:
		if d.Text == "" {
			return nil
		}
		var frames [][]byte
		if e.openKind != "message" {
			frames = append(frames, e.closeItem()...)
			e.openMessage()
			frames = append(frames, responsesEvent("response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": e.openIndex,
				"item": map[string]any{
					"type":    "message",
					"id":      e.openItemID,
					"role":    "assistant",
					"status":  "in_progress",
					"content": []any{},
				},
			}, &e.seq))
			frames = append(frames, responsesEvent("response.content_part.added", map[string]any{
				"type":          "response.content_part.added",
				"item_id":       e.openItemID,
				"output_index":  e.openIndex,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			}, &e.seq))
		}
		e.textBuf.WriteString(d.Text)
		frames = append(frames, responsesEvent("response.output_text.delta", map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       e.openItemID,
			"output_index":  e.openIndex,
			"content_index": 0,
			"delta":         d.Text,
		}, &e.seq))
		return frames
	}
	return nil
}

func (e *responsesStreamEncoder) Finish(stopReason string, usage *Usage) [][]byte {
	if e.finished {
		return nil
	}
	e.finished = true
	var frames [][]byte
	frames = append(frames, e.closeItem()...)
	status := "completed"
	if stopReason == StopLength {
		status = "incomplete"
	}
	typeName := "response.completed"
	if status == "incomplete" {
		typeName = "response.incomplete"
	}
	body := map[string]any{
		"type": typeName,
		"response": map[string]any{
			"id":     e.id,
			"object": "response",
			"status": status,
			"model":  e.model,
			"output": []any{},
		},
	}
	if status == "incomplete" {
		body["response"].(map[string]any)["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	inner := body["response"].(map[string]any)
	if usage != nil {
		prompt, completion, total := usage.Resolved()
		u := map[string]any{
			"input_tokens":  prompt,
			"output_tokens": completion,
			"total_tokens":  total,
		}
		if usage.CacheRead > 0 {
			u["input_tokens_details"] = map[string]any{"cached_tokens": usage.CacheRead}
		}
		if usage.Reasoning > 0 {
			u["output_tokens_details"] = map[string]any{"reasoning_tokens": usage.Reasoning}
		}
		inner["usage"] = u
	}
	frames = append(frames, responsesEvent(typeName, body, &e.seq))
	return frames
}

func (e *responsesStreamEncoder) openReasoning() {
	e.openKind = "reasoning"
	e.openIndex = e.nextIndex()
	e.openItemID = "rs_" + e.id + "_" + fmt.Sprint(e.openIndex)
}

func (e *responsesStreamEncoder) openReasoningFrames() [][]byte {
	return [][]byte{responsesEvent("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": e.openIndex,
		"item": map[string]any{
			"type":    "reasoning",
			"id":      e.openItemID,
			"summary": []any{},
		},
	}, &e.seq)}
}

func (e *responsesStreamEncoder) openMessage() {
	e.openKind = "message"
	e.openIndex = e.nextIndex()
	e.openItemID = "msg_" + e.id + "_" + fmt.Sprint(e.openIndex)
}

func (e *responsesStreamEncoder) openCall(d Delta) {
	e.openKind = "function_call"
	e.openIndex = e.nextIndex()
	e.openItemID = nonEmpty(d.ToolCallID, "fc_"+e.id+"_"+fmt.Sprint(e.openIndex))
}

// closeItem closes whatever output item is open, emitting its done events.
func (e *responsesStreamEncoder) closeItem() [][]byte {
	if e.openKind == "" {
		return nil
	}
	var frames [][]byte
	switch e.openKind {
	case "reasoning":
		summary := e.reasoningBuf.String()
		frames = append(frames, responsesEvent("response.reasoning_summary_text.done", map[string]any{
			"type":          "response.reasoning_summary_text.done",
			"item_id":       e.openItemID,
			"output_index":  e.openIndex,
			"summary_index": 0,
			"text":          summary,
		}, &e.seq))
		frames = append(frames, responsesEvent("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": e.openIndex,
			"item": map[string]any{
				"type":    "reasoning",
				"id":      e.openItemID,
				"summary": []any{map[string]any{"type": "summary_text", "text": summary}},
			},
		}, &e.seq))
	case "message":
		text := e.textBuf.String()
		frames = append(frames, responsesEvent("response.output_text.done", map[string]any{
			"type":          "response.output_text.done",
			"item_id":       e.openItemID,
			"output_index":  e.openIndex,
			"content_index": 0,
			"text":          text,
		}, &e.seq))
		frames = append(frames, responsesEvent("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": e.openIndex,
			"item": map[string]any{
				"type":   "message",
				"id":     e.openItemID,
				"role":   "assistant",
				"status": "completed",
				"content": []any{map[string]any{
					"type":        "output_text",
					"text":        text,
					"annotations": []any{},
				}},
			},
		}, &e.seq))
	case "function_call":
		frames = append(frames, responsesEvent("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": e.openIndex,
			"item": map[string]any{
				"type":    "function_call",
				"id":      e.openItemID,
				"call_id": e.openItemID,
				"status":  "completed",
			},
		}, &e.seq))
	}
	e.openKind = ""
	e.openItemID = ""
	return frames
}

func (e *responsesStreamEncoder) nextIndex() int {
	idx := e.seq
	e.seq++
	return idx
}

func responsesEvent(name string, body map[string]any, seq *int) []byte {
	body["sequence_number"] = *seq
	*seq++
	raw, _ := json.Marshal(body)
	return []byte("event: " + name + "\ndata: " + string(raw) + "\n\n")
}

// UpstreamMessageText is the vendor's own words from an error body: never a page
// of HTML, never longer than 300 bytes, never carrying a key.
func UpstreamMessageText(body []byte) string {
	return upstreamMessage(body)
}
