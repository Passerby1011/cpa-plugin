// Package protocol — Anthropic messages codec.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AnthropicVersion is the version header /v1/messages requires. The plugin
// fills it in only when the client did not already send one.
const AnthropicVersion = "2023-06-01"

// ---------------------------------------------------------------------------
// request: messages -> IR
// ---------------------------------------------------------------------------

// DecodeClaudeRequest decodes a /v1/messages body into the canonical request.
func DecodeClaudeRequest(body []byte) (*Request, error) {
	obj, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	req := &Request{
		Model:       stringField(obj, "model"),
		Stream:      boolField(obj, "stream"),
		MaxOutput:   firstIntField(obj, "max_tokens", "max_output_tokens"),
		Temperature: ptrField(obj, "temperature"),
		TopP:        ptrField(obj, "top_p"),
		Stop:        stopList(obj),
		User:        firstStringField(obj, "metadata.user_id"),
	}
	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	// The system field is a string or an array of text blocks.
	if raw, ok := obj["system"]; ok {
		text, parts, err := stringOrParts(raw)
		if err != nil {
			return nil, err
		}
		var b strings.Builder
		b.WriteString(text)
		for _, p := range parts {
			var part map[string]json.RawMessage
			if json.Unmarshal(p, &part) == nil {
				b.WriteString(stringField(part, "text"))
			}
		}
		req.System = strings.TrimSpace(b.String())
	}
	// thinking: {"type":"enabled","budget_tokens":N} carries the budget;
	// a client that asks for a level only does so through the SDK's own
	// mapping, which arrives here as a budget.
	if raw, ok := obj["thinking"]; ok {
		var t map[string]json.RawMessage
		if json.Unmarshal(raw, &t) == nil {
			kind := strings.ToLower(stringField(t, "type"))
			if kind == "enabled" || kind == "adaptive" {
				req.Budget = firstIntField(t, "budget_tokens")
				if req.Budget <= 0 {
					// "enabled" with no budget is the SDK's default: 2048.
					req.Budget = 2048
				}
			}
		}
	}
	if err := decodeClaudeTurns(obj, req); err != nil {
		return nil, err
	}
	if err := decodeClaudeTools(obj, req); err != nil {
		return nil, err
	}
	return req, nil
}

func decodeClaudeTurns(obj map[string]json.RawMessage, req *Request) error {
	raw, ok := obj["messages"]
	if !ok {
		return fmt.Errorf("messages is required")
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return ErrMalformed("messages")
	}
	for _, msg := range messages {
		role := strings.ToLower(strings.TrimSpace(stringField(msg, "role")))
		text, parts, err := stringOrParts(msg["content"])
		if err != nil {
			return err
		}
		switch role {
		case "user":
			turn, err := decodeClaudeUserTurn(text, parts)
			if err != nil {
				return err
			}
			req.Turns = append(req.Turns, turn)
		case "assistant":
			turn, err := decodeClaudeAssistantTurn(text, parts)
			if err != nil {
				return err
			}
			req.Turns = append(req.Turns, turn)
		case "system":
			// Anthropic has no system message; a client that sends one is
			// folded into the system prompt rather than refused.
			if s := strings.TrimSpace(text); s != "" {
				if req.System != "" {
					req.System += "\n\n"
				}
				req.System += s
			}
		}
	}
	return nil
}

