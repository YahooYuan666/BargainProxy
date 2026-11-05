package antigravity

import (
	"encoding/json"
	"reflect"
	"testing"

	"bargainproxy/internal/ir"
)

// BP-289 (plugins gcp-ai 5112465 #734): Gemini/Vertex answers three history
// shapes with a bare 400 — ending with the model's turn, opening with it,
// and a user turn mixing a functionResponse with the user's own parts. The
// outbound builder hardens all three; a conversation that already fits is
// marshalled byte-identical.

type hardenDoc struct {
	Request struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text         string `json:"text"`
				FunctionResp *struct {
					Name string `json:"name"`
				} `json:"functionResponse"`
			} `json:"parts"`
		} `json:"contents"`
	} `json:"request"`
}

func buildContents(t *testing.T, req *ir.ChatRequest) hardenDoc {
	t.Helper()
	raw, err := buildRequest("proj-1", "gemini-claude-sonnet-4-6", req, nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	var doc hardenDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return doc
}

// Rule 1: a conversation ENDING with the model's turn (prefill-style
// history) gets a user turn after it, or the upstream answers 400
// "Requests ending with a model turn are not supported".
func TestBP289TrailingModelTurnGetsUserTurn(t *testing.T) {
	doc := buildContents(t, &ir.ChatRequest{
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "讲个笑话"}}},
			{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "为什么程序员..."}}},
		},
	})
	cs := doc.Request.Contents
	if len(cs) != 3 {
		t.Fatalf("contents = %d turns, want 3 (user, model, user)", len(cs))
	}
	if cs[len(cs)-1].Role != "user" || len(cs[len(cs)-1].Parts) != 1 ||
		cs[len(cs)-1].Parts[0].Text != continueTurnText {
		t.Fatalf("trailing model turn must be followed by a %q user turn, got %+v", continueTurnText, cs[len(cs)-1])
	}
}

// Rule 2: a conversation OPENING with the model's turn (compaction
// leftover) gets a user turn before it — "function call turn comes
// immediately after a user turn" otherwise.
func TestBP289LeadingModelTurnGetsUserTurn(t *testing.T) {
	doc := buildContents(t, &ir.ChatRequest{
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "接着说"}}},
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "好"}}},
		},
	})
	cs := doc.Request.Contents
	if len(cs) != 3 {
		t.Fatalf("contents = %d turns, want 3 (user, model, user)", len(cs))
	}
	if cs[0].Role != "user" || len(cs[0].Parts) != 1 ||
		cs[0].Parts[0].Text != leadingModelTurnText {
		t.Fatalf("leading model turn must be preceded by a %q user turn, got %+v", leadingModelTurnText, cs[0])
	}
}

// Rule 3: a user turn mixing a functionResponse with the user's own parts
// is split — responses first, the user's parts in their own turn after.
func TestBP289MixedUserTurnIsSplit(t *testing.T) {
	mixed := geminiContent{Role: "user", Parts: []geminiPart{
		{FunctionResp: &geminiFnResult{ID: "call_1", Name: "bash", Response: map[string]any{"content": "ok"}}},
		{Text: "另外帮我看看这个"},
	}}
	got := hardenGeminiContents([]geminiContent{mixed})
	if len(got) != 2 {
		t.Fatalf("mixed user turn must split into 2 turns, got %d", len(got))
	}
	if got[0].Parts[0].FunctionResp == nil {
		t.Fatalf("responses must come first, got %+v", got[0])
	}
	if got[1].Parts[0].Text != "另外帮我看看这个" {
		t.Fatalf("the user's own parts must keep their own turn, got %+v", got[1])
	}
}

// A conversation that already fits the rules is returned unchanged — the
// harden must not perturb normal traffic (byte-identical contents).
func TestBP289NormalHistoryUntouched(t *testing.T) {
	normal := []geminiContent{
		{Role: "user", Parts: []geminiPart{{Text: "你好"}}},
		{Role: "model", Parts: []geminiPart{{Text: "你好！"}}},
		{Role: "user", Parts: []geminiPart{{FunctionResp: &geminiFnResult{ID: "c1", Name: "bash", Response: map[string]any{"content": "x"}}}}},
		{Role: "user", Parts: []geminiPart{{Text: "继续"}}},
	}
	got := hardenGeminiContents(normal)
	if !reflect.DeepEqual(got, normal) {
		t.Fatalf("normal history must be returned unchanged, got %+v", got)
	}
}
