package gocli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hex-ci/cpa-plugin/commandcode/internal/adapter/chatcompletions"
	"github.com/hex-ci/cpa-plugin/commandcode/internal/adapter/shared"
	"github.com/hex-ci/cpa-plugin/commandcode/internal/errclass"
)

// ---- CLI transport (/alpha/generate) response parsing ----
//
// The CLI stream is SSE with data-only frames ("data: {json}\n\n") ending
// in "data: [DONE]\n\n". Every CLI event is translated onto the OpenAI
// Chat Completions chunk vocabulary and fed into the chatcompletions
// StreamConverter, so the openai / claude / openai-response synthesis is
// the one shared kernel instead of a second implementation of the three
// formats here.

// The CLI stream carries neither an id nor a model. Every synthesized chunk
// therefore needs a stand-in, and the delegate keys per-stream state on the
// FIRST chunk it sees, so the id must stay stable for the life of the stream.
//
// The model is the exception: the caller asked for a specific model, and the
// CLI transport hides the real upstream id, so echoing the REQUESTED name is
// the only honest value. It is also what downstream gateways compare against
// their own record - a fixed placeholder made every response look like the
// upstream had substituted the model. cliFallbackModel is used only when a
// caller genuinely supplied no model name.
const (
	cliChunkID       = "chatcmpl-commandcode-gocli"
	cliFallbackModel = "commandcode-gocli"
)

