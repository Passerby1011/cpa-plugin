// Package auth implements Open Design's device authorization: the sign-in its
// own CLI performs against the Vela control plane.
//
// The flow, as the vendor's CLI does it:
//
//	POST {control}/api/v1/cli/device-authorizations
//	     {"cliVersion":…, "host":<this machine's name>, "profile":"prod"}
//	  -> {deviceId, deviceSecret, userCode, activationUrl,
//	      pollIntervalSeconds, expiresAt}
//	POST {control}/api/v1/cli/device-authorizations/{deviceId}/token
//	     {"deviceSecret":…}
//	  -> pending (no keys yet), or approved:
//	     {runtimeKey: sk-amr-…, controlKey: vela_ctrl-…, user:{id,email}}
//
// # TWO KEYS, TWO JOBS
//
// The runtime key rides every model request. The control key reads the account's
// plan and its allowance, and the control plane does not accept an API key —
// which is why a pasted key (sk-amr-…) has no allowance at all while a device
// sign-in does.
//
// # WHAT IS A FAILURE AND WHAT IS NOT
//
// A 404 or a terminal status is the authorization being over: the code ran out,
// or somebody refused it. Start again. A 401 is the device secret no longer
// being valid. Anything else — a DNS hiccup, a 5xx, a body this build does not
// recognize — is NOT the sign-in failing: polling continues until the code's
// own expiry, because a transport blip must not cost the operator a fresh code.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
	"os"
	"strings"
	"time"
)

// CLIVersion is what the sign-in says it is. amr-api gates its CLI endpoints on
// a plausible version, so this is the vendor CLI's own, not this plugin's.
const CLIVersion = "0.1.4"

// Profile is the control plane deployment the sign-in targets.
const Profile = "prod"

// ClientVersion builds the User-Agent this plugin presents. amr-api's CLI
// endpoints are used to a vendor client, and a bare Go default is the anomaly.
const userAgent = "opencode-opendesign-auth/0.1.4"

// TerminalStatuses are the poll statuses that mean the authorization is over.
var TerminalStatuses = []string{"expired", "canceled", "cancelled", "denied", "rejected"}

// Device is a started device authorization: everything the poll needs.
type Device struct {
	DeviceID      string
	DeviceSecret  string
	UserCode      string
	ActivationURL string
	// Interval is how long to wait between polls, as the control plane asked.
	Interval time.Duration
	// ExpiresAt is when the code stops being approvable.
	ExpiresAt time.Time
}

// Authorization is a completed sign-in: the account's two keys and who it is.
type Authorization struct {
	RuntimeKey string
	ControlKey string
	Email      string
	UID        string
}

// ErrHostnameUnreadable is returned when this machine's name cannot be read.
// The vendor CLI stops with "resolve hostname" rather than sending a name of
// its own, and so does this: a fabricated host name is a different machine's
// sign-in.
var ErrHostnameUnreadable = errors.New("this computer's name couldn't be read, and the sign-in sends it")

// ErrTerminal is returned when the authorization is over.
type ErrTerminal struct {
	// Status is the control plane's own word, or "not found" when it answered 404.
	Status string
}

func (e *ErrTerminal) Error() string {
	return fmt.Sprintf("the request was %s — start again", e.Status)
}

// ErrInvalidSecret is returned when the control plane no longer recognizes this
// device's secret (401 invalid_device_secret).
var ErrInvalidSecret = errors.New("the code is no longer valid — start again")

// ErrInvalidApproval is returned on a 401 the control plane did not attribute to
// the device secret.
var ErrInvalidApproval = errors.New("the approval is no longer valid — start again")

// ErrCanceleled is returned when the caller's context ends.
var ErrCancelled = errors.New("cancelled")

// ErrCodeExpired is returned when the code ran out before it was approved.
var ErrCodeExpired = errors.New("the code ran out before it was approved")

// Transport issues one HTTP call. status is the upstream status code; body is
// the raw response; err is a transport failure, in which case status is 0.
type Transport func(ctx context.Context, method, url string, headers map[string]string, body []byte) (status int, respBody []byte, err error)

// HostnameFn reads this machine's name. os.Hostname by default; injected so a
// test can cover both the readable and the unreadable machine.
type HostnameFn func() (string, error)

// ---- responses -------------------------------------------------------------

type startResponse struct {
	DeviceID            string `json:"deviceId"`
	DeviceSecret        string `json:"deviceSecret"`
	UserCode            string `json:"userCode"`
	ActivationURL       string `json:"activationUrl"`
	PollIntervalSeconds int    `json:"pollIntervalSeconds"`
	ExpiresAt           string `json:"expiresAt"`
}

type tokenUser struct {
	ID        string `json:"id"`
	AppUserID string `json:"appUserId"`
	Email     string `json:"email"`
}

