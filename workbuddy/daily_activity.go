// daily_activity.go implements the WorkBuddy Global (international) daily
// activity reward: the official "每日活跃奖励 30 积分/天" (50 on Pro) that
// workbuddy.ai pays for one finished agent conversation per day.
//
// Why a separate file from activity.go: activity.go drives the CN growth
// subsystem (streak / first_buddy / cat-travel) by POSTing a chat_request_send
// telemetry event to /v2/report. That path is CN-personal only — Global accounts
// answer HTTP 500 there, so activityEligible() excludes them. The international
// daily reward is a DIFFERENT mechanism, measured against the live service by
// the wb2api-hub project (issues #59 / #75 / #90), and implemented here:
//
//	POST {web}/console/as/conversations/        → queue an agent conversation
//	GET  {web}/console/as/conversations/{id}/session → sandbox link + token
//	ACP over streamable HTTP: initialize → session/load → session/prompt
//	poll GET {web}/console/as/conversations/{id} until status == completed
//
// The critical, non-obvious part is the ACP drive: creating the conversation
// only QUEUES it. Without connecting the sandbox and requesting the turn the
// conversation sits at CREATING forever, produces no output, and does NOT count
// as an effective conversation (measured, issue #90). A gateway that only POSTs
// the conversation therefore never earns the reward.
//
// The web channel deliberately carries only two credential headers —
// Authorization: Bearer and X-User-Id — and NO desktop X-IDE-* fingerprint,
// which is exactly what lets a gateway reuse the same account credential the
// desktop login already stored (measured: the desktop-identity conversation does
// not earn the reward; the web-channel one does).
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// webOrigin is the international web app origin. The console/agent API lives
// under it; the CN service has no equivalent of this reward. A var so tests can
// point it at a local server.
var webOrigin = "https://www.workbuddy.ai"

const (
	// webOriginGlobal is the production origin, kept as the constant the
	// referer/header builders read so a test override never leaks into a
	// header the service validates.
	webOriginGlobal = "https://www.workbuddy.ai"
	// webConversationsPath queues and reads agent conversations.
	webConversationsPath = "/console/as/conversations/"
	// dailyActivityModel is the cheap model the lightweight turn uses. Pinned so
	// the daily reward costs as few credits as possible.
	dailyActivityModel = "deepseek-v4.1-flash"
	// dailyActivityPrompt is the turn content. "Hi" is enough to register an
	// effective conversation; anything longer wastes credits.
	dailyActivityPrompt = "Hi"
	// dailyActivityUserAgent is the web app's UA (distinct from clientUA, the
	// desktop fingerprint UA). webHeadersFor is explicit that no X-IDE-* header
	// accompanies this channel.
	dailyActivityUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

	acpProtocolVersion = 1
)

// acpTurnTimeout bounds one finished turn. The reference waits up to 120s; the
// var (not const) lets tests collapse it.
var acpTurnTimeout = 120 * time.Second

// acpPollInterval is how often the conversation status is re-read while waiting
// for the turn to finish. A var so tests can collapse it.
var acpPollInterval = 3 * time.Second

// setWebBase temporarily overrides the web app origin for tests; returns a
// restore func.
func setWebBase(s string) func() {
	old := webOrigin
	webOrigin = s
	return func() { webOrigin = old }
}

// clearDailyActivityState drops the per-day de-duplication map (tests only).
func clearDailyActivityState() {
	dailyActivityLastMu.Lock()
	dailyActivityLast = map[string]string{}
	dailyActivityLastMu.Unlock()
}

// dailyActivityEligible reports whether the international daily reward applies
// to this account: Global realm, personal (not enterprise), with a usable
// credential. CN and enterprise accounts are handled by other paths and must not
// be sent here — the web console API is a Global feature.
func dailyActivityEligible(sa *storedAuth) bool {
	if sa == nil || sa.Account.UID == "" || sa.Auth.AccessToken == "" {
		return false
	}
	if isEnterpriseAccount(sa) {
		return false
	}
	return isGlobalDomain(sa.Auth.Domain)
}

