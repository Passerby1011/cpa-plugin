package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeChat is a test helper that fails the test on a decode error.
func mustDecodeChat(t *testing.T, body string) *Request {
	t.Helper()
	req, err := DecodeChatRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeChatRequest: %v", err)
	}
	return req
}

func TestDecodeChatRequestKeepsSystemAndTurns(t *testing.T) {
	req := mustDecodeChat(t, "{\"model\":\"m\",\"messages\":["+
		"{\"role\":\"system\",\"content\":\"be terse\"},"+
		"{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":16}")
	if req.System != "be terse" {
		t.Errorf("system = %q, want %q", req.System, "be terse")
	}
	if len(req.Turns) != 1 || req.Turns[0].Role != RoleUser {
		t.Fatalf("turns = %+v", req.Turns)
	}
	if got := req.Turns[0].Text(); got != "hi" {
		t.Errorf("user text = %q", got)
	}
	if req.MaxOutput != 16 {
		t.Errorf("max output = %d", req.MaxOutput)
	}
}

// The three schemas put a model's reasoning in three different places; this is
// the test that pins the chat side of that contract.
func TestDecodeChatResponseKeepsReasoningBeforeAnswer(t *testing.T) {
	body := `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"answer","reasoning_content":"pondering"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	resp, err := DecodeChatResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeChatResponse: %v", err)
	}
	if len(resp.Parts) < 2 {
		t.Fatalf("parts = %+v", resp.Parts)
	}
	if resp.Parts[0].Kind != PartThinking || resp.Parts[0].Text != "pondering" {
		t.Errorf("first part = %+v, want the thought first", resp.Parts[0])
	}
	if resp.Parts[1].Kind != PartText || resp.Parts[1].Text != "answer" {
		t.Errorf("second part = %+v, want the answer", resp.Parts[1])
	}
	if resp.StopReason != StopEnd {
		t.Errorf("stop = %q", resp.StopReason)
	}
}

func TestDecodeChatToolCallArgumentsStayVerbatim(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[` +
		`{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":"tool_calls"}]}`
	resp, err := DecodeChatResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeChatResponse: %v", err)
	}
	calls := resp.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("tool calls = %+v", calls)
	}
	if calls[0].Text != `{"city":"SF"}` {
		t.Errorf("arguments = %q, want the upstream text unchanged", calls[0].Text)
	}
	if resp.StopReason != StopToolUse {
		t.Errorf("stop = %q", resp.StopReason)
	}
}

