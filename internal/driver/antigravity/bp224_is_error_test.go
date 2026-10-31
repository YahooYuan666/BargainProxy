package antigravity

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/ir"
)

// BP-224: a failed tool result must ride functionResponse's response.error —
// leaving it in the success payload shape made the model read the failure
// text as a real result and plan on it (CLIProxyAPI 068fe153/81de4b9c #6838).
func TestBP224IsErrorRidesResponseError(t *testing.T) {
	raw, err := buildRequest("proj-1", "gemini-claude-sonnet-4-6", &ir.ChatRequest{
		Model: "gemini-claude-sonnet-4-6",
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{
				ID: "call_1", Name: "bash", Arguments: `{"cmd":"ls"}`,
			}}},
			{Role: ir.RoleTool, ToolCallID: "call_1", ToolName: "bash", IsError: true,
				Content: []ir.ContentPart{{Kind: "text", Text: "command failed with exit code 1"}}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	var doc struct {
		Request struct {
			Contents []struct {
				Parts []struct {
					FunctionResp *struct {
						Response map[string]any `json:"response"`
					} `json:"functionResponse"`
				} `json:"parts"`
			} `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var resp map[string]any
	for _, c := range doc.Request.Contents {
		for _, p := range c.Parts {
			if p.FunctionResp != nil {
				resp = p.FunctionResp.Response
			}
		}
	}
	if resp == nil {
		t.Fatal("no functionResponse found")
	}
	if _, has := resp["error"]; !has {
		t.Fatalf("failed tool result must carry response.error, got %v", resp)
	}
	if _, has := resp["content"]; has {
		t.Fatalf("failed tool result must not use the success payload shape, got %v", resp)
	}
}

// Success results keep the ordinary payload — the fix must not over-trigger.
func TestBP224SuccessKeepsOrdinaryPayload(t *testing.T) {
	raw, err := buildRequest("proj-1", "gemini-claude-sonnet-4-6", &ir.ChatRequest{
		Model: "gemini-claude-sonnet-4-6",
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{
				ID: "call_1", Name: "bash", Arguments: `{"cmd":"ls"}`,
			}}},
			{Role: ir.RoleTool, ToolCallID: "call_1", ToolName: "bash",
				Content: []ir.ContentPart{{Kind: "text", Text: "file.go"}}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("empty request")
	}
	var doc struct {
		Request struct {
			Contents []struct {
				Parts []struct {
					FunctionResp *struct {
						Response map[string]any `json:"response"`
					} `json:"functionResponse"`
				} `json:"parts"`
			} `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var resp map[string]any
	for _, c := range doc.Request.Contents {
		for _, p := range c.Parts {
			if p.FunctionResp != nil {
				resp = p.FunctionResp.Response
			}
		}
	}
	if resp == nil {
		t.Fatal("no functionResponse found")
	}
	if _, has := resp["error"]; has {
		t.Fatalf("success result must not carry response.error, got %v", resp)
	}
}
