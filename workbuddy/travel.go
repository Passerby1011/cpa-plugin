// travel.go implements the growth-system "cat travel" loop.
//
// Why this exists: it is the one growth task that pays out directly. Adoption
// grants a one-off bonus and every completed trip can be claimed for credits
// (data.reward_credit). The steps are a small state machine driven once per
// run, deliberately without polling or waiting:
//
//	no cat            -> agreement + first   (adoption; +300 the first time)
//	state "idle"      -> depart              (one trip per natural day, CST)
//	state "arrived"   -> claim               (needs record_id)
//	state "traveling" -> leave it alone
//
// Scope matches activity.go: CN personal accounts only. Enterprise accounts
// answer 403 and Global accounts return empty payloads, so both are skipped
// before any upstream call.
//
// The adoption gate ("first_buddy": five conversations) is met by the activity
// report. When adoption is refused because the gate is not yet met, the account
// is remembered for the rest of the day so we do not hammer the endpoint; a run
// that follows a successful activity report retries anyway, because that report
// is exactly what can lift the gate.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	travelStatusPath   = "/activity/growth/buddy/travel/status"
	travelDepartPath   = "/activity/growth/buddy/travel/depart"
	travelClaimPath    = "/activity/growth/buddy/travel/claim"
	buddyInfoPath      = "/activity/growth/buddy/info"
	buddyFirstPath     = "/activity/growth/buddy/first"
	buddyAgreementPath = "/activity/growth/buddy/agreement"
)

// travelLocationID is the departure destination. All four destinations share
// the same reward/duration range, so there is no optimal choice.
const travelLocationID = 4

// buddyTaskIncompleteMarker marks the expected "adoption gate not met yet"
// refusal. It is a normal condition, not an error worth retrying the same day.
const buddyTaskIncompleteMarker = "first_buddy task not completed yet"

// adoptionTriedToday remembers, per account, the CST day on which adoption was
// refused for an unmet gate. In-memory only: a restart clears it, which is
// harmless (one extra attempt).
var (
	adoptionMu    sync.Mutex
	adoptionTried = map[string]string{}
)

// cst is the fixed UTC+8 zone the upstream uses for its daily reset. Fixed
// rather than loaded from tzdata so the plugin does not depend on the host's
// zone database.
var cst = time.FixedZone("CST", 8*60*60)

// travelDay returns the natural day (CST) that t belongs to.
func travelDay(t time.Time) string {
	return t.In(cst).Format("2006-01-02")
}

// travelState mirrors the upstream travel/status payload.
type travelState struct {
	State             string `json:"state"`               // idle / traveling / arrived
	DailyLimitReached bool   `json:"daily_limit_reached"` // departed already today
	RecordID          int64  `json:"record_id"`           // required by claim
	RewardCredit      int64  `json:"reward_credit"`
}