// webHeadersFor builds the browser-app request headers. Only Authorization and
// X-User-Id authenticate the call; the remaining headers mirror the real web
// client so the request shape matches what the service expects. Notably absent:
// any X-IDE-* / X-Client-Platform desktop fingerprint.
func webHeadersFor(sa *storedAuth) map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + sa.Auth.AccessToken,
		"X-User-Id":     sa.Account.UID,
		"Content-Type":  "application/json",
		"Accept":        "application/json, text/plain, */*",
		"Origin":        webOriginGlobal,
		"Referer":       webOriginGlobal + "/app",
		"User-Agent":    dailyActivityUserAgent,
	}
}

// webConsoleCall issues one JSON request against the web console API. body nil
// means GET; otherwise POST. Responses are the standard {code,msg,data}
// envelope, which is decoded by the caller.
func webConsoleCall(sa *storedAuth, method, path string, body any) (map[string]any, error) {
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, webOrigin+path, reader)
	if err != nil {
		return nil, err
	}
	for k, v := range webHeadersFor(sa) {
		req.Header.Set(k, v)
	}
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		snippet := truncateRedacted(string(resp.Body), 160)
		return nil, fmt.Errorf("http %d from %s: %s", resp.StatusCode, path, snippet)
	}
	var payload map[string]any
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return payload, nil
}

// queueWebConversation POSTs a new agent conversation and returns its id. The
// body shape (prompt / model / conversationOrigin / plugins) is the one the web
// app sends; omitting conversationOrigin or plugins changes the request shape
// and is not what the live service expects.
func queueWebConversation(sa *storedAuth, prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		prompt = dailyActivityPrompt
	}
	body := map[string]any{
		"prompt":             prompt,
		"model":              dailyActivityModel,
		"conversationOrigin": "workbuddy-app",
		"plugins":            []any{map[string]any{"name": "weixinpay", "marketplace": "codebuddy-builtin"}},
	}
	payload, err := webConsoleCall(sa, http.MethodPost, webConversationsPath, body)
	if err != nil {
		return "", err
	}
	if code, ok := payload["code"]; ok && code != nil {
		switch v := code.(type) {
		case float64:
			if v != 0 {
				return "", fmt.Errorf("console code=%v msg=%s", v, stringify(payload["msg"]))
			}
		case int:
			if v != 0 {
				return "", fmt.Errorf("console code=%v msg=%s", v, stringify(payload["msg"]))
			}
		}
	}
	data, _ := payload["data"].(map[string]any)
	id := ""
	if data != nil {
		if s, ok := data["id"].(string); ok {
			id = s
		}
	}
	if id == "" {
		return "", fmt.Errorf("console response carried no conversation id (msg=%s)", stringify(payload["msg"]))
	}
	return id, nil
}

// webConversationSession reads the sandbox link + token for a queued
// conversation. Without these the turn cannot be driven and the conversation
// never leaves CREATING.
func webConversationSession(sa *storedAuth, conversationID string) (link, token, sessionID, cwd string, err error) {
	path := webConversationsPath + url.PathEscape(conversationID) + "/session"
	payload, err := webConsoleCall(sa, http.MethodGet, path, nil)
	if err != nil {
		return "", "", "", "", err
	}
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		return "", "", "", "", fmt.Errorf("session response carried no data")
	}
	link = firstStringField(data, "link", "endpoint")
	token = firstStringField(data, "token")
	sessionID = firstStringField(data, "sessionId", "session_id")
	cwd = firstStringField(data, "cwd")
	if sessionID == "" {
		sessionID = conversationID
	}
	if cwd == "" {
		cwd = "/workspace"
	}
	if link == "" || token == "" {
		return "", "", "", "", fmt.Errorf("sandbox not ready (link/token missing)")
	}
	return link, token, sessionID, cwd, nil
}

