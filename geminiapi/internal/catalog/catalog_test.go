package catalog

import (
	"strings"
	"testing"

	"github.com/hex-ci/cpa-plugin/geminiapi/internal/backend"
	"github.com/hex-ci/cpa-plugin/geminiapi/internal/config"
)

func chatModel(id string, extra map[string]any) backend.Model {
	caps := map[string]any{"chat_model": true}
	for k, v := range extra {
		caps[k] = v
	}
	return backend.Model{
		ID:                         id,
		Object:                     "model",
		Capabilities:               caps,
		SupportedGenerationMethods: []string{"generateContent"},
		InputTokenLimit:            1000000,
		OutputTokenLimit:           8192,
	}
}

func buildIDs(t *testing.T, models []backend.Model, opts BuildOptions) []string {
	t.Helper()
	if opts.Namespace.Name == "" {
		opts.Namespace = AIStudio
	}
	res := Build(models, opts)
	ids := make([]string, 0, len(res.Records))
	for _, rec := range res.Records {
		ids = append(ids, rec.PublicID)
	}
	return ids
}

// TestBuildPublishesNamespacePrefixedIDs is the acceptance criterion
// "ai-studio/ 前缀路由": every published id carries the namespace.
func TestBuildPublishesNamespacePrefixedIDs(t *testing.T) {
	ids := buildIDs(t, []backend.Model{
		chatModel("gemini-2.5-pro", nil),
		chatModel("gemini-2.5-flash", nil),
	}, BuildOptions{})
	for _, id := range ids {
		if !strings.HasPrefix(id, "ai-studio/") {
			t.Fatalf("published id %q lacks the ai-studio/ prefix", id)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("ids = %v, want 2", ids)
	}
}

func TestBuildKeepsUpstreamIDForExecution(t *testing.T) {
	res := Build([]backend.Model{chatModel("gemini-2.5-pro", nil)}, BuildOptions{Namespace: AIStudio})
	if len(res.Records) != 1 {
		t.Fatalf("records = %d", len(res.Records))
	}
	rec := res.Records[0]
	if rec.UpstreamID != "gemini-2.5-pro" {
		t.Fatalf("upstream id = %q: the backend must receive its own id, not the prefixed one", rec.UpstreamID)
	}
	if rec.PublicID != "ai-studio/gemini-2.5-pro" {
		t.Fatalf("public id = %q", rec.PublicID)
	}
}

// TestBuildDropsBuildModels covers the "-build" filter requirement.
func TestBuildDropsBuildModels(t *testing.T) {
	ids := buildIDs(t, []backend.Model{
		chatModel("gemini-2.5-pro", nil),
		chatModel("gemini-2.5-flash-build", nil),
		chatModel("gemini-2.5-pro-BUILD", nil),
	}, BuildOptions{})
	for _, id := range ids {
		if strings.HasSuffix(strings.ToLower(id), "-build") {
			t.Fatalf("build model %q was published", id)
		}
	}
	if len(ids) != 1 || ids[0] != "ai-studio/gemini-2.5-pro" {
		t.Fatalf("ids = %v", ids)
	}
}

// TestBuildDropsAssistantRouteModels covers the assistant_route filter.
func TestBuildDropsAssistantRouteModels(t *testing.T) {
	ids := buildIDs(t, []backend.Model{
		chatModel("gemini-2.5-pro", nil),
		chatModel("gemini-2.5-pro-route", map[string]any{"assistant_route": true}),
		chatModel("gemini-3-pro-preview", map[string]any{"chat_model": false}),
	}, BuildOptions{})
	if len(ids) != 1 || ids[0] != "ai-studio/gemini-2.5-pro" {
		t.Fatalf("ids = %v, want only the plain chat model", ids)
	}
}

// TestBuildExcludesImageModelsByDefault covers "图片生成模型只在支持时列出".
func TestBuildExcludesImageModelsByDefault(t *testing.T) {
	imageModel := chatModel("gemini-2.5-flash-image", map[string]any{"image_route": true, "aspect_ratio": true})
	ids := buildIDs(t, []backend.Model{chatModel("gemini-2.5-pro", nil), imageModel}, BuildOptions{})
	if len(ids) != 1 || ids[0] != "ai-studio/gemini-2.5-pro" {
		t.Fatalf("ids = %v: an image model must not be published when allow-image-models is off", ids)
	}

	idsWithImages := buildIDs(t, []backend.Model{chatModel("gemini-2.5-pro", nil), imageModel},
		BuildOptions{AllowImageModels: true})
	if len(idsWithImages) != 2 {
		t.Fatalf("ids = %v: an image model must be published when explicitly allowed", idsWithImages)
	}
	// And it must be marked as an image OUTPUT model, not a text one.
	res := Build([]backend.Model{imageModel}, BuildOptions{Namespace: AIStudio, AllowImageModels: true})
	if len(res.Records) != 1 {
		t.Fatalf("records = %d", len(res.Records))
	}
	if len(res.Records[0].OutputModes) != 1 || res.Records[0].OutputModes[0] != "image" {
		t.Fatalf("output modes = %v, want [image]", res.Records[0].OutputModes)
	}
}

// TestBuildReportsWhyAModelWasSkipped: a missing model must be explainable.
func TestBuildReportsWhyAModelWasSkipped(t *testing.T) {
	res := Build([]backend.Model{chatModel("gemini-2.5-flash-image", map[string]any{"image_route": true})}, BuildOptions{Namespace: AIStudio})
	if len(res.Records) != 0 {
		t.Fatalf("image model was published: %v", res.Records)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("skipped = %v", res.Skipped)
	}
	if !strings.Contains(res.Skipped[0].Reason, "image") {
		t.Fatalf("skip reason does not explain the image route: %q", res.Skipped[0].Reason)
	}
}

// TestBuildExcludesNonTextCapabilities: speech/live/transcription models cannot
// serve a chat completion.
func TestBuildExcludesNonTextCapabilities(t *testing.T) {
	ids := buildIDs(t, []backend.Model{
		chatModel("gemini-2.5-pro", nil),
		chatModel("gemini-live-2.5-flash", map[string]any{"live_route": true}),
		chatModel("gemini-2.5-pro-tts", map[string]any{"speech_route": true}),
	}, BuildOptions{})
	if len(ids) != 1 {
		t.Fatalf("ids = %v, want only the plain chat model", ids)
	}
}

// TestBuildExcludesModelsWithNoChannelCoverage covers the 403 guard.
func TestBuildExcludesModelsWithNoChannelCoverage(t *testing.T) {
	covered := chatModel("gemini-2.5-pro", nil)
	covered.Channels = []string{"acc-1"}
	uncovered := chatModel("gemini-2.5-ultra", nil)
	uncovered.Channels = []string{}

	ids := buildIDs(t, []backend.Model{covered, uncovered}, BuildOptions{})
	if len(ids) != 1 || ids[0] != "ai-studio/gemini-2.5-pro" {
		t.Fatalf("ids = %v: a model no account covers must be excluded", ids)
	}
}

// TestBuildKeepsModelsWhenBackendListsNoChannels: an older backend describes
// no coverage, so every model must survive.
func TestBuildKeepsModelsWhenBackendListsNoChannels(t *testing.T) {
	ids := buildIDs(t, []backend.Model{
		chatModel("gemini-2.5-pro", nil),
		chatModel("gemini-2.5-flash", nil),
	}, BuildOptions{})
	if len(ids) != 2 {
		t.Fatalf("ids = %v, want both models kept", ids)
	}
}

// TestBuildPublishesThinkingLevels covers "minimal/low/medium/high 思考档按目录给".
func TestBuildPublishesThinkingLevels(t *testing.T) {
	model := chatModel("gemini-2.5-pro", map[string]any{"thinking": true, "thinking_level": true})
	model.ThinkingLevels = []string{"low", "medium", "high", "minimal"}

	res := Build([]backend.Model{model}, BuildOptions{Namespace: AIStudio})
	if len(res.Records) != 1 {
		t.Fatalf("records = %d", len(res.Records))
	}
	thinking := res.Records[0].Thinking
	if thinking == nil {
		t.Fatal("a thinking-capable model published no reasoning support")
	}
	if len(thinking.Levels) != 4 {
		t.Fatalf("levels = %v, want minimal/low/medium/high", thinking.Levels)
	}
	if !thinking.Budget {
		t.Fatal("thinking: true must publish budget support")
	}
}

// TestBuildFiltersUnknownThinkingLevels: an unknown level cannot be expressed
// in CPA's suffix syntax, so it is dropped rather than surfaced.
func TestBuildFiltersUnknownThinkingLevels(t *testing.T) {
	model := chatModel("gemini-2.5-pro", map[string]any{"thinking_level": true})
	model.ThinkingLevels = []string{"low", "extreme", "", "LOW"}

	res := Build([]backend.Model{model}, BuildOptions{Namespace: AIStudio})
	levels := res.Records[0].Thinking.Levels
	if len(levels) != 1 || levels[0] != "low" {
		t.Fatalf("levels = %v, want [low]", levels)
	}
}

func TestBuildOmitsThinkingWhenBackendSaysNo(t *testing.T) {
	res := Build([]backend.Model{chatModel("gemini-2.5-pro", nil)}, BuildOptions{Namespace: AIStudio})
	if res.Records[0].Thinking != nil {
		t.Fatal("a model without thinking capabilities published reasoning support")
	}
}

// TestBuildInputModes: images in a chat are on by default.
func TestBuildInputModes(t *testing.T) {
	res := Build([]backend.Model{chatModel("gemini-2.5-pro", nil)}, BuildOptions{Namespace: AIStudio})
	modes := res.Records[0].InputModes
	found := false
	for _, m := range modes {
		if m == "image" {
			found = true
		}
	}
	if !found {
		t.Fatalf("input modes = %v: image input must be enabled by default", modes)
	}
}

// TestBuildAppliesOperatorFilterLast: a deny beats every capability judgment.
func TestBuildAppliesOperatorFilterLast(t *testing.T) {
	filter := config.ModelFilter{Deny: []string{"gemini-2.5-flash"}}
	ids := buildIDs(t, []backend.Model{
		chatModel("gemini-2.5-pro", nil),
		chatModel("gemini-2.5-flash", nil),
	}, BuildOptions{Filter: filter})
	if len(ids) != 1 || ids[0] != "ai-studio/gemini-2.5-pro" {
		t.Fatalf("ids = %v", ids)
	}
}

// TestBuildFilterMatchesPublicID lets an operator paste the id they SEE.
func TestBuildFilterMatchesPublicID(t *testing.T) {
	filter := config.ModelFilter{Allow: []string{"ai-studio/gemini-2.5-pro"}}
	ids := buildIDs(t, []backend.Model{
		chatModel("gemini-2.5-pro", nil),
		chatModel("gemini-2.5-flash", nil),
	}, BuildOptions{Filter: filter})
	if len(ids) != 1 || ids[0] != "ai-studio/gemini-2.5-pro" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestBuildDeduplicatesModels(t *testing.T) {
	ids := buildIDs(t, []backend.Model{
		chatModel("gemini-2.5-pro", nil),
		chatModel("gemini-2.5-pro", nil),
	}, BuildOptions{})
	if len(ids) != 1 {
		t.Fatalf("duplicate ids were published: %v", ids)
	}
}

func TestBuildUsesBackendLimits(t *testing.T) {
	m := chatModel("gemini-2.5-pro", nil)
	m.InputTokenLimit = 500000
	m.OutputTokenLimit = 4096
	res := Build([]backend.Model{m}, BuildOptions{Namespace: AIStudio})
	if res.Records[0].ContextLimit != 500000 || res.Records[0].OutputLimit != 4096 {
		t.Fatalf("limits = %d/%d, want the backend's 500000/4096",
			res.Records[0].ContextLimit, res.Records[0].OutputLimit)
	}
}

func TestBuildClampsLimitsToConfiguredMaximums(t *testing.T) {
	m := chatModel("gemini-2.5-pro", nil)
	m.InputTokenLimit = 500000
	m.OutputTokenLimit = 20000
	res := Build([]backend.Model{m}, BuildOptions{Namespace: AIStudio, ContextLimit: 200000, OutputLimit: 8192})
	if res.Records[0].ContextLimit != 200000 {
		t.Fatalf("context = %d, want the configured 200000", res.Records[0].ContextLimit)
	}
	if res.Records[0].OutputLimit != 8192 {
		t.Fatalf("output = %d, want the configured 8192", res.Records[0].OutputLimit)
	}
}

func TestBuildNeverExceedsContextWithOutput(t *testing.T) {
	m := chatModel("gemini-2.5-pro", nil)
	m.InputTokenLimit = 1000
	m.OutputTokenLimit = 50000
	res := Build([]backend.Model{m}, BuildOptions{Namespace: AIStudio})
	if res.Records[0].OutputLimit > res.Records[0].ContextLimit {
		t.Fatalf("output %d exceeds context %d", res.Records[0].OutputLimit, res.Records[0].ContextLimit)
	}
}

// --- resolution tests ---

func TestResolveAcceptsPublicID(t *testing.T) {
	res := Build([]backend.Model{chatModel("gemini-2.5-pro", nil)}, BuildOptions{Namespace: AIStudio})
	rec, ok := res.Resolve("ai-studio/gemini-2.5-pro")
	if !ok {
		t.Fatal("Resolve rejected a published public id")
	}
	if rec.UpstreamID != "gemini-2.5-pro" {
		t.Fatalf("upstream = %q", rec.UpstreamID)
	}
}

func TestResolveAcceptsBareUpstreamID(t *testing.T) {
	res := Build([]backend.Model{chatModel("gemini-2.5-pro", nil)}, BuildOptions{Namespace: AIStudio})
	rec, ok := res.Resolve("gemini-2.5-pro")
	if !ok {
		t.Fatal("Resolve rejected a bare upstream id that exists in exactly one namespace")
	}
	if rec.PublicID != "ai-studio/gemini-2.5-pro" {
		t.Fatalf("public = %q", rec.PublicID)
	}
}

func TestResolveRejectsUnknownModel(t *testing.T) {
	res := Build([]backend.Model{chatModel("gemini-2.5-pro", nil)}, BuildOptions{Namespace: AIStudio})
	if _, ok := res.Resolve("ai-studio/gemini-9.9-imaginary"); ok {
		t.Fatal("Resolve accepted a model that was never published")
	}
}

// TestResolveNamespaceReportsUnknownPrefix documents the routing gate.
func TestResolveNamespaceSplitsKnownPrefixes(t *testing.T) {
	ns, bare, ok := ResolveNamespace("ai-studio/gemini-2.5-pro")
	if !ok || ns != config.NamespaceAIStudio || bare != "gemini-2.5-pro" {
		t.Fatalf("got ns=%q bare=%q ok=%t", ns, bare, ok)
	}
}

func TestResolveNamespaceRejectsBareAndEmptyTail(t *testing.T) {
	if _, _, ok := ResolveNamespace("gemini-2.5-pro"); ok {
		t.Fatal("a namespace-less id was accepted")
	}
	if _, _, ok := ResolveNamespace("ai-studio/"); ok {
		t.Fatal("an id with an empty model name was accepted")
	}
	if _, _, ok := ResolveNamespace("unknownns/model"); ok {
		t.Fatal("an unknown namespace was accepted")
	}
}

func TestResolveNamespaceAllowsExtraSlashes(t *testing.T) {
	// A multi-slash model name must still route by its FIRST segment.
	ns, bare, ok := ResolveNamespace("ai-studio/models/gemini-2.5-pro")
	if !ok || ns != config.NamespaceAIStudio || bare != "models/gemini-2.5-pro" {
		t.Fatalf("got ns=%q bare=%q ok=%t", ns, bare, ok)
	}
}

func TestModelIDErrorNamesTheExpectedShape(t *testing.T) {
	msg := ModelIDError("gemini-2.5-pro").Error()
	if !strings.Contains(msg, "ai-studio/") {
		t.Fatalf("error does not show the expected prefix: %s", msg)
	}
}
