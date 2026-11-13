// Package plugin — the streaming pump's frame handling.
package plugin

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/hex-ci/cpa-plugin/opendesign/internal/protocol"
)

// feedAndEmit decodes one upstream frame and emits the frames the client's
// schema needs. finished reports that the upstream signalled the end of the
// response, so the pump can stop reading.
func (m *Manager) feedAndEmit(downID string, conv *streamConverter, model string, raw []byte) (bool, error) {
	if conv == nil {
		return false, fmt.Errorf("no stream converter")
	}
	var finished bool
	for _, frame := range splitFrames(raw) {
		if protocol.IsDoneFrame(frame) {
			finished = true
			continue
		}
		deltas, errDecode := conv.decoder.Feed(frame)
		if errDecode != nil {
			// A decoder error is the upstream reporting a failure (an error
			// event) — it ends the stream, and its text is already redacted.
			return true, splitFrameError(sanitizeError(errDecode))
		}
		for _, d := range deltas {
			// The first delta of any stream opens the response, and it is
			// opened here rather than by the encoder so every schema gets a
			// start frame even when the upstream never sent one.
			if !conv.started && (d.Kind == protocol.DeltaStart || d.Kind != protocol.DeltaStop) {
				if err := m.emitAll(downID, conv.encoder.Start(d.ID, model, d.CreatedAt)); err != nil {
					return true, splitFrameError(sanitizeError(err))
				}
				conv.started = true
			}
			switch d.Kind {
			case protocol.DeltaStop:
				conv.stop = d.StopReason
				continue
			case protocol.DeltaUsage:
				if d.Usage != nil {
					conv.usage = d.Usage
				}
				continue
			}
			if err := m.emitAll(downID, conv.encoder.Emit(d)); err != nil {
				return true, splitFrameError(sanitizeError(err))
			}
		}
	}
	return finished, nil
}

// splitFrames separates the frames in one read.
//
// The host hands back whatever byte chunk arrived, which may be half a frame, one
// frame, or several; and a Claude or Responses frame is a named event that spans
// two lines. So a chunk is split on the SSE blank-line separator, and a single
// "event:"/"data:" pair is kept together — splitting it would deliver the event
// name and its payload as two unknown frames.
func splitFrames(raw []byte) [][]byte {
	chunk := bytes.TrimSpace(raw)
	if len(chunk) == 0 {
		return nil
	}
	// A single bare JSON object (an unframed upstream, or a test's frame).
	if chunk[0] == '{' && !bytes.Contains(chunk, []byte("\n\n")) {
		return [][]byte{chunk}
	}
	parts := bytes.Split(chunk, []byte("\n\n"))
	out := make([][]byte, 0, len(parts))
	for _, part := range parts {
		trimmed := bytes.TrimSpace(part)
		if len(trimmed) == 0 {
			continue
		}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return [][]byte{chunk}
	}
	return out
}

// emitAll feeds converted frames downstream in order. StreamEmit is
// deadline-bounded, so a host that stops draining surfaces here as an error.
func (m *Manager) emitAll(downStreamID string, frames [][]byte) error {
	for _, frame := range frames {
		if len(frame) == 0 {
			continue
		}
		if err := m.bridge.StreamEmit(downStreamID, frame); err != nil {
			return err
		}
	}
	return nil
}

// sanitizeError turns a transport error into text safe for the downstream stream
// close. It keeps the shape of the failure and drops anything that could be a
// credential.
func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	return sanitizeMessage(err.Error())
}

// sanitizeMessage removes the key-shaped substrings a message can pick up.
//
// A key reaching a downstream close reason is the worst case for this plugin:
// the reason travels to the client and into the host's logs, so a token that
// arrives in an upstream error body must not survive the trip.
func sanitizeMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	for _, prefix := range []string{"sk-amr-", "vela_ctrl_"} {
		for {
			idx := strings.Index(message, prefix)
			if idx < 0 {
				break
			}
			end := idx + len(prefix)
			for end < len(message) && isKeyChar(message[end]) {
				end++
			}
			message = message[:idx] + prefix + "…" + message[end:]
		}
	}
	if len(message) > 300 {
		message = message[:300] + "…"
	}
	return message
}

// splitFrameError turns a redacted message into an error the pump can return.
func splitFrameError(message string) error {
	if message == "" {
		return nil
	}
	return fmt.Errorf("%s", message)
}

func isKeyChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '-', b == '_':
		return true
	}
	return false
}
