// Package protocol — OpenAI chat completions codec.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// request: chat completions -> IR
// ---------------------------------------------------------------------------

// DecodeChatRequest decodes a /v1/chat/completions body into the canonical
// request.
func DecodeChatRequest(body []byte) (*Request, error) {
	obj, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	req := &Request{
		Model:       stringField(obj, "model"),
		Stream:      boolField(obj, "stream"),
		MaxOutput:   firstIntField(obj, "max_completion_tokens", "max_tokens"),
		Temperature: ptrField(obj, "temperature"),
		TopP:        ptrField(obj, "top_p"),
		Stop:        stopList(obj),
		Effort:      normalizeEffortName(stringField(obj, "reasoning_effort")),
		User:        stringField(obj, "user"),
	}
	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	// A client that asks for thinking with a level but names no budget has
	// expressed a level; the encoder decides whether the target model takes it.
	if raw, ok := obj["reasoning"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) == nil {
			if e := stringField(nested, "effort"); e != "" {
				req.Effort = normalizeEffortName(e)
			}
		}
	}
	if err := decodeChatTurns(obj, req); err != nil {
		return nil, err
	}
	if err := decodeChatTools(obj, req); err != nil {
		return nil, err
	}
	return req, nil
}

func decodeChatTurns(obj map[string]json.RawMessage, req *Request) error {
	raw, ok := obj["messages"]
	if !ok {
		return fmt.Errorf("messages is required")
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return ErrMalformed("messages")
	}
	for _, rawMsg := range messages {
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			return ErrMalformed("messages")
		}
		role := strings.ToLower(strings.TrimSpace(stringField(msg, "role")))
		switch role {
		case "system", "developer":
			text, parts, err := stringOrParts(msg["content"])
			if err != nil {
				return err
			}
			// A system turn's array form carries text only; anything else is
			// concatenated as text rather than silently dropped.
			var b strings.Builder
			b.WriteString(text)
			for _, p := range parts {
				var part map[string]json.RawMessage
				if json.Unmarshal(p, &part) == nil {
					b.WriteString(stringField(part, "text"))
				}
			}
			if s := strings.TrimSpace(b.String()); s != "" {
				if req.System != "" {
					req.System += "\n\n"
				}
				req.System += s
			}
		case "user":
			text, parts, err := stringOrParts(msg["content"])
			if err != nil {
				return err
			}
			turn := Turn{Role: RoleUser}
			if text != "" {
				turn.Parts = append(turn.Parts, Part{Kind: PartText, Text: text})
			}
			for _, p := range parts {
				part, err := decodeChatContentPart(p)
				if err != nil {
					return err
				}
				turn.Parts = append(turn.Parts, part)
			}
			req.Turns = append(req.Turns, turn)
		case "assistant":
			turn := Turn{Role: RoleAssistant}
			text, parts, err := stringOrParts(msg["content"])
			if err != nil {
				return err
			}
			if text != "" {
				turn.Parts = append(turn.Parts, Part{Kind: PartText, Text: text})
			}
			for _, p := range parts {
				var part map[string]json.RawMessage
				if json.Unmarshal(p, &part) != nil {
					continue
				}
				kind := stringField(part, "type")
				if kind == "text" {
					turn.Parts = append(turn.Parts, Part{Kind: PartText, Text: stringField(part, "text")})
				}
			}
			// A reasoning turn replayed by the client (some SDKs keep the
			// previous turn's reasoning_content in history) is carried as a
			// thinking part so an Anthropic target can replay it.
			if reason := firstStringField(msg, "reasoning_content", "reasoning"); reason != "" {
				turn.Parts = append(turn.Parts, Part{Kind: PartThinking, Text: reason})
			}
			for _, rawCall := range chatToolCalls(msg) {
				cid := stringField(rawCall, "id")
				name, args := "", ""
				if fn, ok := rawCall["function"]; ok {
					var f map[string]json.RawMessage
					if json.Unmarshal(fn, &f) == nil {
						name = stringField(f, "name")
						args = stringField(f, "arguments")
					}
				}
				turn.Parts = append(turn.Parts, Part{
					Kind:       PartToolCall,
					ToolCallID: cid,
					ToolName:   name,
					Text:       args,
				})
			}
			// An assistant turn that is empty is still a turn: dropping it
			// would reorder the conversation the upstream sees.
			if len(turn.Parts) == 0 {
				turn.Parts = []Part{{Kind: PartText, Text: ""}}
			}
			req.Turns = append(req.Turns, turn)
		case "tool", "function":
			text, parts, err := stringOrParts(msg["content"])
			if err != nil {
				return err
			}
			var b strings.Builder
			b.WriteString(text)
			for _, p := range parts {
				var part map[string]json.RawMessage
				if json.Unmarshal(p, &part) == nil {
					b.WriteString(stringField(part, "text"))
				}
			}
			req.Turns = append(req.Turns, Turn{
				Role: RoleTool,
				Parts: []Part{{
					Kind:       PartToolResult,
					ToolCallID: firstStringField(msg, "tool_call_id", "id", "name"),
					Text:       b.String(),
				}},
			})
		}
	}
	return nil
}

