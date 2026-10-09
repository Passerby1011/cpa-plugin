package gocli

import (
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// http.Header canonicalizes every key on Set, so a map lookup must use the
// canonical spelling: `h["x-session-id"]` is absent even when the header IS
// set, while `h["X-Session-Id"]` finds it. The wire spellings below are the
// reference `forwardToCC` names; canonicalHeaderKey adapts them for direct map
// access.
var (
	wireSessionKey     = http.CanonicalHeaderKey("x-session-id")
	wireTraceparentKey = http.CanonicalHeaderKey("traceparent")
)

// wireHeaderKeys is the reference `forwardToCC` header set, with the one
// conditional member (x-session-id) listed separately so a test can count
// keys exactly instead of merely checking the ones it remembers. Entries are
// canonicalized because that is how they key the map.
var wireHeaderKeys = []string{
	http.CanonicalHeaderKey("Content-Type"),
	http.CanonicalHeaderKey("User-Agent"),
	http.CanonicalHeaderKey("x-command-code-version"),
	http.CanonicalHeaderKey("x-cli-environment"),
	http.CanonicalHeaderKey("x-project-slug"),
	http.CanonicalHeaderKey("x-taste-learning"),
	http.CanonicalHeaderKey("Authorization"),
	wireTraceparentKey,
}

// TestGenerateHeadersWithOptionsExactSet pins the whole header map against the
// reference wire shape: exactly nine keys with a session id, each carrying the
// documented value. A missing or extra header (the reference's absence of
// x-co-flag included) changes the key count and fails here.
func TestGenerateHeadersWithOptionsExactSet(t *testing.T) {
	t.Setenv(envProjectDir, "") // x-project-slug must follow the compiled-in default
	h := GenerateHeadersWithOptions("user_x", HeaderOptions{SessionID: "sess-9"})

	if got := len(h); got != 9 {
		t.Errorf("header key count with a session id = %d, want 9 (%v)", got, headerKeys(h))
	}
	want := map[string]string{
		"Content-Type":           "application/json",
		"User-Agent":             "cli",
		"x-command-code-version": DefaultVersion,
		"x-cli-environment":      "production",
		"x-project-slug":         SlugifyProjectPath(DefaultProjectDir),
		"x-taste-learning":       "false",
		"x-session-id":           "sess-9",
		"Authorization":          "Bearer user_x",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	if _, ok := h[wireSessionKey]; !ok {
		t.Errorf("header %s is missing from the session-carrying set", wireSessionKey)
	}
	if got := h.Get("Authorization"); got != "Bearer user_x" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer user_x")
	}
	if got := h.Get("x-project-slug"); got != SlugifyProjectPath(DefaultProjectDir) {
		t.Errorf("x-project-slug = %q, want the default dir's slug %q", got, SlugifyProjectPath(DefaultProjectDir))
	}
	if got := h.Get("x-co-flag"); got != "" {
		t.Errorf("x-co-flag must not be sent, got %q", got)
	}
	assertOnlyKeys(t, h, wireHeaderKeys, wireSessionKey)
}

// TestGenerateHeadersWithOptionsOmitsEmptySession pins the conditional member:
// an empty SessionID drops the header entirely, which is not the same wire
// shape as sending it with an empty value.
func TestGenerateHeadersWithOptionsOmitsEmptySession(t *testing.T) {
	h := GenerateHeadersWithOptions("user_x", HeaderOptions{})
	if got := len(h); got != 8 {
		t.Errorf("header key count without a session id = %d, want 8 (%v)", got, headerKeys(h))
	}
	if got := h.Get("x-session-id"); got != "" {
		t.Errorf("x-session-id = %q, want the header to be absent", got)
	}
	if _, ok := h[wireSessionKey]; ok {
		t.Errorf("%s is present in the map although SessionID was empty", wireSessionKey)
	}
	assertOnlyKeys(t, h, wireHeaderKeys)
}

// TestGenerateHeadersWrapperBackCompat pins that the original two-argument
// entry point still produces the session-carrying header map: every key and
// value matches the options-aware form (traceparent excepted, being random by
// construction).
func TestGenerateHeadersWrapperBackCompat(t *testing.T) {
	wrapper := GenerateHeaders("user_k", "sess-1")
	explicit := GenerateHeadersWithOptions("user_k", HeaderOptions{SessionID: "sess-1"})

	if len(wrapper) != len(explicit) {
		t.Fatalf("wrapper key count = %d, explicit = %d", len(wrapper), len(explicit))
	}
	for k, v := range explicit {
		if k == wireTraceparentKey {
			continue
		}
		if got := wrapper.Get(k); got != v[0] {
			t.Errorf("wrapper header %s = %q, explicit = %q", k, got, v[0])
		}
	}
	wrapperCopy, explicitCopy := cloneHeader(wrapper), cloneHeader(explicit)
	delete(wrapperCopy, wireTraceparentKey)
	delete(explicitCopy, wireTraceparentKey)
	if !reflect.DeepEqual(wrapperCopy, explicitCopy) {
		t.Errorf("wrapper diverged from the options-aware form:\n wrapper=%v\nexplicit=%v", wrapperCopy, explicitCopy)
	}

	// The wrapper must also forward an empty session id, not fabricate one.
	if h := GenerateHeaders("user_k", ""); len(h) != 8 {
		t.Errorf("wrapper with an empty session id = %d keys, want 8", len(h))
	}
}

// TestGenerateHeadersProjectDirOverride checks the project dir becomes the
// x-project-slug value, and that an empty override falls back to the default.
func TestGenerateHeadersProjectDirOverride(t *testing.T) {
	tests := []struct {
		name string
		dir  string
		want string
	}{
		{name: "windows path", dir: `C:\Users\me\code\my-app`, want: "c-users-me-code-my-app"},
		{name: "posix path", dir: "/home/me/code/my-app", want: "home-me-code-my-app"},
		{name: "empty falls back to the default", dir: "", want: SlugifyProjectPath(DefaultProjectDir)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := GenerateHeadersWithOptions("user_k", HeaderOptions{ProjectDir: tc.dir})
			if got := h.Get("x-project-slug"); got != tc.want {
				t.Errorf("x-project-slug = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGenerateHeadersVersionOverride checks the version header follows the
// option when set and DefaultVersion when not.
func TestGenerateHeadersVersionOverride(t *testing.T) {
	h := GenerateHeadersWithOptions("user_k", HeaderOptions{Version: "1.99.9"})
	if got := h.Get("x-command-code-version"); got != "1.99.9" {
		t.Errorf("x-command-code-version = %q, want 1.99.9", got)
	}
	h = GenerateHeadersWithOptions("user_k", HeaderOptions{})
	if got := h.Get("x-command-code-version"); got != DefaultVersion {
		t.Errorf("x-command-code-version = %q, want the default %q", got, DefaultVersion)
	}
}

// TestGenerateHeadersTraceparent checks the traceparent is a fresh W3C trace
// context per request rather than a constant.
func TestGenerateHeadersTraceparent(t *testing.T) {
	pat := regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`)
	first := GenerateHeadersWithOptions("user_k", HeaderOptions{SessionID: "s"})
	second := GenerateHeadersWithOptions("user_k", HeaderOptions{SessionID: "s"})
	for i, h := range []http.Header{first, second} {
		if got := h.Get("traceparent"); !pat.MatchString(got) {
			t.Errorf("header set %d traceparent %q does not match the W3C shape", i, got)
		}
	}
	if first.Get("traceparent") == second.Get("traceparent") {
		t.Errorf("two header sets share traceparent %q", first.Get("traceparent"))
	}
	// The two sets differ only in the random trace context.
	a, b := cloneHeader(first), cloneHeader(second)
	delete(a, wireTraceparentKey)
	delete(b, wireTraceparentKey)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("header sets differ beyond traceparent:\n a=%v\n b=%v", a, b)
	}
}

// cloneHeader deep-copies a header map so a test can mutate the copy without
// touching what the function under test returned.
func cloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// headerKeys returns the sorted key list, for readable failure messages. These
// lists only ever appear inside a failure message, never in a comparison.
func headerKeys(h http.Header) []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// assertOnlyKeys fails when h carries any key outside base (plus the optional
// allowlist), so an accidental extra header is caught by name.
func assertOnlyKeys(t *testing.T, h http.Header, base []string, allowed ...string) {
	t.Helper()
	ok := map[string]bool{}
	for _, k := range base {
		ok[k] = true
	}
	for _, k := range allowed {
		ok[k] = true
	}
	for k := range h {
		if !ok[k] {
			t.Errorf("unexpected header %q in %v", k, headerKeys(h))
		}
	}
	for _, k := range base {
		if _, present := h[k]; !present {
			t.Errorf("missing header %q in %v", k, headerKeys(h))
		}
	}
}

// TestMapUpstreamStatus pins the reference CC_STATUS_MAP row for row, plus the
// fallback for codes the map does not know.
func TestMapUpstreamStatus(t *testing.T) {
	tests := []struct {
		name     string
		ccStatus int
		want     UpstreamStatus
	}{
		{name: "bad request", ccStatus: 400, want: UpstreamStatus{Status: 400, Type: "invalid_request_error"}},
		{name: "unauthorized", ccStatus: 401, want: UpstreamStatus{Status: 401, Type: "authentication_error"}},
		{name: "payment required becomes rate limit", ccStatus: 402, want: UpstreamStatus{Status: 429, Type: "rate_limit_error"}},
		{name: "forbidden becomes unauthorized", ccStatus: 403, want: UpstreamStatus{Status: 401, Type: "authentication_error"}},
		{name: "not found", ccStatus: 404, want: UpstreamStatus{Status: 404, Type: "not_found"}},
		{name: "unprocessable becomes bad request", ccStatus: 422, want: UpstreamStatus{Status: 400, Type: "invalid_request_error"}},
		{name: "rate limited", ccStatus: 429, want: UpstreamStatus{Status: 429, Type: "rate_limit_error"}},
		{name: "server error becomes bad gateway", ccStatus: 500, want: UpstreamStatus{Status: 502, Type: "upstream_error"}},
		{name: "bad gateway stays bad gateway", ccStatus: 502, want: UpstreamStatus{Status: 502, Type: "upstream_error"}},
		{name: "unavailable stays temporarily unavailable", ccStatus: 503, want: UpstreamStatus{Status: 503, Type: "temporarily_unavailable"}},
		{name: "unmapped client code", ccStatus: 418, want: UpstreamStatus{Status: 502, Type: "upstream_error"}},
		{name: "unmapped zero", ccStatus: 0, want: UpstreamStatus{Status: 502, Type: "upstream_error"}},
		{name: "unmapped negative", ccStatus: -1, want: UpstreamStatus{Status: 502, Type: "upstream_error"}},
		{name: "unmapped 599", ccStatus: 599, want: UpstreamStatus{Status: 502, Type: "upstream_error"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MapUpstreamStatus(tc.ccStatus); got != tc.want {
				t.Errorf("MapUpstreamStatus(%d) = %+v, want %+v", tc.ccStatus, got, tc.want)
			}
		})
	}
}

// referenceStatusMap is the reference implementation's CC_STATUS_MAP, written
// out in full. TestMapUpstreamStatusTableIntegrity compares the shipping table
// against it row for row: pinning a handful of sample codes is not enough,
// because a wrong row for an untested code stays invisible until it reaches a
// client.
var referenceStatusMap = map[int]UpstreamStatus{
	400: {Status: 400, Type: "invalid_request_error"},
	401: {Status: 401, Type: "authentication_error"},
	402: {Status: 429, Type: "rate_limit_error"},
	403: {Status: 401, Type: "authentication_error"},
	404: {Status: 404, Type: "not_found"},
	422: {Status: 400, Type: "invalid_request_error"},
	429: {Status: 429, Type: "rate_limit_error"},
	500: {Status: 502, Type: "upstream_error"},
	502: {Status: 502, Type: "upstream_error"},
	503: {Status: 503, Type: "temporarily_unavailable"},
}

// TestMapUpstreamStatusTableIntegrity compares the shipping table against the
// reference, both directions, so a row can neither be dropped nor invented.
func TestMapUpstreamStatusTableIntegrity(t *testing.T) {
	if len(ccStatusMap) != len(referenceStatusMap) {
		t.Fatalf("ccStatusMap has %d rows, want the %d reference rows", len(ccStatusMap), len(referenceStatusMap))
	}
	for code, want := range referenceStatusMap {
		got, ok := ccStatusMap[code]
		if !ok {
			t.Errorf("ccStatusMap is missing the reference row for %d", code)
			continue
		}
		if got != want {
			t.Errorf("ccStatusMap[%d] = %+v, want %+v", code, got, want)
		}
	}
	for code := range ccStatusMap {
		if _, ok := referenceStatusMap[code]; !ok {
			t.Errorf("ccStatusMap carries the non-reference row %d", code)
		}
	}
	if upstreamErrorStatus != (UpstreamStatus{Status: 502, Type: "upstream_error"}) {
		t.Errorf("fallback status = %+v, want 502/upstream_error", upstreamErrorStatus)
	}
}

// TestZeroOutputReason checks the zero-output guard distinguishes a real
// answer from a completed stream that carried nothing visible. Each fixture is
// a completed stream (a finish event closes it) built with the same SSE frame
// helper the other stream tests use.
func TestZeroOutputReason(t *testing.T) {
	tests := []struct {
		name       string
		stream     string
		wantReason bool
	}{
		{
			name:   "text answer",
			stream: cliFrame(`{"type":"text-delta","text":"Hello world"}`) + cliFrame(`{"type":"finish","finishReason":"stop"}`) + cliFrame("[DONE]"),
		},
		{
			name:   "tool call only",
			stream: cliFrame(`{"type":"tool-call","toolCallId":"call_1","toolName":"lookup","input":{"q":"x"}}`) + cliFrame(`{"type":"finish","finishReason":"tool_calls"}`) + cliFrame("[DONE]"),
		},
		{
			name:   "tool call with null input",
			stream: cliFrame(`{"type":"tool-call","toolCallId":"call_1","toolName":"lookup","input":null}`) + cliFrame(`{"type":"finish","finishReason":"tool_calls"}`),
		},
		{
			name:   "reasoning only answer",
			stream: cliFrame(`{"type":"reasoning-start"}`) + cliFrame(`{"type":"reasoning-delta","text":"hmm"}`) + cliFrame(`{"type":"reasoning-end"}`) + cliFrame(`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":3,"outputTokens":0}}`),
		},
		{
			name:       "empty answer",
			stream:     cliFrame(`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":3,"outputTokens":0}}`) + cliFrame("[DONE]"),
			wantReason: true,
		},
		{
			name:       "whitespace-only answer",
			stream:     cliFrame(`{"type":"text-delta","text":"  \n\t "}`) + cliFrame(`{"type":"finish","finishReason":"stop"}`),
			wantReason: true,
		},
		{
			name:       "no events at all",
			stream:     "",
			wantReason: true,
		},
		{
			name:       "lifecycle markers only",
			stream:     cliFrame(`{"type":"reasoning-start"}`) + cliFrame(`{"type":"reasoning-end"}`) + cliFrame(`{"type":"cache-write-tokens","tokens":12}`) + cliFrame(`{"type":"finish","finishReason":"stop"}`),
			wantReason: true,
		},
		{
			name:       "error event yields no output",
			stream:     cliFrame(`{"type":"error","error":"internal_error","message":"boom"}`),
			wantReason: true,
		},
		{
			name:       "malformed frame yields no output",
			stream:     cliFrame(`{"type":"text-del`),
			wantReason: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ZeroOutputReason([]byte(tc.stream))
			if tc.wantReason {
				if strings.TrimSpace(got) == "" {
					t.Fatalf("ZeroOutputReason(%q) = %q, want a non-empty reason", tc.stream, got)
				}
				return
			}
			if got != "" {
				t.Fatalf("ZeroOutputReason(%q) = %q, want no reason", tc.stream, got)
			}
		})
	}
}

// TestZeroOutputReasonTruncatedStreamIsNotZeroOutput pins the boundary with the
// truncation contract: a stream that dropped mid-answer still carries visible
// output, so the zero-output guard must leave that verdict to the executor's
// truncation check rather than reporting it as an empty answer.
func TestZeroOutputReasonTruncatedStreamIsNotZeroOutput(t *testing.T) {
	partial := cliFrame(`{"type":"text-delta","text":"partial"}`) // no finish event
	if got := ZeroOutputReason([]byte(partial)); got != "" {
		t.Errorf("a truncated but non-empty stream reported %q, want no reason", got)
	}
}
