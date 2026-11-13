package auth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeTransport records every call and answers through a handler.
// resp builds a transport handler's 200 answer.
func resp(body string) (int, []byte, error) { return 200, []byte(body), nil }

type fakeTransport struct {
	calls   []string
	handler func(method, url string, body []byte) (int, []byte, error)
}

func (f *fakeTransport) fn() Transport {
	return func(ctx context.Context, method, url string, headers map[string]string, body []byte) (int, []byte, error) {
		f.calls = append(f.calls, method+" "+url)
		if f.handler == nil {
			return 0, nil, errors.New("no handler")
		}
		return f.handler(method, url, body)
	}
}

// TestStartSendsTheMachinesName pins the start body: the vendor CLI sends this
// machine's name and a pinned CLI version, and the control plane gates on both.
func TestStartSendsTheMachinesName(t *testing.T) {
	var gotBody map[string]any
	tr := &fakeTransport{handler: func(method, url string, body []byte) (int, []byte, error) {
		if method != "POST" || !strings.HasSuffix(url, "/api/v1/cli/device-authorizations") {
			t.Errorf("call = %s %s", method, url)
		}
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("body: %v", err)
		}
		return resp(`{"deviceId":"d1","deviceSecret":"s1","userCode":"ABCD-1234","activationUrl":"https://activate.example/x","pollIntervalSeconds":2,"expiresAt":"2030-01-01T00:00:00Z"}`)
	}}
	device, err := Start(context.Background(), tr.fn(), "https://control.example", func() (string, error) {
		return "workstation-1", nil
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if gotBody["host"] != "workstation-1" {
		t.Errorf("host = %v, want this machine's name", gotBody["host"])
	}
	if gotBody["profile"] != "prod" {
		t.Errorf("profile = %v", gotBody["profile"])
	}
	if gotBody["cliVersion"] != CLIVersion {
		t.Errorf("cliVersion = %v, want %q", gotBody["cliVersion"], CLIVersion)
	}
	if device.DeviceID != "d1" || device.DeviceSecret != "s1" {
		t.Errorf("device = %+v", device)
	}
	if device.UserCode != "ABCD-1234" || device.ActivationURL == "" {
		t.Errorf("device = %+v", device)
	}
	if device.Interval != 2*time.Second {
		t.Errorf("interval = %v", device.Interval)
	}
	if device.ExpiresAt.IsZero() {
		t.Errorf("expiry was not read")
	}
}

// An unreadable host name is a refusal, not a fabricated name: the vendor CLI
// stops there, and a sign-in that names another machine is worse than none.
func TestStartRefusesWithoutAMachineName(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   HostnameFn
	}{
		{"nil fn", nil},
		{"error", func() (string, error) { return "", errors.New("no name") }},
		{"empty", func() (string, error) { return "   ", nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &fakeTransport{}
			_, err := Start(context.Background(), tr.fn(), "https://control.example", tc.fn)
			if err == nil {
				t.Fatal("a sign-in with no machine name must be refused")
			}
			if !errors.Is(err, ErrHostnameUnreadable) {
				t.Errorf("err = %v, want ErrHostnameUnreadable", err)
			}
			if len(tr.calls) != 0 {
				t.Errorf("no request may be sent, got %v", tr.calls)
			}
		})
	}
}

func TestStartReportsAnUpstreamRefusal(t *testing.T) {
	tr := &fakeTransport{handler: func(string, string, []byte) (int, []byte, error) {
		return 500, []byte(`{"error":{"message":"control plane is down"}}`), nil
	}}
	_, err := Start(context.Background(), tr.fn(), "https://control.example", func() (string, error) { return "host", nil })
	if err == nil {
		t.Fatal("a 500 start must fail")
	}
	if !strings.Contains(err.Error(), "control plane is down") {
		t.Errorf("err = %v, want the vendor's own words", err)
	}
}

// The pending -> success path, with the poll taking BOTH keys.
func TestPollPendingThenSuccess(t *testing.T) {
	polls := 0
	tr := &fakeTransport{handler: func(method, url string, body []byte) (int, []byte, error) {
		if !strings.Contains(url, "/device-authorizations/d1/token") {
			t.Errorf("poll url = %s", url)
		}
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if req["deviceSecret"] != "s1" {
			t.Errorf("poll body = %v, want the device secret", req)
		}
		polls++
		if polls < 2 {
			return resp(`{"status":"pending","runtimeKey":null,"controlKey":null,"user":null}`)
		}
		return resp(`{"status":"approved","runtimeKey":"sk-amr-runtime","controlKey":"vela_ctrl_control","user":{"id":"u1","email":"ada@example.com"}}`)
	}}
	device := &Device{DeviceID: "d1", DeviceSecret: "s1", Interval: 0, ExpiresAt: time.Now().Add(time.Hour)}
	var slept int
	a, err := WaitFor(context.Background(), tr.fn(), "https://control.example", device, func(time.Duration) { slept++ })
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if a.RuntimeKey != "sk-amr-runtime" || a.ControlKey != "vela_ctrl_control" {
		t.Errorf("authorization = %+v", a)
	}
	if a.Email != "ada@example.com" || a.UID != "u1" {
		t.Errorf("account = %+v", a)
	}
	if polls != 2 {
		t.Errorf("polls = %d, want 2", polls)
	}
	if slept != 2 {
		t.Errorf("slept = %d", slept)
	}
}

