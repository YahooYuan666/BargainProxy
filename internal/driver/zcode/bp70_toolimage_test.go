package zcode

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// BP-70: a tool result with text + image renders as an Anthropic tool_result
// whose content is a block array; text-only results must stay plain strings.
func TestNormalizeBodyToolResultImage(t *testing.T) {
	d := testDriver("http://unused")
	call := driver.ChatCall{
		UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{
			Model: "glm-5.3",
			Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "screenshot?"}}},
				{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "call_1", Name: "shot", Arguments: `{}`}}},
				{Role: ir.RoleTool, ToolCallID: "call_1", Content: []ir.ContentPart{
					{Kind: "text", Text: "done"},
					{Kind: "image", ImageMIME: "image/png", ImageB64: "QUJD"},
				}},
			},
		},
	}
	raw, _, _, err := d.normalizeBody(call, &Creds{JWT: makeJWT("u1")}, true)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tr := toolResultBlock(t, doc)
	content, ok := tr["content"].([]any)
	if !ok {
		t.Fatalf("tool_result content must be an array when images ride along: %v", tr["content"])
	}
	var sawText, sawImage bool
	for _, c := range content {
		cc, _ := c.(map[string]any)
		switch cc["type"] {
		case "text":
			sawText = cc["text"] == "done"
		case "image":
			src, _ := cc["source"].(map[string]any)
			sawImage = src["type"] == "base64" && src["data"] == "QUJD" && src["media_type"] == "image/png"
		}
	}
	if !sawText || !sawImage {
		t.Fatalf("blocks = %v", content)
	}
}

func TestNormalizeBodyToolResultTextOnlyStaysString(t *testing.T) {
	d := testDriver("http://unused")
	call := driver.ChatCall{
		UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{
			Model: "glm-5.3",
			Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
				{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "call_1", Name: "f", Arguments: `{}`}}},
				{Role: ir.RoleTool, ToolCallID: "call_1", Content: []ir.ContentPart{{Kind: "text", Text: "18C"}}},
			},
		},
	}
	raw, _, _, err := d.normalizeBody(call, &Creds{JWT: makeJWT("u1")}, true)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tr := toolResultBlock(t, doc)
	if s, ok := tr["content"].(string); !ok || s != "18C" {
		t.Fatalf("text-only tool_result must stay a string, got %#v", tr["content"])
	}
}

func toolResultBlock(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	msgs, _ := doc["messages"].([]any)
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		content, _ := mm["content"].([]any)
		for _, c := range content {
			cc, _ := c.(map[string]any)
			if cc["type"] == "tool_result" {
				return cc
			}
		}
	}
	t.Fatal("no tool_result block in body")
	return nil
}
