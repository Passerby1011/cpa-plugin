// Package plugin implements the CLIProxyAPI dispatcher for the opendesign
// provider: registration, the device sign-in, model publication, the three
// protocol executors and the host-callback bridge.
package plugin

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
)

// RawCaller performs one host callback round-trip: method name plus JSON request
// bytes in, JSON envelope bytes out.
type RawCaller func(method string, payload []byte) ([]byte, error)

// HostBridge routes every outbound call through the host's callbacks:
// host.http.do / do_stream / stream_read / stream_close / stream.emit /
// stream.close / log / auth.*. Nothing in this plugin opens a socket of its own,
// so the host's proxy, TLS and request-log rules apply to all of it.
//
// Callers must never put a key, an auth header, a prompt or a tool argument into
// a Log message or field.
type HostBridge struct {
	call RawCaller
	// inFlight counts host-callback invocations whose goroutines may still
	// run. handleShutdown waits on it: on Unix the loader frees host_api and
	// dlclose's this library as soon as the shutdown export returns, and a
	// goroutine still inside an FFI frame at that moment crashes the host.
	inFlight sync.WaitGroup
}

// NewHostBridge wraps the injected raw host caller.
func NewHostBridge(call RawCaller) *HostBridge {
	return &HostBridge{call: call}
}

// hostHTTPReq is the wire shape host.http.do / do_stream accept.
type hostHTTPReq struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers http.Header `json:"headers"`
	Body    []byte      `json:"body,omitempty"`
}

type hostStreamIDReq struct {
	StreamID string `json:"stream_id"`
}

type hostStreamStartResp struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

type hostStreamReadResp struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type hostLogReq struct {
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// timeouts. Package vars so tests can shrink them.
var (
	logTimeout  = 5 * time.Second
	emitTimeout = 5 * time.Second
	streamGrace = 3 * time.Second
)

func decodeEnvelope(raw []byte) (pluginabi.Envelope, error) {
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return pluginabi.Envelope{}, fmt.Errorf("undecodable host response")
	}
	return env, nil
}

type callResult struct {
	raw []byte
	err error
}

// callWithTimeout runs one host callback under ctx. On expiry it returns a
// timeout error immediately and lets the orphaned goroutine finish on its own
// (buffered channel, so it never leaks); the inFlight count is released only once
// that goroutine has actually exited, which is what keeps shutdown honest.
func (b *HostBridge) callWithTimeout(ctx context.Context, method string, payload []byte) ([]byte, error) {
	raw, err := b.callWithTimeoutGrace(ctx, method, payload, 0)
	return raw, err
}

func (b *HostBridge) callWithTimeoutGrace(ctx context.Context, method string, payload []byte, grace time.Duration) ([]byte, error) {
	done := make(chan callResult, 1)
	b.inFlight.Add(1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- callResult{err: fmt.Errorf("host callback panicked")}
			}
			b.inFlight.Done()
		}()
		raw, err := b.call(method, payload)
		done <- callResult{raw: raw, err: err}
	}()
	if ctx == nil {
		res := <-done
		return res.raw, res.err
	}
	select {
	case res := <-done:
		return res.raw, res.err
	case <-ctx.Done():
		if grace > 0 {
			timer := time.NewTimer(grace)
			defer timer.Stop()
			select {
			case res := <-done:
				return res.raw, res.err
			case <-timer.C:
			}
		}
		return nil, fmt.Errorf("host call %s timed out", method)
	}
}

// WaitForInFlight blocks until every callback has finished, or the timeout
// elapses.
func (b *HostBridge) WaitForInFlight(timeout time.Duration) bool {
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

// invoke marshals a payload and performs one callback, normalizing failures
// into a redacted error. Result decoding is the caller's job, because each
// method has its own result shape.
func (b *HostBridge) invoke(ctx context.Context, method string, payload any, what string) (pluginabi.Envelope, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return pluginabi.Envelope{}, fmt.Errorf("%s failed: invalid request", what)
	}
	raw, errCall := b.callWithTimeout(ctx, method, body)
	if errCall != nil {
		return pluginabi.Envelope{}, fmt.Errorf("%s failed: %w", what, errCall)
	}
	env, errDecode := decodeEnvelope(raw)
	if errDecode != nil {
		return pluginabi.Envelope{}, fmt.Errorf("%s failed: %w", what, errDecode)
	}
	if !env.OK {
		// The host's error text is not trusted: it may echo request JSON.
		return pluginabi.Envelope{}, fmt.Errorf("%s failed", what)
	}
	return env, nil
}

