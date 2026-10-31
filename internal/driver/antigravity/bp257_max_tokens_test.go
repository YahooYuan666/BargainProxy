package antigravity

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

// BP-257: maxOutputTokens must NOT be sent for Gemini-family models.
//
// AIClient2API — the reference this driver was ported from and the one the user
// confirms worked — deletes it for every non-Claude model:
//
//	if (!isClaudeModel && generationConfig?.maxOutputTokens !== undefined) {
//	    delete generationConfig.maxOutputTokens;
//	}
//
// We were forwarding the client's value (ZCode asks for 32768) on every Gemini
// request. That was the one structural difference from the known-good payload.
func TestMaxOutputTokensDroppedForGemini(t *testing.T) {
	body, err := buildRequest("proj-1", "gemini-3.8-flash-high", &ir.ChatRequest{
		MaxTokens: 32768,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "你好"}}}},
	}, nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	inner, _ := doc["request"].(map[string]any)
	gen, _ := inner["generationConfig"].(map[string]any)
	if gen == nil {
		return // no generationConfig at all is fine
	}
	if _, present := gen["maxOutputTokens"]; present {
		t.Fatalf("maxOutputTokens was sent for a Gemini model; the reference deletes it "+
			"and that is the known-good shape: %s", string(body))
	}
}

// Claude keeps the field: that endpoint bills thinking against it, and the
// reference clamps rather than deletes there.
func TestMaxOutputTokensKeptForClaude(t *testing.T) {
	body, err := buildRequest("proj-1", "claude-sonnet-4-6", &ir.ChatRequest{
		MaxTokens: 4096,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}, nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if !strings.Contains(string(body), "maxOutputTokens") {
		t.Fatalf("maxOutputTokens missing for a Claude model: %s", string(body))
	}
}
