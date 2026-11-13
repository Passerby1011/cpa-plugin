// Package protocol — OpenAI Responses codec (/v1/responses).
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// request: responses -> IR
// ---------------------------------------------------------------------------

// DecodeResponsesRequest decodes a /v1/responses body into the canonical
// request. `input` is either a string or an array of items; `instructions` is
// the system prompt.
func DecodeResponsesRequest(body []byte) (*Request, error) {
	obj, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	req := &Request{
		Model:       stringField(obj, "model"),
		Stream:      boolField(obj, "stream"),
		MaxOutput:   firstIntField(obj, "max_output_tokens", "max_tokens"),
		Temperature: ptrField(obj, "temperature"),
		TopP:        ptrField(obj, "top_p"),
		Stop:        stopList(obj),
		User:        stringField(obj, "user"),
	}
	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	req.System = strings.TrimSpace(stringField(obj, "instructions"))
	// A Responses client states its reasoning as {"effort": ...} (or, for
	// Anthropic models routed here, as a budget).
	if raw, ok := obj["reasoning"]; ok {
		var r map[string]json.RawMessage
		if json.Unmarshal(raw, &r) == nil {
			if e := stringField(r, "effort"); e != "" {
				req.Effort = normalizeEffortName(e)
			}
			req.Budget = firstIntField(r, "budget_tokens")
		}
	}
	if err := decodeResponsesInput(obj, req); err != nil {
		return nil, err
	}
	if err := decodeResponsesTools(obj, req); err != nil {
		return nil, err
	}
	return req, nil
}

func decodeResponsesInput(obj map[string]json.RawMessage, req *Request) error {
	raw, ok := obj["input"]
	if !ok {
		return fmt.Errorf("input is required")
	}
	// The string form is one user turn.
	if text, parts, err := stringOrParts(raw); err == nil && text != "" && len(parts) == 0 {
		req.Turns = append(req.Turns, Turn{Role: RoleUser, Parts: []Part{{Kind: PartText, Text: text}}})
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return ErrMalformed("input")
	}
	// A tool result in the Responses API is an item of its own
	// ({"type":"function_call_output"}), and every tool call is an item too;
	// they are folded into the surrounding turns so the canonical request only
	// ever has user/assistant/tool turns.
	var pendingToolResults []Part
	flushToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		req.Turns = append(req.Turns, Turn{Role: RoleTool, Parts: pendingToolResults})
		pendingToolResults = nil
	}
	for _, rawItem := range items {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(rawItem, &item); err != nil {
			return ErrMalformed("input")
		}
		kind := strings.ToLower(stringField(item, "type"))
		role := strings.ToLower(strings.TrimSpace(stringField(item, "role")))
		switch kind {
		case "", "message":
			flushToolResults()
			text, parts, err := stringOrParts(item["content"])
			if err != nil {
				return err
			}
			turn := Turn{Role: Role(role)}
			if turn.Role == "" || turn.Role == RoleSystem {
				if s := strings.TrimSpace(text); s != "" {
					if req.System != "" {
						req.System += "\n\n"
					}
					req.System += s
				}
				// A system message's array form carries text only.
				for _, p := range parts {
					var part map[string]json.RawMessage
					if json.Unmarshal(p, &part) == nil {
						if t := stringField(part, "text"); t != "" {
							if req.System != "" {
								req.System += "\n\n"
							}
							req.System += t
						}
					}
				}
				continue
			}
			if text != "" {
				turn.Parts = append(turn.Parts, Part{Kind: PartText, Text: text})
			}
			for _, p := range parts {
				var part map[string]json.RawMessage
				if json.Unmarshal(p, &part) != nil {
					continue
				}
				switch strings.ToLower(stringField(part, "type")) {
				case "input_text", "text", "output_text":
					turn.Parts = append(turn.Parts, Part{Kind: PartText, Text: stringField(part, "text")})
				case "input_image", "image_url":
					url := firstStringField(part, "image_url", "url")
					if len(url) > 0 && url[0] == '{' {
						var img map[string]json.RawMessage
						if json.Unmarshal([]byte(url), &img) == nil {
							url = stringField(img, "url")
						}
					}
					turn.Parts = append(turn.Parts, Part{Kind: PartImage, ImageURL: url})
				}
			}
			if len(turn.Parts) == 0 {
				turn.Parts = []Part{{Kind: PartText, Text: ""}}
			}
			req.Turns = append(req.Turns, turn)
		case "function_call", "tool_call":
			flushToolResults()
			// A call replayed by the client belongs to the assistant turn it
			// was made in; a new one opens a turn.
			if n := len(req.Turns); n > 0 && req.Turns[n-1].Role == RoleAssistant {
				req.Turns[n-1].Parts = append(req.Turns[n-1].Parts, Part{
					Kind:       PartToolCall,
					ToolCallID: firstStringField(item, "call_id", "id"),
					ToolName:   stringField(item, "name"),
					Text:       firstStringField(item, "arguments", "input"),
				})
				continue
			}
			req.Turns = append(req.Turns, Turn{
				Role: RoleAssistant,
				Parts: []Part{{
					Kind:       PartToolCall,
					ToolCallID: firstStringField(item, "call_id", "id"),
					ToolName:   stringField(item, "name"),
					Text:       firstStringField(item, "arguments", "input"),
				}},
			})
		case "function_call_output", "tool_result":
			flushToolResults()
			pendingToolResults = append(pendingToolResults, Part{
				Kind:       PartToolResult,
				ToolCallID: firstStringField(item, "call_id", "id", "tool_use_id"),
				Text:       responsesToolOutput(item["output"]),
			})
		case "reasoning":
			flushToolResults()
			// Replayed reasoning keeps its summary text as a thinking part so
			// an Anthropic target can replay it.
			if think := responsesReasoningText(item); think != "" {
				if n := len(req.Turns); n > 0 && req.Turns[n-1].Role == RoleAssistant {
					req.Turns[n-1].Parts = append(req.Turns[n-1].Parts, Part{Kind: PartThinking, Text: think})
					continue
				}
				req.Turns = append(req.Turns, Turn{Role: RoleAssistant, Parts: []Part{{Kind: PartThinking, Text: think}}})
			}
		}
	}
	flushToolResults()
	return nil
}

