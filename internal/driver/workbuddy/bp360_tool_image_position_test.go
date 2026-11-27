// BP-360: a tool's images sit where the tool left them — their own message
// right after the tool result, flushed when the model's next turn begins.
//
// BP-337 gave the carrying message its attribution ("[From the result of
// Read (tool call ...):]") but left the FLUSH POINT alone: the parked images
// were attached to the next user turn. On 2026-11-27 that produced the exact
// confusion it was meant to end — an image the gateway read off disk arrived
// on the user's next line, and both sides read it as a picture the user had
// just pasted ("没给你发呀"). The client replays the whole history every
// turn, so a parked image walks forward with each turn and always sits on
// the newest user line.
//
// magpie flushes at the head of every assistant turn (internal/gateway/
// chat.go:488 showSeen). The image's position is then fixed at the moment it
// was returned and never moves again.
package workbuddy

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
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "读取 blue.png 并告诉我颜色"}}},
		{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "call_x", Name: "Read", Arguments: `{"file_path":"blue.png"}`}}},
		{Role: ir.RoleTool, ToolCallID: "call_x", ToolName: "Read", Content: []ir.ContentPart{
			{Kind: "image", ImageMIME: "image/png", ImageB64: bp308PNG},
		}},
		{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "我看完图了，这是结论"}}},
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "第二句用户话"}}},
	}
	req := &ir.ChatRequest{Model: "deepseek-v4.1-flash", Messages: msgs, MaxTokens: 100}
	raw, err := testDriver("http://unused").buildBody(driver.ChatCall{Request: req}, testCreds(), false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return bp337Messages(body)
}

// bp360CarrierAt returns the index of the message carrying the tool's images,
// or -1.
func bp360CarrierAt(wire []map[string]any) int {
	for i, m := range wire {
		if strings.Contains(bp337TextOf(m), "From the result of") {
			return i
		}
	}
	return -1
}

// TestBP360ImageKeepsItsPlace: the tool's image must own a message, and the
// user's later line must carry no image of its own.
func TestBP360ImageKeepsItsPlace(t *testing.T) {
	wire := bp360Wire(t)

	imageAt := bp360CarrierAt(wire)
	if imageAt < 0 {
		t.Fatal("the tool's image never reached the wire")
	}

	last := wire[len(wire)-1]
	if last["role"] != "user" {
		t.Fatalf("last message is %v, want the user's line", last["role"])
	}
	if strings.Contains(bp337TextOf(last), "From the result of") {
		t.Error("the user's line carries the tool's image — that is what read as a fresh paste")
	}
	if !strings.Contains(bp337TextOf(last), "第二句用户话") {
		t.Errorf("last message is not the user's line: %q", bp337TextOf(last))
	}
}

// TestBP360ImageBeforeAnswerNotAfter: the flushed message must land between
// the tool result and the answer, i.e. immediately after the tool message.
// Parking until the user speaks put it after the answer and onto the user's
// line instead.
func TestBP360ImageBeforeAnswerNotAfter(t *testing.T) {
	wire := bp360Wire(t)
	imageAt, toolAt, answerAt := bp360CarrierAt(wire), -1, -1
	for i, m := range wire {
		if m["role"] == "tool" {
			toolAt = i
		}
		if strings.Contains(bp337TextOf(m), "我看完图了") {
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
}
