package anthropic

import "testing"

// TestEffortReachesIRLikeZCodeSends is the end-to-end shape check: the exact
// body ZCode 3.17.2 sends, through the parser, to the value the driver turns
// into reasoning_effort.
//
// This is the regression that matters. The unit tests above prove the parser
// reads output_config; this one proves the parser reads the REAL body — field
// order, system blocks, 115 tools and all — so a future change to any of those
// cannot quietly reintroduce the downgrade.
func TestEffortReachesIRLikeZCodeSends(t *testing.T) {
	body := []byte(`{
		"model": "deepseek-v4.1-flash",
		"max_tokens": 384000,
		"metadata": {"user_id": "u"},
		"system": [{"type":"text","text":"You are ZCode, an interactive agent"}],
		"messages": [{"role":"user","content":[{"type":"text","text":"hi"}]}],
		"tools": [{"name":"Bash","description":"run","input_schema":{"type":"object"}}],
		"tool_choice": {"type":"auto"},
		"stream": true,
		"thinking": {"type": "enabled"},
		"output_config": {"effort": "max"}
	}`)
	req, err := ParseMessagesRequest(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.RequestedEffort != "max" {
		t.Fatalf("RequestedEffort = %q, want max (the driver sends this as reasoning_effort)", req.RequestedEffort)
	}
	if req.Thinking == nil || !req.Thinking.Enabled {
		t.Fatalf("thinking must stay enabled: %+v", req.Thinking)
	}
	if req.Thinking.Level != "max" {
		t.Fatalf("Thinking.Level = %q, want max", req.Thinking.Level)
	}
}
