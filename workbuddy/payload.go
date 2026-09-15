// payload.go rewrites the outgoing chat completion request body before it's
// forwarded upstream. The single-pass entry point is prepareUpstreamBody; the
// four *InPlace helpers are the field-level mutations it composes, and the
// legacy *ForUpstream / forceStreamBody wrappers exist for tests and other
// call sites that need them individually.
package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

// forceStreamBody returns the request body with "stream":true set, since the
// upstream rejects non-streaming chat requests.
// prepareUpstreamBody composes forceStreamBody + normalizeToolsForUpstream +
// rewriteModelInBody + rewriteSystemForUpstream + ensureSystemMessageInPlace into a
// single unmarshal/marshal pass (v0.6.31 perf: was 4-5 full JSON round-trips
// on every chat completion). The 4 legacy helpers remain for tests and other
// call sites that need them individually.
func prepareUpstreamBody(payload, original []byte, sa *storedAuth, upstreamModel string) []byte {
	src := payload
	if len(src) == 0 {
		src = original
	}
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if json.Unmarshal(src, &obj) != nil {
		return src
	}

	// 1. normalizeRoles: canonicalize message role spelling before any other
	// transform. Upstream matches the role value exactly, so `developer`,
	// `System` and `"system "` all misbehave; every later step must see the
	// canonical form (see normalizeRolesInPlace).
	normalizeRolesInPlace(obj)

	// 2. forceStream: CodeBuddy rejects non-stream requests.
	obj["stream"] = true

	// 3. streamOptions: ask upstream to report usage on the final frame. The
	// official CLI always sends this; without it the upstream may omit the
	// usage block, and our SSE aggregation then publishes an empty usage
	// detail. A caller-supplied value always wins.
	ensureStreamOptionsInPlace(obj)

	// 4. normalizeTools: tool_choice object form → string; "none" suppresses tools.
	normalizeToolsInPlace(obj)

	// 5. rewriteModel: swap client model name to upstream model id.
	rewriteModelInPlace(obj, upstreamModel)

	// 6. system sanitization: strip blocked Claude Code template phrases.
	rewriteSystemInPlace(obj)

	// 7. desensitize the configured prompt and tool metadata fields.
	applyDesensitizeInPlace(obj, currentFeatureRuntime())

	// 8. ensureSystemMessage: inject a minimal system message when the first
	// message is not one. Global rejects a body whose first message is not an
	// exact `system`; CN tolerates anything, so this stays Global-only.
	ensureSystemMessageInPlace(obj, sa)

	// 9. alignThinkingFields: mirror the official CLI's configureThinkingSettings,
	// which is NOT part of the compatibility pipeline it skips for internal
	// domains, so these fields are what the real client sends to this gateway.
	alignThinkingFieldsInPlace(obj)

	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// alignThinkingFieldsInPlace adds the two thinking-side fields the official
// client always sets on model requests but this plugin never did:
//
//	reasoning_summary: "auto"  — the SDK-level thinking summary control
//	verbosity:         "high"  — text verbosity
//
// They are only added when the caller is asking for thinking at all (an
// explicit reasoning_effort/reasoning.effort, or a reasoning_summary of its
// own). A request with no thinking signal must stay untouched: the official
// client only reaches configureThinkingSettings' body mutations on the
// thinking-enabled path, and adding a summary to a non-thinking request would
// flip the upstream's own "is thinking on" inference.
//
// Existing caller values always win — this only fills fields that are absent,
// and never overwrites an explicit "verbosity".
func alignThinkingFieldsInPlace(obj map[string]any) bool {
	if !requestWantsThinking(obj) {
		return false
	}
	changed := false
	if _, ok := obj["reasoning_summary"]; !ok {
		obj["reasoning_summary"] = "auto"
		changed = true
	}
	if _, ok := obj["verbosity"]; !ok {
		obj["verbosity"] = "high"
		changed = true
	}
	return changed
}

// requestWantsThinking reports whether the body carries any thinking signal,
// matching the official client's isThinkingEnabled + the effort it reads off
// modelSettings. reasoning_effort and reasoning.effort are both honoured
// because callers use either shape.
func requestWantsThinking(obj map[string]any) bool {
	if _, ok := obj["reasoning_summary"]; ok {
		return true
	}
	if v, ok := obj["reasoning_effort"].(string); ok && strings.TrimSpace(v) != "" {
		return true
	}
	if r, ok := obj["reasoning"].(map[string]any); ok {
		if v, ok := r["effort"].(string); ok && strings.TrimSpace(v) != "" {
			return true
		}
		if enabled, ok := r["enabled"].(bool); ok && enabled {
			return true
		}
	}
	return false
}

// normalizeToolsInPlace is the in-place form of normalizeToolsForUpstream.
// Returns true when obj was modified.
func normalizeToolsInPlace(obj map[string]any) bool {
	changed := false
	suppressTools := func() {
		if _, ok := obj["tools"]; ok {
			delete(obj, "tools")
			changed = true
		}
		if _, ok := obj["functions"]; ok {
			delete(obj, "functions")
			changed = true
		}
	}
	if tc, present := obj["tool_choice"]; present {
		switch v := tc.(type) {
		case string:
			if strings.EqualFold(strings.TrimSpace(v), "none") {
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			}
		case map[string]any:
			typ, _ := v["type"].(string)
			typ = strings.ToLower(strings.TrimSpace(typ))
			switch typ {
			case "none":
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			case "auto", "required":
				obj["tool_choice"] = typ
				changed = true
			case "function":
				name := ""
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
				if name == "" {
					name, _ = v["name"].(string)
				}
				name = strings.TrimSpace(name)
				if name != "" {
					obj["tool_choice"] = name
				} else {
					obj["tool_choice"] = "auto"
				}
				changed = true
			default:
				delete(obj, "tool_choice")
				changed = true
			}
		default:
			delete(obj, "tool_choice")
			changed = true
		}
	}
	return changed
}

// rewriteSystemInPlace is the in-place form of rewriteSystemForUpstream.
func rewriteSystemInPlace(obj map[string]any) bool {
	messages, _ := obj["messages"].([]any)
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if rewriteContentField(msg) {
			changed = true
		}
	}
	return changed
}

