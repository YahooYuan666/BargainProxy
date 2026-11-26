// BP-350 (magpie 33b04b64, #7565) on the native Gemini surface.
package geminiv1

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

func namelessResp(calls ...ir.ToolCall) *ir.ChatResponse {
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, ToolCalls: calls},
		StopReason: ir.StopToolUse,
	}
}

func TestBP350GeminiRenderDropsNamelessCall(t *testing.T) {
	out := Render("m", namelessResp(
		ir.ToolCall{ID: "c1", Name: "", Arguments: `{"a":1}`},
		ir.ToolCall{ID: "c2", Name: "Bash", Arguments: `{"command":"ls"}`},
	), false)
	var names []string
	for _, p := range out.Candidates[0].Content.Parts {
		if p.FunctionCall != nil {
			names = append(names, p.FunctionCall.Name)
		}
	}
	if len(names) != 1 || names[0] != "Bash" {
		t.Fatalf("functionCalls = %v, want one Bash", names)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), `"name":""`) {
		t.Fatalf("a nameless functionCall is on the wire: %s", b)
	}
}

func TestBP350GeminiRenderOnlyNameless(t *testing.T) {
	out := Render("m", namelessResp(ir.ToolCall{ID: "c1", Name: "", Arguments: `{}`}), false)
	for _, p := range out.Candidates[0].Content.Parts {
		if p.FunctionCall != nil {
			t.Fatalf("a nameless functionCall survived: %+v", p.FunctionCall)
		}
	}
}

// The streamed lane materialises parts in finalize().
func TestBP350GeminiStreamDropsNamelessCall(t *testing.T) {
	var sb strings.Builder
	sw := NewStreamWriter(&sb, nil, "m", false)
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
	if strings.Contains(out, `"name":""`) {
		t.Fatalf("a nameless functionCall reached the client:\n%s", out)
	}
	if !strings.Contains(out, `"name":"Bash"`) {
		t.Fatalf("the named call was lost:\n%s", out)
	}
}