func decodeClaudeUserTurn(text string, parts []json.RawMessage) (Turn, error) {
	turn := Turn{Role: RoleUser}
	if text != "" {
		turn.Parts = append(turn.Parts, Part{Kind: PartText, Text: text})
	}
	for _, raw := range parts {
		var part map[string]json.RawMessage
		if err := json.Unmarshal(raw, &part); err != nil {
			return Turn{}, ErrMalformed("content")
		}
		switch strings.ToLower(stringField(part, "type")) {
		case "text":
			turn.Parts = append(turn.Parts, Part{Kind: PartText, Text: stringField(part, "text")})
		case "image":
			url, media := "", ""
			if src, ok := part["source"]; ok {
				var source map[string]json.RawMessage
				if json.Unmarshal(src, &source) == nil {
					switch strings.ToLower(stringField(source, "type")) {
					case "base64":
						media = stringField(source, "media_type")
						url = "data:" + media + ";base64," + stringField(source, "data")
					case "url":
						url = stringField(source, "url")
					}
				}
			}
			turn.Parts = append(turn.Parts, Part{Kind: PartImage, ImageURL: url, MediaType: media})
		case "tool_result":
			text := claudeToolResultText(part["content"])
			turn.Parts = append(turn.Parts, Part{
				Kind:       PartToolResult,
				ToolCallID: stringField(part, "tool_use_id"),
				Text:       text,
				IsError:    boolField(part, "is_error"),
			})
		case "document", "file":
			// A document block carries text or a base64 payload; text is kept,
			// a binary document becomes a text placeholder rather than a
			// silent hole in the conversation.
			turn.Parts = append(turn.Parts, Part{Kind: PartText, Text: claudeDocumentText(part)})
		default:
			if t := stringField(part, "text"); t != "" {
				turn.Parts = append(turn.Parts, Part{Kind: PartText, Text: t})
			}
		}
	}
	if len(turn.Parts) == 0 {
		turn.Parts = []Part{{Kind: PartText, Text: ""}}
	}
	return turn, nil
}

func decodeClaudeAssistantTurn(text string, parts []json.RawMessage) (Turn, error) {
	turn := Turn{Role: RoleAssistant}
	if text != "" {
		turn.Parts = append(turn.Parts, Part{Kind: PartText, Text: text})
	}
	for _, raw := range parts {
		var part map[string]json.RawMessage
		if err := json.Unmarshal(raw, &part); err != nil {
			return Turn{}, ErrMalformed("content")
		}
		switch strings.ToLower(stringField(part, "type")) {
		case "text":
			turn.Parts = append(turn.Parts, Part{Kind: PartText, Text: stringField(part, "text")})
		case "thinking":
			// Replayed thinking keeps its signature: the upstream refuses a
			// turn whose thinking block lost the signature it issued.
			turn.Parts = append(turn.Parts, Part{
				Kind:      PartThinking,
				Text:      firstStringField(part, "thinking", "text"),
				Signature: stringField(part, "signature"),
			})
		case "redacted_thinking":
			turn.Parts = append(turn.Parts, Part{
				Kind:      PartRedactedThinking,
				Text:      stringField(part, "data"),
				Signature: stringField(part, "data"),
			})
		case "tool_use":
			args, _ := json.Marshal(part["input"])
			turn.Parts = append(turn.Parts, Part{
				Kind:       PartToolCall,
				ToolCallID: stringField(part, "id"),
				ToolName:   stringField(part, "name"),
				Text:       string(args),
			})
		default:
			if t := stringField(part, "text"); t != "" {
				turn.Parts = append(turn.Parts, Part{Kind: PartText, Text: t})
			}
		}
	}
	if len(turn.Parts) == 0 {
		turn.Parts = []Part{{Kind: PartText, Text: ""}}
	}
	return turn, nil
}

// claudeToolResultText flattens a tool_result's content, which is a string or an
// array of blocks.
func claudeToolResultText(raw json.RawMessage) string {
	text, parts, err := stringOrParts(raw)
	if err != nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(text)
	for _, p := range parts {
		var part map[string]json.RawMessage
		if json.Unmarshal(p, &part) == nil {
			b.WriteString(stringField(part, "text"))
		}
	}
	return b.String()
}

func claudeDocumentText(part map[string]json.RawMessage) string {
	if src, ok := part["source"]; ok {
		var source map[string]json.RawMessage
		if json.Unmarshal(src, &source) == nil {
			switch strings.ToLower(stringField(source, "type")) {
			case "text":
				return stringField(source, "data")
			case "content":
				if raw, ok := source["data"]; ok {
					return claudeToolResultText(raw)
				}
			}
			if media := stringField(source, "media_type"); media != "" {
				return "[document: " + media + "]"
			}
		}
	}
	return ""
}

