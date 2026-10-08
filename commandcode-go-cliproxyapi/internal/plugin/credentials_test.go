package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/config"
	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/resources"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// authStore is an in-memory stand-in for CPA's auth store: it answers
// host.auth.list / get / save the way the host does, including the crucial
// detail that list omits the secret while get returns it.
type authStore struct {
	mu    sync.Mutex
	order []string
	types map[string]string
	data  map[string][]byte
}

func newAuthStore() *authStore {
	return &authStore{types: map[string]string{}, data: map[string][]byte{}}
}

func (s *authStore) save(name, providerType string, record []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.data[name]; !seen {
		s.order = append(s.order, name)
	}
	s.types[name] = providerType
	s.data[name] = append([]byte(nil), record...)
}

func (s *authStore) call(method string, payload []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch method {
	case pluginabi.MethodHostAuthList:
		files := make([]pluginapi.HostAuthFileEntry, 0, len(s.order))
		for _, name := range s.order {
			// AuthList deliberately carries no credential.
			files = append(files, pluginapi.HostAuthFileEntry{
				ID: name, AuthIndex: name, Name: name, Type: s.types[name],
			})
		}
		return hostOK(hostAuthListResponse{Files: files}), nil
	case pluginabi.MethodHostAuthGet:
		var req pluginapi.HostAuthGetRequest
		_ = json.Unmarshal(payload, &req)
		raw, ok := s.data[req.AuthIndex]
		if !ok {
			return hostOK(pluginapi.HostAuthGetResponse{AuthIndex: req.AuthIndex}), nil
		}
		return hostOK(pluginapi.HostAuthGetResponse{AuthIndex: req.AuthIndex, JSON: raw}), nil
	case pluginabi.MethodHostAuthSave:
		var req pluginapi.HostAuthSaveRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return hostErr("bad_request", "undecodable"), nil
		}
		var rec authRecordCredential
		_ = json.Unmarshal(req.JSON, &rec)
		if _, seen := s.data[req.Name]; !seen {
			s.order = append(s.order, req.Name)
		}
		s.types[req.Name] = rec.Type
		s.data[req.Name] = append([]byte(nil), req.JSON...)
		return hostOK(map[string]any{}), nil
	default:
		return hostOK(map[string]any{}), nil
	}
}

// TestRefreshAuthCredentialsReadsRecordsThePluginOwns pins the credential
// source that a key added from the quota page lands in - and that a record
// belonging to another provider is ignored, since the auth store is shared.
func TestRefreshAuthCredentialsReadsRecordsThePluginOwns(t *testing.T) {
	store := newAuthStore()
	mine, _ := json.Marshal(authRecordCredential{
		Type: ProviderID, ID: "commandcode-key-aaa", Label: "mine", APIKey: "user_mine", Mode: "go-cli",
	})
	foreign, _ := json.Marshal(map[string]string{"type": "workbuddy", "api_key": "wb_secret"})
	store.save("commandcode-key-aaa.json", ProviderID, mine)
	store.save("workbuddy.json", "workbuddy", foreign)

	m := NewManager(NewHostBridge(store.call))
	if err := m.refreshAuthCredentials(context.Background(), m.bridge); err != nil {
		t.Fatalf("refreshAuthCredentials: %v", err)
	}
	got := m.poolAccounts(config.Config{})
	if len(got) != 1 {
		t.Fatalf("pool = %+v, want exactly the plugin-owned record", got)
	}
	if got[0].Credential != "user_mine" || got[0].Mode != config.TransportGoCLI {
		t.Fatalf("pool entry = %+v", got[0])
	}
	if got[0].Label != "mine" {
		t.Errorf("label = %q, want mine", got[0].Label)
	}
}

// TestAddAccountStoresTheKeyWithoutEchoingIt pins the page's add flow: the key
// is persisted, becomes usable immediately, and never appears in the response.
func TestAddAccountStoresTheKeyWithoutEchoingIt(t *testing.T) {
	store := newAuthStore()
	m := NewManager(NewHostBridge(store.call))

	const secret = "user_THISMUSTNOTLEAK"
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management" + accountsPath,
		Body:   []byte(`{"credential":"` + secret + `","label":"工作号","mode":"go-cli"}`),
	})
	if err != nil {
		t.Fatalf("HandleManagement: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, resp.Body)
	}
	if strings.Contains(string(resp.Body), secret) {
		t.Fatal("the response echoed the credential")
	}
	// Stored under the digest-derived name, carrying the declared mode.
	name := authFileNameFromHash(authKeyHash(secret))
	if _, ok := store.data[name]; !ok {
		t.Fatalf("credential not stored; have %v", store.order)
	}
	// And usable at once - the pool grew without a reconfigure.
	pool := m.poolAccounts(config.Config{})
	if len(pool) != 1 || pool[0].Credential != secret {
		t.Fatalf("pool = %+v, want the added credential", pool)
	}
	if pool[0].Mode != config.TransportGoCLI {
		t.Errorf("mode = %q, want go-cli (the form default for a Go key)", pool[0].Mode)
	}
}

