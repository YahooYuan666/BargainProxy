package workbuddy

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// BP-70: tool messages entering via the Anthropic ingress may carry image
// parts, but the OpenAI-shaped tool message this channel speaks accepts a
// plain string only.
func TestBuildBodyToolMessageIsPlainText(t *testing.T) {
	d := testDriver("http://unused")
	call := driver.ChatCall{Request: &ir.ChatRequest{
		Model: "glm-5.3",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "c1", Name: "f", Arguments: `{}`}}},
			{Role: ir.RoleTool, ToolCallID: "c1", Content: []ir.ContentPart{
				{Kind: "text", Text: "done"},
				{Kind: "image", ImageMIME: "image/png", ImageB64: "QUJD"},
			}},
		},
	}}
	creds := &Creds{AccessToken: fakeToken("access"), UID: "u-100", Realm: "cn"}
	raw, err := d.buildBody(call, creds)
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
	found := false
	for _, m := range doc.Messages {
		if m.Role != "tool" {
			continue
		}
		found = true
		var s string
		if err := json.Unmarshal(m.Content, &s); err != nil {
			t.Fatalf("tool content must be a plain string, got %s", m.Content)
		}
		if s != "done" {
			t.Fatalf("tool content = %q, want %q (image dropped)", s, "done")
		}
	}
	if !found {
		t.Fatal("tool message missing from body")
	}
}
