// BP-315: the tool-result image budget moved here from the shared Anthropic
// parser, and stopped dropping images silently.
//
// Two regressions this pins down:
//
//  1. Wrong layer. maxToolResultImages = 5 came from AIClient2API's
//     ClaudeConverter — a Gemini-family budget, expressed as inlineData parts.
//     Sitting in ParseMessagesRequest it applied to EVERY channel, including
//     the OpenAI-shaped ones (workbuddy/deepseek) that never had the limit.
//     Real case 2026-11-12: 64 tool images in, 59 deleted by this cap alone.
//
//  2. Silent loss. The old cap left no trace, so the model could not tell
//     "the picture is gone" from "there was no picture". It re-ran the same
//     contact-sheet command 28 times and gave contradictory readings of the
//     same image (CURREN → TIMEX → SONY). The upstream keeps the same five-
//     image budget but replaces the dropped part with a text placeholder —
//     this driver now does too.
package antigravity

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

const bp315PNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// bp315Req builds a history whose tool messages carry n images in total,
// spread one-per-tool-message so the message/image indexing is exercised.
func bp315Req(n int) *ir.ChatRequest {
	msgs := []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "look at the pictures"}}},
	}
	for i := 0; i < n; i++ {
		callID := "call_" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		msgs = append(msgs,
			ir.Message{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{
				ID: callID, Name: "Read", Arguments: `{"path":"p.png"}`,
			}}},
			ir.Message{Role: ir.RoleTool, ToolCallID: callID, ToolName: "Read",
				Content: []ir.ContentPart{{Kind: "image", ImageMIME: "image/png", ImageB64: bp315PNG}}},
		)
	}
	return &ir.ChatRequest{Model: "gemini-3-pro", Messages: msgs}
}

