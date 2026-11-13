// Package catalog turns a backend's raw /v1/models into the model set this
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
//
// # Two backends, two rulesets
//
// The two backends describe their models differently, and one ruleset cannot
// serve both:
//
//	ai-studio (AIStudio2API)        a CAPABILITY map per model: chat_model,
//	                                assistant_route, image_route, thinking_level.
//	                                Filtering by capability is the only way to
//	                                tell a chat model from a draw model.
//
//	gemini-web (gemini-web2api-go)  an OpenAI-shaped list: id/object/owned_by
//	                                and NOTHING else. No capability map, no
//	                                generation methods, no token limits. A rule
//	                                requiring chat_model=true would publish ZERO
//	                                models, which is the defect this split
//	                                exists to remove.
//
// So BuildStudio applies the capability rules and BuildGeminiWeb publishes
// whatever the backend lists (after a name-level media filter and the operator
// filter). Neither returns an empty result silently: Build reports the skip
// reasons through BuildResult.EmptyError.
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
	// Backend is the flavor that serves it.
	Backend backend.FlavorID
	// Title is the display/description string used in model metadata.
	Title string
	// Prefix is Name + "/".
	Prefix string
}

// AIStudio is the ai-studio namespace descriptor, backed by AIStudio2API.
var AIStudio = Namespace{
	Name:    config.NamespaceAIStudio,
	Backend: backend.FlavorAIStudio,
	Title:   "AI Studio",
	Prefix:  config.NamespaceAIStudio + "/",
}

// GeminiWeb is the gemini-web namespace descriptor, backed by gemini-web2api-go.
var GeminiWeb = Namespace{
	Name:    config.NamespaceGeminiWeb,
	Backend: backend.FlavorGeminiWeb,
	Title:   "Gemini Web",
	Prefix:  config.NamespaceGeminiWeb + "/",
}

// Namespaces lists every namespace this build serves, in publication order.
var Namespaces = []Namespace{GeminiWeb, AIStudio}

// StudioExclude filters ids that are not chat models by name: a model NAMED for
// images, music, canvas, video or research is not what a general-purpose
// provider should offer.
//
// It only applies to models whose capability map does NOT claim an image route:
// a model the backend explicitly marks as an image model is judged by that flag
// (see AllowImageModels), not by its name.
var StudioExclude = regexp.MustCompile(`(?:image|nano-banana|tts|audio|live|transcribe|robotics|research|video|music|canvas)`)

// MediaExclude is the gemini-web media name filter. It mirrors magpie's own
// MEDIA_SPECIAL rule for that backend, which drops ids naming a non-text
// modality — the backend serves those over a different endpoint.
var MediaExclude = regexp.MustCompile(`(?:^|[-_])(image|music|canvas|video|research)(?:[-_]|$)`)

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

// gemini-web thinking suffixes: that backend has no effort parameter, so a level
// is a separate model id.
const (
	geminiWebThinkingSuffix       = "-thinking"
	geminiWebMediumThinkingSuffix = "-thinking-medium"
)

// BuildOptions controls how raw models are turned into published records.
type BuildOptions struct {
	// Namespace is the prefix (and backend) to apply.
	Namespace Namespace
	// Filter is the operator's allow/deny list.
	Filter config.ModelFilter
	// AllowImageModels exposes image-generation models. Only the Studio
	// ruleset consults it: gemini-web2api-go publishes no image models.
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
	// UpstreamID is what the backend expects. It may differ from the bare id
	// in the /v1/models entry: a gemini-web "high" thinking record runs on its
	// "-thinking" twin (see UpstreamModelForLevel).
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
	// for the "why was this filtered" explanation. It is nil for gemini-web,
	// which publishes none.
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
	// Namespace is the namespace this result belongs to. It is recorded here
	// so a caller can prove a result was not published under the wrong
	// prefix — the defect this split exists to remove.
	Namespace string
	Records   []Record
	Skipped   []Skipped
	// Notes are non-fatal remarks about the build (for example "a thinking
	// twin was folded into its base model").
	Notes []string
}

// Empty reports whether this build produced no records.
func (r BuildResult) Empty() bool { return len(r.Records) == 0 }

// EmptyError explains why a namespace published nothing, naming the likely cause
// so the operator has something to act on.
//
// An empty catalog is not a normal state: it means every id the backend
// reported was judged unusable, so every client request fails with "not in the
// published catalog" and no clue. Returning a readable error (which the refresh
// path surfaces on the page) is what makes that operable.
func (r BuildResult) EmptyError() error {
	ns := r.Namespace
	if ns == "" {
		ns = "this namespace"
	}
	reason := "the backend listed no models at all"
	if len(r.Skipped) > 0 {
		seen := make([]string, 0, 3)
		for _, skip := range r.Skipped {
			if len(seen) >= 3 {
				break
			}
			seen = append(seen, skip.ID+" ("+skip.Reason+")")
		}
		reason = fmt.Sprintf("the backend listed %d model(s) and every one was filtered out: %s",
			len(r.Skipped), strings.Join(seen, "; "))
	}
	return fmt.Errorf("namespace %s published 0 models, so nothing is routable: %s", ns, reason)
}