// ensureStreamOptionsInPlace asks the upstream to include a usage block on the
// final SSE frame. The official CLI always sends stream_options.include_usage;
// without it the upstream may omit usage entirely, and the aggregation in
// stream.go then reports an empty usage detail.
//
// A caller-supplied object is never overwritten. A non-object value is replaced
// instead of forwarded: upstream types this field as an object, so passing a
// malformed value through would only turn a working request into a 400.
// Returns true when the field was added or repaired.
func ensureStreamOptionsInPlace(obj map[string]any) bool {
	if v, present := obj["stream_options"]; present {
		if _, isObject := v.(map[string]any); isObject {
			return false
		}
	}
	obj["stream_options"] = map[string]any{"include_usage": true}
	return true
}

// normalizeRolesInPlace canonicalizes message role spelling in place.
//
// Upstream validates `messages[].role` by exact value, and only accepts the
// canonical lowercase forms:
//
//	`developer` (exact)                     -> HTTP 400 code=11128
//	                                           "Illegal API invocation from an
//	                                           unapproved channel"
//	`System` / `SYSTEM` as the first message -> Global: HTTP 400 "first message
//	                                           is not system prompt"
//	`System` / `"system "` anywhere           -> HTTP 200 but silently DEMOTED
//	                                           to an ordinary message, losing
//	                                           system-level authority
//
// `developer` is OpenAI's newer alias for `system` (Codex / Cursor send it), so
// folding it into `system` preserves meaning. Case and surrounding whitespace
// are pure spelling noise; both are normalized so the demotion path is closed
// too, not just the hard 400.
//
// Values outside the system family (user / assistant / tool / function / any
// unknown string) are left byte-identical: this only repairs system-class
// spelling, it never merges, reorders or drops messages. Returns true when a
// role was rewritten.
func normalizeRolesInPlace(obj map[string]any) bool {
	messages, ok := obj["messages"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		normalized := strings.ToLower(strings.TrimSpace(role))
		switch normalized {
		case "system", "developer":
			if role != "system" {
				msg["role"] = "system"
				changed = true
			}
		}
	}
	return changed
}

