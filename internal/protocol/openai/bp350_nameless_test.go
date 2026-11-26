// BP-350 (magpie 33b04b64, #7565): every protocol writer must leave out a call
// that names no function. A guard in one writer leaves the other three open,
// and the shape poisons a whole session once it reaches the client.
package openai

import (
	"testing"

	"bargainproxy/internal/ir"
)

func namelessResp(calls ...ir.ToolCall) *ir.ChatResponse {
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, ToolCalls: calls},
		StopReason: ir.StopToolUse,
	}
}

func TestBP350CompletionDropsNamelessCall(t *testing.T) {
	resp := namelessResp(
		ir.ToolCall{ID: "c1", Name: "", Arguments: `{"a":1}`}, // the poisonous shape
		ir.ToolCall{ID: "c2", Name: "Bash", Arguments: `{"command":"ls"}`},
	)
	out := RenderCompletion("id", "m", 1, resp)
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d", len(out.Choices))
	}
	calls := out.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].Function.Name != "Bash" {
		t.Fatalf("calls = %+v, want only the named one", calls)
	}
	// A real call remains, so the round still ends for tools.
	if got := *out.Choices[0].FinishReason; got != "tool_calls" {
		t.Fatalf("finish = %q, want tool_calls", got)
	}
}

// The headline damage: all calls nameless. The turn must end "stop" — an agent
// told "tool_calls" with an empty list waits for a call that does not exist.
func TestBP350CompletionOnlyNamelessEndsStop(t *testing.T) {
	resp := namelessResp(ir.ToolCall{ID: "c1", Name: "", Arguments: `{}`})
	out := RenderCompletion("id", "m", 1, resp)
	if len(out.Choices[0].Message.ToolCalls) != 0 {
		t.Fatalf("calls = %+v", out.Choices[0].Message.ToolCalls)
	}
	if got := *out.Choices[0].FinishReason; got != "stop" {
		t.Fatalf("finish = %q, want stop", got)
	}
}

// A clean reply is untouched (byte-for-byte equivalence of intent).
func TestBP350CompletionCleanReplyUnchanged(t *testing.T) {
	resp := namelessResp(ir.ToolCall{ID: "c1", Name: "Bash", Arguments: `{"command":"ls"}`})
	out := RenderCompletion("id", "m", 1, resp)
	if len(out.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("calls = %+v", out.Choices[0].Message.ToolCalls)
	}
	if got := *out.Choices[0].FinishReason; got != "tool_calls" {
		t.Fatalf("finish = %q", got)
	}
}