// webConversationStatus reads the conversation's current status. "completed"
// is the only terminal state that proves the turn ran and was counted.
func webConversationStatus(sa *storedAuth, conversationID string) string {
	path := webConversationsPath + url.PathEscape(conversationID)
	payload, err := webConsoleCall(sa, http.MethodGet, path, nil)
	if err != nil {
		return ""
	}
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		return ""
	}
	return strings.TrimSpace(stringify(data["status"]))
}

// stringify renders a decoded JSON value as a plain string for logs/messages.
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(raw)
	}
}

func firstStringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// ACP over streamable HTTP
// -----------------------------------------------------------------------------

// acpTurn drives one conversation turn to completion against the sandbox.
//
// The transport: GET the sandbox link with Accept: text/event-stream to open an
// SSE channel (the response carries Acp-Connection-Id), then POST JSON-RPC
// requests to the same URL with that connection id — initialize, session/load,
// session/prompt — while session/update notifications stream back. The turn is
// finished once the console reports the conversation completed.
//
// Only the minimum of the protocol is implemented: no tool calls, no terminal,
// no filesystem callbacks — the daily reward only needs the agent to answer.
func acpTurn(sa *storedAuth, link, token, sessionID, cwd, prompt string) (chunks, events int, err error) {
	parsed, perr := url.Parse(link)
	if perr != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return 0, 0, fmt.Errorf("sandbox link unusable: %q", link)
	}

	// 1. Open the SSE channel and capture Acp-Connection-Id.
	sseReq, err := http.NewRequest(http.MethodGet, link, nil)
	if err != nil {
		return 0, 0, err
	}
	sseReq.Header.Set("Accept", "text/event-stream")
	sseReq.Header.Set("Authorization", "Bearer "+token)
	sseReq.Header.Set("User-Agent", dailyActivityUserAgent)
	stream, status, headers, err := hostHTTPDoStream(sseReq)
	if err != nil {
		return 0, 0, fmt.Errorf("open ACP channel: %w", err)
	}
	if status != http.StatusOK {
		stream.Close()
		return 0, 0, fmt.Errorf("ACP channel returned HTTP %d", status)
	}
	connectionID := ""
	if headers != nil {
		connectionID = headers.Get("Acp-Connection-Id")
	}
	if connectionID == "" {
		stream.Close()
		return 0, 0, fmt.Errorf("ACP channel returned no Acp-Connection-Id")
	}

	// Drain SSE in the background, counting session/update notifications.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The host bridge emits arbitrary chunks (not SSE lines), so line
		// framing is re-assembled through hostStreamReader.
		reader := bufio.NewReader(newHostStreamReader(stream))
		for {
			select {
			case <-stop:
				return
			default:
			}
			line, rerr := reader.ReadBytes('\n')
			if len(line) > 0 {
				text := strings.TrimSpace(string(line))
				if strings.HasPrefix(text, "data:") {
					payload := strings.TrimSpace(text[len("data:"):])
					if payload != "" && payload != "[DONE]" {
						var msg map[string]any
						if json.Unmarshal([]byte(payload), &msg) == nil && msg["method"] == "session/update" {
							events++
							if params, ok := msg["params"].(map[string]any); ok {
								if upd, ok := params["update"].(map[string]any); ok && upd["sessionUpdate"] == "agent_message_chunk" {
									chunks++
								}
							}
						}
					}
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
	defer func() {
		close(stop)
		stream.Close()
		<-done
	}()

	// 2. Drive the turn: initialize → session/load → session/prompt.
	post := func(method string, params map[string]any, id int) error {
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": method, "params": params,
		})
		req, rerr := http.NewRequest(http.MethodPost, link, bytes.NewReader(body))
		if rerr != nil {
			return rerr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Acp-Connection-Id", connectionID)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("User-Agent", dailyActivityUserAgent)
		resp, rerr := hostHTTPDo(req)
		if rerr != nil {
			return rerr
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
			return fmt.Errorf("%s returned HTTP %d", method, resp.StatusCode)
		}
		return nil
	}

	if err := post("initialize", map[string]any{
		"protocolVersion": acpProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	}, 1); err != nil {
		return 0, 0, err
	}
	if err := post("session/load", map[string]any{
		"sessionId": sessionID, "cwd": cwd, "mcpServers": []any{},
	}, 2); err != nil {
		return 0, 0, err
	}
	if err := post("session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": prompt}},
	}, 3); err != nil {
		return 0, 0, err
	}

	// 3. Wait for the console to report the conversation completed.
	deadline := time.Now().Add(acpTurnTimeout)
	var lastStatus string
	for time.Now().Before(deadline) {
		lastStatus = webConversationStatus(sa, sessionID)
		switch lastStatus {
		case "completed":
			return chunks, events, nil
		case "failed", "error":
			return chunks, events, fmt.Errorf("conversation ended in state %q", lastStatus)
		}
		time.Sleep(acpPollInterval)
	}
	if lastStatus == "" {
		lastStatus = "unknown"
	}
	return chunks, events, fmt.Errorf("conversation did not complete within %s (status=%s)", acpTurnTimeout, lastStatus)
}

