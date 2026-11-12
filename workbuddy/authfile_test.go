package main

import (
	"strings"
	"testing"
)

// One person's personal account and their 「WorkBuddy 教育公益版」 (team) account
// share BOTH the uid and the nickname. Signing in to the team account used to
// write workbuddy-<uid>.json, overwriting the personal one, so the two could
// never sit in one pool. The file name must therefore carry the enterpriseId.
func TestPersonalAndEnterpriseAccountsGetDistinctFileNames(t *testing.T) {
	const uid = "00e26541-1884-4916-9c26-253a325d64ac"
	const entID = "entA1B2C3"

	personal := &storedAuth{Account: storedAccount{UID: uid, Nickname: "小明"}}
	enterprise := &storedAuth{Account: storedAccount{UID: uid, Nickname: "小明", EnterpriseID: entID}}

	personalName := authFileNameFor(personal)
	enterpriseName := authFileNameFor(enterprise)

	if personalName != "workbuddy-"+uid+".json" {
		t.Fatalf("personal file name = %q, want the unchanged workbuddy-<uid>.json", personalName)
	}
	if enterpriseName != "workbuddy-"+uid+"-"+entID+".json" {
		t.Fatalf("enterprise file name = %q, want workbuddy-<uid>-<enterpriseId>.json", enterpriseName)
	}
	if strings.EqualFold(personalName, enterpriseName) {
		t.Fatalf("personal and enterprise accounts of one person share the file name %q", personalName)
	}
	// Both names must pass the delete-path guard, or lifecycle could never
	// clean either of them up.
	for _, name := range []string{personalName, enterpriseName} {
		if !isSafeWorkbuddyAuthPath("/auth/dir/" + name) {
			t.Fatalf("%q must be a safe auth path", name)
		}
	}
}

// The enterpriseId is upstream-controlled input and reaches a file name, so it
// must be sanitized exactly like the uid.
func TestEnterpriseFileNamesSanitizeUpstreamIDs(t *testing.T) {
	sa := &storedAuth{Account: storedAccount{
		UID:          "uid-1",
		EnterpriseID: "../../etc/passwd",
	}}
	name := authFileNameFor(sa)
	if strings.Contains(name, "..") || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		t.Fatalf("enterprise file name kept traversal bytes: %q", name)
	}
	if !strings.HasPrefix(name, "workbuddy-uid-1-") || !strings.HasSuffix(name, ".json") {
		t.Fatalf("enterprise file name = %q", name)
	}
}

// An enterpriseId alone (no uid) cannot name a file: the bare legacy name is
// the only safe answer, exactly as before.
func TestAuthFileNameWithoutUIDStaysLegacy(t *testing.T) {
	sa := &storedAuth{Account: storedAccount{EnterpriseID: "ent-1"}}
	if got := authFileNameFor(sa); got != authFileName {
		t.Fatalf("authFileNameFor(no uid) = %q, want %q", got, authFileName)
	}
}

// Display names are what the panel and the host label show: the person, plus
// the team for an enterprise account. Without the team, a personal and a team
// account of one person would render identically.
func TestAccountNameDistinguishesPersonalFromEnterprise(t *testing.T) {
	const uid = "u-1"
	cases := []struct {
		name string
		who  accountData
		ent  enterpriseRef
		want string
	}{
		{
			name: "personal keeps the nickname alone",
			who:  accountData{UID: uid, Nickname: "小明"},
			want: "小明",
		},
		{
			name: "enterprise appends the team name",
			who:  accountData{UID: uid, Nickname: "小明"},
			ent:  enterpriseRef{ID: "ent-1", Name: "教育公益版"},
			want: "小明 · 教育公益版",
		},
		{
			name: "unnamed enterprise falls back to the id tail",
			who:  accountData{UID: uid, Nickname: "小明"},
			ent:  enterpriseRef{ID: "entA1B2C3"},
			want: "小明 · enterprise A1B2C3",
		},
		{
			name: "short enterprise id is used whole",
			who:  accountData{UID: uid, Nickname: "小明"},
			ent:  enterpriseRef{ID: "ent-1"},
			want: "小明 · enterprise ent-1",
		},
		{
			name: "missing nickname falls back to the uid",
			who:  accountData{UID: uid},
			want: uid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountName(tc.who, tc.ent); got != tc.want {
				t.Fatalf("accountName = %q, want %q", got, tc.want)
			}
		})
	}
	personal := accountName(accountData{UID: uid, Nickname: "小明"}, enterpriseRef{})
	enterprise := accountName(accountData{UID: uid, Nickname: "小明"}, enterpriseRef{ID: "ent-1", Name: "教育公益版"})
	if personal == enterprise {
		t.Fatalf("personal and enterprise accounts share the display name %q", personal)
	}
}

