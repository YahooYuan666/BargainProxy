package qoder

import (
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// BP-70: the Qoder chat payload speaks OpenAI shapes — tool messages must be
// plain strings even when the Anthropic ingress attached screenshot images.
func TestIRPayloadToolMessageIsPlainText(t *testing.T) {
	payload, err := irPayload(driver.ChatCall{Request: &ir.ChatRequest{
		Model: "GLM-5.3",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
			{Role: ir.RoleTool, ToolCallID: "c1", Content: []ir.ContentPart{
				{Kind: "text", Text: "done"},
				{Kind: "image", ImageMIME: "image/png", ImageB64: "QUJD"},
			}},
		},
	}})
	if err != nil {
		t.Fatalf("irPayload: %v", err)
	}
	msgs, _ := payload["messages"].([]any)
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["role"] != "tool" {
			continue
		}
		if s, ok := mm["content"].(string); !ok || !strings.HasPrefix(s, "done") {
			t.Fatalf("tool content = %#v, want a plain string opening with %q", mm["content"], "done")
		}
		// BP-337: the result now announces its images that follow. The BP-70
		// contract this test guards is the SHAPE (a plain string, no image
		// part), which still holds — the image data never rides the content.
		if s, _ := mm["content"].(string); strings.Contains(s, "QUJD") ||
			strings.Contains(s, "image_url") {
			t.Fatalf("image data leaked into the tool content: %q", s)
		}
		return
	}
	t.Fatal("tool message missing")
}
