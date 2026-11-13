package backend

// Health probing and the backend's own HTTP surface.
//
// # Two credential classes, never mixed
//
//	geminiapi key      – the OpenAI-compatible surface (/v1/models,
//	                     /v1/chat/completions). What requests use, and what
//	                     a health probe may echo into a reason string.
//	admin credential   – the backend's own management surface. gemini-web
//	                     takes a bearer admin token under /admin/api; AI
//	                     Studio takes a session cookie under /api. Either one
//	                     manages the Google account pool, so nothing from that
//	                     surface is ever logged or returned to a client:
//	                     callers get counts and booleans only.
//
// # Why the probe differs per flavor
//
// The check that proves "this listener is OUR backend" is the same idea in both
// cases — send our own key and see whether it is accepted — but the reachability
// proof is not: gemini-web2api-go answers "/" with a version document, while
// AIStudio2API answers only on its admin surface until it is ready. Treating one
// like the other would report a healthy AI Studio service as "foreign".

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
	HealthUp           = "up"
	HealthDown         = "down"
	HealthForeign      = "foreign"
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

// HasAdminCredentials reports whether this setup can drive its backend's own
// admin surface at all.
func (s Setup) HasAdminCredentials() bool {
	if s.AdminToken != "" {
		return true
	}
	return s.AdminUser != "" && s.AdminPassword != ""
}

// Probe asks the backend at setup.Origin how it is doing.
//
// A refused connection means "down" (nothing there). A response that is not the
// pinned backend, or one that rejects this setup's API key, means "foreign":
// somebody else owns the port, and the manager must not adopt or kill it.
//
// The final authority in every flavor is /v1/models with this setup's key, because
// that is the surface requests actually use: a listener that accepts our key is
// ours whatever else it says, and one that refuses it is not ours whatever else
// it says.
func Probe(ctx context.Context, doer HTTPDoer, setup Setup) Health {
	if doer == nil {
		return Health{State: HealthDown, Reason: "no HTTP transport available"}
	}
	// 1. Is anything there at all? A refused connection is the only
	// unambiguous "down" signal, and checking it first keeps a hanging
	// listener from being reported as ours.
	if host, err := probeReachable(ctx, doer, setup); err != nil || host == nil {
		if err == nil {
			err = errString("no answer")
		}
		if isConnectRefused(err) {
			return Health{State: HealthDown, Reason: "connection refused"}
		}
		return Health{State: HealthUnresponsive, Reason: redactTransportError(err)}
	} else {
		health := Health{State: HealthUp, Version: host.version, Models: host.models}
		// 2. /v1/models with this setup's key: the check that proves the
		// listener is OUR backend and not a stranger on the recorded port.
		modelReq := HTTPRequest{
			Method:  http.MethodGet,
			URL:     setup.BaseURL + "/models",
			Headers: bearerHeader(setup.APIKey),
		}
		modelResp, err := doWithTimeout(ctx, doer, modelReq, probeTimeout)
		if err != nil {
			if isConnectRefused(err) {
				return Health{State: HealthDown, Reason: "connection refused"}
			}
			health.State = HealthUnresponsive
			health.Reason = redactTransportError(err)
			return health
		}
		switch {
		case modelResp.StatusCode == http.StatusOK:
			health.APIKeyOK = true
		case modelResp.StatusCode == http.StatusUnauthorized || modelResp.StatusCode == http.StatusForbidden:
			health.State = HealthForeign
			health.Reason = fmt.Sprintf("%s/v1/models rejected this setup's API key (HTTP %d); another service may own port %s",
				setup.Origin, modelResp.StatusCode, portOf(setup.Origin))
			health.APIKeyOK = false
		default:
			health.State = HealthForeign
			health.Reason = fmt.Sprintf("%s/v1/models returned HTTP %d", setup.Origin, modelResp.StatusCode)
		}
		return health
	}
}

// hostInfo is what a flavor's reachability probe learned.
type hostInfo struct {
	version string
	models  []string
}

