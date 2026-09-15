package main

import (
	"encoding/json"
	"testing"
)

// The upstream may omit the usage block unless the request asks for it, and our
// SSE aggregation reads usage from the final frame. The official CLI always
// sends stream_options.include_usage, so we do too.
func TestEnsureStreamOptionsInjected(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	out := prepareUpstreamBody(body, nil, nil, "m")

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	so, ok := obj["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options missing or not an object: %v", obj["stream_options"])
	}
	if so["include_usage"] != true {
		t.Errorf("include_usage = %v, want true", so["include_usage"])
	}
	// forceStream must have set this in the same pass.
	if obj["stream"] != true {
		t.Errorf("stream = %v, want true", obj["stream"])
	}
}

// A caller-supplied object is authoritative: its values must survive verbatim,
// including an explicit include_usage=false and any extra keys.
func TestEnsureStreamOptionsPreserved(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"explicit false", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":false}}`},
		{"extra keys", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":true,"extra":1}}`},
		{"empty object", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream_options":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var before, after map[string]any
			if err := json.Unmarshal([]byte(tc.body), &before); err != nil {
				t.Fatal(err)
			}
			out := prepareUpstreamBody([]byte(tc.body), nil, nil, "m")
			if err := json.Unmarshal(out, &after); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !equalJSON(t, before["stream_options"], after["stream_options"]) {
				t.Errorf("caller stream_options changed:\n before = %v\n after  = %v",
					before["stream_options"], after["stream_options"])
			}
		})
	}
}

// A non-object value would be rejected upstream, so it is repaired rather than
// forwarded verbatim.
func TestEnsureStreamOptionsRepairsWrongType(t *testing.T) {
	for _, raw := range []string{`"nope"`, `123`, `[]`, `null`} {
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream_options":` + raw + `}`
		out := prepareUpstreamBody([]byte(body), nil, nil, "m")
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal %s: %v", body, err)
		}
		so, ok := obj["stream_options"].(map[string]any)
		if !ok {
			t.Errorf("stream_options (%s) not repaired to an object: %#v", raw, obj["stream_options"])
			continue
		}
		if so["include_usage"] != true {
			t.Errorf("stream_options (%s).include_usage = %v, want true", raw, so["include_usage"])
		}
	}
}

// equalJSON compares two decoded JSON values structurally.
func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()
	ra, err1 := json.Marshal(a)
	rb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		t.Fatalf("marshal: %v %v", err1, err2)
	}
	return string(ra) == string(rb)
}
