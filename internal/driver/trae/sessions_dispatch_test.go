package trae

import (
	"context"
	"strings"
	"testing"

	"bargainproxy/internal/driver"

	"bargainproxy/internal/ir"
	"bargainproxy/internal/uperr"
)

// TestEstimateInputTokensTracksContent pins the coarse estimator the dispatch
// uses to decide whether a tool request can fit the ladder. It is deliberately
// approximate (the gateway has its own), but it must move with the content: a
// value that ignores text would let a 300k prompt through the gate.
func TestEstimateInputTokensTracksContent(t *testing.T) {
	small := &ir.ChatRequest{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: strings.Repeat("a", 300)}}},
	}}
	big := &ir.ChatRequest{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: strings.Repeat("a", 900000)}}},
	}}
	if got := estimateInputTokens(small); got < 90 || got > 120 {
		t.Fatalf("small prompt estimate = %d, want ~100", got)
	}
	if got := estimateInputTokens(big); got < 290000 {
		t.Fatalf("big prompt estimate = %d, want >=290000", got)
	}

	// Images cost a flat allowance each; their base64 size is not a token count.
	withImage := &ir.ChatRequest{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{
			{Kind: "text", Text: strings.Repeat("a", 300)},
			{Kind: "image", ImageMIME: "image/png", ImageB64: strings.Repeat("x", 500000)},
		}},
	}}
	if got := estimateInputTokens(withImage); got > 1000 {
		t.Fatalf("image estimate = %d; base64 length must not be counted as tokens", got)
	}

	if got := estimateInputTokens(nil); got != 0 {
		t.Fatalf("nil request = %d, want 0", got)
	}
}

// TestToolCeilingIsTheMeasuredWall pins the number itself.
//
// RE-MEASURED 2026-11-01, and the old value was wrong. The 2026-10-23 note
// (261623 pass, ~264000 answer 4026, "channel-wide wall") no longer holds: a
// tool-bearing prompt was measured reaching 1,020,179 tokens with a needle
// echoed back proving the whole prompt was read, and only 1,800,000 answered
// 4022. Probes WITHOUT is_max_mode reached exactly as far as those with it, so
// Max Mode is not what lifts the ceiling — the surface simply allows ~1M now.
//
// The constant is therefore the catalogue's context_window_tokens.max plus a
// tokenizer allowance. Changing it without a fresh measurement either throws
// away context the channel serves or lets a certain 4022 through.
func TestToolCeilingIsTheMeasuredWall(t *testing.T) {
	if traeToolInputCeiling != 1024000 {
		t.Fatalf("tool ceiling = %d, want the 2026-11-01 measured 1024000", traeToolInputCeiling)
	}
	if chatSessionMaxInput != 1000000 {
		t.Fatalf("session ceiling = %d, want the documented 1000000", chatSessionMaxInput)
	}
	// The tool surface must reach at least as far as the session surface:
	// measurement shows both land at ~1M, so a tool ceiling BELOW the session
	// ceiling would push tool turns off a surface that handles them.
	if traeToolInputCeiling < chatSessionMaxInput {
		t.Fatalf("tool ceiling %d is below the session ceiling %d; the 2026-11-01 "+
			"measurement puts both at ~1M", traeToolInputCeiling, chatSessionMaxInput)
	}
}

// TestOversizeToolRequestFailsBeforeDispatch pins the gate that saves a leg —
// at the REAL wall, not the old one.
//
// A tool-carrying prompt above the ceiling cannot succeed on this surface and
// the session surface would drop every tool, so the driver must refuse it up
// front and let the router cascade. The threshold moved from 262144 to
// 1024000 on 2026-11-01 (see TestToolCeilingIsTheMeasuredWall): a 330k-token
// tool request now DISPATCHES instead of being refused, because measurement
// shows it works. This test therefore uses a prompt above the new ceiling.
func TestOversizeToolRequestFailsBeforeDispatch(t *testing.T) {
	d := testDriver("http://127.0.0.1:1", "http://127.0.0.1:1")
	req := &ir.ChatRequest{
		Model: "deepseek-v4.1-flash",
		// ~1.5M estimated tokens: above the measured ~1.02M wall, with a tool.
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{
			{Kind: "text", Text: strings.Repeat("a", 4500000)},
		}}},
		Tools: []ir.ToolDef{{Name: "bash"}},
	}
	acc := testAccount()
	_, err := d.Stream(context.Background(),
		driver.ChatCall{Account: acc, UpstreamModel: "deepseek-v4.1-flash", Request: req},
		func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("an oversized tool request must fail before dispatch")
	}
	ue, ok := uperr.As(err)
	if !ok {
		t.Fatalf("want *uperr.Error, got %T", err)
	}
	if ue.Class != uperr.ClassModelUnavailable {
		t.Fatalf("class = %s, want %s (so the router cascades)", ue.Class, uperr.ClassModelUnavailable)
	}
	if !strings.Contains(ue.Message, "ceiling") {
		t.Fatalf("message should name the ceiling: %q", ue.Message)
	}
	if uperr.Rotatable(ue.Class) && ue.Status != 400 {
		t.Fatalf("want a non-rotatable 400, got status %d", ue.Status)
	}
}

// TestToolsDispatchBelowTheNewCeiling locks the point of the 2026-11-01
// change: a tool-bearing prompt that the OLD 262144 gate refused must now be
// dispatched to llm_utils_chat rather than rejected. Measurement put the real
// wall at ~1.02M, so a 330k-token tool turn is ordinary traffic now.
//
// Without this test the old refusal could come back unnoticed, which is
// exactly how the wrong ceiling survived from 10-23 to 11-01.
func TestToolsDispatchBelowTheNewCeiling(t *testing.T) {
	// A real driver pointed at an unroutable host: the request must get PAST
	// the ceiling gate and fail in transport. That distinguishes "dispatched"
	// from "refused before dispatch" without needing a live upstream.
	d := testDriver("http://127.0.0.1:1", "http://127.0.0.1:1")
	req := &ir.ChatRequest{
		Model: "deepseek-v4.1-flash",
		// ~330k estimated tokens: over the old 262144 gate, well under the
		// measured ~1.02M wall.
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{
			{Kind: "text", Text: strings.Repeat("a", 990000)},
		}}},
		Tools: []ir.ToolDef{{Name: "bash"}},
	}
	est := estimateInputTokens(req)
	if est <= 262144 {
		t.Fatalf("test premise broken: estimate %d is not above the old gate", est)
	}
	if est >= traeToolInputCeiling {
		t.Fatalf("test premise broken: estimate %d is not below the new ceiling", est)
	}
	acc := testAccount()
	_, err := d.Stream(context.Background(),
		driver.ChatCall{Account: acc, UpstreamModel: "deepseek-v4.1-flash", Request: req},
		func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("expected a transport failure against an unroutable host")
	}
	ue, ok := uperr.As(err)
	if !ok {
		t.Fatalf("want *uperr.Error, got %T: %v", err, err)
	}
	if ue.Class == uperr.ClassModelUnavailable && strings.Contains(ue.Message, "ceiling") {
		t.Fatalf("a %d-token tool request was refused by the ceiling gate; measurement "+
			"puts the wall at ~1.02M tokens: %s", est, ue.Message)
	}
}
