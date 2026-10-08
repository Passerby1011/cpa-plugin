// Package gocli implements the CommandCode CLI transport (/alpha/generate).
//
// Why this exists: CommandCode's documented Provider API (/provider/v1/*)
// refuses a Go-plan credential with "upgrade_required" — Go is the one plan
// without Provider API access. The vendor's own CLI does not use that surface;
// it posts a custom envelope to /alpha/generate, and that endpoint is not
// plan-gated. Serving a Go-plan subscription therefore means speaking the CLI
// protocol and normalizing its stream back into the shapes CLIProxyAPI clients
// already understand.
//
// The wire shape here was derived from two independent open-source
// implementations of the same protocol (Mars-Sea/dsh-commandcode-provider and
// MAXeaglet/commandcode-proxy), which agree on the envelope, the SSE event
// names, and the reasoning-replay requirement.
package gocli

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Options carries the per-request envelope fields that are not derived from
// the client payload.
type Options struct {
	// Version is sent as x-command-code-version. It must describe the protocol
	// shape this code actually implements: claiming a newer version while
	// emitting an older dialect is a stronger tell than an old version number.
	Version string
	// Mode is the envelope "mode". "agent" is the ordinary agent-loop turn.
	Mode string
	// WorkingDir is a fabricated project directory. The real client sends its
	// cwd; sending a constant keeps one account's requests looking like one
	// machine rather than leaking the host's paths upstream.
	WorkingDir string
	// Environment is the fabricated platform string. The reference sends the
	// device profile's platform verbatim ("win32"), not a "linux-x64" style
	// pair, so the envelope cannot contradict the fingerprint the same
	// credential reports.
	Environment string
	// ThreadID must be a well-formed UUID or the field is omitted.
	ThreadID string
	// SystemPlaceholder, when the client sent no system prompt, substitutes a
	// single space. Without it the upstream injects its own multi-thousand
	// token default prompt, which both costs tokens and pollutes the
	// conversation. Matches the reference implementations' default.
	SystemPlaceholder bool
	// DefaultTemperature is used when the client did not set one.
	DefaultTemperature float64
}

// DefaultVersion is the protocol shape this package implements.
const DefaultVersion = "1.53.1"

// legacyWorkingDir / legacyEnvironment are the envelope defaults used when the
// caller supplies no device (the identity layer is off). They are the values
// this plugin sent before the device layer existed, kept separate from
// DefaultProjectDir so "identity off" is a real fallback rather than a silent
// re-introduction of the fabricated machine.
const (
	legacyWorkingDir  = `C:\Users\dev\projects\app`
	legacyEnvironment = "linux-x64"
)