// cliBaseAuthority normalizes a configured base URL for the CLI transport:
// /alpha/generate hangs off the origin root while the documented Provider
// API is mounted at /provider/v1, so a base ending in that suffix (or in a
// slash) is reduced to scheme://host + remaining path. A base without a
// scheme or host cannot address an origin and is rejected.
func cliBaseAuthority(base string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil {
		return "", fmt.Errorf("invalid base url %q: %w", base, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("base url %q must include scheme and host", base)
	}
	path := strings.TrimRight(strings.TrimSuffix(u.EscapedPath(), "/provider/v1"), "/")
	return u.Scheme + "://" + u.Host + path, nil
}

// GenerateURL returns the CLI transport endpoint (/alpha/generate) for a
// configured base URL. The endpoint hangs off the origin root, not the
// documented Provider API mount, so the base is normalized first.
func GenerateURL(base string) (string, error) {
	authority, err := cliBaseAuthority(base)
	if err != nil {
		return "", err
	}
	return authority + "/alpha/generate", nil
}

// HeaderOptions carries everything GenerateHeaders needs beyond the credential.
type HeaderOptions struct {
	SessionID string
	// ProjectDir becomes the x-project-slug header (via SlugifyProjectPath).
	// Empty means the default fabricated project dir.
	ProjectDir string
	// OmitProjectSlug drops x-project-slug entirely. It is how the identity
	// layer is turned OFF: sending the default slug when the operator disabled
	// the device block would keep half the identity on. Distinct from an empty
	// ProjectDir, which still announces the default machine.
	OmitProjectSlug bool
	// Version overrides x-command-code-version. Empty means DefaultVersion.
	Version string
}

// GenerateHeaders returns the headers a CLI-transport request needs, using the
// default session-less identity. It is the thin wrapper around
// GenerateHeadersWithOptions kept for callers that carry only a session id.
func GenerateHeaders(credential, sessionID string) http.Header {
	return GenerateHeadersWithOptions(credential, HeaderOptions{SessionID: sessionID})
}

// GenerateHeadersWithOptions returns the headers a CLI-transport request
// needs. The set is the vendor CLI's own wire shape, not a convenience
// subset: the project slug and taste-learning flag describe the client the
// upstream expects to be talking to, and an empty SessionID drops the header
// entirely rather than sending an empty one.
func GenerateHeadersWithOptions(credential string, opts HeaderOptions) http.Header {
	version := opts.Version
	if version == "" {
		version = DefaultVersion
	}
	projectDir := opts.ProjectDir
	if projectDir == "" {
		projectDir = DefaultProjectDir
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "cli")
	h.Set("x-command-code-version", version)
	h.Set("x-cli-environment", "production")
	if !opts.OmitProjectSlug {
		h.Set("x-project-slug", SlugifyProjectPath(projectDir))
	}
	h.Set("x-taste-learning", "false")
	if opts.SessionID != "" {
		h.Set("x-session-id", opts.SessionID)
	}
	h.Set("Authorization", "Bearer "+credential)
	h.Set("traceparent", NewTraceparent())
	return h
}

// cliEvent is one decoded /alpha/generate SSE data payload. The struct is
// a superset across event types; only the fields of the event's own type
// are read.
type cliEvent struct {
	Type string `json:"type"`
	Text string `json:"text"`

	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Input      json.RawMessage `json:"input"`
	Args       json.RawMessage `json:"args"`
	Arguments  json.RawMessage `json:"arguments"`

	FinishReason string    `json:"finishReason"`
	TotalUsage   *cliUsage `json:"totalUsage"`

	Error   json.RawMessage `json:"error"`
	Message string          `json:"message"`
}

// cliUsage mirrors the finish event's totalUsage block.
type cliUsage struct {
	InputTokens        int64          `json:"inputTokens"`
	OutputTokens       int64          `json:"outputTokens"`
	InputTokenDetails  *cliInputUsage `json:"inputTokenDetails"`
	OutputTokenDetails *struct {
		ReasoningTokens *int64 `json:"reasoningTokens"`
	} `json:"outputTokenDetails"`
}

// cliInputUsage is the finish event's input token breakdown.
type cliInputUsage struct {
	CacheReadTokens  *int64 `json:"cacheReadTokens"`
	CacheWriteTokens *int64 `json:"cacheWriteTokens"`
	NoCacheTokens    *int64 `json:"noCacheTokens"`
}

// chatUsage renders the block as Chat Completions usage through the shared
// majority-sum kernel: cache-read maps to cached_tokens and reasoning to
// completion_tokens_details. Cache-write has no Chat Completions slot and
// is not fabricated into a nonstandard key.
func (u *cliUsage) chatUsage() map[string]any {
	var details shared.UsageDetails
	if u.InputTokenDetails != nil {
		details.CachedTokens = u.InputTokenDetails.CacheReadTokens
	}
	if u.OutputTokenDetails != nil {
		details.ReasoningTokens = u.OutputTokenDetails.ReasoningTokens
	}
	return shared.CCUsageFrom(u.InputTokens, u.OutputTokens, details)
}

// toolArguments resolves the tool-call input (the "input" spelling, or its
// "args"/"arguments" aliases) into the JSON-encoded arguments string of an
// OpenAI tool call. A string-typed input is already the arguments JSON
// text; an absent, null, or empty input becomes the FR-005 default "{}".
func (ev *cliEvent) toolArguments() string {
	raw := ev.Input
	if !shared.HasContent(raw) {
		raw = ev.Args
	}
	if !shared.HasContent(raw) {
		raw = ev.Arguments
	}
	if !shared.HasContent(raw) {
		return "{}"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return shared.DefaultArgs(s)
	}
	return string(raw)
}

// errorCode renders the error field, which may be a string code or an
// object ({"type":...,"message":...}); a non-string is stringified so its
// content still reaches classification and the redacted message.
func (ev *cliEvent) errorCode() string {
	if !shared.HasContent(ev.Error) {
		return ""
	}
	var s string
	if err := json.Unmarshal(ev.Error, &s); err == nil {
		return s
	}
	return string(ev.Error)
}

// classifyCLIError maps a CLI error event onto FR-009 classes the same way
// the sibling adapters classify in-stream error objects: a numeric or
// rate-limit/quota code routes through FromStatus so the section-7
// retryability semantics apply; anything else is a retryable upstream
// server failure. An error event is never treated as an empty delta.
func classifyCLIError(ev *cliEvent) *errclass.Error {
	code := strings.TrimSpace(ev.errorCode())
	msg := strings.TrimSpace(ev.Message)
	detail := msg
	switch {
	case code == "" && msg == "":
		detail = "upstream CLI error event"
	case msg == "":
		detail = code
	case code != "":
		detail = code + ": " + msg
	}
	status := 0
	lower := strings.ToLower(code)
	switch {
	case lower == "":
	case strings.Contains(lower, "rate_limit"), strings.Contains(lower, "quota"):
		status = 429
	default:
		if n, err := strconv.Atoi(code); err == nil && n >= 100 && n <= 599 {
			status = n
		}
	}
	if status > 0 {
		return errclass.FromStatus(status, detail)
	}
	return errclass.UpstreamFallback(detail)
}

// cliSSEData extracts the payload from one line of the CLI transport's
// stream.
//
// The endpoint emits NDJSON - one bare JSON object per line, with no `data:`
// prefix. Accepting only the SSE form silently discarded EVERY line, so the
// finish event was never seen and a perfectly good upstream response was
// failed downstream as "stream ended without a finish event".
//
// The prefixed form is still accepted because it costs nothing and keeps the
// parser working if the vendor ever switches to SSE framing.
func cliSSEData(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if rest, ok := strings.CutPrefix(trimmed, "data:"); ok {
		return strings.TrimSpace(rest), true
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return trimmed, true
	}
	return "", false
}

// Converter incrementally parses the CLI transport's SSE stream into the
// downstream client protocol's stream (sourceFormat selects the target:
// "openai", "claude", or "openai-response").
//
// A converter is safe for SEQUENTIAL Feed calls only, matching the
// delegate; partial SSE lines are buffered across calls. One converter
// serves one response.
type Converter struct {
	inner *chatcompletions.StreamConverter

	// model is the name reported on every synthesized chunk: the model the
	// caller requested, falling back to cliFallbackModel when unknown.
	model string

	lineBuf       []byte
	done          bool
	finished      bool
	truncated     bool
	created       int64
	nextToolIndex int
}

// NewConverter returns a converter translating the CLI stream into
// sourceFormat's stream shape (the same format names
// chatcompletions.NewStreamConverter accepts).
func NewConverter(sourceFormat, model string) *Converter {
	return &Converter{
		inner:   chatcompletions.NewStreamConverter(sourceFormat),
		model:   chunkModel(model),
		created: time.Now().Unix(),
	}
}

// chunkModel resolves the model name carried by synthesized chunks.
func chunkModel(model string) string {
	if trimmed := strings.TrimSpace(model); trimmed != "" {
		return trimmed
	}
	return cliFallbackModel
}

// Feed consumes one upstream chunk (any split of the byte stream),
// returning the client-protocol events completed by this chunk, whether
// the stream is done, and a classified error for malformed frames or
// upstream error events (never a silent empty stream).
//
// The CLI finish event emits one final chunk carrying finish_reason and
// usage, followed by a synthesized [DONE] frame so the delegate's
// deferred terminal events (message_delta/message_stop,
// response.completed) are emitted even before - or without - the peer's
// own [DONE]. Both finish and [DONE] terminate the stream.
func (c *Converter) Feed(chunk []byte) (events [][]byte, done bool, eErr *errclass.Error) {
	if c.done {
		return nil, true, nil
	}
	c.lineBuf = append(c.lineBuf, chunk...)
	for {
		i := bytes.IndexByte(c.lineBuf, '\n')
		if i < 0 {
			break // keep the trailing partial line buffered
		}
		line := strings.TrimSuffix(string(c.lineBuf[:i]), "\r")
		c.lineBuf = c.lineBuf[i+1:]
		evs, lineErr := c.handleLine(line)
		if lineErr != nil {
			return events, false, lineErr
		}
		events = append(events, evs...)
		if c.done {
			break
		}
	}
	if c.done {
		c.lineBuf = nil
	}
	return events, c.done, nil
}

// handleLine parses one SSE line; non-data lines are ignored.
func (c *Converter) handleLine(line string) ([][]byte, *errclass.Error) {
	payload, ok := cliSSEData(line)
	if !ok || payload == "" {
		return nil, nil
	}
	if shared.IsSSEDone(payload) {
		events, _, eErr := c.inner.Feed([]byte("data: [DONE]\n"))
		if eErr != nil {
			return nil, eErr
		}
		// A [DONE] without a finish event is an abnormal end: the peer ended
		// the stream without reporting completion. Record the truncation so
		// the executor fails the stream instead of passing off a dropped
		// connection as a finished answer.
		c.truncated = !c.finished
		c.done = true
		return events, nil
	}
	var ev cliEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return nil, errclass.Translation("malformed CLI stream event: " + shared.RedactedSnippet(payload))
	}
	return c.translate(&ev)
}

