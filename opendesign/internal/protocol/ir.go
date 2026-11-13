// Package protocol holds this plugin's protocol kernel: one canonical form
// (the IR below) that every inbound schema is decoded into and every outbound
// schema is encoded from.
//
// # WHY AN IR AND NOT PAIRWISE TRANSLATORS
//
// amr-link serves every model on all three of its APIs (chat completions,
// Anthropic messages, Responses) but a model only THINKS on the one it is
// declared on, so the plugin must be able to take a request in ANY client
// schema and send it in the target model's own schema. Nine pairwise
// translations is nine places for thinking text to be dropped; one IR with a
// decoder and an encoder per schema is three decoders, three encoders, and a
// single definition of what a thought is.
//
// # THINKING IS THE POINT
//
// The three schemas carry a model's reasoning in three different places:
//
//	chat completions   message.reasoning_content (plus reasoning in some forks)
//	Claude messages    a "thinking" content block, with a signature
//	Responses          a "reasoning" output item carrying summary text
//
// A caller on one schema talking to a model declared on another only keeps its
// thinking if the text is moved between those three places rather than
// dropped, so the IR keeps a thought as a part of its own (PartThinking) and
// every encoder writes it where its schema puts it.
package protocol

import (
	"encoding/json"
	"strings"
)

// Schema is one of the three wire schemas this plugin speaks.
type Schema string

const (
	// SchemaChat is OpenAI chat completions (/v1/chat/completions).
	SchemaChat Schema = "chat-completions"
	// SchemaClaude is Anthropic messages (/v1/messages).
	SchemaClaude Schema = "messages"
	// SchemaResponses is the OpenAI Responses API (/v1/responses).
	SchemaResponses Schema = "responses"
)

// Path is the request path, relative to the model plane's base URL, that a
// schema is served at. /messages is at the base host's /v1/messages; the other
// two hang off /v1.
func (s Schema) Path() string {
	switch s {
	case SchemaClaude:
		return "/messages"
	case SchemaResponses:
		return "/responses"
	default:
		return "/chat/completions"
	}
}

// Valid reports whether s names a schema this plugin serves.
func (s Schema) Valid() bool {
	switch s {
	case SchemaChat, SchemaClaude, SchemaResponses:
		return true
	}
	return false
}

// NormalizeSchema maps every spelling the host and clients use for a schema
// onto this plugin's three. An unknown value returns "" so callers can refuse
// rather than guess.
func NormalizeSchema(name string) Schema {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "openai", "chat", "chat-completions", "chat_completions", "chatcompletions":
		return SchemaChat
	case "claude", "anthropic", "messages":
		return SchemaClaude
	case "openai-response", "openai-responses", "responses", "response":
		return SchemaResponses
	}
	return ""
}

// Role is a turn's author, normalized across the three schemas' vocabularies.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// PartKind is what one part of a turn carries.
type PartKind string

const (
	PartText       PartKind = "text"
	PartImage      PartKind = "image"
	PartToolCall   PartKind = "tool_call"
	PartToolResult PartKind = "tool_result"
	// PartThinking is a model's reasoning, on the response side only.
	PartThinking PartKind = "thinking"
	// PartRedactedThinking is a thinking block the vendor withheld; it must be
	// carried back verbatim (signature included) or the upstream refuses the
	// next turn.
	PartRedactedThinking PartKind = "redacted_thinking"
)

// Part is one piece of a turn.
type Part struct {
	Kind PartKind
	// Text is the part's text: the content of a text part, the JSON arguments
	// of a tool call (as a string, so no re-encode reorders it), the output of
	// a tool result, or the reasoning text of a thinking part.
	Text string
	// ImageURL is the image reference: an https URL or a data: URI.
	ImageURL  string
	MediaType string
	// ToolCallID / ToolName name a tool call or a tool result.
	ToolCallID string
	ToolName   string
	// IsError marks a tool result that reports a failure.
	IsError bool
	// Signature is a thinking block's provider signature.
	Signature string
}

// Turn is one message.
type Turn struct {
	Role  Role
	Parts []Part
}

