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

// pluginStatus is the whole picture the panel renders. It is assembled here so
// the HTTP route and the tests agree on one shape.
type pluginStatus struct {
	// Provider is the single provider id this plugin serves.
	Provider string `json:"provider"`
	// Version / PinnedVersion are this plugin's and the backend's versions.
	Version       string `json:"version"`
	PinnedVersion string `json:"pinned_backend_version"`
	// Namespaces lists the model-id prefixes served (ai-studio now; gemini-web
	// arrives with its own card).
	Namespaces []string `json:"namespaces"`
	// External reports that ai-studio.base-url points at a backend this plugin
	// does not manage.
	External bool `json:"external"`
	// Backend is the supervisor's snapshot (absent for an external backend).
	Backend *backend.Status `json:"backend,omitempty"`
	// BaseURL is the OpenAI-compatible root in use.
	BaseURL string `json:"base_url,omitempty"`
	// Settings echoes the effective knobs so the page can show them without a
	// second call.
	Settings pluginSettings `json:"settings"`
	// Models is the published catalog summary.
	Models catalogSummary `json:"models"`
	// Accounts explains the pool rules in the response itself, so a script
	// consumer cannot miss the caveat the page shows.
	Accounts accountNote `json:"accounts"`
	// Notice carries an unavoidable behavioural caveat (see accountNote).
	Notice string `json:"notice"`
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

// catalogSummary is the published model tally plus skip reasons.
type catalogSummary struct {
	Count     int               `json:"count"`
	FetchedAt string            `json:"fetched_at,omitempty"`
	Skipped   []catalog.Skipped `json:"skipped,omitempty"`
}

// accountNote states the multi-account caveat in the payload itself.
type accountNote struct {
	Managed         bool                  `json:"managed"`
	Counts          backend.AccountCounts `json:"counts"`
	Caveat          string                `json:"caveat"`
	ImportSupported bool                  `json:"import_supported"`
}

// accountCaveat is the sentence the panel and the README must both carry.
const accountCaveat = "多个 auth 记录只是 CPA 的调度槽位，实际用哪个 Google 账号由本地后端的账号池决定；" +
	"插件无法按 auth 记录指定某个账号，也无法读取账号邮箱或 Cookie。"

// snapshotStatus builds the full status document.
func (m *Manager) snapshotStatus(req pluginapi.ManagementRequest) pluginStatus {
	m.mu.RLock()
	cfg := m.cfg
	mgr := m.backendMgr
	external := m.skipSupervision
	result := m.catalog
	at := m.modelsAt
	m.mu.RUnlock()

	st := pluginStatus{
		Provider:      ProviderID,
		Version:       pluginVersion,
		PinnedVersion: backend.ReleaseVersion,
		Namespaces:    cfg.Namespaces,
		External:      external,
		Settings: pluginSettings{
			DataDir:                cfg.DataDir,
			RequestTimeout:         durationText(cfg.RequestTimeout, config.DefaultRequestTimeout),
			StreamIdleTimeout:      durationText(cfg.StreamIdleTimeout, config.DefaultStreamIdleTimeout),
			ModelTimeout:           durationText(cfg.ModelTimeout, config.DefaultModelTimeout),
			AllowImageModels:       cfg.AIStudio.AllowImageModels,
			ProxyConfigured:        cfg.SyncProxy != "",
			BackendProxyConfigured: cfg.AIStudio.Proxy != "",
		},
		Models: catalogSummary{Count: len(result.Records), Skipped: result.Skipped},
		Notice: accountCaveat,
	}
	if len(st.Namespaces) == 0 {
		st.Namespaces = []string{config.NamespaceAIStudio}
	}
	if !at.IsZero() {
		st.Models.FetchedAt = at.Format(timeLayout)
	}

	if external {
		st.BaseURL = cfg.AIStudio.BaseURL
		st.Accounts = accountNote{
			Managed:         false,
			ImportSupported: false,
			Counts:          backend.AccountCounts{Error: "外部后端的账号池不由本插件管理"},
			Caveat:          accountCaveat,
		}
		return st
	}
	if mgr == nil {
		st.Accounts = accountNote{Managed: true, ImportSupported: true, Caveat: accountCaveat,
			Counts: backend.AccountCounts{Error: "插件尚未完成注册"}}
		return st
	}

	ctx, cancel := contextWithTimeout(statusTimeout)
	defer cancel()
	snapshot := mgr.StatusSnapshot(ctx)
	st.Backend = &snapshot
	st.BaseURL = snapshot.Origin
	st.Accounts = accountNote{
		Managed:         true,
		ImportSupported: true,
		Counts:          snapshot.Accounts,
		Caveat:          accountCaveat,
	}
	return st
}

func durationText(value, fallback time.Duration) string {
	if value <= 0 {
		value = fallback
	}
	return value.String()
}
