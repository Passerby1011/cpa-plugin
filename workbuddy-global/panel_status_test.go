package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The panel speaks for a fleet, and one degraded account is enough to say so.
// The previous revision softened the stale wording when the only failure was
// the optional models.dev enrichment; that source is gone, so `stale` has
// exactly one meaning again (a catalogue refresh failed) and one message.
func TestModelStatusMessagePerState(t *testing.T) {
	cases := []struct {
		name      string
		state     modelReadinessState
		wantExact string
	}{
		{name: "ready", state: modelReady, wantExact: "模型目录已就绪"},
		{name: "stale keeps the refresh wording", state: modelStale, wantExact: "模型目录刷新失败，正在使用上次有效缓存"},
		{name: "failed reports unusable", state: modelFailed, wantExact: "模型目录不可用"},
		{name: "loading", state: modelLoading, wantExact: "模型目录正在初始化"},
		{name: "not started", state: modelNotStarted, wantExact: "模型目录尚未初始化"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelStatusMessages[tc.state]; got != tc.wantExact {
				t.Fatalf("modelStatusMessages[%q] = %q, want %q", tc.state, got, tc.wantExact)
			}
		})
	}
}

// A genuinely stale catalogue must be reported as such even when another
// account is perfectly ready: softening that would hide a real upstream
// outage behind a healthy sibling.
func TestBuildModelStatusKeepsStaleWordingInAMixedFleet(t *testing.T) {
	runtime := installModelStatesForTest(t, map[string]modelReadinessState{
		"internal-ready": modelReady,
		"internal-stale": modelStale,
	})
	for authID, source := range map[string]modelSnapshotSource{
		"internal-ready": modelSourceFresh,
		"internal-stale": modelSourceCache,
	} {
		snapshot := runtime.snapshotForAuthID(authID)
		snapshot.ModelSource = source
		runtime.authSlot(authID).current.Store(&snapshot)
	}

	got := buildModelStatus(nil)
	if got.State != modelNotStarted {
		t.Fatalf("no auth files must read as not started, got %#v", got)
	}

	got = buildModelStatus([]pluginapi.HostAuthFileEntry{
		{ID: "internal-ready", AuthIndex: "account-1"},
		{ID: "internal-stale", AuthIndex: "account-2"},
	})
	if got.State != modelStale || got.Message != modelStatusMessages[modelStale] {
		t.Fatalf("mixed fleet status = %#v", got)
	}
}
