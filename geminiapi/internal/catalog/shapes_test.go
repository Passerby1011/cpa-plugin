package catalog

// The namespace-shape probe: the acceptance criterion for the split between the
// two backends.
//
// # Why these cases exist
//
// Both namespaces are published by ONE catalog package, but the two backends
// describe their models in incompatible shapes. A single ruleset applied to both
// is what shipped a namespace with ZERO models:
//
//	gemini-web2api-go  {"id":"gemini-2.5-pro","object":"model","owned_by":"google"}
//	                   — no capabilities, no generation methods, no limits.
//	AIStudio2API       the same id PLUS a capabilities map
//	                   (chat_model / assistant_route / image_route /
//	                   thinking_level / supported_generation_methods / channels).
//
// The Studio rules require chat_model=true, which an OpenAI-shaped entry can never
// satisfy (a missing field reads as false), so applying them to gemini-web
// publishes nothing. TestGeminiWebShapePublishes and TestStudioShapePublishes are
// the two positive controls; TestEmptyBackendListIsAnError is the negative one:
// an empty backend list must surface a readable error instead of a silent empty
// catalog.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hex-ci/cpa-plugin/geminiapi/internal/backend"
	"github.com/hex-ci/cpa-plugin/geminiapi/internal/config"
)

// openAIShape is gemini-web2api-go's real /v1/models body: id/object/owned_by
// and nothing else. The thinking twins are separate ids, exactly as that backend
// publishes them.
const openAIShape = `{"object":"list","data":[
 {"id":"gemini-2.5-pro","object":"model","owned_by":"google"},
 {"id":"gemini-2.5-flash","object":"model","owned_by":"google"},
 {"id":"gemini-3-pro","object":"model","owned_by":"google"},
 {"id":"gemini-2.5-pro-thinking","object":"model","owned_by":"google"},
 {"id":"gemini-2.5-pro-thinking-medium","object":"model","owned_by":"google"},
 {"id":"gemini-2.5-flash-image","object":"model","owned_by":"google"},
 {"id":"gemini-2.5-pro-live","object":"model","owned_by":"google"}]}`

// studioShape is AIStudio2API's real /v1/models body: a capability map per model.
const studioShape = `{"object":"list","data":[
 {"id":"gemini-2.5-pro","object":"model","owned_by":"google",
  "capabilities":{"chat_model":true,"thinking":true,"thinking_level":true},
  "thinking_levels":["low","medium","high"],"supported_generation_methods":["generateContent"],
  "input_token_limit":1048576,"output_token_limit":65536},
 {"id":"gemini-2.5-flash","object":"model","owned_by":"google",
  "capabilities":{"chat_model":true},"supported_generation_methods":["generateContent"],
  "input_token_limit":1048576,"output_token_limit":8192},
 {"id":"gemini-2.5-flash-image","object":"model","owned_by":"google",
  "capabilities":{"chat_model":true,"image_route":true},
  "supported_generation_methods":["generateContent"],"input_token_limit":32768,"output_token_limit":8192}]}`

func decodeProbeModels(t *testing.T, body string) []backend.Model {
	t.Helper()
	var list backend.ModelList
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode probe body: %v", err)
	}
	return list.Data
}

