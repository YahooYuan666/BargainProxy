package anthropic

import (
	"strconv"
	"testing"
)

// BP-286: the thinking gear arrives in output_config.effort, not in
// thinking.budget_tokens.
//
// ZCode v3.15.1 (2026-10-10, zai-org/ZCode anthropic-effort-wire.test.ts)
// switched to this shape and that test asserts the request carries NEITHER
// budget_tokens NOR reasoning_effort. This gateway read only budget_tokens, so
// from that date every gear a user picked — low, max, anything — was rewritten
// to the "high" fallback before it ever reached a driver. Nothing logged it and
// output-length measurements could not see it (run-to-run noise is wider than
// the gear difference), which is why it survived unnoticed.
func TestEffortComesFromOutputConfig(t *testing.T) {
	cases := map[string]string{
		"low":    "low",
		"medium": "medium",
		"high":   "high",
		"xhigh":  "xhigh",
		"max":    "max",
		// Case and stray whitespace are the client's formatting, not a
		// different gear: normalize instead of falling through to the default.
		"MAX":   "max",
		" max ": "max",
		"High":  "high",
	}
	for sent, want := range cases {
		body := []byte(`{"model":"m","max_tokens":100,"thinking":{"type":"enabled"},
			"output_config":{"effort":"` + sent + `"},
			"messages":[{"role":"user","content":"hi"}]}`)
		req, err := ParseMessagesRequest(body)
		if err != nil {
			t.Fatalf("effort %q: parse: %v", sent, err)
		}
		if req.RequestedEffort != want {
			t.Errorf("effort %q -> RequestedEffort %q, want %q", sent, req.RequestedEffort, want)
		}
		if req.Thinking == nil || req.Thinking.Level != want {
			t.Errorf("effort %q -> Thinking.Level %v, want %q", sent, req.Thinking, want)
		}
	}
}

// output_config.effort outranks budget_tokens when a client sends both: it is
// the field the client actually chose, while the budget is often a leftover.
func TestOutputConfigOutranksBudget(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":100,
		"thinking":{"type":"enabled","budget_tokens":1000},
		"output_config":{"effort":"max"},
		"messages":[{"role":"user","content":"hi"}]}`)
	req, err := ParseMessagesRequest(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.RequestedEffort != "max" {
		t.Fatalf("RequestedEffort = %q, want max (budget_tokens 1000 would say low)", req.RequestedEffort)
	}
}

// Budget-only requests keep working: channels and older clients that speak
// budget must still get a sensible gear.
func TestBudgetStillDerivesGearWhenNoOutputConfig(t *testing.T) {
	cases := map[int]string{
		2000:  "low",
		8000:  "medium",
		30000: "high",
	}
	for budget, want := range cases {
		body := []byte(`{"model":"m","max_tokens":100,
			"thinking":{"type":"enabled","budget_tokens":` + strconv.Itoa(budget) + `},
			"messages":[{"role":"user","content":"hi"}]}`)
		req, err := ParseMessagesRequest(body)
		if err != nil {
			t.Fatalf("budget %d: parse: %v", budget, err)
		}
		if req.RequestedEffort != want {
			t.Errorf("budget %d -> %q, want %q", budget, req.RequestedEffort, want)
		}
	}
}

// output_config must no longer be reported as an unimplemented field — the
// warning that surfaced this whole bug would otherwise keep firing after the
// fix and train everyone to ignore it.
func TestOutputConfigIsNoLongerWarned(t *testing.T) {
	if knownRequestFields["output_config"] {
		return // the assertion that matters
	}
	t.Fatal("output_config is read now and must be in knownRequestFields")
}
