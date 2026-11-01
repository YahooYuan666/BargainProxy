package qoder

import (
	"strings"
	"testing"
)

// BP-262 (qoder2api-hub #124):
// 1. A stream frame carrying both content and tool_calls must preserve content.
// 2. A stream whose tool calls all lack function names must downgrade finish_reason to "stop".
func TestBP262PreservesContentWithToolCalls(t *testing.T) {
	var emitted []string
	acc := newStreamAccumulator("r1", 1000, "test-model", false, func(chunk string) {
		emitted = append(emitted, chunk)
	})

	delta := &bridgeDelta{
		Content: "I will call the tool now.",
		ToolCalls: []map[string]interface{}{
			{
				"index": float64(0),
				"id":    "call_1",
				"type":  "function",
				"function": map[string]interface{}{
					"name":      "bash",
					"arguments": "{\"command\":\"ls\"}",
				},
			},
		},
	}
	acc.accept(delta)

	if len(emitted) == 0 {
		t.Fatal("expected at least one SSE chunk emitted")
	}
	combined := strings.Join(emitted, "")
	if !strings.Contains(combined, "I will call the tool now.") {
		t.Fatalf("content was dropped from same-frame tool_call chunk: %s", combined)
	}
	if !strings.Contains(combined, "bash") {
		t.Fatalf("tool_call was missing from chunk: %s", combined)
	}
	if acc.finalFinishReason() != "tool_calls" {
		t.Fatalf("finalFinishReason = %q, want tool_calls", acc.finalFinishReason())
	}
}

func TestBP262NamelessToolCallDowngradesToStop(t *testing.T) {
	acc := newStreamAccumulator("r2", 1000, "test-model", false, func(string) {})

	// Nameless call: arguments only, no function name.
	delta := &bridgeDelta{
		FinishReason: "tool_calls",
		ToolCalls: []map[string]interface{}{
			{
				"index": float64(0),
				"id":    "call_incomplete",
				"type":  "function",
				"function": map[string]interface{}{
					"name":      "",
					"arguments": "{\"some\":",
				},
			},
		},
	}
	acc.accept(delta)

	if got := acc.finalFinishReason(); got != "stop" {
		t.Fatalf("finalFinishReason for nameless calls = %q, want stop", got)
	}

	// Non-stream aggregate check
	chunks := []string{
		`data: {"id":"r2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":"{}"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"r2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	res := aggregateNonStream(chunks, "r2", 1000, "test-model", true)
	choices, _ := res["choices"].([]map[string]interface{})
	if len(choices) == 0 {
		t.Fatal("no choices in response")
	}
	choice := choices[0]
	if choice["finish_reason"] != "stop" {
		t.Fatalf("non-stream finish_reason = %v, want stop", choice["finish_reason"])
	}
	msg, _ := choice["message"].(map[string]interface{})
	if _, has := msg["tool_calls"]; has {
		t.Fatalf("non-stream message should not carry nameless tool_calls: %v", msg["tool_calls"])
	}
}