type tokenResponse struct {
	Status     string    `json:"status"`
	RuntimeKey string    `json:"runtimeKey"`
	ControlKey string    `json:"controlKey"`
	User       tokenUser `json:"user"`
	// Error is the control plane's own error code (invalid_device_secret).
	Error string `json:"error"`
	// Message is a human-readable counterpart when the plane sends one.
	Message string `json:"message"`
}

// ---- HTTP helpers ----------------------------------------------------------

func doJSON(ctx context.Context, transport Transport, method, url string, payload any) (int, []byte, error) {
	headers := map[string]string{
		"Accept":     "application/json",
		"User-Agent": userAgent,
	}
	var body []byte
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, fmt.Errorf("could not build the sign-in request")
		}
		body = raw
		headers["Content-Type"] = "application/json"
	}
	return transport(ctx, method, url, headers, body)
}

// said is the vendor's own words from a response, never a page of HTML.
func said(body []byte, fallback string) string {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		trimmed := strings.TrimSpace(string(body))
		if trimmed == "" || strings.HasPrefix(trimmed, "<") {
			return fallback
		}
		return truncate(trimmed, 300)
	}
	for _, key := range []string{"message", "error"} {
		var s string
		if raw, ok := doc[key]; ok && json.Unmarshal(raw, &s) == nil {
			if s = strings.TrimSpace(s); s != "" && !strings.HasPrefix(s, "<") {
				return truncate(s, 300)
			}
		}
	}
	// error may be an object carrying a message.
	if raw, ok := doc["error"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) == nil {
			var s string
			if msg, ok := nested["message"]; ok && json.Unmarshal(msg, &s) == nil {
				if s = strings.TrimSpace(s); s != "" && !strings.HasPrefix(s, "<") {
					return truncate(s, 300)
				}
			}
		}
	}
	return fallback
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// ---- start -----------------------------------------------------------------

// Start opens a device authorization.
//
// hostname is mandatory: this machine's name is what the approval page shows the
// operator, and a sign-in that cannot say which machine it is for is refused
// here rather than sent with a name of its own.
func Start(ctx context.Context, transport Transport, controlURL string, hostname HostnameFn) (*Device, error) {
	if transport == nil {
		return nil, fmt.Errorf("no host transport available")
	}
	name := ""
	if hostname != nil {
		v, err := hostname()
		if err != nil {
			return nil, fmt.Errorf("Open Design sign-in: %w", ErrHostnameUnreadable)
		}
		name = strings.TrimSpace(v)
	}
	if name == "" {
		return nil, fmt.Errorf("Open Design sign-in: %w", ErrHostnameUnreadable)
	}
	url := strings.TrimRight(controlURL, "/") + "/api/v1/cli/device-authorizations"
	status, body, err := doJSON(ctx, transport, "POST", url, map[string]any{
		"cliVersion": CLIVersion,
		"host":       name,
		"profile":    Profile,
	})
	if err != nil {
		return nil, fmt.Errorf("Open Design sign-in: the control plane could not be reached")
	}
	var doc startResponse
	if status == 200 || status == 201 {
		if json.Unmarshal(body, &doc) != nil {
			return nil, fmt.Errorf("Open Design sign-in: the control plane answered %d (%s)", status, said(body, "no device authorization"))
		}
	}
	if doc.DeviceID == "" || doc.DeviceSecret == "" {
		return nil, fmt.Errorf("Open Design sign-in: the control plane answered %d (%s)", status, said(body, "no device authorization"))
	}
	interval := time.Duration(doc.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}
	expires := time.Now().Add(10 * time.Minute)
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(doc.ExpiresAt)); err == nil {
		expires = t
	}
	return &Device{
		DeviceID:      strings.TrimSpace(doc.DeviceID),
		DeviceSecret:  strings.TrimSpace(doc.DeviceSecret),
		UserCode:      strings.TrimSpace(doc.UserCode),
		ActivationURL: strings.TrimSpace(doc.ActivationURL),
		Interval:      interval,
		ExpiresAt:     expires,
	}, nil
}

// ---- poll ------------------------------------------------------------------

// PollResult is one poll's outcome.
type PollResult struct {
	// Authorization is set when the approval completed.
	Authorization *Authorization
	// Pending is true when the control plane answered "not yet".
	Pending bool
}

