// BP-112: starved-thinking rescue — a turn that spent its whole output budget
// on reasoning (finish=length, no text, no tools, nothing surfaced) gets ONE
// retry with thinking off at the SAME budget. Field basis: DeepSeek-family
// upstreams bill reasoning and answer against the same max_tokens with no
// separate thinking ceiling, so a small client budget can be eaten entirely by
// thinking (misakano starved.go, 2026-10-22 live: budgets 60-300 → 0 answer
// bytes; thinking:disabled at the same budget → normal answer).
package gateway

import (
	"testing"

	"bargainproxy/internal/ir"
)

func txtPart(s string) []ir.ContentPart { return []ir.ContentPart{{Kind: "text", Text: s}} }

func starvedResp() *ir.ChatResponse {
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, Content: txtPart("")},
		StopReason: ir.StopLength,
	}
}

func starvedReq() *ir.ChatRequest {
	return &ir.ChatRequest{
		Model:     "deepseek-v4.1-flash",
		MaxTokens: 100,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: txtPart("hi")}},
	}
}

// The headline shape: starved turn + declared budget + thinking not disabled
// → rescue, with thinking disabled and the effort gear REMOVED (an effort
// asks FOR thinking; keeping it beside disabled is contradictory).
func TestStarvedShapeTriggersRescue(t *testing.T) {
	req := starvedReq()
	req.Thinking = &ir.ThinkingConfig{Enabled: true, Level: "high"}
	req.RequestedEffort = "high"
	retry, ok := starvedRetryRequest(req, starvedResp())
	if !ok {
		t.Fatal("starved shape must trigger the rescue")
	}
	if retry.Thinking == nil || retry.Thinking.Enabled {
		t.Fatalf("rescue must disable thinking, got %+v", retry.Thinking)
	}
	if retry.RequestedEffort != "" {
		t.Fatalf("rescue must drop the effort gear (it asks FOR thinking), got %q", retry.RequestedEffort)
	}
	if retry.MaxTokens != req.MaxTokens {
		t.Fatalf("rescue must keep the client's budget: %d vs %d", retry.MaxTokens, req.MaxTokens)
	}
	if len(retry.Messages) != len(req.Messages) {
		t.Fatal("rescue must not touch the messages")
	}
}

// A normal answer is NOT starved: it produced content.
func TestProducedContentIsNotStarved(t *testing.T) {
	cases := map[string]*ir.ChatResponse{
		"has text": {
			Message:    ir.Message{Role: ir.RoleAssistant, Content: txtPart("an answer")},
			StopReason: ir.StopLength, // truncated, but it PRODUCED
		},
		"has tools": {
			Message: ir.Message{
				Role:      ir.RoleAssistant,
				ToolCalls: []ir.ToolCall{{ID: "c1", Name: "f", Arguments: "{}"}},
			},
			StopReason: ir.StopLength,
		},
		"surfaced thinking": {
			Message:    ir.Message{Role: ir.RoleAssistant, Content: txtPart("")},
			Thinking:   "partial reasoning the client saw",
			StopReason: ir.StopLength,
		},
		"normal stop": {
			Message:    ir.Message{Role: ir.RoleAssistant, Content: txtPart("done")},
			StopReason: ir.StopEnd,
		},
	}
	for name, resp := range cases {
		if _, ok := starvedRetryRequest(starvedReq(), resp); ok {
			t.Fatalf("%s must NOT trigger the rescue", name)
		}
	}
}

// Without a client-declared budget there is nothing starved to fix.
func TestNoDeclaredBudgetNoRescue(t *testing.T) {
	req := starvedReq()
	req.MaxTokens = 0
	if _, ok := starvedRetryRequest(req, starvedResp()); ok {
		t.Fatal("no max_tokens: the rescue cannot know the budget, must not fire")
	}
}

// Liveness probes (max_tokens=1) fail as-is: rescuing doubles their cost.
func TestTinyBudgetNoRescue(t *testing.T) {
	req := starvedReq()
	req.MaxTokens = minStarvedBudget - 1
	if _, ok := starvedRetryRequest(req, starvedResp()); ok {
		t.Fatal("probe-sized budget must not be rescued")
	}
}

// Thinking already off: the retry cannot change anything.
func TestThinkingAlreadyOffNoRescue(t *testing.T) {
	req := starvedReq()
	req.Thinking = &ir.ThinkingConfig{Enabled: false}
	if _, ok := starvedRetryRequest(req, starvedResp()); ok {
		t.Fatal("thinking already disabled: nothing for the rescue to change")
	}
}

// Nil inputs never fire.
func TestNilInputsNoRescue(t *testing.T) {
	if _, ok := starvedRetryRequest(nil, starvedResp()); ok {
		t.Fatal("nil request must not fire")
	}
	if _, ok := starvedRetryRequest(starvedReq(), nil); ok {
		t.Fatal("nil response must not fire")
	}
}

// The rescue preserves everything else the client asked for.
func TestRescuePreservesRequest(t *testing.T) {
	req := starvedReq()
	req.Temperature = func() *float64 { f := 0.3; return &f }()
	req.Thinking = &ir.ThinkingConfig{Enabled: true, Include: true}
	retry, ok := starvedRetryRequest(req, starvedResp())
	if !ok {
		t.Fatal("must fire")
	}
	if retry.Temperature == nil || *retry.Temperature != *req.Temperature {
		t.Fatal("temperature must survive")
	}
	// Include survives (the client still wants reasoning text back — there
	// just will not be any because thinking is off; the flag is the client's
	// preference, not a thinking switch).
	if retry.Thinking == nil || !retry.Thinking.Include {
		t.Fatalf("Include must survive the rewrite, got %+v", retry.Thinking)
	}
}
