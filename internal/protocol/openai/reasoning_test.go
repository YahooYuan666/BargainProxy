// BP-41: non-streamed reasoning surfaces as reasoning_content.
package openai

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/ir"
)

func TestRenderCompletionCarriesReasoningContent(t *testing.T) {
	resp := &ir.ChatResponse{
		StopReason: ir.StopEnd,
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "答案"}}},
		Thinking:   "推理过程",
	}
	out := RenderCompletion("id1", "m", 1, resp)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if got := wire.Choices[0].Message.ReasoningContent; got != "推理过程" {
		t.Fatalf("reasoning_content = %q, want 推理过程", got)
	}
	if wire.Choices[0].Message.Content != "答案" {
		t.Fatalf("content = %q", wire.Choices[0].Message.Content)
	}
}

func TestRenderCompletionNoThinkingOmitsField(t *testing.T) {
	resp := &ir.ChatResponse{
		StopReason: ir.StopEnd,
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "答案"}}},
	}
	raw, _ := json.Marshal(RenderCompletion("id1", "m", 1, resp))
	if bytesContains(raw, "reasoning_content") {
		t.Fatalf("reasoning_content must be omitted when empty: %s", raw)
	}
}

func bytesContains(b []byte, s string) bool {
	return len(b) > 0 && (func() bool {
		for i := 0; i+len(s) <= len(b); i++ {
			if string(b[i:i+len(s)]) == s {
				return true
			}
		}
		return false
	})()
}
