package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

func TestParseMessagesRequestBasics(t *testing.T) {
	body := []byte(`{
	  "model":"glm-5.3",
	  "max_tokens":100,
	  "system":[{"type":"text","text":"be nice"}],
	  "messages":[
	    {"role":"user","content":"hello"},
	    {"role":"user","content":[{"type":"text","text":"again"}]},
	    {"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"42"}]}
	  ],
	  "metadata":{"user_id":"{\"session_id\":\"sess-9\",\"device_id\":\"d\"}"}
	}`)
	req, err := ParseMessagesRequest(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Model != "glm-5.3" || req.MaxTokens != 100 {
		t.Fatalf("basic fields wrong: %+v", req)
	}
	if req.SessionKey != "user:sess-9" {
		t.Fatalf("session key = %q", req.SessionKey)
	}
	// Expected: system, merged user, assistant, tool.
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %d, want 4: %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[0].Role != ir.RoleSystem {
		t.Fatalf("first role = %s", req.Messages[0].Role)
	}
	// Adjacent user merge: "hello" and "again" must be one message.
	merged := req.Messages[1]
	if merged.Role != ir.RoleUser || len(merged.Content) != 2 {
		t.Fatalf("merge failed: %+v", merged)
	}
	toolCall := req.Messages[2].ToolCalls
	if len(toolCall) != 1 || toolCall[0].Name != "f" {
		t.Fatalf("tool call parse failed: %+v", req.Messages[2])
	}
	if req.Messages[3].Role != ir.RoleTool || req.Messages[3].ToolCallID != "toolu_1" {
		t.Fatalf("tool result parse failed: %+v", req.Messages[3])
	}
}

func TestStreamWriterEventSequence(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, nil, "msg_1", "glm-5.3", 0)
	events := []ir.StreamEvent{
		{Kind: ir.EvThinking, Delta: "think"},
		{Kind: ir.EvText, Delta: "Hello"},
		{Kind: ir.EvText, Delta: " world"},
		{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "t1", Name: "f"}},
		{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{"a":1}`},
		{Kind: ir.EvUsage, Usage: &ir.Usage{InputTokens: 5, OutputTokens: 7}},
		{Kind: ir.EvDone, Stop: ir.StopToolUse},
	}
	for _, ev := range events {
		if err := sw.Send(ev); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	out := buf.String()
	// Assert ordering of event names.
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "event: ") {
			names = append(names, strings.TrimPrefix(line, "event: "))
		}
	}
	want := []string{
		"message_start",
		"content_block_start", // thinking
		"content_block_delta", // thinking delta
		"content_block_stop",
		"content_block_start", // text
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"content_block_start", // tool
		"content_block_delta", // input_json_delta
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence:\n got %v\nwant %v", names, want)
	}
	// The message_delta must carry stop_reason tool_use.
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Fatalf("missing stop_reason in output:\n%s", out)
	}
}

func TestEstimateTokensCJK(t *testing.T) {
	req := &ir.ChatRequest{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "你好世界"}}}, // 4 CJK chars
	}}
	got := EstimateTokens(req)
	if got < 4 {
		t.Fatalf("estimate = %d, want >= 4 for 4 CJK chars", got)
	}
	long := &ir.ChatRequest{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: strings.Repeat("a", 400)}}},
	}}
	if e := EstimateTokens(long); e != 100 {
		t.Fatalf("ascii estimate = %d, want 100", e)
	}
}

func TestRenderMessageShape(t *testing.T) {
	resp := &ir.ChatResponse{
		StopReason: ir.StopToolUse,
		Usage:      ir.Usage{InputTokens: 3, OutputTokens: 4},
		Message: ir.Message{
			Role:      ir.RoleAssistant,
			Content:   []ir.ContentPart{{Kind: "text", Text: "hi"}},
			ToolCalls: []ir.ToolCall{{ID: "t1", Name: "f", Arguments: `{"x":1}`}},
		},
	}
	out := RenderMessage("msg_1", "m", resp)
	if out.ID != "msg_1" || out.Type != "message" || out.Role != "assistant" {
		t.Fatalf("envelope: %+v", out)
	}
	if len(out.Content) != 2 || out.Content[0].Type != "text" || out.Content[1].Type != "tool_use" {
		t.Fatalf("content blocks: %+v", out.Content)
	}
	if out.Content[1].Input.(map[string]any)["x"].(float64) != 1 {
		t.Fatalf("tool input: %+v", out.Content[1].Input)
	}
	if *out.StopReason != "tool_use" {
		t.Fatalf("stop: %v", *out.StopReason)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"usage":{"input_tokens":3,"output_tokens":4}`) {
		t.Fatalf("usage shape: %s", raw)
	}
}
