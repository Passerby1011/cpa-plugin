package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

// A record is named after the account, never after the key: the auth file name
// is what the host writes into its auth directory and what an operator sees.
func TestAuthRecordFileNameAndID(t *testing.T) {
	record := &authRecord{
		Type:       ProviderID,
		RuntimeKey: "sk-amr-runtime-secret",
		ControlKey: "vela_ctrl_control-secret",
		UID:        "u-12345",
		Email:      "ada@example.com",
	}
	if got := record.FileName(); got != "opendesign-u-12345.json" {
		t.Errorf("file name = %q, want opendesign-<uid>.json", got)
	}
	if got := record.AuthID(); got != "opendesign-u-12345" {
		t.Errorf("id = %q", got)
	}
	// With no uid the id falls back to a digest, and must not carry the key.
	if got := record.AuthID(); strings.Contains(got, "sk-amr") {
		t.Errorf("the auth id leaked the key: %q", got)
	}
}

func TestAuthRecordFallsBackToADigestWithoutAUid(t *testing.T) {
	record := &authRecord{RuntimeKey: "sk-amr-runtime-secret"}
	id := record.AuthID()
	if !strings.HasPrefix(id, "opendesign-") || strings.Contains(id, "sk-amr") {
		t.Fatalf("id = %q, want a prefix plus a non-secret digest", id)
	}
	if len(record.FileName()) < len("opendesign-.json") {
		t.Errorf("file name = %q", record.FileName())
	}
}

// The label must never be the key: a label shows up in the Management Center and
// in logs.
func TestLabelNeverCarriesAKey(t *testing.T) {
	for _, record := range []*authRecord{
		{RuntimeKey: "sk-amr-secret"},
		{RuntimeKey: "sk-amr-secret", UID: "u1"},
		{RuntimeKey: "sk-amr-secret", Email: "ada@example.com"},
	} {
		if label := record.label(); strings.Contains(label, "sk-amr") {
			t.Errorf("label leaked the key: %q", label)
		}
	}
}

