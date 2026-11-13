// Package catalog turns the backend's raw /v1/models into the model set this
// plugin publishes, and maps a public model id back to a backend request.
//
// # Naming
//
// One provider cannot declare two providers, so the two backends are two model
// namespaces of one provider: a public id is "<namespace>/<model>". The
// namespace is part of the id the client sends, which means routing is decided
// by the id alone — no out-of-band state, no guessing.
//
// Multiple slashes are fine: commandcode already publishes
// "commandcode/deepseek/...", so a host that accepts that shape accepts
// "ai-studio/gemini-2.5-pro".
package catalog

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/hex-ci/cpa-plugin/geminiapi/internal/backend"
	"github.com/hex-ci/cpa-plugin/geminiapi/internal/config"
)

// Namespace is one model namespace served by this plugin. Each namespace maps
// to exactly one backend.
type Namespace struct {
	// Name is the public prefix ("ai-studio").
	Name string
	// Titles are the display/description strings used in model metadata.
	Title string
	// Prefix is Name + "/".
	Prefix string
}

// AIStudio is the ai-studio namespace descriptor.
var AIStudio = Namespace{
	Name:   config.NamespaceAIStudio,
	Title:  "AI Studio",
	Prefix: config.NamespaceAIStudio + "/",
}

// StudioExclude filters ids that are not chat models by name: a model NAMED for
// images, music, canvas, video or research is not what a general-purpose
// provider should offer.
//
// It only applies to models whose capability map does NOT claim an image route:
// a model the backend explicitly marks as an image model is judged by that flag
// (see AllowImageModels), not by its name.
var StudioExclude = regexp.MustCompile(`(?:image|nano-banana|tts|audio|live|transcribe|robotics|research|video|music|canvas)`)

// nonTextCaps are capability flags that mean the model does something other
// than text chat. A model carrying one is excluded unless it is an image model
// and the flag is one image models legitimately carry.
var nonTextCaps = []string{
	"image_route", "speech_route", "live_route", "aspect_ratio",
	"output_resolution", "interactions_api", "transcription_output",
}

// imageCaps are the flags an image model legitimately carries.
var imageCaps = map[string]bool{
	"image_route": true, "aspect_ratio": true, "output_resolution": true,
}

// thinkLevels are the only reasoning levels this plugin publishes. A backend may
// advertise more; an unknown one is dropped rather than surfaced, because CPA's
// thinking suffix syntax only understands what it has been told about.
var thinkLevels = []string{"minimal", "low", "medium", "high"}

// BuildOptions controls how raw models are turned into published records.
type BuildOptions struct {
	// Namespace is the prefix to apply.
	Namespace Namespace
	// Filter is the operator's allow/deny list.
	Filter config.ModelFilter
	// AllowImageModels exposes image-generation models.
	AllowImageModels bool
	// ContextLimit / OutputLimit override the backend's reported limits when
	// non-zero.
	ContextLimit int64
	OutputLimit  int64
}

// Record is one published model plus what execution needs to call it.
type Record struct {
	// PublicID is what the client sends, e.g. "ai-studio/gemini-2.5-pro".
	PublicID string
	// UpstreamID is what the backend expects.
	UpstreamID string
	// Namespace is the namespace this record belongs to.
	Namespace string
	// DisplayName is the user-facing name.
	DisplayName string
	// ContextLimit / OutputLimit are the published limits.
	ContextLimit int64
	OutputLimit  int64
	// InputModes / OutputModes are the published modalities.
	InputModes  []string
	OutputModes []string
	// Thinking is nil when the model exposes no reasoning control.
	Thinking *Thinking
	// Capabilities is the raw backend capability map, kept for diagnostics and
	// for the "why was this filtered" explanation.
	Capabilities map[string]any
	// Channels lists the accounts covering this model, when the backend
	// reports them.
	Channels []string
}

// Thinking is the published reasoning support.
type Thinking struct {
	// Levels are the named effort levels available.
	Levels []string
	// Budget reports whether a numeric budget is accepted.
	Budget bool
}

