// BP-41: Anthropic thinking at the ingress (config → IR, history blocks
// dropped) and at the egress (thinking block without a signature).
package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

func TestThinkingConfigParsesIntoIR(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":4096,
		"thinking":{"type":"enabled","budget_tokens":15000},
		"messages":[{"role":"user","content":"hi"},
		            {"role":"assistant","content":[{"type":"thinking","thinking":"旧思维链"},{"type":"text","text":"答"}]},
		            {"role":"user","content":"再来"}]}`)
	req, err := ParseMessagesRequest(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	th := req.Thinking
	if th == nil || !th.Enabled || th.BudgetTokens != 15000 || !th.Include {
		t.Fatalf("thinking = %+v, want enabled budget=15000 include=true", th)
	}
	if th.Level != "medium" {
		t.Fatalf("level = %q, want medium (15000 ∈ [4096,16384))", th.Level)
	}
	if req.RequestedEffort != "medium" {
		t.Fatalf("requested effort = %q, want medium (gear-only 渠道需要档位)", req.RequestedEffort)
	}
	// History thinking blocks stay dropped (cross-provider signatures are not
	// replayable) — only the text part survives.
	for _, m := range req.Messages {
		for _, p := range m.Content {
			if p.Text == "旧思维链" {
				t.Fatalf("history thinking block leaked into IR: %+v", m)
			}
		}
	}
}

func TestThinkingWithoutBudgetDefaultsToHigh(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":1024,"thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hi"}]}`)
	req, err := ParseMessagesRequest(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Thinking == nil || !req.Thinking.Enabled {
		t.Fatalf("thinking = %+v, want enabled", req.Thinking)
	}
	if req.RequestedEffort != "high" {
		t.Fatalf("requested effort = %q, want high (无预算按参考实现缺省 high)", req.RequestedEffort)
	}
}

func TestRenderMessageEmitsThinkingBlock(t *testing.T) {
	resp := &ir.ChatResponse{
		StopReason: ir.StopEnd,
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "答案"}}},
		Thinking:   "先推理",
	}
	out := RenderMessage("msg_1", "m", resp)
	if len(out.Content) != 2 {
		t.Fatalf("content = %+v, want thinking + text", out.Content)
	}
	if out.Content[0].Type != "thinking" || out.Content[0].Thinking != "先推理" {
		t.Fatalf("first block = %+v, want thinking block", out.Content[0])
	}
	if out.Content[1].Type != "text" || out.Content[1].Text != "答案" {
		t.Fatalf("second block = %+v, want text block", out.Content[1])
	}
}

func TestRenderMessageWithoutThinkingKeepsTextOnly(t *testing.T) {
	resp := &ir.ChatResponse{
		StopReason: ir.StopEnd,
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "答案"}}},
	}
	out := RenderMessage("msg_1", "m", resp)
	if len(out.Content) != 1 || out.Content[0].Type != "text" {
		t.Fatalf("content = %+v, want single text block", out.Content)
	}
}

// 2026-10-16 sub2api 参考：Anthropic wire 上 thinking 块必须带 signature 键
// （哪怕是空串），text 块必须带 text 键——严格客户端（Claude Code / Grok
// Build）缺键直接报 "missing field `signature`"。
func TestThinkingWireAlwaysCarriesSignatureKey(t *testing.T) {
	parse := func(raw []byte) map[string]any {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		return m
	}

	raw, err := json.Marshal(ContentBlock{Type: "thinking", Thinking: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	got := parse(raw)
	if v, ok := got["signature"]; !ok || v != "" {
		t.Fatalf("thinking block must carry an empty signature key, got %s", raw)
	}
	if got["thinking"] != "plan" {
		t.Fatalf("thinking text lost: %s", raw)
	}

	// 带真签名时原样输出
	raw, _ = json.Marshal(ContentBlock{Type: "thinking", Thinking: "plan", Signature: "SIG"})
	if parse(raw)["signature"] != "SIG" {
		t.Fatalf("real signature dropped: %s", raw)
	}

	// text 块强制带 text 键（空内容也不缺键）
	raw, _ = json.Marshal(ContentBlock{Type: "text"})
	if v, ok := parse(raw)["text"]; !ok || v != "" {
		t.Fatalf("text block must carry a text key, got %s", raw)
	}

	// 其他块型不受影响
	raw, _ = json.Marshal(ContentBlock{Type: "tool_use", ID: "call_1", Name: "f", Input: map[string]any{"a": 1}})
	tu := parse(raw)
	if tu["type"] != "tool_use" || tu["id"] != "call_1" || tu["name"] != "f" {
		t.Fatalf("tool_use block changed shape: %s", raw)
	}
	if _, ok := tu["signature"]; ok {
		t.Fatalf("tool_use must not carry a signature key: %s", raw)
	}
}

// 流式出口：thinking 的 content_block_start 同样带 signature 键。
func TestStreamThinkingStartCarriesSignatureKey(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, nil, "msg_1", "m", 0)
	if err := sw.Send(ir.StreamEvent{Kind: ir.EvThinking, Delta: "想"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := sw.Send(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd}); err != nil {
		t.Fatalf("done: %v", err)
	}
	out := buf.String()
	// Go maps marshal with sorted keys, so assert the keys individually.
	if !strings.Contains(out, `"type":"thinking"`) || !strings.Contains(out, `"signature":""`) {
		t.Fatalf("thinking start frame must carry the signature key: %s", out)
	}
}