// translate maps one CLI event onto OpenAI chunk frames fed to the
// delegate. Known event types: text-delta, reasoning-start /
// reasoning-delta / reasoning-end, tool-call, cache-write-tokens, finish,
// error. Unknown types are ignored so a benign new event kind does not
// fail the whole stream.
func (c *Converter) translate(ev *cliEvent) ([][]byte, *errclass.Error) {
	switch ev.Type {
	case "text-delta":
		if ev.Text == "" {
			return nil, nil
		}
		return c.feedChunk(c.openAIChunk(map[string]any{
			"index": 0,
			"delta": map[string]any{"content": ev.Text},
		}, nil))
	case "reasoning-delta":
		if ev.Text == "" {
			return nil, nil
		}
		return c.feedChunk(c.openAIChunk(map[string]any{
			"index": 0,
			"delta": map[string]any{"reasoning_content": ev.Text},
		}, nil))
	case "reasoning-start", "reasoning-end":
		// Lifecycle markers only: the delegate opens the thinking
		// block/item on the first reasoning text and closes it before the
		// next item, so there is no Chat Completions equivalent to send.
		return nil, nil
	case "tool-call":
		index := c.nextToolIndex
		c.nextToolIndex++
		return c.feedChunk(c.openAIChunk(map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": []any{
					shared.CCToolCallOpeningEntry(index, ev.ToolCallID, ev.ToolName, ev.toolArguments()),
				},
			},
		}, nil))
	case "cache-write-tokens":
		// No Chat Completions delta slot; the finish event's
		// inputTokenDetails repeats the totals when they matter.
		return nil, nil
	case "finish":
		return c.finish(ev)
	case "error":
		return nil, classifyCLIError(ev)
	default:
		return nil, nil
	}
}

