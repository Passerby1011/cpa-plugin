package plugin

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Sign-in states a card and a request can report.
//
// The distinction is the whole point of this file. A key Command Code turns
// away EVERYWHERE is a signed-out account: nothing can be done with it until
// somebody signs in again, so the panel must offer that. A 401 that only one
// endpoint gave - a model the plan lacks, a route that wants a different
// credential, a transient gateway hiccup - is NOT that, and marking it would
// send the operator to re-authenticate a perfectly good key.
const (
	// signInKept is the ordinary state: the credential still works.
	signInKept = "kept"
	// signInExpired means the key was refused by Command Code itself, not by
	// one endpoint's rules.
	signInExpired = "expired"
	// signInRenewed is reported when a clean read takes a previous mark off:
	// the account is usable again (the operator signed in again, or the
	// upstream stopped refusing it).
	signInRenewed = "renewed"
)

// signedOutMessage is what an expired credential reports, in words that name
// the remedy. It is deliberately not a bare "Unauthorized": the operator has to
// know that re-authenticating is the fix and that retrying is not.
const signedOutMessage = "Command Code's sign-in has expired; sign in again"

// whoamiPath is the bare identity endpoint used to CONFIRM a refusal. It is not
// accountWhoamiPath (which asks for "?limits=1" for the panel's detail): this
// one is asked only to answer "does Command Code still know this key at all",
// so it carries nothing the answer does not need.
const whoamiPath = "/alpha/whoami"

// whoamiConfirmTimeout bounds the confirming call. It sits on two paths that
// must stay fast - a request that is already failing, and an interactive panel
// refresh - so it is much shorter than the request budget.
const whoamiConfirmTimeout = 10 * time.Second

// revokedKeys records credentials Command Code refused EVERYWHERE, i.e. keys
// whose sign-in is gone. It is deliberately in-memory: a restart clears it,
// which errs toward retrying a key rather than silently refusing a good one
// forever, and it keeps the plugin free of any persistence the host would have
// to own.
//
// Guarded by its own mutex rather than Manager.mu because it is written from
// the request path (after a 401) and read from the panel path, and neither
// should wait for the other's host round-trips.
type revokedKeySet struct {
	mu   sync.Mutex
	keys map[string]bool
}

func newRevokedKeySet() *revokedKeySet {
	return &revokedKeySet{keys: make(map[string]bool)}
}

// mark records one credential as signed out.
func (s *revokedKeySet) mark(credential string) {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return
	}
	s.mu.Lock()
	s.keys[credential] = true
	s.mu.Unlock()
}

// clear removes the mark, returning whether one was there. A clean read calls
// this so a key that works again stops showing "sign in again".
func (s *revokedKeySet) clear(credential string) bool {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	had := s.keys[credential]
	delete(s.keys, credential)
	return had
}

// revoked reports whether the credential is currently marked signed out.
func (s *revokedKeySet) revoked(credential string) bool {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys[credential]
}

// keyRefused asks Command Code whether it still knows this key at all.
//
// WHAT IT ANSWERS: true only when the identity endpoint itself REFUSES the key
// (401/403). Everything else - a network failure, a 429, a 5xx, an unparseable
// answer - is "cannot confirm", reported as false. That asymmetry is the entire
// safety property: a false confirmation would mark a working credential as
// signed out and take it out of rotation, which is far worse than leaving a
// genuinely dead key marked for one more request.
//
// It is asked only AFTER some other call already returned 401, so a normal
// request never pays for it: the panel's 401 (usage read) or a request's 401
// is the trigger, never a healthy path.
func (m *Manager) keyRefused(ctx context.Context, baseURL, credential string) bool {
	if m == nil || m.bridge == nil || strings.TrimSpace(credential) == "" {
		return false
	}
	base, err := AccountAPIBase(baseURL)
	if err != nil {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, whoamiConfirmTimeout)
	defer cancel()
	resp, errDo := m.bridge.Do(cctx, pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    base + whoamiPath,
		Headers: http.Header{
			"Authorization": []string{"Bearer " + credential},
			"Accept":        []string{"application/json"},
		},
	})
	if errDo != nil {
		// Transport failure says nothing about the key's validity.
		return false
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		// Command Code's own "I do not know this key" answer.
		return true
	}
	// Including 429 and every 5xx: the upstream is having a problem, the key
	// is not the question.
	return false
}

// confirmRevocation asks whoami and marks (or clears) the credential accordingly.
// It is the one funnel both the panel and the request path use, so the two can
// never disagree about what counts as "signed out". Returns whether the key is
// confirmed refused.
func (m *Manager) confirmRevocation(ctx context.Context, baseURL, credential string) bool {
	if !m.keyRefused(ctx, baseURL, credential) {
		return false
	}
	m.revoked().mark(credential)
	return true
}

// revoked returns the manager's revoked-key set, creating it on first use.
// Lazy so a zero-value Manager (tests, and any caller that builds one directly)
// works without an explicit constructor step.
func (m *Manager) revoked() *revokedKeySet {
	m.revokedOnce.Do(func() {
		if m.revokedKeys == nil {
			m.revokedKeys = newRevokedKeySet()
		}
	})
	return m.revokedKeys
}

// accountIsRevoked reports whether a credential is currently marked signed out.
func (m *Manager) accountIsRevoked(credential string) bool {
	if m == nil {
		return false
	}
	return m.revoked().revoked(credential)
}

// clearRevokedMark drops the mark and reports whether one was present, which is
// what lets a clean read say "renewed" rather than "kept".
func (m *Manager) clearRevokedMark(credential string) bool {
	if m == nil {
		return false
	}
	return m.revoked().clear(credential)
}
