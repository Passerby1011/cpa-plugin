// activity.go implements the growth-system "conversation activity" report.
//
// Background: the growth subsystem (streak days, the first_buddy adoption
// task, the cat-travel feature) only credits a day when the client posts a
// chat_request_send event to the billing endpoint. A gateway that merely
// proxies chat completions never sends those events, so accounts sit at
// streak.days == 0 and the adoption gate ("5 conversations") is never met —
// which is why the cat-travel reward cannot be claimed at all.
//
// Scope rules, all measured against the live gateway:
//
//   - CN personal accounts: supported.
//   - CN enterprise accounts: HTTP 403 "growth system is only available for
//     personal users". Never called.
//   - Global accounts: travel/buddy answer with empty payloads and streak
//     answers HTTP 500. Never called.
//
// The event body is the full client-shaped chat_request_send record rather than
// a minimal stub: the upstream silently drops events that omit userId, and a
// reduced field set is the kind of shape a later upstream hardening pass
// rejects. A 200 on the POST is not trusted on its own — see verifyActivityStreak.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// activityReportGap spaces the consecutive events of ONE account. Events fired
// in the same instant look like automation, and the upstream rate limits this
// endpoint. A var so tests can collapse the wait.
var activityReportGap = 1500 * time.Millisecond

// activityAccountDelay spaces accounts within one run. A var so tests can
// collapse the wait.
var activityAccountDelay = 800 * time.Millisecond

// activityEventPath is the growth-domain report endpoint (billing base).
const activityEventPath = "/v2/report"

// growthStreakPath is the read-only streak oracle.
const growthStreakPath = "/activity/growth/streak"

// chatRequestEvent mirrors the client's chat_request_send payload field for
// field, so the report has the same shape the real client produces.
type chatRequestEvent struct {
	EventCode             string `json:"eventCode"`
	Timestamp             int64  `json:"timestamp"`
	ReportDelay           int    `json:"reportDelay"`
	Mode                  string `json:"mode"`
	ConversationID        string `json:"conversationId"`
	RequestID             string `json:"requestId"`
	InputLength           int    `json:"inputLength"`
	RequestModelID        string `json:"requestModelId"`
	RequestModelName      string `json:"requestModelName"`
	IsPlan                bool   `json:"isPlan"`
	IsAutoExecuteTerminal bool   `json:"isAutoExecuteTerminal"`
	IsAutoModify          bool   `json:"isAutoModify"`
	CodebaseEnable        bool   `json:"codebaseEnable"`
	MaxToken              int    `json:"maxToken"`
	MaxSteps              int    `json:"maxSteps"`
	Temperature           int    `json:"temperature"`
	MaxRetries            int    `json:"maxRetries"`
	MentionContexts       []any  `json:"mentionContexts"`
	KnowledgeID           []any  `json:"knowledgeId"`
	KnowledgeName         []any  `json:"knowledgeName"`
	CodebaseID            string `json:"codebaseId"`
	MentionContextCount   int    `json:"mentionContextCount"`
	Command               string `json:"command"`
	ExpertID              string `json:"expertId"`
	RecommendID           string `json:"recommendId"`
	SkillID               string `json:"skillId"`
	SkillCount            int    `json:"skillCount"`
	TotalCount            int    `json:"totalCount"`
	FileURI               string `json:"fileUri"`
	PresentAt             int64  `json:"presentAt"`
	TraceID               string `json:"traceId"`
	RootRequestID         string `json:"rootRequestId"`
	ParentConversationID  string `json:"parentConversationId"`
	AgentName             string `json:"agentName"`
	AgentType             string `json:"agentType"`
	UserID                string `json:"userId"`
}

