package qoder

import (
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
		if s, ok := mm["content"].(string); !ok || s != "done" {
			t.Fatalf("tool content = %#v, want plain string %q", mm["content"], "done")
		}
		return
	}
	t.Fatal("tool message missing")
}