// A terminal status ends the flow: the code was refused or ran out, and polling
// it again cannot succeed.
func TestPollTerminalStatusFails(t *testing.T) {
	for _, status := range TerminalStatuses {
		t.Run(status, func(t *testing.T) {
			tr := &fakeTransport{handler: func(string, string, []byte) (int, []byte, error) {
				return resp(`{"status":"` + status + `"}`)
			}}
			_, err := Poll(context.Background(), tr.fn(), "https://c", &Device{DeviceID: "d", DeviceSecret: "s"})
			var terminal *ErrTerminal
			if !errors.As(err, &terminal) {
				t.Fatalf("err = %v, want ErrTerminal", err)
			}
			if terminal.Status != status {
				t.Errorf("status = %q, want %q", terminal.Status, status)
			}
			if IsRetryable(err) {
				t.Errorf("%q must not be retried", status)
			}
		})
	}
}

// A 404 is the authorization being gone, which is the same outcome as an expiry.
func TestPoll404IsTerminal(t *testing.T) {
	tr := &fakeTransport{handler: func(string, string, []byte) (int, []byte, error) {
		return 404, []byte(`{}`), nil
	}}
	_, err := Poll(context.Background(), tr.fn(), "https://c", &Device{DeviceID: "d", DeviceSecret: "s"})
	var terminal *ErrTerminal
	if !errors.As(err, &terminal) {
		t.Fatalf("err = %v, want ErrTerminal", err)
	}
	if terminal.Status != "not found" {
		t.Errorf("status = %q", terminal.Status)
	}
}

// A 401 that names invalid_device_secret is a distinct failure from any other
// 401: the code is dead in one case and the approval withdrawn in the other.
func TestPoll401DistinguishesInvalidSecret(t *testing.T) {
	tr := &fakeTransport{handler: func(string, string, []byte) (int, []byte, error) {
		return 401, []byte(`{"error":"invalid_device_secret"}`), nil
	}}
	_, err := Poll(context.Background(), tr.fn(), "https://c", &Device{DeviceID: "d", DeviceSecret: "s"})
	if !errors.Is(err, ErrInvalidSecret) {
		t.Fatalf("err = %v, want ErrInvalidSecret", err)
	}
	if IsRetryable(err) {
		t.Errorf("an invalid device secret must not be retried")
	}

	tr2 := &fakeTransport{handler: func(string, string, []byte) (int, []byte, error) {
		return 401, []byte(`{"error":"unauthorized"}`), nil
	}}
	_, err = Poll(context.Background(), tr2.fn(), "https://c", &Device{DeviceID: "d", DeviceSecret: "s"})
	if !errors.Is(err, ErrInvalidApproval) {
		t.Fatalf("err = %v, want ErrInvalidApproval", err)
	}
}

// A transport failure is NOT the sign-in failing: the loop keeps asking until the
// code's own expiry, because one lost request must not cost a fresh code.
func TestTransportFailureKeepsPolling(t *testing.T) {
	failures := 2
	polls := 0
	tr := &fakeTransport{handler: func(string, string, []byte) (int, []byte, error) {
		polls++
		if failures > 0 {
			failures--
			return 0, nil, errors.New("dial tcp: connection reset")
		}
		return resp(`{"runtimeKey":"sk-amr-r","controlKey":"vela_ctrl_c","user":{"id":"u1"}}`)
	}}
	device := &Device{DeviceID: "d", DeviceSecret: "s", Interval: 0, ExpiresAt: time.Now().Add(time.Hour)}
	a, err := WaitFor(context.Background(), tr.fn(), "https://c", device, func(time.Duration) {})
	if err != nil {
		t.Fatalf("WaitFor: %v (a transport failure must not end the sign-in)", err)
	}
	if a == nil || a.RuntimeKey == "" {
		t.Fatalf("authorization = %+v", a)
	}
	if polls != 3 {
		t.Errorf("polls = %d, want 3 (two failures then a success)", polls)
	}
}

// The code's own expiry ends the wait, and it says so.
func TestWaitForStopsAtExpiry(t *testing.T) {
	tr := &fakeTransport{handler: func(string, string, []byte) (int, []byte, error) {
		return resp(`{"status":"pending"}`)
	}}
	device := &Device{DeviceID: "d", DeviceSecret: "s", Interval: 0, ExpiresAt: time.Now().Add(-time.Second)}
	_, err := WaitFor(context.Background(), tr.fn(), "https://c", device, func(time.Duration) {})
	if !errors.Is(err, ErrCodeExpired) {
		t.Fatalf("err = %v, want ErrCodeExpired", err)
	}
}

// Both keys are required: an approval carrying only the runtime key would serve
// models while silently losing the allowance reading.
func TestAuthorizationRequiresBothKeys(t *testing.T) {
	tr := &fakeTransport{handler: func(string, string, []byte) (int, []byte, error) {
		return resp(`{"status":"approved","runtimeKey":"sk-amr-r","user":{"id":"u1"}}`)
	}}
	result, err := Poll(context.Background(), tr.fn(), "https://c", &Device{DeviceID: "d", DeviceSecret: "s"})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if result.Authorization != nil {
		t.Errorf("a half-authorized account must not be accepted: %+v", result.Authorization)
	}
	if !result.Pending {
		t.Errorf("a half-answer is pending, got %+v", result)
	}
}

// A control key is never taken as a runtime key.
func TestControlKeyIsNotARuntimeKey(t *testing.T) {
	tr := &fakeTransport{handler: func(string, string, []byte) (int, []byte, error) {
		return resp(`{"status":"approved","runtimeKey":"vela_ctrl_x","controlKey":"vela_ctrl_y","user":{"id":"u1"}}`)
	}}
	result, err := Poll(context.Background(), tr.fn(), "https://c", &Device{DeviceID: "d", DeviceSecret: "s"})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if result.Authorization != nil {
		t.Errorf("a control key must never become the runtime key: %+v", result.Authorization)
	}
}
