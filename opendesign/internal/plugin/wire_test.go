package plugin

import (
	"net/http"
	"testing"

	"github.com/hex-ci/cpa-plugin/opendesign/internal/protocol"
)

// The signature must follow the PATH, not the model's declared API: amr-link
// serves every model on all three, and the key rides as the path takes it.
func TestSignRequestFollowsThePath(t *testing.T) {
	cases := []struct {
		path       string
		wantBearer bool
	}{
		{"/v1/chat/completions", true},
		{"/v1/responses", true},
		{"/v1/messages", false},
		{"/v1/messages/", false},
		{"/v1/messages/count_tokens", false},
		{"/messages", false},
	}
	for _, tc := range cases {
		headers := http.Header{}
		signRequest(headers, tc.path, "sk-amr-key")
		if tc.wantBearer {
			if got := headers.Get("Authorization"); got != "Bearer sk-amr-key" {
				t.Errorf("%s: Authorization = %q, want a Bearer token", tc.path, got)
			}
			if got := headers.Get("X-Api-Key"); got != "" {
				t.Errorf("%s: x-api-key must not be set, got %q", tc.path, got)
			}
			continue
		}
		if got := headers.Get("X-Api-Key"); got != "sk-amr-key" {
			t.Errorf("%s: x-api-key = %q, want the key", tc.path, got)
		}
		if got := headers.Get("Authorization"); got != "" {
			t.Errorf("%s: Authorization must not be set, got %q", tc.path, got)
		}
		if got := headers.Get("Anthropic-Version"); got != protocol.AnthropicVersion {
			t.Errorf("%s: anthropic-version = %q, want %q", tc.path, got, protocol.AnthropicVersion)
		}
	}
}

// A client's own anthropic-version must not be overwritten.
func TestSignRequestKeepsTheClientsAnthropicVersion(t *testing.T) {
	headers := http.Header{}
	headers.Set("Anthropic-Version", "2024-01-01")
	signRequest(headers, "/v1/messages", "sk-amr-key")
	if got := headers.Get("Anthropic-Version"); got != "2024-01-01" {
		t.Errorf("version = %q, want the client's", got)
	}
}

// Cross-signing must clean the other scheme's header off, or a request to
// /messages would carry both a Bearer and an x-api-key.
func TestSignRequestClearsTheOtherScheme(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Api-Key", "stale")
	headers.Set("Authorization", "Bearer stale")
	signRequest(headers, "/v1/chat/completions", "sk-amr-key")
	if headers.Get("X-Api-Key") != "" {
		t.Errorf("a chat request still carries x-api-key")
	}
	if got := headers.Get("Authorization"); got != "Bearer sk-amr-key" {
		t.Errorf("authorization = %q", got)
	}
}

// The endpoint is the model plane's base plus the schema's own path.
func TestEndpointURL(t *testing.T) {
	cases := map[protocol.Schema]string{
		protocol.SchemaChat:      "https://amr-link.open-design.ai/v1/chat/completions",
		protocol.SchemaClaude:    "https://amr-link.open-design.ai/v1/messages",
		protocol.SchemaResponses: "https://amr-link.open-design.ai/v1/responses",
	}
	for schema, want := range cases {
		if got := endpointURL("https://amr-link.open-design.ai/v1/", schema); got != want {
			t.Errorf("%s: url = %q, want %q", schema, got, want)
		}
	}
}
