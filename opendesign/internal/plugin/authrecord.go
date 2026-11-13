// Package plugin — the durable auth record.
//
// An Open Design account is TWO keys plus the account they belong to:
//
//	runtime  sk-amr-…      rides every model request
//	control  vela_ctrl-…   reads the account's plan and its allowance
//
// The record below is what this plugin owns inside a CPA auth file, and it is
// the shape the host persists and hands back verbatim. It must stay
// backward-compatible: a record written by an older release is still read by a
// newer one, so fields are only ever added.
package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/opendesign/internal/config"
)

// authRecord is one account's durable credential.
type authRecord struct {
	// Type is the provider key, which is how the host classifies the file.
	Type string `json:"type"`
	// Provider repeats it for hosts that read that field instead.
	Provider string `json:"provider"`
	// Label is the user-facing name; it never carries key material.
	Label string `json:"label,omitempty"`
	// RuntimeKey is the sk-amr-… key the model plane takes.
	RuntimeKey string `json:"runtime_key"`
	// ControlKey is the vela_ctrl-… key the control plane takes. Absent for a
	// pasted API key, which is exactly what makes its allowance unreadable.
	ControlKey string `json:"control_key,omitempty"`
	Email      string `json:"email,omitempty"`
	UID        string `json:"uid,omitempty"`
	// Note mirrors the host's own note field so the auth row shows the account.
	Note string `json:"note,omitempty"`
}

// authFileNamePrefix is the file name every record this plugin writes uses.
const authFileNamePrefix = ProviderID + "-"

// errNoRuntimeKey is returned when a record carries no usable runtime key.
var errNoRuntimeKey = errors.New("the auth record carries no runtime key")

func (r *authRecord) hasRuntimeKey() bool {
	return r != nil && strings.TrimSpace(r.RuntimeKey) != ""
}

// hasControlKey reports whether the account's allowance can be read.
func (r *authRecord) hasControlKey() bool {
	return r != nil && strings.TrimSpace(r.ControlKey) != ""
}

func (r *authRecord) label() string {
	if r == nil {
		return ""
	}
	if l := strings.TrimSpace(r.Label); l != "" {
		return l
	}
	if e := strings.TrimSpace(r.Email); e != "" {
		return e
	}
	if u := strings.TrimSpace(r.UID); u != "" {
		return u
	}
	// Never the key: a label shows up in the Management Center and in logs.
	return "Open Design account"
}

// AuthID is the stable host auth id. It is derived from the account's uid when
// there is one and from the runtime key's digest otherwise, so re-saving the
// same credential is idempotent and no secret ever appears in an id.
func (r *authRecord) AuthID() string {
	if r == nil {
		return ProviderID
	}
	if uid := sanitizeUID(r.UID); uid != "" {
		return ProviderID + "-" + uid
	}
	return ProviderID + "-" + digestOf(r.RuntimeKey)
}

// FileName is the auth file name the host writes: opendesign-<uid>.json.
func (r *authRecord) FileName() string {
	return r.AuthID() + ".json"
}

// storageJSON is what the host persists. It carries both keys — the host's
// auth store is the only durable place this plugin has — so it must never be
// logged.
func (r *authRecord) storageJSON() []byte {
	if r == nil {
		return nil
	}
	// The type/provider fields are filled here rather than trusted from the
	// caller: the host classifies an auth file by them, and a record built
	// without them would land in the store unclassifiable.
	out := *r
	if out.Type == "" {
		out.Type = ProviderID
	}
	if out.Provider == "" {
		out.Provider = ProviderID
	}
	raw, _ := json.Marshal(out)
	return raw
}

