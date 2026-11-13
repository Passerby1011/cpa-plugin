// Package allowance reads an Open Design account's plan allowance from the Vela
// control plane and shapes it the way the vendor's own card shows it.
//
// Three reads, at the account's CONTROL key (vela_ctrl_…), never at the runtime
// key a model request carries:
//
//	GET /api/v1/workspaces/current                       the workspace and its tier
//	GET /api/v1/workspaces/{id}/billing/preflight         the coding plan and the wallet
//	GET /api/v1/billing/summary                           the subscription's period
//
// The reference implementation (magpie's opendesign package) is the authority
// for every rule here: the window naming, the percentage rounding, the credits
// shortening and the three resetsAt spellings are transcribed from it, because
// each was read off the vendor's own dashboard rather than invented.
//
// # What is a refusal and what is not
//
// A 401/403 from the preflight is the account's sign-in being refused: the auth
// row must be marked expired and the operator must sign in again. Every other
// failure — a 5xx, a transport blip, a body this build does not recognize — is
// "this reading did not work", which says nothing about the credential and must
// NOT mark it.
package allowance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sign-in states a card reports.
//
// They are the same three words this repository's commandcode plugin uses, so
// one panel vocabulary covers both plugins.
const (
	// SignInKept is the ordinary state: the credential still works.
	SignInKept = "kept"
	// SignInExpired means the control plane refused the account's sign-in.
	SignInExpired = "expired"
	// SignInRenewed is reported when a clean read takes a previous mark off.
	SignInRenewed = "renewed"
)

// MessageAPIKey is what an account with no control key reports.
//
// The control plane does not take an API key, so a pasted sk-amr-… key has no
// allowance to read at all. The wording is the reference implementation's: it
// names the reason instead of showing an empty card or a zero, both of which
// would read as "no allowance left".
const MessageAPIKey = "Signed in with an API key: the Design Plan allowance needs the account sign-in."

// MessageNoPlan is what an account whose plan is not eligible reports: its
// requests draw on the wallet balance instead.
const MessageNoPlan = "This account has no Design Plan allowance: its requests draw on the wallet balance."

// MessageRefused is what a refused sign-in reports.
const MessageRefused = "Open Design refused the account's sign-in — sign in again."

// userAgent identifies this plugin to the control plane.
const userAgent = "opencode-opendesign-auth/0.1.4"

// readTimeout bounds ONE allowance reading. It is deliberately independent of
// request-timeout: the panel is interactive, so a stalled control plane must fail
// fast instead of inheriting the model plane's five-minute budget.
const readTimeout = 30 * time.Second

// workspaceTTL is how long one control key's workspace is reused. The workspace
// does not move during a session, and re-reading it per card refresh is a whole
// extra round-trip per account.
const workspaceTTL = 10 * time.Minute

// Transport issues one HTTP call through the host's bridge. status is the
// upstream status; body the raw response; err a transport failure (status 0).
type Transport func(ctx context.Context, method, url string, headers map[string]string, body []byte) (status int, respBody []byte, err error)

// Window is one allowance window as the panel renders it.
type Window struct {
	// Name is the vendor's own name for the window ("5 hours", "7 days").
	Name string `json:"name"`
	// UsedPercent is the metered percentage, rounded to one decimal.
	UsedPercent float64 `json:"used_percent"`
	// DurationSeconds is the window's span; the list is sorted by it.
	DurationSeconds int64 `json:"duration_seconds"`
	// Display is "16.3k / 50k credits", as the vendor's card writes it.
	Display string `json:"display"`
	// ResetsAt is the reset instant in RFC3339, UTC; empty when the vendor
	// did not say.
	ResetsAt string `json:"resets_at,omitempty"`
}

