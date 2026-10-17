package anthropic

import (
	"bytes"
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

// BP-69: assistant thinking blocks from history are preserved (text +
// signature) so an anthropic-shaped upstream can replay them.
func TestParseMessagesPreservesThinkingHistory(t *testing.T) {
	req, err := ParseMessagesRequest([]byte(`{
		"model":"m","max_tokens":10,
		"messages":[{"role":"assistant","content":[
			{"type":"thinking","thinking":"先想","signature":"sig-1"},
			{"type":"text","text":"answer"},
			{"type":"tool_use","id":"toolu_1","name":"f","input":{}}
		]}]
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %d", len(req.Messages))
	}
	m := req.Messages[0]
	if len(m.Thinking) != 1 || m.Thinking[0].Text != "先想" || m.Thinking[0].Signature != "sig-1" {
		t.Fatalf("thinking = %+v", m.Thinking)
	}
	if len(m.Content) != 1 || m.Content[0].Text != "answer" {
		t.Fatalf("content = %+v", m.Content)
	}
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "toolu_1" {
		t.Fatalf("tool calls = %+v", m.ToolCalls)
	}
}

func TestParseMessagesToolChoiceAndTopK(t *testing.T) {
	req, err := ParseMessagesRequest([]byte(`{
		"model":"m","max_tokens":10,"top_k":33,
		"tool_choice":{"type":"tool","name":"get_weather","disable_parallel_tool_use":true},
		"messages":[{"role":"user","content":"hi"}]
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.ToolChoice != "required" || req.ToolChoiceName != "get_weather" || !req.DisableParallel {
		t.Fatalf("tool choice = %v name=%q disable=%v", req.ToolChoice, req.ToolChoiceName, req.DisableParallel)
	}
	if req.TopK == nil || *req.TopK != 33 {
		t.Fatalf("top_k = %v", req.TopK)
	}
}

func TestRenderMessageCarriesThinkingSignature(t *testing.T) {
	resp := &ir.ChatResponse{
		Thinking:          "先想",
		ThinkingSignature: "sig-1",
		Message:           ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "answer"}}},
	}
	out := RenderMessage("msg_1", "m", resp)
	if len(out.Content) < 1 || out.Content[0].Type != "thinking" {
		t.Fatalf("content = %+v", out.Content)
	}
	if out.Content[0].Signature != "sig-1" {
		t.Fatalf("signature = %q", out.Content[0].Signature)
	}
}

func TestStreamEmitsSignatureDelta(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, nil, "msg_1", "m", 1)
	for _, ev := range []ir.StreamEvent{
		{Kind: ir.EvThinking, Delta: "先想"},
		{Kind: ir.EvThinkingSignature, Delta: "sig-1"},
		{Kind: ir.EvDone, Stop: ir.StopEnd},
	} {
		if err := sw.Send(ev); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	out := buf.String()
	if !strings.Contains(out, `"type":"signature_delta"`) || !strings.Contains(out, `"signature":"sig-1"`) {
		t.Fatalf("signature delta missing: %s", out)
	}
}
