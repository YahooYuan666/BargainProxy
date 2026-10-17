package ir

import "testing"

func TestNormalizeToolChoiceDialects(t *testing.T) {
	cases := []struct {
		name        string
		in          any
		wantChoice  any
		wantName    string
		wantDisable bool
	}{
		{"openai-string-required", "required", "required", "", false},
		{"openai-string-auto", "auto", "auto", "", false},
		{"anthropic-any", map[string]any{"type": "any"}, "required", "", false},
		{"anthropic-tool", map[string]any{"type": "tool", "name": "get_weather"}, "required", "get_weather", false},
		{"anthropic-disable", map[string]any{"type": "auto", "disable_parallel_tool_use": true}, "auto", "", true},
		{"chat-object", map[string]any{"type": "function", "function": map[string]any{"name": "f"}}, "required", "f", false},
		{"responses-flat", map[string]any{"type": "function", "name": "g"}, "required", "g", false},
		{"nil", nil, nil, "", false},
	}
	for _, tc := range cases {
		choice, name, disable := NormalizeToolChoice(tc.in)
		if choice != tc.wantChoice || name != tc.wantName || disable != tc.wantDisable {
			t.Fatalf("%s: got (%v,%q,%v), want (%v,%q,%v)",
				tc.name, choice, name, disable, tc.wantChoice, tc.wantName, tc.wantDisable)
		}
	}
}

func TestBudgetFromLevelMapsIntoLevelBands(t *testing.T) {
	cases := map[string]int{"minimal": 1024, "low": 2048, "medium": 8192, "high": 16384, "xhigh": 32768, "max": 32768, "": 0, "bogus": 0}
	for level, want := range cases {
		if got := BudgetFromLevel(level); got != want {
			t.Fatalf("BudgetFromLevel(%q) = %d, want %d", level, got, want)
		}
	}
	for level, budget := range map[string]int{"low": 2048, "medium": 8192, "high": 16384} {
		if got := LevelFromBudget(budget); got != level {
			t.Fatalf("LevelFromBudget(%d) = %q, want %q (inverse must round-trip)", budget, got, level)
		}
	}
}
