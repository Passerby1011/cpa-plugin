package thinking

import "testing"

// The level space this plugin publishes is the whole canonical one, so a client
// asking for "medium" reaches the nearest level a model DOES declare.
func TestCanonicalLevelOrder(t *testing.T) {
	want := []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
	if len(CanonicalLevels) != len(want) {
		t.Fatalf("levels = %d, want %d", len(CanonicalLevels), len(want))
	}
	for i := range want {
		if CanonicalLevels[i] != want[i] {
			t.Errorf("level[%d] = %q, want %q", i, CanonicalLevels[i], want[i])
		}
	}
	// "auto" is a dynamic sentinel, not a rankable level: publishing it would
	// invite a client to ask for a rank the mapping cannot place.
	for _, l := range CanonicalLevels {
		if l == "auto" {
			t.Errorf("auto must not be in the canonical level space")
		}
	}
}

// NormalizeLevels is a FILTER, not a fallback: a model that declares nothing
// gets an empty list, which every caller reads as "no level vocabulary".
func TestNormalizeLevelsFiltersInCanonicalOrder(t *testing.T) {
	// Declared out of order and with a foreign spelling: the result is in
	// canonical order and lowercased.
	got := NormalizeLevels([]string{"MAX", "Low", " high "})
	want := []string{"low", "high", "max"}
	if !sameList(got, want) {
		t.Errorf("NormalizeLevels = %v, want %v", got, want)
	}
	// A level outside the canonical space is dropped, not invented.
	if got := NormalizeLevels([]string{"turbo"}); len(got) != 0 {
		t.Errorf("an unknown level survived: %v", got)
	}
	// Nothing declared stays nothing — this is GPT-6 Luna's measured shape.
	if got := NormalizeLevels(nil); len(got) != 0 {
		t.Errorf("nil levels = %v, want empty", got)
	}
}

// BudgetToLevel uses the SDK's fixed thresholds, never interpolation.
func TestBudgetToLevelThresholds(t *testing.T) {
	cases := []struct {
		budget int64
		want   string
	}{
		{1, "minimal"},
		{512, "minimal"}, // exactly the minimal limit
		{513, "low"},
		{1024, "low"},
		{1025, "medium"},
		{8192, "medium"},
		{8193, "high"},
		{24576, "high"},
		{24577, "xhigh"},
		{128000, "xhigh"}, // max is never derived from a budget
	}
	for _, tc := range cases {
		if got := BudgetToLevel(tc.budget); got != tc.want {
			t.Errorf("BudgetToLevel(%d) = %q, want %q", tc.budget, got, tc.want)
		}
	}
}

// A level a model does not declare clamps to the NEAREST declared one, and a tie
// prefers the weaker level.
func TestClampToSupported(t *testing.T) {
	levels := []string{"low", "high", "max"}
	cases := []struct {
		level string
		want  string
	}{
		{"low", "low"},
		{"LOW", "low"}, // case-insensitive match on a declared level
		{"high", "high"},
		{"max", "max"},
		{"minimal", "low"}, // below the weakest declared level
		{"medium", "low"},  // equidistant from low and high: the weaker wins
		{"xhigh", "high"},  // nearest is high
		{"none", "low"},    // "off" is not declared, so the weakest declared level
	}
	for _, tc := range cases {
		if got := ClampToSupported(tc.level, levels); got != tc.want {
			t.Errorf("ClampToSupported(%q) = %q, want %q", tc.level, got, tc.want)
		}
	}
	// No declared levels means nothing to clamp to: the value passes through so
	// the caller decides whether to send the control at all.
	if got := ClampToSupported("medium", nil); got != "medium" {
		t.Errorf("ClampToSupported with no levels = %q, want the value unchanged", got)
	}
}

// EffortFromBudget: a model with no levels always answers "", which is what keeps
// GPT-6 Luna from being handed a reasoning_effort it ignores.
func TestEffortFromBudget(t *testing.T) {
	cases := []struct {
		name   string
		budget int64
		levels []string
		want   string
	}{
		{"no levels at all", 8192, nil, ""},
		{"no levels, even off", 0, nil, ""},
		{"declared low/high/max, small budget", 1024, []string{"low", "high", "max"}, "low"},
		{"declared low/high/max, mid budget", 8192, []string{"low", "high", "max"}, "low"},
		{"declared low/high/max, big budget", 24576, []string{"low", "high", "max"}, "high"},
		{"declared low/high/max, hugest budget", 128000, []string{"low", "high", "max"}, "max"},
		{"declared low/high/max, exactly low's budget", 1024, []string{"low", "high", "max"}, "low"},
		{"declared low/high/max, exactly high's budget", 24576, []string{"low", "high", "max"}, "high"},
		{"declared low/high/max, one over max's budget", 128001, []string{"low", "high", "max"}, "high"},
		{"max not declared: 128000 clamps to high", 128000, []string{"low", "high"}, "high"},
		{"off with no none level", 0, []string{"low", "high", "max"}, ""},
		{"off with a none level", 0, []string{"none", "low", "high", "max"}, "none"},
	}
	for _, tc := range cases {
		if got := EffortFromBudget(tc.budget, tc.levels); got != tc.want {
			t.Errorf("%s: EffortFromBudget(%d) = %q, want %q", tc.name, tc.budget, got, tc.want)
		}
	}
}

// BudgetFromEffort is the inverse direction, and ok is false when the level cannot
// be expressed so the caller refuses rather than silently dropping the control.
func TestBudgetFromEffort(t *testing.T) {
	levels := []string{"low", "high", "max"}
	cases := []struct {
		effort string
		levels []string
		want   int64
		ok     bool
	}{
		{"low", levels, 1024, true},
		{"high", levels, 24576, true},
		{"max", levels, 128000, true},
		{"medium", levels, 1024, true}, // clamped to low, whose budget is 1024
		{"xhigh", levels, 24576, true}, // clamped to high
		{"none", levels, 0, true},      // reasoning off is expressible as 0
		{"", levels, 0, false},         // nothing asked
		{"nonsense", levels, 0, false}, // no such canonical level
	}
	for _, tc := range cases {
		gott, ok := BudgetFromEffort(tc.effort, tc.levels)
		if ok != tc.ok || gott != tc.want {
			t.Errorf("BudgetFromEffort(%q) = (%d, %v), want (%d, %v)", tc.effort, gott, ok, tc.want, tc.ok)
		}
	}
	// No declared levels: the upstream is the only authority, so a canonical
	// level's table value is forwarded as-is.
	if budget, ok := BudgetFromEffort("max", nil); !ok || budget != 128000 {
		t.Errorf("BudgetFromEffort(max, no levels) = (%d, %v)", budget, ok)
	}
	if _, ok := BudgetFromEffort("nonsense", nil); ok {
		t.Errorf("an unknown level must not be forwarded with no levels declared")
	}
}

// ThinkingLevels is what the plugin publishes; a model with no levels publishes
// none.
func TestThinkingLevelsPublishesOnlyDeclaredOnes(t *testing.T) {
	if got := ThinkingLevels([]string{"low", "high", "max"}); !sameList(got, []string{"low", "high", "max"}) {
		t.Errorf("ThinkingLevels = %v", got)
	}
	if got := ThinkingLevels(nil); len(got) != 0 {
		t.Errorf("ThinkingLevels(nil) = %v, want empty", got)
	}
}

func sameList(a, b []string) bool {
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
