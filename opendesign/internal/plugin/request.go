// Package plugin — the executor's request shaping.
//
// A request arrives in the client's own schema (chat completions, Claude
// messages, or Responses) and must be sent to the model on the schema the model
// is DECLARED on. The three wire concerns that differ:
//
//	path              /chat/completions, /messages, /responses
//	signature         /messages takes x-api-key + anthropic-version; the other
//	                  two take a Bearer token. This follows the PATH, not the
//	                  model's declared API: amr-link serves every model on all
//	                  three, and the key rides as the path it was asked on takes
//	                  it (magpie plugins#888).
//	thinking control  chat: reasoning_effort · messages: thinking.budget_tokens
//	                  · responses: reasoning.effort. A model with no declared
//	                  levels gets NONE of them, which is the only way to keep
//	                  GPT-6 Luna from being asked to think at a level it
//	                  ignores.
package plugin

import (
	"github.com/hex-ci/cpa-plugin/opendesign/internal/catalog"
	"github.com/hex-ci/cpa-plugin/opendesign/internal/modelcaps"
	"github.com/hex-ci/cpa-plugin/opendesign/internal/protocol"
	"github.com/hex-ci/cpa-plugin/opendesign/internal/thinking"
)

// schemaForAPI maps a model's declared API onto a protocol schema.
func schemaForAPI(api modelcaps.API) protocol.Schema {
	switch api {
	case modelcaps.APIMessages:
		return protocol.SchemaClaude
	case modelcaps.APIResponses:
		return protocol.SchemaResponses
	default:
		return protocol.SchemaChat
	}
}

// effortFor derives the reasoning level to send for one entry.
//
// An explicit level from the client wins. A Claude budget (the only control that
// schema has) is converted to the model's own nearest level, because a model
// with no level vocabulary takes no level at all and sending one is a control
// the model ignores.
func effortFor(entry catalog.Entry, req *protocol.Request) string {
	levels := thinking.NormalizeLevels(entry.Efforts)
	if len(levels) == 0 {
		return ""
	}
	if req.Effort != "" {
		return thinking.ClampToSupported(req.Effort, levels)
	}
	if req.Budget > 0 {
		return thinking.EffortFromBudget(req.Budget, levels)
	}
	return ""
}

// budgetFor derives the thinking budget to send for one entry.
func budgetFor(entry catalog.Entry, req *protocol.Request) int64 {
	levels := thinking.NormalizeLevels(entry.Efforts)
	if len(levels) == 0 {
		// No levels means no thinking: a budget here would only spend tokens on
		// a model that never thinks.
		return 0
	}
	if req.Budget > 0 {
		return req.Budget
	}
	if effort := effortFor(entry, req); effort != "" {
		if budget, ok := thinking.BudgetFromEffort(effort, levels); ok {
			return budget
		}
	}
	return 0
}

// maxOutputFor is the output budget to shape the request with: the client's own,
// capped where amr-link takes less than its catalog publishes, else the entry's.
func maxOutputFor(entry catalog.Entry, req *protocol.Request) int64 {
	limit := req.MaxOutput
	if limit <= 0 {
		limit = entry.Output
	}
	return modelcaps.CapOutput(entry.UpstreamID, limit)
}

// buildUpstream renders the canonical request as the body the entry's own schema
// takes, including that schema's thinking control.
func buildUpstream(entry catalog.Entry, req *protocol.Request) (protocol.Schema, []byte, error) {
	schema := schemaForAPI(entry.API)
	stream := req.Stream
	switch schema {
	case protocol.SchemaClaude:
		body, err := protocol.EncodeClaudeRequest(req, entry.UpstreamID, stream)
		if err != nil {
			return schema, nil, err
		}
		body, err = protocol.EncodeClaudeThinking(body, budgetFor(entry, req), entry.Efforts, maxOutputFor(entry, req))
		return schema, body, err
	case protocol.SchemaResponses:
		body, err := protocol.EncodeResponsesRequest(req, entry.UpstreamID, stream)
		if err != nil {
			return schema, nil, err
		}
		body, err = protocol.EncodeResponsesReasoning(body, effortFor(entry, req), entry.Efforts)
		return schema, body, err
	default:
		body, err := protocol.EncodeChatRequest(req, entry.UpstreamID, stream)
		if err != nil {
			return schema, nil, err
		}
		body, err = protocol.EncodeChatReasoning(body, effortFor(entry, req), entry.Efforts)
		return schema, body, err
	}
}

// decodeInbound decodes a client body in the schema it arrived in.
func decodeInbound(source protocol.Schema, body []byte) (*protocol.Request, error) {
	switch source {
	case protocol.SchemaClaude:
		return protocol.DecodeClaudeRequest(body)
	case protocol.SchemaResponses:
		return protocol.DecodeResponsesRequest(body)
	default:
		return protocol.DecodeChatRequest(body)
	}
}

// convertResponse renders one upstream response in the client's schema.
func convertResponse(upstreamSchema, clientSchema protocol.Schema, body []byte, model string) ([]byte, error) {
	resp, err := decodeUpstreamResponse(upstreamSchema, body)
	if err != nil {
		return nil, err
	}
	return encodeClientResponse(clientSchema, resp, model)
}

func decodeUpstreamResponse(schema protocol.Schema, body []byte) (*protocol.Response, error) {
	switch schema {
	case protocol.SchemaClaude:
		return protocol.DecodeClaudeResponse(body)
	case protocol.SchemaResponses:
		return protocol.DecodeResponsesResponse(body)
	default:
		return protocol.DecodeChatResponse(body)
	}
}

func encodeClientResponse(schema protocol.Schema, resp *protocol.Response, model string) ([]byte, error) {
	switch schema {
	case protocol.SchemaClaude:
		return protocol.EncodeClaudeResponse(resp, model)
	case protocol.SchemaResponses:
		return protocol.EncodeResponsesResponse(resp, model)
	default:
		return protocol.EncodeChatResponse(resp, model)
	}
}
