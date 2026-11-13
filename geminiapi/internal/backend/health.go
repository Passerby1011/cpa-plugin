package backend

// Health probing and the backend's own HTTP surface.
//
// # Two credential classes, never mixed
//
//   - baseURL + apiKey: the OpenAI-compatible surface (/v1/models, /v1/chat/...).
//     This is what requests use and what the plugin may echo into a response.
//   - origin + adminToken: the backend's own /admin/api. It manages the Google
//     account pool, so its responses can carry account material. Nothing from
//     this surface is ever logged or returned to a client; callers get counts
//     and booleans only.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultModelTimeout bounds catalog reads when the caller passes none.
const DefaultModelTimeout = 15 * time.Second

// HTTPDoer is the transport seam. Production wires it to the CPA host bridge so
// upstream calls appear in CPA's request log; tests inject a fake.
type HTTPDoer interface {
	Do(ctx context.Context, req HTTPRequest) (HTTPResponse, error)
}

// HTTPRequest is a minimal request shape (no auth headers are ever logged).
type HTTPRequest struct {
	Method  string
	URL     string
	Headers http.Header
	Body    []byte
}

// HTTPResponse is a fully-read response.
type HTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// Health states. They are strings so a management response can carry them
// verbatim without a second enum.
const (
	HealthUp          = "up"
	HealthDown        = "down"
	HealthForeign     = "foreign"
	HealthUnresponsive = "unresponsive"
)

// Health is the result of one probe.
type Health struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	// Version is what the backend reported, empty when unknown.
	Version string `json:"version,omitempty"`
	// Models lists model ids the backend advertised at "/" (best-effort).
	Models []string `json:"models,omitempty"`
	// APIKeyOK reports whether the setup's API key was accepted by
	// /v1/models. It is the only credential check that can distinguish "our
	// backend" from "something else on the port".
	APIKeyOK bool `json:"api_key_ok"`
}

// probeTimeout bounds each health read. A backend that cannot answer in this
// window is treated as unresponsive, which is a different signal from "down".
var probeTimeout = 4 * time.Second

// Probe asks the backend at setup.Origin how it is doing.
//
// A refused connection means "down" (nothing there). A response that is not the
// pinned backend, or one that rejects this setup's API key, means "foreign":
// somebody else owns the port, and the manager must not adopt or kill it.
func Probe(ctx context.Context, doer HTTPDoer, setup Setup) Health {
	if doer == nil {
		return Health{State: HealthDown, Reason: "no HTTP transport available"}
	}
	// 1. Root: version + model list. A backend that answers here is at least
	// speaking our protocol.
	rootReq := HTTPRequest{Method: http.MethodGet, URL: setup.Origin + "/"}
	resp, err := doWithTimeout(ctx, doer, rootReq, probeTimeout)
	if err != nil {
		if isConnectRefused(err) {
			return Health{State: HealthDown, Reason: "connection refused"}
		}
		return Health{State: HealthUnresponsive, Reason: redactTransportError(err)}
	}
	if resp.StatusCode != http.StatusOK {
		return Health{State: HealthForeign, Reason: fmt.Sprintf("GET %s returned HTTP %d", setup.Origin, resp.StatusCode)}
	}
	var rootBody struct {
		Version string   `json:"version"`
		Models  []string `json:"models"`
	}
	_ = json.Unmarshal(resp.Body, &rootBody)
	health := Health{State: HealthUp, Version: rootBody.Version, Models: rootBody.Models}

	// 2. /v1/models with this setup's key: the check that proves the listener
	// is OUR backend and not a stranger on the recorded port.
	modelReq := HTTPRequest{
		Method:  http.MethodGet,
		URL:     setup.BaseURL + "/models",
		Headers: bearerHeader(setup.APIKey),
	}
	modelResp, err := doWithTimeout(ctx, doer, modelReq, probeTimeout)
	if err != nil {
		health.State = HealthUnresponsive
		health.Reason = redactTransportError(err)
		return health
	}
	switch {
	case modelResp.StatusCode == http.StatusOK:
		health.APIKeyOK = true
	case modelResp.StatusCode == http.StatusUnauthorized || modelResp.StatusCode == http.StatusForbidden:
		health.State = HealthForeign
		health.Reason = fmt.Sprintf("%s/v1/models rejected this setup's API key (HTTP %d); another service may own port %s", setup.Origin, modelResp.StatusCode, portOf(setup.Origin))
		health.APIKeyOK = false
	default:
		health.State = HealthForeign
		health.Reason = fmt.Sprintf("%s/v1/models returned HTTP %d", setup.Origin, modelResp.StatusCode)
	}
	return health
}

