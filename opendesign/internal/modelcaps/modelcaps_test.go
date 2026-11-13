package modelcaps

import "testing"

// The seed is transcribed from magpie's models.mjs. This test pins every field,
// because a transcription error here silently changes what a client is told about
// a model's limits.
func TestSeedMatchesTheSource(t *testing.T) {
	want := map[string]SeedModel{
		"claude-haiku-5.5":             {ID: "claude-haiku-5.5", Name: "Claude Haiku 5.5", Context: 1000000, Output: 132000, Input: []string{"image", "file"}, Cost: "low", Capability: "standard", Efforts: []string{"low", "high", "max"}},
		"deepseek-v4-flash":            {ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash", Context: 1048000, Output: 232000, Cost: "medium", Capability: "standard", Efforts: []string{"low", "high", "max"}},
		"deepseek-v4-flash-vision-exp": {ID: "deepseek-v4-flash-vision-exp", Name: "DeepSeek V4 Flash Vision (exp)", Context: 1048000, Output: 232000, Input: []string{"image"}, Cost: "medium", Capability: "standard", Efforts: []string{"low", "high", "max"}},
		"deepseek-v4-pro":              {ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro", Context: 1048000, Output: 232000, Cost: "high", Capability: "advanced", Efforts: []string{"low", "high", "max"}},
		"deepseek-v4.1-flash":          {ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", Context: 1048000, Output: 232000, Input: []string{"image"}, Cost: "low", Capability: "advanced", Efforts: []string{"low", "high", "max"}},
		"deepseek-v4.1-flash-eco":      {ID: "deepseek-v4.1-flash-eco", Name: "DeepSeek V4.1 Flash Eco", Context: 1048000, Output: 232000, Input: []string{"image"}, Cost: "low", Capability: "advanced", Efforts: []string{"low", "high", "max"}},
		"glm-5.3-flash":                {ID: "glm-5.3-flash", Name: "GLM 5.3 Flash", Context: 1048000, Output: 232000, Input: []string{"image", "video"}, Cost: "low", Capability: "advanced", Efforts: []string{"low", "high", "max"}},
		"glm-5.3-flashx":               {ID: "glm-5.3-flashx", Name: "GLM 5.3 FlashX", Context: 1000000, Output: 232000, Input: []string{"image", "file", "video"}, Cost: "medium", Capability: "advanced", Efforts: []string{"low", "high", "max"}},
		"gpt-6-luna":                   {ID: "gpt-6-luna", Name: "GPT-6 Luna", Context: 1050000, Output: 304000, Input: []string{"image", "file"}, Cost: "low", Capability: "standard", Efforts: []string{}},
		"kimi-k2.7-code":               {ID: "kimi-k2.7-code", Name: "Kimi K2.7 Code", Context: 262000, Output: 262000, Input: []string{"image"}, Cost: "medium", Capability: "advanced", Efforts: []string{"low", "high", "max"}},
		"mimo-v2.6-flash":              {ID: "mimo-v2.6-flash", Name: "MiMo V2.6 Flash", Context: 1048000, Output: 232000, Input: []string{"image", "audio", "video"}, Cost: "low", Capability: "standard", Efforts: []string{"low", "high", "max"}},
		"mimo-v2.6-pro":                {ID: "mimo-v2.6-pro", Name: "MiMo V2.6 Pro", Context: 1048000, Output: 232000, Input: []string{"image", "audio", "video"}, Cost: "medium", Capability: "advanced", Efforts: []string{"low", "high", "max"}},
	}
	if len(Seed) != len(want) {
		t.Fatalf("seed has %d entries, want %d", len(Seed), len(want))
	}
	for _, got := range Seed {
		w, ok := want[got.ID]
		if !ok {
			t.Errorf("unexpected seed entry %q", got.ID)
			continue
		}
		if got.Name != w.Name || got.Context != w.Context || got.Output != w.Output {
			t.Errorf("%s: name/limits = %q/%d/%d, want %q/%d/%d", got.ID, got.Name, got.Context, got.Output, w.Name, w.Context, w.Output)
		}
		if got.Cost != w.Cost || got.Capability != w.Capability {
			t.Errorf("%s: words = %q/%q, want %q/%q", got.ID, got.Cost, got.Capability, w.Cost, w.Capability)
		}
		if !sameStrings(got.Input, w.Input) {
			t.Errorf("%s: input = %v, want %v", got.ID, got.Input, w.Input)
		}
		if !sameStrings(got.Efforts, w.Efforts) {
			t.Errorf("%s: efforts = %v, want %v", got.ID, got.Efforts, w.Efforts)
		}
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The per-model wire is measured, not guessed: two models think longer on
// Responses and glm-5.3-flash only thinks on Anthropic's messages.
func TestDeclaredAPIPerModel(t *testing.T) {
	cases := map[string]API{
		"claude-haiku-5.5":            APIResponses,
		"deepseek-v4-pro":             APIResponses,
		"glm-5.3-flash":               APIMessages,
		"gpt-6-luna":                  APIChatCompletions,
		"kimi-k2.7-code":              APIChatCompletions,
		"deepseek-v4.1-flash":         APIChatCompletions,
		"a-model-the-table-never-saw": APIChatCompletions,
	}
	for id, want := range cases {
		if got := APIFor(id); got != want {
			t.Errorf("APIFor(%q) = %q, want %q", id, got, want)
		}
	}
}

// The output cap is what amr-link actually takes, not what its catalog publishes.
func TestOutputCapClampsWhatTheAPIRefuses(t *testing.T) {
	cases := []struct {
		id   string
		in   int64
		want int64
	}{
		{"glm-5.3-flash", 232000, 131072}, // the catalog's number is refused
		{"glm-5.3-flash", 132000, 131072}, // one over the measured cap
		{"glm-5.3-flash", 131072, 131072}, // exactly the cap
		{"glm-5.3-flash", 100000, 100000}, // a catalog that says less is taken as it says
		{"gemini-3.8-flash", 264000, 65536},
		{"gemini-3.8-flash", 65537, 65536},
		{"deepseek-v4.1-flash", 232000, 232000}, // no cap for this one
		{"glm-5.3-flash", 0, 0},                 // nothing declared stays nothing
	}
	for _, tc := range cases {
		if got := CapOutput(tc.id, tc.in); got != tc.want {
			t.Errorf("CapOutput(%q, %d) = %d, want %d", tc.id, tc.in, got, tc.want)
		}
	}
}

// The plan's multipliers are the vendor's own table, and a model the table does
// not name falls back to its catalog cost word.
func TestRatesAndTheCostWordFallback(t *testing.T) {
	cases := []struct {
		id   string
		cost string
		want float64
	}{
		{"claude-haiku-5.5", "low", 2}, // the catalog says "low"; the plan charges 2×
		{"gpt-6-luna", "low", 4},
		{"deepseek-v4.1-flash", "low", 0.5},
		{"deepseek-v4.1-flash-eco", "low", 0.27},
		{"brand-new-9", "high", 2}, // not in the table: the word decides
		{"brand-new-9", "very_high", 4},
		{"brand-new-9", "", 0}, // nothing known
	}
	for _, tc := range cases {
		if got := RateOf(tc.id, tc.cost); got != tc.want {
			t.Errorf("RateOf(%q, %q) = %v, want %v", tc.id, tc.cost, got, tc.want)
		}
	}
}