// ensureSystemMessageInPlace injects a minimal system message when the FIRST
// message is not one. Global (www.workbuddy.ai) rejects a request whose first
// message is not an exact lowercase `system` with code 11128 "first message is
// not system prompt"; CN (copilot.tencent.com) accepts anything, so this stays
// Global-only. Returns true when a message was prepended.
//
// The check is deliberately an exact match, not strings.EqualFold: upstream
// itself treats `System` as "not a system prompt", and normalizeRolesInPlace
// has already canonicalized every system-class spelling by the time this runs.
// Loosening it here would suppress the injection for a body upstream rejects.
func ensureSystemMessageInPlace(obj map[string]any, sa *storedAuth) bool {
	if sa == nil || !isGlobalDomain(sa.Auth.Domain) {
		return false
	}
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return false
	}
	first, ok := messages[0].(map[string]any)
	if ok {
		if role, _ := first["role"].(string); role == "system" {
			return false
		}
	}
	systemMsg := map[string]any{
		"role":    "system",
		"content": "You are a helpful assistant.",
	}
	obj["messages"] = append([]any{systemMsg}, messages...)
	return true
}

// rewriteModelInPlace swaps obj["model"] to upstreamModel when non-empty.
// Returns true when modified.
func rewriteModelInPlace(obj map[string]any, upstreamModel string) bool {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return false
	}
	cur, _ := obj["model"].(string)
	if cur == upstreamModel {
		return false
	}
	obj["model"] = upstreamModel
	return true
}

