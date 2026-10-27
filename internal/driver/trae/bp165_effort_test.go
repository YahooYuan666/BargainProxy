package trae

import "testing"

// BP-165: Trae's client names five levels; medium and max must pass through
// as themselves instead of being folded into high/extra_high (plugins trae
// 0.2.23, issue #434 — DeepSeek V4.1 Flash offers max in the app).
func TestBP165FiveEffortLevels(t *testing.T) {
	cases := map[string]string{
		"minimal": "light",
		"low":     "light",
		"medium":  "medium",
		"high":    "high",
		"xhigh":   "extra_high",
		"max":     "max",
		"off":     "",
		"none":    "",
		"":        "",
	}
	for in, want := range cases {
		if got := traeEffort(in); got != want {
			t.Errorf("traeEffort(%q) = %q, want %q", in, got, want)
		}
	}
}
