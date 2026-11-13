package plugin

// Status assembly and the small shared helpers the management routes need.

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/geminiapi/internal/backend"
	"github.com/hex-ci/cpa-plugin/geminiapi/internal/catalog"
	"github.com/hex-ci/cpa-plugin/geminiapi/internal/config"
)

// statusTimeout bounds the probe behind a status request.
const statusTimeout = 10 * time.Second

// installTimeout bounds a download + verification.
const installTimeout = 10 * time.Minute

// startTimeoutBudget bounds install + spawn + health.
const startTimeoutBudget = 90 * time.Second

// stopTimeoutBudget bounds a process-group teardown.
const stopTimeoutBudget = 30 * time.Second

// timeLayout is the timestamp shape used in management responses.
const timeLayout = "2006-01-02 15:04:05 MST"

// timeNow is a seam so tests can pin a timestamp.
var timeNow = time.Now

// contextWithTimeout is context.WithTimeout with a nil-safe parent.
func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = 30 * time.Second
	}
	return context.WithTimeout(context.Background(), d)
}

// namespaceStatus is one namespace's section of the status document.
type namespaceStatus struct {
	// Name is the model-id prefix this namespace publishes.
	Name string `json:"name"`
	// Backend names and versions the pinned binary this namespace serves.
	Backend       string `json:"backend"`
	PinnedVersion string `json:"pinned_backend_version"`
	// Managed is false when the namespace points at an operator-owned backend.
	Managed bool `json:"managed"`
	// BaseURL is the OpenAI-compatible root in use.
	BaseURL string `json:"base_url,omitempty"`
	// Supervisor is the backend snapshot (absent for an external backend).
	Supervisor *backend.Status `json:"supervisor,omitempty"`
	// Models is the published catalog summary, including the reason a refresh
	// produced nothing. An empty catalog is never silent: CatalogError says
	// why.
	Models namespaceCatalogSummary `json:"models"`
	// Accounts reports the pool counts this namespace's backend exposes.
	Accounts accountNote `json:"accounts"`
}

// namespaceCatalogSummary is one namespace's catalog tally plus the reason it is
// empty when it is.
type namespaceCatalogSummary struct {
	Count     int               `json:"count"`
	FetchedAt string            `json:"fetched_at,omitempty"`
	Skipped   []catalog.Skipped `json:"skipped,omitempty"`
	Notes     []string          `json:"notes,omitempty"`
	// Error explains why this namespace published nothing (or why the last
	// refresh failed and the previous snapshot is being kept). It is empty on
	// a healthy namespace.
	Error string `json:"error,omitempty"`
}

// pluginStatus is the whole picture the panel renders. It is assembled here so
// the HTTP route and the tests agree on one shape.
type pluginStatus struct {
	// Provider is the single provider id this plugin serves.
	Provider string `json:"provider"`
	// Version is this plugin's own version.
	Version string `json:"version"`
	// Namespaces lists the model-id prefixes served.
	Namespaces []string `json:"namespaces"`
	// Backends is the per-namespace section: one entry per namespace, always,
	// so the page can show a failing backend rather than omitting it.
	Backends []namespaceStatus `json:"backends"`
	// Settings echoes the effective knobs so the page can show them without a
	// second call.
	Settings pluginSettings `json:"settings"`
	// Notice carries the unavoidable account-pool caveat.
	Notice string `json:"notice"`
	// PinnedVersion is kept for the pre-split shape: it is the ai-studio
	// backend's version, which is the namespace an operator usually watches.
	PinnedVersion string `json:"pinned_backend_version"`
}

// pluginSettings echoes the effective configuration (never a credential).
type pluginSettings struct {
	DataDir                string `json:"data_dir"`
	RequestTimeout         string `json:"request_timeout"`
	StreamIdleTimeout      string `json:"stream_idle_timeout"`
	ModelTimeout           string `json:"model_timeout"`
	AllowImageModels       bool   `json:"allow_image_models"`
	ProxyConfigured        bool   `json:"proxy_configured"`
	BackendProxyConfigured bool   `json:"backend_proxy_configured"`
}

// accountNote states the multi-account caveat in the payload itself.
type accountNote struct {
	Managed         bool                  `json:"managed"`
	Counts          backend.AccountCounts `json:"counts"`
	Caveat          string                `json:"caveat"`
	ImportSupported bool                  `json:"import_supported"`
	// ImportKind names HOW an account is added on this backend, because the two
	// differ: gemini-web takes a pasted Cookie, AI Studio drives its own
	// browser sign-in.
	ImportKind string `json:"import_kind,omitempty"`
}

// accountCaveat is the sentence the panel and the README must both carry.
const accountCaveat = "多个 auth 记录只是 CPA 的调度槽位，实际用哪个 Google 账号由本地后端的账号池决定；" +
	"插件无法按 auth 记录指定某个账号，也无法读取账号邮箱或 Cookie。"