// chatToolCalls reads an assistant turn's tool_calls array.
func chatToolCalls(msg map[string]json.RawMessage) []map[string]json.RawMessage {
	raw, ok := msg["tool_calls"]
	if !ok {
		return nil
	}
	var list []map[string]json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	// Legacy single function_call shape.
	if len(list) == 0 {
		if fc, ok := msg["function_call"]; ok && len(fc) > 0 && string(fc) != "null" {
			list = append(list, map[string]json.RawMessage{
				"id":       json.RawMessage(`""`),
				"function": fc,
			})
		}
	}
	return list
}

func decodeChatContentPart(raw json.RawMessage) (Part, error) {
	var part map[string]json.RawMessage
	if err := json.Unmarshal(raw, &part); err != nil {
		return Part{}, ErrMalformed("content")
	}
	switch strings.ToLower(stringField(part, "type")) {
	case "text", "input_text":
		return Part{Kind: PartText, Text: stringField(part, "text")}, nil
	case "image_url", "input_image":
		url, media := "", ""
		if nested, ok := part["image_url"]; ok {
			var img map[string]json.RawMessage
			if json.Unmarshal(nested, &img) == nil {
				url = stringField(img, "url")
			} else {
				url = strings.Trim(strings.TrimSpace(string(nested)), `"`)
			}
		}
		if url == "" {
			url = stringField(part, "image_url")
		}
		_, media = splitDataURL(url)
		return Part{Kind: PartImage, ImageURL: url, MediaType: media}, nil
	}
	// An unrecognized part is kept as text when it carries text, and refused
	// otherwise rather than silently sending a turn the model cannot read.
	if text := stringField(part, "text"); text != "" {
		return Part{Kind: PartText, Text: text}, nil
	}
	return Part{}, fmt.Errorf("unsupported content part type %q", stringField(part, "type"))
}

func decodeChatTools(obj map[string]json.RawMessage, req *Request) error {
	raw, ok := obj["tools"]
	if !ok {
		return nil
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return ErrMalformed("tools")
	}
	for _, t := range tools {
		if fn, ok := t["function"]; ok {
			var f map[string]json.RawMessage
			if json.Unmarshal(fn, &f) != nil {
				continue
			}
			req.Tools = append(req.Tools, Tool{
				Name:        stringField(f, "name"),
				Description: stringField(f, "description"),
				Schema:      f["parameters"],
			})
			continue
		}
		// A Responses-shaped tool in a chat body (and vice versa) is accepted:
		// the name and schema are the same, only the wrapper differs.
		if name := stringField(t, "name"); name != "" {
			req.Tools = append(req.Tools, Tool{
				Name:        name,
				Description: stringField(t, "description"),
				Schema:      t["parameters"],
			})
		}
	}
	decodeChatToolChoice(obj, req)
	return nil
}

func decodeChatToolChoice(obj map[string]json.RawMessage, req *Request) {
	raw, ok := obj["tool_choice"]
	if !ok {
		return
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "auto", "required", "none":
			req.Choice = ToolChoice{Kind: strings.ToLower(strings.TrimSpace(s))}
		}
		return
	}
	var named map[string]json.RawMessage
	if json.Unmarshal(raw, &named) == nil {
		name := ""
		if fn, ok := named["function"]; ok {
			var f map[string]json.RawMessage
			if json.Unmarshal(fn, &f) == nil {
				name = stringField(f, "name")
			}
		}
		if name == "" {
			name = stringField(named, "name")
		}
		if name != "" {
			req.Choice = ToolChoice{Kind: "tool", Name: name}
		}
	}
}

// ---------------------------------------------------------------------------
// request: IR -> chat completions
// ---------------------------------------------------------------------------