// Do issues one non-streaming upstream HTTP request through host.http.do.
func (b *HostBridge) Do(ctx context.Context, method, url string, headers map[string]string, body []byte) (int, http.Header, []byte, error) {
	env, err := b.invoke(ctx, pluginabi.MethodHostHTTPDo, hostHTTPReq{
		Method:  method,
		URL:     url,
		Headers: toHTTPHeader(headers),
		Body:    body,
	}, "host http do")
	if err != nil {
		return 0, nil, nil, err
	}
	var resp pluginapi.HTTPResponse
	if len(env.Result) > 0 {
		if errDecode := json.Unmarshal(env.Result, &resp); errDecode != nil {
			return 0, nil, nil, fmt.Errorf("host http do failed: undecodable response body")
		}
	}
	return resp.StatusCode, resp.Headers, resp.Body, nil
}

// Transport adapts Do into the plain function shape the auth and catalog
// packages take, so neither of them depends on this package.
func (b *HostBridge) Transport() func(ctx context.Context, method, url string, headers map[string]string, body []byte) (int, []byte, error) {
	return func(ctx context.Context, method, url string, headers map[string]string, body []byte) (int, []byte, error) {
		status, _, respBody, err := b.Do(ctx, method, url, headers, body)
		return status, respBody, err
	}
}

// DoStream opens an upstream stream.
//
// The grace window matters: the host registers the upstream stream BEFORE
// surfacing its status, so a result landing just past the attempt deadline still
// has a live stream behind it. A late result is closed here rather than leaked.
func (b *HostBridge) DoStream(ctx context.Context, method, url string, headers map[string]string, body []byte) (int, string, error) {
	payload, _ := json.Marshal(hostHTTPReq{
		Method:  method,
		URL:     url,
		Headers: toHTTPHeader(headers),
		Body:    body,
	})
	raw, err := b.callWithTimeoutGrace(ctx, pluginabi.MethodHostHTTPDoStream, payload, streamGrace)
	if err != nil {
		// Whatever the orphaned start produced is released here.
		b.closeLateStream(raw)
		return 0, "", err
	}
	env, errDecode := decodeEnvelope(raw)
	if errDecode != nil {
		return 0, "", fmt.Errorf("host http do_stream failed: %w", errDecode)
	}
	if !env.OK {
		return 0, "", fmt.Errorf("host http do_stream failed")
	}
	var resp hostStreamStartResp
	if len(env.Result) > 0 {
		if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
			return 0, "", fmt.Errorf("host http do_stream failed: undecodable response body")
		}
	}
	return resp.StatusCode, resp.StreamID, nil
}

// closeLateStream releases a do_stream result that completed after its deadline.
func (b *HostBridge) closeLateStream(raw []byte) {
	env, err := decodeEnvelope(raw)
	if err != nil || !env.OK || len(env.Result) == 0 {
		return
	}
	var resp hostStreamStartResp
	if json.Unmarshal(env.Result, &resp) != nil || resp.StreamID == "" {
		return
	}
	_ = b.StreamClose(resp.StreamID)
}

// StreamRead reads one chunk from an upstream stream. errMsg carries an
// upstream-reported failure label; done reports a clean end.
func (b *HostBridge) StreamRead(upstreamID string) (payload []byte, errMsg string, done bool, err error) {
	env, errInvoke := b.invoke(context.Background(), pluginabi.MethodHostHTTPStreamRead, hostStreamIDReq{StreamID: upstreamID}, "host http stream_read")
	if errInvoke != nil {
		return nil, "", false, errInvoke
	}
	var resp hostStreamReadResp
	if len(env.Result) > 0 {
		if errDecode := json.Unmarshal(env.Result, &resp); errDecode != nil {
			return nil, "", false, fmt.Errorf("host http stream_read failed: undecodable response body")
		}
	}
	return resp.Payload, resp.Error, resp.Done, nil
}

