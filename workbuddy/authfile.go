// authfile.go owns every physical auth-file path the plugin touches: the
// workbuddy-<uid>.json naming rule, UID sanitization (path-traversal defense),
// path safety checks, and the read / write / delete helpers that talk to the
// host's auth store via host.auth.* RPC. Callers above (lifecycle reconcile)
// decide when to disable / re-enable / delete; this file decides how.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// authFileNameFor is the canonical on-disk name for one credential:
//
//	personal   workbuddy-<uid>.json          (unchanged)
//	enterprise workbuddy-<uid>-<enterpriseId>.json
//
// The enterprise suffix exists because one person's personal account and
// their team account (「WorkBuddy 教育公益版」 and friends) share BOTH the
// uid and the nickname — signing in to the team account used to overwrite
// the personal one, and the two could never sit in one pool. The uid alone
// cannot tell them apart; the enterpriseId can.
//
// Bare "workbuddy.json" stays the legacy single-account name (no UID).
var unsafeUIDChars = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func sanitizeUIDForFileName(uid string) string {
	uid = strings.TrimSpace(uid)
	uid = unsafeUIDChars.ReplaceAllString(uid, "_")
	if uid == "" || uid == "." || uid == ".." {
		return ""
	}
	if len(uid) > 64 {
		uid = uid[:64]
	}
	return uid
}

// accountName is the name an account is listed under: the person's own name,
// plus the team's for an enterprise account. Mirrors the reference
// implementation's accountName(): an enterprise is always distinguished from
// the same person's personal account, falling back to the end of its id when
// WorkBuddy names none.
func accountName(who accountData, ent enterpriseRef) string {
	person := strings.TrimSpace(firstNonEmpty(who.Nickname, who.UID))
	if strings.TrimSpace(ent.ID) == "" {
		return person
	}
	team := strings.TrimSpace(ent.Name)
	if team == "" {
		team = "enterprise " + enterpriseIDSuffix(ent.ID)
	}
	if person == "" {
		return team
	}
	return person + " · " + team
}

// enterpriseIDSuffix returns the last six characters of an enterprise id —
// the reference implementation's `ent.id.slice(-6)` fallback label.
func enterpriseIDSuffix(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 6 {
		return id
	}
	return id[len(id)-6:]
}

// credentialIDFor is the plugin-side identity of one credential: the sanitized
// uid, plus "-<sanitized enterpriseId>" for an enterprise account. It is the
// same string authFileNameFor builds its name from, so the ID the plugin
// reports and the file the host stores always agree on who a credential is.
//
// The host derives its record key from the file path whenever the plugin
// leaves AuthData.ID empty (pluginAuthDataToCoreAuth -> authIDForPath), which
// is what the login/parse/refresh paths do. Keeping the ID consistent too
// means no future caller that does send it can key one person's personal and
// team accounts alike.
func credentialIDFor(sa *storedAuth) string {
	if sa == nil {
		return ""
	}
	uid := sanitizeUIDForFileName(sa.Account.UID)
	if uid == "" {
		return ""
	}
	if entID := sanitizeUIDForFileName(sa.Account.EnterpriseID); entID != "" {
		return uid + "-" + entID
	}
	return uid
}

// authFileNameFor returns the canonical file name for one credential. An
// enterprise account adds "-<sanitized enterpriseId>" so it can never collide
// with the same person's personal account.
func authFileNameFor(sa *storedAuth) string {
	if id := credentialIDFor(sa); id != "" {
		return "workbuddy-" + id + ".json"
	}
	return authFileName
}

// isLegacyWorkbuddyAuthName reports the historical single-file name that collides
// with multi-account workbuddy-<uid>.json for the same credential.

func isLegacyWorkbuddyAuthName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), authFileName)
}

func shouldDeleteLegacyForUID(raw []byte, uid string) bool {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return false
	}
	sa, err := parseStored(raw)
	return err == nil && strings.EqualFold(strings.TrimSpace(sa.Account.UID), uid)
}

