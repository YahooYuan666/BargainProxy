// BP-308: a tool's returned images must reach the model.
//
// The Read tool answers with a picture. OpenAI chat's tool message carries a
// plain string only, so the workbuddy chat lane used to drop the image parts
// outright — the model then reported "the read returned no image data", a
// blank result with no error anywhere (2026-11-11 report against
// deepseek-v4.1-flash, which reads user-attached images fine).
//
// The images are now parked and re-attached to the next user turn (or a fresh
// user turn when the request ends on the tool result) as ordinary multimodal
// parts, which this upstream accepts.
package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

const bp308PNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// bp308Body builds the classic read-then-answer shape and returns the wire
// body the driver would send.
func bp308Body(t *testing.T, withFollowingUserTurn bool) map[string]any {
	t.Helper()
	msgs := []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "读取 blue.png 并告诉我颜色"}}},
		{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "call_x", Name: "Read", Arguments: `{"file_path":"blue.png"}`}}},
		{Role: ir.RoleTool, ToolCallID: "call_x", ToolName: "Read", Content: []ir.ContentPart{
			{Kind: "image", ImageMIME: "image/png", ImageB64: bp308PNG},
		}},
	}
	if withFollowingUserTurn {
		msgs = append(msgs, ir.Message{Role: ir.RoleUser,
			Content: []ir.ContentPart{{Kind: "text", Text: "读到了，它是什么颜色？"}}})
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
	return body
}

// bp308ImageURLs collects every image_url data URI in the wire body.
func bp308ImageURLs(body map[string]any) []string {
	var out []string
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		parts, _ := mm["content"].([]any)
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if pm["type"] != "image_url" {
				continue
			}
			iu, _ := pm["image_url"].(map[string]any)
			if u, _ := iu["url"].(string); u != "" {
				out = append(out, u)
			}
		}
	}
	return out
}

// TestBP308TrailingToolImageReachesWire the request ENDS on the tool result
// (the shape when the client asks and waits). The image must still leave.
func TestBP308TrailingToolImageReachesWire(t *testing.T) {
	body := bp308Body(t, false)
	urls := bp308ImageURLs(body)
	if len(urls) != 1 {
		t.Fatalf("image_url parts on the wire = %d, want 1 (the tool's image was dropped)", len(urls))
	}
	if !strings.HasPrefix(urls[0], "data:image/png;base64,") {
		t.Fatalf("image not a data URI: %.80s", urls[0])
	}
	if !strings.Contains(urls[0], bp308PNG) {
		t.Fatal("image payload altered in transit")
	}
	// The tool message itself must stay a plain string (OpenAI requires it).
	for _, m := range body["messages"].([]any) {
		mm := m.(map[string]any)
		if mm["role"] != "tool" {
			continue
		}
		if _, isString := mm["content"].(string); !isString {
			t.Fatalf("tool message content is not a string: %T", mm["content"])
		}
		if strings.Contains(mm["content"].(string), "image_url") {
			t.Fatal("image leaked back into the tool message string")
		}
	}
}

// TestBP308ImageJoinsFollowingUserTurn when a user turn follows, the image
// rides WITH it instead of creating a separate user message.
func TestBP308ImageJoinsFollowingUserTurn(t *testing.T) {
	body := bp308Body(t, true)
	urls := bp308ImageURLs(body)
	if len(urls) != 1 {
		t.Fatalf("image_url parts = %d, want 1", len(urls))
	}
	msgs := body["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("last message role = %v, want user", last["role"])
	}
	parts, _ := last["content"].([]any)
	hasImage := false
	hasQuestion := false
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		if pm["type"] == "image_url" {
			hasImage = true
		}
		if pm["type"] == "text" {
			if txt, _ := pm["text"].(string); strings.Contains(txt, "什么颜色") {
				hasQuestion = true
			}
		}
	}
	if !hasImage {
		t.Fatal("the following user turn did not carry the tool's image")
	}
	if !hasQuestion {
		t.Fatal("the user's own text was lost while merging the image in")
	}
}

// TestBP308PlainToolResultUnchanged zero impact: a text-only tool result must
// not sprout a carrier turn.
func TestBP308PlainToolResultUnchanged(t *testing.T) {
	req := &ir.ChatRequest{Model: "deepseek-v4.1-flash", MaxTokens: 100, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "run ls"}}},
		{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "c1", Name: "Bash", Arguments: `{"command":"ls"}`}}},
		{Role: ir.RoleTool, ToolCallID: "c1", ToolName: "Bash",
			Content: []ir.ContentPart{{Kind: "text", Text: "a.go\nb.go"}}},
	}}
	raw, err := testDriver("http://unused").buildBody(driver.ChatCall{Request: req}, testCreds(), false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if strings.Contains(string(raw), toolImageCarrier) {
		t.Fatal("a text-only tool result grew a carrier turn")
	}
	if strings.Contains(string(raw), "image_url") {
		t.Fatal("a text-only tool result produced an image part")
	}
}