// sanitizeUID keeps an id usable as a file name: no traversal, no separator.
func TestSanitizeUIDRefusesTraversal(t *testing.T) {
	for _, uid := range []string{"../../etc/passwd", "a/b", `a\b`, "x..y"} {
		got := sanitizeUID(uid)
		if strings.ContainsAny(got, `/\`) || strings.Contains(got, "..") {
			t.Errorf("sanitizeUID(%q) = %q", uid, got)
		}
	}
}

// A record written by an older release, or pasted straight into an auth file, is
// read back: key/api_key/access are all accepted spellings.
func TestParseAuthRecordAcceptsAlternateSpellings(t *testing.T) {
	m := NewManager(nil)
	for _, body := range []string{
		`{"type":"opendesign","runtime_key":"sk-amr-r","control_key":"vela_ctrl_c"}`,
		`{"api_key":"sk-amr-r","control":"vela_ctrl_c"}`,
		`{"key":"sk-amr-r","control_key":"vela_ctrl_c"}`,
		`{"access":"sk-amr-r","control":"vela_ctrl_c"}`,
	} {
		record, err := m.parseAuthRecord([]byte(body))
		if err != nil {
			t.Errorf("%s: %v", body, err)
			continue
		}
		if record.RuntimeKey != "sk-amr-r" {
			t.Errorf("%s: runtime key = %q", body, record.RuntimeKey)
		}
		if record.ControlKey != "vela_ctrl_c" {
			t.Errorf("%s: control key = %q", body, record.ControlKey)
		}
	}
}

// A control key in the runtime slot is moved to the control slot, never used to
// sign a model request.
func TestParseAuthRecordMovesAControlKeyOutOfTheRuntimeSlot(t *testing.T) {
	m := NewManager(nil)
	record, err := m.parseAuthRecord([]byte(`{"api_key":"vela_ctrl_c"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if record.RuntimeKey != "" {
		t.Errorf("runtime key = %q, want empty", record.RuntimeKey)
	}
	if record.ControlKey != "vela_ctrl_c" {
		t.Errorf("control key = %q", record.ControlKey)
	}
	if record.hasRuntimeKey() {
		t.Errorf("a control key must not satisfy hasRuntimeKey")
	}
}

// storageJSON round-trips both keys, which is what the host persists.
func TestStorageJSONCarriesBothKeys(t *testing.T) {
	record := &authRecord{RuntimeKey: "sk-amr-r", ControlKey: "vela_ctrl_c", UID: "u1"}
	raw := record.storageJSON()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc["runtime_key"] != "sk-amr-r" || doc["control_key"] != "vela_ctrl_c" {
		t.Errorf("storage = %s", raw)
	}
	if doc["type"] != ProviderID {
		t.Errorf("type = %v, want the provider key", doc["type"])
	}
}

// A pasted-key account has no control key, and the auth row says so rather than
// leaving the operator to discover it when the panel cannot render an allowance.
func TestAuthDataStatesTheMissingAllowance(t *testing.T) {
	record := &authRecord{RuntimeKey: "sk-amr-r"}
	auth := toAuthData(record, false, "")
	note, _ := auth.Metadata["note"].(string)
	if !strings.Contains(note, "no plan allowance") {
		t.Errorf("note = %q, want the limitation stated", note)
	}
	if auth.FileName != record.FileName() {
		t.Errorf("file name = %q", auth.FileName)
	}
	if strings.Contains(auth.Label, "sk-amr") {
		t.Errorf("label leaked the key: %q", auth.Label)
	}
	// With a control key the note does not carry the limitation.
	withPlan := toAuthData(&authRecord{RuntimeKey: "sk-amr-r", ControlKey: "vela_ctrl_c"}, false, "")
	if n, _ := withPlan.Metadata["note"].(string); strings.Contains(n, "no plan allowance") {
		t.Errorf("note = %q, want no limitation for an account sign-in", n)
	}
}

// A refused account is marked disabled with the vendor's own words, so the host
// stops scheduling it.
func TestRefusedAuthDataIsDisabled(t *testing.T) {
	record := &authRecord{RuntimeKey: "sk-amr-r", ControlKey: "vela_ctrl_c", UID: "u1"}
	auth := toAuthData(record, true, "invalid_api_key")
	if !auth.Disabled {
		t.Errorf("a refused account must be disabled")
	}
	if msg, _ := auth.Metadata["status_message"].(string); msg != "invalid_api_key" {
		t.Errorf("status message = %q", msg)
	}
	if status, _ := auth.Metadata["status"].(string); status != "error" {
		t.Errorf("status = %q", status)
	}
}

// isOwnRecord is by the file-name prefix first: auth files written before "type"
// existed on disk do not carry it.
func TestIsOwnRecordUsesTheFileNamePrefix(t *testing.T) {
	if !isOwnRecord(hostAuthEntry("opendesign-u1.json", "", "")) {
		t.Errorf("a prefixed file must be ours")
	}
	if !isOwnRecord(hostAuthEntry("x.json", ProviderID, "")) {
		t.Errorf("a typed record must be ours")
	}
	if isOwnRecord(hostAuthEntry("workbuddy-u1.json", "workbuddy", "workbuddy")) {
		t.Errorf("another provider's file must not be claimed")
	}
}

// A pasted key is trimmed and refused when it is not a runtime key.
func TestNormalizePastedKey(t *testing.T) {
	cases := map[string]string{
		"  sk-amr-k  ":         "sk-amr-k",
		"Bearer sk-amr-k":      "sk-amr-k",
		"vela_ctrl_c":          "",
		"sk-amr-k with spaces": "",
		"":                     "",
	}
	for in, want := range cases {
		if got := normalizePastedKey(in); got != want {
			t.Errorf("normalizePastedKey(%q) = %q, want %q", in, got, want)
		}
	}
}