// probeReachable asks the flavor-specific "are you there" question. It returns
// errString("no answer") when the backend answered nothing recognisable but did
// not refuse the connection either.
func probeReachable(ctx context.Context, doer HTTPDoer, setup Setup) (*hostInfo, error) {
	switch setup.Flavor {
	case FlavorAIStudio:
		// AIStudio2API's only always-on surface is its admin API: GET
		// /api/status returns ready/running/accounts/version. It needs a
		// session, which a 401 answers — and that is still proof the
		// service is there, so 401 is treated as reachable.
		req := HTTPRequest{Method: http.MethodGet, URL: setup.Origin + "/api/status"}
		resp, err := doWithTimeout(ctx, doer, req, probeTimeout)
		if err != nil {
			return nil, err
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			var status struct {
				Version string `json:"version"`
			}
			_ = json.Unmarshal(resp.Body, &status)
			return &hostInfo{version: status.Version}, nil
		case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
			// The admin surface exists but wants its cookie: reachable.
			return &hostInfo{}, nil
		case resp.StatusCode >= 500:
			return nil, fmt.Errorf("admin status returned HTTP %d", resp.StatusCode)
		default:
			return nil, fmt.Errorf("GET %s/api/status returned HTTP %d", setup.Origin, resp.StatusCode)
		}
	default:
		// gemini-web2api-go answers "/" with its version and model list.
		req := HTTPRequest{Method: http.MethodGet, URL: setup.Origin + "/"}
		resp, err := doWithTimeout(ctx, doer, req, probeTimeout)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s returned HTTP %d", setup.Origin, resp.StatusCode)
		}
		var root struct {
			Version string   `json:"version"`
			Models  []string `json:"models"`
		}
		_ = json.Unmarshal(resp.Body, &root)
		return &hostInfo{version: root.Version, models: root.Models}, nil
	}
}

// errString is an error carrying a fixed message.
type errString string

func (e errString) Error() string { return string(e) }

// Model is one entry of the backend's /v1/models.
//
// The fields beyond ID/Object/OwnedBy are what either backend may publish:
// AI Studio describes capabilities and per-model thinking levels, gemini-web
// describes token limits and nothing else. Unknown fields are ignored
// deliberately — the catalog must keep working when a backend adds one.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`

	// Capabilities is AI Studio's capability map. gemini-web2api-go does not
	// publish one at all, which is why the gemini-web catalog rules may not
	// depend on it.
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
//
// An empty list is reported as an ERROR rather than an empty result: a catalog
// with zero models publishes nothing, and a caller that cannot tell "the backend
// said nothing" from "the backend has no models" will show an empty page with no
// reason on it. The one exception is a backend that has no account yet — that
// answers with an empty data array, and the error says so.
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
	if len(list.Data) == 0 {
		return ModelList{}, errors.New("/v1/models returned an empty data array: the backend has no model to publish " +
			"(usually no usable account imported yet)")
	}
	return list, nil
}

// AdminCall is one request to the backend's own management surface.
//
// The path is flavor-relative: gemini-web's "/cookies" becomes
// <origin>/admin/api/cookies, AI Studio's "/api/status" is used verbatim under
// <origin>. Both surfaces manage the Google account pool, so their bodies and
// responses are account material: callers must never log or return them.
//
// timeoutMessage replaces the transport error when a request timed out, because
// the operator-facing meaning is "the backend was reached but the work it does
// (a Google sign-in check) took too long".
func AdminCall(ctx context.Context, doer HTTPDoer, setup Setup, method, path string, body any, timeout time.Duration, timeoutMessage string) (json.RawMessage, error) {
	if setup.AdminPassword != "" && setup.AdminUser != "" {
		return studioAdminCall(ctx, doer, setup, method, path, body, timeout, timeoutMessage)
	}
	return geminiWebAdminCall(ctx, doer, setup, method, path, body, timeout, timeoutMessage)
}

// AdminURL is the absolute URL one admin call would use, so a caller can report
// which surface it talked to without leaking a credential.
func AdminURL(setup Setup, path string) string {
	if setup.AdminPassword != "" && setup.AdminUser != "" {
		return setup.Origin + normalizeStudioPath(path)
	}
	return setup.Origin + "/admin/api" + strings.TrimPrefix(path, "/")
}

// normalizeStudioPath makes a path absolute under the origin.
func normalizeStudioPath(path string) string {
	if strings.HasPrefix(path, "/") {
		return path
	}
	return "/" + path
}

// geminiWebAdminCall is one request to gemini-web2api-go's /admin/api, which
// authenticates with a bearer admin token.
func geminiWebAdminCall(ctx context.Context, doer HTTPDoer, setup Setup, method, path string, body any, timeout time.Duration, timeoutMessage string) (json.RawMessage, error) {
	if setup.AdminToken == "" {
		return nil, errors.New("this backend has no admin token; account import needs a plugin-managed backend")
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+setup.AdminToken)
	return adminRoundTrip(ctx, doer, setup.Origin+"/admin/api"+strings.TrimPrefix(path, "/"),
		method, body, headers, timeout, timeoutMessage, setup)
}

// AdminLoginTimeout bounds the session login a studio call may need.
var adminLoginTimeout = 10 * time.Second

