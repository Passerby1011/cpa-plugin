// Package plugin — the device sign-in RPCs.
//
// auth.login.start opens a device authorization and hands the operator a page to
// approve it on; auth.login.poll answers whether it has been approved yet. The
// two keys the approval returns are what makes an account different from a
// pasted API key: the control key is the only way to read the plan allowance,
// and the control plane does not accept an API key.
package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/opendesign/internal/auth"
	"github.com/hex-ci/cpa-plugin/opendesign/internal/config"
)

// loginTTL bounds one started sign-in. The control plane issues its own expiry,
// which the device carries; this is the outer bound for the state we keep.
const loginTTL = 15 * time.Minute

// loginContext is one in-flight sign-in.
type loginContext struct {
	device    *auth.Device
	startedAt time.Time
	expires   time.Time
	// controlURL and transport are captured at start so a reconfigure mid-flow
	// cannot send the poll to a different plane than the code was issued by.
	controlURL string
	transport  auth.Transport
	// pastedKey is set when the operator pasted a key into the modal instead
	// of approving on the page. It is a runtime key only: no control key comes
	// with it, so its allowance stays unreadable.
	pastedKey string
}

// handleLoginStart implements auth.login.start.
func (m *Manager) handleLoginStart(request []byte) ([]byte, error) {
	var req pluginapi.AuthLoginStartRequest
	if len(request) > 0 {
		_ = json.Unmarshal(request, &req)
	}
	m.mu.RLock()
	controlURL := m.cfg.ControlURL
	m.mu.RUnlock()
	if strings.TrimSpace(controlURL) == "" {
		controlURL = config.DefaultControlURL
	}
	if m.bridge == nil {
		return ErrEnvelope("not_ready", "the plugin has no host transport; registration has not run"), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), registerRefreshTimeout)
	defer cancel()
	device, err := auth.Start(ctx, m.bridge.Transport(), controlURL, auth.Hostname)
	if err != nil {
		return errorEnvelopeWithStatus("login_start_failed", err.Error(), http.StatusBadGateway, false), nil
	}
	state, errState := newLoginState()
	if errState != nil {
		return errorEnvelopeWithStatus("login_start_failed", "could not allocate a login state", http.StatusInternalServerError, false), nil
	}
	now := time.Now()
	expires := device.ExpiresAt
	if expires.IsZero() || expires.After(now.Add(loginTTL)) {
		expires = now.Add(loginTTL)
	}
	m.logins.Store(state, &loginContext{
		device:     device,
		startedAt:  now,
		expires:    expires,
		controlURL: controlURL,
		transport:  m.bridge.Transport(),
	})
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  ProviderID,
		URL:       device.ActivationURL,
		State:     state,
		ExpiresAt: expires.UTC(),
		Metadata: map[string]any{
			"logo":   pluginLogoURL,
			"code":   device.UserCode,
			"prompt": startPrompt(device),
		},
	}), nil
}

// startPrompt is what the sign-in modal tells the operator, quoting the code the
// page asks for.
func startPrompt(device *auth.Device) string {
	if device == nil {
		return "Approve the CLI sign-in on the page that opens. It finishes here by itself."
	}
	if code := strings.TrimSpace(device.UserCode); code != "" {
		return "Approve the CLI sign-in on the page that opens (code " + code + "). It finishes here by itself."
	}
	return "Approve the CLI sign-in on the page that opens. It finishes here by itself."
}

// newLoginState mints an unguessable state token.
func newLoginState() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "od-" + hex.EncodeToString(buf), nil
}

// loginState reads one in-flight sign-in.
func (m *Manager) loginState(state string) (*loginContext, bool) {
	v, ok := m.logins.Load(strings.TrimSpace(state))
	if !ok {
		return nil, false
	}
	lc, ok := v.(*loginContext)
	return lc, ok
}

// dropLogin removes a finished sign-in.
func (m *Manager) dropLogin(state string) { m.logins.Delete(strings.TrimSpace(state)) }

// handleLoginPoll implements auth.login.poll.
func (m *Manager) handleLoginPoll(request []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed login poll body"), nil
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return ErrEnvelope("invalid_request", "login poll: state is required"), nil
	}
	lc, ok := m.loginState(state)
	if !ok {
		return ErrEnvelope("login_state_unknown", "login poll: unknown state (start the sign-in again)"), nil
	}
	if time.Now().After(lc.expires) {
		m.dropLogin(state)
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "the sign-in code ran out before it was approved — start again",
		}), nil
	}

	// A key pasted into the modal takes the fast path: it is a runtime key,
	// there is nothing to poll, and it is usable immediately.
	if key := pastedKeyFrom(req.Metadata); key != "" {
		return m.completePastedKey(state, key)
	}

	result, err := auth.Poll(context.Background(), lc.transport, lc.controlURL, lc.device)
	if err != nil {
		if auth.IsRetryable(err) {
			// A transport hiccup is not the sign-in failing: keep waiting.
			return pendingPoll("waiting for the page to approve the sign-in"), nil
		}
		m.dropLogin(state)
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: err.Error(),
		}), nil
	}
	if result.Authorization == nil {
		return pendingPoll(pendingMessage(lc.device)), nil
	}
	return m.completeSignIn(state, result.Authorization)
}

func pendingPoll(message string) []byte {
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusPending,
		Message: message,
	})
}

// pendingMessage names the code while it is still live, so the operator can
// find it again if the page was closed.
func pendingMessage(device *auth.Device) string {
	if device != nil {
		if code := strings.TrimSpace(device.UserCode); code != "" {
			return "waiting for the page to approve the sign-in (code " + code + ")"
		}
	}
	return "waiting for the page to approve the sign-in"
}

// pastedKeyFrom reads a pasted key out of a poll's metadata. Nothing is
// accepted silently: a value that is not a runtime key is ignored rather than
// turned into an account that would fail on every request.
func pastedKeyFrom(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	for _, name := range []string{"api_key", "api-key", "key", "runtime_key", "runtime-key"} {
		raw, ok := metadata[name]
		if !ok {
			continue
		}
		s, _ := raw.(string)
		if key := normalizePastedKey(s); key != "" {
			return key
		}
	}
	return ""
}

// normalizePastedKey trims a pasted key and refuses anything that is not a
// runtime key.
func normalizePastedKey(raw string) string {
	key := strings.TrimSpace(raw)
	key = strings.TrimPrefix(key, "Bearer ")
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if strings.HasPrefix(key, "vela_ctrl_") {
		// A control key cannot serve a model request; refusing it here is what
		// keeps the failure at the sign-in step instead of at every call.
		return ""
	}
	if strings.ContainsAny(key, " \t\r\n") {
		return ""
	}
	return key
}