// EncodeChatRequest renders the canonical request as a chat completions body.
func EncodeChatRequest(req *Request, model string, stream bool) ([]byte, error) {
	out := map[string]any{
		"model":  model,
		"stream": stream,
	}
	var messages []any
	if sys := req.SystemText(); sys != "" {
		messages = append(messages, map[string]any{"role": "system", "content": sys})
	}
	for _, turn := range req.Turns {
		msg, err := encodeChatTurn(turn)
		if err != nil {
			return nil, err
		}
		if msg != nil {
			messages = append(messages, msg)
		}
	}
	if len(messages) == 0 {
		messages = append(messages, map[string]any{"role": "user", "content": ""})
	}
	out["messages"] = messages

	if req.MaxOutput > 0 {
		out["max_tokens"] = req.MaxOutput
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		out["stop"] = req.Stop
	}
	if req.User != "" {
		out["user"] = req.User
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			fn["parameters"] = schemaOrDefault(t.Schema)
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		out["tools"] = tools
		if choice := encodeChatToolChoice(req.Choice); choice != nil {
			out["tool_choice"] = choice
		}
	}
	return json.Marshal(out)
}

func encodeChatTurn(turn Turn) (any, error) {
	switch turn.Role {
	case RoleTool:
		parts := toolResultParts(turn)
		if len(parts) == 0 {
			return nil, nil
		}
		p := parts[0]
		msg := map[string]any{
			"role":         "tool",
			"tool_call_id": p.ToolCallID,
			"content":      p.Text,
		}
		return msg, nil
	case RoleAssistant:
		text := turn.Text()
		var calls []any
		var reasoning string
		for _, p := range turn.Parts {
			switch p.Kind {
			case PartToolCall:
				calls = append(calls, map[string]any{
					"id":   p.ToolCallID,
					"type": "function",
					"function": map[string]any{
						"name":      p.ToolName,
						"arguments": argsOrEmptyObject(p.Text),
					},
				})
			case PartThinking:
				reasoning += p.Text
			}
		}
		msg := map[string]any{"role": "assistant", "content": text}
		if len(calls) > 0 {
			msg["tool_calls"] = calls
			if text == "" {
				msg["content"] = nil
			}
		}
		if reasoning != "" {
			msg["reasoning_content"] = reasoning
		}
		return msg, nil
	default:
		// user, and any role this plugin does not model: rendered as a user
		// turn so its content is not lost.
		var parts []any
		var plain strings.Builder
		allText := true
		for _, p := range turn.Parts {
			switch p.Kind {
			case PartText:
				parts = append(parts, map[string]any{"type": "text", "text": p.Text})
				plain.WriteString(p.Text)
			case PartImage:
				allText = false
				parts = append(parts, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url": p.ImageURL,
					},
				})
			case PartToolResult:
				parts = append(parts, map[string]any{"type": "text", "text": p.Text})
				plain.WriteString(p.Text)
			}
		}
		if len(parts) == 0 {
			return map[string]any{"role": "user", "content": ""}, nil
		}
		if allText {
			return map[string]any{"role": "user", "content": plain.String()}, nil
		}
		return map[string]any{"role": "user", "content": parts}, nil
	}
}

func encodeChatToolChoice(choice ToolChoice) any {
	switch choice.Kind {
	case "auto", "required", "none":
		return choice.Kind
	case "tool":
		return map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}
	}
	if choice.Name != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}
	}
	return nil
}

// ---------------------------------------------------------------------------
// response: chat completions -> IR
// ---------------------------------------------------------------------------

// DecodeChatResponse decodes a non-streaming chat completions body.
func DecodeChatResponse(body []byte) (*Response, error) {
	obj, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	resp := &Response{
		ID:        stringField(obj, "id"),
		Model:     stringField(obj, "model"),
		CreatedAt: intField(obj, "created"),
	}
	var choices []map[string]json.RawMessage
	if raw, ok := obj["choices"]; ok {
		if json.Unmarshal(raw, &choices) != nil {
			return nil, ErrMalformed("response")
		}
	}
	if len(choices) > 0 {
		choice := choices[0]
		if msg, ok := choice["message"]; ok {
			var m map[string]json.RawMessage
			if json.Unmarshal(msg, &m) == nil {
				text, parts, err := stringOrParts(m["content"])
				if err != nil {
					return nil, err
				}
				// A reasoning model answers with reasoning_content; it goes
				// FIRST so an encoder that writes parts in order puts the
				// thought before the answer, as every schema expects.
				if reason := firstStringField(m, "reasoning_content", "reasoning"); reason != "" {
					resp.Parts = append(resp.Parts, Part{Kind: PartThinking, Text: reason})
				}
				if text != "" {
					resp.Parts = append(resp.Parts, Part{Kind: PartText, Text: text})
				}
				for _, p := range parts {
					var part map[string]json.RawMessage
					if json.Unmarshal(p, &part) != nil {
						continue
					}
					if stringField(part, "type") == "text" {
						resp.Parts = append(resp.Parts, Part{Kind: PartText, Text: stringField(part, "text")})
					}
				}
				for _, rawCall := range chatToolCalls(m) {
					name, args := "", ""
					if fn, ok := rawCall["function"]; ok {
						var f map[string]json.RawMessage
						if json.Unmarshal(fn, &f) == nil {
							name = stringField(f, "name")
							args = stringField(f, "arguments")
						}
					}
					resp.Parts = append(resp.Parts, Part{
						Kind:       PartToolCall,
						ToolCallID: stringField(rawCall, "id"),
						ToolName:   name,
						Text:       args,
					})
				}
			}
		}
		resp.StopReason = normalizeStop(stringField(choice, "finish_reason"))
	}
	if raw, ok := obj["usage"]; ok {
		var u map[string]json.RawMessage
		if json.Unmarshal(raw, &u) == nil {
			resp.Usage = decodeChatUsage(u)
		}
	}
	if resp.StopReason == StopUnknown {
		if resp.HasToolCalls() {
			resp.StopReason = StopToolUse
		} else {
			resp.StopReason = StopEnd
		}
	}
	return resp, nil
}