// bp315Parts pulls every geminiPart out of the built request, in order.
func bp315Parts(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var doc struct {
		Request struct {
			Contents []struct {
				Role  string           `json:"role"`
				Parts []map[string]any `json:"parts"`
			} `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var out []map[string]any
	for _, c := range doc.Request.Contents {
		out = append(out, c.Parts...)
	}
	return out
}

// bp315ToolImageParts walks the built request and counts, inside each
// functionResponse's nested parts, the inlineData images; omission
// placeholders are counted at the SIBLING level (same parts array, after the
// functionResponse) — Google rejects a text field nested inside
// function_response.parts, so the placeholder must not live there.
func bp315ToolImageParts(parts []map[string]any) (images, placeholders int) {
	for _, p := range parts {
		if _, ok := p["inlineData"]; ok {
			// user-message and nested images both count as images
		}
		fr, ok := p["functionResponse"].(map[string]any)
		if !ok {
			if txt, ok := p["text"].(string); ok && txt == toolImageOmitted {
				placeholders++
			}
			continue
		}
		nested, _ := fr["parts"].([]any)
		for _, n := range nested {
			nm, _ := n.(map[string]any)
			if _, ok := nm["inlineData"]; ok {
				images++
			}
			if _, bad := nm["text"]; bad {
				// caught by TestBP315PlaceholderNeverNestsInFunctionResponse
			}
		}
	}
	return images, placeholders
}

// TestBP315OverBudgetKeepsLatestFiveAsImages is the core budget rule: only the
// most recent five tool images ride as inlineData.
func TestBP315OverBudgetKeepsLatestFiveAsImages(t *testing.T) {
	raw, err := buildRequest("proj-1", "gemini-3-pro", bp315Req(12), nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	images, placeholders := bp315ToolImageParts(bp315Parts(t, raw))
	if images != maxToolResultImages {
		t.Fatalf("inlineData images = %d, want %d", images, maxToolResultImages)
	}
	if placeholders != 12-maxToolResultImages {
		t.Fatalf("omission placeholders = %d, want %d", placeholders, 12-maxToolResultImages)
	}
}

// TestBP315OmissionsCarryPlaceholder — the whole point of the ticket. A
// dropped image must leave the model a readable marker, not a gap.
func TestBP315OmissionsCarryPlaceholder(t *testing.T) {
	raw, err := buildRequest("proj-1", "gemini-3-pro", bp315Req(7), nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if !strings.Contains(string(raw), "Earlier image omitted") {
		t.Fatal("no omission placeholder on the wire: the model cannot tell a dropped image from an absent one")
	}
	if !strings.Contains(string(raw), "Re-read the file") {
		t.Fatal("placeholder does not tell the model it may re-read the image")
	}
}

// TestBP315UnderBudgetUntouched — a request inside the budget keeps every
// image and grows no placeholder.
func TestBP315UnderBudgetUntouched(t *testing.T) {
	raw, err := buildRequest("proj-1", "gemini-3-pro", bp315Req(3), nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	images, placeholders := bp315ToolImageParts(bp315Parts(t, raw))
	if images != 3 || placeholders != 0 {
		t.Fatalf("images=%d placeholders=%d, want 3/0", images, placeholders)
	}
	if strings.Contains(string(raw), "Earlier image omitted") {
		t.Fatal("under-budget request grew an omission placeholder")
	}
}

// TestBP315TextOnlyToolResultsUnaffected — no images, no placeholders, no
// structural change.
func TestBP315TextOnlyToolResultsUnaffected(t *testing.T) {
	req := &ir.ChatRequest{Model: "gemini-3-pro", Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "run ls"}}},
		{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "c1", Name: "Bash", Arguments: `{}`}}},
		{Role: ir.RoleTool, ToolCallID: "c1", ToolName: "Bash",
			Content: []ir.ContentPart{{Kind: "text", Text: `{"content":"a.go"}`}}},
	}}
	raw, err := buildRequest("proj-1", "gemini-3-pro", req, nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if strings.Contains(string(raw), "Earlier image omitted") {
		t.Fatal("text-only tool result grew an omission placeholder")
	}
}

// TestBP315MultipleImagesInOneResultCountIndividually — the budget counts
// images, not messages: a single result carrying six images keeps its last
// five and marks the first.
func TestBP315MultipleImagesInOneResultCountIndividually(t *testing.T) {
	var parts []ir.ContentPart
	for i := 0; i < 6; i++ {
		parts = append(parts, ir.ContentPart{Kind: "image", ImageMIME: "image/png", ImageB64: bp315PNG})
	}
	req := &ir.ChatRequest{Model: "gemini-3-pro", Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "six shots"}}},
		{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "c1", Name: "Read", Arguments: `{}`}}},
		{Role: ir.RoleTool, ToolCallID: "c1", ToolName: "Read", Content: parts},
	}}
	raw, err := buildRequest("proj-1", "gemini-3-pro", req, nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	images, placeholders := bp315ToolImageParts(bp315Parts(t, raw))
	if images != 5 || placeholders != 1 {
		t.Fatalf("images=%d placeholders=%d, want 5/1", images, placeholders)
	}
}

// TestBP315PlaceholderNeverNestsInFunctionResponse — live-probe regression
// (2026-11-12 23:15): Google rejects `Unknown name "text" at
// '...function_response.parts[N]'` with a bare 400. The omission placeholder
// must ride as a SIBLING of the functionResponse, never inside its nested
// parts.
func TestBP315PlaceholderNeverNestsInFunctionResponse(t *testing.T) {
	raw, err := buildRequest("proj-1", "gemini-3-pro", bp315Req(7), nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	var doc struct {
		Request struct {
			Contents []struct {
				Role  string           `json:"role"`
				Parts []map[string]any `json:"parts"`
			} `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sawPlaceholder := false
	for _, c := range doc.Request.Contents {
		for _, p := range c.Parts {
			fr, ok := p["functionResponse"].(map[string]any)
			if !ok {
				continue
			}
			nested, _ := fr["parts"].([]any)
			for _, n := range nested {
				nm, _ := n.(map[string]any)
				if _, bad := nm["text"]; bad {
					t.Fatalf("text part nested inside functionResponse.parts: %v — upstream 400s on this", nm)
				}
			}
		}
	}
	for _, c := range doc.Request.Contents {
		for _, p := range c.Parts {
			if txt, ok := p["text"].(string); ok && txt == toolImageOmitted {
				sawPlaceholder = true
			}
		}
	}
	if !sawPlaceholder {
		t.Fatal("no sibling-level omission placeholder found")
	}
}