func responsesToolOutput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(firstStringField(p, "text", "output"))
		}
		return b.String()
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		return firstStringField(obj, "content", "text")
	}
	return string(raw)
}

func responsesReasoningText(item map[string]json.RawMessage) string {
	var b strings.Builder
	var summaries []map[string]json.RawMessage
	if raw, ok := item["summary"]; ok {
		_ = json.Unmarshal(raw, &summaries)
	}
	for _, s := range summaries {
		b.WriteString(stringField(s, "text"))
	}
	// Some responses carry content blocks as well as a summary.
	if raw, ok := item["content"]; ok {
		var contents []map[string]json.RawMessage
		if json.Unmarshal(raw, &contents) == nil {
			for _, c := range contents {
				b.WriteString(firstStringField(c, "text", "thinking"))
			}
		}
	}
	return b.String()
}

func decodeResponsesTools(obj map[string]json.RawMessage, req *Request) error {
	raw, ok := obj["tools"]
	if !ok {
		return nil
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return ErrMalformed("tools")
	}
	for _, t := range tools {
		switch strings.ToLower(stringField(t, "type")) {
		case "function", "":
			name := stringField(t, "name")
			schema := t["parameters"]
			description := stringField(t, "description")
			// A chat-shaped tool in a Responses body wraps its function.
			if fn, ok := t["function"]; ok && name == "" {
				var f map[string]json.RawMessage
				if json.Unmarshal(fn, &f) == nil {
					name = stringField(f, "name")
					schema = f["parameters"]
					description = stringField(f, "description")
				}
			}
			if name == "" {
				continue
			}
			req.Tools = append(req.Tools, Tool{Name: name, Description: description, Schema: schema})
		default:
			// A built-in tool (web_search, file_search, …) has no function
			// schema this plugin can hand a chat-completions target, and
			// forwarding it unchanged is a 400 at the upstream.
			continue
		}
	}
	if raw, ok := obj["tool_choice"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "auto", "required", "none":
				req.Choice = ToolChoice{Kind: strings.ToLower(strings.TrimSpace(s))}
			}
		} else {
			var named map[string]json.RawMessage
			if json.Unmarshal(raw, &named) == nil {
				switch strings.ToLower(stringField(named, "type")) {
				case "function":
					req.Choice = ToolChoice{Kind: "tool", Name: stringField(named, "name")}
				case "auto":
					req.Choice = ToolChoice{Kind: "auto"}
				case "required":
					req.Choice = ToolChoice{Kind: "required"}
				case "none":
					req.Choice = ToolChoice{Kind: "none"}
				}
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// request: IR -> responses
// ---------------------------------------------------------------------------

// EncodeResponsesRequest renders the canonical request as a Responses body.
func EncodeResponsesRequest(req *Request, model string, stream bool) ([]byte, error) {
	out := map[string]any{
		"model":  model,
		"stream": stream,
	}
	if sys := req.SystemText(); sys != "" {
		out["instructions"] = sys
	}
	items := make([]any, 0, len(req.Turns))
	for _, turn := range req.Turns {
		switch turn.Role {
		case RoleTool:
			for _, p := range turn.Parts {
				if p.Kind != PartToolResult {
					continue
				}
				items = append(items, map[string]any{
					"type":    "function_call_output",
					"call_id": p.ToolCallID,
					"output":  p.Text,
				})
			}
		case RoleAssistant:
			var text, thinking strings.Builder
			for _, p := range turn.Parts {
				switch p.Kind {
				case PartText:
					text.WriteString(p.Text)
				case PartThinking:
					thinking.WriteString(p.Text)
				}
			}
			if thinking.Len() > 0 {
				items = append(items, map[string]any{
					"type": "reasoning",
					"summary": []any{map[string]any{
						"type": "summary_text",
						"text": thinking.String(),
					}},
				})
			}
			if text.Len() > 0 {
				items = append(items, map[string]any{
					"type": "message",
					"role": "assistant",
					"content": []any{map[string]any{
						"type": "output_text",
						"text": text.String(),
					}},
				})
			}
			for _, p := range turn.Parts {
				if p.Kind != PartToolCall {
					continue
				}
				items = append(items, map[string]any{
					"type":      "function_call",
					"call_id":   p.ToolCallID,
					"name":      p.ToolName,
					"arguments": argsOrEmptyObject(p.Text),
				})
			}
		default:
			var content []any
			for _, p := range turn.Parts {
				switch p.Kind {
				case PartText:
					content = append(content, map[string]any{"type": "input_text", "text": p.Text})
				case PartImage:
					content = append(content, map[string]any{"type": "input_image", "image_url": p.ImageURL})
				case PartToolResult:
					content = append(content, map[string]any{"type": "input_text", "text": p.Text})
				}
			}
			role := "user"
			if turn.Role == RoleSystem {
				role = "system"
			}
			if len(content) == 0 {
				content = append(content, map[string]any{"type": "input_text", "text": ""})
			}
			items = append(items, map[string]any{
				"type":    "message",
				"role":    role,
				"content": content,
			})
		}
	}
	if len(items) == 0 {
		items = append(items, map[string]any{
			"type":    "message",
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": ""}},
		})
	}
	out["input"] = items
	if req.MaxOutput > 0 {
		out["max_output_tokens"] = req.MaxOutput
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tool := map[string]any{"type": "function", "name": t.Name}
			if t.Description != "" {
				tool["description"] = t.Description
			}
			tool["parameters"] = schemaOrDefault(t.Schema)
			tools = append(tools, tool)
		}
		out["tools"] = tools
		if choice := encodeResponsesToolChoice(req.Choice); choice != nil {
			out["tool_choice"] = choice
		}
	}
	return json.Marshal(out)
}

// EncodeResponsesReasoning writes the request's reasoning block for a model
// whose declared levels are `levels`. A model with no levels gets no block:
// asking it for a level is what GPT-6 Luna does and ignores.
func EncodeResponsesReasoning(body []byte, effort string, levels []string) ([]byte, error) {
	if strings.TrimSpace(effort) == "" || len(levels) == 0 {
		return body, nil
	}
	obj, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	obj["reasoning"] = json.RawMessage(fmt.Sprintf(`{"effort":%q}`, effort))
	return json.Marshal(obj)
}

func encodeResponsesToolChoice(choice ToolChoice) any {
	switch choice.Kind {
	case "auto", "required", "none":
		return choice.Kind
	case "tool":
		return map[string]any{"type": "function", "name": choice.Name}
	}
	if choice.Name != "" {
		return map[string]any{"type": "function", "name": choice.Name}
	}
	return nil
}

// ---------------------------------------------------------------------------
// response: responses -> IR
// ---------------------------------------------------------------------------

// DecodeResponsesResponse decodes a non-streaming Responses body.
//
// Like /messages, amr-link frames a Responses answer as SSE even when the
// request was not streaming, so a framed body is reassembled from its events.
func DecodeResponsesResponse(body []byte) (*Response, error) {
	if looksLikeSSE(body) {
		return decodeResponsesEventStream(body)
	}
	obj, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	resp := &Response{
		ID:        stringField(obj, "id"),
		Model:     stringField(obj, "model"),
		CreatedAt: intField(obj, "created_at"),
	}
	if errObj, ok := obj["error"]; ok && len(errObj) > 0 && string(errObj) != "null" {
		return nil, errUpstreamBody(0, body)
	}
	var output []json.RawMessage
	if raw, ok := obj["output"]; ok {
		_ = json.Unmarshal(raw, &output)
	}
	for _, rawItem := range output {
		var item map[string]json.RawMessage
		if json.Unmarshal(rawItem, &item) != nil {
			continue
		}
		switch strings.ToLower(stringField(item, "type")) {
		case "reasoning":
			if think := responsesReasoningText(item); think != "" {
				resp.Parts = append(resp.Parts, Part{Kind: PartThinking, Text: think})
			}
		case "message":
			text, parts, err := stringOrParts(item["content"])
			if err != nil {
				continue
			}
			if text != "" {
				resp.Parts = append(resp.Parts, Part{Kind: PartText, Text: text})
			}
			for _, p := range parts {
				var part map[string]json.RawMessage
				if json.Unmarshal(p, &part) != nil {
					continue
				}
				switch strings.ToLower(stringField(part, "type")) {
				case "output_text", "text", "input_text":
					resp.Parts = append(resp.Parts, Part{Kind: PartText, Text: stringField(part, "text")})
				case "refusal":
					resp.Parts = append(resp.Parts, Part{Kind: PartText, Text: stringField(part, "refusal")})
					resp.StopReason = StopFiltered
				}
			}
		case "function_call":
			resp.Parts = append(resp.Parts, Part{
				Kind:       PartToolCall,
				ToolCallID: firstStringField(item, "call_id", "id"),
				ToolName:   stringField(item, "name"),
				Text:       firstStringField(item, "arguments", "input"),
			})
		}
	}
	if status := strings.ToLower(stringField(obj, "status")); status == "incomplete" {
		if details, ok := obj["incomplete_details"]; ok {
			var d map[string]json.RawMessage
			if json.Unmarshal(details, &d) == nil {
				if reason := stringField(d, "reason"); reason != "" {
					resp.StopReason = normalizeStop(reason)
				}
			}
		}
	}
	if raw, ok := obj["usage"]; ok {
		var u map[string]json.RawMessage
		if json.Unmarshal(raw, &u) == nil {
			resp.Usage = decodeResponsesUsage(u)
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

func decodeResponsesUsage(u map[string]json.RawMessage) Usage {
	usage := Usage{
		Input:  firstIntField(u, "input_tokens", "prompt_tokens"),
		Output: firstIntField(u, "output_tokens", "completion_tokens"),
	}
	if raw, ok := u["input_tokens_details"]; ok {
		var d map[string]json.RawMessage
		if json.Unmarshal(raw, &d) == nil {
			usage.CacheRead = firstIntField(d, "cached_tokens")
		}
	}
	if raw, ok := u["output_tokens_details"]; ok {
		var d map[string]json.RawMessage
		if json.Unmarshal(raw, &d) == nil {
			usage.Reasoning = firstIntField(d, "reasoning_tokens")
		}
	}
	return usage
}

// decodeResponsesEventStream reassembles a Responses event sequence.
func decodeResponsesEventStream(body []byte) (*Response, error) {
	resp := &Response{}
	var textBuf, thinkBuf strings.Builder
	type callState struct {
		index int
		id    string
		name  string
		args  strings.Builder
	}
	calls := map[string]*callState{}
	var order []string
	for _, payload := range sseDataPayloads(body) {
		ev, err := decodeObject([]byte(payload))
		if err != nil {
			continue
		}
		kind := strings.ToLower(stringField(ev, "type"))
		switch kind {
		case "response.created", "response.in_progress":
			if r, ok := ev["response"]; ok {
				var rm map[string]json.RawMessage
				if json.Unmarshal(r, &rm) == nil {
					resp.ID = stringField(rm, "id")
					resp.Model = stringField(rm, "model")
					resp.CreatedAt = intField(rm, "created_at")
				}
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			thinkBuf.WriteString(firstStringField(ev, "delta", "text"))
		case "response.reasoning_summary_text.done", "response.reasoning_text.done":
			// The done event repeats the whole text; the deltas already carried
			// it, so writing it again would double the thought.
		case "response.output_text.delta":
			textBuf.WriteString(firstStringField(ev, "delta", "text"))
		case "response.output_item.added":
			if item, ok := ev["item"]; ok {
				var im map[string]json.RawMessage
				if json.Unmarshal(item, &im) == nil && strings.ToLower(stringField(im, "type")) == "function_call" {
					id := firstStringField(im, "call_id", "id")
					if id == "" {
						id = firstStringField(im, "id")
					}
					if _, seen := calls[id]; !seen {
						order = append(order, id)
					}
					calls[id] = &callState{
						index: int(intField(ev, "output_index")),
						id:    id,
						name:  stringField(im, "name"),
					}
				}
			}
		case "response.function_call_arguments.delta":
			id := firstStringField(ev, "call_id", "item_id")
			state, ok := calls[id]
			if !ok {
				// A gateway that never sent output_item.added: derive the call
				// from the argument delta so its arguments are not dropped.
				state = &callState{index: int(firstIntField(ev, "output_index"))}
				state.id = id
				order = append(order, id)
				calls[id] = state
			}
			state.args.WriteString(stringField(ev, "delta"))
		case "response.output_item.done":
			if item, ok := ev["item"]; ok {
				var im map[string]json.RawMessage
				if json.Unmarshal(item, &im) == nil {
					switch strings.ToLower(stringField(im, "type")) {
					case "function_call":
						id := firstStringField(im, "call_id", "id")
						state, ok := calls[id]
						if !ok {
							state = &callState{id: id, name: stringField(im, "name")}
							order = append(order, id)
							calls[id] = state
						}
						if name := stringField(im, "name"); name != "" {
							state.name = name
						}
						if args := firstStringField(im, "arguments"); args != "" && state.args.Len() == 0 {
							state.args.WriteString(args)
						}
					}
				}
			}
		case "response.completed", "response.incomplete", "response.failed":
			if r, ok := ev["response"]; ok {
				var rm map[string]json.RawMessage
				if json.Unmarshal(r, &rm) == nil {
					if id := stringField(rm, "id"); id != "" {
						resp.ID = id
					}
					if model := stringField(rm, "model"); model != "" {
						resp.Model = model
					}
					if u, ok := rm["usage"]; ok {
						var um map[string]json.RawMessage
						if json.Unmarshal(u, &um) == nil {
							resp.Usage = decodeResponsesUsage(um)
						}
					}
					if kind == "response.incomplete" {
						if details, ok := rm["incomplete_details"]; ok {
							var d map[string]json.RawMessage
							if json.Unmarshal(details, &d) == nil {
								resp.StopReason = normalizeStop(stringField(d, "reason"))
							}
						}
					}
					if kind == "response.failed" {
						if e, ok := rm["error"]; ok {
							var em map[string]json.RawMessage
							if json.Unmarshal(e, &em) == nil {
								if msg := stringField(em, "message"); msg != "" {
									return nil, fmt.Errorf("upstream answered an error: %s", truncate(msg, 300))
								}
							}
						}
					}
				}
			}
		case "error":
			msg := firstStringField(ev, "message")
			if msg == "" {
				if e, ok := ev["error"]; ok {
					var em map[string]json.RawMessage
					if json.Unmarshal(e, &em) == nil {
						msg = stringField(em, "message")
					}
				}
			}
			if msg != "" {
				return nil, fmt.Errorf("upstream answered an error: %s", truncate(msg, 300))
			}
		}
	}
	if think := thinkBuf.String(); think != "" {
		resp.Parts = append(resp.Parts, Part{Kind: PartThinking, Text: think})
	}
	if text := textBuf.String(); text != "" {
		resp.Parts = append(resp.Parts, Part{Kind: PartText, Text: text})
	}
	for _, id := range order {
		state := calls[id]
		if state == nil {
			continue
		}
		resp.Parts = append(resp.Parts, Part{
			Kind:       PartToolCall,
			ToolCallID: state.id,
			ToolName:   state.name,
			Text:       state.args.String(),
		})
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

// ---------------------------------------------------------------------------
// response: IR -> responses
// ---------------------------------------------------------------------------

// EncodeResponsesResponse renders a canonical response as a Responses body.
func EncodeResponsesResponse(resp *Response, model string) ([]byte, error) {
	var output []any
	if think := resp.Thinking(); think != "" {
		summary := []any{map[string]any{"type": "summary_text", "text": think}}
		output = append(output, map[string]any{
			"type":              "reasoning",
			"id":                reasoningID(resp.ID),
			"summary":           summary,
			"encrypted_content": "",
		})
	}
	answer := resp.Text()
	calls := resp.ToolCalls()
	if answer != "" || len(calls) == 0 {
		output = append(output, map[string]any{
			"type":    "message",
			"id":      messageID(resp.ID),
			"role":    "assistant",
			"status":  "completed",
			"content": []any{map[string]any{"type": "output_text", "text": answer, "annotations": []any{}}},
		})
	}
	for i, c := range calls {
		id := c.ToolCallID
		if id == "" {
			id = fmt.Sprintf("call_opendesign_%d", i)
		}
		output = append(output, map[string]any{
			"type":      "function_call",
			"id":        fmt.Sprintf("fc_%s_%d", nonEmpty(resp.ID, "resp"), i),
			"call_id":   id,
			"name":      c.ToolName,
			"arguments": argsOrEmptyObject(c.Text),
			"status":    "completed",
		})
	}
	created := resp.CreatedAt
	if created <= 0 {
		created = 1
	}
	out := map[string]any{
		"id":                  nonEmpty(resp.ID, "resp_opendesign"),
		"object":              "response",
		"created_at":          created,
		"status":              responsesStatus(resp.StopReason),
		"model":               model,
		"output":              output,
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
	}
	if resp.StopReason == StopLength {
		out["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	prompt, completion, total := resp.Usage.Resolved()
	usage := map[string]any{
		"input_tokens":  prompt,
		"output_tokens": completion,
		"total_tokens":  total,
	}
	if resp.Usage.CacheRead > 0 {
		usage["input_tokens_details"] = map[string]any{"cached_tokens": resp.Usage.CacheRead}
	}
	if resp.Usage.Reasoning > 0 {
		usage["output_tokens_details"] = map[string]any{"reasoning_tokens": resp.Usage.Reasoning}
	}
	out["usage"] = usage
	return json.Marshal(out)
}

func responsesStatus(stop string) string {
	if stop == StopLength {
		return "incomplete"
	}
	return "completed"
}

func reasoningID(id string) string {
	return "rs_" + nonEmpty(id, "opendesign")
}

func messageID(id string) string {
	return "msg_" + nonEmpty(id, "opendesign")
}