// shouldDeleteMigratedSource reports whether the file being migrated away from
// holds the SAME credential that was just written under the target name.
//
// The uid alone is no longer enough: one person's personal account and their
// team account share the uid, so a uid-only check would let an enterprise
// migration delete the personal account's file (and the reverse). Two
// credentials are the same when uid AND enterpriseId both match; that covers
// the pre-enterprise file (no enterpriseId) migrating to the enterprise name,
// and the bare legacy workbuddy.json migrating to either form.
func shouldDeleteMigratedSource(raw []byte, sa *storedAuth) bool {
	if sa == nil {
		return false
	}
	uid := strings.TrimSpace(sa.Account.UID)
	if uid == "" {
		return false
	}
	source, err := parseStored(raw)
	if err != nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(source.Account.UID), uid) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(source.Account.EnterpriseID), strings.TrimSpace(sa.Account.EnterpriseID))
}

// resolveAuthFileTarget picks the canonical file name + path for save/delete.
// Prefer workbuddy-<uid>.json; if the host still points at legacy workbuddy.json
// for a UID-bearing account, rewrite to the uid name and schedule legacy removal.
//
// The same migration covers the enterprise rename: a credential that carries an
// enterpriseId but still lives at workbuddy-<uid>.json (written before the
// enterprise suffix existed) is saved under workbuddy-<uid>-<enterpriseId>.json
// and the old file is removed. Without this, the upgraded plugin would keep
// writing the legacy name — and the next login would again overwrite it.
func resolveAuthFileTarget(sa *storedAuth, phys *hostAuthPhysical) (name, path string, legacyPath string) {
	name = authFileNameFor(sa)
	if phys != nil {
		path = strings.TrimSpace(phys.Path)
		physName := strings.TrimSpace(phys.Name)
		// The account is on the uid-only name but its canonical name now carries
		// the enterprise suffix: that file is the migration source.
		staleUIDOnlyName := sa != nil && isUIDOnlyWorkbuddyAuthName(physName, sa.Account.UID) &&
			!strings.EqualFold(physName, name)
		if physName != "" && !isLegacyWorkbuddyAuthName(physName) && !staleUIDOnlyName {
			// Already on multi-account name — keep host name (should match uid form).
			name = physName
		}
		if isLegacyWorkbuddyAuthName(physName) || isLegacyWorkbuddyAuthName(filepath.Base(path)) || staleUIDOnlyName {
			if sa != nil && strings.TrimSpace(sa.Account.UID) != "" {
				// Migrate: write canonical, delete legacy path after save.
				legacyPath = path
				// After persist to name, remove legacyPath if different.
			}
		}
	}
	return name, path, legacyPath
}

// isUIDOnlyWorkbuddyAuthName reports whether name is exactly the uid-only
// multi-account name for uid (workbuddy-<uid>.json), i.e. the pre-enterprise
// form — not the enterprise form, and not the bare legacy name.
func isUIDOnlyWorkbuddyAuthName(name, uid string) bool {
	name = strings.TrimSpace(name)
	uid = strings.TrimSpace(uid)
	if name == "" || uid == "" {
		return false
	}
	sanitized := sanitizeUIDForFileName(uid)
	if sanitized == "" {
		return false
	}
	return strings.EqualFold(name, "workbuddy-"+sanitized+".json")
}

// hostAuthPersist saves via host API only. Dual-writing the physical path after
// a successful host.auth.save is redundant (host already WriteFile) and can
// re-fire the watcher → extra re-parse / transient dual registration risk.

type hostAuthPhysical struct {
	AuthIndex string
	Name      string
	Path      string
	JSON      []byte
	Disabled  bool
}

func hostAuthGetPhysical(authIndex string) (*hostAuthPhysical, error) {
	body, _ := json.Marshal(map[string]string{"auth_index": authIndex})
	raw, err := hostCall(pluginabi.MethodHostAuthGet, body)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.get: bad envelope")
	}
	var resp rpcHostAuthGetResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	return &hostAuthPhysical{
		AuthIndex: resp.AuthIndex,
		Name:      resp.Name,
		Path:      resp.Path,
		JSON:      resp.JSON,
		Disabled:  parseDisabledFromAuthJSON(resp.JSON),
	}, nil
}