func decodeClaudeTools(obj map[string]json.RawMessage, req *Request) error {
	raw, ok := obj["tools"]
	if !ok {
		return nil
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return ErrMalformed("tools")
	}
	for _, t := range tools {
		// Anthropic's own shape: name + input_schema. A chat-shaped tool in a
		// messages body is accepted too.
		name := stringField(t, "name")
		schema := t["input_schema"]
		if fn, ok := t["function"]; ok && name == "" {
			var f map[string]json.RawMessage
			if json.Unmarshal(fn, &f) == nil {
				name = stringField(f, "name")
				schema = f["parameters"]
			}
		}
		if name == "" {
			continue
		}
		req.Tools = append(req.Tools, Tool{
			Name:        name,
			Description: stringField(t, "description"),
			Schema:      schema,
		})
	}
	// tool_choice: {"type":"auto"|"any"|"tool","name":...} or a bare string.
	if raw, ok := obj["tool_choice"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "auto":
				req.Choice = ToolChoice{Kind: "auto"}
			case "any", "required":
				req.Choice = ToolChoice{Kind: "required"}
			case "none":
				req.Choice = ToolChoice{Kind: "none"}
			}
		} else {
			var named map[string]json.RawMessage
			if json.Unmarshal(raw, &named) == nil {
				switch strings.ToLower(stringField(named, "type")) {
				case "auto":
					req.Choice = ToolChoice{Kind: "auto"}
				case "any", "required":
					req.Choice = ToolChoice{Kind: "required"}
				case "none":
					req.Choice = ToolChoice{Kind: "none"}
				case "tool":
					req.Choice = ToolChoice{Kind: "tool", Name: stringField(named, "name")}
				}
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// request: IR -> messages
// ---------------------------------------------------------------------------

// EncodeClaudeRequest renders the canonical request as an Anthropic messages
// body. The caller fills in the thinking block through EncodeClaudeThinking so
// the level -> budget decision stays in one place.
func EncodeClaudeRequest(req *Request, model string, stream bool) ([]byte, error) {
	out := map[string]any{
		"model":  model,
		"stream": stream,
	}
	maxTokens := req.MaxOutput
	if maxTokens <= 0 {
		// Anthropic requires max_tokens. A request that named none gets the
		// model's own output limit, which the caller has already capped.
		maxTokens = 8192
	}
	out["max_tokens"] = maxTokens
	if sys := req.SystemText(); sys != "" {
		out["system"] = sys
	}
	messages, err := encodeClaudeMessages(req)
	if err != nil {
		return nil, err
	}
	out["messages"] = messages
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		// Anthropic refuses temperature and top_p together in some versions;
		// a client that sent both keeps temperature.
		if req.Temperature == nil {
			out["top_p"] = *req.TopP
		}
	}
	if len(req.Stop) > 0 {
		out["stop_sequences"] = req.Stop
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tool := map[string]any{
				"name":         t.Name,
				"input_schema": schemaOrDefault(t.Schema),
			}
			if t.Description != "" {
				tool["description"] = t.Description
			}
			tools = append(tools, tool)
		}
		out["tools"] = tools
		if choice := encodeClaudeToolChoice(req.Choice); choice != nil {
			out["tool_choice"] = choice
		}
	}
	return json.Marshal(out)
}

// encodeClaudeMessages folds a tool-result turn into a user turn, which is the
// only place Anthropic accepts a tool_result block.
func encodeClaudeMessages(req *Request) ([]any, error) {
	out := make([]any, 0, len(req.Turns))
	for _, turn := range req.Turns {
		switch turn.Role {
		case RoleTool:
			blocks := make([]any, 0, len(turn.Parts))
			for _, p := range turn.Parts {
				if p.Kind != PartToolResult {
					continue
				}
				block := map[string]any{
					"type":        "tool_result",
					"tool_use_id": p.ToolCallID,
					"content":     p.Text,
				}
				if p.IsError {
					block["is_error"] = true
				}
				blocks = append(blocks, block)
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, map[string]any{"role": "user", "content": blocks})
		case RoleUser, RoleAssistant:
			blocks := make([]any, 0, len(turn.Parts))
			for _, p := range turn.Parts {
				switch p.Kind {
				case PartText:
					if p.Text == "" && len(turn.Parts) > 1 {
						continue
					}
					blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
				case PartImage:
					blocks = append(blocks, map[string]any{
						"type":   "image",
						"source": claudeImageSource(p.ImageURL, p.MediaType),
					})
				case PartThinking:
					if turn.Role != RoleAssistant {
						// A thought only belongs in an assistant turn.
						continue
					}
					block := map[string]any{"type": "thinking", "thinking": p.Text}
					if p.Signature != "" {
						block["signature"] = p.Signature
					}
					blocks = append(blocks, block)
				case PartRedactedThinking:
					blocks = append(blocks, map[string]any{"type": "redacted_thinking", "data": p.Text})
				case PartToolCall:
					blocks = append(blocks, map[string]any{
						"type":  "tool_use",
						"id":    p.ToolCallID,
						"name":  p.ToolName,
						"input": inputOrEmptyObject(p.Text),
					})
				}
			}
			if len(blocks) == 0 {
				blocks = append(blocks, map[string]any{"type": "text", "text": ""})
			}
			out = append(out, map[string]any{"role": string(turn.Role), "content": blocks})
		}
	}
	if len(out) == 0 {
		out = append(out, map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": ""}},
		})
	}
	return out, nil
}

