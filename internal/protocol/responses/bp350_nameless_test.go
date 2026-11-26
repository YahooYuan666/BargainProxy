// BP-350 (magpie 33b04b64, #7565) on the Responses and Gemini surfaces: a
// call naming no function must never reach the client, whatever protocol the
// client speaks.
package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

func namelessResp(calls ...ir.ToolCall) *ir.ChatResponse {
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, ToolCalls: calls},
		StopReason: ir.StopToolUse,
	}
}

func outputTypes(t *testing.T, out *ResponseObject) (functionCalls int, names []string) {
	t.Helper()
	for _, item := range out.Output {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == "function_call" {
			functionCalls++
			if s, _ := m["name"].(string); s != "" {
				names = append(names, s)
			} else {
				t.Errorf("a function_call with no name reached the client: %+v", m)
			}
		}
	}
	return
}

func TestBP350ResponsesDropsNamelessCall(t *testing.T) {
	out := Render("id", "m", 1, namelessResp(
		ir.ToolCall{ID: "c1", Name: "", Arguments: `{"a":1}`},
		ir.ToolCall{ID: "c2", Name: "Bash", Arguments: `{"command":"ls"}`},
	), "")
	n, names := outputTypes(t, out)
	if n != 1 || len(names) != 1 || names[0] != "Bash" {
		t.Fatalf("function_calls = %d names = %v, want 1 Bash", n, names)
	}
}

func TestBP350ResponsesOnlyNamelessEmitsNothing(t *testing.T) {
	out := Render("id", "m", 1, namelessResp(
		ir.ToolCall{ID: "c1", Name: "", Arguments: `{}`},
	), "")
	if n, _ := outputTypes(t, out); n != 0 {
		t.Fatalf("function_calls = %d, want 0", n)
	}
	// The raw JSON must not carry an empty name either.
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), `"name":""`) {
		t.Fatalf("a nameless function_call is on the wire: %s", b)
	}
}

func TestBP350ResponsesCleanReplyUnchanged(t *testing.T) {
	out := Render("id", "m", 1, namelessResp(
		ir.ToolCall{ID: "c1", Name: "Bash", Arguments: `{"command":"ls"}`},
	), "")
	if n, names := outputTypes(t, out); n != 1 || names[0] != "Bash" {
		t.Fatalf("function_calls = %d names = %v", n, names)
	}
}