// activityEventModel is the model identifier the client reports inside its
// chat_request_send telemetry. It is a wire-protocol constant, not a model the
// plugin routes to (the plugin discovers its catalog dynamically). The contract
// guard in production_model_contract_test.go bans model IDs from production
// files to keep routing free of pinned models; this single telemetry literal is
// exempted there with the same rationale.
const (
	activityEventModelID   = "deepseek-v4-flash"
	activityEventModelName = "DeepSeek V4 Flash"

	// glmChatEventModelID is the model reported by the tasks that key off GLM
	// (Model_chat_GLM5.2 and the night-owl black_cat). Same rationale as
	// activityEventModelID above: a telemetry literal the growth system keys
	// its counters off, not a model this plugin routes to.
	glmChatEventModelID   = "glm-5.2"
	glmChatEventModelName = "GLM-5.2"
)

// eventModeCraft and eventModeNight are the telemetry modes the growth system
// distinguishes. The night-owl task only counts events reported in night mode
// inside its window (see nightOwlWindow).
const (
	eventModeCraft = "craft"
	eventModeNight = "night"
)

// newChatRequestEvent builds one event for the given account and conversation.
func newChatRequestEvent(uid, conversationID, requestID string, now time.Time) chatRequestEvent {
	ms := now.UnixMilli()
	return chatRequestEvent{
		EventCode:            "chat_request_send",
		Timestamp:            ms,
		Mode:                 eventModeCraft,
		ConversationID:       conversationID,
		RequestID:            requestID,
		InputLength:          12,
		RequestModelID:       activityEventModelID,
		RequestModelName:     activityEventModelName,
		MentionContexts:      []any{},
		KnowledgeID:          []any{},
		KnowledgeName:        []any{},
		PresentAt:            ms,
		RootRequestID:        conversationID,
		ParentConversationID: conversationID,
		AgentName:            "default",
		AgentType:            "conversation",
		UserID:               uid,
	}
}

// activityEligible reports whether the growth subsystem applies to this
// account. Enterprise and Global accounts are excluded on measured grounds
// (403 and 500 respectively), not by guesswork.
func activityEligible(sa *storedAuth) bool {
	if sa == nil || sa.Account.UID == "" || sa.Auth.AccessToken == "" {
		return false
	}
	if isEnterpriseAccount(sa) {
		return false
	}
	return !isGlobalDomain(sa.Auth.Domain)
}

// growthCall issues one growth-domain request against the billing base with the
// standard billing headers, through the host bridge so the call is logged.
func growthCall(sa *storedAuth, method, path string, body any) (json.RawMessage, error) {
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader([]byte("{}"))
	}
	req, err := http.NewRequest(method, billingBaseFor(sa)+path, reader)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		snippet := truncateRedacted(string(resp.Body), 120)
		return nil, fmt.Errorf("http %d from %s: %s", resp.StatusCode, path, snippet)
	}
	return json.RawMessage(resp.Body), nil
}

// reportChatActivity posts one event. Events of one conversation share a
// conversationId while each carries its own requestId, matching how a real
// multi-turn conversation reports.
func reportChatActivity(sa *storedAuth, conversationID, requestID string) error {
	event := newChatRequestEvent(sa.Account.UID, conversationID, requestID, time.Now())
	_, err := growthCall(sa, http.MethodPost, activityEventPath, []any{event})
	return err
}

// runActivityForAccount sends `count` events for one account, then verifies the
// streak actually moved.
func runActivityForAccount(sa *storedAuth, count int) error {
	if count <= 0 {
		count = 1
	}
	conversationID := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
	for i := 1; i <= count; i++ {
		requestID := fmt.Sprintf("%s-r%d", conversationID, i)
		if err := reportChatActivity(sa, conversationID, requestID); err != nil {
			return fmt.Errorf("report %d/%d: %w", i, count, err)
		}
		if i < count {
			time.Sleep(activityReportGap)
		}
	}
	return nil
}

