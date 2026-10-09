package plugin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestUsageFromJSONAcceptsWrappedAndBare pins the defensive decode: the vendor
// has returned the aggregate both wrapped ({"data":{...}}) and bare, and a
// decoder that only accepts one form silently drops every number.
func TestUsageFromJSONAcceptsWrappedAndBare(t *testing.T) {
	wrapped := []byte(`{"data":{"totalCount":12,"totalCost":3.5,"successRate":0.98,` +
		`"totalTokensIn":1000,"totalTokensOut":2000,"periodBasis":"current-period"}}`)
	bare := []byte(`{"totalCount":12,"totalCost":3.5,"successRate":0.98,` +
		`"totalTokensIn":1000,"totalTokensOut":2000,"periodBasis":"current-period"}`)

	for name, raw := range map[string][]byte{"wrapped": wrapped, "bare": bare} {
		got := usageFromJSON(raw)
		if got == nil {
			t.Fatalf("%s: usageFromJSON returned nil", name)
		}
		if got.TotalCount != 12 || got.TotalCost != 3.5 || got.SuccessRate != 0.98 {
			t.Errorf("%s: counts = %+v", name, *got)
		}
		if got.TotalTokensIn != 1000 || got.TotalTokensOut != 2000 {
			t.Errorf("%s: tokens = %+v", name, *got)
		}
		if got.PeriodBasis != "current-period" {
			t.Errorf("%s: periodBasis = %q", name, got.PeriodBasis)
		}
	}
}

// TestUsageFromJSONOmitsEmpty pins the "absent is not zero" rule: an empty or
// unparseable aggregate must yield nil so the panel renders no detail section
// at all, rather than a row of confident zeroes the vendor never reported.
func TestUsageFromJSONOmitsEmpty(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"data":{}}`),
		[]byte(`not json`),
		[]byte(`{"data":null}`),
	} {
		if got := usageFromJSON(raw); got != nil {
			t.Errorf("usageFromJSON(%s) = %+v, want nil", raw, *got)
		}
	}
}

// TestUsageFromJSONMissingFieldsStayUnset pins that an omitted field is not
// published as 0: TotalCost is absent here, so it must stay unset (and thus
// omitempty on the wire) while the fields that ARE present carry through.
func TestUsageFromJSONMissingFieldsStayUnset(t *testing.T) {
	got := usageFromJSON([]byte(`{"totalCount":5,"successRate":1,"periodBasis":"p"}`))
	if got == nil {
		t.Fatal("usageFromJSON returned nil for a non-empty summary")
	}
	if got.TotalCount != 5 {
		t.Errorf("totalCount = %v, want 5", got.TotalCount)
	}
	if got.TotalCost != 0 {
		t.Errorf("totalCost = %v, want 0 (absent)", got.TotalCost)
	}
	// And the absent field must not be serialised as a value at all.
	encoded, _ := json.Marshal(got)
	var back map[string]any
	_ = json.Unmarshal(encoded, &back)
	if _, present := back["total_cost"]; present {
		t.Errorf("absent totalCost was serialised: %s", encoded)
	}
	if _, present := back["total_count"]; !present {
		t.Errorf("present totalCount was dropped: %s", encoded)
	}
}

// TestFetchQuotaIncludesUsageAndToleratesItsFailure pins the wiring: the
// aggregate is fetched alongside the balances, and a failing aggregate must not
// take the balances down with it.
func TestFetchQuotaIncludesUsageAndToleratesItsFailure(t *testing.T) {
	const key = "usage-summary-secret"
	base := "https://usage.test"
	cases := []struct {
		name        string
		usageBody   string
		usageStatus int
		wantUsage   bool
		wantBalance float64
	}{
		{"usage present", `{"data":{"totalCount":9,"successRate":0.9,"periodBasis":"current-period"}}`, 200, true, 42},
		{"usage endpoint fails", `boom`, 500, false, 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
				if method != pluginabi.MethodHostHTTPDo {
					return hostOK(map[string]any{}), nil
				}
				var wire struct {
					URL string `json:"url"`
				}
				_ = json.Unmarshal(payload, &wire)
				switch {
				case strings.HasSuffix(wire.URL, accountCreditsPath):
					return hostOK(pluginapi.HTTPResponse{
						StatusCode: 200,
						Body:       []byte(`{"credits":{"monthlyCredits":42},"windowLimits":{"limited":true,"fiveHour":{"used":1,"cap":10},"weekly":{"used":2,"cap":20}}}`),
					}), nil
				case strings.HasSuffix(wire.URL, accountUsagePath):
					return hostOK(pluginapi.HTTPResponse{
						StatusCode: tc.usageStatus,
						Body:       []byte(tc.usageBody),
					}), nil
				default:
					return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"success":true}`)}), nil
				}
			}}
			m := NewManager(NewHostBridge(f.call))
			usage, _, _, err := fetchQuota(t.Context(), m.bridge, base+"/provider/v1", 0, key)
			if err != nil {
				t.Fatalf("fetchQuota: %v", err)
			}
			if usage.CreditsLeft != tc.wantBalance {
				t.Errorf("credits_left = %v, want %v (the balance must survive an aggregate failure)", usage.CreditsLeft, tc.wantBalance)
			}
			if tc.wantUsage {
				if usage.Usage == nil {
					t.Fatal("usage summary missing")
				}
				if usage.Usage.TotalCount != 9 || usage.Usage.SuccessRate != 0.9 {
					t.Errorf("usage = %+v", *usage.Usage)
				}
			} else if usage.Usage != nil {
				t.Errorf("usage = %+v, want nil when the endpoint failed", *usage.Usage)
			}
		})
	}
}
