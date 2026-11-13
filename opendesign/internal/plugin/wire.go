// Package plugin — signing, URLs and upstream error classification.
package plugin

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/hex-ci/cpa-plugin/opendesign/internal/protocol"
)

// messagesPath matches the two Anthropic paths, with or without a trailing slash
// and with the count_tokens variant.
var messagesPath = regexp.MustCompile(`/messages(/count_tokens)?/?$`)

// endpointURL is the absolute upstream URL for one schema.
//
// base-url is the model plane's /v1 (https://amr-link.open-design.ai/v1), so the
// chat and Responses paths hang off it directly while /messages — which lives at
// the host's own root in anthropic's own routing — is still reached through /v1,
// which is where amr-link serves it.
func endpointURL(baseURL string, target protocol.Schema) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return base + target.Path()
}

// signRequest sets the credential header THE PATH takes.
//
// This is deliberate and is the one place the plugin apes the vendor rather than
// choosing: amr-link serves all three APIs for every model, so a request can
// reach /messages for a model declared on chat completions, and the key rides as
// the path takes it — x-api-key (+ anthropic-version) on /messages, Bearer
// everywhere else. Signing by the model's declared API instead would send the
// wrong header on the paths a client actually chose (magpie plugins#888).
func signRequest(headers http.Header, path, key string) {
	key = strings.TrimSpace(key)
	if messagesPath.MatchString(path) {
		headers.Del("Authorization")
		headers.Set("X-Api-Key", key)
		if headers.Get("Anthropic-Version") == "" {
			headers.Set("Anthropic-Version", protocol.AnthropicVersion)
		}
		return
	}
	headers.Del("X-Api-Key")
	headers.Del("Anthropic-Version")
	headers.Set("Authorization", "Bearer "+key)
}

// upstreamHeaders builds the full header set for one upstream call.
func upstreamHeaders(target protocol.Schema, path, key string, stream bool) http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	if stream {
		headers.Set("Accept", "text/event-stream")
	} else {
		headers.Set("Accept", "application/json")
	}
	signRequest(headers, path, key)
	return headers
}

// headerMap converts a header set into the plain map the bridge takes.
func headerMap(headers http.Header) map[string]string {
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		if len(v) == 0 {
			continue
		}
		out[k] = v[0]
	}
	return out
}