// runDailyActivityForAccount performs the whole reward sequence for one Global
// account: queue a web conversation, connect its sandbox, run one turn, and
// confirm completion. Returns a small result map for the panel/dashboard.
func runDailyActivityForAccount(sa *storedAuth) map[string]any {
	if !dailyActivityEligible(sa) {
		return map[string]any{"ok": false, "reason": "not a Global personal account"}
	}
	conversationID, err := queueWebConversation(sa, dailyActivityPrompt)
	if err != nil {
		return map[string]any{"ok": false, "error": "queue conversation: " + err.Error()}
	}
	link, token, sessionID, cwd, err := webConversationSession(sa, conversationID)
	if err != nil {
		return map[string]any{"ok": false, "conversation": conversationID, "error": "session: " + err.Error()}
	}
	chunks, events, err := acpTurn(sa, link, token, sessionID, cwd, dailyActivityPrompt)
	result := map[string]any{
		"ok":           err == nil,
		"conversation": conversationID,
		"chunks":       chunks,
		"events":       events,
	}
	if err != nil {
		result["error"] = err.Error()
		return result
	}
	result["status"] = "completed"
	return result
}

// -----------------------------------------------------------------------------
// Runner + per-day de-duplication
// -----------------------------------------------------------------------------

// dailyActivityLast records, per account UID, the local calendar date
// (YYYY-MM-DD) of the last successful reward, so each Global account is driven
// at most once per day. The in-memory map is authoritative for the running
// process; the auth file's metadata note is not used here because the reward
// must not be re-attempted after a confirmed success even across restarts
// within the same day — a re-run would spend credits for no extra reward.
var (
	dailyActivityLastMu sync.Mutex
	dailyActivityLast   = map[string]string{}
)

// dailyActivityDoneToday reports whether this UID already earned today's reward.
func dailyActivityDoneToday(uid string) bool {
	if uid == "" {
		return false
	}
	today := time.Now().Format("2006-01-02")
	dailyActivityLastMu.Lock()
	defer dailyActivityLastMu.Unlock()
	return dailyActivityLast[uid] == today
}

// rememberDailyActivity marks this UID as rewarded today.
func rememberDailyActivity(uid string) {
	if uid == "" {
		return
	}
	today := time.Now().Format("2006-01-02")
	dailyActivityLastMu.Lock()
	dailyActivityLast[uid] = today
	dailyActivityLastMu.Unlock()
}

