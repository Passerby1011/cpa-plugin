package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/commandcode/internal/config"
)

// The refusal CommandCode answered for an unknown key on every path
// (captured 2026-10-29), and the credits snapshot a live key returns.
const (
	refusedKeyBody = `{"success":false,"error":{"code":"UNAUTHORIZED","status":401,` +
		`"message":"Invalid 'Authorization' header or token."}}`
	liveCreditsBody = `{"credits":{"monthlyCredits":10,"purchasedCredits":0,"freeCredits":0}}`
)

// signedOutBridge answers bridged calls by URL suffix so one responder can play
// the whole account surface (credits, whoami) with independent statuses.
//
// whoami is keyed off its own path so a test can make the credits call fail
// while identity still works - which is the case that must NOT mark an
// account signed out.
func signedOutBridge(creditsStatus int, creditsBody string, whoamiStatus int, whoamiBody string) func(string, []byte) ([]byte, error) {
	return func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire map[string]any
		_ = json.Unmarshal(payload, &wire)
		url, _ := wire["url"].(string)
		path := url
		if index := strings.Index(url, "/alpha/"); index >= 0 {
			path = url[index:]
		}
		switch {
		case strings.HasPrefix(path, "/alpha/whoami"):
			return hostOK(pluginapi.HTTPResponse{StatusCode: whoamiStatus, Body: []byte(whoamiBody)}), nil
		case strings.HasPrefix(path, accountCreditsPath):
			return hostOK(pluginapi.HTTPResponse{StatusCode: creditsStatus, Body: []byte(creditsBody)}), nil
		case strings.HasPrefix(path, accountSubscriptionPath):
			return hostOK(pluginapi.HTTPResponse{StatusCode: whoamiStatus, Body: []byte(`{"success":true,"data":{"planId":"individual-pro"}}`)}), nil
		case strings.HasPrefix(path, accountUsagePath):
			return hostOK(pluginapi.HTTPResponse{StatusCode: whoamiStatus, Body: []byte(`{}`)}), nil
		}
		return hostErr("test", "unrouted "+url), nil
	}
}

// whoamiCalls counts how often the identity endpoint was asked, which is the
// assertion behind "a healthy path never pays for the check".
func whoamiCalls(f *fakeCaller) int {
	count := 0
	for _, call := range f.callsOf(pluginabi.MethodHostHTTPDo) {
		if strings.Contains(string(call.payload), whoamiPath) {
			count++
		}
	}
	return count
}

func quotaCardFor(t *testing.T, m *Manager, key string) quotaCard {
	t.Helper()
	id, _ := quotaIdentity(key)
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/" + pluginName + "/quota-usage",
		Body:   []byte(`{"key_id":"` + id + `"}`),
	})
	if err != nil {
		t.Fatalf("quota read: %v", err)
	}
	var card quotaCard
	if err := json.Unmarshal(resp.Body, &card); err != nil {
		t.Fatalf("decode card: %v (%s)", err, resp.Body)
	}
	return card
}

func newQuotaManager(t *testing.T, responder func(string, []byte) ([]byte, error), key string) (*Manager, *fakeCaller) {
	t.Helper()
	f := &fakeCaller{responder: responder}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{
		BaseURL:        "https://quota.test/provider/v1",
		RequestTimeout: config.DefaultRequestTimeout,
		APIKeys:        []config.APIKey{{Value: key}},
	}
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	return m, f
}

// TestQuotaSignedOutNeedsWhoamiRefusal pins the asymmetry that is the whole
// point of the check: a 401 alone is NOT a signed-out account, because a
// healthy key can be refused by one endpoint's rules. Only the identity
// endpoint refusing the key too may mark it.
func TestQuotaSignedOutNeedsWhoamiRefusal(t *testing.T) {
	const key = "quota-signed-out-secret"

	t.Run("401 and whoami refuses: expired, with the remedy in words", func(t *testing.T) {
		m, f := newQuotaManager(t, signedOutBridge(http.StatusUnauthorized, refusedKeyBody,
			http.StatusUnauthorized, refusedKeyBody), key)
		card := quotaCardFor(t, m, key)
		if card.SignIn != signInExpired {
			t.Fatalf("sign_in = %q, want %q", card.SignIn, signInExpired)
		}
		if card.Error != signedOutMessage {
			t.Fatalf("error = %q, want %q", card.Error, signedOutMessage)
		}
		if card.Usage != nil {
			t.Errorf("expired card carried usage: %+v", card.Usage)
		}
		if whoamiCalls(f) != 1 {
			t.Errorf("whoami asked %d times, want 1", whoamiCalls(f))
		}
		if strings.Contains(string(mustJSON(t, card)), key) {
			t.Error("card leaked the credential")
		}
	})

	t.Run("401 but whoami still knows the key: kept", func(t *testing.T) {
		m, f := newQuotaManager(t, signedOutBridge(http.StatusUnauthorized, refusedKeyBody,
			http.StatusOK, `{"user":{"email":"me@example.test"}}`), key)
		card := quotaCardFor(t, m, key)
		if card.SignIn != signInKept {
			t.Fatalf("sign_in = %q, want %q (whoami did not refuse the key)", card.SignIn, signInKept)
		}
		if card.Error == "" {
			t.Error("a failed read must still report its reason")
		}
		if card.Error == signedOutMessage {
			t.Error("the card told a live account to sign in again")
		}
		if whoamiCalls(f) != 1 {
			t.Errorf("whoami asked %d times, want 1", whoamiCalls(f))
		}
	})

	// A 429 or a 5xx says the upstream is having a problem; it says nothing
	// about the credential, so the identity endpoint must not even be asked.
	for _, status := range []int{
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
	} {
		t.Run("status "+http.StatusText(status)+" never marks", func(t *testing.T) {
			m, f := newQuotaManager(t, signedOutBridge(status, refusedKeyBody,
				http.StatusUnauthorized, refusedKeyBody), key)
			card := quotaCardFor(t, m, key)
			if card.SignIn != signInKept {
				t.Fatalf("sign_in = %q after %d, want %q", card.SignIn, status, signInKept)
			}
			if got := whoamiCalls(f); got != 0 {
				t.Errorf("whoami asked %d times after %d; only a 401 may spend that call", got, status)
			}
		})
	}

	// A transport failure (the bridge itself errors) is likewise not a verdict.
	t.Run("transport failure never marks", func(t *testing.T) {
		m, f := newQuotaManager(t, func(method string, _ []byte) ([]byte, error) {
			if method == pluginabi.MethodHostHTTPDo {
				return nil, context.DeadlineExceeded
			}
			return hostOK(map[string]any{}), nil
		}, key)
		card := quotaCardFor(t, m, key)
		if card.SignIn != signInKept {
			t.Fatalf("sign_in = %q after a transport failure, want %q", card.SignIn, signInKept)
		}
		if got := whoamiCalls(f); got != 0 {
			t.Errorf("whoami asked %d times after a transport failure", got)
		}
	})
}

