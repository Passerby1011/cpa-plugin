package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/config"
	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/resources"
)

// Account endpoints (CommandCode account surface). They live on the provider
// authority but outside the OpenAI-compatible surface: base-url points at
// <authority>/provider/v1 while the account API is <authority>/alpha/*.
// A provider API key authenticates them (Authorization: Bearer <key>), which
// is what lets one management-center card show one plan's remaining quota.
const (
	accountCreditsPath      = "/alpha/billing/credits"
	accountSubscriptionPath = "/alpha/billing/subscriptions"
	accountWhoamiPath       = "/alpha/whoami?limits=1"
	// quotaMaxTimeout bounds one account call: the management UI is
	// interactive, so a stalled upstream must fail fast rather than inherit
	// the (request-timeout, default 5m) upstream budget.
	quotaMaxTimeout = 30 * time.Second
)

// quotaWindow is one rate-limit window as rendered by the page.
type quotaWindow struct {
	Status   string  `json:"status"`
	Percent  int     `json:"percent"`
	Used     float64 `json:"used"`
	Cap      float64 `json:"cap"`
	ResetsAt string  `json:"resets_at"`
}

// quotaUsage is one credential's account snapshot.
type quotaUsage struct {
	Plan             string  `json:"plan,omitempty"`
	PlanCredits      float64 `json:"plan_credits,omitempty"`
	CreditsLeft      float64 `json:"credits_left"`
	PurchasedCredits float64 `json:"purchased_credits,omitempty"`
	// Month is the monthly plan allowance rendered as a window so the page can
	// meter it like the two rate-limit windows. ResetsAt is the subscription
	// currentPeriodEnd (the plan's renewal / expiry instant). The window is
	// absent when the plan is unknown, because CommandCode reports only the
	// REMAINING monthly credits and a bar without a denominator would be a lie.
	Month       *quotaWindow `json:"month,omitempty"`
	FiveHour    quotaWindow  `json:"five_hour"`
	Weekly      quotaWindow  `json:"weekly"`
	RefreshedAt string       `json:"refreshed_at"`
}

type quotaCard struct {
	KeyID string      `json:"key_id"`
	Label string      `json:"label"`
	Email string      `json:"email,omitempty"`
	Usage *quotaUsage `json:"usage,omitempty"`
	Error string      `json:"error,omitempty"`
}

type quotaRequest struct {
	KeyID string `json:"key_id"`
}

type quotaList struct {
	Cards []quotaCard `json:"cards"`
}

// ---- account API response shapes (probed live) --------------------------

type accountCredits struct {
	Credits struct {
		BelowThreshold   bool    `json:"belowThreshold"`
		CreditThreshold  float64 `json:"creditThreshold"`
		MonthlyCredits   float64 `json:"monthlyCredits"` // remaining plan credits
		PurchasedCredits float64 `json:"purchasedCredits"`
		FreeCredits      float64 `json:"freeCredits"`
	} `json:"credits"`
	// windowLimits.exceeded is deliberately not decoded: CommandCode reports it
	// as null while no window is over its cap, but as the NAME of the over-cap
	// window ("weekly") once one is, so a bool field made exactly the exhausted
	// accounts fail to decode and their whole snapshot was dropped. Each
	// window's own boolean exceeded already carries that information.
	WindowLimits struct {
		Limited  bool          `json:"limited"`
		FiveHour accountWindow `json:"fiveHour"`
		Weekly   accountWindow `json:"weekly"`
	} `json:"windowLimits"`
}

type accountWindow struct {
	Used     float64 `json:"used"`
	Cap      float64 `json:"cap"`
	Exceeded bool    `json:"exceeded"`
	ResetAt  int64   `json:"resetAt"` // Unix milliseconds
}

type accountSubscription struct {
	Success bool `json:"success"`
	Data    struct {
		PlanID           string `json:"planId"`
		Status           string `json:"status"`
		CurrentPeriodEnd string `json:"currentPeriodEnd"`
	} `json:"data"`
}

type accountWhoami struct {
	Success bool `json:"success"`
	User    struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		UserName string `json:"userName"`
	} `json:"user"`
	Org *struct {
		Login string `json:"login"`
	} `json:"org"`
}

// planAllowances is the vendor's own plan table (CommandCode CLI: planId →
// monthly credit allowance). It is display metadata only: an unknown plan
// simply shows the remaining credits without a denominator.
var planAllowances = map[string]float64{
	"individual-go":       10,
	"individual-goat":     70,
	"individual-pro":      30,
	"individual-pro-v1":   80,
	"individual-provider": 15,
	"individual-max":      150,
	"individual-ultra":    300,
	"teams-pro":           40,
}

var planNames = map[string]string{
	"individual-go":       "Go",
	"individual-goat":     "GOAT",
	"individual-pro":      "Pro",
	"individual-pro-v1":   "Pro",
	"individual-provider": "Provider",
	"individual-max":      "Max",
	"individual-ultra":    "Ultra",
	"teams-pro":           "Teams Pro",
}

