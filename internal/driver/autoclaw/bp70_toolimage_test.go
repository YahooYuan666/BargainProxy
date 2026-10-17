package autoclaw

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// BP-70: the chat lane speaks OpenAI wire — tool messages must serialize as a
// plain string even when the Anthropic ingress attached screenshot parts.
func TestBuildBodyToolMessageIsPlainText(t *testing.T) {
	call := driver.ChatCall{Request: &ir.ChatRequest{
		Model: "glm-5.3",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
			{Role: ir.RoleTool, ToolCallID: "c1", Content: []ir.ContentPart{
				{Kind: "text", Text: "done"},
				{Kind: "image", ImageMIME: "image/png", ImageB64: "QUJD"},
			}},
		},
	}}
	raw, _, err := buildBody(call)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var doc struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, m := range doc.Messages {
		if m.Role != "tool" {
			continue
		}
		var s string
		if err := json.Unmarshal(m.Content, &s); err != nil {
			t.Fatalf("tool content must be a plain string, got %s", m.Content)
		}
		if s != "done" {
			t.Fatalf("tool content = %q, want %q (image dropped)", s, "done")
		}
		return
	}
	t.Fatal("tool message missing")
}
