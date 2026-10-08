package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

// rolesOf decodes messages[].role from an already-rewritten body.
func rolesOf(t *testing.T, body []byte) []string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		t.Fatalf("messages missing or not an array: %s", body)
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			out = append(out, "<non-object>")
			continue
		}
		role, _ := msg["role"].(string)
		out = append(out, role)
	}
	return out
}

func contentsOf(t *testing.T, body []byte) []string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		c, _ := msg["content"].(string)
		out = append(out, c)
	}
	return out
}

// Upstream validates the role value exactly: `developer` is a hard 400 and
// non-lowercase/whitespace system spellings silently lose system authority.
// The canonicalizer must fold the whole system family to lowercase `system`
// while leaving every other role byte-identical.
func TestNormalizeRolesInPlace(t *testing.T) {
	cases := []struct {
		name string
		role string
		want string
	}{
		{"developer exact", "developer", "system"},
		{"developer capitalized", "Developer", "system"},
		{"developer upper", "DEVELOPER", "system"},
		{"developer leading space", " developer", "system"},
		{"developer trailing space", "developer ", "system"},
		{"developer tab wrapped", "\tdeveloper\t", "system"},
		{"developer mixed case spaced", " DevElopEr ", "system"},
		{"system exact stays", "system", "system"},
		{"system capitalized", "System", "system"},
		{"system upper", "SYSTEM", "system"},
		{"system trailing space", "system ", "system"},
		{"system leading space", " system", "system"},
		// Outside the system family: must not be touched.
		{"user untouched", "user", "user"},
		{"assistant untouched", "assistant", "assistant"},
		{"tool untouched", "tool", "tool"},
		{"function untouched", "function", "function"},
		{"unknown value untouched", "systemx", "systemx"},
		{"unrelated value untouched", "DeveloperX", "DeveloperX"},
		{"empty stays empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"m","messages":[{"role":` + string(mustJSON(tc.role)) + `,"content":"hi"}]}`)
			out := prepareUpstreamBody(body, nil, nil, "m", nil)
			got := rolesOf(t, out)
			if len(got) != 1 {
				t.Fatalf("message count = %d, want 1 (%s)", len(got), out)
			}
			if got[0] != tc.want {
				t.Errorf("role = %q, want %q", got[0], tc.want)
			}
		})
	}
}

// Malformed inputs must never panic or gain/lose messages.
func TestNormalizeRolesInPlaceMalformed(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no messages key", `{"model":"m"}`},
		{"messages null", `{"model":"m","messages":null}`},
		{"messages empty", `{"model":"m","messages":[]}`},
		{"messages wrong type", `{"model":"m","messages":"nope"}`},
		{"non-object element", `{"model":"m","messages":[123,"x",null]}`},
		{"role missing", `{"model":"m","messages":[{"content":"hi"}]}`},
		{"role wrong type", `{"model":"m","messages":[{"role":7,"content":"hi"}]}`},
		{"role null", `{"model":"m","messages":[{"role":null,"content":"hi"}]}`},
		{"empty body", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = prepareUpstreamBody([]byte(tc.body), nil, nil, "m", nil) // must not panic
		})
	}

	// Non-object elements keep their position (no message may be dropped).
	body := []byte(`{"model":"m","messages":[123,{"role":"developer","content":"a"},{"role":"user","content":"b"}]}`)
	out := prepareUpstreamBody(body, nil, nil, "m", nil)
	got := rolesOf(t, out)
	want := []string{"<non-object>", "system", "user"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("roles = %v, want %v", got, want)
	}
	if contents := contentsOf(t, out); len(contents) != 2 || contents[0] != "a" || contents[1] != "b" {
		t.Errorf("contents = %v, order/content changed", contents)
	}
}