// hostAuthSaveJSON persists credential JSON via host.auth.save.

func hostAuthPersist(name, path string, raw []byte) error {
	_ = path // reserved for callers that still pass physical path for migrate logic
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("empty auth file name")
	}
	return hostAuthSaveJSON(name, raw)
}

// hostAuthPersistMigrate is like hostAuthPersist but also removes a legacy path
// when the canonical name differs (workbuddy.json → workbuddy-<uid>.json).

func hostAuthPersistMigrate(name, path, legacyPath string, raw []byte) error {
	if err := hostAuthPersist(name, path, raw); err != nil {
		return err
	}
	// If path was legacy and name is canonical, also write canonical path next to it.
	if legacyPath != "" && !strings.EqualFold(filepath.Base(legacyPath), name) {
		target, targetErr := parseStored(raw)
		legacyRaw, legacyErr := os.ReadFile(legacyPath)
		if targetErr == nil && legacyErr == nil && shouldDeleteMigratedSource(legacyRaw, target) {
			_ = deleteAuthFileInDir(legacyPath, filepath.Dir(legacyPath))
		}
	}
	// If path points at legacy but name is uid form, do not dual-write path (would keep legacy alive).
	return nil
}

// buildAuthFileJSON produces host-save payload: nested storage + top-level metadata.
// extra merges additional top-level keys (optional).

