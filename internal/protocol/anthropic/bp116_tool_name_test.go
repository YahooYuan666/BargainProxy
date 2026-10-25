package anthropic

import (
	"testing"

	"bargainproxy/internal/ir"
)

// BP-116: Anthropic's tool_result block carries only tool_use_id — no name.
// The IR tool message built from it therefore had an empty ToolName, and the
// antigravity driver (which must emit Gemini functionResponse{name,...}) fell
// back to the literal "tool". The upstream then saw
//
//	functionCall{name:"Bash"}  next to  functionResponse{name:"tool"}
//
// and answered the generic "Request contains an invalid argument" — no field
// named, which is why this took a dumped body to find (2026-10-25 00:02, the
// subagent request in session_f00a47eb).
func TestToolResultGetsItsCallName(t *testing.T) {
	msgs := []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "run it"}}},
		{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "call_1", Name: "Bash", Arguments: `{"cmd":"ls"}`}}},
		{Role: ir.RoleTool, ToolCallID: "call_1", Content: []ir.ContentPart{{Kind: "text", Text: "a.txt"}}},
	}
	fillToolResultNames(msgs)
	if msgs[2].ToolName != "Bash" {
		t.Fatalf("tool result name = %q, want %q (an empty name makes the driver send the literal \"tool\")",
			msgs[2].ToolName, "Bash")
	}
}

// Parallel calls pair by id, not by position.
func TestParallelToolResultsPairByID(t *testing.T) {
	msgs := []ir.Message{
		{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{
			{ID: "a", Name: "Read"}, {ID: "b", Name: "Bash"},
		}},
		{Role: ir.RoleTool, ToolCallID: "b", Content: []ir.ContentPart{{Kind: "text", Text: "out-b"}}},
		{Role: ir.RoleTool, ToolCallID: "a", Content: []ir.ContentPart{{Kind: "text", Text: "out-a"}}},
	}
	fillToolResultNames(msgs)
	if msgs[1].ToolName != "Bash" || msgs[2].ToolName != "Read" {
		t.Fatalf("pairing wrong: %q / %q", msgs[1].ToolName, msgs[2].ToolName)
	}
}

// An existing name is never overwritten.
func TestExistingToolNameSurvives(t *testing.T) {
	msgs := []ir.Message{
		{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "a", Name: "Read"}}},
		{Role: ir.RoleTool, ToolCallID: "a", ToolName: "AlreadySet", Content: []ir.ContentPart{{Kind: "text", Text: "x"}}},
	}
	fillToolResultNames(msgs)
	if msgs[1].ToolName != "AlreadySet" {
		t.Fatalf("name was overwritten: %q", msgs[1].ToolName)
	}
}

// A result whose call is not in this history stays empty: the driver's own
// fallback applies, and that case genuinely has no name to recover.
func TestUnknownCallLeavesNameEmpty(t *testing.T) {
	msgs := []ir.Message{
		{Role: ir.RoleTool, ToolCallID: "ghost", Content: []ir.ContentPart{{Kind: "text", Text: "x"}}},
	}
	fillToolResultNames(msgs)
	if msgs[0].ToolName != "" {
		t.Fatalf("invented a name for an unknown call: %q", msgs[0].ToolName)
	}
}

// End-to-end through the real Anthropic entry point: the wire shape that broke
// the subagent (assistant tool_use, then a user turn carrying tool_result).
// This is the test that must go red if the fill step is removed from
// ParseMessagesRequest — asserting on the helper alone proves nothing about
// whether the entry point calls it.
func TestParseMessagesRequestFillsToolResultName(t *testing.T) {
	body := []byte(`{
	  "model": "gemini-3.8-flash-high",
	  "max_tokens": 100,
	  "messages": [
	    {"role":"user","content":[{"type":"text","text":"list the dir"}]},
	    {"role":"assistant","content":[
	      {"type":"text","text":"running"},
	      {"type":"tool_use","id":"call_0_b54ca4c0","name":"Bash","input":{"cmd":"ls"}}
	    ]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"call_0_b54ca4c0","content":"a.txt"}
	    ]}
	  ]
	}`)
	req, err := ParseMessagesRequest(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, m := range req.Messages {
		if m.Role == ir.RoleTool && m.ToolCallID == "call_0_b54ca4c0" {
			found = true
			if m.ToolName != "Bash" {
				t.Fatalf("tool result name = %q, want Bash — an empty name makes the antigravity driver send the literal \"tool\" and the upstream answers 'invalid argument'", m.ToolName)
			}
		}
	}
	if !found {
		t.Fatalf("tool message missing from parsed request: %+v", req.Messages)
	}
}
