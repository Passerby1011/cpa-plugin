// growth_tasks.go implements the growth task centre: the list / accept / claim
// cycle, plus the event reports that complete the tasks this gateway can
// complete by itself.
//
// Why this exists: the growth system pays credits for a catalogue of tasks (a
// first conversation, five conversations, a night-owl chat, ...). A gateway
// that only proxies chat completions never accepts any of them, so the credits
// sit unclaimed for the life of the account. The endpoints, the reward shape
// and the idempotency semantics below were measured against the live service
// rather than inferred from the reference implementation.
//
// Scope is deliberately narrow. Only tasks that complete through a
// chat_request_send telemetry event are listed, because that event is exactly
// what this plugin already posts for the streak (see activity.go). A task that
// needs a desktop or web fingerprint event chain, a marketplace lookup or a
// real-world side effect is left unaccepted: accepting it would advertise
// progress the gateway can never deliver.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	// growthTasksListPath lists every task with its accept status and progress.
	// Both www.codebuddy.cn and copilot.tencent.com serve it (measured), so the
	// billing base is the right host and no extra base constant is needed.
	growthTasksListPath = "/v2/activity/growth/tasks"
	// growthTasksAcceptPath accepts tasks by code. The body is the plural
	// {"task_codes": [...]} form; a single-object body is rejected.
	growthTasksAcceptPath = "/v2/activity/growth/tasks/accept"
	// The claim endpoint carries the task code in the path and takes no body.
	// The older .../tasks/reward/claim form is NOT authoritative: for an
	// already-claimed task it answers "task not completed" instead of
	// already_claimed (measured), so a non-200 there says nothing about the
	// claim state.
	growthTasksTaskPrefix  = "/v2/activity/growth/tasks/"
	growthTasksClaimSuffix = "/claim"
)

// taskReportGap spaces the events of one task. The report endpoint rate limits,
// and events fired in the same instant look like automation. A var so tests can
// collapse the wait.
var taskReportGap = 1500 * time.Millisecond

// taskCenterDelay separates the list / accept / claim round trips so a re-read
// observes the previous write. A var so tests can collapse it.
var taskCenterDelay = 1200 * time.Millisecond

// growthTask is one entry of data.tasks. progress is null until the task is
// accepted, which is why the cycle accepts first and only then reads progress.
type growthTask struct {
	TaskCode     string `json:"task_code"`
	Title        string `json:"title"`
	RewardCredit int    `json:"reward_credit"`
	AcceptStatus string `json:"accept_status"`
	Progress     *struct {
		Current int `json:"current"`
		Target  int `json:"target"`
	} `json:"progress"`
}

// complete reports whether the task has met its target. A nil progress or a
// zero target is not complete: an accepted-but-untouched task carries
// {"current":0,"target":N} and must not be claimed early.
func (t growthTask) complete() bool {
	return t.Progress != nil && t.Progress.Target > 0 && t.Progress.Current >= t.Progress.Target
}

// taskReportPlan describes how this gateway completes one task on its own.
type taskReportPlan struct {
	// modelID is the model reported inside the telemetry event. The growth
	// system keys some tasks off the reported model, so it must match.
	modelID   string
	modelName string
	count     int
	// separateConversations gives every event its own conversationId. A task
	// phrased as "N conversations" counts distinct conversations, so reusing a
	// single id would under-count.
	separateConversations bool
	// nightOnly restricts the report to the night-owl window.
	nightOnly bool
}

// taskReportPlans maps a task code to the events that complete it. Every entry
// uses chat_request_send, the event this plugin already understands.
var taskReportPlans = map[string]taskReportPlan{
	"chat_5": {
		modelID: activityEventModelID, modelName: activityEventModelName,
		count: 5, separateConversations: true,
	},
	"Model_chat_GLM5.2": {
		modelID: glmChatEventModelID, modelName: glmChatEventModelName,
		count: 1,
	},
	"black_cat": {
		modelID: glmChatEventModelID, modelName: glmChatEventModelName,
		count: 3, separateConversations: true, nightOnly: true,
	},
}

