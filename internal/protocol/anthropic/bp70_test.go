package anthropic

import (
	"bytes"
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

// BP-70: upstream streams may interleave argument deltas across tool indexes.
// The Anthropic wire allows only one open content block, so the writer buffers
// per tool index and emits complete blocks at the end — never `call_<index>`
// duplicates with split arguments.
func TestStreamInterleavedToolCalls(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, nil, "msg_1", "m", 1)
	for _, ev := range []ir.StreamEvent{
		{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "toolu_A", Name: "a"}},
		{Kind: ir.EvToolStart, ToolIndex: 1, ToolCall: &ir.ToolCall{ID: "toolu_B", Name: "b"}},
		{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{"x":1}`},
		{Kind: ir.EvToolArgs, ToolIndex: 1, Delta: `{"y":2}`},
		{Kind: ir.EvDone, Stop: ir.StopToolUse},
	} {
		if err := sw.Send(ev); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	out := buf.String()
	if n := strings.Count(out, `"type":"tool_use"`); n != 2 {
		t.Fatalf("tool_use blocks = %d, want 2:\n%s", n, out)
	}
	if !strings.Contains(out, `"id":"toolu_A"`) || !strings.Contains(out, `"id":"toolu_B"`) {
		t.Fatalf("original call ids missing:\n%s", out)
	}
	if !strings.Contains(out, `{\"x\":1}`) || !strings.Contains(out, `{\"y\":2}`) {
		t.Fatalf("arguments missing:\n%s", out)
	}
	if strings.Contains(out, `"call_0"`) || strings.Contains(out, `"call_1"`) {
		t.Fatalf("synthetic call ids leaked:\n%s", out)
	}
}

func TestStreamSequentialToolCall(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, nil, "msg_2", "m", 1)
	for _, ev := range []ir.StreamEvent{
		{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "toolu_A", Name: "a"}},
		{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{"x":`},
		{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `1}`},
		{Kind: ir.EvDone, Stop: ir.StopToolUse},
	} {
		if err := sw.Send(ev); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	out := buf.String()
	if n := strings.Count(out, `"type":"tool_use"`); n != 1 {
		t.Fatalf("tool_use blocks = %d, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, `"partial_json":"{\"x\":1}"`) {
		t.Fatalf("accumulated args missing:\n%s", out)
	}
}

func TestStreamTextBeforeBufferedTools(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, nil, "msg_3", "m", 1)
	for _, ev := range []ir.StreamEvent{
		{Kind: ir.EvText, Delta: "thinking out loud"},
		{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "toolu_A", Name: "a"}},
		{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{}`},
		{Kind: ir.EvDone, Stop: ir.StopToolUse},
	} {
		if err := sw.Send(ev); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	out := buf.String()
	textPos := strings.Index(out, `"type":"text_delta"`)
	toolPos := strings.Index(out, `"type":"tool_use"`)
	if textPos < 0 || toolPos < 0 || textPos > toolPos {
		t.Fatalf("text block must precede tool blocks (text=%d tool=%d):\n%s", textPos, toolPos, out)
	}
}
