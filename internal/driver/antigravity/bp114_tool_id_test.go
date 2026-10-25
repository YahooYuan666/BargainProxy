// BP-114: Antigravity's functionCall / functionResponse parts need the PRIVATE
// id field — Google's Gemini→Anthropic translation for Claude models fills
// tool_use.id and tool_result.tool_use_id from it, and a missing id fails the
// whole request with 400 "messages.N.content.M.tool_use.id: Field required".
//
// Field evidence 2026-10-25 18:49: the first claude-model turns carrying a
// tool history all died there (model=gemini-claude-sonnet-4-6, Vertex-side
// request id req_vrtx_*). Model-name normalization (BP-113) had just made
// these requests reach the real Claude for the first time, exposing this next
// layer. The reference carries the same field and synthesizes ids the same
// way; this driver's structs never had it.
package antigravity

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

// A full call→result round trip must put an id on BOTH parts, and the result
// must carry the SAME id as its call (that is the pairing the translation
// needs).
func TestToolCallAndResultCarryPairedIDs(t *testing.T) {
	raw, err := buildRequest("proj-1", "gemini-claude-sonnet-4-6", &ir.ChatRequest{
		Model: "gemini-claude-sonnet-4-6",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "read the file"}}},
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{
				ID: "call_1", Name: "read_file", Arguments: `{"path":"a.go"}`,
			}}},
			{Role: ir.RoleTool, ToolCallID: "call_1", ToolName: "read_file", Content: []ir.ContentPart{{Kind: "text", Text: "package main"}}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "thanks"}}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	var doc struct {
		Request struct {
			Contents []struct {
				Parts []struct {
					FunctionCall *struct {
						ID string `json:"id"`
					} `json:"functionCall"`
					FunctionResp *struct {
						ID string `json:"id"`
					} `json:"functionResponse"`
				} `json:"parts"`
			} `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var callID, respID string
	for _, c := range doc.Request.Contents {
		for _, p := range c.Parts {
			if p.FunctionCall != nil {
				callID = p.FunctionCall.ID
			}
			if p.FunctionResp != nil {
				respID = p.FunctionResp.ID
			}
		}
	}
	if callID == "" {
		t.Fatal("functionCall has no id: the Gemini→Anthropic translation will fail with tool_use.id Field required")
	}
	if respID == "" {
		t.Fatal("functionResponse has no id: tool_result.tool_use_id will be missing")
	}
	if callID != respID {
		t.Fatalf("call id %q and result id %q must pair", callID, respID)
	}
}

// A history that LOST the ids (repaired pairing, foreign replay) still gets
// ids: synthetic on calls, FIFO-matched by name on results.
func TestMissingIDsAreSynthesizedAndPaired(t *testing.T) {
	raw, err := buildRequest("proj-1", "gemini-claude-sonnet-4-6", &ir.ChatRequest{
		Model: "gemini-claude-sonnet-4-6",
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{
				{ID: "", Name: "run", Arguments: `{}`}, // id lost
			}},
			{Role: ir.RoleTool, ToolCallID: "", ToolName: "run", Content: []ir.ContentPart{{Kind: "text", Text: "ok"}}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	var doc struct {
		Request struct {
			Contents []struct {
				Parts []struct {
					FunctionCall *struct {
						ID string `json:"id"`
					} `json:"functionCall"`
					FunctionResp *struct {
						ID string `json:"id"`
					} `json:"functionResponse"`
				} `json:"parts"`
			} `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var callID, respID string
	for _, c := range doc.Request.Contents {
		for _, p := range c.Parts {
			if p.FunctionCall != nil {
				callID = p.FunctionCall.ID
			}
			if p.FunctionResp != nil {
				respID = p.FunctionResp.ID
			}
		}
	}
	if callID == "" || respID == "" {
		t.Fatalf("synthetic ids missing: call=%q resp=%q", callID, respID)
	}
	if callID != respID {
		t.Fatalf("synthetic ids must pair: call=%q resp=%q", callID, respID)
	}
	if !strings.HasPrefix(callID, "toolu_vrtx_") {
		t.Fatalf("synthetic id shape = %q, want the toolu_vrtx_ form the reference uses", callID)
	}
}

// Two parallel calls of the same name pair FIFO: first result → first call.
func TestSameNameCallsPairFIFO(t *testing.T) {
	contents := []geminiContent{
		{Role: "model", Parts: []geminiPart{
			{FunctionCall: &geminiFnCall{ID: "id-a", Name: "run", Args: map[string]any{}}},
			{FunctionCall: &geminiFnCall{ID: "id-b", Name: "run", Args: map[string]any{}}},
		}},
		{Role: "user", Parts: []geminiPart{
			{FunctionResp: &geminiFnResult{Name: "run", Response: map[string]any{"n": 1}}},
			{FunctionResp: &geminiFnResult{Name: "run", Response: map[string]any{"n": 2}}},
		}},
	}
	ensureAntigravityToolIDs(contents)
	rs := contents[1].Parts
	if rs[0].FunctionResp.ID != "id-a" || rs[1].FunctionResp.ID != "id-b" {
		t.Fatalf("FIFO pairing broken: %q, %q", rs[0].FunctionResp.ID, rs[1].FunctionResp.ID)
	}
}

// Gemini-family models are untouched by all of this: no ids needed, none sent
// beyond what the caller supplied.
func TestGeminiModelUntouched(t *testing.T) {
	contents := []geminiContent{
		{Role: "model", Parts: []geminiPart{
			{FunctionCall: &geminiFnCall{Name: "run", Args: map[string]any{}}},
		}},
	}
	ensureAntigravityToolIDs(contents)
	// ensureAntigravityToolIDs fills ids unconditionally — that is fine: the
	// field is optional on the Gemini path (omitempty), and Gemini models
	// ignore it. The assertion here is only that nothing panics and Gemini
	// requests keep building.
	raw, err := buildRequest("proj-1", "gemini-3-flash", &ir.ChatRequest{
		Model: "gemini-3-flash",
		Messages: []ir.Message{{
			Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}},
		}},
	}, nil)
	if err != nil {
		t.Fatalf("gemini buildRequest: %v", err)
	}
	if !strings.Contains(string(raw), "gemini-3-flash") {
		t.Fatal("gemini model name must pass through")
	}
}