// nightOwlWindow reports whether now falls inside the night-owl window
// (23:00-08:00 China Standard Time) that black_cat counts in. The gateway may
// run in any time zone, so the window is evaluated in CST rather than in the
// host's local zone — a host on UTC would otherwise never be inside it.
func nightOwlWindow(now time.Time) bool {
	cst := now.In(time.FixedZone("CST", 8*60*60))
	return cst.Hour() >= 23 || cst.Hour() < 8
}

// listGrowthTasks reads the task catalogue.
func listGrowthTasks(sa *storedAuth) ([]growthTask, error) {
	raw, err := growthCall(sa, http.MethodGet, growthTasksListPath, nil)
	if err != nil {
		return nil, err
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	var data struct {
		Tasks []growthTask `json:"tasks"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, err
	}
	return data.Tasks, nil
}

// acceptGrowthTasks accepts the given task codes in one call and returns the
// subset the service actually accepted.
//
// The envelope is not enough to tell success: acceptance can fail per task and
// the service still answers HTTP 200 with code 0, reporting the failure inside
// data.results. An account with no buddy instance, for example, cannot accept
// the conversation tasks because their prerequisite (first_buddy) is unmet.
// Treating that as success makes the caller report events for a task that does
// not exist yet — pure cost against a rate-limited endpoint.
func acceptGrowthTasks(sa *storedAuth, codes []string) (map[string]bool, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	raw, err := growthCall(sa, http.MethodPost, growthTasksAcceptPath,
		map[string]any{"task_codes": codes})
	if err != nil {
		return nil, err
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	var data struct {
		Results []struct {
			TaskCode string `json:"task_code"`
			Status   string `json:"status"`
			Message  string `json:"message"`
		} `json:"results"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, err
	}
	accepted := make(map[string]bool, len(data.Results))
	for _, r := range data.Results {
		if r.Status == "accepted" {
			accepted[r.TaskCode] = true
			continue
		}
		hostLogf("info", fmt.Sprintf("tasks %s: %s not accepted (%s): %s",
			shortUID(sa.Account.UID), r.TaskCode, r.Status, r.Message))
	}
	return accepted, nil
}

// claimGrowthTask claims one completed task and returns the credit this call
// actually added. A repeat claim answers already_claimed with zero credit,
// which is a success, not an error.
func claimGrowthTask(sa *storedAuth, code string) (int, error) {
	raw, err := growthCall(sa, http.MethodPost,
		growthTasksTaskPrefix+code+growthTasksClaimSuffix, nil)
	if err != nil {
		return 0, err
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, err
	}
	var data struct {
		AlreadyClaimed bool `json:"already_claimed"`
		Credit         int  `json:"credit"`
		Energy         int  `json:"energy"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return 0, err
	}
	return data.Credit, nil
}

// reportTaskEvents posts the events that complete one task.
func reportTaskEvents(sa *storedAuth, plan taskReportPlan) error {
	base := fmt.Sprintf("wb2api-task-%d", time.Now().UnixMilli())
	for i := 1; i <= plan.count; i++ {
		conversationID := base
		if plan.separateConversations {
			conversationID = fmt.Sprintf("%s-c%d", base, i)
		}
		event := newChatRequestEvent(sa.Account.UID, conversationID,
			fmt.Sprintf("%s-r%d", conversationID, i), time.Now())
		event.RequestModelID = plan.modelID
		event.RequestModelName = plan.modelName
		if plan.nightOnly {
			event.Mode = eventModeNight
		}
		if _, err := growthCall(sa, http.MethodPost, activityEventPath, []any{event}); err != nil {
			return fmt.Errorf("event %d/%d: %w", i, plan.count, err)
		}
		if i < plan.count {
			time.Sleep(taskReportGap)
		}
	}
	return nil
}

// taskRunResult summarizes one task-centre run.
type taskRunResult struct {
	Total    int `json:"total"`
	Reported int `json:"reported"`
	Claimed  int `json:"claimed"`
	Credit   int `json:"credit"`
	Skipped  int `json:"skipped"`
	Failed   int `json:"failed"`
}

// runTasksForAccount runs the list -> accept -> report -> claim cycle for one
// account. Only planned tasks are accepted, and every stage re-reads the list
// rather than assuming the previous write landed.
func runTasksForAccount(sa *storedAuth, now time.Time) (taskRunResult, error) {
	result := taskRunResult{}
	tasks, err := listGrowthTasks(sa)
	if err != nil {
		return result, err
	}

	// reportable holds the planned tasks the service has accepted, whether in
	// this run or before. A task whose acceptance failed is excluded rather
	// than reported against, so a missing prerequisite costs nothing.
	reportable := map[string]bool{}
	var toAccept []string
	for _, t := range tasks {
		if _, planned := taskReportPlans[t.TaskCode]; !planned {
			continue
		}
		if t.AcceptStatus == "not_accepted" {
			toAccept = append(toAccept, t.TaskCode)
			continue
		}
		reportable[t.TaskCode] = true
	}
	if len(toAccept) > 0 {
		accepted, err := acceptGrowthTasks(sa, toAccept)
		if err != nil {
			return result, fmt.Errorf("accept %v: %w", toAccept, err)
		}
		for code := range accepted {
			reportable[code] = true
		}
		time.Sleep(taskCenterDelay)
		// progress stays null until acceptance, so re-read before deciding.
		if tasks, err = listGrowthTasks(sa); err != nil {
			return result, err
		}
	}

	reported := false
	for _, t := range tasks {
		plan, planned := taskReportPlans[t.TaskCode]
		if !planned {
			result.Skipped++
			continue
		}
		result.Total++
		if !reportable[t.TaskCode] {
			result.Skipped++
			continue
		}
		if t.complete() {
			continue
		}
		if plan.nightOnly && !nightOwlWindow(now) {
			result.Skipped++
			continue
		}
		if err := reportTaskEvents(sa, plan); err != nil {
			result.Failed++
			hostLogf("warn", fmt.Sprintf("tasks %s: %s: %v", shortUID(sa.Account.UID), t.TaskCode, err))
			continue
		}
		result.Reported++
		reported = true
	}
	if reported {
		time.Sleep(taskCenterDelay)
		if tasks, err = listGrowthTasks(sa); err != nil {
			return result, err
		}
	}

	for _, t := range tasks {
		if _, planned := taskReportPlans[t.TaskCode]; !planned || !t.complete() {
			continue
		}
		credit, err := claimGrowthTask(sa, t.TaskCode)
		if err != nil {
			result.Failed++
			hostLogf("warn", fmt.Sprintf("tasks %s: claim %s: %v", shortUID(sa.Account.UID), t.TaskCode, err))
			continue
		}
		if credit > 0 {
			result.Claimed++
			result.Credit += credit
		}
	}
	return result, nil
}

// runAutoGrowthTasks is the scheduled tick. Accounts outside the growth
// subsystem's scope are skipped without any upstream call.
func runAutoGrowthTasks() taskRunResult {
	result := taskRunResult{}
	growthTasksAutoMu.RLock()
	enabled := growthTasksAuto
	growthTasksAutoMu.RUnlock()
	if !enabled {
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
		if !activityEligible(sa) {
			result.Skipped++
			continue
		}
		if !first {
			time.Sleep(activityAccountDelay)
		}
		first = false
		per, err := runTasksForAccount(sa, time.Now())
		if err != nil {
			result.Failed++
			hostLogf("warn", fmt.Sprintf("tasks %s: %v", shortUID(sa.Account.UID), err))
			continue
		}
		result.Total += per.Total
		result.Reported += per.Reported
		result.Claimed += per.Claimed
		result.Credit += per.Credit
		result.Skipped += per.Skipped
		result.Failed += per.Failed
	}
	if result.Reported > 0 || result.Claimed > 0 || result.Failed > 0 {
		hostLogf("info", fmt.Sprintf("growth tasks done: reported=%d claimed=%d credit=%d failed=%d",
			result.Reported, result.Claimed, result.Credit, result.Failed))
	}
	return result
}

// handleManualGrowthTasks backs POST /growth-tasks.
func handleManualGrowthTasks(req pluginapi.ManagementRequest) map[string]any {
	result := runAutoGrowthTasks()
	return map[string]any{
		"success":  result.Failed == 0,
		"total":    result.Total,
		"reported": result.Reported,
		"claimed":  result.Claimed,
		"credit":   result.Credit,
		"skipped":  result.Skipped,
		"failed":   result.Failed,
	}
}