// Build dispatches to the namespace's own ruleset and refuses a silent empty
// result.
//
// The empty result is returned WITH its error, so a caller can keep a previous
// snapshot and still display the skip reasons that explain why the new one is
// unusable.
func Build(models []backend.Model, opts BuildOptions) (BuildResult, error) {
	var res BuildResult
	switch opts.Namespace.Name {
	case config.NamespaceGeminiWeb:
		res = BuildGeminiWeb(models, opts)
	case config.NamespaceAIStudio:
		res = BuildStudio(models, opts)
	default:
		return BuildResult{}, fmt.Errorf("namespace %q has no catalog rules; this build serves %s and %s",
			opts.Namespace.Name, config.NamespaceGeminiWeb, config.NamespaceAIStudio)
	}
	if res.Empty() {
		return res, res.EmptyError()
	}
	return res, nil
}

// NamespaceFor returns the namespace descriptor with this name.
func NamespaceFor(name string) (Namespace, error) {
	switch strings.TrimSpace(name) {
	case config.NamespaceAIStudio:
		return AIStudio, nil
	case config.NamespaceGeminiWeb:
		return GeminiWeb, nil
	}
	return Namespace{}, fmt.Errorf("unknown model namespace %q; this build serves %s and %s", name, config.NamespaceGeminiWeb, config.NamespaceAIStudio)
}

// BuildStudio applies AIStudio2API's capability rules.
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
func BuildStudio(models []backend.Model, opts BuildOptions) BuildResult {
	ns := opts.Namespace
	if ns.Name == "" {
		ns = AIStudio
	}
	res := BuildResult{Namespace: ns.Name}
	for _, m := range models {
		id := strings.TrimSpace(m.ID)
		if id == "" || hasRecord(res.Records, id) {
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
			PublicID:     ns.Prefix + id,
			UpstreamID:   id,
			Namespace:    ns.Name,
			DisplayName:  id,
			Capabilities: caps,
			Channels:     m.Channels,
		}
		applyLimits(&rec, m, opts)
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
				rec.Thinking.Levels = supportedLevels(m.ThinkingLevels, thinkLevels)
			}
		}

		// Operator filter runs last: an explicit deny always wins over every
		// capability judgment above.
		if opts.Filter.Excludes(id, rec.PublicID) {
			res.skip(id, "excluded by models.allow/deny")
			continue
		}
		res.Records = append(res.Records, rec)
	}
	return res
}

// BuildGeminiWeb publishes gemini-web2api-go's models.
//
// The backend publishes only an OpenAI-shaped id, so the rules here are
// correspondingly minimal:
//
//  1. an id is published if it is a plausibly-usable model name: non-empty, no
//     control characters, and not named for a non-text modality;
//  2. capabilities are NEVER consulted — the backend reports none, and
//     requiring one would publish nothing;
//  3. generateContent is NEVER required, for the same reason;
//  4. a thinking twin is folded into its base model: the base model gains the
//     levels the backend can serve by switching ids, and the twin is not
//     published as its own entry (a client naming the twin directly still
//     routes, because Resolve matches upstream ids too);
//  5. the operator filter runs last.
func BuildGeminiWeb(models []backend.Model, opts BuildOptions) BuildResult {
	ns := opts.Namespace
	if ns.Name == "" {
		ns = GeminiWeb
	}
	res := BuildResult{Namespace: ns.Name}

	// Pass one: which ids does this backend actually list? That is what decides
	// whether a thinking twin has a base to fold into.
	listed := make(map[string]bool, len(models))
	ids := make([]string, 0, len(models))
	for _, m := range models {
		id := strings.TrimSpace(m.ID)
		if id == "" || listed[id] {
			continue
		}
		if containsControlChars(id) {
			res.skip(id, "model id contains control characters")
			continue
		}
		listed[id] = true
		ids = append(ids, id)
	}

	for _, id := range ids {
		base, isTwin := geminiWebBaseID(id)
		if isTwin && listed[base] {
			// A twin of a listed model is folded into it; publishing it
			// separately would offer the same model twice under a suffix most
			// clients never type.
			res.Notes = append(res.Notes, "thinking twin "+id+" folded into "+base)
			continue
		}
		if MediaExclude.MatchString(id) {
			res.skip(id, "named for a non-text modality (image/music/canvas/video/research)")
			continue
		}
		rec := Record{
			PublicID:    ns.Prefix + id,
			UpstreamID:  id,
			Namespace:   ns.Name,
			DisplayName: id,
		}
		// The backend reports no limits, so the defaults stand unless the
		// operator caps them.
		applyLimits(&rec, backend.Model{ID: id}, opts)
		rec.InputModes = []string{"text", "image"}
		rec.OutputModes = []string{"text"}

		// Reasoning: only a model with a real twin gets levels, because a
		// single level is not reasoning control worth offering.
		if levels, twin := geminiWebThinking(base, isTwin, listed); len(levels) > 0 {
			rec.Thinking = &Thinking{Levels: levels}
			res.Notes = append(res.Notes, fmt.Sprintf("%s exposes %s; high runs on %s",
				id, strings.Join(levels, "/"), twin))
		}

		if opts.Filter.Excludes(id, rec.PublicID) {
			res.skip(id, "excluded by models.allow/deny")
			continue
		}
		res.Records = append(res.Records, rec)
	}
	return res
}