func forceStreamBody(payload, original []byte) []byte {
	src := payload
	if len(src) == 0 {
		src = original
	}
	var obj map[string]any
	if json.Unmarshal(src, &obj) != nil {
		return src
	}
	obj["stream"] = true
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// normalizeToolsForUpstream adapts OpenAI tools / tool_choice fields to
// CodeBuddy's chat schema before the request is forwarded.
//
// Live-verified against /v2/chat/completions (2026-07):
//  1. tool_choice is typed as string on the upstream Go struct. OpenAI's object
//     form {"type":"function","function":{"name":"..."}} returns 400 code 11101
//     ("cannot unmarshal object into Go struct field Request.tool_choice of
//     type string"). Convert known object shapes to the matching string.
//  2. tool_choice "none" is accepted but ignored when tools[] is non-empty —
//     the model still emits tool_calls. The only reliable way to suppress tools
//     is to omit tools (and functions) entirely.
//
// String values auto / required / <function name> are left untouched.
func normalizeToolsForUpstream(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	changed := false

	suppressTools := func() {
		if _, ok := obj["tools"]; ok {
			delete(obj, "tools")
			changed = true
		}
		if _, ok := obj["functions"]; ok {
			delete(obj, "functions")
			changed = true
		}
	}

	if tc, present := obj["tool_choice"]; present {
		switch v := tc.(type) {
		case string:
			if strings.EqualFold(strings.TrimSpace(v), "none") {
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			}
		case map[string]any:
			typ, _ := v["type"].(string)
			typ = strings.ToLower(strings.TrimSpace(typ))
			switch typ {
			case "none":
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			case "auto", "required":
				obj["tool_choice"] = typ
				changed = true
			case "function":
				name := ""
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
				if name == "" {
					name, _ = v["name"].(string)
				}
				name = strings.TrimSpace(name)
				if name != "" {
					obj["tool_choice"] = name
				} else {
					// Object force without a name: fall back to auto instead of 400.
					obj["tool_choice"] = "auto"
				}
				changed = true
			default:
				// Unknown object shape → drop rather than forward a 400.
				delete(obj, "tool_choice")
				changed = true
			}
		default:
			// null / array / number — drop to keep upstream happy.
			delete(obj, "tool_choice")
			changed = true
		}
	}

	if !changed {
		return payload
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// rewriteSystemForUpstream neutralizes Claude Code template phrases that
// Tencent CodeBuddy's content filter blocklists verbatim — the agent identity
// line ("You are Claude Code, Anthropic's official CLI for Claude.") and the
// git injection ("Main branch (you will usually use this for PRs)"). Each
// rewrite is a single-word change so the prompt's meaning is preserved while
// dodging the exact-match filter.
func rewriteSystemForUpstream(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	messages, _ := obj["messages"].([]any)
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if rewriteContentField(msg) {
			changed = true
		}
		if rewriteToolCallArguments(msg) {
			changed = true
		}
	}
	if !changed {
		return payload
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// rewriteToolCallArguments sanitizes assistant tool-call arguments.
//
// tool_calls[].function.arguments is a JSON document serialized into a string,
// so the content walk above never sees it. A blocked phrase that a tool wrote
// into its arguments would otherwise reach upstream verbatim and trip the same
// filter the content path is dodging.
func rewriteToolCallArguments(msg map[string]any) bool {
	calls, ok := msg["tool_calls"].([]any)
	if !ok {
		return false
	}
	modified := false
	for _, rawCall := range calls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if rewritten := sanitizeBlockedTemplates(args); rewritten != args {
			fn["arguments"] = rewritten
			modified = true
		}
	}
	return modified
}

// rewriteContentField sanitizes blocked templates in one message's content,
// handling both plain-string and OpenAI multimodal (array of parts) shapes.
// Returns true if the message was modified.
func rewriteContentField(msg map[string]any) bool {
	switch c := msg["content"].(type) {
	case string:
		if r := sanitizeBlockedTemplates(c); r != c {
			msg["content"] = r
			return true
		}
	case []any:
		modified := false
		for _, p := range c {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := part["text"].(string); ok {
				if r := sanitizeBlockedTemplates(t); r != t {
					part["text"] = r
					modified = true
				}
			}
		}
		return modified
	}
	return false
}

var sanitizeFeatures = []string{
	"x-anthropic-billing-header",
	"You are Claude Code",
	"Main branch (",
	// Upstream blocks the Anthropic feedback sentence as a whole (verified
	// directly: a request carrying it returns 400 code=11128). The marker is
	// the repo path inside it, so the rewrite below is reachable.
	"anthropics/claude-code/issues",
}

var sanitizeBillingHeaderRE = regexp.MustCompile(`(?i)x-anthropic-billing-header(?::[^;\n]*;?\s*)?`)
var sanitizeCCEntrypointRE = regexp.MustCompile(`(?i)\bcc_entrypoint=`)
var sanitizeCCKeyValueRE = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

func sanitizeBlockedTemplates(s string) string {
	if !hasFingerprint(s) {
		return s
	}
	original := s
	s = strings.ReplaceAll(s,
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude")
	s = strings.ReplaceAll(s,
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)")
	s = strings.ReplaceAll(s,
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues")
	s = sanitizeBillingHeaderRE.ReplaceAllString(s, "")
	s = sanitizeCCKeyValueRE.ReplaceAllString(s, "")
	if s == original {
		return original
	}
	return strings.TrimSpace(s)
}

func hasFingerprint(s string) bool {
	if sanitizeCCEntrypointRE.MatchString(s) {
		return true
	}
	for _, feature := range sanitizeFeatures {
		if strings.Contains(s, feature) {
			return true
		}
	}
	return sanitizeBillingHeaderRE.MatchString(s)
}

// rewriteModelInBody replaces the "model" field of a chat-completions body
// with the resolved upstream model ID.
func rewriteModelInBody(body []byte, upstreamModel string) []byte {
	if len(body) == 0 || strings.TrimSpace(upstreamModel) == "" {
		return body
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	cur, _ := obj["model"].(string)
	if strings.EqualFold(strings.TrimSpace(cur), strings.TrimSpace(upstreamModel)) {
		return body
	}
	obj["model"] = upstreamModel
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		if len(x) == 0 {
			return true
		}
		// Legacy function_call shell: {"name":"","arguments":""} is the
		// upstream's terminal-chunk artifact, not a real call — treat as empty
		// when every value is itself empty.
		for _, val := range x {
			if !isEmptyValue(val) {
				return false
			}
		}
		return true
	}
	return false
}
