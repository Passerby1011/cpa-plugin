package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hex-ci/cpa-plugin/commandcode/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// accountsPath is the management route that adds one credential.
//
// The host exposes no way for a plugin to write its own config, so an added
// key cannot go into `accounts`. It goes into a CPA auth record instead: the
// host persists it, schedules it, and hands it back through host.auth.get -
// and refreshAuthCredentials folds it into the pool. A key added here is
// therefore usable immediately, without the operator editing config by hand.
const accountsPath = "/plugins/" + pluginName + "/accounts"

// addAccountRequest is the wire shape the quota page posts.
type addAccountRequest struct {
	// Credential is the CommandCode key ("user_..."). Required.
	Credential string `json:"credential"`
	// Label is display-only. Optional.
	Label string `json:"label"`
	// Mode is "go-cli" or "provider". Empty defaults to go-cli, because the
	// keys this page manages are Go-plan keys, and a Go key sent to the
	// Provider API is refused - defaulting to provider would add a credential
	// that cannot work.
	Mode string `json:"mode"`
}

// handleAddAccount persists one credential as an auth record this plugin owns.
//
// The response never echoes the credential: not in the body, not in an error,
// not in a log line.
func (m *Manager) handleAddAccount(ctx context.Context, body []byte) pluginapi.ManagementResponse {
	var req addAccountRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return managementJSON(http.StatusBadRequest, map[string]any{"error": "invalid request"})
		}
	}
	credential := strings.TrimSpace(req.Credential)
	if credential == "" {
		return managementJSON(http.StatusBadRequest, map[string]any{"error": "credential is required"})
	}
	if m.bridge == nil {
		return managementJSON(http.StatusServiceUnavailable, map[string]any{"error": "host bridge unavailable"})
	}
	mode := config.TransportMode(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = config.TransportGoCLI
	}
	if !mode.Valid() {
		return managementJSON(http.StatusBadRequest, map[string]any{"error": `mode must be "go-cli" or "provider"`})
	}

	hash := authKeyHash(credential)
	id := authRecordIDFromHash(hash)
	name := authFileNameFromHash(hash)
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = "CommandCode credential " + hash[:12]
	} else {
		label = label + " (" + hash[:12] + ")"
	}
	record, errMarshal := json.Marshal(authRecordCredential{
		Type: ProviderID, ID: id, Label: label, APIKey: credential, Mode: string(mode),
	})
	if errMarshal != nil {
		return managementJSON(http.StatusInternalServerError, map[string]any{"error": "could not build the credential record"})
	}
	if errSave := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: name, JSON: record}); errSave != nil {
		// The bridge already redacts host error text; do not add detail here.
		return managementJSON(http.StatusBadGateway, map[string]any{"error": "host refused to store the credential"})
	}
	// Re-read so the new key is in the pool immediately, not at the next
	// reconfigure.
	if errRefresh := m.refreshAuthCredentials(ctx, m.bridge); errRefresh != nil {
		return managementJSON(http.StatusBadGateway, map[string]any{"error": "credential stored but the account list could not be refreshed"})
	}
	// A deployment whose ONLY key was just added has no catalog yet: its
	// registration ran with zero credentials and skipped the fetch. Populate it
	// here so the new key yields models without an extra reconfigure.
	m.mu.RLock()
	cfg, mgr := m.cfg, m.mgr
	m.mu.RUnlock()
	if mgr != nil && len(mgr.Models()) == 0 {
		if errCat := m.refreshOnce(ctx, mgr, m.bridge, registerRefreshTimeout, cfg); errCat != nil {
			debugTrace("accounts add: catalog refresh failed")
		}
	}
	return managementJSON(http.StatusOK, map[string]any{"ok": true, "key_id": id, "label": label, "mode": string(mode)})
}

func managementJSON(status int, payload any) pluginapi.ManagementResponse {
	raw, _ := json.Marshal(payload)
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       raw,
	}
}