// Snapshot is one account's allowance reading.
type Snapshot struct {
	Plan    string   `json:"plan,omitempty"`
	Tier    string   `json:"tier,omitempty"`
	Balance string   `json:"balance,omitempty"`
	Windows []Window `json:"windows"`
	Until   string   `json:"until,omitempty"`
	// Renew is "auto" while the subscription renews itself and "off" when it
	// cancels at the period's end.
	Renew  string `json:"renew,omitempty"`
	Status string `json:"subscription_status,omitempty"`
	// SignIn is kept / expired / renewed.
	SignIn string `json:"sign_in"`
	// Error is the operator-facing explanation, in the reference's words: it
	// names the remedy rather than the HTTP status.
	Error string `json:"error,omitempty"`
	// Refused reports the control plane refusing the sign-in. It is the signal
	// the caller uses to mark the account; it is not serialized.
	Refused bool `json:"-"`
	// Readable is false when there was nothing to read at all (an API key
	// sign-in), which is different from a reading that failed: the panel shows
	// a different message for each.
	Readable bool `json:"readable"`
}

// Workspace is the account's active workspace and its personal tier.
type Workspace struct {
	ID   string `json:"id"`
	Tier string `json:"tier,omitempty"`
}

// Request is one allowance reading's inputs.
type Request struct {
	// ControlURL and ControlKey are the control plane's; the key is the
	// vela_ctrl_… one, never the runtime key a model request carries.
	ControlURL string
	ControlKey string
	// WorkspaceID is what WorkspaceOf returned; empty means the workspace could
	// not be read.
	WorkspaceID string
	// Tier is the workspace's personal tier, used only when the summary does
	// not name a tier of its own.
	Tier string
}

// Number is a JSON value that may arrive as a number or as a string, which the
// control plane does both ways (balanceUsd is a string, usedCredits sometimes a
// number). An unparseable value keeps its text so the panel can show what the
// vendor sent instead of a confident zero.
type Number struct {
	value  float64
	set    bool
	text   string
	isText bool
}

// UnmarshalJSON accepts a number, a numeric string, or null.
func (n *Number) UnmarshalJSON(raw []byte) error {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return nil
		}
		n.text, n.isText = strings.TrimSpace(str), true
		if f, errFloat := strconv.ParseFloat(n.text, 64); errFloat == nil {
			n.value, n.set = f, true
		}
		return nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		n.value, n.set = f, true
	}
	return nil
}

// MarshalJSON renders the value back as a number when it parsed, as its text
// otherwise.
func (n Number) MarshalJSON() ([]byte, error) {
	if n.set {
		return []byte(strconv.FormatFloat(n.value, 'f', -1, 64)), nil
	}
	if n.isText {
		return json.Marshal(n.text)
	}
	return []byte("null"), nil
}

// Float returns the parsed value.
func (n Number) Float() (float64, bool) { return n.value, n.set }

// Text returns the raw text, when the vendor sent one.
func (n Number) Text() string { return n.text }

// Any returns the value in the shape the shaping helpers take.
func (n Number) Any() any {
	if n.set {
		return n.value
	}
	if n.isText {
		return n.text
	}
	return nil
}

// WindowInput is one window as the control plane writes it.
type WindowInput struct {
	UsedCredits     Number `json:"usedCredits"`
	LimitCredits    Number `json:"limitCredits"`
	DurationSeconds Number `json:"durationSeconds"`
	ResetsAt        Number `json:"resetsAt"`
}

// Plan is codingPlan from the preflight.
type Plan struct {
	// Eligible is a pointer because absent is not false: an account whose plan
	// block omits it must not be reported as having no allowance.
	Eligible *bool         `json:"eligible"`
	Tier     string        `json:"tier"`
	Windows  []WindowInput `json:"windows"`
}

// HasNoAllowance reports whether the vendor said this account has no Design
// Plan allowance at all.
func (p Plan) HasNoAllowance() bool { return p.Eligible != nil && !*p.Eligible }

// preflightBody is the preflight's answer, reduced to what the card shows.
type preflightBody struct {
	BalanceUSD Number `json:"balanceUsd"`
	CodingPlan Plan   `json:"codingPlan"`
}

// summaryBody is the billing summary's answer.
type summaryBody struct {
	MembershipTier                string `json:"membershipTier"`
	SubscriptionStatus            string `json:"subscriptionStatus"`
	SubscriptionCancelAtPeriodEnd bool   `json:"subscriptionCancelAtPeriodEnd"`
	SubscriptionCurrentPeriodEnd  string `json:"subscriptionCurrentPeriodEnd"`
	BalanceUSD                    Number `json:"balanceUsd"`
}

// ---------------------------------------------------------------------------
// the three reads
// ---------------------------------------------------------------------------

