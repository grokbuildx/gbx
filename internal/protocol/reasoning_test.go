package protocol

import "testing"

func TestEffortRankAndClampEffort(t *testing.T) {
	if EffortRank("none") >= EffortRank("minimal") ||
		EffortRank("minimal") >= EffortRank("low") ||
		EffortRank("low") >= EffortRank("medium") ||
		EffortRank("medium") >= EffortRank("high") ||
		EffortRank("high") >= EffortRank("xhigh") ||
		EffortRank("xhigh") >= EffortRank("max") ||
		EffortRank("max") >= EffortRank("ultra") {
		t.Fatal("EffortRank ordering invariant violated")
	}
	if EffortRank("bogus") != -1 {
		t.Fatalf("EffortRank(bogus) = %d, want -1", EffortRank("bogus"))
	}

	cases := []struct {
		name    string
		desired string
		allowed []string
		want    string
	}{
		{"exact preserves casing", "HIGH", []string{"Low", "High", "Max"}, "High"},
		{"ultra clamps down to xhigh", "ultra", []string{"low", "medium", "high", "xhigh"}, "xhigh"},
		{"max clamps down to high", "max", []string{"low", "medium", "high"}, "high"},
		{"medium clamps down to low when absent", "medium", []string{"low", "high", "max"}, "low"},
		{"minimal clamps up to low", "minimal", []string{"low", "medium", "high"}, "low"},
		{"unrecognized returns empty", "turbo", []string{"low", "high"}, ""},
		{"empty allowed returns empty", "high", nil, ""},
	}
	for _, tc := range cases {
		if got := ClampEffort(tc.desired, tc.allowed); got != tc.want {
			t.Errorf("%s: ClampEffort(%q, %v) = %q, want %q", tc.name, tc.desired, tc.allowed, got, tc.want)
		}
	}

	levels := []ReasoningLevel{{Effort: "low"}, {Effort: "high"}, {Effort: "max"}}
	if got := ClampReasoningLevel("ultra", levels); got != "max" {
		t.Fatalf("ClampReasoningLevel(ultra) = %q, want max", got)
	}
	if got := ChooseDefaultEffort(levels, "max"); got != "max" {
		t.Fatalf("ChooseDefaultEffort(preferred=max) = %q, want max", got)
	}
	if got := ChooseDefaultEffort(levels, ""); got != "high" {
		t.Fatalf("ChooseDefaultEffort(no medium) = %q, want high", got)
	}
	if got := ChooseDefaultEffort([]ReasoningLevel{{Effort: "low"}, {Effort: "medium"}, {Effort: "high"}}, ""); got != "medium" {
		t.Fatalf("ChooseDefaultEffort(with medium) = %q, want medium", got)
	}
}