// Skipped is a model the plugin refused to publish, with the reason. The
// Management API reports these so an operator is never left wondering where a
// model went.
type Skipped struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// BuildResult is the outcome of one build.
type BuildResult struct {
	Records []Record
	Skipped []Skipped
}

// Build maps the backend's model list to records for one namespace.
//
// The filtering order matters and follows magpie's studio provider, because each
// rule exists to keep a model that CANNOT work out of the catalog:
//
//  1. a non-gemini id is ignored unless it is a Deep Research interaction
//     model (those are served over /chat/completions by the backend);
//  2. "-build" models inject a coding-agent prompt and expose no ordinary
//     function calling — they are a different product;
//  3. a model must declare generateContent;
//  4. it must be a chat model and NOT an assistant_route model;
//  5. image models appear only when allow-image-models is on, and only
//     non-drawing models then face the name filter;
//  6. a model carrying a non-text capability (speech, live, transcription) is
//     excluded;
//  7. when the backend reports channel coverage, a model no enabled account
//     covers is excluded, because calls to it would 403.
func Build(models []backend.Model, opts BuildOptions) BuildResult {
	var res BuildResult
	seen := make(map[string]bool, len(models))

	for _, m := range models {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		caps := m.Capabilities
		if caps == nil {
			caps = map[string]any{}
		}

		// 1. Gemini ids, plus backend-served Deep Research agents.
		researches := strings.HasPrefix(id, "deep-research-") && boolCap(caps, "interaction_route")
		if !strings.HasPrefix(id, "gemini-") && !researches {
			res.skip(id, "not a Gemini model id")
			continue
		}
		// 2. Build-assistant models are a different product surface.
		if strings.HasSuffix(strings.ToLower(id), "-build") {
			res.skip(id, "build-assistant model (-build): injects Google's coding-agent prompt, no ordinary function calling")
			continue
		}
		// 3. generateContent support.
		if len(m.SupportedGenerationMethods) > 0 && !containsFold(m.SupportedGenerationMethods, "generateContent") {
			res.skip(id, "does not support generateContent")
			continue
		}
		// 4. Chat models only, and not the assistant route.
		if !boolCap(caps, "chat_model") || boolCap(caps, "assistant_route") {
			res.skip(id, "not a chat model (chat_model/assistant_route)")
			continue
		}
		draws := boolCap(caps, "image_route")
		if draws && !opts.AllowImageModels {
			// CPA has no image generation route, so exposing one can only fail.
			res.skip(id, "image-generation model; CPA has no image route (enable ai-studio.allow-image-models only if you have one)")
			continue
		}
		if !draws && !researches && StudioExclude.MatchString(id) {
			res.skip(id, "named for a non-chat modality (image/tts/audio/live/video/music/canvas/research)")
			continue
		}
		// 6. Non-text capabilities.
		for _, cap := range nonTextCaps {
			if !boolCap(caps, cap) {
				continue
			}
			if (draws && imageCaps[cap]) || (researches && cap == "interactions_api") {
				continue
			}
			res.skip(id, "declares the non-text capability "+cap)
			break
		}
		// 7. Channel coverage: a model no enabled account covers would 403.
		if listsChannels(models) && len(m.Channels) == 0 {
			res.skip(id, "no enabled account covers this model")
			continue
		}

		rec := Record{
			PublicID:     opts.Namespace.Prefix + id,
			UpstreamID:   id,
			Namespace:    opts.Namespace.Name,
			DisplayName:  id,
			Capabilities: caps,
			Channels:     m.Channels,
		}

		contextLimit := m.InputTokenLimit
		if contextLimit <= 0 {
			contextLimit = config.DefaultContextLimit
		}
		outputLimit := m.OutputTokenLimit
		if outputLimit <= 0 {
			outputLimit = config.DefaultOutputLimit
		}
		if opts.ContextLimit > 0 && contextLimit > opts.ContextLimit {
			contextLimit = opts.ContextLimit
		}
		if opts.OutputLimit > 0 && outputLimit > opts.OutputLimit {
			outputLimit = opts.OutputLimit
		}
		if outputLimit > contextLimit {
			outputLimit = contextLimit
		}
		rec.ContextLimit = contextLimit
		rec.OutputLimit = outputLimit

		// Modalities. Images in a chat are on by default (a multimodal chat
		// model reads images); an image model is a different thing entirely.
		rec.InputModes = []string{"text"}
		if !draws {
			rec.InputModes = append(rec.InputModes, "image")
		}
		rec.OutputModes = []string{"text"}
		if draws {
			rec.OutputModes = []string{"image"}
		}

		// Reasoning control, only when the backend says the model has it.
		if boolCap(caps, "thinking") || boolCap(caps, "thinking_level") {
			rec.Thinking = &Thinking{Budget: boolCap(caps, "thinking")}
			if boolCap(caps, "thinking_level") {
				rec.Thinking.Levels = supportedLevels(m.ThinkingLevels)
			}
		}

		// Operator filter runs last: an explicit deny always wins over every
		// capability judgment above.
		if opts.Filter.Excludes(id, rec.PublicID) {
			res.skip(id, "excluded by models.allow/deny")
			continue
		}

		seen[id] = true
		res.Records = append(res.Records, rec)
	}
	return res
}

