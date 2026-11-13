// Package plugin — the executor.
//
// One execution: resolve the requested model against the account's served list,
// pick a credential, decode the client's body in the schema it arrived in, encode
// it for the model's own schema, send it, decode the answer back and encode it in
// the client's schema. The decoder/encoder pair is the protocol package's; this
// file owns credential selection, the wire, and failure classification.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/opendesign/internal/catalog"
	"github.com/hex-ci/cpa-plugin/opendesign/internal/config"
	"github.com/hex-ci/cpa-plugin/opendesign/internal/protocol"
)

// executorRequest mirrors rpcExecutorRequest: the SDK embeds
// pluginapi.ExecutorRequest untagged, so its fields marshal under Go field names.
// StreamID is the DOWNSTREAM host-allocated id for execute_stream emissions; ids
// returned by DoStream are UPSTREAM and never interchangeable.
type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID string `json:"stream_id,omitempty"`
}

// resolvedExecution is everything both execution paths need after resolution.
type resolvedExecution struct {
	cfg     config.Config
	entry   catalog.Entry
	account config.Account
	// clientSchema is the schema the request arrived in; upstreamSchema the one
	// it is sent in.
	clientSchema   protocol.Schema
	upstreamSchema protocol.Schema
	model          string
	lease          *lease
	pool           *poolState
	released       bool
	status         int
}

// requestedModel is the model name a synthesized response echoes: the client's
// own, so a downstream gateway comparing it against what it sent does not
// false-alarm.
func (r *resolvedExecution) requestedModel() string {
	if r == nil {
		return ""
	}
	if name := strings.TrimSpace(r.model); name != "" {
		return name
	}
	if name := strings.TrimSpace(r.entry.PublicID); name != "" {
		return name
	}
	return strings.TrimSpace(r.entry.UpstreamID)
}

// releaseAccount returns the credential to the pool it was taken from, with the
// cooldown its status earned. The once-guard matters: a request has several
// failure exits plus its completion path, and a double release would free another
// request's slot.
func (m *Manager) releaseAccount(res *resolvedExecution, status int) {
	if res == nil || res.released {
		return
	}
	res.released = true
	if res.pool == nil || res.lease == nil {
		return
	}
	res.pool.release(res.lease, status, time.Now())
}

// resolveExecution resolves the requested model and picks a credential. A
// non-nil second return is a ready-made failure envelope.
func (m *Manager) resolveExecution(req executorRequest) (*resolvedExecution, []byte) {
	m.mu.RLock()
	cfg, seed := m.cfg, m.seed
	m.mu.RUnlock()

	clientSchema := protocol.NormalizeSchema(req.SourceFormat)
	if clientSchema == "" {
		clientSchema = protocol.SchemaChat
	}
	if len(req.OriginalRequest) == 0 && len(req.Payload) == 0 {
		return nil, errorEnvelopeWithStatus("invalid_request", "the request carried no body", http.StatusBadRequest, false)
	}

	accounts := m.poolAccounts(cfg)
	if len(accounts) == 0 {
		return nil, errorEnvelopeWithStatus("auth_required",
			"opendesign has no credential: sign in with an Open Design account or add a sk-amr-… key in the plugin config",
			http.StatusUnauthorized, false)
	}

	// The served list is whichever account's snapshot covers this model. Every
	// snapshot was built from the same seed where the catalog said nothing, so
	// the first match is the right one.
	entry, found := m.lookupModel(req.Model, seed)
	if !found {
		return nil, errorEnvelopeWithStatus("model_not_found",
			fmt.Sprintf("model %q is not in the served catalog", req.Model), http.StatusNotFound, false)
	}
	lease := m.pool.acquire(accounts, cfg.Pool, time.Now())
	if lease == nil {
		return nil, errorEnvelopeWithStatus("no_account_available",
			"no opendesign credential is available: all are cooling down or at their concurrency limit",
			http.StatusServiceUnavailable, true)
	}
	return &resolvedExecution{
		cfg:            cfg,
		entry:          entry,
		account:        lease.account,
		clientSchema:   clientSchema,
		upstreamSchema: schemaForAPI(entry.API),
		model:          req.Model,
		lease:          lease,
		pool:           m.pool,
	}, nil
}

// poolAccounts is every credential the pool may use: the config's own, then any
// credential stored as an auth record.
func (m *Manager) poolAccounts(cfg config.Config) []config.Account {
	return m.accountsOf(cfg)
}

// lookupModel resolves a requested id against the served snapshots, falling back
// to the seed when no live snapshot covers it.
func (m *Manager) lookupModel(model string, seed *catalog.Snapshot) (catalog.Entry, bool) {
	m.mu.RLock()
	snapshots := make([]*catalog.Snapshot, 0, len(m.snapshots))
	for _, snap := range m.snapshots {
		snapshots = append(snapshots, snap)
	}
	m.mu.RUnlock()
	for _, snap := range snapshots {
		if entry, ok := snap.Lookup(model); ok {
			return entry, true
		}
	}
	if entry, ok := seed.Lookup(model); ok {
		return entry, true
	}
	return catalog.Entry{}, false
}

