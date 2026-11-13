package plugin

// Auth records.
//
// # Why the plugin writes auth records it does not authenticate with
//
// The host picks an executor by walking ITS OWN auth table for a record whose
// provider matches the requested model. A plugin provider with no auth record is
// never reached: every request fails with "auth_not_found: no auth available"
// before this executor runs. The model list and the panel still work, which
// makes the failure look like a routing bug.
//
// So one record per namespace exists purely so the host can schedule this
// provider. The record's JSON carries the BACKEND's API key (a local
// credential), never a Google credential.
//
// # Why one record per namespace
//
// The two backends have different keys, and a record carries one. Writing a
// single record would mean one namespace's requests travel with the other
// backend's key — a 401 at the backend. Each record's label names its namespace
// so an operator looking at CPA's auth list can tell them apart.
//
// # The honest caveat this code cannot fix
//
// Which Google account actually serves a request is decided by the BACKEND's
// account pool, not by which auth record CPA selected. N auth records are N
// scheduling slots; they are not N independently selectable Google accounts.
// The panel and the README say this in as many words, because an operator who
// believes otherwise will file bugs about "the wrong account answering".

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/geminiapi/internal/backend"
	"github.com/hex-ci/cpa-plugin/geminiapi/internal/config"
)

// authSaveTimeout bounds one host auth-save callback.
const authSaveTimeout = 10 * time.Second

// authKeyHash is the digest that names one record. Deriving the id from the
// namespace + credential makes re-registration idempotent: rotating a label
// cannot orphan the previous record.
func authKeyHash(namespace, secret string) string {
	digest := sha256.Sum256([]byte(namespace + "\n" + secret))
	return hex.EncodeToString(digest[:])
}

func authRecordID(namespace, hash string) string {
	return ProviderID + "-" + namespace + "-key-" + hash
}

func authFileName(namespace, hash string) string { return authRecordID(namespace, hash) + ".json" }

// materializeAuth ensures one auth record per namespace exists.
//
// For an EXTERNAL backend the record carries the operator's own key (that is
// what the plugin sends upstream). For a MANAGED backend the key is the one the
// backend generated into its data directory. Both are local to this deployment,
// and neither is a Google credential.
func (m *Manager) materializeAuth(ctx context.Context) error {
	if m.bridge == nil {
		return nil
	}
	m.mu.RLock()
	cfg := m.cfg
	externals := m.externals
	backends := m.backends
	m.mu.RUnlock()

	var lastErr error
	for _, ns := range cfg.Namespaces {
		secret, baseURL := "", ""
		nsCfg, cfgErr := cfg.Namespace(ns)
		if cfgErr != nil {
			lastErr = cfgErr
			continue
		}
		if externals[ns] {
			secret = nsCfg.APIKey
			baseURL = nsCfg.BaseURL
		} else {
			mgr := backends[ns]
			if mgr == nil {
				continue
			}
			flavor, flavorErr := backend.FlavorFor(backend.FlavorID(ns))
			if flavorErr != nil {
				lastErr = flavorErr
				continue
			}
			setup, ok := flavor.ReadSetup(mgr.DataDir())
			if !ok || setup.APIKey == "" {
				// Nothing installed yet: the backend has no key until its data
				// files exist. Registration must still succeed so the panel can
				// drive an install, so this is not an error.
				continue
			}
			secret = setup.APIKey
			baseURL = setup.BaseURL
		}
		if strings.TrimSpace(secret) == "" {
			continue
		}
		if err := m.saveAuthRecord(ctx, ns, secret, baseURL); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// saveAuthRecord writes one namespace's auth record if it does not exist.
func (m *Manager) saveAuthRecord(ctx context.Context, namespace, secret, baseURL string) error {
	hash := authKeyHash(namespace, secret)
	recordID := authRecordID(namespace, hash)
	name := authFileName(namespace, hash)

	entries, err := m.bridge.AuthList(ctx)
	if err == nil {
		for _, entry := range entries {
			if strings.TrimSpace(entry.ID) == recordID || strings.TrimSpace(entry.Name) == name {
				return nil // already materialized; leave host metadata alone
			}
		}
	}

	// The label carries the namespace and the digest prefix, never the secret:
	// an auth record is visible in the Management Center and in host logs.
	label := ProviderID + " " + namespace + " backend key (" + hash[:12] + ")"
	payload, err := json.Marshal(struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Label   string `json:"label"`
		APIKey  string `json:"api_key"`
		BaseURL string `json:"base_url,omitempty"`
	}{Type: ProviderID, ID: recordID, Label: label, APIKey: secret, BaseURL: baseURL})
	if err != nil {
		return fmt.Errorf("build auth record")
	}
	if err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: name, JSON: payload}); err != nil {
		return fmt.Errorf("save auth record: %w", err)
	}
	return nil
}

// AuthSave persists an auth record through the host.
func (b *HostBridge) AuthSave(ctx context.Context, req pluginapi.HostAuthSaveRequest) error {
	if b == nil || b.call == nil {
		return fmt.Errorf("host transport is unavailable")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode auth save: %w", err)
	}
	raw, err := b.callWithTimeout(ctx, pluginabi.MethodHostAuthSave, payload, authSaveTimeout)
	if err != nil {
		return err
	}
	env, err := decodeEnvelope(raw)
	if err != nil {
		return err
	}
	if !env.OK {
		// Host error text is not trusted: it can echo the record's JSON.
		return fmt.Errorf("host auth save failed")
	}
	return nil
}

// AuthList lists auth records without their secrets.
func (b *HostBridge) AuthList(ctx context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	if b == nil || b.call == nil {
		return nil, fmt.Errorf("host transport is unavailable")
	}
	payload, _ := json.Marshal(struct{}{})
	raw, err := b.callWithTimeout(ctx, pluginabi.MethodHostAuthList, payload, authSaveTimeout)
	if err != nil {
		return nil, err
	}
	env, err := decodeEnvelope(raw)
	if err != nil {
		return nil, err
	}
	if !env.OK {
		return nil, fmt.Errorf("host auth list failed")
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, &resp); err != nil {
			return nil, fmt.Errorf("host auth list failed: undecodable response")
		}
	}
	return resp.Files, nil
}

// ensureHTTPStatus turns an error class into an HTTP status for management
// responses.
func ensureHTTPStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "not configured"), strings.Contains(text, "required"):
		return http.StatusBadRequest
	case strings.Contains(text, "refus"), strings.Contains(text, "not ours"):
		return http.StatusConflict
	case strings.Contains(text, "timed out"), strings.Contains(text, "timeout"):
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}

// namespaceOfModel reports the namespace prefix of a published model id, or ""
// when it has none.
func namespaceOfModel(model string) string {
	ns, _, ok := backendNamespaceSplit(model)
	if !ok {
		return ""
	}
	return ns
}

// backendNamespaceSplit is catalog.ResolveNamespace without importing catalog
// here (this file only needs the prefix).
func backendNamespaceSplit(model string) (string, string, bool) {
	model = strings.TrimSpace(model)
	for _, ns := range []string{config.NamespaceGeminiWeb, config.NamespaceAIStudio} {
		if strings.HasPrefix(model, ns+"/") {
			return ns, strings.TrimPrefix(model, ns+"/"), true
		}
	}
	return "", "", false
}
