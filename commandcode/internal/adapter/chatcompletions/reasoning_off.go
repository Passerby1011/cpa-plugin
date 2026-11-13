// Package chatcompletions: reasoning off-state translation.
//
// CommandCode's Provider API has its own word for "do not think": the string
// "off" in reasoning_effort. Everything else about this is surprising, so it is
// recorded here verbatim from the live upstream (a GOAT key, 2026-10-23):
//
//   - the OpenAI vocabulary's "none" is REFUSED: 'Invalid option: expected one
//     of "off"|"low"|"medium"|"high"|"xhigh"|"max"';
//   - "off" is only accepted by models that CAN turn thinking off. For the rest
//     it is refused too: 'does not support reasoning_effort "off"'.
//
// So "off" cannot be forwarded blindly and "none" cannot be forwarded blindly
// either. This file owns the one table of models that took "off" and the one
// rewrite both legs go through.
package chatcompletions

import (
	"encoding/json"
	"strings"

	"github.com/hex-ci/cpa-plugin/commandcode/internal/errclass"
)

// ReasoningEffortOff is the PROVIDER's spelling of "do not think", as opposed to
// the OpenAI-facing "none". Both that spelling and a client's literal "off"
// mean the same thing to us and are sent upstream as this.
const ReasoningEffortOff = "off"

// offCapableModels are the models measured to accept reasoning_effort "off" and
// to actually stop thinking (0 reasoning tokens on each, on a GOAT key).
//
// WHY A TABLE: the upstream publishes no capability field for this, and the
// vendor's own clients hardcode it. Measuring it per request is not an option
// (it would cost a completion per probe), so the measured set is embedded.
//
// The negative half matters as much as the positive one: a model NOT in this
// table REFUSES "off" outright (deepseek/deepseek-v4-flash-fast, moonshotai/Kimi-K3
// among the measured ones), so the field must be DROPPED for it rather than
// sent and failed. "Drop the control" is the honest translation of "this model
// cannot turn thinking off": the client asked for a state the target does not
// have, and sending a guaranteed 400 would be worse than running with the
// model's default.
//
// Keep this list in sync with the vendor's own effort table
// (docs/model-capabilities.md) and update it only from measurements: a model
// added here that the upstream rejects turns a working request into a 400.
var offCapableModels = map[string]bool{
	"deepseek/deepseek-v4-pro":              true,
	"deepseek/deepseek-v4-flash":            true,
	"deepseek/deepseek-v4-flash-vision-exp": true,
	"deepseek/deepseek-v4.1-flash":          true,
	"deepseek/deepseek-v4.1-flash-fast":     true,
}

// SupportsReasoningOff reports whether the upstream accepts reasoning_effort
// "off" for this model id.
func SupportsReasoningOff(upstreamModel string) bool {
	return offCapableModels[strings.TrimSpace(upstreamModel)]
}

// OffCapableModelIDs returns the measured set, for tests and documentation that
// must not drift from the code.
func OffCapableModelIDs() []string {
	ids := make([]string, 0, len(offCapableModels))
	for id := range offCapableModels {
		ids = append(ids, id)
	}
	return ids
}

// normalizeReasoningEffortOff is the value-level rule of applyReasoningOffState,
// shared by every leg that builds a Chat Completions body structurally (the
// Claude and Responses sources) and by the verbatim OpenAI passthrough.
//
// Returns the effort to SEND and whether it should be sent at all:
//
//   - "none"/"off" on a model that takes "off"  -> ("off", true)
//   - "none"/"off" on any other model           -> ("", false)  (omit)
//   - anything else                             -> (effort, true) untouched
func normalizeReasoningEffortOff(upstreamModel, effort string) (string, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(effort))
	if trimmed != "none" && trimmed != ReasoningEffortOff {
		return effort, true
	}
	if !SupportsReasoningOff(upstreamModel) {
		return "", false
	}
	return ReasoningEffortOff, true
}

// applyReasoningOffState rewrites the off-state of one encoded Chat Completions
// body so the Provider API accepts it, and reports whether it had to change
// anything.
//
// The rule, and why each branch exists:
//
//   - a client's "none" or "off" on a model that TAKES "off"  -> "off"
//   - a client's "none" or "off" on any other model           -> field dropped
//   - anything else (low/high/max/absent)                     -> untouched
//
// "Untouched" is deliberate for the middle levels: the upstream validates them
// against the union low|medium|high|xhigh|max and accepts them broadly, so
// there is nothing to correct there. Only the off-state is systematically wrong
// (the OpenAI spelling is refused outright and the provider spelling is
// per-model), which is why this is a targeted rewrite of one value rather than
// a general effort mapper.
//
// The body is decoded and re-encoded rather than string-patched: a substring
// replacement would also hit a user message containing the word none.
func applyReasoningOffState(upstreamModel string, body []byte) ([]byte, bool, *errclass.Error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, false, errclass.Translation("malformed openai request JSON: " + err.Error())
	}
	if fields == nil {
		return nil, false, errclass.Translation("malformed request body: JSON null is not a valid request")
	}
	raw, ok := fields["reasoning_effort"]
	if !ok {
		return body, false, nil
	}
	var effort string
	if err := json.Unmarshal(raw, &effort); err != nil {
		// A non-string reasoning_effort is not something this plugin can
		// interpret, and guessing would either invent a level or drop a
		// control silently. Surface it.
		return nil, false, errclass.Translation("reasoning_effort must be a string")
	}
	trimmed := strings.ToLower(strings.TrimSpace(effort))
	if trimmed != "none" && trimmed != ReasoningEffortOff {
		return body, false, nil
	}
	effortToSend, send := normalizeReasoningEffortOff(upstreamModel, effort)
	if !send {
		delete(fields, "reasoning_effort")
	} else {
		fields["reasoning_effort"] = json.RawMessage(`"` + effortToSend + `"`)
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, false, errclass.Translation("request cannot be represented as JSON")
	}
	return encoded, true, nil
}