// ---------------------------------------------------------------------------
// non-streaming
// ---------------------------------------------------------------------------

// handleExecute implements executor.execute. A stream-flagged request is routed
// to the stream path rather than rejected.
func (m *Manager) handleExecute(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}
	if req.Stream {
		return m.executeStream(req)
	}
	res, failEnv := m.resolveExecution(req)
	if res == nil {
		return failEnv, nil
	}

	canonical, errDecode := decodeInbound(res.clientSchema, requestBodyOf(req))
	if errDecode != nil {
		m.releaseAccount(res, 0)
		return errorEnvelopeWithStatus("invalid_request", errDecode.Error(), http.StatusBadRequest, false), nil
	}
	canonical.Model = res.entry.UpstreamID
	canonical.Stream = false

	_, body, errBuild := buildUpstream(res.entry, canonical)
	if errBuild != nil {
		m.releaseAccount(res, 0)
		return errorEnvelopeWithStatus("invalid_request", errBuild.Error(), http.StatusBadRequest, false), nil
	}
	path := res.upstreamSchema.Path()
	url := endpointURL(res.cfg.BaseURL, res.upstreamSchema)
	headers := upstreamHeaders(res.upstreamSchema, path, res.account.RuntimeKey, false)

	ctx, cancel := context.WithTimeout(context.Background(), res.cfg.RequestTimeout)
	defer cancel()
	status, respHeaders, respBody, errDo := m.bridge.Do(ctx, http.MethodPost, url, headerMap(headers), body)
	if errDo != nil {
		m.releaseAccount(res, 0)
		return errorEnvelopeWithStatus("upstream_unavailable", errDo.Error(), http.StatusBadGateway, true), nil
	}
	m.releaseAccount(res, status)
	if status >= 400 {
		return upstreamFailure(status, respBody), nil
	}
	if int64(len(respBody)) > res.cfg.MaxResponseBytes {
		return errorEnvelopeWithStatus("response_too_large", "the upstream response exceeded max-response-bytes", http.StatusBadGateway, false), nil
	}
	converted, errConvert := convertResponse(res.upstreamSchema, res.clientSchema, respBody, res.requestedModel())
	if errConvert != nil {
		return errorEnvelopeWithStatus("translation_failed", errConvert.Error(), http.StatusBadGateway, false), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: converted, Headers: respHeaders}), nil
}

// requestBodyOf is the body a request is decoded from: the client's original
// bytes when the host passed them, else the translated payload.
//
// The original is preferred because the host may have already translated the
// client's body into another schema; decoding the original in the schema it
// actually arrived in is the only way to keep a field the host's translator
// dropped (a reasoning block, a tool-result shape).
func requestBodyOf(req executorRequest) []byte {
	if len(req.OriginalRequest) > 0 {
		return req.OriginalRequest
	}
	return req.Payload
}

// upstreamFailure renders an upstream error as a client-facing failure with a
// status the client's SDK can act on.
//
// The vendor's status vocabulary is translated where a plain pass-through would
// be misleading: 402 is its own payment wall and reads as "bad request" to an
// SDK, so it becomes the 429 a retrying client backs off on. 401/403 mean the
// account's key was refused, which is an auth failure here, not a bad request.
func upstreamFailure(status int, body []byte) []byte {
	message := protocol.UpstreamMessageText(body)
	switch {
	case status == 401 || status == 403:
		return errorEnvelopeWithStatus("auth_refused",
			declineMessage(status, message), http.StatusUnauthorized, false)
	case status == 402:
		return errorEnvelopeWithStatus("quota_exhausted",
			declineMessage(status, message), http.StatusTooManyRequests, false)
	case status == 429:
		return errorEnvelopeWithStatus("rate_limited",
			declineMessage(status, message), http.StatusTooManyRequests, true)
	case status >= 500:
		return errorEnvelopeWithStatus("upstream_error",
			declineMessage(status, message), http.StatusBadGateway, true)
	default:
		return errorEnvelopeWithStatus("upstream_rejected",
			declineMessage(status, message), http.StatusBadRequest, false)
	}
}

func declineMessage(status int, said string) string {
	if said = strings.TrimSpace(said); said != "" {
		return fmt.Sprintf("Open Design refused the request (%d): %s", status, said)
	}
	return fmt.Sprintf("Open Design refused the request (%d)", status)
}

// ---------------------------------------------------------------------------
// streaming
// ---------------------------------------------------------------------------

// handleExecuteStream implements executor.execute_stream.
func (m *Manager) handleExecuteStream(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor stream request body"), nil
	}
	return m.executeStream(req)
}

