package qoder

// BP-331 (plugins 2b5e7d66, qoder 0.2.69): Kimi on this lane rejects a
// call-only assistant turn whose content is null OR EMPTY — one space passes
// (workbuddy chat lane same fill, plugins #1213). The right layer is
// convertIncomingMessage: that is where the assistant wire message's content
// is derived, and the structured path is the one a call-only turn rides.

import (
	"encoding/json"
	"testing"
)

func TestBP331CallOnlyAssistantGetsSpace(t *testing.T) {
	// The structured path: a call-only assistant (tool_calls, no text).
	msg := map[string]interface{}{
		"role": "assistant",
		"tool_calls": []interface{}{
			map[string]interface{}{
				"id":   "c1",
				"type": "function",
				"function": map[string]interface{}{
					"name":      "shell",
					"arguments": "{}",
				},
			},
		},
	}
	out := convertIncomingMessage(msg, true, true)
	if out == nil {
		t.Fatal("conversion must produce a message")
	}
	raw, _ := json.Marshal(out)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	if c, _ := doc["content"].(string); c != " " {
		t.Fatalf("call-only assistant content = %v, want a single space (raw %s)", doc["content"], raw)
	}
}

// An assistant whose text IS the tool-call wire form folds to empty content —
// the same fill applies (parseToolCallsText branch).
func TestBP331TextFormCallOnlyGetsSpace(t *testing.T) {
	msg := map[string]interface{}{
		"role": "assistant",
		"content": "Tool calls:\n" + `[{"id":"c1","type":"function","function":{"name":"shell","arguments":"{}"}}]`,
	}
	out := convertIncomingMessage(msg, true, true)
	raw, _ := json.Marshal(out)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	if c, _ := doc["content"].(string); c != " " {
		t.Fatalf("text-form call-only content = %v, want a single space (raw %s)", doc["content"], raw)
	}
}
