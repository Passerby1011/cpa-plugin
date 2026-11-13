// Package plugin — turning a finished sign-in into an auth record.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/opendesign/internal/auth"
	"github.com/hex-ci/cpa-plugin/opendesign/internal/catalog"
)

// authPersistTimeout bounds the host.auth.save round-trip a completed sign-in
// performs. The record must land before the poll response returns: the host
// writes the auth file from that response, and anything slower would delay
// persistence past the point the operator sees "done".
const authPersistTimeout = 10 * time.Second

// completeSignIn persists an approved sign-in's two keys and answers the poll.
func (m *Manager) completeSignIn(state string, a *auth.Authorization) ([]byte, error) {
	if a == nil || strings.TrimSpace(a.RuntimeKey) == "" || strings.TrimSpace(a.ControlKey) == "" {
		m.dropLogin(state)
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "the approval did not carry both keys — start again",
		}), nil
	}
	record := &authRecord{
		Type:       ProviderID,
		Provider:   ProviderID,
		RuntimeKey: a.RuntimeKey,
		ControlKey: a.ControlKey,
		Email:      a.Email,
		UID:        a.UID,
		Label:      accountLabel(a.Email, a.UID),
		Note:       "Open Design account",
	}
	m.dropLogin(state)
	return m.persistAndRespond(record)
}

// completePastedKey persists a pasted runtime key.
//
// A pasted key has no control key, so the account cannot read its plan
// allowance. That is a real limitation of this mode, not a failure: its models
// are still discovered and served at the key.
func (m *Manager) completePastedKey(state, key string) ([]byte, error) {
	m.dropLogin(state)
	record := &authRecord{
		Type:       ProviderID,
		Provider:   ProviderID,
		RuntimeKey: key,
		Label:      "Open Design API key",
		Note:       "Open Design API key (no plan allowance: sign in with an account to read one)",
	}
	return m.persistAndRespond(record)
}

// accountLabel names an account without ever showing a key.
func accountLabel(email, uid string) string {
	if email = strings.TrimSpace(email); email != "" {
		return email
	}
	if uid = strings.TrimSpace(uid); uid != "" {
		return uid
	}
	return "Open Design account"
}

// persistAndRespond writes the auth file through the host and answers the poll.
//
// The file name is fixed by the record (opendesign-<uid>.json), so re-signing in
// the same account rewrites one file instead of accumulating one per sign-in.
func (m *Manager) persistAndRespond(record *authRecord) ([]byte, error) {
	if err := m.persistRecord(record); err != nil {
		return errorEnvelopeWithStatus("auth_save_failed", err.Error(), http.StatusInternalServerError, false), nil
	}
	m.afterAccountAdded(record)
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   toAuthData(record, false, ""),
	}), nil
}

// persistRecord writes one record through host.auth.save.
func (m *Manager) persistRecord(record *authRecord) error {
	if m.bridge == nil {
		return fmt.Errorf("the plugin has no host transport, so the credential cannot be saved")
	}
	ctx, cancel := context.WithTimeout(context.Background(), authPersistTimeout)
	defer cancel()
	if err := m.bridge.AuthSave(ctx, record.FileName(), record.storageJSON()); err != nil {
		// The host's error text is not trusted: it may echo the credential JSON.
		return fmt.Errorf("the host could not save the credential")
	}
	return nil
}

// toAuthData renders a record as the host's auth data.
//
// `type` is required by the host for auth-file classification, and `note` /
// `logo` / `disabled` are the fields the auth row shows. status is "error" only
// when the account was refused by the vendor, and message says why in the
// vendor's own words.
func toAuthData(record *authRecord, refused bool, message string) pluginapi.AuthData {
	meta := map[string]any{
		"type":     ProviderID,
		"provider": ProviderID,
		"logo":     pluginLogoURL,
	}
	if refused {
		meta["status"] = "error"
		meta["disabled"] = true
		if message != "" {
			meta["status_message"] = message
		}
	}
	note := strings.TrimSpace(record.Note)
	if note == "" {
		note = record.label()
	}
	if !record.hasControlKey() {
		// The limitation is stated where the operator will see it rather than
		// discovered when the panel cannot render an allowance.
		note = note + " — no plan allowance: an API key sign-in has no control key"
	}
	meta["note"] = note
	return pluginapi.AuthData{
		Provider:    ProviderID,
		ID:          record.AuthID(),
		FileName:    record.FileName(),
		Label:       record.label(),
		Disabled:    refused,
		StorageJSON: record.storageJSON(),
		Metadata:    meta,
	}
}

// afterAccountAdded publishes the model list the new account is entitled to, so
// a sign-in is followed by models appearing without a manual reconfigure.
func (m *Manager) afterAccountAdded(record *authRecord) {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	if m.bridge == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), registerRefreshTimeout)
	defer cancel()
	result := catalog.Fetch(ctx, m.bridge.Transport(), cfg, record.RuntimeKey)
	switch result.Kind {
	case catalog.LiveOK:
		m.storeSnapshot(record.RuntimeKey, snapshotFrom(result))
	case catalog.LiveRefused:
		_ = m.log("warn", "the new account's key was refused while reading its model catalog", map[string]any{
			"reason": result.Message,
		})
	default:
		_ = m.log("warn", "the new account's model catalog could not be read; serving the seed", map[string]any{
			"error": errorText(result.Err),
		})
	}
	// The new credential must be in the pool the request path reads, which is
	// the config's accounts plus the auth store. A fresh snapshot is enough:
	// poolAccounts re-reads the auth store on the next request.
}

// handleAuthRefresh answers auth.refresh.
//
// The two keys do not expire and are not renewable, so the only correct answer
// is the record unchanged: reporting a rotation here would be a lie the host
// then acts on by re-saving an identical file.
func (m *Manager) handleAuthRefresh(request []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed auth refresh body"), nil
	}
	record, err := m.parseAuthRecord(req.StorageJSON)
	if err != nil {
		return ErrEnvelope("invalid_auth", "the auth record carries no usable runtime key"), nil
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{Auth: toAuthData(record, false, "")}), nil
}

// handleParseAuth answers auth.parse: the plugin claims a file only when it is
// its own, so another provider's record is never rewritten by this one.
func (m *Manager) handleParseAuth(request []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed auth parse body"), nil
	}
	name := strings.ToLower(strings.TrimSpace(req.FileName))
	ours := strings.HasPrefix(name, authFileNamePrefix) || strings.EqualFold(strings.TrimSpace(req.Provider), ProviderID)
	if !ours {
		var probe struct {
			Type     string `json:"type"`
			Provider string `json:"provider"`
		}
		if json.Unmarshal(req.RawJSON, &probe) == nil {
			ours = strings.EqualFold(strings.TrimSpace(probe.Type), ProviderID) ||
				strings.EqualFold(strings.TrimSpace(probe.Provider), ProviderID)
		}
	}
	if !ours {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false}), nil
	}
	record, err := m.parseAuthRecord(req.RawJSON)
	if err != nil {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false}), nil
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth:    toAuthData(record, false, ""),
	}), nil
}