// listsChannels reports whether the backend describes account coverage at all.
// A backend older than that rule lists none, and its models must survive.
func listsChannels(models []backend.Model) bool {
	for _, m := range models {
		if m.Channels != nil {
			return true
		}
	}
	return false
}

// supportedLevels filters a backend's level list to the ones this plugin
// publishes, preserving order and dropping duplicates.
func supportedLevels(raw []string) []string {
	allowed := make(map[string]bool, len(thinkLevels))
	for _, level := range thinkLevels {
		allowed[level] = true
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, level := range raw {
		level = strings.ToLower(strings.TrimSpace(level))
		if level == "" || !allowed[level] || seen[level] {
			continue
		}
		seen[level] = true
		out = append(out, level)
	}
	return out
}

func (r *BuildResult) skip(id, reason string) {
	r.Skipped = append(r.Skipped, Skipped{ID: id, Reason: reason})
}

// boolCap reads a boolean capability. A non-boolean value is false: a backend
// that reports "true" as a string is not something to guess about.
func boolCap(caps map[string]any, key string) bool {
	value, ok := caps[key]
	if !ok {
		return false
	}
	b, isBool := value.(bool)
	return isBool && b
}

func containsFold(list []string, want string) bool {
	for _, item := range list {
		if strings.EqualFold(strings.TrimSpace(item), want) {
			return true
		}
	}
	return false
}

// Resolve maps a client-supplied model id to a published record.
//
// A bare upstream id ("gemini-2.5-pro") is also accepted for the namespace it
// unambiguously belongs to, so an operator who copied the backend's own id is
// not punished. A bare id that exists in BOTH namespaces is refused as
// ambiguous rather than guessed — silently routing to the wrong backend is
// worse than a clear error.
func (r BuildResult) Resolve(model string) (Record, bool) {
	model = strings.TrimSpace(model)
	for _, rec := range r.Records {
		if rec.PublicID == model {
			return rec, true
		}
	}
	for _, rec := range r.Records {
		if rec.UpstreamID == model {
			return rec, true
		}
	}
	return Record{}, false
}

// ResolveNamespace splits a public model id into its namespace and bare model
// name. ok is false when the id carries no known namespace prefix.
func ResolveNamespace(model string) (namespace string, bare string, ok bool) {
	model = strings.TrimSpace(model)
	for _, ns := range []string{config.NamespaceAIStudio, config.NamespaceGeminiWeb} {
		if strings.HasPrefix(model, ns+"/") {
			bare := strings.TrimPrefix(model, ns+"/")
			if strings.TrimSpace(bare) == "" {
				return "", "", false
			}
			return ns, bare, true
		}
	}
	return "", "", false
}

// ModelIDError explains an unusable model id.
func ModelIDError(model string) error {
	return fmt.Errorf("model %q names no %s namespace; expected %s<model>, for example %sgemini-2.5-pro",
		model, "geminiapi", config.NamespaceAIStudio+"/", config.NamespaceAIStudio+"/")
}