// TestQuotaCleanReadClearsSignedOutMark pins the recovery direction: an account
// that works again must stop offering "sign in again", so the operator is not
// sent to re-authenticate a key that was fixed (or replaced).
func TestQuotaCleanReadClearsSignedOutMark(t *testing.T) {
	const key = "quota-renew-secret"
	refuse := true
	m, _ := newQuotaManager(t, func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		status, body := http.StatusUnauthorized, refusedKeyBody
		if !refuse {
			status, body = http.StatusOK, liveCreditsBody
		}
		var wire map[string]any
		_ = json.Unmarshal(payload, &wire)
		url, _ := wire["url"].(string)
		if strings.Contains(url, "/alpha/whoami") {
			return hostOK(pluginapi.HTTPResponse{StatusCode: status, Body: []byte(body)}), nil
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: status, Body: []byte(body)}), nil
	}, key)

	if card := quotaCardFor(t, m, key); card.SignIn != signInExpired {
		t.Fatalf("first read sign_in = %q, want %q", card.SignIn, signInExpired)
	}
	// The key works again: a clean read is proof, so the mark must go.
	refuse = false
	card := quotaCardFor(t, m, key)
	if card.SignIn != signInRenewed {
		t.Fatalf("clean read sign_in = %q, want %q", card.SignIn, signInRenewed)
	}
	if card.Usage == nil {
		t.Error("a clean read must carry usage")
	}
	// ... and the mark stays off on the next read.
	if card := quotaCardFor(t, m, key); card.SignIn != signInKept {
		t.Fatalf("third read sign_in = %q, want %q", card.SignIn, signInKept)
	}
}

// TestSignedOutMarkIsListedWithoutAnUpstreamRead pins that the mark survives
// the cheap list read: the page shows "sign in again" for a marked credential
// immediately, instead of looking healthy until somebody refreshes it.
func TestSignedOutMarkIsListedWithoutAnUpstreamRead(t *testing.T) {
	const key = "quota-listed-secret"
	m, f := newQuotaManager(t, signedOutBridge(http.StatusOK, liveCreditsBody, http.StatusOK, `{}`), key)
	m.revoked().mark(key)
	id, _ := quotaIdentity(key)
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/" + pluginName + "/quota-usage",
		Body:   []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var list quotaList
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Cards) != 1 || list.Cards[0].KeyID != id {
		t.Fatalf("cards = %+v", list.Cards)
	}
	if list.Cards[0].SignIn != signInExpired {
		t.Fatalf("list sign_in = %q, want %q", list.Cards[0].SignIn, signInExpired)
	}
	if len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 0 {
		t.Errorf("the list read called upstream: %v", f.callsOf(pluginabi.MethodHostHTTPDo))
	}
}

// TestKeyRefusedAnswersOnlyRefusal pins the confirming call's own contract in
// isolation: 401/403 refuse, everything else (including a 200 that says
// nothing) does not.
func TestKeyRefusedAnswersOnlyRefusal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		refused bool
	}{
		{"401", http.StatusUnauthorized, true},
		{"403", http.StatusForbidden, true},
		{"200", http.StatusOK, false},
		{"404", http.StatusNotFound, false},
		{"429", http.StatusTooManyRequests, false},
		{"500", http.StatusInternalServerError, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := newQuotaManager(t, signedOutBridge(http.StatusOK, liveCreditsBody,
				tc.status, `{}`), "whoami-secret")
			if got := m.keyRefused(context.Background(), "https://quota.test/provider/v1", "whoami-secret"); got != tc.refused {
				t.Fatalf("keyRefused on %d = %t, want %t", tc.status, got, tc.refused)
			}
			if whoamiCalls(f) != 1 {
				t.Fatalf("whoami asked %d times, want 1", whoamiCalls(f))
			}
		})
	}
	// A transport error is "cannot confirm", never "refused".
	m, _ := newQuotaManager(t, func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			return nil, context.DeadlineExceeded
		}
		return hostOK(map[string]any{}), nil
	}, "whoami-secret")
	if m.keyRefused(context.Background(), "https://quota.test/provider/v1", "whoami-secret") {
		t.Fatal("a transport failure was read as a refusal")
	}
	if m.keyRefused(context.Background(), "https://quota.test/provider/v1", "") {
		t.Fatal("an empty credential was read as refused")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
