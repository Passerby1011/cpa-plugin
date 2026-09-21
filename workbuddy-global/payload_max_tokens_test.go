// payload_max_tokens_test.go pins the max_completion_tokens → max_tokens
// translation.
//
// Why the translation exists: the upstream request struct only reads
// max_tokens. A client that sends the newer OpenAI alias
// (max_completion_tokens) gets it silently ignored, the upstream falls back to
// its own default output cap, and long generations are cut short with no error
// anywhere. Measured against the live service: a 128000 alias request was
// capped at the 32000 default.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTranslateMaxCompletionTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// wantTokens is the expected max_tokens value; empty means the key
		// must be absent from the marshalled body.
		wantTokens string
	}{
		{
			name:       "alias alone is translated",
			in:         `{"max_completion_tokens":128000}`,
			wantTokens: "128000",
		},
		{
			name:       "explicit max_tokens wins and alias is dropped",
			in:         `{"max_tokens":500,"max_completion_tokens":128000}`,
			wantTokens: "500",
		},
		{
			name:       "explicit max_tokens zero is preserved",
			in:         `{"max_tokens":0,"max_completion_tokens":128000}`,
			wantTokens: "0",
		},
		{
			name:       "float alias is dropped not translated",
			in:         `{"max_completion_tokens":1.5}`,
			wantTokens: "",
		},
		{
			name:       "null alias is dropped",
			in:         `{"max_completion_tokens":null}`,
			wantTokens: "",
		},
		{
			name:       "negative alias is dropped",
			in:         `{"max_completion_tokens":-100}`,
			wantTokens: "",
		},
		{
			name:       "zero alias is dropped (means unset upstream)",
			in:         `{"max_completion_tokens":0}`,
			wantTokens: "",
		},
		{
			name:       "string alias is dropped",
			in:         `{"max_completion_tokens":"128000"}`,
			wantTokens: "",
		},
		{
			name:       "absent alias leaves body untouched",
			in:         `{"max_tokens":1000}`,
			wantTokens: "1000",
		},
		{
			name:       "no token fields at all",
			in:         `{"model":"x"}`,
			wantTokens: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var obj map[string]any
			if err := json.Unmarshal([]byte(tc.in), &obj); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			translateMaxCompletionTokensInPlace(obj)

			if v, present := obj["max_completion_tokens"]; present {
				t.Fatalf("alias survived: %v", v)
			}
			out, err := json.Marshal(obj)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			body := string(out)
			if tc.wantTokens == "" {
				if strings.Contains(body, `"max_tokens"`) {
					t.Fatalf("body has max_tokens, want absent: %s", body)
				}
				return
			}
			want := `"max_tokens":` + tc.wantTokens
			if !strings.Contains(body, want) {
				t.Fatalf("body = %s, want it to contain %s", body, want)
			}
		})
	}
}

// The translated body must marshal to an integer literal, not scientific
// notation — a float-formatted cap (1.28e+05) is not what the upstream expects.
func TestTranslatedMaxTokensMarshalsAsInteger(t *testing.T) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(`{"max_completion_tokens":128000}`), &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	translateMaxCompletionTokensInPlace(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(out); got != `{"max_tokens":128000}` {
		t.Fatalf("body = %s, want {\"max_tokens\":128000}", got)
	}
	if strings.Contains(string(out), "e+") {
		t.Fatalf("scientific notation leaked into the body: %s", out)
	}
}

// A large integer-valued float must still come out as a plain integer.
func TestTranslatedMaxTokensLargeValue(t *testing.T) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(`{"max_completion_tokens":200000}`), &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	translateMaxCompletionTokensInPlace(obj)
	out, _ := json.Marshal(obj)
	if got := string(out); got != `{"max_tokens":200000}` {
		t.Fatalf("body = %s, want {\"max_tokens\":200000}", got)
	}
}