// Poll asks the control plane once whether the code has been approved.
//
// The three failure kinds are kept apart because they call for different
// behaviour: ErrTerminal and ErrInvalidSecret/ErrInvalidApproval end the flow,
// while any other error is a transport blip the caller should retry until the
// code's own expiry.
func Poll(ctx context.Context, transport Transport, controlURL string, device *Device) (PollResult, error) {
	if transport == nil {
		return PollResult{}, fmt.Errorf("no host transport available")
	}
	if device == nil || device.DeviceID == "" || device.DeviceSecret == "" {
		return PollResult{}, fmt.Errorf("Open Design sign-in: no device authorization to poll")
	}
	url := strings.TrimRight(controlURL, "/") + "/api/v1/cli/device-authorizations/" + neturl.PathEscape(device.DeviceID) + "/token"
	status, body, err := doJSON(ctx, transport, "POST", url, map[string]any{"deviceSecret": device.DeviceSecret})
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return PollResult{}, ErrCancelled
		}
		// A hiccup on the way to the approval is not the sign-in failing.
		return PollResult{}, errTransport{err}
	}
	var doc tokenResponse
	if json.Unmarshal(body, &doc) == nil {
		if a := authorizationOf(doc); a != nil {
			return PollResult{Authorization: a}, nil
		}
	}
	rawStatus := strings.ToLower(strings.TrimSpace(doc.Status))
	if status == 404 || isTerminal(rawStatus) {
		label := rawStatus
		if label == "" {
			label = "not found"
		}
		return PollResult{}, &ErrTerminal{Status: label}
	}
	if status == 401 {
		if strings.TrimSpace(doc.Error) == "invalid_device_secret" {
			return PollResult{}, ErrInvalidSecret
		}
		return PollResult{}, ErrInvalidApproval
	}
	// pending, or an answer this build does not know: wait for the next poll.
	return PollResult{Pending: true}, nil
}

// errTransport marks a retryable transport failure, so a caller can tell it
// apart from the three terminal errors without matching on strings.
type errTransport struct{ err error }

func (e errTransport) Error() string { return e.err.Error() }
func (e errTransport) Unwrap() error { return e.err }

// IsRetryable reports whether a Poll error should be retried: a transport
// failure is, the three terminal kinds are not.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var t errTransport
	if errors.As(err, &t) {
		return true
	}
	if errors.Is(err, ErrCancelled) || errors.Is(err, ErrCodeExpired) ||
		errors.Is(err, ErrInvalidSecret) || errors.Is(err, ErrInvalidApproval) {
		return false
	}
	var terminal *ErrTerminal
	if errors.As(err, &terminal) {
		return false
	}
	// Anything else is reported as retryable: the safest reading of an
	// unrecognized failure is "try again", because giving up costs the operator
	// a fresh code.
	return true
}

func isTerminal(status string) bool {
	for _, t := range TerminalStatuses {
		if status == t {
			return true
		}
	}
	return false
}

// authorizationOf extracts a completed authorization from a poll response.
//
// Both keys are required. The control plane has been seen to answer an
// approval with only the runtime key (a state the CLI never stores), and an
// account half-authorized is worse than one still pending: it would serve
// models while silently losing the allowance reading.
func authorizationOf(doc tokenResponse) *Authorization {
	runtime := strings.TrimSpace(doc.RuntimeKey)
	control := strings.TrimSpace(doc.ControlKey)
	// The check has to happen BEFORE the emptiness test below: a body that
	// carries a control key in the runtime slot has both fields non-empty, so
	// testing them first would accept it and then sign every model request with
	// a key the model plane refuses.
	if strings.HasPrefix(runtime, "vela_ctrl_") {
		return nil
	}
	if runtime == "" || control == "" {
		return nil
	}
	return &Authorization{
		RuntimeKey: runtime,
		ControlKey: control,
		Email:      strings.TrimSpace(doc.User.Email),
		UID:        firstNonEmpty(doc.User.ID, doc.User.AppUserID),
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// WaitFor polls until the authorization completes, is refused, or the code
// expires. Interval and expiry are the device's own; sleep is injected so a test
// does not wait in real time.
//
// A transport failure does not end the wait: the loop sleeps and asks again,
// because the code's expiry — not one failed request — is what ends a sign-in.
func WaitFor(ctx context.Context, transport Transport, controlURL string, device *Device, sleep func(time.Duration)) (*Authorization, error) {
	if sleep == nil {
		sleep = func(d time.Duration) {
			time.Sleep(d)
		}
	}
	interval := device.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		if ctx != nil && ctx.Err() != nil {
			return nil, ErrCancelled
		}
		if !device.ExpiresAt.IsZero() && time.Now().After(device.ExpiresAt) {
			return nil, ErrCodeExpired
		}
		sleep(interval)
		if ctx != nil && ctx.Err() != nil {
			return nil, ErrCancelled
		}
		result, err := Poll(ctx, transport, controlURL, device)
		if err == nil {
			if result.Authorization != nil {
				return result.Authorization, nil
			}
			continue
		}
		if IsRetryable(err) {
			continue
		}
		return nil, err
	}
}

// Hostname is the production HostnameFn.
func Hostname() (string, error) { return os.Hostname() }

// DefaultTransportHint is the transport the plugin must supply: every upstream
// call goes through the host's bridge (host.http.do), never a bare client, so
// the host's proxy and request-log rules apply.
const DefaultTransportHint = "host.http.do"
