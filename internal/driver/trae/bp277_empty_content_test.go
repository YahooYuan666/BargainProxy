package trae

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// BP-277 (traework2api-panel a45bd4e): the upstream only accepts
// text/image_url content; a message whose blocks were all unsupported
// (input_audio etc., dropped at the openai ingress) reached Trae as
// content:null and died as 4001 "the param is invalid" — and an emptied
// content array dies the same way. The egress must hand the upstream a
// placeholder instead, so the request stays alive and the drop is visible
// in the ingress log rather than as an opaque 4001.
func TestBuildSOLOBodyPlaceholdersEmptyUserContent(t *testing.T) {
	call := driver.ChatCall{
		Request: &ir.ChatRequest{
			Model: "glm-5.3-flash",
			Messages: []ir.Message{
				{Role: ir.RoleUser, Content: nil},
			},
		},
	}
	raw, err := buildSOLOBody(call, "solo_work_lite")
	if err != nil {
		t.Fatalf("buildSOLOBody: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("body: %v", err)
	}
	msgs, _ := doc["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	content, ok := first["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("empty user content must go out as one placeholder block, got %v", first["content"])
	}
	blk, _ := content[0].(map[string]any)
	if blk["type"] != "text" || blk["text"] != nonTextPlaceholder {
		t.Fatalf("placeholder block mismatch: %v", blk)
	}
}

// A message with real parts must keep them untouched — the placeholder is
// only for the emptied case.
func TestBuildSOLOBodyKeepsNonEmptyParts(t *testing.T) {
	call := driver.ChatCall{
		Request: &ir.ChatRequest{
			Model: "glm-5.3-flash",
			Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
			},
		},
	}
	raw, err := buildSOLOBody(call, "solo_work_lite")
	if err != nil {
		t.Fatalf("buildSOLOBody: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("body: %v", err)
	}
	msgs, _ := doc["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	content, _ := first["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("plain text message must keep its single block, got %v", first["content"])
	}
}
