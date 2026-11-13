package plugin

// HostBridge routes outbound traffic through the host callbacks so every
// upstream call lands in CPA's request log and inherits its proxy/transport
// policy. A plugin never opens its own net/http connection to an upstream.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/geminiapi/internal/backend"
)

// RawCaller performs one host callback round-trip.
type RawCaller func(method string, payload []byte) ([]byte, error)

// HostBridge implements backend.HTTPDoer over the host callbacks.
type HostBridge struct {
	call     RawCaller
	inFlight sync.WaitGroup
}

// NewHostBridge wraps the injected caller (main.go wires it to the C ABI;
// tests inject fakes).
func NewHostBridge(call RawCaller) *HostBridge { return &HostBridge{call: call} }

// hostHTTPReq is the wire shape of host.http.do.
type hostHTTPReq struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers http.Header `json:"headers"`
	Body    []byte      `json:"body"`
}

// hostLogReq is the wire shape of host.log.
type hostLogReq struct {
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields"`
}

// callbackTimeout bounds a single host HTTP callback.
var callbackTimeout = 30 * time.Minute

// logTimeout bounds a host.log callback so a wedged log cannot stall a request.
var logTimeout = 5 * time.Second

// Do satisfies backend.HTTPDoer by calling host.http.do.
//
// The request carries an Authorization header for the backend's own API key —
// that is a LOCAL backend credential, and it is the host's log that would
// capture it. The host's request log is the operator's own audit surface, so a
// backend key appearing there is expected and is never echoed back to a client.
func (b *HostBridge) Do(ctx context.Context, req backend.HTTPRequest) (backend.HTTPResponse, error) {
	if b == nil || b.call == nil {
		return backend.HTTPResponse{}, fmt.Errorf("host transport is unavailable")
	}
	payload, err := json.Marshal(hostHTTPReq{
		Method:  req.Method,
		URL:     req.URL,
		Headers: req.Headers,
		Body:    req.Body,
	})
	if err != nil {
		return backend.HTTPResponse{}, fmt.Errorf("encode host http request: %w", err)
	}
	raw, err := b.callWithTimeout(ctx, pluginabi.MethodHostHTTPDo, payload, callbackTimeout)
	if err != nil {
		return backend.HTTPResponse{}, err
	}
	env, err := decodeEnvelope(raw)
	if err != nil {
		return backend.HTTPResponse{}, err
	}
	if !env.OK {
		msg := "host http do failed"
		if env.Error != nil && env.Error.Message != "" {
			msg = "host http do failed: " + truncate(env.Error.Message, 200)
		}
		return backend.HTTPResponse{}, fmt.Errorf("%s", msg)
	}
	var resp pluginapi.HTTPResponse
	if len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, &resp); err != nil {
			return backend.HTTPResponse{}, fmt.Errorf("host http do failed: undecodable response")
		}
	}
	return backend.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: resp.Body}, nil
}

// Log forwards one diagnostic record. message must already be secret-free.
func (b *HostBridge) Log(level, message string, fields map[string]any) error {
	if b == nil || b.call == nil {
		return nil
	}
	payload, err := json.Marshal(hostLogReq{Level: level, Message: message, Fields: fields})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), logTimeout)
	defer cancel()
	_, err = b.callWithTimeout(ctx, pluginabi.MethodHostLog, payload, logTimeout)
	return err
}

// callWithTimeout runs one host callback under a deadline, recovering a panic
// so a host-boundary crash degrades to a redacted error instead of taking the
// host process down. Every invocation holds one inFlight count for its whole
// duration, so shutdown can drain goroutines that may still touch host memory.
func (b *HostBridge) callWithTimeout(ctx context.Context, method string, payload []byte, timeout time.Duration) ([]byte, error) {
	type result struct {
		raw []byte
		err error
	}
	done := make(chan result, 1)
	b.inFlight.Add(1)
	var once sync.Once
	release := func() { once.Do(func() { b.inFlight.Done() }) }
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- result{err: fmt.Errorf("host callback panicked")}
			}
		}()
		raw, err := b.call(method, payload)
		done <- result{raw: raw, err: err}
		release()
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	// A zero or negative timeout means "no budget of our own": the call is
	// governed by the host or by the caller's context. context.WithTimeout(ctx, 0)
	// would expire instantly, so it must never be used here.
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	select {
	case res := <-done:
		if res.err != nil {
			return nil, fmt.Errorf("host callback %s failed", method)
		}
		return res.raw, nil
	case <-ctx.Done():
		// The callback goroutine keeps its inFlight count until it returns, so
		// shutdown waits for it rather than dlclose-ing over a live frame.
		return nil, fmt.Errorf("host callback %s timed out", method)
	}
}

// WaitForInFlight blocks until every callback goroutine has fully returned, or
// the budget lapses.
func (b *HostBridge) WaitForInFlight(timeout time.Duration) bool {
	if b == nil {
		return true
	}
	drained := make(chan struct{})
	go func() {
		b.inFlight.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		return true
	case <-time.After(timeout):
		return false
	}
}

func decodeEnvelope(raw []byte) (pluginabi.Envelope, error) {
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return pluginabi.Envelope{}, fmt.Errorf("undecodable host response")
	}
	return env, nil
}

func truncate(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

func okEnvelope(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

// ErrEnvelope builds a failure envelope with a code and message. Message text
// must never carry a credential, a cookie or a raw upstream body.
func ErrEnvelope(code, message string) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message}})
	return out
}