// executeStream opens the upstream stream and returns as soon as it is open, so
// the host can start draining chunks; the reading and emitting happens in a pump
// goroutine that outlives this call.
//
// Every pre-first-byte failure (decode, model, credential, upstream >= 400) still
// returns as an error envelope, so the host can fail over before it emits
// anything downstream.
func (m *Manager) executeStream(req executorRequest) ([]byte, error) {
	res, failEnv := m.resolveExecution(req)
	if res == nil {
		return failEnv, nil
	}
	canonical, errDecode := decodeInbound(res.clientSchema, requestBodyOf(req))
	if errDecode != nil {
		m.releaseAccount(res, 0)
		return errorEnvelopeWithStatus("invalid_request", errDecode.Error(), http.StatusBadRequest, false), nil
	}
	canonical.Model = res.entry.UpstreamID
	canonical.Stream = true

	_, body, errBuild := buildUpstream(res.entry, canonical)
	if errBuild != nil {
		m.releaseAccount(res, 0)
		return errorEnvelopeWithStatus("invalid_request", errBuild.Error(), http.StatusBadRequest, false), nil
	}
	path := res.upstreamSchema.Path()
	url := endpointURL(res.cfg.BaseURL, res.upstreamSchema)
	headers := upstreamHeaders(res.upstreamSchema, path, res.account.RuntimeKey, true)

	ctx, cancel := context.WithTimeout(context.Background(), res.cfg.RequestTimeout)
	defer cancel()
	status, upstreamID, errStream := m.bridge.DoStream(ctx, http.MethodPost, url, headerMap(headers), body)
	if errStream != nil {
		m.releaseAccount(res, 0)
		return errorEnvelopeWithStatus("upstream_unavailable", errStream.Error(), http.StatusBadGateway, true), nil
	}
	if status >= 400 {
		var payload []byte
		if upstreamID != "" {
			read, errMsg, _, errRead := m.bridge.StreamRead(upstreamID)
			if errRead == nil {
				payload = read
				if len(payload) == 0 && errMsg != "" {
					payload = []byte(errMsg)
				}
			}
			_ = m.bridge.StreamClose(upstreamID)
		}
		m.releaseAccount(res, status)
		return upstreamFailure(status, payload), nil
	}
	res.status = status
	if m.bridge != nil {
		m.bridge.inFlight.Add(1)
	}
	go func() {
		if m.bridge != nil {
			defer m.bridge.inFlight.Done()
		}
		m.pumpStream(req.StreamID, upstreamID, res)
	}()
	return okEnvelope(struct{}{}), nil
}

// streamConverter is the running pair of decoder and encoder for one stream.
type streamConverter struct {
	decoder protocol.StreamDecoder
	encoder protocol.StreamEncoder
	started bool
	stop    string
	// usage is the last accounting the upstream reported.
	usage *protocol.Usage
}

// pumpStream reads the upstream stream to its end, translating each frame and
// emitting the result downstream.
func (m *Manager) pumpStream(downID, upstreamID string, res *resolvedExecution) {
	defer m.releaseAccount(res, res.status)

	var closeOnce sync.Once
	closeStreams := func(errMsg string) {
		closeOnce.Do(func() {
			_ = m.bridge.StreamClose(upstreamID)
			_ = m.bridge.StreamCloseDownstream(downID, errMsg)
		})
	}
	defer closeStreams("")

	// The total-duration watchdog: the host's transport has no deadline of its
	// own for a stream, so a wedged upstream would otherwise hold the stream and
	// the credential lease forever.
	var aborted atomic.Bool
	watchdog := time.AfterFunc(res.cfg.RequestTimeout, func() {
		defer func() {
			if r := recover(); r != nil {
				_ = m.log("error", "stream watchdog panicked", nil)
			}
		}()
		aborted.Store(true)
		_ = m.bridge.StreamClose(upstreamID)
	})
	defer watchdog.Stop()

	conv := &streamConverter{
		decoder: protocol.NewStreamDecoder(res.upstreamSchema),
		encoder: protocol.NewStreamEncoder(res.clientSchema),
	}
	var total int64
	for {
		payload, readErrMsg, done, errRead := m.bridge.StreamRead(upstreamID)
		if aborted.Load() {
			closeStreams("the stream exceeded request-timeout")
			return
		}
		if errRead != nil {
			closeStreams(sanitizeError(errRead))
			return
		}
		if readErrMsg != "" {
			closeStreams(sanitizeMessage(readErrMsg))
			return
		}
		total += int64(len(payload))
		if total > res.cfg.MaxResponseBytes {
			closeStreams("the stream exceeded max-response-bytes")
			return
		}
		if len(payload) > 0 {
			finished, errEmit := m.feedAndEmit(downID, conv, res.requestedModel(), payload)
			if errEmit != nil {
				closeStreams(errEmit.Error())
				return
			}
			if finished {
				return
			}
		}
		if done {
			break
		}
	}
	// The upstream closed cleanly: close the client's response with whatever
	// stop reason the stream carried, defaulting to a normal end.
	stop := conv.stop
	if stop == "" {
		stop = protocol.StopEnd
	}
	if err := m.emitAll(downID, conv.encoder.Finish(stop, conv.usage)); err != nil {
		closeStreams(sanitizeError(err))
	}
}
