// Package plugin — the plan allowance the panel shows.
//
// The reading itself lives in internal/allowance; this file is the plugin side
// of it: which accounts can be read at all, which the control plane has refused,
// and the two Management API routes the panel calls.
//
// # Why a route and not a host card
//
// CPA's plugin API has no provider-level quota surface, so an allowance card
// cannot be published to the host. The shape this repository already uses for
// that (the commandcode plugin) is the one taken here: the plugin's own
// management routes answer the numbers and its own embedded panel renders them.
//
// # Two calls, on purpose
//
//	GET  /allowance   the account list, with NO upstream call at all
//	POST /allowance   one account's live reading, which is what a card's own
//	                  refresh button asks for
//
// The list is cheap because a panel that read every account on load would turn
// one page open into N control-plane requests, and a control plane that is down
// would make the whole page hang instead of one card.
package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/opendesign/internal/allowance"
	"github.com/hex-ci/cpa-plugin/opendesign/internal/config"
)

// allowanceWorkspaceTTL is how long one account's control-plane workspace is
// reused. The workspace does not move during a session, and re-reading it for
// every card refresh would add a round-trip per account per click.
const allowanceWorkspaceTTL = 10 * time.Minute

// allowanceTimeout bounds the whole refresh route, which is independent of the
// model plane's request-timeout for the same reason the reading's own timeout is:
// the panel is interactive.
const allowanceTimeout = 40 * time.Second

// accountHasControlKey reports whether this account's allowance can be read at
// all.
//
// The PREFIX is what decides it, not merely a non-empty field: a runtime key pasted
// into the control slot would otherwise be sent to the control plane, which does
// not take one, and the failure would read as "the account is broken" rather than
// "this credential has no plan".
func accountHasControlKey(a config.Account) bool {
	return strings.HasPrefix(strings.TrimSpace(a.ControlKey), "vela_ctrl_")
}

// allowanceCard is one account as the panel renders it.
type allowanceCard struct {
	// ID is the runtime key's DIGEST: it identifies the card without ever
	// carrying the key, and it is what a refresh posts back.
	ID    string `json:"id"`
	Label string `json:"label"`
	Email string `json:"email,omitempty"`
	// PlanReadable is false for a pasted API key: no control key, nothing to
	// read. The panel states that rather than drawing an empty card.
	PlanReadable bool `json:"plan_readable"`
	// SignIn is kept / expired / renewed.
	SignIn string `json:"sign_in"`
	// Message is the operator-facing reason when there is nothing (yet) to
	// show: an API key sign-in, or a plan the account is not eligible for.
	Message string `json:"message,omitempty"`
	// Usage is the live reading, present only on a refresh answer.
	Usage *allowance.Snapshot `json:"usage,omitempty"`
}

// allowanceList is the GET answer.
type allowanceList struct {
	Cards []allowanceCard `json:"cards"`
	// Accounts is the total, so the panel can say "3 accounts" without
	// counting cards it received.
	Accounts int `json:"accounts"`
	// WithPlan is how many of them have a readable allowance.
	WithPlan int `json:"with_plan"`
}

// allowanceRequest is the POST body: one card's id.
type allowanceRequest struct {
	// ID is the digest the list handed out — never a key.
	ID string `json:"id"`
}

// allowanceRead is what the POST answers.
type allowanceRead struct {
	Card allowanceCard `json:"card"`
}

// expiredAllowanceKeys records accounts whose CONTROL-PLANE sign-in the
// allowance read was refused for. It is keyed by the runtime key's digest, and
// it is deliberately in-memory: a restart clears it, which errs toward retrying
// rather than refusing a working account forever.
//
// Its own mutex rather than Manager.mu because it is written behind an upstream
// round-trip and read from the panel path; neither should wait for the other.
type expiredAllowanceKeys struct {
	mu   sync.Mutex
	keys map[string]bool
}

func newExpiredAllowanceKeys() *expiredAllowanceKeys {
	return &expiredAllowanceKeys{keys: map[string]bool{}}
}

// mark records one account's sign-in as refused.
func (s *expiredAllowanceKeys) mark(id string) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	s.keys[id] = true
	s.mu.Unlock()
}

// clear removes the mark, returning whether one was there: a clean read is
// proof the sign-in works again, and the row must stop offering "sign in again".
func (s *expiredAllowanceKeys) clear(id string) bool {
	if s == nil || id == "" {
		return false
	}
	s.mu.Lock()
	_, had := s.keys[id]
	delete(s.keys, id)
	s.mu.Unlock()
	return had
}

// has reports whether the account is marked.
func (s *expiredAllowanceKeys) has(id string) bool {
	if s == nil || id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys[id]
}

// ---------------------------------------------------------------------------
// routes
// ---------------------------------------------------------------------------

// allowanceCards lists the accounts without touching the network.
//
// The nothing-to-read case is stated HERE, not left to the refresh: a pasted API
// key's card would otherwise render as an empty allowance with unknown meaning.
func (m *Manager) allowanceCards(cfg config.Config) allowanceList {
	accounts := m.accountsOf(cfg)
	out := allowanceList{Cards: make([]allowanceCard, 0, len(accounts)), Accounts: len(accounts)}
	for _, a := range accounts {
		id := accountKey(a.RuntimeKey)
		if id == "" {
			continue
		}
		card := allowanceCard{
			ID:           id,
			Label:        a.Label,
			Email:        a.Email,
			PlanReadable: accountHasControlKey(a),
			SignIn:       allowance.SignInKept,
		}
		if !card.PlanReadable {
			card.Message = allowance.MessageAPIKey
		} else {
			out.WithPlan++
			if m.expiredAllowances.has(id) {
				card.SignIn = allowance.SignInExpired
				card.Message = allowance.MessageRefused
			}
		}
		out.Cards = append(out.Cards, card)
	}
	return out
}