// A chat -> Claude crossing must carry the thought into an assistant thinking
// block, not drop it.
func TestChatResponseToClaudeKeepsThinking(t *testing.T) {
	chatBody := `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"answer","reasoning_content":"pondering"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	resp, err := DecodeChatResponse([]byte(chatBody))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := EncodeClaudeResponse(resp, "m")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var doc struct {
		Content []struct {
			Type     string `json:"type"`
			Thinking string `json:"thinking"`
			Text     string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Content) != 2 {
		t.Fatalf("content = %+v", doc.Content)
	}
	if doc.Content[0].Type != "thinking" || doc.Content[0].Thinking != "pondering" {
		t.Errorf("block 0 = %+v, want a thinking block", doc.Content[0])
	}
	if doc.Content[1].Type != "text" || doc.Content[1].Text != "answer" {
		t.Errorf("block 1 = %+v", doc.Content[1])
	}
}

// The other direction: a Claude thinking block must land in reasoning_content.
func TestClaudeResponseToChatKeepsThinking(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"thinking","thinking":"pondering"},{"type":"text","text":"answer"}],` +
		`"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
	resp, err := DecodeClaudeResponse([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := EncodeChatResponse(resp, "m")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	message := doc["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if got, _ := message["reasoning_content"].(string); got != "pondering" {
		t.Errorf("reasoning_content = %q, want %q", got, "pondering")
	}
	if got, _ := message["content"].(string); got != "answer" {
		t.Errorf("content = %q", got)
	}
}

// A framed messages body — what amr-link actually answers a non-streaming
// request with — must decode as the message it describes.
func TestDecodeClaudeResponseReadsFramedBody(t *testing.T) {
	body := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\",\"usage\":{\"input_tokens\":3}}}\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"pondering\"}}\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n" +
		"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":1}\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n" +
		"data: {\"type\":\"message_stop\"}\n"
	resp, err := DecodeClaudeResponse([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Thinking() != "pondering" {
		t.Errorf("thinking = %q", resp.Thinking())
	}
	if resp.Text() != "answer" {
		t.Errorf("text = %q", resp.Text())
	}
	if resp.StopReason != StopEnd {
		t.Errorf("stop = %q", resp.StopReason)
	}
	if resp.Usage.Input != 3 || resp.Usage.Output != 2 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

// A Responses reasoning item must survive into a Claude thinking block and into
// a chat reasoning_content.
func TestResponsesReasoningToBothTargets(t *testing.T) {
	body := `{"id":"resp_1","object":"response","created_at":1,"model":"m","status":"completed","output":[` +
		`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"pondering"}]},` +
		`{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"answer"}]}],` +
		`"usage":{"input_tokens":3,"output_tokens":2}}`
	resp, err := DecodeResponsesResponse([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Thinking() != "pondering" || resp.Text() != "answer" {
		t.Fatalf("parts = %+v", resp.Parts)
	}
	chatOut, err := EncodeChatResponse(resp, "m")
	if err != nil {
		t.Fatalf("chat encode: %v", err)
	}
	if !strings.Contains(string(chatOut), `"reasoning_content":"pondering"`) {
		t.Errorf("chat body lost the thought: %s", chatOut)
	}
	claudeOut, err := EncodeClaudeResponse(resp, "m")
	if err != nil {
		t.Fatalf("claude encode: %v", err)
	}
	if !strings.Contains(string(claudeOut), `"type":"thinking"`) {
		t.Errorf("claude body lost the thought: %s", claudeOut)
	}
}

// A request asking for a thinking budget on Claude must reach a Responses target
// as an effort level, and vice versa.
func TestEncodeResponsesReasoningWritesEffort(t *testing.T) {
	req := &Request{Effort: "high", Turns: []Turn{{Role: RoleUser, Parts: []Part{{Kind: PartText, Text: "hi"}}}}}
	body, err := EncodeResponsesRequest(req, "m", false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := EncodeResponsesReasoning(body, req.Effort, []string{"low", "high", "max"})
	if err != nil {
		t.Fatalf("reasoning: %v", err)
	}
	if !strings.Contains(string(out), `"reasoning":{"effort":"high"}`) {
		t.Errorf("body = %s", out)
	}
}

func TestEncodeChatReasoningOmitsControlForLevelLessModel(t *testing.T) {
	req := &Request{Effort: "high", Turns: []Turn{{Role: RoleUser, Parts: []Part{{Kind: PartText, Text: "hi"}}}}}
	body, err := EncodeChatRequest(req, "m", false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := EncodeChatReasoning(body, req.Effort, nil)
	if err != nil {
		t.Fatalf("reasoning: %v", err)
	}
	if strings.Contains(string(out), "reasoning_effort") {
		t.Errorf("a model with no levels got a reasoning_effort: %s", out)
	}
}

func TestEncodeClaudeRequestDefaultsMaxTokens(t *testing.T) {
	req := &Request{Turns: []Turn{{Role: RoleUser, Parts: []Part{{Kind: PartText, Text: "hi"}}}}}
	body, err := EncodeClaudeRequest(req, "m", false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(body), `"max_tokens"`) {
		t.Errorf("Anthropic requires max_tokens: %s", body)
	}
}

// A tool result must land where each schema takes it: a tool role for chat, a
// user turn holding a tool_result block for Claude, a function_call_output item
// for Responses.
func TestToolResultsLandInEachSchemasShape(t *testing.T) {
	req := &Request{Turns: []Turn{
		{Role: RoleAssistant, Parts: []Part{{Kind: PartToolCall, ToolCallID: "call_1", ToolName: "get_weather", Text: `{"city":"SF"}`}}},
		{Role: RoleTool, Parts: []Part{{Kind: PartToolResult, ToolCallID: "call_1", Text: "sunny"}}},
	}}
	chatBody, err := EncodeChatRequest(req, "m", false)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if !strings.Contains(string(chatBody), `"role":"tool"`) || !strings.Contains(string(chatBody), `"tool_call_id":"call_1"`) {
		t.Errorf("chat body = %s", chatBody)
	}
	claudeBody, err := EncodeClaudeRequest(req, "m", false)
	if err != nil {
		t.Fatalf("claude: %v", err)
	}
	if !strings.Contains(string(claudeBody), `"type":"tool_result"`) || !strings.Contains(string(claudeBody), `"tool_use_id":"call_1"`) {
		t.Errorf("claude body = %s", claudeBody)
	}
	respBody, err := EncodeResponsesRequest(req, "m", false)
	if err != nil {
		t.Fatalf("responses: %v", err)
	}
	if !strings.Contains(string(respBody), `"type":"function_call_output"`) {
		t.Errorf("responses body = %s", respBody)
	}
}