// getJSON performs one control-plane GET at the control key.
func getJSON(ctx context.Context, transport Transport, url, controlKey string) (int, []byte, error) {
	if transport == nil {
		return 0, nil, fmt.Errorf("no host transport available")
	}
	headers := map[string]string{
		"Accept":        "application/json",
		"User-Agent":    userAgent,
		"Authorization": "Bearer " + controlKey,
	}
	return transport(ctx, "GET", url, headers, nil)
}

// WorkspaceOf reads the account's active workspace and its personal tier.
//
// Both id spellings are accepted (the plane has answered activeWorkspaceId and
// workspaceId), and a body carrying neither is an ERROR rather than an empty
// workspace: without an id there is no preflight to read, and answering "" would
// build /workspaces//billing/preflight.
func WorkspaceOf(ctx context.Context, transport Transport, controlURL, controlKey string) (Workspace, error) {
	if strings.TrimSpace(controlKey) == "" {
		return Workspace{}, fmt.Errorf("the account carries no control key")
	}
	url := strings.TrimRight(strings.TrimSpace(controlURL), "/") + "/api/v1/workspaces/current"
	status, body, err := getJSON(ctx, transport, url, controlKey)
	if err != nil {
		// Never echo the transport error: it may quote the request.
		return Workspace{}, fmt.Errorf("the workspace read failed")
	}
	if status < 200 || status >= 300 {
		if status == 401 || status == 403 {
			// The same verdict a refused preflight gives: this control key
			// cannot read this account at all.
			return Workspace{}, &RefusedError{Where: "the workspaces read"}
		}
		return Workspace{}, fmt.Errorf("the workspaces read answered %d (%s)", status, said(body, "unauthorized"))
	}
	var doc struct {
		ActiveWorkspaceID      string `json:"activeWorkspaceId"`
		WorkspaceID            string `json:"workspaceId"`
		PersonalMembershipTier string `json:"personalMembershipTier"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return Workspace{}, fmt.Errorf("the workspaces read answered %d (undecodable body)", status)
	}
	id := strings.TrimSpace(doc.ActiveWorkspaceID)
	if id == "" {
		id = strings.TrimSpace(doc.WorkspaceID)
	}
	if id == "" {
		return Workspace{}, fmt.Errorf("the account has no active workspace")
	}
	return Workspace{ID: id, Tier: strings.TrimSpace(doc.PersonalMembershipTier)}, nil
}

// Read reads one workspace's allowance at the account's control key.
//
// A 401/403 from the preflight is reported as SignIn == SignInExpired with
// Refused set, never as an ordinary error: only that answer means the sign-in
// must be redone. Everything else keeps the sign-in as it was and carries the
// failure in Error, because a 5xx says nothing about the credential.
func Read(ctx context.Context, transport Transport, req Request) Snapshot {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(req.ControlKey) == "" {
		// No control key is not a failed read: there is nothing to read, and the
		// card must say WHY rather than show empties.
		return Snapshot{SignIn: SignInKept, Error: MessageAPIKey}
	}
	if strings.TrimSpace(req.WorkspaceID) == "" {
		return Snapshot{SignIn: SignInKept, Error: "the account has no active workspace", Readable: true}
	}
	sub, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	control := strings.TrimRight(strings.TrimSpace(req.ControlURL), "/")
	preflightURL := control + "/api/v1/workspaces/" + pathEscape(req.WorkspaceID) + "/billing/preflight"
	status, body, err := getJSON(sub, transport, preflightURL, req.ControlKey)
	if err != nil {
		return Snapshot{SignIn: SignInKept, Error: "Open Design usage: the control plane could not be reached", Readable: true}
	}
	switch status {
	case 401, 403:
		return Snapshot{SignIn: SignInExpired, Error: MessageRefused, Refused: true, Readable: true}
	}
	if status < 200 || status >= 300 {
		return Snapshot{
			SignIn:   SignInKept,
			Error:    fmt.Sprintf("the Design Plan read answered %d (%s)", status, said(body, "no reading")),
			Readable: true,
		}
	}
	var preflight preflightBody
	if err := json.Unmarshal(body, &preflight); err != nil {
		return Snapshot{SignIn: SignInKept, Error: "the Design Plan read answered an undecodable body", Readable: true}
	}

	// The summary is best-effort: the plan and the wallet already came from the
	// preflight, so a failed summary loses names and dates, not numbers.
	var summary summaryBody
	summaryOK := false
	statusSum, bodySum, errSum := getJSON(sub, transport, control+"/api/v1/billing/summary", req.ControlKey)
	if errSum == nil && statusSum >= 200 && statusSum < 300 {
		if json.Unmarshal(bodySum, &summary) == nil {
			summaryOK = true
		}
	}

	snap := Snapshot{
		Plan:     PlanName(summary.MembershipTier, req.Tier),
		Tier:     strings.TrimSpace(summary.MembershipTier),
		Balance:  Money(preflight.BalanceUSD.Any()),
		Windows:  Allowances(preflight.CodingPlan),
		SignIn:   SignInKept,
		Readable: true,
	}
	if snap.Tier == "" {
		snap.Tier = strings.TrimSpace(req.Tier)
	}
	if snap.Balance == "" && summaryOK {
		snap.Balance = Money(summary.BalanceUSD.Any())
	}
	if summaryOK {
		snap.Until = When(summary.SubscriptionCurrentPeriodEnd)
		if s := strings.TrimSpace(summary.SubscriptionStatus); s != "" {
			snap.Status = s
			snap.Renew = "auto"
			if summary.SubscriptionCancelAtPeriodEnd {
				snap.Renew = "off"
			}
		}
	}
	if preflight.CodingPlan.HasNoAllowance() {
		// Not a refusal and not an empty card: the account is fine, its requests
		// just do not draw on a Design Plan allowance.
		snap.Error = MessageNoPlan
	}
	return snap
}

// said is the vendor's own words from an error body, never a page of HTML and
// never longer than 300 bytes.
func said(body []byte, fallback string) string {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		trimmed := strings.TrimSpace(string(body))
		if trimmed == "" || strings.HasPrefix(trimmed, "<") {
			return fallback
		}
		return truncate(trimmed, 300)
	}
	for _, path := range [][]string{{"error", "message"}, {"message"}, {"error"}, {"detail"}} {
		if v := nestedString(doc, path...); v != "" && !strings.HasPrefix(v, "<") {
			return truncate(v, 300)
		}
	}
	return fallback
}

func nestedString(obj map[string]json.RawMessage, path ...string) string {
	cur := obj
	for i, key := range path {
		raw, ok := cur[key]
		if !ok {
			return ""
		}
		if i == len(path)-1 {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				return strings.TrimSpace(s)
			}
			return ""
		}
		var next map[string]json.RawMessage
		if json.Unmarshal(raw, &next) != nil {
			return ""
		}
		cur = next
	}
	return ""
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// pathEscape percent-encodes one path segment (a workspace id), so an id the
// vendor hands us cannot reshape the request path.
func pathEscape(segment string) string {
	var b strings.Builder
	for _, r := range segment {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '~':
			b.WriteRune(r)
		default:
			for _, by := range []byte(string(r)) {
				b.WriteString(fmt.Sprintf("%%%02X", by))
			}
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// the workspace cache
// ---------------------------------------------------------------------------

// WorkspaceCache reuses one control key's workspace for workspaceTTL, so a card
// refresh does not pay for a workspaces read it already made.
//
// It is keyed by the control key's DIGEST, never by the key: this map outlives
// one request, and nothing in it may be key material.
type WorkspaceCache struct {
	mu      sync.Mutex
	entries map[string]workspaceEntry
	ttl     time.Duration
	now     func() time.Time
}

type workspaceEntry struct {
	workspace Workspace
	at        time.Time
}

// NewWorkspaceCache returns a cache with the given TTL. A non-positive TTL
// disables reuse (every Get re-reads), which is what a test of the read path
// itself wants.
func NewWorkspaceCache(ttl time.Duration, now func() time.Time) *WorkspaceCache {
	if now == nil {
		now = time.Now
	}
	return &WorkspaceCache{entries: map[string]workspaceEntry{}, ttl: ttl, now: now}
}

// Get returns the control key's workspace, reading it when the cached copy is
// absent or older than the TTL.
//
// A FAILED read is never cached: caching a failure would keep an account
// unreadable for the whole TTL after one hiccup, and the operator would see a
// stale error they cannot clear.
func (c *WorkspaceCache) Get(ctx context.Context, transport Transport, controlURL, controlKey string) (Workspace, error) {
	if c == nil {
		return WorkspaceOf(ctx, transport, controlURL, controlKey)
	}
	key := digestOf(controlKey)
	if key == "" {
		return Workspace{}, fmt.Errorf("the account carries no control key")
	}
	if c.ttl > 0 {
		c.mu.Lock()
		entry, ok := c.entries[key]
		c.mu.Unlock()
		if ok && c.now().Sub(entry.at) < c.ttl {
			return entry.workspace, nil
		}
	}
	ws, err := WorkspaceOf(ctx, transport, controlURL, controlKey)
	if err != nil {
		return Workspace{}, err
	}
	if c.ttl > 0 {
		c.mu.Lock()
		c.entries[key] = workspaceEntry{workspace: ws, at: c.now()}
		c.mu.Unlock()
	}
	return ws, nil
}

// Forget drops one control key's cached workspace, so the next Get re-reads.
func (c *WorkspaceCache) Forget(controlKey string) {
	if c == nil {
		return
	}
	key := digestOf(controlKey)
	if key == "" {
		return
	}
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// Len reports how many workspaces are cached.
func (c *WorkspaceCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// DigestOfKey is the non-secret 12-hex-character digest of a key, which is what
// this package keys its cache by and what a caller may put in a response.
func DigestOfKey(key string) string { return digestOf(key) }

func digestOf(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

// HOUR and WEEK are the window units the vendor's names are derived from.
const (
	HOUR = 3600
	WEEK = 7 * 24 * HOUR
)

// Credits shortens a credit count the way the vendor's card rounds it: whole
// below five figures, thousands, then millions.
//
// The bands are exclusive and their boundaries matter: 9999 stays whole, 10000
// is 10k, above 100k the thousands lose their decimal (150000 is 150k), and
// 1000000 is the first million.
func Credits(v any) string {
	n, ok := numberValue(v)
	if !ok {
		// The vendor may have sent words instead of a number; show them.
		if s, isString := v.(string); isString {
			return strings.TrimSpace(s)
		}
		return ""
	}
	whole := math.Round(n)
	a := math.Abs(whole)
	switch {
	case a < 10000:
		return strconv.FormatFloat(whole, 'f', 0, 64)
	case a < 1000000:
		k := whole / 1000
		if math.Abs(k) >= 100 {
			return strconv.FormatFloat(math.Round(k), 'f', 0, 64) + "k"
		}
		return trimTrailingZero(strconv.FormatFloat(math.Round(k*10)/10, 'f', 1, 64)) + "k"
	default:
		return trimTrailingZero(strconv.FormatFloat(math.Round((whole/1000000)*10)/10, 'f', 1, 64)) + "M"
	}
}

// trimTrailingZero drops the ".0" a one-decimal rounding leaves on a whole
// number, because "10.0k" is not how the vendor's card writes it.
func trimTrailingZero(s string) string {
	return strings.TrimSuffix(s, ".0")
}

// UsedPercent meters one window, rounded to one decimal, clamped at zero. ok is
// false when the window cannot be metered at all — a limit of zero or less, or
// a value that is not a number — and such a window is dropped rather than drawn
// as empty.
func UsedPercent(used, limit any) (percent float64, ok bool) {
	u, okUsed := numberValue(used)
	l, okLimit := numberValue(limit)
	if !okUsed || !okLimit || l <= 0 {
		return 0, false
	}
	if math.IsNaN(u) || math.IsInf(u, 0) || math.IsNaN(l) || math.IsInf(l, 0) {
		return 0, false
	}
	return math.Round(math.Max(0, (u/l)*100)*10) / 10, true
}

// WindowName names a window the way the vendor's card does: hours under a day,
// days above, and the week's as "7 days".
func WindowName(seconds any) string {
	s, ok := numberValue(seconds)
	if !ok || s <= 0 {
		return "Allowance"
	}
	plural := func(n int64, one, many string) string {
		unit := many
		if n == 1 {
			unit = one
		}
		return strconv.FormatInt(n, 10) + " " + unit
	}
	switch {
	case int64(s)%WEEK == 0:
		// The reference names the week in DAYS ((s/WEEK)*7), not in weeks: the
		// vendor's card says "7 days".
		return plural((int64(s)/WEEK)*7, "day", "days")
	case int64(s)%(24*HOUR) == 0:
		return plural(int64(s)/(24*HOUR), "day", "days")
	case int64(s)%HOUR == 0:
		return plural(int64(s)/HOUR, "hour", "hours")
	default:
		return plural(int64(math.Round(s/60)), "minute", "minutes")
	}
}

// Allowances turns the plan's windows into the card's, shortest first.
//
// A window whose limit cannot meter it is DROPPED: drawing an empty bar would say
// "nothing consumed", which is the opposite of "this window has no allowance to
// speak of".
func Allowances(plan Plan) []Window {
	out := make([]Window, 0, len(plan.Windows))
	for _, w := range plan.Windows {
		used, ok := UsedPercent(w.UsedCredits.Any(), w.LimitCredits.Any())
		if !ok {
			continue
		}
		span, _ := w.DurationSeconds.Float()
		if span < 0 {
			span = 0
		}
		out = append(out, Window{
			Name:            WindowName(w.DurationSeconds.Any()),
			UsedPercent:     used,
			DurationSeconds: int64(span),
			Display:         Credits(w.UsedCredits.Any()) + " / " + Credits(w.LimitCredits.Any()) + " credits",
			ResetsAt:        When(w.ResetsAt.Any()),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].DurationSeconds < out[j].DurationSeconds })
	return out
}

// Money renders a balance as dollars, or empty when there is no number.
func Money(v any) string {
	n, ok := numberValue(v)
	if !ok {
		return ""
	}
	return "$" + strconv.FormatFloat(n, 'f', 2, 64)
}

// When renders a reset instant. The plane has been seen to send epoch seconds,
// epoch milliseconds and an RFC3339 string, and all three land on the same
// ISO-8601 UTC shape with milliseconds, as the dashboard writes them.
func When(v any) string {
	if s, isString := v.(string); isString && strings.TrimSpace(s) != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s)); err == nil {
			return parsed.UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
	f, ok := numberValue(v)
	if !ok {
		return ""
	}
	stamp := epochTime(f)
	if stamp.IsZero() {
		return ""
	}
	return stamp.UTC().Format("2006-01-02T15:04:05.000Z")
}

// epochTime reads an epoch value whose unit is guessed from its magnitude: a
// value below 1e11 is seconds, above it milliseconds. 1e11 SECONDS is the year
// 5138, so no real instant lands on the seconds side of that line.
func epochTime(f float64) time.Time {
	if f <= 0 {
		return time.Time{}
	}
	if f < 1e11 {
		return time.Unix(int64(f), 0)
	}
	return time.Unix(int64(f)/1000, (int64(f)%1000)*int64(time.Millisecond))
}

// PlanName names the subscription the way the dashboard does: "Design Plan Go",
// from the first non-empty tier, or "Open Design" when no tier is known.
func PlanName(tiers ...string) string {
	tier := ""
	for _, t := range tiers {
		if v := strings.TrimSpace(t); v != "" {
			tier = v
			break
		}
	}
	if tier == "" {
		return "Open Design"
	}
	first := strings.ToUpper(tier[:1])
	return "Design Plan " + first + tier[1:]
}

// RefusedError is the control plane refusing the account's control key (401/403
// on any of the three reads). It is separate from every other failure because
// only this one means the sign-in must be redone.
type RefusedError struct {
	// Where names the read that was refused, for the operator's benefit; it
	// never carries the key.
	Where string
}

func (e *RefusedError) Error() string { return MessageRefused }

// IsRefused reports whether an error is a refusal rather than a failure.
func IsRefused(err error) bool {
	var refused *RefusedError
	return errors.As(err, &refused)
}

// numberValue normalizes anything the control plane may hand us into a float.
func numberValue(v any) (float64, bool) {
	switch t := v.(type) {
	case Number:
		return t.Float()
	case *Number:
		if t == nil {
			return 0, false
		}
		return t.Float()
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}
