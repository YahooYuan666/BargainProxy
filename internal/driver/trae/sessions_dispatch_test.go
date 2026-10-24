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

// TestToolCeilingIsTheMeasuredWall pins the number itself. Field measurement
// 2026-10-23: 261623 input tokens pass, ~264000 answer 4026 for every model.
// If someone changes this constant without a new measurement, the gate either
// rejects requests that would have worked or lets certain failures through.
func TestToolCeilingIsTheMeasuredWall(t *testing.T) {
	if traeToolInputCeiling != 262144 {
		t.Fatalf("tool ceiling = %d, want the measured 262144", traeToolInputCeiling)
	}
	if chatSessionMaxInput != 1000000 {
		t.Fatalf("session ceiling = %d, want the documented 1000000", chatSessionMaxInput)
	}
}

// TestOversizeToolRequestFailsBeforeDispatch pins the gate that saves a leg.
// A tool-carrying prompt above the ladder ceiling can never succeed: the
// session surface would drop every tool, and the ladder answers 4026 after
// seconds-to-minutes. The driver must refuse it up front so the router
// cascades instead of paying that cost.
func TestOversizeToolRequestFailsBeforeDispatch(t *testing.T) {
	d := &Driver{}
	req := &ir.ChatRequest{
		Model: "deepseek-v4.1-flash",
		// ~330k tokens of text plus one declared tool.
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{
			{Kind: "text", Text: strings.Repeat("a", 990000)},
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
	if !strings.Contains(ue.Message, "tool ceiling") {
		t.Fatalf("message should name the tool ceiling: %q", ue.Message)
	}
	if uperr.Rotatable(ue.Class) && ue.Status != 400 {
		t.Fatalf("want a non-rotatable 400, got status %d", ue.Status)
	}
}