// studioAdminCall is one request to AIStudio2API's /api, which authenticates
// with a session cookie obtained from /api/auth/login.
//
// The login is performed per call rather than cached: this plugin talks to the
// backend rarely (imports, status, counts), a stale cached session would make a
// restarted service look unauthenticated, and caching a cookie across calls
// would put a live session in plugin memory for no benefit.
func studioAdminCall(ctx context.Context, doer HTTPDoer, setup Setup, method, path string, body any, timeout time.Duration, timeoutMessage string) (json.RawMessage, error) {
	if setup.AdminUser == "" || setup.AdminPassword == "" {
		return nil, errors.New("this backend has no admin credentials; account import needs a plugin-managed backend")
	}
	loginBody, err := json.Marshal(map[string]string{
		"username": setup.AdminUser,
		"password": setup.AdminPassword,
	})
	if err != nil {
		return nil, errors.New("encode admin login: failed")
	}
	loginResp, err := doWithTimeout(ctx, doer, HTTPRequest{
		Method:  http.MethodPost,
		URL:     setup.Origin + "/api/auth/login",
		Headers: jsonHeader(),
		Body:    loginBody,
	}, adminLoginTimeout)
	if err != nil {
		if isTimeoutError(err) && timeoutMessage != "" {
			return nil, errors.New(timeoutMessage)
		}
		return nil, fmt.Errorf("cannot reach %s's admin login: %s", setup.Origin, redactTransportError(err))
	}
	switch {
	case loginResp.StatusCode == http.StatusOK:
	case loginResp.StatusCode == http.StatusUnauthorized || loginResp.StatusCode == http.StatusForbidden:
		return nil, errors.New("the backend rejected this plugin's admin credentials; its config.json no longer matches the data directory this plugin owns")
	default:
		return nil, fmt.Errorf("admin login returned HTTP %d", loginResp.StatusCode)
	}
	cookie := sessionCookie(loginResp.Headers)
	if cookie == "" {
		return nil, errors.New("admin login did not provide a session cookie")
	}
	headers := http.Header{}
	headers.Set("Cookie", cookie)
	// A session cookie is a live credential: never echo it in an error.
	return adminRoundTrip(ctx, doer, setup.Origin+normalizeStudioPath(path),
		method, body, headers, timeout, timeoutMessage, setup)
}

// sessionCookie extracts the admin session cookie from a login response.
func sessionCookie(headers http.Header) string {
	if headers == nil {
		return ""
	}
	for _, raw := range headers.Values("Set-Cookie") {
		for _, pair := range strings.Split(raw, ";") {
			pair = strings.TrimSpace(pair)
			if strings.HasPrefix(strings.ToLower(pair), "aistudio_admin=") {
				return pair
			}
		}
	}
	return ""
}

// adminRoundTrip performs one authenticated admin request and normalises failures.
func adminRoundTrip(ctx context.Context, doer HTTPDoer, url, method string, body any, headers http.Header, timeout time.Duration, timeoutMessage string, setup Setup) (json.RawMessage, error) {
	var payload []byte
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode admin request: %w", err)
		}
		payload = raw
		if headers == nil {
			headers = http.Header{}
		}
		headers.Set("Content-Type", "application/json")
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if headers == nil {
		headers = http.Header{}
	}
	resp, err := doWithTimeout(ctx, doer, HTTPRequest{
		Method:  strings.ToUpper(method),
		URL:     url,
		Headers: headers,
		Body:    payload,
	}, timeout)
	if err != nil {
		if isTimeoutError(err) && timeoutMessage != "" {
			return nil, errors.New(timeoutMessage)
		}
		return nil, fmt.Errorf("cannot reach the local backend at %s: %s", setup.Origin, redactTransportError(err))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The backend's error text describes the request, not credentials, but
		// it can echo a label. Truncate and never include the body verbatim.
		return nil, fmt.Errorf("backend rejected the admin request (HTTP %d): %s", resp.StatusCode, adminErrorDetail(resp.Body))
	}
	if len(resp.Body) == 0 {
		return json.RawMessage("{}"), nil
	}
	return json.RawMessage(resp.Body), nil
}

// adminErrorDetail extracts a short, credential-free reason from an admin error
// body.
func adminErrorDetail(body []byte) string {
	var doc struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &doc); err == nil {
		if doc.Error != "" {
			return truncate(doc.Error, 200)
		}
		if doc.Message != "" {
			return truncate(doc.Message, 200)
		}
	}
	detail := truncate(strings.TrimSpace(string(body)), 200)
	if detail == "" {
		return "unknown error"
	}
	return detail
}

func jsonHeader() http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	return headers
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
// could carry a credential. URLs are never echoed whole (a query could hold one)
// and the message is truncated.
func redactTransportError(err error) string {
	if err == nil {
		return ""
	}
	return RedactText(err.Error(), 240)
}

// RedactText truncates a message and strips anything resembling a URL's
// userinfo or query, so a caller can surface a transport failure safely.
func RedactText(text string, limit int) string {
	if idx := strings.Index(text, "://"); idx >= 0 {
		if at := strings.Index(text[idx:], "@"); at >= 0 {
			text = text[:idx+3] + "…" + text[idx+at:]
		}
	}
	return truncate(text, limit)
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