// Two passes must converge: normalizing an already-normalized body is a no-op.
func TestNormalizeRolesIdempotent(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"Developer","content":"a"},{"role":"SYSTEM","content":"b"},{"role":"user","content":"c"}]}`)
	once := prepareUpstreamBody(body, nil, nil, "m", nil)
	twice := prepareUpstreamBody(once, nil, nil, "m", nil)
	if string(once) != string(twice) {
		t.Errorf("not idempotent:\n once = %s\n twice = %s", once, twice)
	}
}

// The Global gateway rejects a body whose first message is not an exact
// lowercase `system`. Injection must happen then, and must NOT happen when a
// canonical system is already first (no duplicate injections).
func TestEnsureSystemMessageFirstMessageRules(t *testing.T) {
	global := &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}}

	t.Run("injects when first message is user", func(t *testing.T) {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		out := prepareUpstreamBody(body, nil, global, "m", nil)
		got := rolesOf(t, out)
		want := []string{"system", "user"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("roles = %v, want %v", got, want)
		}
		if c := contentsOf(t, out); c[0] != "You are a helpful assistant." || c[1] != "hi" {
			t.Errorf("contents = %v; original message must follow the injected one", c)
		}
	})

	t.Run("normalization alone satisfies the rule (no injection)", func(t *testing.T) {
		// Capitalized `System` used to be treated as "a system message exists"
		// and suppressed injection, causing upstream 400.
		body := []byte(`{"model":"m","messages":[{"role":"System","content":"be terse"},{"role":"user","content":"hi"}]}`)
		out := prepareUpstreamBody(body, nil, global, "m", nil)
		got := rolesOf(t, out)
		want := []string{"system", "user"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("roles = %v, want %v (exactly two messages)", got, want)
		}
		if c := contentsOf(t, out); c[0] != "be terse" {
			t.Errorf("first content = %q; the client's own system text must be preserved", c[0])
		}
	})

	t.Run("injects when system sits after user", func(t *testing.T) {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"system","content":"be terse"}]}`)
		out := prepareUpstreamBody(body, nil, global, "m", nil)
		got := rolesOf(t, out)
		want := []string{"system", "user", "system"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("roles = %v, want %v (original order preserved)", got, want)
		}
		if c := contentsOf(t, out); c[1] != "hi" || c[2] != "be terse" {
			t.Errorf("contents = %v; existing messages must keep their relative order", c)
		}
	})

	t.Run("developer first is normalized so no injection is needed", func(t *testing.T) {
		body := []byte(`{"model":"m","messages":[{"role":"developer","content":"be terse"},{"role":"user","content":"hi"}]}`)
		out := prepareUpstreamBody(body, nil, global, "m", nil)
		got := rolesOf(t, out)
		want := []string{"system", "user"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("roles = %v, want %v", got, want)
		}
	})

	t.Run("CN is untouched", func(t *testing.T) {
		cn := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		out := prepareUpstreamBody(body, nil, cn, "m", nil)
		got := rolesOf(t, out)
		want := []string{"user"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("roles = %v, want %v (CN must not gain a system message)", got, want)
		}
	})

	t.Run("CN still normalizes roles", func(t *testing.T) {
		cn := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}
		body := []byte(`{"model":"m","messages":[{"role":"developer","content":"x"},{"role":"user","content":"hi"}]}`)
		out := prepareUpstreamBody(body, nil, cn, "m", nil)
		if got := rolesOf(t, out); got[0] != "system" {
			t.Fatalf("roles = %v; developer must be normalized on CN too", got)
		}
	})
}

// A body without any system-class message must not gain one on a nil/absent
// auth (the tests and the direct-HTTP path pass sa=nil).
func TestEnsureSystemMessageNilAuth(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	out := prepareUpstreamBody(body, nil, nil, "m", nil)
	if got := rolesOf(t, out); !reflect.DeepEqual(got, []string{"user"}) {
		t.Fatalf("roles = %v, want [user]", got)
	}
}

// mustJSON renders a Go string as a JSON string literal for inline bodies.
// (host_bridge.go provides the production helper of the same name.)