// StreamClose closes an upstream stream.
func (b *HostBridge) StreamClose(upstreamID string) error {
	_, err := b.invoke(context.Background(), pluginabi.MethodHostHTTPStreamClose, hostStreamIDReq{StreamID: upstreamID}, "host http stream_close")
	return err
}

// StreamEmit writes one chunk to a downstream stream (the host-allocated id
// received in executor.execute_stream). Bounded by emitTimeout, so a host whose
// consumer stopped draining surfaces as an error instead of wedging the pump.
func (b *HostBridge) StreamEmit(downstreamID string, payload []byte) error {
	req := struct {
		StreamID string `json:"stream_id"`
		Payload  []byte `json:"payload"`
	}{downstreamID, payload}
	ctx, cancel := context.WithTimeout(context.Background(), emitTimeout)
	defer cancel()
	_, err := b.invoke(ctx, pluginabi.MethodHostStreamEmit, req, "host stream emit")
	return err
}

// StreamCloseDownstream terminates a downstream stream, optionally with a
// redacted error label.
func (b *HostBridge) StreamCloseDownstream(downstreamID, errMsg string) error {
	req := struct {
		StreamID string `json:"stream_id"`
		Error    string `json:"error,omitempty"`
	}{downstreamID, errMsg}
	ctx, cancel := context.WithTimeout(context.Background(), emitTimeout)
	defer cancel()
	_, err := b.invoke(ctx, pluginabi.MethodHostStreamClose, req, "host stream close")
	return err
}

// Log forwards one diagnostic record. message and fields must already be
// secret-free; the returned error is best-effort.
func (b *HostBridge) Log(level, message string, fields map[string]any) error {
	ctx, cancel := context.WithTimeout(context.Background(), logTimeout)
	defer cancel()
	_, err := b.invoke(ctx, pluginabi.MethodHostLog, hostLogReq{Level: level, Message: message, Fields: fields}, "host log")
	return err
}

// AuthList returns every auth record the host knows, unfiltered.
func (b *HostBridge) AuthList(ctx context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	env, err := b.invoke(ctx, pluginabi.MethodHostAuthList, struct{}{}, "host auth list")
	if err != nil {
		return nil, err
	}
	var resp hostAuthListResponse
	if len(env.Result) > 0 {
		if errDecode := json.Unmarshal(env.Result, &resp); errDecode != nil {
			return nil, fmt.Errorf("host auth list failed: undecodable response body")
		}
	}
	return resp.Files, nil
}

// AuthGet returns ONE auth record's stored JSON by its auth index.
//
// Unlike AuthList, which omits secrets, this returns the file as stored — key
// included. Callers must never put it, or any field of it, into a Log message,
// an error string, or a client response.
func (b *HostBridge) AuthGet(ctx context.Context, authIndex string) (pluginapi.HostAuthGetResponse, error) {
	var out pluginapi.HostAuthGetResponse
	if strings.TrimSpace(authIndex) == "" {
		return out, fmt.Errorf("host auth get failed: empty auth index")
	}
	env, err := b.invoke(ctx, pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex}, "host auth get")
	if err != nil {
		return out, err
	}
	if len(env.Result) > 0 {
		if errDecode := json.Unmarshal(env.Result, &out); errDecode != nil {
			return out, fmt.Errorf("host auth get failed: undecodable response body")
		}
	}
	return out, nil
}

// AuthSave persists one auth record through the host. The name must end with
// .json; the record is this plugin's own JSON.
func (b *HostBridge) AuthSave(ctx context.Context, name string, record []byte) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("host auth save failed: empty auth file name")
	}
	env, err := b.invoke(ctx, pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{
		Name: name,
		JSON: json.RawMessage(record),
	}, "host auth save")
	if err != nil {
		return err
	}
	_ = env
	return nil
}

func toHTTPHeader(headers map[string]string) http.Header {
	if len(headers) == 0 {
		return nil
	}
	out := make(http.Header, len(headers))
	for k, v := range headers {
		out.Set(k, v)
	}
	return out
}
