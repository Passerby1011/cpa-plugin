// Package catalog builds the model snapshot this plugin serves: the live
// catalog an account's plan entitles it to, with the seed filling in whatever
// the live entry leaves out.
//
// The rules below are magpie's, transcribed because each one was learned from a
// measured failure rather than invented:
//
//   - A live entry's limits/words win where it gives them; the seed fills the
//     rest. Falling back to a generic 200K/32K would silently halve a model
//     whose live entry omitted its limits.
//   - A kind the seed declares STAYS when the live catalog leaves it out. The
//     two mistakes do not cost the same: a kind listed that the model refuses
//     is the vendor's visible error, while a kind dropped is silent — the
//     caller's image is sent as text and the model answers about nothing
//     (magpie #829).
//   - A live catalog's output limit is what it says, capped where amr-link
//     takes less than it publishes (modelcaps.OutputCap, magpie #827).
//   - 401/403 is not "no catalog": it is the account's key being refused, and
//     the caller marks the sign-in as expired instead of serving a stale list.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hex-ci/cpa-plugin/opendesign/internal/config"
	"github.com/hex-ci/cpa-plugin/opendesign/internal/modelcaps"
	"github.com/hex-ci/cpa-plugin/opendesign/internal/protocol"
)

// Entry is one model as this plugin serves it.
type Entry struct {
	// UpstreamID is the id amr-link knows the model by.
	UpstreamID string
	// PublicID is the client-facing id, "opendesign/<upstream id>" by default.
	PublicID string
	Name     string
	Context  int64
	Output   int64
	// InputKinds are the non-text modalities the model accepts; "text" is
	// always accepted and is added by consumers that need a full list.
	InputKinds []string
	// Efforts are the reasoning levels the model was measured to think at.
	// Empty means "takes a level and does not think" (GPT-6 Luna).
	Efforts []string
	// API is the wire the model is declared on.
	API modelcaps.API
	// Rate is the plan's deduction multiplier, 0 when no rate is known.
	Rate float64
	// Thinking reports whether the model reasons at all.
	Thinking bool
}

// InputModalities returns the full input list, "text" first.
func (e Entry) InputModalities() []string {
	out := []string{"text"}
	for _, k := range e.InputKinds {
		if k == "text" {
			continue
		}
		out = append(out, k)
	}
	return out
}

// OutputModalities is always text: no Open Design model returns anything else.
func (e Entry) OutputModalities() []string { return []string{"text"} }

// LiveErrorKind classifies a failed live fetch.
type LiveErrorKind int

const (
	// LiveOK means the fetch produced a usable list.
	LiveOK LiveErrorKind = iota
	// LiveRefused means the account's key was refused (401/403) — the sign-in
	// is no longer valid, which is different from a vendor hiccup.
	LiveRefused
	// LiveUnavailable means the vendor could not answer (network, 5xx, a
	// malformed body). The seed is served and the account is NOT marked.
	LiveUnavailable
)

// LiveResult is one live fetch's outcome.
type LiveResult struct {
	Kind  LiveErrorKind
	Err   error
	Model []Entry
	// Message is the vendor's own words on a refusal, already truncated and
	// free of key material.
	Message string
}

