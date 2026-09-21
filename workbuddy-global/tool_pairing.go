// tool_pairing.go keeps the outbound message list from carrying a broken
// assistant.tool_calls ↔ role:tool pairing.
//
// Why this exists: the upstream validates the pairing as a hard protocol rule.
// Every id in assistant.tool_calls[] must have a matching role:"tool" message
// (matched by tool_call_id), and every role:"tool" message must follow the
// assistant call it answers; the tool results for one batch must also sit
// directly after their assistant message. Breaking any of those returns a 400
// ("tool calls and tool results do not match"), and — this is the part that
// matters — an agent client that persisted a call it never got a result for
// keeps replaying that same broken history on every subsequent turn, so the
// whole conversation stays dead. Cleaning the pair list before sending lets the
// session heal instead: one lost tool turn is a much smaller cost than a
// permanently unusable conversation.
//
// The two steps run in order and are both required:
//
//	repackToolResultBlocks — move non-tool messages that sit between an
//	    assistant's tool_calls and its results to after the whole group, so the
//	    results are contiguous again. Content is never changed, only order.
//	cleanupOrphanToolCalls — drop calls with no result and results with no
//	    call, using one shared keep-set so the two sides cannot disagree.
//
// This applies to every model: the pairing rule is protocol-level, not a
// per-model compatibility quirk.
package main

// toolPairingKeep is the shared decision for one batch: the set of call ids
// that have BOTH a call and a result. Both sides are filtered against this one
// set, which is what makes the cleanup symmetric — filtering each side by its
// own criteria is how an earlier implementation left result-only orphans behind
// and kept the 400 alive.
type toolPairingKeep map[string]struct{}

// toolCallIDs extracts the call ids from one message's tool_calls array.
func toolCallIDs(msg map[string]any) []string {
	calls, _ := msg["tool_calls"].([]any)
	if len(calls) == 0 {
		return nil
	}
	ids := make([]string, 0, len(calls))
	for _, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := call["id"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// collectToolPairing scans the message list once and returns the keep-set plus
// the call/result bookkeeping the two passes need.
func collectToolPairing(messages []any) (calls map[string]struct{}, results map[string]struct{}) {
	calls = map[string]struct{}{}
	results = map[string]struct{}{}
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "assistant":
			for _, id := range toolCallIDs(msg) {
				calls[id] = struct{}{}
			}
		case "tool":
			if id, _ := msg["tool_call_id"].(string); id != "" {
				results[id] = struct{}{}
			}
		}
	}
	return calls, results
}

// buildToolPairingKeep intersects the call ids and the result ids: only a call
// that has a result (and a result that has a call) survives.
func buildToolPairingKeep(calls, results map[string]struct{}) toolPairingKeep {
	keep := toolPairingKeep{}
	for id := range calls {
		if _, ok := results[id]; ok {
			keep[id] = struct{}{}
		}
	}
	return keep
}

