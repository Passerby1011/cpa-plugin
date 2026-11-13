package plugin

// Streaming host callbacks.
//
// Two distinct stream ids exist and they are NEVER interchangeable:
//   - the UPSTREAM id, allocated by the host when host.http.do_stream opens the
//     backend connection; it is read with host.http.stream_read and released
//     with host.http.stream_close.
//   - the DOWNSTREAM id, allocated by the host for executor.execute_stream; it
//     is written with host.stream.emit and terminated with host.stream.close.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"

	"github.com/hex-ci/cpa-plugin/geminiapi/internal/backend"
)

// HostStream is an open upstream stream.
type HostStream struct {
	// StatusCode is the backend's HTTP status.
	StatusCode int
	// Headers are the backend's response headers.
	Headers http.Header
	// ID is the UPSTREAM stream id.
	ID string
}

// hostStreamStartResp decodes host.http.do_stream's result.
type hostStreamStartResp struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

// hostStreamReadResp decodes one host.http.stream_read result.
type hostStreamReadResp struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// DoStream opens an upstream stream. The returned error means the connection
// never opened, which is the failover window: nothing has been emitted yet.
func (b *HostBridge) DoStream(ctx context.Context, req backend.HTTPRequest) (HostStream, error) {
	if b == nil || b.call == nil {
		return HostStream{}, fmt.Errorf("host transport is unavailable")
	}
	payload, err := json.Marshal(hostHTTPReq{
		Method:  req.Method,
		URL:     req.URL,
		Headers: req.Headers,
		Body:    req.Body,
	})
	if err != nil {
		return HostStream{}, fmt.Errorf("encode host stream request: %w", err)
	}
	raw, err := b.callWithTimeout(ctx, pluginabi.MethodHostHTTPDoStream, payload, callbackTimeout)
	if err != nil {
		return HostStream{}, err
	}
	env, err := decodeEnvelope(raw)
	if err != nil {
		return HostStream{}, err
	}
	if !env.OK {
		msg := "host http do_stream failed"
		if env.Error != nil && env.Error.Message != "" {
			msg = "host http do_stream failed: " + truncate(env.Error.Message, 200)
		}
		return HostStream{}, fmt.Errorf("%s", msg)
	}
	var out hostStreamStartResp
	if len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, &out); err != nil {
			return HostStream{}, fmt.Errorf("host http do_stream failed: undecodable response")
		}
	}
	return HostStream{StatusCode: out.StatusCode, Headers: out.Headers, ID: out.StreamID}, nil
}

// StreamRead reads the next upstream chunk. done reports a clean end of stream;
// errMsg carries the upstream's own failure label; err is a transport failure.
//
// No deadline is imposed here: blocking is governed by the upstream's close
// semantics and by the caller's idle guard, which closes the stream when it
// trips. That is what keeps a legitimately slow first token from being killed
// by an arbitrary read timeout.
func (b *HostBridge) StreamRead(streamID string) (payload []byte, errMsg string, done bool, err error) {
	if b == nil || b.call == nil {
		return nil, "", false, fmt.Errorf("host transport is unavailable")
	}
	payloadBytes, mErr := json.Marshal(struct {
		StreamID string `json:"stream_id"`
	}{streamID})
	if mErr != nil {
		return nil, "", false, fmt.Errorf("encode stream read: %w", mErr)
	}
	raw, callErr := b.callWithTimeout(context.Background(), pluginabi.MethodHostHTTPStreamRead, payloadBytes, 0)
	if callErr != nil {
		return nil, "", false, callErr
	}
	env, decErr := decodeEnvelope(raw)
	if decErr != nil {
		return nil, "", false, decErr
	}
	if !env.OK {
		return nil, "", false, fmt.Errorf("host http stream_read failed")
	}
	var out hostStreamReadResp
	if len(env.Result) > 0 {
		if uErr := json.Unmarshal(env.Result, &out); uErr != nil {
			return nil, "", false, fmt.Errorf("host http stream_read failed: undecodable response")
		}
	}
	return out.Payload, out.Error, out.Done, nil
}

// StreamClose releases an UPSTREAM stream.
func (b *HostBridge) StreamClose(streamID string) error {
	if b == nil || b.call == nil || streamID == "" {
		return nil
	}
	payload, err := json.Marshal(struct {
		StreamID string `json:"stream_id"`
	}{streamID})
	if err != nil {
		return fmt.Errorf("encode stream close: %w", err)
	}
	_, callErr := b.callWithTimeout(context.Background(), pluginabi.MethodHostHTTPStreamClose, payload, 0)
	return callErr
}

// Emit writes one chunk to the DOWNSTREAM stream, bounded so a wedged consumer
// surfaces as an error rather than parking the pump.
func (b *HostBridge) Emit(downstreamID string, payload []byte) error {
	if b == nil || b.call == nil || downstreamID == "" {
		return fmt.Errorf("downstream stream id is missing")
	}
	raw, err := json.Marshal(struct {
		StreamID string `json:"stream_id"`
		Payload  []byte `json:"payload"`
	}{downstreamID, payload})
	if err != nil {
		return fmt.Errorf("encode stream emit: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), streamTimeout)
	defer cancel()
	_, callErr := b.callWithTimeout(ctx, pluginabi.MethodHostStreamEmit, raw, streamTimeout)
	return callErr
}

// CloseDownstream terminates the DOWNSTREAM stream, optionally with a redacted
// error label.
func (b *HostBridge) CloseDownstream(downstreamID string, errMsg string) error {
	if b == nil || b.call == nil || downstreamID == "" {
		return nil
	}
	raw, err := json.Marshal(struct {
		StreamID string `json:"stream_id"`
		Error    string `json:"error,omitempty"`
	}{downstreamID, errMsg})
	if err != nil {
		return fmt.Errorf("encode downstream close: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), streamTimeout)
	defer cancel()
	_, callErr := b.callWithTimeout(ctx, pluginabi.MethodHostStreamClose, raw, streamTimeout)
	return callErr
}

// noDeadline is a huge budget for calls that are governed by the upstream
// itself; context.Background() carries no deadline, and this only guards
// against a host that never returns at all.
const noDeadline = 24 * time.Hour
