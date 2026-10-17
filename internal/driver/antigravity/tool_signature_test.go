// Gemini 3 thinking models require a thought_signature on replayed
// functionCall parts; OpenAI-protocol clients cannot carry it, so the
// replayed part is stamped with Google's documented placeholder (用户实测
// 2026-10-16：gemini-3.8-flash-high + MCP 工具调用 → 400
// "Function call is missing a thought_signature in functionCall parts").
package antigravity

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/ir"
)

type wirePart struct {
	Text             string          `json:"text"`
	Thought          bool            `json:"thought"`
	ThoughtSignature string          `json:"thoughtSignature"`
	FunctionCall     json.RawMessage `json:"functionCall"`
	FunctionResp     json.RawMessage `json:"functionResponse"`
}

func bodyContents(t *testing.T, body string) []struct {
	Role  string     `json:"role"`
	Parts []wirePart `json:"parts"`
} {
	t.Helper()
	var outer struct {
		Request struct {
			Contents []struct {
				Role  string     `json:"role"`
				Parts []wirePart `json:"parts"`
			} `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(body), &outer); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	return outer.Request.Contents
}

func TestReplayedToolCallsCarryThoughtSignatureSentinel(t *testing.T) {
	req := baseReq()
	req.Messages = append(req.Messages,
		ir.Message{
			Role:      ir.RoleAssistant,
			ToolCalls: []ir.ToolCall{{ID: "call_1", Name: "get_weather", Arguments: `{"city":"BJ"}`}},
		},
		ir.Message{
			Role: ir.RoleTool, ToolCallID: "call_1", ToolName: "get_weather",
			Content: []ir.ContentPart{{Kind: "text", Text: `{"temp":30}`}},
		},
	)
	contents := bodyContents(t, buildBody(t, "gemini-3.8-flash-high", req))

	var sawCall, sawResp bool
	for _, c := range contents {
		for _, p := range c.Parts {
			switch {
			case len(p.FunctionCall) > 0:
				sawCall = true
				if c.Role != "model" {
					t.Fatalf("functionCall part on role %q, want model", c.Role)
				}
				if p.ThoughtSignature != skipThoughtSignature {
					t.Fatalf("functionCall thoughtSignature = %q, want %q (复放工具调用必须带哨兵签名)",
						p.ThoughtSignature, skipThoughtSignature)
				}
			case len(p.FunctionResp) > 0:
				sawResp = true
				if p.ThoughtSignature != "" {
					t.Fatalf("functionResponse must not carry a thoughtSignature, got %q", p.ThoughtSignature)
				}
			default:
				if p.ThoughtSignature != "" {
					t.Fatalf("non-call part carries thoughtSignature: %+v", p)
				}
			}
		}
	}
	if !sawCall || !sawResp {
		t.Fatalf("expected both functionCall and functionResponse parts, saw call=%v resp=%v", sawCall, sawResp)
	}
}

// The live model turn we stream back to the client must NOT be fabricated
// with a sentinel — only the replayed history gets stamped.
func TestModelOutputPartsAreUntouched(t *testing.T) {
	req := baseReq()
	contents := bodyContents(t, buildBody(t, "gemini-3.8-flash-high", req))
	for _, c := range contents {
		for _, p := range c.Parts {
			if p.ThoughtSignature != "" {
				t.Fatalf("plain user turn carries thoughtSignature: %+v", p)
			}
		}
	}
}
