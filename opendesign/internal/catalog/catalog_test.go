package catalog

import (
	"context"
	"errors"
	"testing"

	"github.com/hex-ci/cpa-plugin/opendesign/internal/config"
	"github.com/hex-ci/cpa-plugin/opendesign/internal/modelcaps"
)

func testConfig() config.Config {
	return config.Config{
		BaseURL:     config.DefaultBaseURL,
		ModelPrefix: config.ModelPrefix{Enabled: true, Value: "opendesign"},
	}
}

// A live catalog for one of the seed's own models must keep the seed's name and
// measured levels, which the catalog does not carry, and take the live limits.
func TestLiveEntryKeepsSeedNameAndLevels(t *testing.T) {
	body := []byte(`{"data":[{"id":"deepseek-v4-pro",` +
		`"architecture":{"input_modalities":["text"]},` +
		`"metadata":{"cost":"high","capability":"advanced","context_limit":1048000,"context_budget":232000}}]}`)
	entries, err := ParseLive(body)
	if err != nil {
		t.Fatalf("ParseLive: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	e := entries[0]
	if e.Name != "DeepSeek V4 Pro" {
		t.Errorf("name = %q, want the seed's", e.Name)
	}
	if !sameStrings(e.Efforts, []string{"low", "high", "max"}) {
		t.Errorf("efforts = %v, want the seed's measured levels", e.Efforts)
	}
	if e.Context != 1048000 || e.Output != 232000 {
		t.Errorf("limits = %d/%d", e.Context, e.Output)
	}
	if e.API != modelcaps.APIResponses {
		t.Errorf("api = %q", e.API)
	}
}

// A kind the seed declares must survive a catalog that leaves it out: the caller's
// image is sent as text otherwise, silently (magpie #829).
func TestSeedKindsSurviveACatalogThatOmitsThem(t *testing.T) {
	body := []byte(`{"data":[` +
		`{"id":"deepseek-v4.1-flash-eco","architecture":{"input_modalities":["text"]}},` +
		`{"id":"glm-5.3-flashx"},` +
		`{"id":"kimi-k2.7-code","architecture":{"input_modalities":["text","image","video"]}}]}`)
	entries, err := ParseLive(body)
	if err != nil {
		t.Fatalf("ParseLive: %v", err)
	}
	byID := map[string]Entry{}
	for _, e := range entries {
		byID[e.UpstreamID] = e
	}
	if !contains(byID["deepseek-v4.1-flash-eco"].InputKinds, "image") {
		t.Errorf("seed kind dropped: %v", byID["deepseek-v4.1-flash-eco"].InputKinds)
	}
	if !sameStrings(byID["glm-5.3-flashx"].InputKinds, []string{"image", "file", "video"}) {
		t.Errorf("glm-5.3-flashx kinds = %v, want the seed's", byID["glm-5.3-flashx"].InputKinds)
	}
	// The catalog's kinds ADD to the seed's, never replace them.
	for _, k := range []string{"image", "video"} {
		if !contains(byID["kimi-k2.7-code"].InputKinds, k) {
			t.Errorf("catalog kind %q missing: %v", k, byID["kimi-k2.7-code"].InputKinds)
		}
	}
}

// A model the seed does not know gets a workable entry from the catalog alone.
func TestUnknownModelGetsWorkableDefaults(t *testing.T) {
	body := []byte(`{"data":[{"id":"brand-new-9","architecture":{"input_modalities":["text","image"]},` +
		`"metadata":{"cost":"medium","capability":"advanced"}}]}`)
	entries, err := ParseLive(body)
	if err != nil {
		t.Fatalf("ParseLive: %v", err)
	}
	e := entries[0]
	if e.Name != "brand-new-9" {
		t.Errorf("name = %q, want the id", e.Name)
	}
	if e.Context != 200000 || e.Output != 32000 {
		t.Errorf("limits = %d/%d, want the generic 200K/32K", e.Context, e.Output)
	}
	if !e.Thinking {
		t.Errorf("an advanced model reasons: %+v", e)
	}
	if len(e.Efforts) != 0 {
		t.Errorf("an unmeasured model has no levels: %v", e.Efforts)
	}
}

// The live output limit is capped where amr-link takes less than it publishes.
func TestLiveOutputLimitIsCapped(t *testing.T) {
	body := []byte(`{"data":[` +
		`{"id":"glm-5.3-flash","metadata":{"context_limit":1048000,"context_budget":232000}},` +
		`{"id":"gemini-3.8-flash","metadata":{"context_limit":1048576,"context_budget":264000}}]}`)
	entries, err := ParseLive(body)
	if err != nil {
		t.Fatalf("ParseLive: %v", err)
	}
	byID := map[string]Entry{}
	for _, e := range entries {
		byID[e.UpstreamID] = e
	}
	if byID["glm-5.3-flash"].Output != 131072 {
		t.Errorf("glm output = %d, want the measured cap", byID["glm-5.3-flash"].Output)
	}
	if byID["gemini-3.8-flash"].Output != 65536 {
		t.Errorf("gemini output = %d, want the measured cap", byID["gemini-3.8-flash"].Output)
	}
}

// The seed is served before anyone signs in — and it is the WHOLE seed, with the
// public id applied.
func TestSeedEntriesCoverTheWholeSeed(t *testing.T) {
	cfg := testConfig()
	entries := Finalize(SeedEntries(cfg), cfg)
	if len(entries) != len(modelcaps.Seed) {
		t.Fatalf("entries = %d, want %d", len(entries), len(modelcaps.Seed))
	}
	for _, e := range entries {
		if e.PublicID != "opendesign/"+e.UpstreamID {
			t.Errorf("public id = %q", e.PublicID)
		}
	}
}

// A deny pattern is a glob, and deny always wins over allow.
func TestModelFilterGlobAndDenyWins(t *testing.T) {
	seed := SeedEntries(testConfig())

	deny := testConfig()
	deny.Models = config.ModelFilter{Deny: []string{"claude-*"}}
	entries := Finalize(seed, deny)
	for _, e := range entries {
		if e.UpstreamID == "claude-haiku-5.5" {
			t.Errorf("a denied family was published")
		}
	}
	if len(entries) != len(seed)-1 {
		t.Errorf("entries = %d, want one fewer", len(entries))
	}

	// deny wins over an allow that names the same model.
	both := testConfig()
	both.Models = config.ModelFilter{
		Allow: []string{"glm-*", "claude-haiku-5.5"},
		Deny:  []string{"claude-*"},
	}
	entries = Finalize(seed, both)
	for _, e := range entries {
		if e.UpstreamID == "claude-haiku-5.5" {
			t.Errorf("deny must win over allow")
		}
		if e.UpstreamID != "glm-5.3-flash" && e.UpstreamID != "glm-5.3-flashx" {
			t.Errorf("unexpected entry %q", e.UpstreamID)
		}
	}

	// The prefixed public id is matchable too, so an operator can copy whichever
	// id they see.
	allow := testConfig()
	allow.Models = config.ModelFilter{Allow: []string{"opendesign/deepseek-v4-pro"}}
	entries = Finalize(seed, allow)
	if len(entries) != 1 || entries[0].UpstreamID != "deepseek-v4-pro" {
		t.Errorf("entries = %+v", entries)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSnapshotLookupResolvesEveryIdForm(t *testing.T) {
	cfg := testConfig()
	snap := NewSnapshot(Finalize(SeedEntries(cfg), cfg))
	for _, want := range []string{"opendesign/glm-5.3-flash", "glm-5.3-flash", "openai/glm-5.3-flash"} {
		e, ok := snap.Lookup(want)
		if !ok {
			t.Errorf("Lookup(%q) failed", want)
			continue
		}
		if e.UpstreamID != "glm-5.3-flash" {
			t.Errorf("Lookup(%q) = %q", want, e.UpstreamID)
		}
	}
	if _, ok := snap.Lookup("nope"); ok {
		t.Errorf("an unknown model must not resolve")
	}
}

// Fetch classifies outcomes: a refusal is not the seed's problem, and a vendor
// hiccup is.
func TestFetchClassifiesOutcomes(t *testing.T) {
	cfg := testConfig()
	okTransport := func(status int, body []byte) Transport {
		return func(ctx context.Context, method, url string, headers map[string]string, b []byte) (int, []byte, error) {
			if headers["Authorization"] != "Bearer sk-amr-x" {
				t.Errorf("authorization = %q, want the account's runtime key", headers["Authorization"])
			}
			return status, body, nil
		}
	}

	refused := Fetch(context.Background(), okTransport(401, []byte(`{"error":{"message":"invalid_api_key"}}`)), cfg, "sk-amr-x")
	if refused.Kind != LiveRefused {
		t.Errorf("kind = %v, want LiveRefused", refused.Kind)
	}
	if refused.Message != "invalid_api_key" {
		t.Errorf("message = %q, want the vendor's own words", refused.Message)
	}

	forbidden := Fetch(context.Background(), okTransport(403, nil), cfg, "sk-amr-x")
	if forbidden.Kind != LiveRefused {
		t.Errorf("a 403 is a refusal too, kind = %v", forbidden.Kind)
	}

	broken := Fetch(context.Background(), okTransport(500, nil), cfg, "sk-amr-x")
	if broken.Kind != LiveUnavailable {
		t.Errorf("kind = %v, want LiveUnavailable", broken.Kind)
	}

	empty := Fetch(context.Background(), okTransport(200, []byte(`{"data":[]}`)), cfg, "sk-amr-x")
	if empty.Kind != LiveUnavailable {
		t.Errorf("an empty list is not a catalog, kind = %v", empty.Kind)
	}

	network := func(context.Context, string, string, map[string]string, []byte) (int, []byte, error) {
		return 0, nil, errors.New("dial tcp: refused")
	}
	failed := Fetch(context.Background(), network, cfg, "sk-amr-x")
	if failed.Kind != LiveUnavailable {
		t.Errorf("kind = %v, want LiveUnavailable", failed.Kind)
	}

	good := Fetch(context.Background(), okTransport(200, []byte(`{"data":[{"id":"glm-5.3-flash"}]}`)), cfg, "sk-amr-x")
	if good.Kind != LiveOK {
		t.Errorf("kind = %v, want LiveOK", good.Kind)
	}
	if len(good.Model) != 1 || good.Model[0].PublicID != "opendesign/glm-5.3-flash" {
		t.Errorf("models = %+v", good.Model)
	}
}

// A live catalog carries every model on all three wires; the entry's own wire is
// still the one it is declared on.
func TestLiveEntryKeepsItsDeclaredWire(t *testing.T) {
	body := []byte(`{"data":[{"id":"claude-haiku-5.5"},{"id":"gpt-6-luna"}]}`)
	entries, err := ParseLive(body)
	if err != nil {
		t.Fatalf("ParseLive: %v", err)
	}
	if entries[0].API != modelcaps.APIResponses {
		t.Errorf("claude-haiku-5.5 wire = %q", entries[0].API)
	}
	if entries[1].API != modelcaps.APIChatCompletions {
		t.Errorf("gpt-6-luna wire = %q", entries[1].API)
	}
}