func claudeImageSource(url, media string) map[string]any {
	if payload, mediaType := splitDataURL(url); strings.HasPrefix(payload, "data:") {
		if mediaType == "" {
			mediaType = media
		}
		if mediaType == "" {
			mediaType = "image/png"
		}
		comma := strings.Index(payload, ",")
		body := payload
		if comma >= 0 {
			body = payload[comma+1:]
		}
		return map[string]any{"type": "base64", "media_type": mediaType, "data": body}
	}
	return map[string]any{"type": "url", "url": url}
}

func inputOrEmptyObject(args string) json.RawMessage {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return json.RawMessage(`{}`)
	}
	// A tool_use input must be an object; a client that sent a non-object
	// fragment gets an empty object rather than a 400 from the upstream.
	if trimmed[0] != '{' {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(trimmed)
}

func encodeClaudeToolChoice(choice ToolChoice) any {
	switch choice.Kind {
	case "auto":
		return map[string]any{"type": "auto"}
	case "required":
		return map[string]any{"type": "any"}
	case "none":
		return map[string]any{"type": "none"}
	case "tool":
		return map[string]any{"type": "tool", "name": choice.Name}
	}
	if choice.Name != "" {
		return map[string]any{"type": "tool", "name": choice.Name}
	}
	return nil
}

// EncodeClaudeThinking writes the request's thinking block for a model whose
// declared levels are `levels` and whose output budget is maxTokens.
//
// A model with no levels gets no thinking block: amr-link takes
// reasoning_effort there and never thinks (GPT-6 Luna, measured), so sending a
// budget would only spend tokens on nothing. `disabled` is honoured by
// omitting the block entirely — Anthropic's own "disabled" type is rejected by
// some gateways.
func EncodeClaudeThinking(body []byte, budget int64, levels []string, maxTokens int64) ([]byte, error) {
	if budget <= 0 {
		return body, nil
	}
	if len(levels) == 0 {
		return body, nil
	}
	// Anthropic refuses a budget at or above max_tokens; the SDK's own guard is
	// to leave room for the answer.
	if maxTokens > 0 && budget >= maxTokens {
		budget = maxTokens - 1
	}
	if budget < 1024 {
		budget = 1024
	}
	obj, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	obj["thinking"] = json.RawMessage(fmt.Sprintf(`{"type":"enabled","budget_tokens":%d}`, budget))
	return json.Marshal(obj)
}

// ---------------------------------------------------------------------------
// response: messages -> IR
// ---------------------------------------------------------------------------

// DecodeClaudeResponse decodes a non-streaming messages body.
//
// amr-link answers /messages with an SSE-framed body even for a non-streaming
// request (its gateway always frames), so a body whose first bytes are data:
// lines is decoded as the event sequence it is rather than as one JSON object.
func DecodeClaudeResponse(body []byte) (*Response, error) {
	if looksLikeSSE(body) {
		return decodeClaudeEventStream(body)
	}
	obj, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	resp := &Response{
		ID:    stringField(obj, "id"),
		Model: stringField(obj, "model"),
	}
	// A JSON non-stream response carries an error instead of content.
	if e := stringField(obj, "type"); e == "error" {
		return nil, errUpstreamBody(0, body)
	}
	for _, rawPart := range jsonArray(obj["content"]) {
		var part map[string]json.RawMessage
		if json.Unmarshal(rawPart, &part) != nil {
			continue
		}
		switch strings.ToLower(stringField(part, "type")) {
		case "text":
			resp.Parts = append(resp.Parts, Part{Kind: PartText, Text: stringField(part, "text")})
		case "thinking":
			resp.Parts = append(resp.Parts, Part{Kind: PartThinking, Text: firstStringField(part, "thinking", "text"), Signature: stringField(part, "signature")})
		case "redacted_thinking":
			resp.Parts = append(resp.Parts, Part{Kind: PartRedactedThinking, Text: stringField(part, "data"), Signature: stringField(part, "data")})
		case "tool_use":
			args, _ := json.Marshal(part["input"])
			resp.Parts = append(resp.Parts, Part{Kind: PartToolCall, ToolCallID: stringField(part, "id"), ToolName: stringField(part, "name"), Text: string(args)})
		}
	}
	resp.StopReason = normalizeStop(stringField(obj, "stop_reason"))
	if raw, ok := obj["usage"]; ok {
		var u map[string]json.RawMessage
		if json.Unmarshal(raw, &u) == nil {
			resp.Usage = decodeClaudeUsage(u)
		}
	}
	return resp, nil
}