// BuildEnvelope converts an OpenAI chat-completions body (the shape our other
// adapters already produce) into the /alpha/generate envelope.
//
// Going through the OpenAI shape keeps this package free of per-client-format
// parsing: callers translate their own protocol into chat-completions first.
func BuildEnvelope(openAIBody []byte, opts Options) ([]byte, error) {
	if !gjson.ValidBytes(openAIBody) {
		return nil, fmt.Errorf("invalid request body")
	}
	model := strings.TrimSpace(gjson.GetBytes(openAIBody, "model").String())
	if model == "" {
		return nil, fmt.Errorf("request has no model")
	}

	messages, err := convertMessages(gjson.GetBytes(openAIBody, "messages").Array())
	if err != nil {
		return nil, err
	}

	params := map[string]any{
		"model":    model,
		"messages": messages,
		"stream":   true, // /alpha/generate is always a stream
	}

	// The CLI transport carries tools in a flattened {type,name,input_schema}
	// shape, not OpenAI's nested {type,function:{...}}.
	if tools := gjson.GetBytes(openAIBody, "tools"); tools.IsArray() && len(tools.Array()) > 0 {
		params["tools"] = convertTools(tools.Array())
	}

	// system: the CLI sends an array of sections with a cache marker. An absent
	// system prompt becomes a single space so upstream does not inject its own.
	systemText := systemFromMessages(openAIBody)
	if systemText == "" && opts.SystemPlaceholder {
		systemText = " "
	}
	if systemText != "" {
		params["system"] = []map[string]any{{
			"type":          "text",
			"text":          systemText,
			"cache_control": map[string]any{"type": "ephemeral"},
		}}
	}

	if mt := gjson.GetBytes(openAIBody, "max_tokens"); mt.Exists() {
		params["max_tokens"] = mt.Int()
	}
	temp := opts.DefaultTemperature
	if t := gjson.GetBytes(openAIBody, "temperature"); t.Exists() {
		temp = t.Float()
	}
	params["temperature"] = temp
	if effort := strings.TrimSpace(gjson.GetBytes(openAIBody, "reasoning_effort").String()); effort != "" {
		params["reasoning_effort"] = effort
	}

	mode := opts.Mode
	if mode == "" {
		mode = "agent"
	}
	envelope := map[string]any{
		"config": map[string]any{
			"workingDir":    orDefault(opts.WorkingDir, legacyWorkingDir),
			"date":          time.Now().Format("2006-01-02"),
			"environment":   orDefault(opts.Environment, legacyEnvironment),
			"structure":     []any{},
			"isGitRepo":     false,
			"currentBranch": "",
			"mainBranch":    "",
			"gitStatus":     "",
			"recentCommits": []any{},
		},
		"memory":         nil,
		"taste":          nil,
		"skills":         nil,
		"permissionMode": "standard",
		"mode":           mode,
		"params":         params,
	}
	// threadId must be a valid UUID; the reference implementations omit the
	// field entirely rather than sending a malformed one.
	if isUUID(opts.ThreadID) {
		envelope["threadId"] = opts.ThreadID
	}
	return json.Marshal(envelope)
}

// convertMessages maps OpenAI messages to the CLI's content-part shape.
//
// Historical reasoning is replayed as a {type:"reasoning"} part on the
// assistant message. That is not cosmetic: the upstream rejects a DeepSeek
// thinking-mode tool loop whose assistant tool calls arrive without their
// preceding reasoning, so dropping these parts breaks multi-turn tool use.
func convertMessages(raw []gjson.Result) ([]any, error) {
	out := make([]any, 0, len(raw))

	// toolCallId → toolName, harvested from every assistant tool_call first.
	//
	// A "tool" message only carries the call id; the upstream requires the
	// matching tool NAME on its tool-result block and rejects the whole request
	// with 400 when it is missing ("expected string, received undefined"). The
	// name lives on the earlier assistant message, so it has to be resolved
	// with a prepass: a single forward pass cannot see it yet.
	//
	// This is why a plain chat works while anything that uses tools fails -
	// which is every real agent client (Claude Code, Codex).
	toolNames := make(map[string]string)
	for _, m := range raw {
		if strings.TrimSpace(m.Get("role").String()) != "assistant" {
			continue
		}
		for _, tc := range m.Get("tool_calls").Array() {
			id := strings.TrimSpace(tc.Get("id").String())
			if id == "" {
				continue
			}
			toolNames[id] = tc.Get("function.name").String()
		}
	}

	for _, m := range raw {
		role := strings.TrimSpace(m.Get("role").String())
		switch role {
		case "system":
			// Lifted into params.system by the caller.
			continue
		case "user":
			out = append(out, map[string]any{
				"role":    "user",
				"content": userParts(m),
			})
		case "assistant":
			parts := assistantParts(m)
			if len(parts) == 0 {
				continue
			}
			out = append(out, map[string]any{"role": "assistant", "content": parts})
		case "tool":
			callID := m.Get("tool_call_id").String()
			// Fall back to the message's own name, then to "" - the field must
			// be a string, so a nil/absent value is not an option.
			name := toolNames[callID]
			if name == "" {
				name = m.Get("name").String()
			}
			out = append(out, map[string]any{
				"role": "tool",
				"content": []map[string]any{{
					"type":       "tool-result",
					"toolCallId": callID,
					"toolName":   name,
					"output":     map[string]any{"type": "text", "value": textOf(m.Get("content"))},
				}},
			})
		}
	}
	return out, nil
}

