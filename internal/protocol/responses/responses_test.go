package responses

import (
	"bytes"
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

func TestParseRequestInputForms(t *testing.T) {
	// String input + instructions.
	req, err := ParseRequest([]byte(`{"model":"m","instructions":"sys","input":"hello"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != ir.RoleSystem || req.Messages[1].Content[0].Text != "hello" {
		t.Fatalf("string input: %+v", req.Messages)
	}

	// Array input with function_call / function_call_output round trip.
	body := []byte(`{
	  "model":"m",
	  "input":[
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
	    {"type":"function_call","call_id":"call_9","name":"f","arguments":"{\"x\":1}"},
	    {"type":"function_call_output","call_id":"call_9","output":"ok"}
	  ],
	  "tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],
	  "reasoning":{"effort":"high"}
	}`)
	req, err = ParseRequest(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d", len(req.Messages))
	}
	if req.Messages[1].Role != ir.RoleAssistant || len(req.Messages[1].ToolCalls) != 1 || req.Messages[1].ToolCalls[0].ID != "call_9" {
		t.Fatalf("function_call item: %+v", req.Messages[1])
	}
	if req.Messages[2].Role != ir.RoleTool || req.Messages[2].ToolCallID != "call_9" {
		t.Fatalf("function_call_output item: %+v", req.Messages[2])
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "f" {
		t.Fatalf("tools: %+v", req.Tools)
	}
	if req.RequestedEffort != "high" {
		t.Fatalf("effort = %q", req.RequestedEffort)
	}
}

func TestStreamEventStateMachine(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, nil, "resp_1", "m")
	for _, ev := range []ir.StreamEvent{
		{Kind: ir.EvText, Delta: "He"},
		{Kind: ir.EvText, Delta: "llo"},
		{Kind: ir.EvUsage, Usage: &ir.Usage{InputTokens: 2, OutputTokens: 3}},
		{Kind: ir.EvDone, Stop: ir.StopEnd},
	} {
		if err := sw.Send(ev); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	out := buf.String()
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "event: ") {
			names = append(names, strings.TrimPrefix(line, "event: "))
		}
	}
	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("sequence:\n got %v\nwant %v", names, want)
	}
	if !strings.Contains(out, `"text":"Hello"`) && !strings.Contains(out, `"text":"He"`) {
		t.Fatalf("deltas missing: %s", out)
	}
	if !strings.Contains(out, `"status":"completed"`) {
		t.Fatalf("completed status missing: %s", out)
	}
}

func TestStreamToolFunctionCallEvents(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, nil, "resp_2", "m")
	for _, ev := range []ir.StreamEvent{
		{Kind: ir.EvText, Delta: "calling"},
		{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "call_1", Name: "f"}},
		{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{"a":`},
		{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `1}`},
		{Kind: ir.EvDone, Stop: ir.StopToolUse},
	} {
		if err := sw.Send(ev); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	out := buf.String()
	if !strings.Contains(out, "response.function_call_arguments.delta") {
		t.Fatalf("function call deltas missing: %s", out)
	}
	if !strings.Contains(out, `"type":"function_call"`) {
		t.Fatalf("function_call item missing: %s", out)
	}
}

func TestRenderNonStreamed(t *testing.T) {
	resp := &ir.ChatResponse{
		Usage: ir.Usage{InputTokens: 4, OutputTokens: 6},
		Message: ir.Message{
			Role:      ir.RoleAssistant,
			Content:   []ir.ContentPart{{Kind: "text", Text: "body"}},
			ToolCalls: []ir.ToolCall{{ID: "c1", Name: "f", Arguments: `{}`}},
		},
	}
	out := Render("resp_9", "m", 123, resp, "body")
	if out.Object != "response" || out.Status != "completed" {
		t.Fatalf("envelope: %+v", out)
	}
	if len(out.Output) != 2 {
		t.Fatalf("output items = %d", len(out.Output))
	}
	if out.Usage.TotalTokens != 10 {
		t.Fatalf("usage: %+v", out.Usage)
	}
}
