// model_union_live_test.go is a live, opt-in check of the union path against
// the real upstream. It is skipped unless WORKBUDDY_LIVE_AUTH points at a real
// stored auth file, so it never runs in CI or during a normal `make test`.
//
// Why it exists: the unit tests pin the merge logic against synthetic bodies,
// but the shape of the real catalogue (which endpoint carries which model, and
// whether the non-chat filter removes anything a client could pick) can only be
// confirmed against the live endpoints.
//
// Usage:
//
//	WORKBUDDY_LIVE_AUTH=/path/to/workbuddy-<uid>.json \
//	  go test -run TestLiveWorkBuddyCatalogUnion -count=1 -v .
package main

import (
	"net/http"
	"os"
	"testing"
)

func TestLiveWorkBuddyCatalogUnion(t *testing.T) {
	path := os.Getenv("WORKBUDDY_LIVE_AUTH")
	if path == "" {
		t.Skip("set WORKBUDDY_LIVE_AUTH to a stored auth file to run the live check")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth: %v", err)
	}
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parse auth: %v", err)
	}

	do := func(req *http.Request, _ string) (*hostHTTPResponse, error) {
		// hostHTTPDoDirect performs a real request; the modelSource do hook has
		// the same signature the plugin uses through the host bridge.
		return hostHTTPDoDirect(req, nil)
	}
	catalog, err := fetchWorkBuddyCatalog(sa, "", do)
	if err != nil {
		t.Fatalf("fetch catalog: %v", err)
	}
	t.Logf("endpoint=%s realm=%s models=%d", catalog.Endpoint, catalog.Realm, len(catalog.Models))
	if len(catalog.Models) == 0 {
		t.Fatal("live catalogue is empty")
	}
	for _, m := range catalog.Models {
		t.Logf("  %-26s ctx=%-8v out=%-7v efforts=%v", m.ID, derefInt64(m.ContextLength), derefInt64(m.MaxCompletionTokens), m.ReasoningEfforts)
	}
}

func derefInt64(v *int64) any {
	if v == nil {
		return "-"
	}
	return *v
}
