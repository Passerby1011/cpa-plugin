package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// syncAccountEmail persists one account's email into its own auth file so the
// host can surface it as the credential's email (and the management panel can
// title the entry with the mailbox instead of the key hash).
//
// The write goes straight to the file rather than through host.auth.save: the
// host derives a saved record's ID from the file path (commandcode-key-<hash>.json)
// while this plugin's ParseAuth returns the ID stored in the JSON (without the
// .json suffix), and the host performs no path-based dedupe, so a save would
// register a second credential for the same key that lacks the api_key routing
// attribute and therefore fails every request routed to it. Writing the file
// instead leaves the credential identity untouched and lets the auth-directory
// watcher re-parse it through ParseAuth.
//
// The email is the only field that changes: the existing bytes are edited in
// place, so api_key, disabled, label, id and any host-managed metadata survive
// byte for byte. Failures are reported to the caller, which treats them as
// best-effort so a quota refresh never fails over a cosmetic sync.
func (m *Manager) syncAccountEmail(ctx context.Context, key, email string) error {
	if m.bridge == nil {
		return fmt.Errorf("account email sync unavailable: no host bridge")
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return fmt.Errorf("account email sync skipped: account reported no email")
	}
	id := authRecordIDFromHash(authKeyHash(key))
	fileName := authFileNameFromHash(authKeyHash(key))

	entries, err := m.bridge.AuthList(ctx)
	if err != nil {
		return fmt.Errorf("account email sync failed: %w", err)
	}
	path := ""
	known := ""
	for _, entry := range entries {
		if !strings.EqualFold(strings.TrimSpace(entry.ID), id) && strings.TrimSpace(entry.Name) != fileName {
			continue
		}
		path = strings.TrimSpace(entry.Path)
		known = strings.TrimSpace(entry.Email)
		break
	}
	if path == "" {
		return fmt.Errorf("account email sync skipped: no auth file for this key")
	}
	if known == email {
		return nil
	}
	if !strings.HasSuffix(path, ".json") {
		return fmt.Errorf("account email sync skipped: auth path is not a json file")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("account email sync failed: %w", err)
	}
	if !gjson.ValidBytes(raw) {
		return fmt.Errorf("account email sync skipped: auth file is not valid json")
	}
	if gjson.GetBytes(raw, "email").String() == email {
		return nil
	}
	merged, err := sjson.SetBytes(raw, "email", email)
	if err != nil {
		return fmt.Errorf("account email sync failed: %w", err)
	}
	// Atomic replace: the watcher reads the file on every write event, so a
	// partially written file would make it drop the event and never retry.
	// The temp name deliberately avoids the .json suffix the watcher filters
	// on, so only the rename is observed.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".commandcode-email-*.tmp")
	if err != nil {
		return fmt.Errorf("account email sync failed: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(merged); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("account email sync failed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("account email sync failed: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("account email sync failed: %w", err)
	}
	return nil
}

// accountEmails maps each CommandCode credential id to the mailbox already
// stored on its auth file. Missing or unreadable entries are omitted so the
// quota page can treat those keys as never-refreshed and fetch them once.
// AuthList is a local host callback, not an upstream round-trip.
func (m *Manager) accountEmails(ctx context.Context) map[string]string {
	out := make(map[string]string)
	if m == nil || m.bridge == nil {
		return out
	}
	if ctx != nil && ctx.Err() != nil {
		return out
	}
	entries, err := m.bridge.AuthList(ctx)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		email := strings.TrimSpace(entry.Email)
		if email == "" {
			continue
		}
		if id := strings.TrimSpace(entry.ID); id != "" {
			out[id] = email
		}
		if name := strings.TrimSpace(entry.Name); name != "" {
			out[strings.TrimSuffix(name, ".json")] = email
		}
	}
	return out
}