func decodeChatUsage(u map[string]json.RawMessage) Usage {
	usage := Usage{
		Input:  firstIntField(u, "prompt_tokens", "input_tokens"),
		Output: firstIntField(u, "completion_tokens", "output_tokens"),
	}
	if raw, ok := u["completion_tokens_details"]; ok {
		var details map[string]json.RawMessage
		if json.Unmarshal(raw, &details) == nil {
			usage.Reasoning = firstIntField(details, "reasoning_tokens")
		}
	}
	if raw, ok := u["prompt_tokens_details"]; ok {
		var details map[string]json.RawMessage
		if json.Unmarshal(raw, &details) == nil {
			usage.CacheRead = firstIntField(details, "cached_tokens")
			usage.CacheHit = firstIntField(details, "cache_read_input_tokens")
		}
	}
	return usage
}

// ---------------------------------------------------------------------------
// response: IR -> chat completions
// ---------------------------------------------------------------------------

// EncodeChatResponse renders a canonical response as a chat completions body.
func EncodeChatResponse(resp *Response, model string) ([]byte, error) {
	message := map[string]any{"role": "assistant", "content": resp.Text()}
	if think := resp.Thinking(); think != "" {
		message["reasoning_content"] = think
	}
	if calls := resp.ToolCalls(); len(calls) > 0 {
		list := make([]any, 0, len(calls))
		for i, c := range calls {
			id := c.ToolCallID
			if id == "" {
				id = fmt.Sprintf("call_%d_%d", resp.CreatedAt, i)
			}
			list = append(list, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      c.ToolName,
					"arguments": argsOrEmptyObject(c.Text),
				},
			})
		}
		message["tool_calls"] = list
		if resp.Text() == "" {
			message["content"] = nil
		}
	}
	created := resp.CreatedAt
	if created <= 0 {
		created = 1
	}
	out := map[string]any{
		"id":      nonEmpty(resp.ID, "chatcmpl-opendesign"),
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": chatFinishReason(resp.StopReason),
		}},
	}
	prompt, completion, total := resp.Usage.Resolved()
	usage := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
	}
	if resp.Usage.CacheRead > 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": resp.Usage.CacheRead}
	}
	if resp.Usage.Reasoning > 0 {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": resp.Usage.Reasoning}
	}
	out["usage"] = usage
	return json.Marshal(out)
}

func chatFinishReason(stop string) string {
	switch stop {
	case StopLength:
		return "length"
	case StopToolUse:
		return "tool_calls"
	case StopFiltered:
		return "content_filter"
	default:
		return "stop"
	}
}

// EncodeChatReasoning writes the request's reasoning control for a model whose
// declared levels are `levels`.
//
// A model with no levels gets no control at all: amr-link accepts
// reasoning_effort on such a model and it never thinks (GPT-6 Luna, measured),
// so sending one spends a request field on nothing and — worse — makes a client
// believe thinking was enabled.
func EncodeChatReasoning(body []byte, effort string, levels []string) ([]byte, error) {
	effort = strings.TrimSpace(effort)
	if effort == "" || len(levels) == 0 {
		return body, nil
	}
	obj, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	obj["reasoning_effort"] = json.RawMessage(fmt.Sprintf("%q", effort))
	return json.Marshal(obj)
}