func userParts(m gjson.Result) []any {
	c := m.Get("content")
	if c.Type == gjson.String {
		return []any{map[string]any{"type": "text", "text": c.String()}}
	}
	parts := make([]any, 0)
	for _, p := range c.Array() {
		switch p.Get("type").String() {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": p.Get("text").String()})
		case "image_url":
			url := p.Get("image_url.url").String()
			mime := "image/png"
			if i := strings.Index(url, ":"); i > 5 {
				if j := strings.Index(url[i:], ";"); j > 0 {
					mime = url[5 : i+j]
				}
			}
			parts = append(parts, map[string]any{"type": "image", "image": url, "mimeType": mime})
		}
	}
	if len(parts) == 0 {
		parts = append(parts, map[string]any{"type": "text", "text": textOf(c)})
	}
	return parts
}

func assistantParts(m gjson.Result) []any {
	parts := make([]any, 0)
	if rc := strings.TrimSpace(m.Get("reasoning_content").String()); rc != "" {
		parts = append(parts, map[string]any{"type": "reasoning", "text": rc})
	}
	if txt := textOf(m.Get("content")); strings.TrimSpace(txt) != "" {
		parts = append(parts, map[string]any{"type": "text", "text": txt})
	}
	for _, tc := range m.Get("tool_calls").Array() {
		input := map[string]any{}
		if raw := tc.Get("function.arguments").String(); raw != "" {
			_ = json.Unmarshal([]byte(raw), &input)
		}
		parts = append(parts, map[string]any{
			"type":       "tool-call",
			"toolCallId": tc.Get("id").String(),
			"toolName":   tc.Get("function.name").String(),
			"input":      input,
		})
	}
	return parts
}

func convertTools(raw []gjson.Result) []any {
	out := make([]any, 0, len(raw))
	for _, t := range raw {
		name := strings.TrimSpace(t.Get("function.name").String())
		if name == "" {
			continue
		}
		schema := t.Get("function.parameters")
		var params any = map[string]any{"type": "object", "properties": map[string]any{}}
		if schema.Exists() {
			_ = json.Unmarshal([]byte(schema.Raw), &params)
		}
		out = append(out, map[string]any{
			"type":         "function",
			"name":         name,
			"description":  t.Get("function.description").String(),
			"input_schema": params,
		})
	}
	return out
}

// systemFromMessages folds any system/developer messages into one string.
func systemFromMessages(body []byte) string {
	var parts []string
	for _, m := range gjson.GetBytes(body, "messages").Array() {
		role := m.Get("role").String()
		if role != "system" && role != "developer" {
			continue
		}
		if s := strings.TrimSpace(textOf(m.Get("content"))); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}

// textOf reads a content field that may be a string or an array of parts.
func textOf(v gjson.Result) string {
	if v.Type == gjson.String {
		return v.String()
	}
	var b strings.Builder
	for _, p := range v.Array() {
		if t := p.Get("text").String(); t != "" {
			b.WriteString(t)
		}
	}
	return b.String()
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// isUUID reports whether s is a well-formed RFC 4122 UUID.
func isUUID(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return false
			}
		}
	}
	return true
}

// sha1Of returns the SHA-1 digest of s. SHA-1 is used here purely as a stable
// name derivation for a thread id, never as a security primitive.
func sha1Of(s string) [20]byte {
	return sha1.Sum([]byte(s))
}

// NormalizeThreadID returns a UUID-shaped thread id derived from an arbitrary
// session key, so the same client session maps to the same upstream thread.
// A value that is already a UUID is returned unchanged.
func NormalizeThreadID(sessionKey string) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return ""
	}
	if isUUID(sessionKey) {
		return sessionKey
	}
	// Deterministic UUIDv5-shaped id: the shape is what the upstream accepts;
	// the derivation only has to be stable per session.
	sum := sha1Of("commandcode-gocli-thread/" + sessionKey)
	hexs := fmt.Sprintf("%x", sum)
	return hexs[0:8] + "-" + hexs[8:12] + "-5" + hexs[13:16] + "-" + hexs[16:20] + "-" + hexs[20:32]
}