// ParseLive decodes an amr-link /v1/models body into entries, before the seed
// merge. Entries with no usable id are dropped.
func ParseLive(body []byte) ([]Entry, error) {
	var doc struct {
		Data []struct {
			ID           string `json:"id"`
			Architecture *struct {
				InputModalities []string `json:"input_modalities"`
			} `json:"architecture"`
			Metadata *struct {
				Cost          string          `json:"cost"`
				Capability    string          `json:"capability"`
				ContextLimit  json.Number     `json:"context_limit"`
				ContextBudget json.Number     `json:"context_budget"`
				RawSkip       json.RawMessage `json:"-"`
			} `json:"metadata"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("the model catalog body was not JSON of the expected shape")
	}
	out := make([]Entry, 0, len(doc.Data))
	for _, m := range doc.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		var inputKinds []string
		if m.Architecture != nil {
			inputKinds = nonTextKinds(m.Architecture.InputModalities)
		}
		var contextLimit, contextBudget int64
		cost, capability := "", ""
		if m.Metadata != nil {
			contextLimit = numberToInt(m.Metadata.ContextLimit)
			contextBudget = numberToInt(m.Metadata.ContextBudget)
			cost = strings.TrimSpace(m.Metadata.Cost)
			capability = strings.TrimSpace(m.Metadata.Capability)
		}
		out = append(out, mergeWithSeed(id, contextLimit, contextBudget, inputKinds, cost, capability))
	}
	return out, nil
}

func numberToInt(n json.Number) int64 {
	if n == "" {
		return 0
	}
	if v, err := n.Int64(); err == nil {
		return v
	}
	if f, err := n.Float64(); err == nil {
		return int64(f)
	}
	return 0
}

func nonTextKinds(list []string) []string {
	out := make([]string, 0, len(list))
	for _, k := range list {
		k = strings.TrimSpace(strings.ToLower(k))
		if k == "" || k == "text" {
			continue
		}
		out = append(out, k)
	}
	return out
}

// mergeWithSeed applies the seed to one live entry: the live values win where
// given, the seed fills the rest, and the seed's kinds are UNIONED with the
// live ones rather than replaced.
func mergeWithSeed(id string, contextLimit, contextBudget int64, inputKinds []string, cost, capability string) Entry {
	seed, known := modelcaps.SeedByID[id]
	entry := Entry{UpstreamID: id, Name: id, API: modelcaps.APIFor(id)}

	if known {
		entry.Name = seed.Name
		entry.Efforts = appendsCopy(seed.Efforts)
	}
	if contextLimit > 0 {
		entry.Context = contextLimit
	} else if known {
		entry.Context = seed.Context
	} else {
		entry.Context = 200000
	}
	if contextBudget > 0 {
		entry.Output = modelcaps.CapOutput(id, contextBudget)
	} else if known {
		entry.Output = modelcaps.CapOutput(id, seed.Output)
	} else {
		entry.Output = 32000
	}
	// The seed's kinds survive a catalog that omits them (#829); the catalog's
	// kinds add to the seed's.
	entry.InputKinds = unionKinds(seedKinds(known, seed), inputKinds)

	if !known {
		// A model the seed does not know: the vendor's words are all there is.
		entry.Rate = modelcaps.RateOf(id, cost)
		entry.Thinking = modelcaps.ThinksFromCapability[strings.ToLower(capability)]
		return entry
	}
	entry.Rate = modelcaps.RateOf(id, firstNonEmpty(cost, seed.Cost))
	entry.Thinking = len(entry.Efforts) > 0 || modelcaps.ThinksFromCapability[strings.ToLower(firstNonEmpty(capability, seed.Capability))]
	return entry
}

func seedKinds(known bool, seed modelcaps.SeedModel) []string {
	if !known {
		return nil
	}
	return seed.Input
}

func unionKinds(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, k := range list {
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, k)
		}
	}
	return out
}

func appendsCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// SeedEntries renders the seed catalog as entries, with each public id applied.
// This is what is served before anyone signs in and after a fetch the vendor
// could not answer.
func SeedEntries(cfg config.Config) []Entry {
	out := make([]Entry, 0, len(modelcaps.Seed))
	for _, m := range modelcaps.Seed {
		entry := Entry{
			UpstreamID: m.ID,
			Name:       m.Name,
			Context:    m.Context,
			Output:     modelcaps.CapOutput(m.ID, m.Output),
			InputKinds: appendsCopy(m.Input),
			Efforts:    appendsCopy(m.Efforts),
			API:        modelcaps.APIFor(m.ID),
			Rate:       modelcaps.RateOf(m.ID, m.Cost),
			Thinking:   len(m.Efforts) > 0 || modelcaps.ThinksFromCapability[strings.ToLower(m.Capability)],
		}
		entry.PublicID = cfg.PublicID(entry.UpstreamID)
		out = append(out, entry)
	}
	return out
}

// Finalize sorts entries, applies the model filter and the public id, and drops
// duplicates by upstream id. Sorting is by upstream id so /v1/models is stable
// across refreshes — a client that caches a list should not see it reorder
// because the vendor's response did.
func Finalize(entries []Entry, cfg config.Config) []Entry {
	seen := make(map[string]struct{}, len(entries))
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.UpstreamID == "" {
			continue
		}
		if _, dup := seen[e.UpstreamID]; dup {
			continue
		}
		e.PublicID = cfg.PublicID(e.UpstreamID)
		if cfg.Models.Excludes(e.UpstreamID, e.PublicID) {
			continue
		}
		if e.Name == "" {
			e.Name = e.UpstreamID
		}
		if e.API == "" {
			e.API = modelcaps.APIFor(e.UpstreamID)
		}
		seen[e.UpstreamID] = struct{}{}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpstreamID < out[j].UpstreamID })
	return out
}

// Snapshot is one account's served model list plus how it was produced.
type Snapshot struct {
	Entries []Entry
	// Live is true when the entries came from a live fetch, false when they are
	// the seed.
	Live bool
	// Refused is true when the vendor refused the account's key; the caller
	// uses it to mark the sign-in as expired.
	Refused bool
	// Err is the fetch error, when one occurred.
	Err        error
	byUpstream map[string]Entry
	byPublic   map[string]Entry
}

// NewSnapshot indexes a finalized entry list for lookup.
func NewSnapshot(entries []Entry) *Snapshot {
	s := &Snapshot{
		Entries:    entries,
		byUpstream: make(map[string]Entry, len(entries)),
		byPublic:   make(map[string]Entry, len(entries)),
	}
	for _, e := range entries {
		s.byUpstream[e.UpstreamID] = e
		s.byPublic[e.PublicID] = e
	}
	return s
}

// Lookup resolves a requested model id — the public id, the bare upstream id, or
// a prefix the client invented — onto an entry.
func (s *Snapshot) Lookup(model string) (Entry, bool) {
	if s == nil {
		return Entry{}, false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return Entry{}, false
	}
	if e, ok := s.byPublic[model]; ok {
		return e, true
	}
	if e, ok := s.byUpstream[model]; ok {
		return e, true
	}
	// A client may reach this executor with the upstream id under a foreign
	// prefix ("openai/glm-5.3-flash"); the part after the last slash is tried
	// as an upstream id, which is the only reading that can be right here.
	if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
		if e, ok := s.byUpstream[model[i+1:]]; ok {
			return e, true
		}
	}
	return Entry{}, false
}

// Transport is the one upstream call this package needs: a non-streaming HTTP
// request through the host's bridge, so the request-log capture and the host's
// proxy rules apply.
type Transport func(ctx context.Context, method, url string, headers map[string]string, body []byte) (status int, respBody []byte, err error)

// Fetch reads an account's live catalog at its runtime key and finalizes it
// against cfg (prefix, allow/deny).
//
// A refusal (401/403) is reported as LiveRefused and never downgraded to
// LiveUnavailable: the two mean different things to the operator, and only the
// first invalidates the sign-in.
func Fetch(ctx context.Context, transport Transport, cfg config.Config, runtimeKey string) LiveResult {
	if transport == nil {
		return LiveResult{Kind: LiveUnavailable, Err: fmt.Errorf("no host transport available")}
	}
	if strings.TrimSpace(runtimeKey) == "" {
		return LiveResult{Kind: LiveUnavailable, Err: fmt.Errorf("no runtime key for this account")}
	}
	url := strings.TrimRight(cfg.BaseURL, "/") + "/models"
	status, respBody, err := transport(ctx, "GET", url, map[string]string{
		"Authorization": "Bearer " + runtimeKey,
		"Accept":        "application/json",
	}, nil)
	if err != nil {
		return LiveResult{Kind: LiveUnavailable, Err: err}
	}
	switch {
	case status == 401 || status == 403:
		return LiveResult{Kind: LiveRefused, Message: protocol.UpstreamMessageText(respBody)}
	case status >= 400:
		return LiveResult{Kind: LiveUnavailable, Err: fmt.Errorf("the model catalog read answered %d", status)}
	}
	entries, errParse := ParseLive(respBody)
	if errParse != nil {
		return LiveResult{Kind: LiveUnavailable, Err: errParse}
	}
	if len(entries) == 0 {
		// An empty list is not a catalog: keep serving what was served.
		return LiveResult{Kind: LiveUnavailable, Err: fmt.Errorf("the model catalog listed no models")}
	}
	return LiveResult{Kind: LiveOK, Model: Finalize(entries, cfg)}
}