func hostAuthSaveJSON(name string, raw []byte) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("empty auth file name")
	}
	saveReq := pluginapi.HostAuthSaveRequest{
		Name: name,
		JSON: raw,
	}
	saveBody, _ := json.Marshal(saveReq)
	rawResp, err := hostCall(pluginabi.MethodHostAuthSave, saveBody)
	if err != nil {
		return fmt.Errorf("host.auth.save: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(rawResp, &env); err != nil || !env.OK {
		msg := "host.auth.save failed"
		if env.Error != nil && env.Error.Message != "" {
			msg = truncateRedacted(env.Error.Message, 200)
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// lifecycleStateUnchanged avoids redundant saves when note/disabled unchanged.

// buildAuthFileJSON produces host-save payload: nested storage + top-level
// metadata. It starts from the auth file the host already holds (physical, may
// be nil) so user-owned top-level fields — weight, priority, proxy_url, prefix,
// headers, request_retry… — survive every lifecycle-driven rewrite. Only the
// keys this plugin owns are overwritten.
func buildAuthFileJSON(physical []byte, sa *storedAuth, disabled bool, note string, extra map[string]any) ([]byte, error) {
	if sa == nil {
		return nil, fmt.Errorf("nil storedAuth")
	}
	storage, err := json.Marshal(sa)
	if err != nil {
		return nil, err
	}
	var nested map[string]any
	if err := json.Unmarshal(storage, &nested); err != nil {
		return nil, err
	}
	out := map[string]any{}
	if len(physical) > 0 {
		if err := json.Unmarshal(physical, &out); err != nil {
			return nil, fmt.Errorf("parse existing auth json: %w", err)
		}
	}
	// A fault note (re-login required / banned) is the only record of why an
	// account is disabled, and every writer here rebuilds the file from scratch.
	// Downgrading it to the credits wording would erase the reason the panel and
	// the re-enable gate depend on, so the fault note wins while disabled.
	if disabled {
		if existing := parseNoteFromAuthJSON(physical); existing != "" && reenableBlockedByNote(existing) {
			note = existing
		}
	}
	out["type"] = providerName
	out["provider"] = providerName
	out["logo"] = pluginLogoURL
	out["disabled"] = disabled
	out["note"] = note
	out["auth"] = nested["auth"]
	out["account"] = nested["account"]
	for k, v := range extra {
		out[k] = v
	}
	return json.Marshal(out)
}

func buildRefreshedAuthJSON(physical []byte, sa *storedAuth) ([]byte, error) {
	if sa == nil {
		return nil, fmt.Errorf("nil storedAuth")
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(physical, &doc); err != nil {
		return nil, err
	}
	storage, err := json.Marshal(sa)
	if err != nil {
		return nil, err
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(storage, &nested); err != nil {
		return nil, err
	}
	doc["auth"] = nested["auth"]
	doc["account"] = nested["account"]
	return json.Marshal(doc)
}

// physJSON returns the raw auth file the host holds, tolerating a nil record.
func physJSON(phys *hostAuthPhysical) []byte {
	if phys == nil {
		return nil
	}
	return phys.JSON
}

// parseDisabledFromAuthJSON reads top-level disabled from physical auth JSON.

func parseDisabledFromAuthJSON(raw []byte) bool {
	var m struct {
		Disabled bool `json:"disabled"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Disabled
}

// parseNoteFromAuthJSON reads the top-level note from physical auth JSON. The
// note is the only persisted place a disable reason survives a process
// restart, so lifecycle gates read it back instead of trusting in-memory state.
func parseNoteFromAuthJSON(raw []byte) string {
	var m struct {
		Note string `json:"note"`
	}
	_ = json.Unmarshal(raw, &m)
	return strings.TrimSpace(m.Note)
}

// isSafeWorkbuddyAuthPath rejects non-WorkBuddy filenames, empty paths, and
// traversal attempts. It validates both the basename pattern AND that the path
// does not escape via ".." segments. Callers that need to confine deletes to
// a specific directory should additionally check isPathUnder(path, dir).
//
// Accepted names: the bare legacy workbuddy.json and every
// workbuddy-<uid>[-<enterpriseId>].json form. Both suffix segments are
// sanitized by sanitizeUIDForFileName (alphanumerics, "_" and "-" only) before
// they reach a file name, and traversal is rejected above regardless of
// basename, so the prefix check is the whole pattern.

func isSafeWorkbuddyAuthPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	// Reject any path containing ".." — prevents traversal regardless of basename.
	if strings.Contains(filepath.ToSlash(path), "../") || strings.Contains(filepath.ToSlash(path), "/..") {
		return false
	}
	base := filepath.Base(path)
	lower := strings.ToLower(base)
	if !strings.HasPrefix(lower, "workbuddy-") && lower != "workbuddy.json" {
		return false
	}
	if !strings.HasSuffix(lower, ".json") {
		return false
	}
	// Path traversal / absolute weirdness: base must equal cleaned base.
	if base != filepath.Base(filepath.Clean(path)) {
		return false
	}
	return true
}

// isPathUnder reports whether path is inside dir (after cleaning both).
// Empty dir means "no constraint" (returns true for any safe path).

func isPathUnder(path, dir string) bool {
	path = strings.TrimSpace(path)
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return true
	}
	cleanPath := filepath.Clean(path)
	cleanDir := filepath.Clean(dir)
	if cleanPath == cleanDir {
		return false // path is the dir itself, not under it
	}
	rel, err := filepath.Rel(cleanDir, cleanPath)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..") && !strings.Contains(rel, string(filepath.Separator)+"..")
}

// deleteAuthFileAt removes a WorkBuddy auth file. Missing file is success.
// Deprecated: use deleteAuthFileInDir instead (adds directory + absolute path
// confinement). Retained for test coverage of the base safe-delete path.

func deleteAuthFileAt(path string) error {
	if !isSafeWorkbuddyAuthPath(path) {
		return fmt.Errorf("refusing to delete unsafe path: %s", path)
	}
	err := os.Remove(path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// deleteAuthFileInDir is like deleteAuthFileAt but additionally requires the
// path to be under dir. Use for lifecycle deletes where the auth directory is
// known — prevents a malicious/buggy host path from deleting arbitrary files.
// The path MUST be absolute (defense against relative-path CWD deletion).

func deleteAuthFileInDir(path, dir string) error {
	if !isSafeWorkbuddyAuthPath(path) {
		return fmt.Errorf("refusing to delete unsafe path: %s", path)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("refusing to delete relative path: %s", path)
	}
	if dir != "" && !isPathUnder(path, dir) {
		return fmt.Errorf("refusing to delete path outside auth dir: %s (dir=%s)", path, dir)
	}
	err := os.Remove(path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// hostAuthGetFull returns physical JSON, path, and name for an auth index.