// fetchGrowthStreakDays reads data.streak.days.
func fetchGrowthStreakDays(sa *storedAuth) (int, error) {
	raw, err := growthCall(sa, http.MethodGet, growthStreakPath, nil)
	if err != nil {
		return 0, err
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, err
	}
	var data struct {
		Streak struct {
			Days int `json:"days"`
		} `json:"streak"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return 0, err
	}
	return data.Streak.Days, nil
}

// activityRunResult summarizes one activity run.
type activityRunResult struct {
	Total    int `json:"total"`
	Reported int `json:"reported"`
	Failed   int `json:"failed"`
	Skipped  int `json:"skipped"`
}

// runAutoActivity is the scheduled tick. Global and enterprise accounts are
// skipped without any upstream call.
func runAutoActivity() activityRunResult {
	result := activityRunResult{}
	activityAutoMu.RLock()
	enabled := activityAuto
	count := activityReportCount
	activityAutoMu.RUnlock()
	if !enabled {
		return result
	}

	files, err := hostAuthList()
	if err != nil {
		return result
	}
	first := true
	for _, f := range files {
		result.Total++
		sa, _, err := hostAuthGetBundle(f.AuthIndex)
		if err != nil || sa == nil {
			result.Skipped++
			continue
		}
		if !activityEligible(sa) {
			result.Skipped++
			continue
		}
		if !first {
			time.Sleep(activityAccountDelay)
		}
		first = false
		if err := runActivityForAccount(sa, count); err != nil {
			result.Failed++
			hostLogf("warn", fmt.Sprintf("activity %s: %v", shortUID(sa.Account.UID), err))
			continue
		}
		result.Reported++
		verifyActivityStreak(sa)
		// The report is what lifts the first_buddy gate, so retry adoption now
		// rather than waiting for the next travel tick. force=true bypasses the
		// same-day debounce that an earlier refusal would have set.
		if _, err := runTravelForAccount(sa, time.Now(), true); err != nil {
			hostLogf("warn", fmt.Sprintf("activity %s: post-report travel: %v", shortUID(sa.Account.UID), err))
		}
	}
	hostLogf("info", fmt.Sprintf("activity done: total=%d reported=%d failed=%d skipped=%d",
		result.Total, result.Reported, result.Failed, result.Skipped))
	return result
}

// verifyActivityStreak re-reads the streak after a successful report. The
// report endpoint answers 200 even when it discards the event, so a streak that
// stays at zero is the only signal that the day did not count.
func verifyActivityStreak(sa *storedAuth) {
	days, err := fetchGrowthStreakDays(sa)
	if err != nil {
		hostLogf("warn", fmt.Sprintf("activity %s: streak check failed (report OK): %v",
			shortUID(sa.Account.UID), err))
		return
	}
	if days == 0 {
		hostLogf("warn", fmt.Sprintf("activity %s: report OK but streak.days=0 (silent drop?)",
			shortUID(sa.Account.UID)))
		return
	}
	hostLogf("info", fmt.Sprintf("activity %s: streak days=%d", shortUID(sa.Account.UID), days))
}

// runAutoTravel is the scheduled travel tick. It honours the travel_auto switch
// and, unlike the manual entry point, never forces past the same-day adoption
// debounce (only a fresh activity report justifies that).
func runAutoTravel() travelRunResult {
	travelAutoMu.RLock()
	enabled := travelAuto
	travelAutoMu.RUnlock()
	if !enabled {
		return travelRunResult{}
	}
	return runTravel(false)
}

// handleManualActivity backs POST /activity.
func handleManualActivity(_ pluginapi.ManagementRequest) map[string]any {
	result := runAutoActivity()
	return map[string]any{
		"success":  result.Failed == 0,
		"total":    result.Total,
		"reported": result.Reported,
		"failed":   result.Failed,
		"skipped":  result.Skipped,
	}
}

// hostLogf writes one line to the host log. main.log does not render the
// fields object, so any value worth reading belongs in the message itself.
func hostLogf(level, message string) {
	body, err := json.Marshal(map[string]any{
		"level":   level,
		"message": message,
		"fields":  map[string]any{"plugin": providerName},
	})
	if err != nil {
		return
	}
	_, _ = hostCall(pluginabi.MethodHostLog, body)
}

// shortUID renders the first 8 characters of an account UID for logs.
func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
