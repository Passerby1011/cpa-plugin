// Package protocol — small helpers shared by the three codecs.
package protocol

import (
	"encoding/json"
	"strings"
)

// ptrField reads a nullable float field.
func ptrField(obj map[string]json.RawMessage, name string) *float64 {
	v, ok := floatField(obj, name)
	if !ok {
		return nil
	}
	return v
}

// normalizeEffortName lowercases an effort label and maps the vendor-neutral
// spellings onto the canonical names.
func normalizeEffortName(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "", "none", "off", "disabled":
		return ""
	case "minimal", "low", "medium", "high", "xhigh", "max":
		return strings.ToLower(strings.TrimSpace(effort))
	case "auto":
		return "auto"
	}
	return strings.ToLower(strings.TrimSpace(effort))
}

// splitDataURL separates a data: URI's media type from its payload. A plain URL
// returns "" for the media type.
func splitDataURL(url string) (payload, mediaType string) {
	if !strings.HasPrefix(url, "data:") {
		return url, ""
	}
	comma := strings.Index(url, ",")
	if comma < 0 {
		return url, ""
	}
	head := url[len("data:"):comma]
	semicolon := strings.Index(head, ";")
	if semicolon < 0 {
		return url, head
	}
	return url, head[:semicolon]
}

// defaultToolSchema is what a tool with no declared parameters gets. The three
// schemas all require a schema object; an empty one is rejected by some
// upstreams.
var defaultToolSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// schemaOrDefault returns the tool's schema, or the empty-object schema.
func schemaOrDefault(schema json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(schema))
	if trimmed == "" || trimmed == "null" {
		return defaultToolSchema
	}
	return schema
}

// argsOrEmptyObject returns a tool call's arguments as a JSON string, defaulting
// to "{}" so a schema that requires an object still parses.
func argsOrEmptyObject(args string) string {
	if strings.TrimSpace(args) == "" {
		return "{}"
	}
	return args
}

// toolResultParts returns a turn's tool-result parts.
func toolResultParts(turn Turn) []Part {
	var out []Part
	for _, p := range turn.Parts {
		if p.Kind == PartToolResult {
			out = append(out, p)
		}
	}
	return out
}

// nonEmpty returns first when it is non-empty, else fallback.
func nonEmpty(first, fallback string) string {
	if strings.TrimSpace(first) != "" {
		return first
	}
	return fallback
}