// parseAuthRecord decodes a persisted record. Records written by an older
// release, and records a client pasted straight into the auth file, are both
// accepted: the key may be under key/api_key/access as well as runtime_key.
func (m *Manager) parseAuthRecord(raw []byte) (*authRecord, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error")
	}
	// The host nests a plugin's payload under "auth" for some providers; unwrap
	// it so one decoder covers both shapes.
	if nested, ok := probe["auth"]; ok {
		var inner map[string]json.RawMessage
		if json.Unmarshal(nested, &inner) == nil {
			if _, has := inner["runtime_key"]; has {
				probe = inner
			}
		}
	}
	var record authRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, fmt.Errorf("storage_parse_error")
	}
	if !record.hasRuntimeKey() {
		// Alternate spellings, in a fixed precedence.
		for _, key := range []string{"runtime-key", "api_key", "key", "access"} {
			if rawKey, ok := probe[key]; ok {
				var s string
				if json.Unmarshal(rawKey, &s) == nil && strings.TrimSpace(s) != "" {
					record.RuntimeKey = strings.TrimSpace(s)
					break
				}
			}
		}
	}
	if !record.hasControlKey() {
		for _, key := range []string{"control_key", "control-key", "control"} {
			if rawKey, ok := probe[key]; ok {
				var s string
				if json.Unmarshal(rawKey, &s) == nil && strings.TrimSpace(s) != "" {
					record.ControlKey = strings.TrimSpace(s)
					break
				}
			}
		}
	}
	// A control key is never a runtime key.
	if strings.HasPrefix(strings.TrimSpace(record.RuntimeKey), "vela_ctrl_") {
		if record.ControlKey == "" {
			record.ControlKey = record.RuntimeKey
		}
		record.RuntimeKey = ""
	}
	if record.Type == "" {
		record.Type = ProviderID
	}
	if record.Provider == "" {
		record.Provider = ProviderID
	}
	// A record with no runtime key is returned, not refused: the caller decides
	// what a missing key means (the request path refuses it, a diagnostics
	// read merely reports it), and refusing here would hide the control key of
	// an account whose runtime key was pasted wrong.
	return &record, nil
}

// authRecordAccounts returns the accounts stored as CPA auth records. This is
// where a credential added from the Management Center lands: the host's auth
// store is the only durable write this plugin has.
//
// A failure is logged, never fatal: a broken auth store must not take down a
// pool the config can still drive.
func (m *Manager) authRecordAccounts() []config.Account {
	if m.bridge == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), authReadTimeout)
	defer cancel()
	entries, err := m.bridge.AuthList(ctx)
	if err != nil {
		_ = m.log("warn", "could not list the host's auth records", map[string]any{"error": err.Error()})
		return nil
	}
	found := make([]config.Account, 0, len(entries))
	for _, entry := range entries {
		if !isOwnRecord(entry) {
			continue
		}
		if entry.Disabled {
			// A record the host disabled is one whose sign-in was refused; it
			// must not be offered as a usable credential.
			continue
		}
		index := strings.TrimSpace(entry.AuthIndex)
		if index == "" {
			index = strings.TrimSpace(entry.ID)
		}
		if index == "" {
			continue
		}
		got, errGet := m.bridge.AuthGet(ctx, index)
		if errGet != nil || len(got.JSON) == 0 {
			continue
		}
		record, errParse := m.parseAuthRecord(got.JSON)
		if errParse != nil || !record.hasRuntimeKey() {
			continue
		}
		found = append(found, config.Account{
			Label:      record.label(),
			RuntimeKey: record.RuntimeKey,
			ControlKey: record.ControlKey,
			Email:      record.Email,
			UID:        record.UID,
		})
	}
	return found
}

// isOwnRecord reports whether an auth entry belongs to this plugin. The file
// name prefix is the only reliable cross-version discriminator: auth files
// written before "type" existed on disk do not carry it.
func isOwnRecord(entry pluginapi.HostAuthFileEntry) bool {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(entry.Name)), authFileNamePrefix) {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(entry.Type), ProviderID) {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(entry.Provider), ProviderID)
}

// authReadTimeout bounds the auth-store reads a lifecycle performs.
const authReadTimeout = 10 * time.Second

// sanitizeUID keeps an id usable in a file name and in an auth id: only
// alphanumerics, '_' and '-' survive, so no traversal and no separator can
// reach the host's auth directory.
func sanitizeUID(uid string) string {
	uid = strings.TrimSpace(uid)
	var b strings.Builder
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// digestOf is the non-secret 12-hex-character digest that identifies one key
// without putting it anywhere visible.
func digestOf(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

// accountKey is the map key one account's snapshot is stored under: the runtime
// key's digest, never the key.
func accountKey(runtimeKey string) string {
	return digestOf(runtimeKey)
}