// geminiWebBaseID strips a thinking suffix, reporting whether it did.
func geminiWebBaseID(id string) (base string, isTwin bool) {
	if strings.HasSuffix(id, geminiWebMediumThinkingSuffix) {
		return strings.TrimSuffix(id, geminiWebMediumThinkingSuffix), true
	}
	if strings.HasSuffix(id, geminiWebThinkingSuffix) {
		return strings.TrimSuffix(id, geminiWebThinkingSuffix), true
	}
	return id, false
}

// geminiWebThinking reports the levels a gemini-web model can serve and the id
// its "high" level runs on.
//
// The backend has no effort parameter: "<id>-thinking-medium" and "<id>-thinking"
// exist only when the backend lists them, and the plain id is the low level. A
// model with no twin gets no Thinking block at all — offering only "low" would be
// a level list that cannot change anything.
func geminiWebThinking(base string, isTwin bool, listed map[string]bool) ([]string, string) {
	if isTwin {
		return nil, ""
	}
	levels := []string{}
	if listed[base] {
		levels = append(levels, "low")
	}
	if listed[base+geminiWebMediumThinkingSuffix] {
		levels = append(levels, "medium")
	}
	if listed[base+geminiWebThinkingSuffix] {
		levels = append(levels, "high")
	}
	if len(levels) < 2 {
		return nil, ""
	}
	return levels, base + geminiWebThinkingSuffix
}

// containsControlChars reports whether an id carries a C0 control character,
// which no model name legitimately has.
func containsControlChars(id string) bool {
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// applyLimits fills a record's published token limits.
func applyLimits(rec *Record, m backend.Model, opts BuildOptions) {
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
}

// hasRecord reports whether an upstream id is already published, which keeps a
// duplicated list entry from becoming a duplicated model.
func hasRecord(records []Record, upstreamID string) bool {
	for _, rec := range records {
		if rec.UpstreamID == upstreamID || rec.PublicID == upstreamID {
			return true
		}
	}
	return false
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
func supportedLevels(raw, allowed []string) []string {
	allow := make(map[string]bool, len(allowed))
	for _, level := range allowed {
		allow[level] = true
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, level := range raw {
		level = strings.ToLower(strings.TrimSpace(level))
		if level == "" || !allow[level] || seen[level] {
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

// UpstreamModelForLevel reports the id a level-specific request must be sent on.
//
// Only gemini-web encodes a level as a separate model: a "high" request goes to
// its "-thinking" twin and a "medium" one to "-thinking-medium". The Studio
// backend takes reasoning_effort on the same id, so it is left untouched.
func (r Record) UpstreamModelForLevel(level string) string {
	level = strings.ToLower(strings.TrimSpace(level))
	if r.Thinking == nil || level == "" || !containsFold(r.Thinking.Levels, level) {
		return r.UpstreamID
	}
	if r.Namespace != config.NamespaceGeminiWeb {
		return r.UpstreamID
	}
	base, _ := geminiWebBaseID(r.UpstreamID)
	switch level {
	case "medium":
		return base + geminiWebMediumThinkingSuffix
	case "high":
		return base + geminiWebThinkingSuffix
	default:
		// The plain id IS the low level.
		return base
	}
}

// ResolveNamespace splits a public model id into its namespace and bare model
// name. ok is false when the id carries no known namespace prefix.
func ResolveNamespace(model string) (namespace string, bare string, ok bool) {
	model = strings.TrimSpace(model)
	for _, ns := range []string{config.NamespaceGeminiWeb, config.NamespaceAIStudio} {
		if strings.HasPrefix(model, ns+"/") {
			offered := strings.TrimPrefix(model, ns+"/")
			if strings.TrimSpace(offered) == "" {
				return "", "", false
			}
			return ns, offered, true
		}
	}
	return "", "", false
}

// ModelIDError explains an unusable model id.
func ModelIDError(model string) error {
	return fmt.Errorf("model %q names no %s namespace; expected %s<model> or %s<model>, for example %sgemini-2.5-pro",
		model, "geminiapi", config.NamespaceAIStudio+"/", config.NamespaceGeminiWeb+"/", config.NamespaceAIStudio+"/")
}