// snapshotStatus builds the full status document.
func (m *Manager) snapshotStatus(req pluginapi.ManagementRequest) pluginStatus {
	m.mu.RLock()
	cfg := m.cfg
	backends := m.backends
	externals := m.externals
	catalogs := m.catalogs
	catalogErrs := m.catalogErrs
	modelsAt := m.modelsAt
	m.mu.RUnlock()

	st := pluginStatus{
		Provider:   ProviderID,
		Version:    pluginVersion,
		Namespaces: cfg.Namespaces,
		Settings: pluginSettings{
			DataDir:                cfg.DataDir,
			RequestTimeout:         durationText(cfg.RequestTimeout, config.DefaultRequestTimeout),
			StreamIdleTimeout:      durationText(cfg.StreamIdleTimeout, config.DefaultStreamIdleTimeout),
			ModelTimeout:           durationText(cfg.ModelTimeout, config.DefaultModelTimeout),
			AllowImageModels:       cfg.AIStudio.AllowImageModels,
			ProxyConfigured:        cfg.SyncProxy != "",
			BackendProxyConfigured: cfg.AIStudio.Proxy != "" || cfg.GeminiWeb.Proxy != "",
		},
		Notice: accountCaveat,
	}
	if len(st.Namespaces) == 0 {
		st.Namespaces = []string{config.NamespaceAIStudio, config.NamespaceGeminiWeb}
	}
	if flavor, err := backend.FlavorFor(backend.FlavorAIStudio); err == nil {
		st.PinnedVersion = flavor.ReleaseVersion
	}

	ctx, cancel := contextWithTimeout(statusTimeout)
	defer cancel()

	for _, ns := range st.Namespaces {
		flavorID := backend.FlavorID(ns)
		flavor, flavorErr := backend.FlavorFor(flavorID)
		section := namespaceStatus{
			Name:          ns,
			PinnedVersion: flavor.ReleaseVersion,
			Backend:       flavor.Label(),
			Managed:       !externals[ns],
			Models:        namespaceCatalogSummary{Count: len(catalogs[ns].Records)},
		}
		if flavorErr != nil {
			section.Backend = ns
			section.Models.Error = flavorErr.Error()
		}
		section.Models.Skipped = catalogs[ns].Skipped
		section.Models.Notes = catalogs[ns].Notes
		if errText := catalogErrs[ns]; errText != "" {
			section.Models.Error = errText
		}
		if at := modelsAt[ns]; !at.IsZero() {
			section.Models.FetchedAt = at.Format(timeLayout)
		}
		nsCfg, nsCfgErr := cfg.Namespace(ns)
		if nsCfgErr != nil {
			section.Models.Error = nsCfgErr.Error()
			st.Backends = append(st.Backends, section)
			continue
		}
		if !section.Managed {
			section.BaseURL = nsCfg.BaseURL
			section.Accounts = accountNote{
				Managed:         false,
				ImportSupported: false,
				ImportKind:      importKindFor(flavorID),
				Counts:          backend.AccountCounts{Error: "外部后端的账号池不由本插件管理"},
				Caveat:          accountCaveat,
			}
			st.Backends = append(st.Backends, section)
			continue
		}
		mgr := backends[ns]
		if mgr == nil {
			section.Accounts = accountNote{
				Managed: true, ImportSupported: true, ImportKind: importKindFor(flavorID),
				Counts: backend.AccountCounts{Error: "插件尚未完成注册"}, Caveat: accountCaveat,
			}
			st.Backends = append(st.Backends, section)
			continue
		}
		snapshot := mgr.StatusSnapshot(ctx)
		section.Supervisor = &snapshot
		section.BaseURL = snapshot.Origin
		section.Accounts = accountNote{
			Managed:         true,
			ImportSupported: true,
			ImportKind:      importKindFor(flavorID),
			Counts:          snapshot.Accounts,
			Caveat:          accountCaveat,
		}
		st.Backends = append(st.Backends, section)
	}
	return st
}

// importKindFor says how an account is added on this backend, in one word the
// page can switch on: "cookie" for gemini-web, "browser" for AI Studio.
func importKindFor(id backend.FlavorID) string {
	switch id {
	case backend.FlavorAIStudio:
		return "browser"
	default:
		return "cookie"
	}
}

// namespaceSection returns one namespace's status section, or an error when the
// namespace does not exist.
func (s pluginStatus) namespaceSection(name string) (namespaceStatus, bool) {
	for _, section := range s.Backends {
		if section.Name == name {
			return section, true
		}
	}
	return namespaceStatus{}, false
}

func durationText(value, fallback time.Duration) string {
	if value <= 0 {
		value = fallback
	}
	return value.String()
}
