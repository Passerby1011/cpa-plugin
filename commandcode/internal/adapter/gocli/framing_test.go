package gocli

import (
	"testing"
)

// TestCliSSEDataAcceptsBareNDJSON pins the framing the endpoint actually uses.
//
// /alpha/generate emits NDJSON - one bare JSON object per line - with no
// `data:` prefix. A parser that only accepted the SSE form discarded every
// line, so the stream never reported its finish event and a successful
// upstream response was failed downstream as "stream ended without a finish
// event".
func TestCliSSEDataAcceptsBareNDJSON(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		`{"type":"start"}`:                          {`{"type":"start"}`, true},
		`  {"type":"finish","finishReason":"stop"}`: {`{"type":"finish","finishReason":"stop"}`, true},
		`data: {"type":"start"}`:                    {`{"type":"start"}`, true},
		`[{"a":1}]`:                                 {`[{"a":1}]`, true},
		``:                                          {"", false},
		`: keep-alive`:                              {"", false},
		`event: message`:                            {"", false},
		`random text`:                               {"", false},
	}
	for line, want := range cases {
		got, ok := cliSSEData(line)
		if ok != want.ok || (ok && got != want.want) {
			t.Errorf("cliSSEData(%q) = (%q, %v), want (%q, %v)", line, got, ok, want.want, want.ok)
		}
	}
}

// TestConverterCompletesOnBareNDJSONFinish is the end-to-end regression: a
// bare-NDJSON stream carrying a finish event must complete, not truncate.
func TestConverterCompletesOnBareNDJSONFinish(t *testing.T) {
	c := NewConverter("openai", testModel)
	stream := `{"type":"start"}
{"type":"text-start","id":"txt-0"}
{"type":"text-delta","id":"txt-0","text":"hello"}
{"type":"text-end","id":"txt-0"}
{"type":"finish","finishReason":"stop"}
`
	_, done, eErr := c.Feed([]byte(stream))
	if eErr != nil {
		t.Fatalf("feed: %s", eErr.Message)
	}
	if !done {
		t.Fatal("a stream carrying a finish event did not complete")
	}
	if c.Truncated() {
		t.Fatal("a completed stream was marked truncated")
	}
}