// TestAddAccountRejectsBadInput pins the guard rails: a blank credential and an
// unknown mode are refused before anything reaches the host.
func TestAddAccountRejectsBadInput(t *testing.T) {
	store := newAuthStore()
	m := NewManager(NewHostBridge(store.call))
	path := "/v0/management" + accountsPath

	for _, tc := range []struct{ name, body string }{
		{"blank credential", `{"credential":"   ","mode":"go-cli"}`},
		{"unknown mode", `{"credential":"user_x","mode":"carrier-pigeon"}`},
	} {
		resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
			Method: http.MethodPost, Path: path, Body: []byte(tc.body),
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.name, resp.StatusCode)
		}
	}
	if len(store.order) != 0 {
		t.Fatalf("a rejected request still wrote to the host: %v", store.order)
	}
}

// TestPoolAccountsIsAUnionAndDeduplicates pins that declaring a key in BOTH the
// config and an auth record offers it once, and that config accounts keep
// working exactly as before.
func TestPoolAccountsIsAUnionAndDeduplicates(t *testing.T) {
	store := newAuthStore()
	shared := "user_shared"
	record, _ := json.Marshal(authRecordCredential{Type: ProviderID, APIKey: shared, Mode: "go-cli"})
	store.save(authFileNameFromHash(authKeyHash(shared)), ProviderID, record)
	only, _ := json.Marshal(authRecordCredential{Type: ProviderID, APIKey: "user_only_auth", Mode: "go-cli"})
	store.save(authFileNameFromHash(authKeyHash("user_only_auth")), ProviderID, only)

	m := NewManager(NewHostBridge(store.call))
	if err := m.refreshAuthCredentials(context.Background(), m.bridge); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	cfg := config.Config{Accounts: []config.Account{
		{Label: "from-config", Mode: config.TransportGoCLI, Credential: shared},
		{Label: "config-only", Mode: config.TransportGoCLI, Credential: "user_only_config"},
	}}
	pool := m.poolAccounts(cfg)
	seen := map[string]string{}
	for _, a := range pool {
		if _, dup := seen[a.Credential]; dup {
			t.Fatalf("credential %q appeared twice in the pool", a.Credential)
		}
		seen[a.Credential] = a.Label
	}
	if len(pool) != 3 {
		t.Fatalf("pool size = %d (%v), want 3", len(pool), seen)
	}
	if seen[shared] != "from-config" {
		t.Errorf("the config declaration must win for a shared credential, got %q", seen[shared])
	}
	if _, ok := seen["user_only_auth"]; !ok {
		t.Error("an auth-record-only credential was dropped")
	}
	if _, ok := seen["user_only_config"]; !ok {
		t.Error("a config-only credential was dropped")
	}
}

// TestQuotaPageOffersAddKey pins the form the page renders, and that it posts
// to the accounts route.
func TestQuotaPageOffersAddKey(t *testing.T) {
	page := resources.QuotaPage
	for _, marker := range []string{
		`id="addForm"`,
		`id="addKey"`,
		`id="addMode"`,
		`id="addSubmit"`,
		"accountsEndpoint",
		"/v0/management/plugins/commandcode-go-cliproxyapi/accounts",
		`type="password"`,
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("quota page is missing the add-key form marker %q", marker)
		}
	}
}

// TestLifecycleFetchesCatalogWhenTheOnlyCredentialIsAnAuthRecord pins the
// regression that made an added key useless on an otherwise empty config.
//
// The lifecycle used to key its "do we have a credential?" decision on
// cfg.Pending, which is derived from the CONFIG alone. A deployment whose keys
// were all added from the quota page keeps zero config accounts, so Pending
// stayed true and the catalog was never fetched: the page added a working
// credential and no models ever appeared.
func TestLifecycleFetchesCatalogWhenTheOnlyCredentialIsAnAuthRecord(t *testing.T) {
	store := newAuthStore()
	const only = "user_auth_record_only"
	record, _ := json.Marshal(authRecordCredential{Type: ProviderID, APIKey: only, Mode: "go-cli"})
	store.save(authFileNameFromHash(authKeyHash(only)), ProviderID, record)

	sawCatalogFetch := false
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList, pluginabi.MethodHostAuthGet, pluginabi.MethodHostAuthSave:
			return store.call(method, payload)
		case pluginabi.MethodHostHTTPDo:
			var wire struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(payload, &wire)
			switch {
			case strings.HasSuffix(wire.URL, "/models"):
				sawCatalogFetch = true
				return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(testCatalogJSON)}), nil
			case strings.HasSuffix(wire.URL, accountSubscriptionPath):
				return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK,
					Body: []byte(`{"success":true,"data":{"planId":"individual-go-v1"}}`)}), nil
			case strings.HasSuffix(wire.URL, accountCreditsPath):
				return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK,
					Body: []byte(`{"credits":{"monthlyCredits":10}}`)}), nil
			}
		}
		return hostOK(map[string]any{}), nil
	}}

	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	// An EMPTY config: the only credential is the auth record.
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody("")); err != nil {
		t.Fatalf("register: %v", err)
	}
	if !sawCatalogFetch {
		t.Fatal("a pool whose only credential is an auth record must still fetch the catalog")
	}
	var published pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", []byte("{}")), &published)
	if len(published.Models) == 0 {
		t.Fatal("no models published although an auth-record credential can fetch them")
	}
}