// dailyActivityResult aggregates one sweep across all Global accounts.
type dailyActivityResult struct {
	Total    int `json:"total"`
	Claimed  int `json:"claimed"`
	Skipped  int `json:"skipped"`
	Failed   int `json:"failed"`
	Attempts int `json:"attempts"`
}

// dailyActivitySweepAllowed reports whether a sweep may run. force=true is the
// manual path (panel button / POST /daily-activity) and always passes; the
// scheduled path defers to the daily_activity_auto toggle, which is opt-in
// (default false) — a manual trigger must never be swallowed by it.
func dailyActivitySweepAllowed(force bool) (bool, bool) {
	if force {
		return true, true
	}
	dailyActivityAutoMu.RLock()
	enabled := dailyActivityAuto
	dailyActivityAutoMu.RUnlock()
	return enabled, false
}

// runAutoDailyActivity is the Global daily-activity sweep. CN and enterprise
// accounts are skipped here (they earn credits through the CN check-in / growth
// paths). Accounts already rewarded today are skipped.
//
// force=true is the manual path (the panel button, or POST /daily-activity):
// it runs regardless of the daily_activity_auto toggle, which only governs the
// scheduled sweep. Without this the button silently no-ops on a fresh install
// where the toggle is still off — the toggle is opt-in by default.
func runAutoDailyActivity(force bool) dailyActivityResult {
	result := dailyActivityResult{}
	if allowed, _ := dailyActivitySweepAllowed(force); !allowed {
		return result
	}

	files, err := hostAuthList()
	if err != nil {
		return result
	}
	first := true
	for _, f := range files {
		sa, _, err := hostAuthGetBundle(f.AuthIndex)
		if err != nil || sa == nil {
			result.Skipped++
			continue
		}
		if !dailyActivityEligible(sa) {
			result.Skipped++
			continue
		}
		if dailyActivityDoneToday(sa.Account.UID) {
			result.Skipped++
			continue
		}
		if !first {
			// Space the accounts: a burst of identically-timed conversations
			// looks like automation and risks the account.
			time.Sleep(activityAccountDelay)
		}
		first = false
		result.Attempts++
		res := runDailyActivityForAccount(sa)
		if ok, _ := res["ok"].(bool); ok {
			result.Claimed++
			rememberDailyActivity(sa.Account.UID)
			hostLogf("info", fmt.Sprintf("daily activity %s: completed (chunks=%v events=%v)",
				shortUID(sa.Account.UID), res["chunks"], res["events"]))
			continue
		}
		result.Failed++
		hostLogf("warn", fmt.Sprintf("daily activity %s: %v", shortUID(sa.Account.UID), res["error"]))
	}
	if result.Attempts > 0 {
		hostLogf("info", fmt.Sprintf("daily activity done: claimed=%d failed=%d skipped=%d",
			result.Claimed, result.Failed, result.Skipped))
	}
	return result
}

// handleManualDailyActivity backs POST /daily-activity. Manual invocation is
// always allowed — the caller asked for it explicitly, so the auto toggle does
// not gate it (mirrors handleManualTravel).
func handleManualDailyActivity(_ pluginapi.ManagementRequest) map[string]any {
	result := runAutoDailyActivity(true)
	return map[string]any{
		"success": result.Failed == 0,
		"total":   result.Total,
		"claimed": result.Claimed,
		"skipped": result.Skipped,
		"failed":  result.Failed,
	}
}

// handleDailyActivityConfig backs POST /daily-activity/config. Like the
// check-in toggle this is runtime-only: the CPA host exposes no plugin-config
// write callback, so the config_yaml value wins again on restart.
func handleDailyActivityConfig(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	_ = json.Unmarshal(req.Body, &body)
	dailyActivityAutoMu.Lock()
	if body.Enabled != nil {
		dailyActivityAuto = *body.Enabled
	}
	cur := dailyActivityAuto
	dailyActivityAutoMu.Unlock()
	return map[string]any{"daily_activity_auto": cur, "persistent": false}
}