// planFor resolves planId to its display name and allowance, preferring the
// longest matching prefix so "individual-goat" is not read as "individual-go".
func planFor(planID string) (string, float64) {
	id := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(planID, "_", "-")))
	if id == "" {
		return "", 0
	}
	if name, ok := planNames[id]; ok {
		return name, planAllowances[id]
	}
	best, bestLen := "", 0
	for key := range planNames {
		if len(key) > bestLen && strings.HasPrefix(id, key) {
			best, bestLen = key, len(key)
		}
	}
	if best == "" {
		return "", 0
	}
	return planNames[best], planAllowances[best]
}

func quotaIdentity(key string) (id, label string) {
	hash := authKeyHash(key)
	return authRecordIDFromHash(hash), "CommandCode credential " + hash[:12]
}

func (m *Manager) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if req.Method == http.MethodGet && req.Path == "/v0/resource/plugins/"+pluginName+"/quota" {
		return pluginapi.ManagementResponse{Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, Body: []byte(resources.QuotaPage)}, nil
	}
	if req.Method != http.MethodPost || req.Path != "/v0/management/plugins/"+pluginName+"/quota-usage" {
		return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound, Body: []byte(`{"error":"not found"}`)}, nil
	}
	var body quotaRequest
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return pluginapi.ManagementResponse{StatusCode: http.StatusBadRequest, Body: []byte(`{"error":"invalid request"}`)}, nil
		}
	}
	m.mu.RLock()
	baseURL, timeout := m.cfg.BaseURL, m.cfg.RequestTimeout
	keys := append([]config.APIKey(nil), m.cfg.APIKeys...)
	m.mu.RUnlock()
	if body.KeyID == "" {
		// List is deliberately cheap: no upstream account calls. The page
		// uses the email (already synced into the auth file by a prior
		// refresh) as the signal that a credential has been fetched once,
		// and only auto-refreshes cards that still lack one.
		emails := m.accountEmails(ctx)
		cards := make([]quotaCard, 0, len(keys))
		for _, key := range keys {
			id, label := quotaIdentity(key.Value)
			email := emails[id]
			if email != "" {
				label = email
			}
			cards = append(cards, quotaCard{KeyID: id, Label: label, Email: email})
		}
		return quotaJSON(quotaList{Cards: cards})
	}
	for _, key := range keys {
		id, label := quotaIdentity(key.Value)
		if id != body.KeyID {
			continue
		}
		usage, account, email, err := fetchQuota(ctx, m.bridge, baseURL, timeout, key.Value)
		if err != nil {
			// One unreachable or unsupported account must not blank the
			// whole page: the card carries the failure so the others still
			// render, and the credential hash stays as the label.
			return quotaJSON(quotaCard{KeyID: id, Label: label, Error: err.Error()})
		}
		if account != "" {
			label = account
		}
		// The account email is known only here, so this is where it is
		// carried back into the credential's own auth file, which is what
		// lets the host (and the panel's auth-file list) title the
		// credential with the mailbox. Best-effort by design: a failed
		// sync is a cosmetic loss and must never blank the quota card.
		if email != "" {
			if errSync := m.syncAccountEmail(ctx, key.Value, email); errSync != nil {
				_ = m.bridge.Log("warn", "commandcode credential email not synced", map[string]any{"reason": errSync.Error()})
			}
		}
		return quotaJSON(quotaCard{KeyID: id, Label: label, Email: email, Usage: &usage})
	}
	return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound, Body: []byte(`{"error":"unknown quota key"}`)}, nil
}

func quotaJSON(v any) (pluginapi.ManagementResponse, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return pluginapi.ManagementResponse{}, fmt.Errorf("quota response encoding failed")
	}
	return pluginapi.ManagementResponse{Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: body}, nil
}

// AccountAPIBase derives the account API root (<scheme>://<authority>) from
// the configured provider base-url by trimming the OpenAI-compatible surface
// path. CommandCode serves both surfaces on one authority, so the host is
// preserved (staging and self-hosted bases keep working) and no second config
// key is needed.
func AccountAPIBase(baseURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("account API base unavailable: base-url is not an absolute URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("account API base unavailable: unsupported scheme %q", u.Scheme)
	}
	path := strings.TrimSuffix(u.Path, "/")
	for _, suffix := range []string{"/provider/v1", "/provider"} {
		if strings.HasSuffix(path, suffix) {
			path = strings.TrimSuffix(path, suffix)
			break
		}
	}
	return u.Scheme + "://" + u.Host + path, nil
}

