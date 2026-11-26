// BP-350 (magpie 33b04b64, #7565) on the three remaining egresses. The openai
// chat path has its own file; these cover the buffered (anthropic, responses)
// and streamed-finalize (geminiv1) writers.
package anthropic

import (
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

func TestBP350MessageDropsNamelessToolUse(t *testing.T) {
	resp := &ir.ChatResponse{
		StopReason: ir.StopToolUse,
		Message: ir.Message{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{
			{ID: "c1", Name: "", Arguments: `{"a":1}`},
			{ID: "c2", Name: "Bash", Arguments: `{"command":"ls"}`},
		}},
	}
	out := RenderMessage("id", "m", resp)
	var tools int
	for _, b := range out.Content {
		if b.Type == "tool_use" {
			tools++
			if b.Name == "" {
				t.Fatalf("a tool_use with an empty name reached the wire: %+v", b)
			}
		}
	}
	if tools != 1 {
		t.Fatalf("tool_use blocks = %d, want 1", tools)
	}
	if out.StopReason == nil || *out.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %v, want tool_use (a real call remains)", out.StopReason)
	}
}

func TestBP350MessageOnlyNamelessEndsEndTurn(t *testing.T) {
	resp := &ir.ChatResponse{
		StopReason: ir.StopToolUse,
		Message: ir.Message{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{
			{ID: "c1", Name: "", Arguments: `{}`},
		}},
	}
	out := RenderMessage("id", "m", resp)
	for _, b := range out.Content {
		if b.Type == "tool_use" {
			t.Fatalf("a nameless tool_use survived: %+v", b)
		}
	}
	if out.StopReason == nil || *out.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %v, want end_turn (nothing to run)", out.StopReason)
	}
}

// The streamed lane holds tool blocks until the turn ends, so the drop happens
// in emitToolBlocks.
func TestBP350StreamDropsNamelessToolBlock(t *testing.T) {
	var sb strings.Builder
	sw := NewStreamWriter(&sb, nil, "msg_1", "m", 0)
	// One nameless, one named.
	if err := sw.Send(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "c1"}}); err != nil {
		t.Fatal(err)
	}
	if err := sw.Send(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{"a":1}`}); err != nil {
		t.Fatal(err)
	}
	if err := sw.Send(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 1, ToolCall: &ir.ToolCall{ID: "c2", Name: "Bash"}}); err != nil {
		t.Fatal(err)
	}
	if err := sw.Send(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: 1, Delta: `{"command":"ls"}`}); err != nil {
		t.Fatal(err)
	}
	if err := sw.Send(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopToolUse}); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if strings.Count(out, `"type":"tool_use"`) != 1 {
		t.Fatalf("tool_use blocks = %d, want 1:\n%s", strings.Count(out, `"type":"tool_use"`), out)
	}
	if strings.Contains(out, `"name":""`) {
		t.Fatalf("a nameless tool_use reached the client:\n%s", out)
	}
	if !strings.Contains(out, `"name":"Bash"`) {
		t.Fatalf("the named call was lost:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Fatalf("stop_reason changed although a real call remains:\n%s", out)
	}
}

func TestBP350StreamOnlyNamelessEndsEndTurn(t *testing.T) {
	var sb strings.Builder
	sw := NewStreamWriter(&sb, nil, "msg_1", "m", 0)
	if err := sw.Send(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "c1"}}); err != nil {
		t.Fatal(err)
	}
	if err := sw.Send(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{}`}); err != nil {
		t.Fatal(err)
	}
	if err := sw.Send(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopToolUse}); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("a nameless tool_use reached the client:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Fatalf("stop_reason must be end_turn with nothing to run:\n%s", out)
	}
}