func decodeClaudeUsage(u map[string]json.RawMessage) Usage {
	return Usage{
		Input:     firstIntField(u, "input_tokens"),
		Output:    firstIntField(u, "output_tokens"),
		CacheRead: firstIntField(u, "cache_read_input_tokens"),
		CacheHit:  firstIntField(u, "cache_creation_input_tokens"),
	}
}

// claudeToolState accumulates one tool_use block while its arguments stream.
type claudeToolState struct {
	id   string
	name string
	args strings.Builder
}

// decodeClaudeEventStream reassembles the message a framed event sequence
// describes: message_start, the content blocks, message_delta, message_stop.
func decodeClaudeEventStream(body []byte) (*Response, error) {
	resp := &Response{}
	var textBuf, thinkBuf strings.Builder
	var signature string
	var tools []*claudeToolState
	for _, payload := range sseDataPayloads(body) {
		ev, err := decodeObject([]byte(payload))
		if err != nil {
			continue
		}
		switch strings.ToLower(stringField(ev, "type")) {
		case "message_start":
			if msg, ok := ev["message"]; ok {
				var m map[string]json.RawMessage
				if json.Unmarshal(msg, &m) == nil {
					resp.ID = stringField(m, "id")
					resp.Model = stringField(m, "model")
					if u, ok := m["usage"]; ok {
						var um map[string]json.RawMessage
						if json.Unmarshal(u, &um) == nil {
							resp.Usage = decodeClaudeUsage(um)
						}
					}
				}
			}
		case "content_block_start":
			index := int(intField(ev, "index"))
			if block, ok := ev["content_block"]; ok {
				var b map[string]json.RawMessage
				if json.Unmarshal(block, &b) == nil {
					switch strings.ToLower(stringField(b, "type")) {
					case "tool_use":
						for len(tools) <= index {
							tools = append(tools, &claudeToolState{})
						}
						tools[index] = &claudeToolState{
							id:   stringField(b, "id"),
							name: stringField(b, "name"),
						}
					case "thinking":
						thinkBuf.WriteString(firstStringField(b, "thinking", "text"))
					case "text":
						textBuf.WriteString(stringField(b, "text"))
					}
				}
			}
		case "content_block_delta":
			index := int(intField(ev, "index"))
			if delta, ok := ev["delta"]; ok {
				var d map[string]json.RawMessage
				if json.Unmarshal(delta, &d) == nil {
					switch strings.ToLower(stringField(d, "type")) {
					case "text_delta":
						textBuf.WriteString(stringField(d, "text"))
					case "thinking_delta":
						thinkBuf.WriteString(stringField(d, "thinking"))
					case "signature_delta":
						if sig := stringField(d, "signature"); sig != "" {
							signature = sig
						}
					case "input_json_delta":
						for len(tools) <= index {
							tools = append(tools, &claudeToolState{})
						}
						if tools[index] == nil {
							tools[index] = &claudeToolState{}
						}
						tools[index].args.WriteString(stringField(d, "partial_json"))
					}
				}
			}
		case "message_delta":
			if delta, ok := ev["delta"]; ok {
				var d map[string]json.RawMessage
				if json.Unmarshal(delta, &d) == nil {
					resp.StopReason = normalizeStop(stringField(d, "stop_reason"))
				}
			}
			if u, ok := ev["usage"]; ok {
				var um map[string]json.RawMessage
				if json.Unmarshal(u, &um) == nil {
					out := decodeClaudeUsage(um)
					if out.Output > 0 {
						resp.Usage.Output = out.Output
					}
					if out.Input > 0 {
						resp.Usage.Input = out.Input
					}
				}
			}
		case "error":
			msg := ""
			if e, ok := ev["error"]; ok {
				var em map[string]json.RawMessage
				if json.Unmarshal(e, &em) == nil {
					msg = stringField(em, "message")
				}
			}
			if msg != "" {
				return nil, fmt.Errorf("upstream answered an error: %s", truncate(msg, 300))
			}
		}
	}
	resp.Parts = assembleOrdered(thinkBuf.String(), signature, textBuf.String(), tools)
	if resp.StopReason == StopUnknown {
		if resp.HasToolCalls() {
			resp.StopReason = StopToolUse
		} else {
			resp.StopReason = StopEnd
		}
	}
	return resp, nil
}

