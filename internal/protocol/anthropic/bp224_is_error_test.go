package anthropic

import (
	"testing"

	"bargainproxy/internal/ir"
)

// BP-224: tool_result.is_error must ride the IR. Strict bool semantics (the
// upstream translator tests treat string/number values as success).
func TestBP224ParseToolResultIsError(t *testing.T) {
	body := `{
		"model": "m", "max_tokens": 16,
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t1", "is_error": true, "content": "permission denied"},
				{"type": "tool_result", "tool_use_id": "t2", "is_error": false, "content": "ok"},
				{"type": "tool_result", "tool_use_id": "t3", "content": "no flag"},
				{"type": "tool_result", "tool_use_id": "t4", "is_error": "true", "content": "non-bool means success"}
			]}
		]
	}`
	req, err := ParseMessagesRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := make(map[string]bool) // tool_use_id → IsError
	for _, m := range req.Messages {
		if m.Role == ir.RoleTool {
			got[m.ToolCallID] = m.IsError
		}
	}
	want := map[string]bool{"t1": true, "t2": false, "t3": false, "t4": false}
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("tool %s IsError = %v, want %v", id, got[id], w)
		}
	}
}
