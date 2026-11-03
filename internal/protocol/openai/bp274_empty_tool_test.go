package openai

import (
	"testing"

	"bargainproxy/internal/ir"
)

// BP-274 (traework2api-panel c5aec914ff): a fully-empty tool call
// (no id, no name, no arguments) is a parallel-call placeholder the vendor
// leaked through aggregation; strict clients fail the whole round on it.
// The non-stream renderer drops it defensively — real shapes (named but
// argless etc.) always stay.
func TestRenderCompletionDropsEmptyToolCalls(t *testing.T) {
	resp := &ir.ChatResponse{
		StopReason: ir.StopToolUse,
		Message: ir.Message{
			ToolCalls: []ir.ToolCall{
				{ID: "", Name: "", Arguments: ""}, // placeholder: dropped
				{ID: "call_1", Name: "bash", Arguments: `{"command":"ls"}`},
				{ID: "call_2", Name: "think", Arguments: ""}, // named, argless: kept
			},
		},
	}
	out := RenderCompletion("id-1", "m", 1234, resp)
	msg := out.Choices[0].Message
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v, want 2 (empty placeholder dropped)", msg.ToolCalls)
	}
	if msg.ToolCalls[0].Function.Name != "bash" || msg.ToolCalls[1].Function.Name != "think" {
		t.Fatalf("tool calls mangled: %+v", msg.ToolCalls)
	}
}