// A credential written before the enterprise suffix existed lives at
// workbuddy-<uid>.json while carrying an enterpriseId. Saving it must migrate
// to the canonical enterprise name and remove the old file — otherwise the
// upgrade keeps writing the legacy name and the next team login overwrites the
// personal account again.
func TestResolveAuthFileTargetMigratesLegacyUIDOnlyEnterpriseFile(t *testing.T) {
	const uid = "uid-1"
	sa := &storedAuth{Account: storedAccount{UID: uid, EnterpriseID: "ent-9"}}
	phys := &hostAuthPhysical{
		Name: "workbuddy-" + uid + ".json",
		Path: "/auth/dir/workbuddy-" + uid + ".json",
	}
	name, path, legacyPath := resolveAuthFileTarget(sa, phys)
	if want := "workbuddy-" + uid + "-ent-9.json"; name != want {
		t.Fatalf("target name = %q, want %q", name, want)
	}
	if path != phys.Path {
		t.Fatalf("target path = %q, want the host path %q", path, phys.Path)
	}
	if legacyPath != phys.Path {
		t.Fatalf("legacyPath = %q, want the uid-only file %q to be removed after save", legacyPath, phys.Path)
	}

	// A personal account on the same name is already canonical: no migration.
	personal := &storedAuth{Account: storedAccount{UID: uid}}
	name, _, legacyPath = resolveAuthFileTarget(personal, phys)
	if name != "workbuddy-"+uid+".json" || legacyPath != "" {
		t.Fatalf("personal account: name=%q legacyPath=%q, want no rename", name, legacyPath)
	}

	// An enterprise account already on its canonical name stays there.
	entPhys := &hostAuthPhysical{
		Name: "workbuddy-" + uid + "-ent-9.json",
		Path: "/auth/dir/workbuddy-" + uid + "-ent-9.json",
	}
	name, _, legacyPath = resolveAuthFileTarget(sa, entPhys)
	if name != entPhys.Name || legacyPath != "" {
		t.Fatalf("canonical enterprise account: name=%q legacyPath=%q, want no rename", name, legacyPath)
	}
}

// The migration deletes the file it moved away from. A uid-only check would
// delete the WRONG account here: one person's personal and team credentials
// share the uid, so migrating the team account must not remove the personal
// file (and the personal migration must not remove the team file).
func TestMigratedSourceDeleteRequiresSameEnterprise(t *testing.T) {
	personalRaw := []byte(`{"auth":{"accessToken":"a"},"account":{"uid":"u-1","nickname":"小明"}}`)
	teamRaw := []byte(`{"auth":{"accessToken":"b"},"account":{"uid":"u-1","nickname":"小明","enterpriseId":"ent-1"}}`)
	personal := &storedAuth{Account: storedAccount{UID: "u-1", Nickname: "小明"}}
	team := &storedAuth{Account: storedAccount{UID: "u-1", Nickname: "小明", EnterpriseID: "ent-1"}}

	if shouldDeleteMigratedSource(personalRaw, team) {
		t.Fatal("migrating the team account must not delete the personal file")
	}
	if shouldDeleteMigratedSource(teamRaw, personal) {
		t.Fatal("migrating the personal account must not delete the team file")
	}
	if !shouldDeleteMigratedSource(personalRaw, personal) {
		t.Fatal("the same personal credential must be removed after its migration")
	}
	if !shouldDeleteMigratedSource(teamRaw, team) {
		t.Fatal("the same team credential must be removed after its migration")
	}
	if shouldDeleteMigratedSource(personalRaw, &storedAuth{Account: storedAccount{UID: "u-2"}}) {
		t.Fatal("a different uid must never be deleted")
	}
}

// Both storage shapes must round-trip the new fields, or an enterprise name
// captured at login would vanish on the next read of an older flat file.
func TestParseStoredReadsEnterpriseFieldsInBothShapes(t *testing.T) {
	nested := []byte(`{"auth":{"accessToken":"at"},"account":{"uid":"u1","enterpriseId":"e1","enterpriseName":"Team","type":"enterprise"}}`)
	flat := []byte(`{"accessToken":"at","uid":"u1","enterpriseId":"e1","enterpriseName":"Team","type":"enterprise"}`)
	for name, raw := range map[string][]byte{"nested": nested, "flat": flat} {
		sa, err := parseStored(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if sa.Account.EnterpriseName != "Team" || sa.Account.Type != "enterprise" || sa.Account.EnterpriseID != "e1" {
			t.Fatalf("%s account = %#v", name, sa.Account)
		}
	}
}

// The plugin-side credential identity must carry the enterprise dimension, not
// just the file name: AuthData.ID and AuthData.FileName both feed the host's
// record key, and a uid-only ID would let one person's team login replace
// their personal account.
func TestCredentialIdentityCarriesEnterprise(t *testing.T) {
	const uid = "u-1"
	personal := &storedAuth{Account: storedAccount{UID: uid}}
	team := &storedAuth{Account: storedAccount{UID: uid, EnterpriseID: "ent-a"}}

	if got := credentialIDFor(personal); got != uid {
		t.Fatalf("personal credential id = %q, want the bare uid", got)
	}
	if got := credentialIDFor(team); got != uid+"-ent-a" {
		t.Fatalf("enterprise credential id = %q, want uid + enterpriseId", got)
	}
	if credentialIDFor(personal) == credentialIDFor(team) {
		t.Fatal("personal and enterprise credentials share an id")
	}
	// AuthData must agree with the file name it reports.
	for _, sa := range []*storedAuth{personal, team} {
		ad := toAuthData(sa)
		want := authFileNameFor(sa)
		if ad.FileName != want {
			t.Fatalf("AuthData.FileName = %q, want %q", ad.FileName, want)
		}
		if ad.ID != credentialIDFor(sa) {
			t.Fatalf("AuthData.ID = %q, want %q", ad.ID, credentialIDFor(sa))
		}
		if strings.TrimSuffix(ad.FileName, ".json") != "workbuddy-"+ad.ID {
			t.Fatalf("id %q and file name %q disagree", ad.ID, ad.FileName)
		}
	}
	// A credential without a uid keeps the bare legacy name and provider id.
	bare := toAuthData(&storedAuth{Account: storedAccount{}})
	if bare.FileName != authFileName || bare.ID != providerName {
		t.Fatalf("uid-less AuthData = %+v", bare)
	}
	if got := credentialIDFor(nil); got != "" {
		t.Fatalf("credentialIDFor(nil) = %q", got)
	}
}
