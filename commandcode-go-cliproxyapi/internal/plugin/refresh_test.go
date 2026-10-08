package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/catalog"
	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/config"
)

const (
	cooledRefreshKey  = "sk-refresh-cooled-key"
	goodRefreshKey    = "sk-refresh-good-key"
	refreshTwoKeyYAML = "api-keys:\n" +
		"  - value: " + cooledRefreshKey + "\n" +
		"  - value: " + goodRefreshKey + "\n" +
		"catalog-url: https://catalog.test/models\n"
)

// authRecorder answers host.http.do, records every Authorization header, and
// rejects the cooled first key 401-style while any other key gets a good
// catalog body.
type authRecorder struct {
	mu    sync.Mutex
	auths []string
	logs  [][]byte
	// body, when non-empty, replaces testCatalogJSON as the served catalog.
	body []byte
	// catalogFetch, when non-nil, replaces the canned /models response; the
	// fallback tests use it to fail, succeed, or alternate per fetch while
	// host.log call recording keeps working.
	catalogFetch func() ([]byte, error)
}

func (r *authRecorder) call(method string, payload []byte) ([]byte, error) {
	if method == pluginabi.MethodHostLog {
		r.mu.Lock()
		r.logs = append(r.logs, append([]byte(nil), payload...))
		r.mu.Unlock()
		return hostOK(map[string]any{}), nil
	}
	if method != pluginabi.MethodHostHTTPDo {
		return hostOK(map[string]any{}), nil
	}
	// The plan gate's billing reads are answered FIRST and from their own
	// canned bodies: catalogFetch exists to control what a /models fetch does
	// (fail, succeed, alternate), and routing billing traffic into it would
	// (a) count a billing call as a catalog attempt and (b) hand catalog JSON
	// to the subscription decoder. An individual-go planId is used rather than
	// an unknown one so the pool's tier is KNOWN: a fixture that cannot say
	// what the account's plan is would make the gate publish everything and
	// then log a warning about it, which is exactly what the log-count
	// assertions below must not have to expect.
	var wireURL struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(payload, &wireURL)
	switch {
	case strings.HasSuffix(wireURL.URL, accountSubscriptionPath):
		return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK,
			Body: []byte(`{"success":true,"data":{"planId":"individual-go"}}`)}), nil
	case strings.HasSuffix(wireURL.URL, accountCreditsPath):
		return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK,
			Body: []byte(`{"credits":{"monthlyCredits":10,"purchasedCredits":0,"freeCredits":0}}`)}), nil
	}
	if r.catalogFetch != nil {
		return r.catalogFetch()
	}
	var wire struct {
		Headers http.Header `json:"headers"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return hostErr("test", "undecodable payload"), nil
	}
	auth := wire.Headers.Get("Authorization")
	r.mu.Lock()
	r.auths = append(r.auths, auth)
	r.mu.Unlock()
	if auth == "Bearer "+cooledRefreshKey {
		return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized}), nil
	}
	body := testCatalogJSON
	if len(r.body) > 0 {
		body = string(r.body)
	}
	return hostOK(pluginapi.HTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(body),
	}), nil
}

func (r *authRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.auths...)
}

func (r *authRecorder) logCalls() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.logs...)
}

func twoKeyFixture(t *testing.T) (config.Config, *catalog.Manager, *HostBridge, *authRecorder) {
	t.Helper()
	cfg, err := config.Load([]byte(refreshTwoKeyYAML))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{}
	bridge := NewHostBridge(rec.call)
	return cfg, catalog.New(cfg, bridge), bridge, rec
}

// TestRefreshUsesFirstConfiguredKey pins the Phase 5 fallback: catalog refresh
// uses the deterministic first configured key and does not try another
// configured key when that key fails.
func TestRefreshUsesFirstConfiguredKey(t *testing.T) {
	cfg, mgr, bridge, rec := twoKeyFixture(t)
	if err := refreshOnce(context.Background(), mgr, bridge, time.Second, cfg); err == nil {
		t.Fatal("expected first configured key failure")
	}
	auths := rec.seen()
	if len(auths) != 1 || auths[0] != "Bearer "+cooledRefreshKey {
		t.Fatalf("refresh auths = %v; want one first-key attempt", auths)
	}
	if len(mgr.Models()) != 0 {
		t.Fatalf("failed refresh changed catalog: %+v", mgr.Models())
	}
}

// TestRefreshUnsupportedLogQuotesNewlineIDs pins F2: a catalog entry excluded
// from the routable set must reach the host.log "models" field quoted (%q), so
// no raw newline carried by an upstream ID can forge log lines (CWE-117).
// The exclusion is produced by the protocol kill-switch — the default route is
// chat-completions here, so turning that switch off demotes the entry while
// leaving its ID in the diagnostic.
func TestRefreshUnsupportedLogQuotesNewlineIDs(t *testing.T) {
	const evilID = "evil\n2027-01-01 ERROR forged host.log line"
	cfg, err := config.Load([]byte("api-keys:\n  - value: sk-refresh-evil-key\n" +
		"catalog-url: https://catalog.test/models\n" +
		"protocols:\n  chat-completions: false\n"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{body: []byte(`{"data":[{"id":"evil\n2027-01-01 ERROR forged host.log line"}]}`)}
	bridge := NewHostBridge(rec.call)
	mgr := catalog.New(cfg, bridge)
	if err := refreshOnce(context.Background(), mgr, bridge, time.Second, cfg); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}
	logCalls := rec.logCalls()
	if len(logCalls) != 1 {
		t.Fatalf("host.log calls = %d, want exactly one unsupported-models warn", len(logCalls))
	}
	var req struct {
		Level   string         `json:"level"`
		Message string         `json:"message"`
		Fields  map[string]any `json:"fields"`
	}
	if err := json.Unmarshal(logCalls[0], &req); err != nil {
		t.Fatalf("decode host.log payload: %v", err)
	}
	if req.Message != "unsupported models excluded from routable catalog" {
		t.Fatalf("unexpected log message: %q", req.Message)
	}
	modelsField, _ := req.Fields["models"].(string)
	if strings.ContainsRune(modelsField, '\n') {
		t.Fatalf("models log field contains raw newline (log injection): %q", modelsField)
	}
	if want := fmt.Sprintf("%q: ", evilID); !strings.Contains(modelsField, want) {
		t.Fatalf("models field must quote the upstream ID; got %q, want prefix %q", modelsField, want)
	}
}

// ---- static-catalog fallback -------------------------------------------
//
// The matrix below pins the full catalog resolution order of refreshOnce: a
// provider-mode account selects the live /models fetch; a failure falls back
// to a still-usable previous snapshot first (stale-while-unavailable) and to
// the configured static model list second, never the other way around. The
// static list is a config convenience, not a second code path: it feeds
// catalog.Manager.SeedStatic, the same snapshot builder the live fetch uses,
// so models.allow/deny, model-prefix, the protocol kill-switches, route
// overrides, and dedup all apply to it.

const (
	// refreshStaticKey is a provider-mode credential, so the live /models
	// fetch is attempted (unlike a go-cli-only pool).
	refreshStaticKey = "sk-refresh-static-key"
	// refreshGoCliKey is a Go-plan credential: the Provider API refuses it,
	// so the pool has no provider account and /models is unreachable.
	refreshGoCliKey = "sk-refresh-gocli-key"

	refreshStaticKeyYAML = "api-keys:\n  - value: " + refreshStaticKey + "\n"
	refreshGoCliYAML     = "accounts:\n  - mode: go-cli\n    credential: " + refreshGoCliKey + "\n"
)

// refreshCatalogFor decides what a /models fetch does: nil serves okBody;
// non-nil serves its error (or a 401 when it returns no envelope, which the
// catalog classifies as an http-status failure).
type refreshCatalogFor func() ([]byte, error)

// upstreamFailure is the shorthand for "the live catalog fetch fails".
func upstreamFailure() ([]byte, error) {
	return hostErr("upstream_down", "simulated upstream failure"), nil
}

// alwaysOKCatalog answers every /models fetch with body.
func alwaysOKCatalog(body string) refreshCatalogFor {
	return func() ([]byte, error) {
		return hostOK(pluginapi.HTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(body),
		}), nil
	}
}

// catalogFailOn serves body on every /models fetch except the nth, which
// fails — the shape of "registration succeeded, the reconfigure that follows
// hits an outage".
func catalogFailOn(body string, n int64) (refreshCatalogFor, *atomic.Int64) {
	fetches := &atomic.Int64{}
	return func() ([]byte, error) {
		if fetches.Add(1) == n {
			return upstreamFailure()
		}
		return alwaysOKCatalog(body)()
	}, fetches
}

// seedStaticManager installs an empty manager already carrying a previous
// snapshot published from ids, as if an earlier refresh had succeeded.
func seedStaticManager(t *testing.T, cfg config.Config, bridge *HostBridge, ids ...string) *catalog.Manager {
	t.Helper()
	mgr := catalog.New(cfg, bridge)
	mgr.SeedStatic(ids)
	if len(mgr.Models()) != len(ids) {
		t.Fatalf("preloaded snapshot = %d models, want %d", len(mgr.Models()), len(ids))
	}
	return mgr
}

func refreshLogEntries(t *testing.T, rec *authRecorder) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, payload := range rec.logCalls() {
		var entry struct {
			Level   string         `json:"level"`
			Message string         `json:"message"`
			Fields  map[string]any `json:"fields"`
		}
		if err := json.Unmarshal(payload, &entry); err != nil {
			t.Fatalf("decode host.log payload %s: %v", payload, err)
		}
		flat := map[string]any{"level": entry.Level, "message": entry.Message, "raw": string(payload)}
		for k, v := range entry.Fields {
			flat[k] = v
		}
		out = append(out, flat)
	}
	return out
}

func assertRefreshLoggedOnce(t *testing.T, rec *authRecorder, level, contains string) {
	t.Helper()
	entries := refreshLogEntries(t, rec)
	if len(entries) != 1 {
		t.Fatalf("host.log calls = %d (%v), want exactly the one fallback diagnostic", len(entries), entries)
	}
	entry := entries[0]
	if entry["level"] != level || !strings.Contains(entry["message"].(string), contains) {
		t.Fatalf("log entry = %v; want level %q containing %q", entry, level, contains)
	}
	if entry["error"] == nil {
		t.Fatalf("fallback diagnostic must carry the failure reason: %v", entry)
	}
}

// publishedModelIDs reads the served ids through the model.static handler,
// which is what the host actually asks for.
func publishedModelIDs(t *testing.T, m *Manager) []string {
	t.Helper()
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	ids := make([]string, 0, len(static.Models))
	for _, mi := range static.Models {
		ids = append(ids, mi.ID)
	}
	sort.Strings(ids)
	return ids
}

// catalogPublishedIDs reads the same ids straight off a manager that a test
// built by hand (refreshOnce takes the manager, not the dispatcher).
func catalogPublishedIDs(mgr *catalog.Manager) []string {
	records := mgr.Models()
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.PublicID)
	}
	sort.Strings(ids)
	return ids
}

func assertPublishedIDs(t *testing.T, got, want []string) {
	t.Helper()
	sort.Strings(want)
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("published ids = %v, want %v", got, want)
	}
}

// TestRefreshStaticFallbackMatrix pins the resolution order in one table:
// static is seeded both when the pool has no provider account at all (a
// go-cli-only deployment, whose Provider API refuses its keys) and when a
// provider fetch fails with no usable snapshot to keep.
func TestRefreshStaticFallbackMatrix(t *testing.T) {
	for _, tc := range []struct {
		name     string // development test name
		desc     string // what the row proves
		yaml     string
		catalog  refreshCatalogFor
		seed     []string
		wantIDs  []string
		wantErr  bool
		wantLogs int
	}{
		{
			name: "go-cli-only pool fetches live catalog",
			desc: "a Go-plan-only pool CAN reach /models, so the live catalog wins over static (0.4.2)",
			// A Go key calls /models successfully; before 0.4.2 this pool was
			// wrongly locked out of live discovery and served static instead.
			yaml:     refreshGoCliYAML + "catalog:\n  static:\n    - claude-sonnet-4-6\n    - deepseek/deepseek-v4-pro\nmodel-prefix:\n  enabled: false\n",
			catalog:  alwaysOKCatalog(`{"data":[{"id":"live-model"}]}`),
			wantIDs:  []string{"live-model"},
			wantLogs: 0,
		},
		{
			name: "go-cli-only pool without static stays empty",
			desc: "no static list and an empty live catalog -> empty catalog, no error",
			yaml: refreshGoCliYAML,
			// A successful-but-empty live fetch: nothing to publish, no error.
			catalog:  alwaysOKCatalog(`{"data":[]}`),
			wantLogs: 0,
		},
		{
			name: "live success wins over static",
			desc: "a successful fetch is served and the configured static list is left alone",
			yaml: refreshStaticKeyYAML +
				"catalog:\n  static:\n    - static/must-not-be-published\n" +
				"model-prefix:\n  enabled: false\n",
			catalog: alwaysOKCatalog(`{"data":[{"id":"live-model"}]}`),
			wantIDs: []string{"live-model"},
		},
		{
			name: "failure with no snapshot falls back to static",
			desc: "fail-closed fetch failure with a static list -> static is served and the error is swallowed",
			yaml: refreshStaticKeyYAML +
				"catalog:\n  stale-while-unavailable: false\n  static:\n    - static/fallback-a\n    - static/fallback-b\n" +
				"model-prefix:\n  enabled: false\n",
			catalog:  upstreamFailure,
			wantIDs:  []string{"static/fallback-a", "static/fallback-b"},
			wantLogs: 1,
		},
		{
			name: "failure with a usable snapshot keeps it",
			desc: "stale-while-unavailable plus a served snapshot -> the snapshot survives and static is NOT published",
			yaml: refreshStaticKeyYAML +
				"catalog:\n  static:\n    - static/must-not-be-published\n" +
				"model-prefix:\n  enabled: false\n",
			catalog:  upstreamFailure,
			seed:     []string{"prev/kept-model"},
			wantIDs:  []string{"prev/kept-model"},
			wantLogs: 1,
		},
		{
			name: "fail-closed failure with a snapshot falls back to static",
			desc: "stale-while-unavailable off -> Manager.fail clears the snapshot, so static wins over it",
			yaml: refreshStaticKeyYAML +
				"catalog:\n  stale-while-unavailable: false\n  static:\n    - static/fallback-a\n" +
				"model-prefix:\n  enabled: false\n",
			catalog:  upstreamFailure,
			seed:     []string{"prev/dropped-model"},
			wantIDs:  []string{"static/fallback-a"},
			wantLogs: 1,
		},
		{
			name: "no snapshot and no static reports the failure",
			desc: "nothing to fall back to -> the classified error reaches the caller (FR-002 fail-closed)",
			yaml: refreshStaticKeyYAML +
				"catalog:\n  stale-while-unavailable: false\n",
			catalog:  upstreamFailure,
			wantErr:  true,
			wantLogs: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if tc.catalog == nil {
				tc.catalog = func() ([]byte, error) { t.Fatal("unexpected upstream fetch"); return nil, nil }
			}
			rec := &authRecorder{catalogFetch: tc.catalog}
			bridge := NewHostBridge(rec.call)
			var mgr *catalog.Manager
			if len(tc.seed) > 0 {
				mgr = seedStaticManager(t, cfg, bridge, tc.seed...)
			} else {
				mgr = catalog.New(cfg, bridge)
			}

			err = refreshOnce(context.Background(), mgr, bridge, time.Second, cfg)
			if tc.wantErr && err == nil {
				t.Fatal("expected the refresh error to reach the caller")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("refreshOnce: %v (a served catalog must not surface as an error)", err)
			}
			assertPublishedIDs(t, catalogPublishedIDs(mgr), tc.wantIDs)
			entries := refreshLogEntries(t, rec)
			if len(entries) != tc.wantLogs {
				t.Fatalf("host.log calls = %d (%v), want %d", len(entries), entries, tc.wantLogs)
			}
		})
	}
}

// TestRefreshStaleFallbackDiagnostics pins the exact fallback wording: the
// kept snapshot logs "serving stale catalog" at warn, and the static path
// logs its own distinct message — an operator can tell the two apart.
func TestRefreshStaleFallbackDiagnostics(t *testing.T) {
	cfg, err := config.Load([]byte(refreshStaticKeyYAML +
		"catalog:\n  static:\n    - static/must-not-be-published\n"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{catalogFetch: upstreamFailure}
	bridge := NewHostBridge(rec.call)
	mgr := seedStaticManager(t, cfg, bridge, "prev/kept-model")

	if err := refreshOnce(context.Background(), mgr, bridge, time.Second, cfg); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}
	assertRefreshLoggedOnce(t, rec, "warn", "serving stale catalog")
	for _, record := range mgr.Models() {
		if record.UpstreamID == "static/must-not-be-published" {
			t.Fatalf("static list overwrote a usable snapshot: %+v", record)
		}
	}
}

// TestRefreshStaticFallbackDiagnostics pins the exact fallback wording on the
// static path. The live fetch is forced to fail: since 0.4.2 a go-cli account
// reaches /models successfully, so a static fallback only happens when the
// fetch itself fails.
func TestRefreshStaticFallbackDiagnostics(t *testing.T) {
	cfg, err := config.Load([]byte(refreshGoCliYAML +
		"catalog:\n  static:\n    - static/only-model\n"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{catalogFetch: upstreamFailure}
	bridge := NewHostBridge(rec.call)
	mgr := catalog.New(cfg, bridge)

	if err := refreshOnce(context.Background(), mgr, bridge, time.Second, cfg); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}
	entries := refreshLogEntries(t, rec)
	if len(entries) != 1 {
		t.Fatalf("host.log calls = %d (%v), want 1", len(entries), entries)
	}
	entry := entries[0]
	if entry["level"] != "warn" ||
		!strings.Contains(entry["message"].(string), "catalog served from the configured static model list") {
		t.Fatalf("log entry = %v", entry)
	}
}

// TestStaticFallbackHonorsModelFilter pins that the static path is not a
// filter bypass: a model excluded by models.allow/deny is absent from every
// published surface and is reported as a diagnostic instead.
func TestStaticFallbackHonorsModelFilter(t *testing.T) {
	yaml := refreshStaticKeyYAML +
		"catalog:\n  stale-while-unavailable: false\n  static:\n    - keep/one\n    - drop/one\n" +
		"models:\n  deny: [drop/one]\n" +
		"model-prefix:\n  enabled: false\n"
	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{catalogFetch: upstreamFailure}
	bridge := NewHostBridge(rec.call)
	mgr := catalog.New(cfg, bridge)
	if err := refreshOnce(context.Background(), mgr, bridge, time.Second, cfg); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}

	ids := catalogPublishedIDs(mgr)
	assertPublishedIDs(t, ids, []string{"keep/one"})
	if _, ok := mgr.Lookup("drop/one"); ok {
		t.Fatal("denied static model is still resolvable")
	}
	unsup := mgr.Unsupported()
	if len(unsup) != 1 || unsup[0].UpstreamID != "drop/one" ||
		unsup[0].Reason != "excluded by models.allow/deny" {
		t.Fatalf("unsupported = %+v, want drop/one excluded by models.allow/deny", unsup)
	}
}

// TestStaticFallbackHonorsModelPrefix pins the prefix transform on the static
// path: published ids carry the configured prefix, and the bare upstream id
// still resolves.
func TestStaticFallbackHonorsModelPrefix(t *testing.T) {
	yaml := refreshGoCliYAML +
		"catalog:\n  static:\n    - fixture/plan-unknown-model\n" +
		"model-prefix:\n  enabled: true\n  value: proxy\n"
	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{}
	// 显式让实时拉取失败，才能验证 static 兜底：
	// go-cli 账号自 0.4.2 起也能访问 /models，不再自动落到 static。
	rec.catalogFetch = upstreamFailure
	bridge := NewHostBridge(rec.call)
	mgr := catalog.New(cfg, bridge)
	if err := refreshOnce(context.Background(), mgr, bridge, time.Second, cfg); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}

	assertPublishedIDs(t, catalogPublishedIDs(mgr), []string{"proxy/fixture/plan-unknown-model"})
	if _, ok := mgr.Lookup("proxy/fixture/plan-unknown-model"); !ok {
		t.Fatal("prefixed static id does not resolve")
	}
	if _, ok := mgr.Lookup("fixture/plan-unknown-model"); !ok {
		t.Fatal("bare static id does not resolve")
	}
}

// TestStaticFallbackHonorsProtocolSwitch pins that a protocol kill-switch
// excludes the models it owns on the static path too: the default route is
// chat-completions, so turning that protocol off leaves the runnable set
// empty while the diagnostic keeps the excluded ids visible.
func TestStaticFallbackHonorsProtocolSwitch(t *testing.T) {
	yaml := refreshGoCliYAML +
		"catalog:\n  static:\n    - fixture/plan-unknown-a\n    - fixture/plan-unknown-b\n" +
		"protocols:\n  chat-completions: false\n" +
		"model-prefix:\n  enabled: false\n"
	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{}
	// 显式让实时拉取失败，才能验证 static 兜底：
	// go-cli 账号自 0.4.2 起也能访问 /models，不再自动落到 static。
	rec.catalogFetch = upstreamFailure
	bridge := NewHostBridge(rec.call)
	mgr := catalog.New(cfg, bridge)
	if err := refreshOnce(context.Background(), mgr, bridge, time.Second, cfg); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}

	if ids := catalogPublishedIDs(mgr); len(ids) != 0 {
		t.Fatalf("chat-completions disabled but static models were published: %v", ids)
	}
	unsup := mgr.Unsupported()
	if len(unsup) != 2 {
		t.Fatalf("unsupported = %+v, want both static models excluded", unsup)
	}
	for _, u := range unsup {
		if !strings.Contains(u.Reason, "protocol chat-completions disabled") {
			t.Fatalf("reason = %q, want the protocol kill-switch diagnostic", u.Reason)
		}
	}
}

// TestStaticFallbackDeduplicatesIDs pins that a duplicated static entry is
// published once: config normalization drops exact repeats, and the shared
// snapshot builder drops whatever reaches it.
func TestStaticFallbackDeduplicatesIDs(t *testing.T) {
	yaml := refreshGoCliYAML +
		"catalog:\n  static:\n    - dup/model\n    - dup/model\n    - other/model\n" +
		"model-prefix:\n  enabled: false\n"
	cfg, err := config.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{}
	// 显式让实时拉取失败，才能验证 static 兜底：
	// go-cli 账号自 0.4.2 起也能访问 /models，不再自动落到 static。
	rec.catalogFetch = upstreamFailure
	bridge := NewHostBridge(rec.call)
	mgr := catalog.New(cfg, bridge)
	if err := refreshOnce(context.Background(), mgr, bridge, time.Second, cfg); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}

	assertPublishedIDs(t, catalogPublishedIDs(mgr), []string{"dup/model", "other/model"})
	seen := 0
	for _, record := range mgr.Models() {
		if record.UpstreamID == "dup/model" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("dup/model published %d times, want 1", seen)
	}
}

// TestStaticFallbackEmptyListPublishesNothing pins the explicit empty static
// list: normalization yields nil, so no seed happens and the catalog stays
// empty instead of erroring.
func TestStaticFallbackEmptyListPublishesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{name: "empty sequence", yaml: "catalog:\n  static: []\n"},
		{name: "blank entries", yaml: "catalog:\n  static:\n    - \"   \"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load([]byte(refreshGoCliYAML + tc.yaml))
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			// 实时拉取成功但返回空目录；配合空 static 列表，结果必须为空且无错误。
			// （go-cli 账号自 0.4.2 起会真的去拉实时接口，所以这里不能让它失败——
			//  失败会按设计把错误抛给调用方。）
			rec := &authRecorder{catalogFetch: alwaysOKCatalog(`{"data":[]}`)}
			bridge := NewHostBridge(rec.call)
			mgr := catalog.New(cfg, bridge)
			if err := refreshOnce(context.Background(), mgr, bridge, time.Second, cfg); err != nil {
				t.Fatalf("refreshOnce: %v", err)
			}
			if ids := catalogPublishedIDs(mgr); len(ids) != 0 {
				t.Fatalf("empty static list published %v", ids)
			}
		})
	}
}

// TestStaticFallbackOnFailedReconfigureStillRecovers pins the fallback across
// the lifecycle path: a reconfigure whose live fetch fails republishes the
// static list, and the following successful tick replaces it with the live
// catalog.
func TestStaticFallbackOnFailedReconfigureStillRecovers(t *testing.T) {
	next, fetches := catalogFailOn(`{"data":[{"id":"live-model"}]}`, 2)
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			return next()
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	// Registration's live fetch (1) succeeds, so the default prefix publishes
	// commandcode/live-model.
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(refreshStaticKeyYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	assertPublishedIDs(t, publishedModelIDs(t, m), []string{"commandcode/live-model"})
	if got := fetches.Load(); got != 1 {
		t.Fatalf("catalog fetches after register = %d, want 1", got)
	}

	// The reconfigure's live fetch (2) fails; fail-closed drops the live
	// snapshot, so the configured static list takes over.
	yaml := refreshStaticKeyYAML +
		"catalog:\n  stale-while-unavailable: false\n  static:\n    - static/fallback\n" +
		"model-prefix:\n  enabled: false\n"
	if _, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(yaml)); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	assertPublishedIDs(t, publishedModelIDs(t, m), []string{"static/fallback"})

	if got := fetches.Load(); got != 2 {
		t.Fatalf("catalog fetches = %d, want 2", got)
	}
	// The next successful tick (3) replaces the static list with the live
	// catalog.
	manualTick(t, m)
	assertPublishedIDs(t, publishedModelIDs(t, m), []string{"live-model"})
}