// TestGeminiWebShapePublishes is positive control 1: an OpenAI-shaped list must
// publish every usable model WITHOUT any capability information.
func TestGeminiWebShapePublishes(t *testing.T) {
	res, err := Build(decodeProbeModels(t, openAIShape), BuildOptions{Namespace: GeminiWeb})
	if err != nil {
		t.Fatalf("gemini-web shape published nothing: %v", err)
	}
	if len(res.Records) == 0 {
		t.Fatalf("gemini-web shape published 0 models (skipped %d)", len(res.Skipped))
	}
	// Every published record must carry the gemini-web prefix, and its upstream
	// id must be the backend's own bare id.
	for _, rec := range res.Records {
		if !strings.HasPrefix(rec.PublicID, config.NamespaceGeminiWeb+"/") {
			t.Fatalf("record %q lacks the gemini-web/ prefix", rec.PublicID)
		}
		if rec.Namespace != config.NamespaceGeminiWeb {
			t.Fatalf("record %q claims namespace %q", rec.PublicID, rec.Namespace)
		}
	}
	// The three plain chat models must all be there.
	ids := make([]string, 0, len(res.Records))
	for _, rec := range res.Records {
		ids = append(ids, rec.PublicID)
	}
	for _, want := range []string{
		"gemini-web/gemini-2.5-pro",
		"gemini-web/gemini-2.5-flash",
		"gemini-web/gemini-3-pro",
	} {
		if !contains(ids, want) {
			t.Fatalf("%s was not published (published %v)", want, ids)
		}
	}
	// A thinking twin is folded into its base: the base must expose levels and
	// the twin must NOT be a second entry.
	if contains(ids, "gemini-web/gemini-2.5-pro-thinking") {
		t.Fatalf("a thinking twin was published as its own model: %v", ids)
	}
	for _, rec := range res.Records {
		if rec.PublicID != "gemini-web/gemini-2.5-pro" {
			continue
		}
		if rec.Thinking == nil {
			t.Fatal("gemini-2.5-pro has twins but published no reasoning levels")
		}
		if got := strings.Join(rec.Thinking.Levels, ","); got != "low,medium,high" {
			t.Fatalf("levels = %q, want low,medium,high", got)
		}
		if got := rec.UpstreamModelForLevel("high"); got != "gemini-2.5-pro-thinking" {
			t.Fatalf("high level routes to %q, want the -thinking twin", got)
		}
		if got := rec.UpstreamModelForLevel("medium"); got != "gemini-2.5-pro-thinking-medium" {
			t.Fatalf("medium level routes to %q, want the -thinking-medium twin", got)
		}
		if got := rec.UpstreamModelForLevel("low"); got != "gemini-2.5-pro" {
			t.Fatalf("low level routes to %q, want the plain id", got)
		}
	}
	// Media-named models are not chat models for this backend either. The
	// filter mirrors magpie's own list (image/music/canvas/video/research) —
	// "live" is deliberately NOT in it, so a -live id survives, exactly as it
	// does in magpie.
	if contains(ids, "gemini-web/gemini-2.5-flash-image") {
		t.Fatalf("an image-named model was published: %v", ids)
	}
}

// TestStudioShapePublishes is positive control 2: the capability rules must keep
// working for the backend they were written for.
func TestStudioShapePublishes(t *testing.T) {
	res, err := Build(decodeProbeModels(t, studioShape), BuildOptions{Namespace: AIStudio})
	if err != nil {
		t.Fatalf("studio shape published nothing: %v", err)
	}
	ids := make([]string, 0, len(res.Records))
	for _, rec := range res.Records {
		ids = append(ids, rec.PublicID)
		if rec.Namespace != config.NamespaceAIStudio {
			t.Fatalf("record %q claims namespace %q", rec.PublicID, rec.Namespace)
		}
	}
	if !contains(ids, "ai-studio/gemini-2.5-pro") || !contains(ids, "ai-studio/gemini-2.5-flash") {
		t.Fatalf("chat models were not published: %v", ids)
	}
	// The image model stays out unless explicitly allowed — CPA has no image
	// generation route.
	if contains(ids, "ai-studio/gemini-2.5-flash-image") {
		t.Fatalf("an image model was published with allow-image-models off: %v", ids)
	}
	// Backend-reported limits survive.
	for _, rec := range res.Records {
		if rec.PublicID == "ai-studio/gemini-2.5-pro" {
			if rec.ContextLimit != 1048576 || rec.OutputLimit != 65536 {
				t.Fatalf("limits = %d/%d, want the backend's 1048576/65536", rec.ContextLimit, rec.OutputLimit)
			}
			if rec.Thinking == nil || strings.Join(rec.Thinking.Levels, ",") != "low,medium,high" {
				t.Fatalf("thinking = %+v, want low/medium/high", rec.Thinking)
			}
		}
	}
}