// Text returns the turn's concatenated text parts, which is what a schema that
// only carries text (a tool result, a plain prompt) needs.
func (t Turn) Text() string {
	var b strings.Builder
	for _, p := range t.Parts {
		if p.Kind == PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// Tool is one callable function.
type Tool struct {
	Name        string
	Description string
	// Schema is the JSON Schema object describing the arguments. Empty means
	// "any object".
	Schema json.RawMessage
}

// ToolChoice is how a request selects tools. Empty Kind means "unset".
type ToolChoice struct {
	// Kind is "auto", "required", "none", or "tool" (a named function).
	Kind string
	Name string
}

// Request is one canonical request.
type Request struct {
	Model string
	// Turns is the conversation. System turns are folded into System.
	Turns  []Turn
	System string
	Tools  []Tool
	Choice ToolChoice
	// MaxOutput is the requested output budget; 0 means "unspecified".
	MaxOutput   int64
	Temperature *float64
	TopP        *float64
	Stop        []string
	Stream      bool
	// Effort is the requested reasoning level after normalization to the
	// target model's declared levels; "" means "no reasoning control".
	Effort string
	// Budget is the requested thinking budget in tokens (Claude's own
	// control). 0 means "unspecified".
	Budget int64
	// User is the caller-supplied end-user id, when a schema carries one.
	User string
}

// SystemText returns System with surrounding whitespace removed.
func (r *Request) SystemText() string { return strings.TrimSpace(r.System) }

// Usage is a response's token accounting, normalized across schemas.
type Usage struct {
	Input     int64
	Output    int64
	Reasoning int64
	CacheRead int64
	CacheHit  int64
}

// Resolved returns the total input tokens a client should see: the schema's
// own input count, plus cached reads, because the three schemas disagree about
// whether a cached token is counted in the input total.
func (u Usage) Resolved() (prompt, completion, total int64) {
	prompt = u.Input + u.CacheRead
	completion = u.Output
	total = prompt + completion
	return
}

// Response is one canonical non-streaming response.
type Response struct {
	ID    string
	Model string
	Parts []Part
	// StopReason is normalized: "stop", "length", "tool_calls",
	// "content_filter", or "" (unknown).
	StopReason string
	Usage      Usage
	// CreatedAt is a unix second, when the upstream gave one.
	CreatedAt int64
}

// Text returns the response's concatenated answer text.
func (r *Response) Text() string {
	var b strings.Builder
	for _, p := range r.Parts {
		if p.Kind == PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// Thinking returns the response's concatenated reasoning text.
func (r *Response) Thinking() string {
	var b strings.Builder
	for _, p := range r.Parts {
		if p.Kind == PartThinking {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ToolCalls returns the response's tool calls in order.
func (r *Response) ToolCalls() []Part {
	var out []Part
	for _, p := range r.Parts {
		if p.Kind == PartToolCall {
			out = append(out, p)
		}
	}
	return out
}

// HasToolCalls reports whether the response asked for a tool.
func (r *Response) HasToolCalls() bool { return len(r.ToolCalls()) > 0 }

// StopReason vocabulary, normalized.
const (
	StopEnd      = "stop"
	StopLength   = "length"
	StopToolUse  = "tool_calls"
	StopFiltered = "content_filter"
	StopUnknown  = ""
)

// normalizeStop maps each schema's stop vocabulary onto the shared one.
func normalizeStop(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "stop", "end_turn", "stop_sequence", "eos":
		return StopEnd
	case "length", "max_tokens", "max_output_tokens", "max_completion_tokens":
		return StopLength
	case "tool_calls", "tool_use", "function_call":
		return StopToolUse
	case "content_filter", "refusal", "guardrail":
		return StopFiltered
	}
	return StopUnknown
}

// DeltaKind is one incremental event's kind, in canonical form.
type DeltaKind int

const (
	// DeltaStart carries the response id and model.
	DeltaStart DeltaKind = iota
	// DeltaText appends answer text.
	DeltaText
	// DeltaThinking appends reasoning text.
	DeltaThinking
	// DeltaThinkingSignature carries a thinking block's signature.
	DeltaThinkingSignature
	// DeltaToolCall starts a tool call (ToolCallID/ToolName set) or appends
	// its arguments (ToolArgs).
	DeltaToolCall
	// DeltaUsage carries token accounting.
	DeltaUsage
	// DeltaStop carries the normalized stop reason.
	DeltaStop
)

// Delta is one canonical incremental event. Every stream parser produces these
// and every stream encoder consumes them, so a scheme pair that has no
// pairwise translator still carries thinking, text and tool calls.
type Delta struct {
	Kind DeltaKind
	// Index identifies the part this delta belongs to (a tool call's index, a
	// content block's index).
	Index int
	// Text is the appended text (DeltaText / DeltaThinking).
	Text string
	// Signature is a thinking block's signature.
	Signature string
	// ToolCallID / ToolName are set on the first delta of a tool call.
	ToolCallID string
	ToolName   string
	// ToolArgs is an argument fragment.
	ToolArgs string
	// Started reports whether this tool-call delta opens a new call.
	Started bool
	// StopReason is the normalized stop reason on DeltaStop.
	StopReason string
	// Usage is the accounting on DeltaUsage.
	Usage *Usage
	// ID / Model are set on DeltaStart.
	ID    string
	Model string
	// CreatedAt is a unix second, when the upstream gave one.
	CreatedAt int64
}