// finish renders the terminal chunk (finish_reason + usage) and feeds the
// delegate a synthesized [DONE] so its deferred terminal events flush with
// the usage attached. The stream is done afterwards; the peer's own [DONE]
// is then redundant.
func (c *Converter) finish(ev *cliEvent) ([][]byte, *errclass.Error) {
	reason := strings.TrimSpace(ev.FinishReason)
	if reason == "" {
		reason = "stop"
	}
	c.finished = true
	var usage map[string]any
	if ev.TotalUsage != nil {
		usage = ev.TotalUsage.chatUsage()
	}
	events, eErr := c.feedChunk(c.openAIChunk(map[string]any{
		"index":         0,
		"delta":         map[string]any{},
		"finish_reason": reason,
	}, usage))
	if eErr != nil {
		return nil, eErr
	}
	terminal, _, eErr := c.inner.Feed([]byte("data: [DONE]\n"))
	if eErr != nil {
		return nil, eErr
	}
	events = append(events, terminal...)
	c.done = true
	return events, nil
}

// openAIChunk renders one single-choice chat.completion.chunk with the
// fixed stream identity; usage is attached only when present.
func (c *Converter) openAIChunk(choice map[string]any, usage map[string]any) map[string]any {
	chunk := map[string]any{
		"id":      cliChunkID,
		"object":  "chat.completion.chunk",
		"created": c.created,
		"model":   c.model,
		"choices": []any{choice},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	return chunk
}

// feedChunk wraps one chunk as an SSE data frame and feeds the delegate,
// returning the events the delegate completed.
func (c *Converter) feedChunk(chunk map[string]any) ([][]byte, *errclass.Error) {
	payload, err := json.Marshal(chunk)
	if err != nil {
		return nil, errclass.Translation("failed to encode CLI chunk: " + err.Error())
	}
	frame := make([]byte, 0, len(payload)+12)
	frame = append(frame, "data: "...)
	frame = append(frame, payload...)
	frame = append(frame, '\n', '\n')
	events, _, eErr := c.inner.Feed(frame)
	return events, eErr
}

// Truncated reports whether the CLI stream ended without its finish event
// (a [DONE] with no finish, or a connection that dropped mid-stream). The
// executor fails such a stream instead of closing it cleanly, so a dropped
// connection is never passed off as a completed answer.
func (c *Converter) Truncated() bool {
	return c.truncated
}

// ---- non-stream aggregation ----
//
// The protocol contract the aggregation below implements, mirroring the
// streaming Converter (Feed + Truncated):
//
//   - a finish event is the terminal signal: a stream carrying finish is a
//     NORMAL end even when the peer never sends its [DONE] sentinel;
//   - [DONE] without a finish event is a TRUNCATED end (the peer ended the
//     stream without reporting completion);
//   - neither finish nor [DONE] (the connection simply ended) is TRUNCATED;
//   - a stream with no events at all is an EMPTY response, reported as
//     truncated.
//
// Truncation never changes the aggregated completion's shape: the body is
// still a valid chat.completion with a synthesized terminal finish_reason
// ("stop", or "tool_calls" when tool calls were observed without a finish
// event). The flag is what lets a caller tell "the upstream finished" from
// "the upstream went away", as the streaming path already does through
// Converter.Truncated. Malformed frames and unparseable tool-call
// arguments fail classified instead of silently degrading into either.

// ConvertNonStream aggregates a complete CLI (/alpha/generate) SSE stream
// into one non-streaming OpenAI chat.completions response, for
// non-streaming clients served over the CLI transport. Text and reasoning
// deltas concatenate in arrival order, every tool-call event becomes one
// tool_calls entry, and the finish event supplies finish_reason and usage.
// Malformed frames and upstream error events fail classified, exactly like
// the streaming path; [DONE] ends parsing.
//
// It is the flag-ignoring wrapper around ConvertNonStreamChecked kept for
// existing callers: a truncated stream still yields the synthesized
// terminal completion described above. Use the checked form when a dropped
// connection must not be passed off as a finished answer.
func ConvertNonStream(stream []byte, model string) ([]byte, *errclass.Error) {
	body, _, eErr := ConvertNonStreamChecked(stream, model)
	return body, eErr
}

// ConvertNonStreamChecked is ConvertNonStream reporting whether the stream
// ended without its finish event (see the contract above): truncated is
// true for a [DONE] with no finish, for a stream that stops without either
// terminal signal, and for an empty stream. The body is the same
// completion ConvertNonStream returns, so a caller that acts on the flag
// still has the partial answer it describes; on a classified error the
// body is nil.
func ConvertNonStreamChecked(stream []byte, model string) (body []byte, truncated bool, eErr *errclass.Error) {
	var (
		text      strings.Builder
		reasoning strings.Builder
		toolCalls []any
		usage     map[string]any
		finish    string
		ended     bool // the completion was reported by a finish event
	)
	for _, line := range strings.Split(string(stream), "\n") {
		payload, ok := cliSSEData(strings.TrimSuffix(line, "\r"))
		if !ok || payload == "" {
			continue
		}
		if shared.IsSSEDone(payload) {
			// [DONE] ends parsing; only a preceding finish event makes it a
			// normal end.
			break
		}
		var ev cliEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return nil, false, errclass.Translation("malformed CLI stream event: " + shared.RedactedSnippet(payload))
		}
		switch ev.Type {
		case "text-delta":
			text.WriteString(ev.Text)
		case "reasoning-delta":
			reasoning.WriteString(ev.Text)
		case "tool-call":
			args := ev.toolArguments()
			if !json.Valid([]byte(args)) {
				// An unparseable arguments value means the frame itself is
				// damaged; emitting it would hand the client broken
				// tool-call arguments, so fail like any malformed frame.
				return nil, false, errclass.Translation("malformed CLI tool-call arguments: " + shared.RedactedSnippet(args))
			}
			toolCalls = append(toolCalls, map[string]any{
				"index": len(toolCalls),
				"id":    ev.ToolCallID,
				"type":  "function",
				"function": map[string]any{
					"name":      ev.ToolName,
					"arguments": args,
				},
			})
		case "finish":
			// The finish event is the terminal signal: it makes the stream a
			// normal end whether or not the [DONE] sentinel shows up later.
			ended = true
			if ev.TotalUsage != nil {
				usage = ev.TotalUsage.chatUsage()
			}
			if reason := strings.TrimSpace(ev.FinishReason); reason != "" {
				finish = reason
			}
		case "error":
			return nil, false, classifyCLIError(&ev)
		}
	}
	// Only a finish event reports completion. A [DONE] without one, a
	// connection that dropped mid-stream, and an empty stream are all
	// truncated; the body below still carries whatever was observed.
	truncated = !ended

	message := map[string]any{"role": "assistant", "content": text.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if finish == "" {
		// A truncated stream still yields a valid terminal reason; calls
		// observed without a finish event are tool calls by construction.
		finish = "stop"
		if len(toolCalls) > 0 {
			finish = "tool_calls"
		}
	}
	if usage == nil {
		usage = shared.CCUsageFrom(0, 0, shared.UsageDetails{})
	}
	choice := map[string]any{"index": 0, "message": message, "finish_reason": finish}
	out, err := json.Marshal(shared.CompletionEnvelope(
		cliChunkID, chunkModel(model), time.Now().Unix(), []map[string]any{choice}, usage))
	if err != nil {
		return nil, false, errclass.Translation("failed to encode CLI completion: " + err.Error())
	}
	return out, truncated, nil
}

// ---- upstream status mapping ----

// UpstreamStatus describes how a CommandCode-specific status should be
// presented to the client.
type UpstreamStatus struct {
	Status int
	Type   string
}

// ccStatusMap is the reference CC_STATUS_MAP: CommandCode reports its own
// statuses (a quota wall is 402, an overloaded backend is 529) which a client
// speaking HTTP would misread, so each is translated onto the status the
// client protocol actually expects.
var ccStatusMap = map[int]UpstreamStatus{
	// Copied verbatim from the reference CC_STATUS_MAP. The mapping is a
	// translation, not an identity: a quota wall arrives as 402 and must reach
	// the client as 429 (payment required is not a rate limit, but it is what
	// a retrying client should back off on), an overloaded backend arrives as
	// 503 and must not be reported as the generic 502.
	400: {Status: 400, Type: "invalid_request_error"},
	401: {Status: 401, Type: "authentication_error"},
	402: {Status: 429, Type: "rate_limit_error"}, // payment required -> rate limit
	403: {Status: 401, Type: "authentication_error"},
	404: {Status: 404, Type: "not_found"},
	422: {Status: 400, Type: "invalid_request_error"},
	429: {Status: 429, Type: "rate_limit_error"},
	500: {Status: 502, Type: "upstream_error"},
	502: {Status: 502, Type: "upstream_error"},
	503: {Status: 503, Type: "temporarily_unavailable"},
}

// upstreamErrorStatus is the fallback for codes the map does not know: an
// unmapped CommandCode status is still a bad upstream, never a client error.
var upstreamErrorStatus = UpstreamStatus{Status: 502, Type: "upstream_error"}

// MapUpstreamStatus translates a CommandCode status code. Unknown codes map to
// 502 / "upstream_error".
func MapUpstreamStatus(ccStatus int) UpstreamStatus {
	if s, ok := ccStatusMap[ccStatus]; ok {
		return s
	}
	return upstreamErrorStatus
}

// ---- zero-output guard ----

// zeroOutputReason is the reason reported when a completed stream carries no
// visible output at all.
const zeroOutputReason = "upstream returned no output"

// ZeroOutputReason returns a non-empty reason when a completed CLI stream
// produced no visible output at all. An empty answer that reports success is a
// silent failure the client would otherwise cache as an answer.
// It returns "" when the stream produced text, reasoning or a tool call.
func ZeroOutputReason(completedStream []byte) string {
	// Only the choices matter here, so the model name is deliberately not
	// threaded in: this guard never inspects it.
	body, _, eErr := ConvertNonStreamChecked(completedStream, "")
	if eErr != nil {
		// Unparseable is indistinguishable from empty for this purpose: either
		// way the caller has nothing to show the client.
		return zeroOutputReason
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []any  `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &completion); err != nil || len(completion.Choices) == 0 {
		return zeroOutputReason
	}
	msg := completion.Choices[0].Message
	if strings.TrimSpace(msg.Content) != "" || strings.TrimSpace(msg.ReasoningContent) != "" || len(msg.ToolCalls) > 0 {
		return ""
	}
	return zeroOutputReason
}
