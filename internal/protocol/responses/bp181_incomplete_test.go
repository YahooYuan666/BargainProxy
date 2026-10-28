package responses

import (
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

// BP-181: a length-capped turn reports incomplete/max_output_tokens —
// non-streamed via Render, streamed via response.incomplete.
func TestBP181IncompleteStatus(t *testing.T) {
	resp := &ir.ChatResponse{
		StopReason: ir.StopLength,
		Usage:      ir.Usage{InputTokens: 4, OutputTokens: 6},
		Message: ir.Message{
			Role:    ir.RoleAssistant,
			Content: []ir.ContentPart{{Kind: "text", Text: "partial answer"}},
		},
	}
	out := Render("resp_inc", "m", 123, resp, "partial answer")
	if out.Status != "incomplete" {
		t.Fatalf("status = %q, want incomplete", out.Status)
	}
	if out.IncompleteDetails == nil || out.IncompleteDetails.Reason != "max_output_tokens" {
		t.Fatalf("incomplete_details = %+v", out.IncompleteDetails)
	}

	var buf strings.Builder
	sw := NewStreamWriter(&buf, nil, "resp_inc2", "m")
	_ = sw.Send(ir.StreamEvent{Kind: ir.EvText, Delta: "partial"})
	_ = sw.Send(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopLength})
	if !strings.Contains(buf.String(), `"response.incomplete"`) {
		t.Fatal("stream must end with response.incomplete for a length stop")
	}
	if !strings.Contains(buf.String(), `"reason":"max_output_tokens"`) {
		t.Fatal("stream terminal missing incomplete_details")
	}

	// A normal stop stays completed.
	var buf2 strings.Builder
	sw2 := NewStreamWriter(&buf2, nil, "resp_ok", "m")
	_ = sw2.Send(ir.StreamEvent{Kind: ir.EvText, Delta: "done"})
	_ = sw2.Send(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
	if !strings.Contains(buf2.String(), `"response.completed"`) || strings.Contains(buf2.String(), "incomplete") {
		t.Fatal("a normal stop must stay completed")
	}
}
