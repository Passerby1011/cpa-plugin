// body.go constructs the QoderWork agent_chat_generation request body from
// OpenAI-style chat completion inputs.
//
// The base template lives in baseprompt.json (embedded). Per-request we
// overwrite request/session ids, timestamps, model key, and the user prompt.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

//go:embed baseprompt.json
var basepromptJSON []byte

// cpaToUpstreamKey maps CPA-facing model names to upstream keys.
//
// The first spelling of each case is the display name clients copy from the
// Qoder model picker; the rest are wire keys and legacy names that still arrive
// from pinned configs. Anything unmapped passes through unchanged — the gateway
// silently routes an unknown key to `auto`, which answers with a different model
// than the caller asked for, so every name we advertise must be listed here.
func cpaToUpstreamKey(cpaModel string) string {
	switch cpaModel {
	// Tier ids are their own upstream keys.
	case "qoder-auto", "auto", "ultimate", "performance", "efficient", "lite":
		return strings.TrimPrefix(cpaModel, "qoder-")
	// Qwen
	case "qwen3.8-max", "qwen-3.8-max", "qmodel_38max":
		return "qmodel_38max"
	case "qwen3.8-max-preview", "qmodel_preview":
		return "qmodel_38max"
	case "qwen3.8-flash", "qwen-3.8-flash", "qfmodel":
		return "qfmodel"
	case "qwen3.7-max", "qwen-3.7-max", "qmodel_latest":
		return "qmodel_latest"
	case "qwen3.7-plus", "qwen-3.7-plus", "qwen3.6-plus", "qmodel":
		return "qmodel"
	case "qwen3.7-flash", "qwen-3.7-flash", "q37fmodel":
		return "q37fmodel"
	case "qwen3.6-flash", "q36fmodel":
		return "q37fmodel"
	// DeepSeek
	case "deepseek-v4-pro", "dmodel":
		return "dmodel"
	case "deepseek-v4.1-flash", "deepseek-v4-1-flash", "deepseek-v4-flash", "deepseek-flash", "dfmodel":
		return "dfmodel"
	// GLM
	case "glm-5.3", "gm53model", "gmodel":
		return "gmodel"
	case "glm-5.3-flash", "gfmodel":
		return "gfmodel"
	case "glm-5.2", "glm-5.1", "gm51model":
		return "gm51model"
	// Kimi
	case "kimi-k3", "kmodel_latest":
		return "kmodel_latest"
	case "kimi-k2.8-preview", "kimi-k2.8", "kimi-k2.7-code", "kimi-k2.6", "kmodel":
		return "kmodel"
	// MiniMax
	case "minimax-m3", "minimax-m2.7", "mmodel":
		return "mmodel"
	}
	return cpaModel
}

// openAIMessage is one message in the OpenAI chat completion format.
//
// Content is json.RawMessage (not string) so multi-modal messages whose content
// is a part array decode instead of failing the whole request with
// "cannot unmarshal array into Go struct field ... of type string".
// ToolCalls / ToolCallID are kept so assistant tool-call turns and their
// matching tool results survive the round trip: the upstream rejects a tool
// message without tool_call_id ("missing field `tool_call_id`").
type openAIMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// openAIRequest is the CPA-facing chat completion request.
type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Tools    json.RawMessage `json:"tools,omitempty"`
}

// contentText renders a message content for use as a plain prompt string.
// Handles both the string form and the part-array form; returns "" when the
// content carries no text.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// extractLatestUserPrompt returns the text of the last user message.
func extractLatestUserPrompt(messages []openAIMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			if text := contentText(messages[i].Content); text != "" {
				return text
			}
		}
	}
	return ""
}

// buildQoderBody renders the upstream agent_chat_generation body for one request.
// modelKey is the upstream key (already mapped via cpaToUpstreamKey).
func buildQoderBody(req *openAIRequest, modelKey, userType string) ([]byte, error) {
	var base map[string]any
	if err := json.Unmarshal(basepromptJSON, &base); err != nil {
		return nil, fmt.Errorf("baseprompt decode: %w", err)
	}

	prompt := extractLatestUserPrompt(req.Messages)
	if prompt == "" {
		return nil, fmt.Errorf("no user message in request")
	}

	nid := uuid.NewString()
	base["request_id"] = nid
	base["chat_record_id"] = nid
	base["request_set_id"] = uuid.NewString()
	base["session_id"] = uuid.NewString()
	base["stream"] = true
	base["aliyun_user_type"] = userType
	base["agent_id"] = "agent_common"
	// The template's session timestamp is a placeholder: each request is its own
	// session, so it carries the moment this one started.
	if biz, ok := base["business"].(map[string]any); ok {
		biz["begin_at"] = time.Now().UnixMilli()
	}

	// model_config
	if mc, ok := base["model_config"].(map[string]any); ok {
		mc["key"] = modelKey
	}

	// chat_context.text.text + chat_context.extra.originalContent.text
	if cc, ok := base["chat_context"].(map[string]any); ok {
		if txt, ok := cc["text"].(map[string]any); ok {
			txt["text"] = prompt
		}
		if extra, ok := cc["extra"].(map[string]any); ok {
			if oc, ok := extra["originalContent"].(map[string]any); ok {
				oc["text"] = prompt
			}
			if mc, ok := extra["modelConfig"].(map[string]any); ok {
				mc["key"] = modelKey
			}
		}
	}

	// messages: keep system prompt from baseprompt (template has it), replace user/assistant
	var systemMsgs []any
	if msgs, ok := base["messages"].([]any); ok {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				if role, _ := mm["role"].(string); role == "system" {
					systemMsgs = append(systemMsgs, m)
				}
			}
		}
	}
	// Append the actual conversation.
	//
	// Every field is forwarded verbatim (not just role+content): the upstream
	// parses the full OpenAI message shape, and dropping tool_calls /
	// tool_call_id makes it reject assistant tool-call turns and their tool
	// results. Content stays as-is so part arrays (images) survive.
	for _, m := range req.Messages {
		out := map[string]any{"role": m.Role}
		// The upstream rejects an assistant tool-call turn whose content is
		// JSON null ("Messages with role 'tool' must be a response to a
		// preceding message with 'tool_calls'"), so normalize null/absent
		// content to an empty string. Non-null content (string or part array)
		// is forwarded verbatim.
		if len(m.Content) == 0 || string(m.Content) == "null" {
			out["content"] = ""
		} else {
			out["content"] = m.Content
		}
		if m.Name != "" {
			out["name"] = m.Name
		}
		if len(m.ToolCalls) > 0 {
			out["tool_calls"] = m.ToolCalls
		}
		if m.ToolCallID != "" {
			out["tool_call_id"] = m.ToolCallID
		}
		systemMsgs = append(systemMsgs, out)
	}
	base["messages"] = systemMsgs

	// Forward the caller's tool definitions so the upstream can actually call
	// them; the template's own tool list stays as the fallback when the
	// request carries none.
	if len(req.Tools) > 0 {
		base["tools"] = req.Tools
	}

	// business
	if biz, ok := base["business"].(map[string]any); ok {
		biz["id"] = uuid.NewString()
		biz["begin_at"] = time.Now().UnixMilli()
		if len(prompt) > 30 {
			biz["name"] = prompt[:30]
		} else {
			biz["name"] = prompt
		}
	}

	return json.Marshal(base)
}