// Model is one entry of the backend's /v1/models.
//
// The fields beyond ID/Object/OwnedBy are what both backends publish: AI Studio
// describes capabilities and per-model thinking levels, gemini-web2api-go
// describes token limits. Unknown fields are ignored deliberately — the catalog
// must keep working when a backend adds one.
type Model struct {
	ID     string `json:"id"`
	Object string `json:"object"`
	OwnedBy string `json:"owned_by"`

	// Capabilities is AI Studio's capability map.
	Capabilities map[string]any `json:"capabilities"`
	// ThinkingLevels lists the effort levels the model supports.
	ThinkingLevels []string `json:"thinking_levels"`
	// SupportedGenerationMethods is the Gemini "generateContent" list.
	SupportedGenerationMethods []string `json:"supported_generation_methods"`
	// Channels lists which enabled accounts cover the model (AI Studio).
	Channels []string `json:"channels"`
	// CapabilityOptions carries per-capability settings such as aspect ratios.
	CapabilityOptions map[string]any `json:"capability_options"`

	InputTokenLimit  int64 `json:"input_token_limit"`
	OutputTokenLimit int64 `json:"output_token_limit"`
}

// ModelList is the decoded /v1/models response.
type ModelList struct {
	Data []Model `json:"data"`
}

// FetchModels reads the backend's model catalog. It is used by the catalog
// manager and by the Management API's "refresh models" action.
func FetchModels(ctx context.Context, doer HTTPDoer, setup Setup, timeout time.Duration) (ModelList, error) {
	if timeout <= 0 {
		timeout = DefaultModelTimeout
	}
	req := HTTPRequest{Method: http.MethodGet, URL: setup.BaseURL + "/models", Headers: bearerHeader(setup.APIKey)}
	resp, err := doWithTimeout(ctx, doer, req, timeout)
	if err != nil {
		return ModelList{}, fmt.Errorf("fetch models from %s: %w", setup.Origin, err)
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return ModelList{}, fmt.Errorf("backend rejected the API key (HTTP %d)", resp.StatusCode)
		}
		return ModelList{}, fmt.Errorf("backend returned HTTP %d for /v1/models", resp.StatusCode)
	}
	var list ModelList
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		return ModelList{}, fmt.Errorf("decode /v1/models response: %w", err)
	}
	if list.Data == nil {
		return ModelList{}, fmt.Errorf("/v1/models response has no data array")
	}
	return list, nil
}

// AdminCall is one request to the backend's own /admin/api. The body and the
// response are account material: callers must never log or return them.
//
// timeoutMessage replaces the transport error when a request timed out, because
// the operator-facing meaning is "the backend was reached but the work it does
// (a Google sign-in check) took too long".
func AdminCall(ctx context.Context, doer HTTPDoer, setup Setup, method, path string, body any, timeout time.Duration, timeoutMessage string) (json.RawMessage, error) {
	if setup.AdminToken == "" {
		return nil, errors.New("this backend has no admin token; account import needs a plugin-managed backend")
	}
	var payload []byte
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode admin request: %w", err)
		}
		payload = raw
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+setup.AdminToken)
	if payload != nil {
		headers.Set("Content-Type", "application/json")
	}
	req := HTTPRequest{
		Method:  strings.ToUpper(method),
		URL:     setup.Origin + "/admin/api" + strings.TrimPrefix(path, "/"),
		Headers: headers,
		Body:    payload,
	}
	resp, err := doWithTimeout(ctx, doer, req, timeout)
	if err != nil {
		if isTimeoutError(err) && timeoutMessage != "" {
			return nil, errors.New(timeoutMessage)
		}
		return nil, fmt.Errorf("cannot reach the local backend at %s: %s", setup.Origin, redactTransportError(err))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The backend's error text describes the request, not credentials, but
		// it can echo a label. Truncate and never include the body verbatim.
		detail := truncate(strings.TrimSpace(string(resp.Body)), 200)
		if detail == "" {
			detail = "unknown error"
		}
		return nil, fmt.Errorf("backend rejected the admin request (HTTP %d): %s", resp.StatusCode, detail)
	}
	if len(resp.Body) == 0 {
		return json.RawMessage("{}"), nil
	}
	return json.RawMessage(resp.Body), nil
}

func bearerHeader(key string) http.Header {
	headers := http.Header{}
	if strings.TrimSpace(key) != "" {
		headers.Set("Authorization", "Bearer "+key)
	}
	headers.Set("Accept", "application/json")
	return headers
}

func doWithTimeout(parent context.Context, doer HTTPDoer, req HTTPRequest, timeout time.Duration) (HTTPResponse, error) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return doer.Do(ctx, req)
}

func portOf(origin string) string {
	if parsed, err := url.Parse(origin); err == nil {
		return parsed.Port()
	}
	return "?"
}

// isConnectRefused distinguishes "nothing is listening" from any other failure.
// The host bridge wraps errors, so the check is textual as well as typed.
func isConnectRefused(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{"connection refused", "actively refused", "connectex", "no such host"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	var netErr *url.Error
	if errors.As(err, &netErr) {
		return strings.Contains(strings.ToLower(netErr.Err.Error()), "refused")
	}
	return false
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "deadline exceeded") ||
		strings.Contains(text, "timed out") ||
		strings.Contains(text, "timeout")
}

// redactTransportError keeps an error's meaning while dropping anything that
// could carry a credential. URLs are never echoed (a query could hold one) and
// the message is truncated.
func redactTransportError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// Strip a scheme://host/path prefix so only the failure class remains.
	if idx := strings.Index(msg, "http://"); idx >= 0 {
		msg = msg[:idx] + "…" + msg[idx:]
	}
	return truncate(msg, 240)
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
