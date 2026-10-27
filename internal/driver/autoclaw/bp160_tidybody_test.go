package autoclaw

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

// BP-160: the CN gateway 500s ({"message":"parse response failed"}) on
// message shapes its own client never sends — missing content keys, empty
// argument strings (plugins autoclaw 0.2.9 tidyBody, issue #437).
func TestBP160TidyBody(t *testing.T) {
	call := driver.ChatCall{
		Account:       &model.Account{ID: "ac-tb", Channel: model.ChannelAutoClaw},
		UpstreamModel: "zai_glm-5.3-flash",
		Request: &ir.ChatRequest{
			Model: "zai_glm-5.3-flash",
			Messages: []ir.Message{
				// An assistant turn that only carries tool calls: the old
				// builder emitted no content key at all.
				{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{
					{ID: "call_1", Name: "read_file", Arguments: ""},
				}},
				{Role: ir.RoleTool, ToolCallID: "call_1", Content: []ir.ContentPart{{Kind: "text", Text: "file body"}}},
			},
		},
	}
	raw, _, err := buildBodyWith(call, false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var doc struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("json: %v", err)
	}
	first := doc.Messages[0]
	if _, ok := first["content"]; !ok {
		t.Fatal("assistant tool-call turn lost its content key — the gateway parses that as a 500")
	}
	if first["content"] != "" {
		t.Fatalf("content = %v, want empty string", first["content"])
	}
	tcs, _ := first["tool_calls"].([]any)
	fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != "{}" {
		t.Fatalf("arguments = %v, want the JSON object string", fn["arguments"])
	}
}

// A normal turn is untouched by the tidy-up.
func TestBP160TidyBodyLeavesNormalShapes(t *testing.T) {
	call := driver.ChatCall{
		Account:       &model.Account{ID: "ac-tb", Channel: model.ChannelAutoClaw},
		UpstreamModel: "zai_glm-5.3-flash",
		Request: &ir.ChatRequest{
			Model: "zai_glm-5.3-flash",
			Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hello"}}},
			},
		},
	}
	raw, _, err := buildBodyWith(call, false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var doc struct {
		Messages []map[string]any `json:"messages"`
	}
	_ = json.Unmarshal(raw, &doc)
	if doc.Messages[0]["content"] != "hello" {
		t.Fatalf("content = %v", doc.Messages[0]["content"])
	}
}
