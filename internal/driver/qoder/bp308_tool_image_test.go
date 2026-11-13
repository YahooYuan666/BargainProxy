// BP-308: a tool's returned images must reach the model.
//
// An OpenAI tool message carries a plain string, so a tool's image parts used
// to be dropped and the model answered "the read returned no image data" — a
// blank result with no error anywhere (2026-11-11). The images now ride a
// following user turn, or a new user turn when the request ends on the tool
// result.
package qoder

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

const bp308PNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func bp308Build(t *testing.T, withFollowingUserTurn bool) map[string]any {
	t.Helper()
	msgs := []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "读取 blue.png"}}},
		{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "call_x", Name: "Read", Arguments: `{"file_path":"blue.png"}`}}},
		{Role: ir.RoleTool, ToolCallID: "call_x", ToolName: "Read", Content: []ir.ContentPart{
			{Kind: "image", ImageMIME: "image/png", ImageB64: bp308PNG},
		}},
	}
	if withFollowingUserTurn {
		msgs = append(msgs, ir.Message{Role: ir.RoleUser,
			Content: []ir.ContentPart{{Kind: "text", Text: "它是什么颜色？"}}})
	}
	req := &ir.ChatRequest{Model: "m", Messages: msgs, MaxTokens: 100}
	payload, err := irPayload(driver.ChatCall{Request: req, UpstreamModel: "m"})
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return body
}

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
			if iu, ok := pm["image_url"].(map[string]any); ok {
				if u, _ := iu["url"].(string); u != "" {
					out = append(out, u)
				}
			}
		}
	}
	return out
}

func TestBP308TrailingToolImageReachesWire(t *testing.T) {
	urls := bp308ImageURLs(bp308Build(t, false))
	if len(urls) != 1 {
		t.Fatalf("image_url parts = %d, want 1 (tool image dropped)", len(urls))
	}
	if !strings.Contains(urls[0], bp308PNG) {
		t.Fatal("image payload altered")
	}
	for _, m := range bp308Build(t, false)["messages"].([]any) {
		mm := m.(map[string]any)
		if mm["role"] != "tool" {
			continue
		}
		if _, ok := mm["content"].(string); !ok {
			t.Fatalf("tool message content is not a string: %T", mm["content"])
		}
	}
}

func TestBP308ImageJoinsFollowingUserTurn(t *testing.T) {
	body := bp308Build(t, true)
	if urls := bp308ImageURLs(body); len(urls) != 1 {
		t.Fatalf("image_url parts = %d, want 1", len(urls))
	}
	msgs := body["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if txt, _ := json.Marshal(last); !strings.Contains(string(txt), "什么颜色") {
		t.Fatal("the user's text was lost while merging the image in")
	}
}

func TestBP308PlainToolResultUnchanged(t *testing.T) {
	req := &ir.ChatRequest{Model: "m", MaxTokens: 100, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "ls"}}},
		{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "c1", Name: "Bash", Arguments: `{}`}}},
		{Role: ir.RoleTool, ToolCallID: "c1", ToolName: "Bash",
			Content: []ir.ContentPart{{Kind: "text", Text: "a.go"}}},
	}}
	payload, err := irPayload(driver.ChatCall{Request: req, UpstreamModel: "m"})
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	raw, _ := json.Marshal(payload)
	if strings.Contains(string(raw), toolImageCarrier) {
		t.Fatal("a text-only tool result grew a carrier turn")
	}
}