// fetchQuota reads one credential's account snapshot: remaining plan credits
// and the rolling windows come from /alpha/billing/credits, the plan identity
// from /alpha/billing/subscriptions, and the account identity from
// /alpha/whoami. Two identities are returned for the one whoami answer: label
// is what the quota card shows ("<org> (<email>)" when the account belongs to
// an org) and email is the bare mailbox, the only form written into an auth
// file. Both are best-effort — a key still shows its windows when only the
// credit call succeeds.
func fetchQuota(ctx context.Context, bridge *HostBridge, baseURL string, timeout time.Duration, key string) (quotaUsage, string, string, error) {
	if bridge == nil {
		return quotaUsage{}, "", "", fmt.Errorf("account bridge unavailable")
	}
	base, err := AccountAPIBase(baseURL)
	if err != nil {
		return quotaUsage{}, "", "", err
	}
	if timeout <= 0 || timeout > quotaMaxTimeout {
		timeout = quotaMaxTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	get := func(path string) ([]byte, error) {
		resp, errDo := bridge.Do(ctx, pluginapi.HTTPRequest{
			Method: http.MethodGet,
			URL:    base + path,
			Headers: http.Header{
				"Authorization": []string{"Bearer " + key},
				"Accept":        []string{"application/json"},
			},
		})
		if errDo != nil {
			return nil, fmt.Errorf("account request failed")
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("account request rejected (%d)", resp.StatusCode)
		}
		return resp.Body, nil
	}

	rawCredits, err := get(accountCreditsPath)
	if err != nil {
		return quotaUsage{}, "", "", err
	}
	var credits accountCredits
	if err := json.Unmarshal(rawCredits, &credits); err != nil {
		// Carry the decoder's own reason: it names the offending field, which
		// is the difference between a one-minute and a one-hour diagnosis.
		return quotaUsage{}, "", "", fmt.Errorf("account credits response invalid: %v", err)
	}
	usage := quotaUsage{
		CreditsLeft:      credits.Credits.MonthlyCredits,
		PurchasedCredits: credits.Credits.PurchasedCredits,
		FiveHour:         windowFromAccount(credits.WindowLimits.FiveHour, credits.WindowLimits.Limited),
		Weekly:           windowFromAccount(credits.WindowLimits.Weekly, credits.WindowLimits.Limited),
		RefreshedAt:      time.Now().UTC().Format(time.RFC3339),
	}

	label := ""
	email := ""
	periodEnd := ""
	if raw, errSub := get(accountSubscriptionPath); errSub == nil {
		var sub accountSubscription
		if json.Unmarshal(raw, &sub) == nil && sub.Success {
			periodEnd = sub.Data.CurrentPeriodEnd
			if name, allowance := planFor(sub.Data.PlanID); name != "" {
				usage.Plan = name
				usage.PlanCredits = allowance
			}
		}
	}
	usage.Month = monthWindow(usage.PlanCredits, usage.CreditsLeft, usage.PurchasedCredits)
	if usage.Month != nil {
		usage.Month.ResetsAt = periodEndRFC3339(periodEnd)
	}
	if raw, errWho := get(accountWhoamiPath); errWho == nil {
		var who accountWhoami
		if json.Unmarshal(raw, &who) == nil {
			email = strings.TrimSpace(who.User.Email)
			label = email
			if who.Org != nil && strings.TrimSpace(who.Org.Login) != "" {
				label = strings.TrimSpace(who.Org.Login) + " (" + who.User.Email + ")"
			}
		}
	}
	return usage, label, email, nil
}

// monthWindow renders the monthly plan allowance as a window. CommandCode's
// /alpha/billing/credits reports only the REMAINING monthly credits, so the
// pool and the consumed amount are derived exactly the way the vendor CLI
// derives its own usage bar: pool = max(allowance, remaining) + purchased,
// consumed = pool - (remaining + purchased). A plan that is not in the table
// has no allowance to divide by, so the window is omitted rather than reported
// as a suspicious 0%.
func monthWindow(planCredits, creditsLeft, purchased float64) *quotaWindow {
	if planCredits <= 0 {
		return nil
	}
	remaining := creditsLeft + purchased
	pool := math.Max(planCredits, creditsLeft) + purchased
	if pool <= 0 {
		return nil
	}
	used := pool - remaining
	if used < 0 {
		used = 0
	}
	out := quotaWindow{Used: used, Cap: pool, Status: "ok"}
	if remaining <= 0 {
		out.Status = "exceeded"
	}
	percent := int(used / pool * 100)
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	out.Percent = percent
	return &out
}

// periodEndRFC3339 turns CommandCode's subscription currentPeriodEnd into
// the same RFC3339 UTC the rate-limit windows use. The vendor sends an ISO
// timestamp (probed live as 2026-10-16T04:03:37.000Z); anything unparseable
// is dropped so the page does not invent a date.
func periodEndRFC3339(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

func windowFromAccount(w accountWindow, limited bool) quotaWindow {
	out := quotaWindow{Used: w.Used, Cap: w.Cap, Status: "ok"}
	switch {
	case w.Exceeded:
		out.Status = "exceeded"
	case !limited:
		out.Status = "unlimited"
	}
	if w.Cap > 0 {
		percent := int(w.Used / w.Cap * 100)
		if percent < 0 {
			percent = 0
		}
		if percent > 100 {
			percent = 100
		}
		out.Percent = percent
	}
	if w.ResetAt > 0 {
		out.ResetsAt = time.UnixMilli(w.ResetAt).UTC().Format(time.RFC3339)
	}
	return out
}
