package antigravity

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/ir"
)

// BP-273 (CLIProxyAPI be86430518): tool results can carry images (browser
// screenshots and the like). The Anthropic inbound keeps them as IR image
// parts, but the antigravity egress used to read only text out of a RoleTool
// message — the model silently never saw the screenshot. The vendor reads
// tool-result images from functionResponse.parts (inlineData), the shape
// CLIProxyAPI settled on after their first sibling-part version.

type bp273Doc struct {
	Request struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				FunctionResponse *struct {
					ID       string         `json:"id"`
					Name     string         `json:"name"`
					Response map[string]any `json:"response"`
					Parts    []struct {
						InlineData *struct {
							MIMEType string `json:"mimeType"`
							Data     string `json:"data"`
						} `json:"inlineData"`
					} `json:"parts"`
				} `json:"functionResponse"`
			} `json:"parts"`
		} `json:"contents"`
	} `json:"request"`
}

func firstFunctionResponse(t *testing.T, raw []byte) *struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
	Parts    []struct {
		InlineData *struct {
			MIMEType string `json:"mimeType"`
			Data     string `json:"data"`
		} `json:"inlineData"`
	} `json:"parts"`
} {
	t.Helper()
	var doc bp273Doc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, c := range doc.Request.Contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				return p.FunctionResponse
			}
		}
	}
	t.Fatal("no functionResponse found in request")
	return nil
}

func toolResultImageRequest() *ir.ChatRequest {
	return &ir.ChatRequest{
		Model: "gemini-3-pro",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "look at the page"}}},
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "call_1", Name: "browser", Arguments: "{}"}}},
			{Role: ir.RoleTool, ToolCallID: "call_1", ToolName: "browser", Content: []ir.ContentPart{
				{Kind: "text", Text: "screenshot attached"},
				{Kind: "image", ImageMIME: "image/png", ImageB64: "aGVsbG8="},
			}},
		},
	}
}

func TestToolResultImageNestsIntoFunctionResponse(t *testing.T) {
	raw, err := buildRequest("proj-1", "gemini-pro-agent", toolResultImageRequest(), nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	fr := firstFunctionResponse(t, raw)
	if len(fr.Parts) != 1 || fr.Parts[0].InlineData == nil {
		t.Fatalf("functionResponse.parts must nest the image: %+v", fr)
	}
	img := fr.Parts[0].InlineData
	if img.MIMEType != "image/png" || img.Data != "aGVsbG8=" {
		t.Fatalf("inlineData = %+v, want png aGVsbG8=", img)
	}
	// The text payload is untouched.
	if fr.Response["content"] != "screenshot attached" {
		t.Fatalf("response payload changed: %v", fr.Response)
	}
}

// A text-only tool result must not grow a parts field (omitempty) — the
// plain payload shape stays byte-compatible with what the vendor accepts.
func TestToolResultTextOnlyHasNoPartsField(t *testing.T) {
	req := &ir.ChatRequest{
		Model: "gemini-3-pro",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "run it"}}},
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "call_1", Name: "bash", Arguments: "{}"}}},
			{Role: ir.RoleTool, ToolCallID: "call_1", ToolName: "bash", Content: []ir.ContentPart{{Kind: "text", Text: "ok"}}},
		},
	}
	raw, err := buildRequest("proj-1", "gemini-pro-agent", req, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if fr := firstFunctionResponse(t, raw); len(fr.Parts) != 0 {
		t.Fatalf("text-only tool result must not emit functionResponse.parts: %+v", fr.Parts)
	}
}

// BP-224 regression: a failed tool result still rides response.error, with
// or without images alongside.
func TestToolResultErrorShapeUnchanged(t *testing.T) {
	req := &ir.ChatRequest{
		Model: "gemini-3-pro",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "run it"}}},
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "call_1", Name: "bash", Arguments: "{}"}}},
			{Role: ir.RoleTool, ToolCallID: "call_1", ToolName: "bash", IsError: true,
				Content: []ir.ContentPart{{Kind: "text", Text: "exit 1"}}},
		},
	}
	raw, err := buildRequest("proj-1", "gemini-pro-agent", req, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	fr := firstFunctionResponse(t, raw)
	if fr.Response["error"] != "exit 1" {
		t.Fatalf("error tool result must keep response.error: %v", fr.Response)
	}
	if len(fr.Parts) != 0 {
		t.Fatalf("error tool result without images must not emit parts: %+v", fr.Parts)
	}
}