// refreshAllowance reads ONE account's allowance.
//
// A refusal marks the account and is reported as expired; every other failure
// leaves the mark alone and carries the message, because re-authenticating a
// healthy account is the mistake this split exists to avoid.
func (m *Manager) refreshAllowance(ctx context.Context, cfg config.Config, id string) (allowanceCard, int) {
	accounts := m.accountsOf(cfg)
	for _, a := range accounts {
		if accountKey(a.RuntimeKey) != id {
			continue
		}
		card := allowanceCard{ID: id, Label: a.Label, Email: a.Email, PlanReadable: accountHasControlKey(a)}
		if !card.PlanReadable {
			// No control key: nothing to read, and nothing to refresh. The
			// message says why instead of leaving the card blank.
			card.SignIn = allowance.SignInKept
			card.Message = allowance.MessageAPIKey
			return card, http.StatusOK
		}
		controlURL := strings.TrimSpace(cfg.ControlURL)
		if controlURL == "" {
			controlURL = config.DefaultControlURL
		}
		ws, err := m.workspaces.Get(ctx, m.bridge.Transport(), controlURL, a.ControlKey)
		if err != nil {
			if allowance.IsRefused(err) {
				// The control key itself was refused. Marking is right here
				// even though the preflight was never reached: the same key
				// would be refused there too, and leaving the row "kept" would
				// hide a dead sign-in behind a retryable-looking error.
				m.expiredAllowances.mark(id)
				m.workspaces.Forget(a.ControlKey)
				card.SignIn = allowance.SignInExpired
				card.Message = allowance.MessageRefused
				return card, http.StatusOK
			}
			// Any other workspace failure is not a refusal: the control plane
			// may be down, and marking would send the operator to redo a
			// working sign-in.
			card.SignIn = allowance.SignInKept
			card.Message = "Open Design usage: " + err.Error()
			if m.expiredAllowances.has(id) {
				card.SignIn = allowance.SignInExpired
			}
			return card, http.StatusOK
		}
		snap := allowance.Read(ctx, m.bridge.Transport(), allowance.Request{
			ControlURL:  controlURL,
			ControlKey:  a.ControlKey,
			WorkspaceID: ws.ID,
			Tier:        ws.Tier,
		})
		card.SignIn = snap.SignIn
		card.Message = snap.Error
		if snap.Refused {
			m.expiredAllowances.mark(id)
			// The cached workspace is dropped: the next sign-in may land in a
			// different one, and keeping it would read the wrong account's plan.
			m.workspaces.Forget(a.ControlKey)
		} else if snap.Readable && m.expiredAllowances.clear(id) {
			// A clean read is proof the sign-in works again, so the row stops
			// offering "sign in again" — and the panel is told that happened.
			card.SignIn = allowance.SignInRenewed
		}
		card.Usage = &snap
		return card, http.StatusOK
	}
	return allowanceCard{}, http.StatusNotFound
}

// handleAllowanceRoute answers the two allowance routes.
//
// body is the posted JSON; only the refresh route reads it.
func (m *Manager) handleAllowanceRoute(ctx context.Context, req pluginapi.ManagementRequest) managementResponseBody {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()

	if strings.EqualFold(req.Method, http.MethodGet) {
		return jsonResponse(m.allowanceCards(cfg))
	}
	if !strings.EqualFold(req.Method, http.MethodPost) {
		return methodNotAllowed()
	}
	var body allowanceRequest
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return jsonResponseWithStatus(http.StatusBadRequest, map[string]string{"error": "invalid request"})
		}
	}
	id := strings.TrimSpace(body.ID)
	if id == "" {
		// An empty id means "the list", which the GET route serves; answering
		// every account's live reading from a POST nobody asked for would make
		// one button press into N upstream requests.
		return jsonResponseWithStatus(http.StatusBadRequest, map[string]string{
			"error": "an account id is required; the list is the GET route",
		})
	}
	if m.bridge == nil {
		return jsonResponseWithStatus(http.StatusServiceUnavailable, map[string]string{
			"error": "the plugin has no host transport",
		})
	}
	readCtx, cancel := context.WithTimeout(ctx, allowanceTimeout)
	defer cancel()
	card, status := m.refreshAllowance(readCtx, cfg, id)
	if status != http.StatusOK {
		return jsonResponseWithStatus(status, map[string]string{"error": "unknown account"})
	}
	return jsonResponse(allowanceRead{Card: card})
}

// jsonResponse renders one payload as a JSON answer.
func jsonResponse(v any) managementResponseBody {
	return jsonResponseWithStatus(http.StatusOK, v)
}

// jsonResponseWithStatus renders one payload with an explicit status.
func jsonResponseWithStatus(status int, v any) managementResponseBody {
	body, err := json.Marshal(v)
	if err != nil {
		return managementResponseBody{
			StatusCode: http.StatusInternalServerError,
			Headers:    map[string][]string{"Content-Type": {"application/json"}},
			Body:       []byte(`{"error":"response encoding failed"}`),
		}
	}
	return managementResponseBody{
		StatusCode: status,
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       body,
	}
}