// assembleOrdered builds parts with the thought first, then the answer, then the
// tool calls — the order every schema renders them in.
func assembleOrdered(think, signature, text string, tools []*claudeToolState) []Part {
	var parts []Part
	if think != "" {
		parts = append(parts, Part{Kind: PartThinking, Text: think, Signature: signature})
	}
	if text != "" {
		parts = append(parts, Part{Kind: PartText, Text: text})
	}
	for _, t := range tools {
		if t == nil {
			continue
		}
		parts = append(parts, Part{Kind: PartToolCall, ToolCallID: t.id, ToolName: t.name, Text: t.args.String()})
	}
	return parts
}

// looksLikeSSE reports whether a body is a framed event sequence.
func looksLikeSSE(body []byte) bool {
	trimmed := strings.TrimSpace(string(body))
	return strings.HasPrefix(trimmed, "event:") || strings.HasPrefix(trimmed, "data:")
}

// sseDataPayloads splits an SSE body into its data payloads.
func sseDataPayloads(body []byte) []string {
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		if payload, ok := isDataLine([]byte(line)); ok && payload != "" {
			out = append(out, payload)
		}
	}
	return out
}

func jsonArray(raw json.RawMessage) []json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var out []json.RawMessage
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// response: IR -> messages
// ---------------------------------------------------------------------------

// EncodeClaudeResponse renders a canonical response as a messages body.
func EncodeClaudeResponse(resp *Response, model string) ([]byte, error) {
	var content []any
	if think := resp.Thinking(); think != "" {
		block := map[string]any{"type": "thinking", "thinking": think}
		for _, p := range resp.Parts {
			if p.Kind == PartThinking && p.Signature != "" {
				block["signature"] = p.Signature
				break
			}
		}
		content = append(content, block)
	}
	for _, p := range resp.Parts {
		if p.Kind == PartRedactedThinking {
			content = append(content, map[string]any{"type": "redacted_thinking", "data": p.Text})
		}
	}
	if text := resp.Text(); text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, c := range resp.ToolCalls() {
		id := c.ToolCallID
		if id == "" {
			id = fmt.Sprintf("toolu_%d", resp.CreatedAt)
		}
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  c.ToolName,
			"input": inputOrEmptyObject(c.Text),
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	out := map[string]any{
		"id":            nonEmpty(resp.ID, "msg_opendesign"),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   claudeStopReason(resp.StopReason),
		"stop_sequence": nil,
	}
	out["usage"] = claudeUsageBody(resp.Usage)
	return json.Marshal(out)
}

func claudeStopReason(stop string) string {
	switch stop {
	case StopLength:
		return "max_tokens"
	case StopToolUse:
		return "tool_use"
	case StopFiltered:
		return "refusal"
	default:
		return "end_turn"
	}
}

func claudeUsageBody(u Usage) map[string]any {
	out := map[string]any{
		"input_tokens":  u.Input,
		"output_tokens": u.Output,
	}
	// cache_* fields are only meaningful when non-zero; Anthropic omits them
	// otherwise, and a client that reads them treats 0 as "not reported".
	if u.CacheRead > 0 {
		out["cache_read_input_tokens"] = u.CacheRead
	}
	if u.CacheHit > 0 {
		out["cache_creation_input_tokens"] = u.CacheHit
	}
	return out
}