// repackToolResultBlocks moves messages that were inserted between an
// assistant's tool_calls and that batch's tool results to after the batch.
//
// Only order changes; message objects are reused as-is. The batch's window ends
// at the next assistant message that carries tool_calls (the next batch's
// header — it must never be treated as an insert to move, or its own results
// would be left permanently unreordered) or at the end of the list.
func repackToolResultBlocks(messages []any) ([]any, bool) {
	changed := false
	out := make([]any, 0, len(messages))
	for i := 0; i < len(messages); i++ {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			out = append(out, messages[i])
			continue
		}
		role, _ := msg["role"].(string)
		ids := toolCallIDs(msg)
		if role != "assistant" || len(ids) == 0 {
			out = append(out, messages[i])
			continue
		}
		// Window: everything up to the next assistant call batch (exclusive)
		// or the end of the list.
		end := len(messages)
		for j := i + 1; j < len(messages); j++ {
			next, ok := messages[j].(map[string]any)
			if !ok {
				continue
			}
			nextRole, _ := next["role"].(string)
			if nextRole == "assistant" && len(toolCallIDs(next)) > 0 {
				end = j
				break
			}
		}
		batch := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			batch[id] = struct{}{}
		}

		out = append(out, messages[i])
		var parked []any
		for j := i + 1; j < end; j++ {
			next, ok := messages[j].(map[string]any)
			if !ok {
				parked = append(parked, messages[j])
				continue
			}
			nextRole, _ := next["role"].(string)
			if nextRole == "tool" {
				if id, _ := next["tool_call_id"].(string); id != "" {
					if _, mine := batch[id]; mine {
						// A result of this batch: it belongs next to the call.
						out = append(out, messages[j])
						continue
					}
				}
			}
			// Anything else — a foreign message, or a tool result belonging to
			// some other batch — moves after the group.
			parked = append(parked, messages[j])
		}
		if len(parked) > 0 {
			// Only a real move counts as a change: if nothing from this batch's
			// results was displaced, the parked items are already in place and
			// the list stays byte-identical.
			if displacedBatchResult(messages, i, end, batch) {
				changed = true
			}
			out = append(out, parked...)
		}
		i = end - 1
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// displacedBatchResult reports whether this window's repack actually moves
// anything. A window whose batch results are already contiguous (with any
// foreign messages trailing them) reconstructs to the same order, so the list
// can stay byte-identical.
func displacedBatchResult(messages []any, start, end int, batch map[string]struct{}) bool {
	// The reconstructed order puts the batch's own results first, then
	// everything else. Compare that against the original order.
	seenForeign := false
	for j := start + 1; j < end; j++ {
		m, ok := messages[j].(map[string]any)
		if !ok {
			seenForeign = true
			continue
		}
		role, _ := m["role"].(string)
		isMine := false
		if role == "tool" {
			if id, _ := m["tool_call_id"].(string); id != "" {
				_, isMine = batch[id]
			}
		}
		if isMine && seenForeign {
			// A batch result after a foreign message: it will be moved.
			return true
		}
		if !isMine {
			seenForeign = true
		}
	}
	return false
}

// cleanupOrphanToolCalls drops calls without results and results without calls.
//
// A batch with a partial result set ([c1,c2] with only c1 answered) keeps the
// answered call and drops the other; keeping the whole batch because it was
// "mostly" answered would leave c2 unanswered and still fail the upstream
// check, and dropping the whole batch would discard a result the client
// actually produced.
func cleanupOrphanToolCalls(messages []any, keep toolPairingKeep) ([]any, bool) {
	changed := false
	out := make([]any, 0, len(messages))
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "assistant":
			ids := toolCallIDs(msg)
			if len(ids) == 0 {
				out = append(out, raw)
				continue
			}
			kept := make([]any, 0, len(ids))
			original, _ := msg["tool_calls"].([]any)
			for _, rc := range original {
				call, ok := rc.(map[string]any)
				if !ok {
					continue
				}
				id, _ := call["id"].(string)
				if _, ok := keep[id]; ok {
					kept = append(kept, call)
				}
			}
			if len(kept) == len(original) {
				out = append(out, raw)
				continue
			}
			changed = true
			if len(kept) == 0 {
				// No call survives: drop the key entirely so the message is an
				// ordinary assistant turn rather than a call with no calls.
				delete(msg, "tool_calls")
				out = append(out, msg)
				continue
			}
			msg["tool_calls"] = kept
			out = append(out, msg)
		case "tool":
			id, _ := msg["tool_call_id"].(string)
			if _, ok := keep[id]; ok {
				out = append(out, raw)
				continue
			}
			changed = true
			// Orphan result: dropped.
		default:
			out = append(out, raw)
		}
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// repairToolPairingInPlace rewrites obj["messages"] when the tool pairing needs
// it. Returns true when the message list was replaced.
//
// A correct list is returned untouched (the same slice), so a body that was
// already valid is byte-identical after this step — the caller can rely on
// "no change means no change".
func repairToolPairingInPlace(obj map[string]any) bool {
	if obj == nil {
		return false
	}
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return false
	}
	calls, results := collectToolPairing(messages)
	if len(calls) == 0 && len(results) == 0 {
		return false
	}

	repacked, repackChanged := repackToolResultBlocks(messages)
	keep := buildToolPairingKeep(calls, results)
	cleaned, cleanChanged := cleanupOrphanToolCalls(repacked, keep)

	if !repackChanged && !cleanChanged {
		return false
	}
	obj["messages"] = cleaned
	return true
}
