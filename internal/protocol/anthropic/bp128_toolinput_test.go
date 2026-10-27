package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

// BP-128: tool arguments pass through as raw JSON. Unmarshalling into
// map[string]any turned every number into float64, so a 64-bit id rounded
// on the way out (new-api 4d962e4 hit the same wire bug).
func TestToolArgumentsKeepLargeIntegers(t *testing.T) {
	const big = `{"id":9007199254740993,"note":"keep me"}`
	resp := &ir.ChatResponse{
		Message: ir.Message{
			Role: ir.RoleAssistant,
			ToolCalls: []ir.ToolCall{{ID: "t1", Name: "lookup", Arguments: big}},
		},
	}
	out := RenderMessage("msg_1", "m", resp)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "9007199254740993") {
		t.Fatalf("large integer lost: %s", raw)
	}
	if strings.Contains(string(raw), "900719925474099") && strings.Contains(string(raw), "92") && !strings.Contains(string(raw), "9007199254740993") {
		t.Fatalf("rounded: %s", raw)
	}
}

// Broken argument JSON still degrades to an empty object, not a 500.
func TestToolArgumentsInvalidJSONDegrades(t *testing.T) {
	resp := &ir.ChatResponse{
		Message: ir.Message{
			Role:      ir.RoleAssistant,
			ToolCalls: []ir.ToolCall{{ID: "t1", Name: "lookup", Arguments: `{not json`}},
		},
	}
	out := RenderMessage("msg_1", "m", resp)
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"input":{}`) {
		t.Fatalf("invalid JSON must degrade to {}, got: %s", raw)
	}
}
