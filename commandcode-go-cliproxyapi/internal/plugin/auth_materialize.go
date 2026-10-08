package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// materializeAuthRecords registers the pool's credentials as CPA auth records.
//
// # WHY THIS IS REQUIRED
//
// The host picks an executor by walking its OWN auth table for a record whose
// provider matches the requested model. A plugin provider with no auth record
// therefore has no candidate executor at all, and the request fails with
// "auth_not_found: no auth available" before this plugin's executor is ever
// called - the model list and the quota page still work, which makes the
// failure look like a routing problem rather than a missing registration.
//
// The credential remains authoritative in this plugin's pool; the auth record
// is what lets the host schedule the credential. Both sides read the same
// secret, so there is nothing extra to keep in sync.
//
// Records are keyed by the credential's digest, so re-registering the same key
// is idempotent and rotating a key's label or mode cannot orphan the old
// record. Existing records (by id or file name) are left untouched, which
// preserves any host-managed metadata written since the last lifecycle.
func (m *Manager) materializeAuthRecords(ctx context.Context, cfg config.Config) error {
	if m.bridge == nil {
		return nil
	}
	accounts := cfg.EffectiveAccounts()
	if len(accounts) == 0 {
		return nil
	}
	entries, err := m.bridge.AuthList(ctx)
	if err != nil {
		return fmt.Errorf("list existing auth records: %w", err)
	}
	existing := make(map[string]struct{}, len(entries)*2)
	for _, entry := range entries {
		if name := strings.TrimSpace(entry.Name); name != "" {
			existing[name] = struct{}{}
		}
		if id := strings.TrimSpace(entry.ID); id != "" {
			existing[id] = struct{}{}
		}
	}
	for _, account := range accounts {
		key := strings.TrimSpace(account.Credential)
		if key == "" {
			continue
		}
		hash := authKeyHash(key)
		id := authRecordIDFromHash(hash)
		name := authFileNameFromHash(hash)
		if _, ok := existing[id]; ok {
			continue
		}
		if _, ok := existing[name]; ok {
			continue
		}
		// The label carries the digest, never the secret: an auth record is
		// visible in the Management Center and in host logs.
		label := "CommandCode credential " + hash[:12]
		if l := strings.TrimSpace(account.Label); l != "" {
			label = l + " (" + hash[:12] + ")"
		}
		record, errMarshal := json.Marshal(struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			Label  string `json:"label"`
			APIKey string `json:"api_key"`
		}{
			Type: ProviderID, ID: id, Label: label, APIKey: key,
		})
		if errMarshal != nil {
			return fmt.Errorf("build auth record")
		}
		if errSave := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: name, JSON: record}); errSave != nil {
			return errSave
		}
		existing[id] = struct{}{}
		existing[name] = struct{}{}
		debugTrace("auth materialized id=%s file=%s", id, name)
	}
	return nil
}
