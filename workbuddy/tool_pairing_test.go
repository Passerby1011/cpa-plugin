// tool_pairing_test.go pins the outbound tool pairing repair.
//
// The upstream rejects a broken assistant.tool_calls ↔ tool pairing with a 400,
// and an agent client that persisted an unanswered call replays that history
// every turn, so the conversation stays dead until something removes the
// offending entries. These tests cover the shapes that occur in practice.
package main

import (
	"encoding/json"
	"testing"
)

// msg builds one message map.
func msg(role string, extra map[string]any) map[string]any {
	m := map[string]any{"role": role}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func assistantCall(ids ...string) map[string]any {
	calls := make([]any, 0, len(ids))
	for _, id := range ids {
		calls = append(calls, map[string]any{
			"id":       id,
			"type":     "function",
			"function": map[string]any{"name": "run_shell", "arguments": `{}`},
		})
	}
	return msg("assistant", map[string]any{"tool_calls": calls})
}

func toolResult(id string) map[string]any {
	return msg("tool", map[string]any{"tool_call_id": id, "content": "ok"})
}

func userMsg(text string) map[string]any {
	return msg("user", map[string]any{"content": text})
}

// repair runs the pairing repair and returns the resulting message list.
func repair(t *testing.T, messages []any) []any {
	t.Helper()
	obj := map[string]any{"messages": messages}
	repairToolPairingInPlace(obj)
	out, _ := obj["messages"].([]any)
	return out
}

func messageRoles(messages []any) []string {
	roles := make([]string, 0, len(messages))
	for _, raw := range messages {
		m, _ := raw.(map[string]any)
		r, _ := m["role"].(string)
		roles = append(roles, r)
	}
	return roles
}

// A correct pairing must come back untouched (same slice, no rewrite).
func TestToolPairingCorrectListUntouched(t *testing.T) {
	in := []any{
		userMsg("hi"),
		assistantCall("c1"),
		toolResult("c1"),
		userMsg("next"),
	}
	obj := map[string]any{"messages": in}
	if changed := repairToolPairingInPlace(obj); changed {
		t.Fatal("a correct list must not be reported as changed")
	}
	got, _ := obj["messages"].([]any)
	if len(got) != len(in) {
		t.Fatalf("list length changed: %d -> %d", len(in), len(got))
	}
}

// An orphan call (no result) is dropped from tool_calls.
func TestToolPairingOrphanCallDropped(t *testing.T) {
	got := repair(t, []any{
		userMsg("hi"),
		assistantCall("c1"),
		// no tool result for c1
		userMsg("next"),
	})
	if roles := messageRoles(got); len(roles) != 3 {
		t.Fatalf("roles = %v, want 3 messages", roles)
	}
	call, _ := got[1].(map[string]any)
	if _, present := call["tool_calls"]; present {
		t.Fatalf("orphan tool_calls key survived: %v", call["tool_calls"])
	}
}

// An orphan result (no call) is dropped entirely.
func TestToolPairingOrphanResultDropped(t *testing.T) {
	got := repair(t, []any{
		userMsg("hi"),
		toolResult("c1"), // nothing called c1
		userMsg("next"),
	})
	if roles := messageRoles(got); len(roles) != 2 {
		t.Fatalf("roles = %v, want the orphan result removed", roles)
	}
	for _, raw := range got {
		m, _ := raw.(map[string]any)
		if r, _ := m["role"].(string); r == "tool" {
			t.Fatal("orphan tool result survived")
		}
	}
}

// A partial batch keeps the answered call and drops the unanswered one.
func TestToolPairingPartialBatchKeepsAnswered(t *testing.T) {
	got := repair(t, []any{
		assistantCall("c1", "c2"),
		toolResult("c1"),
		// c2 never answered
		userMsg("next"),
	})
	call, _ := got[0].(map[string]any)
	calls, _ := call["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("kept %d calls, want 1 (the answered one)", len(calls))
	}
	kept, _ := calls[0].(map[string]any)
	if kept["id"] != "c1" {
		t.Fatalf("kept call id = %v, want c1", kept["id"])
	}
	// The answered result must still be there.
	if roles := messageRoles(got); len(roles) != 3 || roles[1] != "tool" {
		t.Fatalf("roles = %v, want assistant/tool/user", roles)
	}
}

// A message inserted between the call and its results is moved after the batch,
// so the results are contiguous again.
func TestToolPairingInsertedMessageRepacked(t *testing.T) {
	got := repair(t, []any{
		assistantCall("c1"),
		userMsg("interruption"), // sits between call and result
		toolResult("c1"),
	})
	want := []string{"assistant", "tool", "user"}
	if roles := messageRoles(got); !equalStrings(roles, want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
}

// Two batches: the second batch's header must not be swallowed by the first
// batch's repack, and both batches must end up with adjacent results.
func TestToolPairingTwoBatchesBothRepacked(t *testing.T) {
	got := repair(t, []any{
		assistantCall("c1"),
		userMsg("noise-1"),
		toolResult("c1"),
		assistantCall("c2"),
		userMsg("noise-2"),
		toolResult("c2"),
	})
	want := []string{"assistant", "tool", "user", "assistant", "tool", "user"}
	if roles := messageRoles(got); !equalStrings(roles, want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
}

// A tool result belonging to an earlier batch must not be pulled into a later
// batch's group.
func TestToolPairingForeignResultNotPulled(t *testing.T) {
	got := repair(t, []any{
		assistantCall("c1"),
		toolResult("c1"),
		assistantCall("c2"),
		toolResult("c2"),
	})
	want := []string{"assistant", "tool", "assistant", "tool"}
	if roles := messageRoles(got); !equalStrings(roles, want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
}

// Messages without any tool involvement are never touched.
func TestToolPairingNonToolConversationUntouched(t *testing.T) {
	in := []any{userMsg("a"), msg("assistant", map[string]any{"content": "b"}), userMsg("c")}
	obj := map[string]any{"messages": in}
	if changed := repairToolPairingInPlace(obj); changed {
		t.Fatal("a conversation without tool calls must not change")
	}
}

// A body with no messages key, or a non-array messages value, is left alone.
func TestToolPairingNoMessages(t *testing.T) {
	obj := map[string]any{"model": "x"}
	if repairToolPairingInPlace(obj) {
		t.Fatal("body without messages must not change")
	}
	obj2 := map[string]any{"messages": "not-an-array"}
	if repairToolPairingInPlace(obj2) {
		t.Fatal("non-array messages must not change")
	}
}

// The repaired body must still marshal to valid JSON.
func TestToolPairingRepairedBodyMarshals(t *testing.T) {
	obj := map[string]any{"messages": []any{
		assistantCall("c1", "c2"),
		toolResult("c1"),
		userMsg("x"),
	}}
	repairToolPairingInPlace(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !json.Valid(out) {
		t.Fatalf("invalid JSON: %s", out)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
