package openaicompat

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// BP-70: tool messages ride into the Anthropic ingress with image parts (MCP
// screenshots), but OpenAI tool messages only accept a plain string — the
// image must be text-only, never a parts array (upstream would 400).
func TestBuildBodyToolMessageIsPlainText(t *testing.T) {
	call := driver.ChatCall{Request: &ir.ChatRequest{
		Model: "m",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "c1", Name: "f", Arguments: `{}`}}},
			{Role: ir.RoleTool, ToolCallID: "c1", Content: []ir.ContentPart{
				{Kind: "text", Text: "done"},
				{Kind: "image", ImageMIME: "image/png", ImageB64: "QUJD"},
			}},
		},
	}}
	raw, err := buildBody(call)
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
	var toolContent string
	for _, m := range doc.Messages {
		if m.Role == "tool" {
			if err := json.Unmarshal(m.Content, &toolContent); err != nil {
				t.Fatalf("tool content must be a plain string, got %s", m.Content)
			}
		}
	}
	if toolContent != "done" {
		t.Fatalf("tool content = %q, want %q (text only, image dropped)", toolContent, "done")
	}
}
