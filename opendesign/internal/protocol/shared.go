// Package protocol — shared JSON plumbing for the three schema codecs.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// RawMessage is a JSON value carried through untouched.
type RawMessage = json.RawMessage

// ErrMalformed is the single error for a body this plugin cannot decode. It
// never quotes client content: a request body may carry a prompt, a key, or an
// image payload, and an error string travels into logs.
func ErrMalformed(what string) error {
	return fmt.Errorf("malformed %s body: not a JSON object of the expected shape", what)
}

// decodeObject decodes a body into a string-keyed map, refusing anything that
// is not a JSON object.
func decodeObject(body []byte) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, ErrMalformed("request")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, ErrMalformed("request")
	}
	return obj, nil
}

// stringField reads a string field, tolerating absence and a number.
func stringField(obj map[string]json.RawMessage, name string) string {
	raw, ok := obj[name]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// boolField reads a bool field, defaulting to false.
func boolField(obj map[string]json.RawMessage, name string) bool {
	raw, ok := obj[name]
	if !ok {
		return false
	}
	var b bool
	_ = json.Unmarshal(raw, &b)
	return b
}

// intField reads an integer field, defaulting to 0.
func intField(obj map[string]json.RawMessage, name string) int64 {
	raw, ok := obj[name]
	if !ok {
		return 0
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		if i, err := n.Int64(); err == nil {
			return i
		}
	}
	return 0
}

// floatField reads a float field and reports whether it was present.
func floatField(obj map[string]json.RawMessage, name string) (*float64, bool) {
	raw, ok := obj[name]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return nil, false
	}
	f, err := n.Float64()
	if err != nil {
		return nil, false
	}
	return &f, true
}

// firstStringField returns the first non-empty string among names. The three
// schemas spell the same field several ways, and a client only ever sends one.
func firstStringField(obj map[string]json.RawMessage, names ...string) string {
	for _, n := range names {
		if v := stringField(obj, n); strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// firstIntField returns the first non-zero integer among names.
func firstIntField(obj map[string]json.RawMessage, names ...string) int64 {
	for _, n := range names {
		if v := intField(obj, n); v != 0 {
			return v
		}
	}
	return 0
}

// stringOrParts decodes a content field that is either a string or an array of
// parts, returning the parts. A schema accepts both and clients send both
// (a prompt as a string, an image turn as an array).
func stringOrParts(raw json.RawMessage) (string, []json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", nil, nil
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return "", nil, ErrMalformed("content")
		}
		return s, nil, nil
	case '[':
		var parts []json.RawMessage
		if err := json.Unmarshal(trimmed, &parts); err != nil {
			return "", nil, ErrMalformed("content")
		}
		return "", parts, nil
	case '{':
		return "", []json.RawMessage{trimmed}, nil
	}
	return "", nil, nil
}

// stopList normalizes a stop field that is a string or an array of strings.
func stopList(obj map[string]json.RawMessage) []string {
	raw, ok := obj["stop"]
	if !ok {
		raw, ok = obj["stop_sequences"]
	}
	if !ok {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		out := make([]string, 0, len(list))
		for _, v := range list {
			if v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	return nil
}

// errUpstreamBody turns an upstream error body into a message a client can act
// on, without echoing a page of HTML and without truncating silently.
func errUpstreamBody(status int, body []byte) error {
	msg := upstreamMessage(body)
	if msg == "" {
		return fmt.Errorf("upstream answered %d", status)
	}
	return fmt.Errorf("upstream answered %d: %s", status, msg)
}

// upstreamMessage is the vendor's own words from an error body, never a page of
// HTML and never longer than 300 bytes.
func upstreamMessage(body []byte) string {
	obj, err := decodeObject(body)
	if err != nil {
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) == 0 || trimmed[0] == '<' {
			return ""
		}
		return truncate(string(trimmed), 300)
	}
	for _, path := range [][]string{
		{"error", "message"},
		{"message"},
		{"error"},
		{"detail"},
	} {
		v := nestedString(obj, path...)
		if v == "" {
			continue
		}
		if strings.HasPrefix(v, "<") {
			return ""
		}
		return truncate(v, 300)
	}
	return ""
}

func nestedString(obj map[string]json.RawMessage, path ...string) string {
	cur := obj
	for i, key := range path {
		raw, ok := cur[key]
		if !ok {
			return ""
		}
		if i == len(path)-1 {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				return strings.TrimSpace(s)
			}
			return ""
		}
		var next map[string]json.RawMessage
		if json.Unmarshal(raw, &next) != nil {
			return ""
		}
		cur = next
	}
	return ""
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// isDataLine reports whether a raw SSE line is a `data:` payload line and
// returns its value. An empty payload ("data:") returns "" with ok=true.
func isDataLine(line []byte) (payload string, ok bool) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return "", false
	}
	return strings.TrimSpace(string(trimmed[len("data:"):])), true
}

// ErrNoSchema is raised when a caller names a schema this plugin does not speak.
var ErrNoSchema = errors.New("unsupported schema")
