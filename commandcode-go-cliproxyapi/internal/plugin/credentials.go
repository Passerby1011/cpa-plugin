package plugin

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/config"
)

// authRecordCredential is the payload this plugin owns inside a CPA auth
// record. The host persists it as a file and AuthGet returns it verbatim, so
// this struct is the durable shape of a key added from the quota page - it
// must stay backward compatible, since records written by an older release
// are still read by a newer one.
type authRecordCredential struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Label  string `json:"label"`
	APIKey string `json:"api_key"`
	// Mode is an extension over the provider credential shape the host knows:
	// it carries the transport this key needs. Absent (older records) falls
	// back to provider mode, the documented default.
	Mode string `json:"mode,omitempty"`
}

// refreshAuthCredentials reloads the credentials stored as CPA auth records.
//
// Two credential sources exist on purpose:
//
//   - the plugin's own config (`accounts`), which is what the management form
//     edits and what the plugin has always used; and
//   - CPA auth records carrying this plugin's provider type, which is where a
//     key added from the quota page lands.
//
// The union is what makes an added key usable: the host already schedules auth
// records, and this makes the plugin execute with them too. Config stays
// authoritative for what it declares - nothing here removes or rewrites a
// config account.
//
// A failure is reported but never fatal: a broken auth store must not take
// down a pool that its config can still drive.
func (m *Manager) refreshAuthCredentials(ctx context.Context, bridge *HostBridge) error {
	if bridge == nil {
		return nil
	}
	entries, err := bridge.AuthList(ctx)
	if err != nil {
		return err
	}
	found := make([]config.Account, 0, len(entries))
	for _, entry := range entries {
		if !strings.EqualFold(strings.TrimSpace(entry.Type), ProviderID) &&
			!strings.EqualFold(strings.TrimSpace(entry.Provider), ProviderID) {
			continue
		}
		if entry.Disabled {
			continue
		}
		index := strings.TrimSpace(entry.AuthIndex)
		if index == "" {
			index = strings.TrimSpace(entry.ID)
		}
		if index == "" {
			continue
		}
		got, errGet := bridge.AuthGet(ctx, index)
		if errGet != nil || len(got.JSON) == 0 {
			continue
		}
		var record authRecordCredential
		if json.Unmarshal(got.JSON, &record) != nil {
			continue
		}
		cred := strings.TrimSpace(record.APIKey)
		if cred == "" {
			continue
		}
		mode := config.TransportMode(strings.TrimSpace(record.Mode))
		if !mode.Valid() {
			mode = config.TransportProvider
		}
		label := strings.TrimSpace(record.Label)
		if label == "" {
			label = strings.TrimSpace(entry.Label)
		}
		found = append(found, config.Account{Label: label, Mode: mode, Credential: cred})
	}

	m.credMu.Lock()
	m.authCreds = found
	m.authCredsLoaded = true
	m.credMu.Unlock()
	return nil
}

// poolAccounts returns every credential the pool may use: the config's own
// accounts first, then any credential stored as an auth record. Deduplicated
// by credential so a key declared in BOTH places is offered once.
//
// It performs no host round-trips - the auth-record list is a snapshot taken
// at lifecycle - so it is safe on the request path.
func (m *Manager) poolAccounts(cfg config.Config) []config.Account {
	base := cfg.EffectiveAccounts()
	m.credMu.Lock()
	extra := append([]config.Account(nil), m.authCreds...)
	m.credMu.Unlock()
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]struct{}, len(base)+len(extra))
	out := make([]config.Account, 0, len(base)+len(extra))
	for _, account := range append(base, extra...) {
		cred := strings.TrimSpace(account.Credential)
		if cred == "" {
			continue
		}
		if _, dup := seen[cred]; dup {
			continue
		}
		seen[cred] = struct{}{}
		out = append(out, account)
	}
	return out
}