// TestStudioShapeStillFailsOnOpenAIShape is the CROSS control: the Studio rules
// must NOT be the ones applied to gemini-web, which is what proves the two
// rulesets are genuinely different rather than one ruleset renamed.
func TestStudioShapeStillFailsOnOpenAIShape(t *testing.T) {
	res := BuildStudio(decodeProbeModels(t, openAIShape), BuildOptions{Namespace: AIStudio})
	if len(res.Records) != 0 {
		t.Fatalf("the Studio rules published %d OpenAI-shaped models; this test exists to prove the shapes differ", len(res.Records))
	}
	if len(res.Skipped) == 0 {
		t.Fatal("the Studio rules skipped the models without a reason")
	}
	if !strings.Contains(res.Skipped[0].Reason, "chat_model") {
		t.Fatalf("the skip reason does not name the missing capability: %q", res.Skipped[0].Reason)
	}
}

// TestEmptyBackendListIsAnError is the negative control: a backend that lists
// nothing must produce a readable error, never a silent empty catalog.
func TestEmptyBackendListIsAnError(t *testing.T) {
	for _, ns := range []Namespace{GeminiWeb, AIStudio} {
		res, err := Build(nil, BuildOptions{Namespace: ns})
		if err == nil {
			t.Fatalf("namespace %s accepted an empty model list without an error", ns.Name)
		}
		if !strings.Contains(err.Error(), "0 models") {
			t.Fatalf("namespace %s error does not state the count: %v", ns.Name, err)
		}
		if !strings.Contains(err.Error(), "backend listed no models at all") {
			t.Fatalf("namespace %s error does not explain the cause: %v", ns.Name, err)
		}
		if len(res.Records) != 0 {
			t.Fatalf("namespace %s returned records for an empty list", ns.Name)
		}
	}
}

// TestFilteredOutEverythingIsAnError: an operator filter that excludes every
// model is also a zero-model catalog, and the error must name what was skipped.
func TestFilteredOutEverythingIsAnError(t *testing.T) {
	res, err := Build(decodeProbeModels(t, openAIShape), BuildOptions{
		Namespace: GeminiWeb,
		Filter:    config.ModelFilter{Deny: []string{"gemini-*"}},
	})
	if err == nil {
		t.Fatal("a fully filtered catalog was accepted without an error")
	}
	if !strings.Contains(err.Error(), "filtered out") {
		t.Fatalf("error does not say the models were filtered: %v", err)
	}
	if !strings.Contains(err.Error(), "models.allow/deny") {
		t.Fatalf("error does not name the operator filter: %v", err)
	}
	if len(res.Records) != 0 {
		t.Fatalf("records = %d, want 0", len(res.Records))
	}
}

// TestEmptyErrorNamesTheNamespace: the message an operator reads must say which
// namespace went empty, because two of them exist.
func TestEmptyErrorNamesTheNamespace(t *testing.T) {
	for _, ns := range []Namespace{GeminiWeb, AIStudio} {
		_, err := Build(nil, BuildOptions{Namespace: ns})
		if err == nil {
			t.Fatalf("namespace %s: no error", ns.Name)
		}
		if !strings.Contains(err.Error(), ns.Name) {
			t.Fatalf("error %q does not name namespace %s", err, ns.Name)
		}
	}
}

// TestUnknownNamespaceIsRefused: a namespace with no ruleset must not silently
// borrow another one's.
func TestUnknownNamespaceIsRefused(t *testing.T) {
	if _, err := Build(decodeProbeModels(t, openAIShape), BuildOptions{Namespace: Namespace{Name: "mystery", Prefix: "mystery/"}}); err == nil {
		t.Fatal("an unknown namespace was built without an error")
	}
}

// contains reports whether list has want.
func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
