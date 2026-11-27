// BP-360: a tool's images sit where the tool left them — their own message
// right after the tool result, flushed when the model's next turn begins.
//
// BP-337 gave the carrying message its attribution but left the flush point
// alone: parked images attached to the next USER turn. On 2026-11-27 that
// produced the confusion it was meant to end — an image read off disk by a
// tool arrived on the user's next line, and both sides read it as a picture
// the user had just pasted. The client replays the whole history every turn,
// so a parked image walks forward with each turn and always sits on the
// newest user line.
//
// magpie flushes at the head of every assistant turn (internal/gateway/
// chat.go:488 showSeen): the position is fixed when the image is returned.
package openaicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// bp360Wire builds the read → answer → user-speaks shape that exposed the
// bug: the tool's image, the model's own answer to it, and then a user line
// that carries no image of its own.
func bp360Wire(t *testing.T) []map[string]any {
	t.Helper()
	msgs := []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "读取 blue.png"}}},
		{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "call_x", Name: "Read", Arguments: `{"file_path":"blue.png"}`}}},
		{Role: ir.RoleTool, ToolCallID: "call_x", ToolName: "Read", Content: []ir.ContentPart{
			{Kind: "image", ImageMIME: "image/png", ImageB64: bp308PNG},
		}},
		{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "我看完图了"}}},
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "第二句用户话"}}},
	}
	req := &ir.ChatRequest{Model: "m", Messages: msgs, MaxTokens: 100}
	raw, err := buildBody(driver.ChatCall{Request: req})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgsOut, _ := body["messages"].([]any)
	out := make([]map[string]any, 0, len(msgsOut))
	for _, m := range msgsOut {
		if mm, ok := m.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

// bp360TextOf joins a message's text parts.
func bp360TextOf(m map[string]any) string {
	switch c := m["content"].(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			pm, _ := p.(map[string]any)
			if pm["type"] == "text" {
				if s, ok := pm["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// TestBP360ImageKeepsItsPlace: the user's later line must carry no image of
// the tool's, and the image must sit immediately after the tool result —
// before the model's answer, not after it.
func TestBP360ImageKeepsItsPlace(t *testing.T) {
	wire := bp360Wire(t)
	toolAt, imageAt, answerAt := -1, -1, -1
	for i, m := range wire {
		if m["role"] == "tool" {
			toolAt = i
		}
		if strings.Contains(bp360TextOf(m), "From the result of") {
			imageAt = i
		}
		if strings.Contains(bp360TextOf(m), "我看完图了") {
			answerAt = i
		}
	}
	if toolAt < 0 || imageAt < 0 || answerAt < 0 {
		t.Fatalf("missing messages: tool=%d image=%d answer=%d", toolAt, imageAt, answerAt)
	}
	if imageAt != toolAt+1 {
		t.Errorf("image at %d, tool at %d — want the image immediately after the tool result", imageAt, toolAt)
	}
	if imageAt > answerAt {
		t.Errorf("image at %d rides after the answer at %d — it was parked until the user spoke", imageAt, answerAt)
	}

	last := wire[len(wire)-1]
	if last["role"] != "user" {
		t.Fatalf("last message is %v, want the user's line", last["role"])
	}
	if strings.Contains(bp360TextOf(last), "From the result of") {
		t.Error("the user's line carries the tool's image — that is what read as a fresh paste")
	}
	if !strings.Contains(bp360TextOf(last), "第二句用户话") {
		t.Errorf("last message is not the user's line: %q", bp360TextOf(last))
	}
}
