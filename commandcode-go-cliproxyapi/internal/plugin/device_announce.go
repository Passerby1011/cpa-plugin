package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/adapter/gocli"
	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/config"
)

// The two account-surface endpoints the vendor CLI announces itself on before
// it starts generating. Neither carries a payload the answer depends on: they
// exist so the upstream sees a client that registered its device and its
// session, which is what a real CLI does.
const (
	accountFingerprintPath = "/alpha/fingerprint/record"
	accountLifecyclePath   = "/alpha/lifecycle-events"

	// deviceAnnounceTimeout bounds one announcement. These calls are advisory
	// bookkeeping: a slow upstream must never delay or fail a completion.
	deviceAnnounceTimeout = 15 * time.Second
	// deviceAnnounceMinInterval throttles re-announcement per credential. The
	// reference refreshes every 8h + jitter; the throttle here is the floor
	// that keeps a burst of concurrent requests from each firing a pair of
	// calls at once.
	deviceAnnounceMinInterval = 8 * time.Hour
)

// deviceAnnouncer remembers, per credential, when its device was last
// announced. Deliberately in-memory: a restart re-announces once, which is the
// safe direction (the upstream would otherwise never see this process's
// device) and keeps the plugin free of persistence the host would own.
type deviceAnnouncer struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newDeviceAnnouncer() *deviceAnnouncer {
	return &deviceAnnouncer{last: make(map[string]time.Time)}
}

// shouldAnnounce reports whether this credential is due, and records the
// attempt. It records BEFORE the call completes so a failed announcement is
// not retried in a tight loop by the next request in the same burst; the next
// window picks it up.
func (d *deviceAnnouncer) shouldAnnounce(credential string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if previous, ok := d.last[credential]; ok && now.Sub(previous) < deviceAnnounceMinInterval {
		return false
	}
	d.last[credential] = now
	return true
}

// announceDeviceIfDue sends the fingerprint and lifecycle pair for one
// credential, at most once per deviceAnnounceMinInterval.
//
// The reference calls this from the request path, awaited before the first
// generate and throttled thereafter (8h + jitter), so the upstream sees the CLI
// register its device before it produces anything. This mirrors that: the
// caller passes its own config snapshot, and the call is bounded and
// best-effort, so a slow or unavailable account endpoint can never fail a
// completion.
//
// It is called ONLY for go-cli accounts. A provider-mode credential talks to
// the documented Provider API, which has no device concept, so announcing a
// fabricated CLI device for it would invent a signal the caller's own
// transport contradicts.
//
// The credential is never placed in a log field.
func (m *Manager) announceDeviceIfDue(ctx context.Context, deviceCfg config.Device, baseURL, credential string) {
	if m.bridge == nil || !deviceCfg.Enabled {
		return
	}
	if !m.device.shouldAnnounce(credential, time.Now()) {
		return
	}
	base, err := AccountAPIBase(baseURL)
	if err != nil {
		debugTrace("device announce skipped: %v", err)
		return
	}

	announceCtx, cancel := context.WithTimeout(ctx, deviceAnnounceTimeout)
	defer cancel()
	headers := deviceAnnounceHeaders(credential)

	if body, errMarshal := json.Marshal(gocli.GenerateFingerprintWithSalt(deviceCfg.IdentitySalt, credential)); errMarshal == nil {
		if _, errDo := m.bridge.Do(announceCtx, pluginapi.HTTPRequest{
			Method:  http.MethodPost,
			URL:     base + accountFingerprintPath,
			Headers: headers,
			Body:    body,
		}); errDo != nil {
			debugTrace("device fingerprint announce failed: %v", errDo)
		}
	}

	profile := gocli.DefaultDeviceProfile()
	lifecycle, errMarshal := json.Marshal(map[string]any{
		"eventType": "cli_session_exists",
		"metadata": map[string]any{
			"sessionId":  "sess_" + sessionToken(credential),
			"cliVersion": gocli.DefaultVersion,
			"mode":       "interactive",
			"os":         profile.Platform + "-" + profile.Arch,
		},
	})
	if errMarshal != nil {
		return
	}
	if _, errDo := m.bridge.Do(announceCtx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     base + accountLifecyclePath,
		Headers: headers,
		Body:    lifecycle,
	}); errDo != nil {
		debugTrace("device lifecycle announce failed: %v", errDo)
	}
}

// deviceAnnounceHeaders is the header set an announcement carries: the CLI
// transport's stable headers only. The per-turn headers (session id,
// traceparent, project slug, taste-learning) belong to a generation request and
// are deliberately absent here.
func deviceAnnounceHeaders(credential string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "cli")
	h.Set("x-command-code-version", gocli.DefaultVersion)
	h.Set("x-cli-environment", "production")
	h.Set("Authorization", "Bearer "+credential)
	return h
}

// isDeviceAnnounceURL reports whether an upstream URL is one of the device
// announcement endpoints. Callers use it to keep announcements from being
// mistaken for generation requests: they share the HTTP transport but nothing
// else.
func isDeviceAnnounceURL(rawURL string) bool {
	return strings.HasSuffix(rawURL, accountFingerprintPath) ||
		strings.HasSuffix(rawURL, accountLifecyclePath)
}

// sessionToken derives a stable per-credential session id fragment. The
// reference uses random bytes per process; a derived value keeps the id stable
// across restarts for the same credential, which is the same stability
// property the device fingerprint has. It is a label, not a secret, and it is
// derived from the credential alone so two credentials never share one.
func sessionToken(credential string) string {
	sum := sha256.Sum256([]byte("command-code:session:" + credential))
	return hex.EncodeToString(sum[:8])
}
