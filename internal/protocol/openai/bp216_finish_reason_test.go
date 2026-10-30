package openai

import (
	"testing"

	"bargainproxy/internal/ir"
)

// BP-216: the OpenAI wire vocabulary spells a tool round "tool_calls". IR uses
// Anthropic's word ("tool_use"), and emitting it raw made strict clients reject
// a complete tool round — OpenMausBot throws "provider tool calls were truncated
// or malformed" for any finish_reason other than "tool_calls"/"stop".
func TestBP216FinishReasonUsesOpenAIVocabulary(t *testing.T) {
	cases := []struct {
		in   ir.StopReason
		want string
	}{
		{ir.StopEnd, "stop"},
		{ir.StopToolUse, "tool_calls"},
		{ir.StopLength, "length"},
		{ir.StopContentFilter, "content_filter"},
	}
	for _, c := range cases {
		if got := FinishReason(c.in); got != c.want {
			t.Errorf("FinishReason(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The rendered body must never leak the IR spelling to an OpenAI client.
func TestBP216RenderCompletionNeverLeaksToolUse(t *testing.T) {
	resp := &ir.ChatResponse{
		StopReason: ir.StopToolUse,
		Message: ir.Message{
			Content:   []ir.ContentPart{{Kind: "text", Text: ""}},
			ToolCalls: []ir.ToolCall{{ID: "call_1", Name: "Bash", Arguments: `{"cmd":"ls"}`}},
		},
	}
	out := RenderCompletion("id", "m", 0, resp)
	if out.Choices[0].FinishReason == nil || *out.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", out.Choices[0].FinishReason)
	}
}