// travelCall issues one growth request and unwraps the envelope.
func travelCall(sa *storedAuth, method, path string, body any) (json.RawMessage, error) {
	raw, err := growthCall(sa, method, path, body)
	if err != nil {
		return nil, err
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// buddyAdopted reports whether the account already has a cat.
func buddyAdopted(sa *storedAuth) (bool, error) {
	data, err := travelCall(sa, http.MethodGet, buddyInfoPath, nil)
	if err != nil {
		return false, err
	}
	var resp struct {
		Buddy json.RawMessage `json:"buddy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, err
	}
	trimmed := strings.TrimSpace(string(resp.Buddy))
	return trimmed != "" && trimmed != "null", nil
}

// isBuddyTaskIncomplete reports the expected adoption-gate refusal.
func isBuddyTaskIncomplete(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), buddyTaskIncompleteMarker)
}

// shouldSkipAdoption reports whether adoption was already refused today and
// must not be retried this run.
func shouldSkipAdoption(uid string, now time.Time, force bool) bool {
	if force {
		return false
	}
	adoptionMu.Lock()
	defer adoptionMu.Unlock()
	return adoptionTried[uid] == travelDay(now)
}

// markAdoptionTried records a gate refusal for the rest of the CST day.
func markAdoptionTried(uid string, now time.Time) {
	adoptionMu.Lock()
	defer adoptionMu.Unlock()
	adoptionTried[uid] = travelDay(now)
}

// clearAdoptionTried forgets a refusal (called once adoption succeeds).
func clearAdoptionTried(uid string) {
	adoptionMu.Lock()
	defer adoptionMu.Unlock()
	delete(adoptionTried, uid)
}

// adoptBuddy runs the adoption sequence: agree to the terms, then adopt. The
// agreement call is idempotent, so it is always sent first — adoption without
// it is refused.
func adoptBuddy(sa *storedAuth) error {
	if _, err := travelCall(sa, http.MethodPost, buddyAgreementPath, map[string]any{"agree": true}); err != nil {
		return fmt.Errorf("agreement: %w", err)
	}
	if _, err := travelCall(sa, http.MethodPost, buddyFirstPath, map[string]any{}); err != nil {
		return fmt.Errorf("adopt: %w", err)
	}
	return nil
}

// travelOutcome describes what one account's run did, for logging and tests.
type travelOutcome struct {
	Action string // adopted / departed / claimed / skipped
	Reward int64
}

// runTravelForAccount advances one account by at most one step.
//
// force lifts the same-day adoption debounce; callers set it right after a
// successful activity report, since that is what can satisfy the gate.
func runTravelForAccount(sa *storedAuth, now time.Time, force bool) (travelOutcome, error) {
	adopted, err := buddyAdopted(sa)
	if err != nil {
		return travelOutcome{Action: "skipped"}, fmt.Errorf("buddy info: %w", err)
	}

	if !adopted {
		if shouldSkipAdoption(sa.Account.UID, now, force) {
			return travelOutcome{Action: "skipped"}, nil
		}
		if err := adoptBuddy(sa); err != nil {
			if isBuddyTaskIncomplete(err) {
				// Expected until the conversation gate is met. Remember it for
				// the day instead of retrying, then report a clean skip.
				markAdoptionTried(sa.Account.UID, now)
				return travelOutcome{Action: "skipped"}, nil
			}
			return travelOutcome{Action: "skipped"}, fmt.Errorf("adopt: %w", err)
		}
		clearAdoptionTried(sa.Account.UID)
		return travelOutcome{Action: "adopted"}, nil
	}

	data, err := travelCall(sa, http.MethodGet, travelStatusPath, nil)
	if err != nil {
		return travelOutcome{Action: "skipped"}, fmt.Errorf("travel status: %w", err)
	}
	var status travelState
	if err := json.Unmarshal(data, &status); err != nil {
		return travelOutcome{Action: "skipped"}, err
	}

	switch strings.ToLower(strings.TrimSpace(status.State)) {
	case "arrived":
		if status.RecordID == 0 {
			// Arrived but the record id is missing: nothing to claim against.
			return travelOutcome{Action: "skipped"}, nil
		}
		reward, err := claimTravel(sa, status.RecordID)
		if err != nil {
			return travelOutcome{Action: "skipped"}, fmt.Errorf("claim: %w", err)
		}
		return travelOutcome{Action: "claimed", Reward: reward}, nil

	case "idle":
		if status.DailyLimitReached {
			// One trip per natural day, already used.
			return travelOutcome{Action: "skipped"}, nil
		}
		if err := departTravel(sa); err != nil {
			return travelOutcome{Action: "skipped"}, fmt.Errorf("depart: %w", err)
		}
		return travelOutcome{Action: "departed"}, nil

	case "traveling":
		return travelOutcome{Action: "skipped"}, nil

	default:
		return travelOutcome{Action: "skipped"}, nil
	}
}

// departTravel sends the cat out.
func departTravel(sa *storedAuth) error {
	_, err := travelCall(sa, http.MethodPost, travelDepartPath, map[string]any{"location_id": travelLocationID})
	return err
}

// claimTravel collects a completed trip's reward.
func claimTravel(sa *storedAuth, recordID int64) (int64, error) {
	data, err := travelCall(sa, http.MethodPost, travelClaimPath, map[string]any{"record_id": recordID})
	if err != nil {
		return 0, err
	}
	// A missing reward field is not a failure; the claim still went through.
	var resp struct {
		RewardCredit int64 `json:"reward_credit"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &resp)
	}
	return resp.RewardCredit, nil
}

// travelRunResult summarizes one run.
type travelRunResult struct {
	Total    int `json:"total"`
	Adopted  int `json:"adopted"`
	Departed int `json:"departed"`
	Claimed  int `json:"claimed"`
	Reward   int `json:"reward"`
	Failed   int `json:"failed"`
	Skipped  int `json:"skipped"`
}

// runTravel is the scheduled pass (09:00 and 21:00, so a trip can be both
// started and collected on the same day).
func runTravel(force bool) travelRunResult {
	result := travelRunResult{}
	files, err := hostAuthList()
	if err != nil {
		return result
	}
	now := time.Now()
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

		outcome, err := runTravelForAccount(sa, now, force)
		if err != nil {
			result.Failed++
			hostLogf("warn", fmt.Sprintf("travel %s: %v", shortUID(sa.Account.UID), err))
			continue
		}
		switch outcome.Action {
		case "adopted":
			result.Adopted++
			hostLogf("info", fmt.Sprintf("travel %s: adopted", shortUID(sa.Account.UID)))
		case "departed":
			result.Departed++
			hostLogf("info", fmt.Sprintf("travel %s: departed location=%d", shortUID(sa.Account.UID), travelLocationID))
		case "claimed":
			result.Claimed++
			result.Reward += int(outcome.Reward)
			hostLogf("info", fmt.Sprintf("travel %s: claimed reward=%d", shortUID(sa.Account.UID), outcome.Reward))
		default:
			result.Skipped++
		}
	}
	hostLogf("info", fmt.Sprintf("travel done: total=%d adopted=%d departed=%d claimed=%d reward=%d failed=%d skipped=%d",
		result.Total, result.Adopted, result.Departed, result.Claimed, result.Reward, result.Failed, result.Skipped))
	return result
}

// handleManualTravel backs POST /travel.
func handleManualTravel() map[string]any {
	result := runTravel(true)
	return map[string]any{
		"success":  result.Failed == 0,
		"total":    result.Total,
		"adopted":  result.Adopted,
		"departed": result.Departed,
		"claimed":  result.Claimed,
		"reward":   result.Reward,
		"failed":   result.Failed,
		"skipped":  result.Skipped,
	}
}
