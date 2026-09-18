package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// modelLog emits a best-effort host.log line for the dynamic-model path.
// The path used to fail silently (every error branch fell back to the static
// table with no signal), which made the production gap between the live
// catalog (rates attached, server-driven metadata) and the static fallback
// undiagnosable. Log lines carry counts and error classes only — never tokens
// or account identifiers.
//
// Dedup: model.static / model.for_auth are re-invoked on every config reload
// and models query, so identical failures would flood main.log. Same-reason
// lines are throttled to one per 10 minutes.
var modelLogMu struct {
	sync.Mutex
	lastSeen map[string]time.Time
}

func modelLogf(reason, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	modelLogMu.Lock()
	if modelLogMu.lastSeen == nil {
		modelLogMu.lastSeen = map[string]time.Time{}
	}
	if t, ok := modelLogMu.lastSeen[reason]; ok && time.Since(t) < 10*time.Minute {
		modelLogMu.Unlock()
		return
	}
	modelLogMu.lastSeen[reason] = time.Now()
	modelLogMu.Unlock()

	req, _ := json.Marshal(map[string]any{
		"level":   "warn",
		"message": "qwenwork models: " + msg,
	})
	_, _ = hostCall(pluginabi.MethodHostLog, req)
}
