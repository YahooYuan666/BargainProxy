package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// bp299Call builds a chat call whose user message carries the given content
// parts (BP-299).
func bp299Call(parts ...ir.ContentPart) driver.ChatCall {
	return driver.ChatCall{
		Account:       &model.Account{ID: "wb-299", Channel: model.ChannelWorkBuddy},
		UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{
			Model:    "glm-5.3",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: parts}},
		},
	}
}

// TestBP299EmptyTextPartPadded an empty text part in a parts array is a hard
// 400 (11151) upstream; the wire body must carry one space instead.
func TestBP299EmptyTextPartPadded(t *testing.T) {
	d := testDriver("http://unused")
	call := bp299Call(
		ir.ContentPart{Kind: "text", Text: ""},
		ir.ContentPart{Kind: "text", Text: "real"},
	)
	body, err := d.buildBody(call, testCreds(), false)
	if err != nil {
		t.Fatal(err)
	}
	doc := bodyMap(t, body)
	var user map[string]any
	for _, raw := range doc["messages"].([]any) {
		if m, ok := raw.(map[string]any); ok && m["role"] == "user" {
			user = m
			break
		}
	}
	if user == nil {
		t.Fatalf("no user message in body: %s", body)
	}
	parts := user["content"].([]any)
	if got := parts[0].(map[string]any)["text"]; got != " " {
		t.Fatalf("empty text part = %q, want a space", got)
	}
	if got := parts[1].(map[string]any)["text"]; got != "real" {
		t.Fatalf("real part mutated: %q", got)
	}
	// A part the upstream already accepts must not be rewritten. A single
	// text part serializes to plain string content, not a parts array.
	call2 := bp299Call(ir.ContentPart{Kind: "text", Text: "   "})
	body2, _ := d.buildBody(call2, testCreds(), false)
	doc2 := bodyMap(t, body2)
	var user2 map[string]any
	for _, raw := range doc2["messages"].([]any) {
		if m, ok := raw.(map[string]any); ok && m["role"] == "user" {
			user2 = m
			break
		}
	}
	if got := user2["content"].(string); got != "   " {
		t.Fatalf("whitespace part rewritten: %q", got)
	}
}

// TestBP299SniffedStringPadded a tool result whose string content is scraped
// form JSON (`[{"type":"text","text":""}…]`) is sniffed into parts by the
// upstream and dies with 11151; the wire body must re-serialize it padded.
func TestBP299SniffedStringPadded(t *testing.T) {
	d := testDriver("http://unused")
	sniff := `[{"type":"text","text":""},{"type":"text","text":"表单内容"}]`
	call := driver.ChatCall{
		Account:       &model.Account{ID: "wb-299", Channel: model.ChannelWorkBuddy},
		UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{
			Model: "glm-5.3",
			Messages: []ir.Message{
				{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "c1", Name: "read_form", Arguments: "{}"}}},
				{Role: ir.RoleTool, ToolCallID: "c1", Content: []ir.ContentPart{{Kind: "text", Text: sniff}}},
			},
		},
	}
	body, err := d.buildBody(call, testCreds(), false)
	if err != nil {
		t.Fatal(err)
	}
	doc := bodyMap(t, body)
	msgs := doc["messages"].([]any)
	var toolContent string
	for _, raw := range msgs {
		m := raw.(map[string]any)
		if m["role"] == "tool" {
			toolContent = m["content"].(string)
		}
	}
	var parts []map[string]any
	if err := json.Unmarshal([]byte(toolContent), &parts); err != nil {
		t.Fatalf("tool content no longer sniffable JSON: %v (%s)", err, toolContent)
	}
	if parts[0]["text"] != " " {
		t.Fatalf("sniffed empty part unpadded: %v", parts[0]["text"])
	}
	if parts[1]["text"] != "表单内容" {
		t.Fatalf("sniffed real part mutated: %v", parts[1]["text"])
	}
	// The scrape JSON that has no empty part rides byte-identical.
	ok := `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`
	call2 := call
	call2.Request = &ir.ChatRequest{
		Model: "glm-5.3",
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "c1", Name: "read_form", Arguments: "{}"}}},
			{Role: ir.RoleTool, ToolCallID: "c1", Content: []ir.ContentPart{{Kind: "text", Text: ok}}},
		},
	}
	body2, _ := d.buildBody(call2, testCreds(), false)
	if !strings.Contains(string(body2), `\"text\":\"a\"`) {
		t.Fatalf("clean scrape JSON rewritten: %s", body2)
	}
	// Plain prose starting with "[" but not parseable as an object array is
	// untouched (the upstream never sniffs it either).
	call3 := bp299Call(ir.ContentPart{Kind: "text", Text: "[见附录 A 与 B]"})
	body3, _ := d.buildBody(call3, testCreds(), false)
	if !strings.Contains(string(body3), "[见附录 A 与 B]") {
		t.Fatalf("plain bracket prose rewritten: %s", body3)
	}
}

// TestBP299MessagesLaneEmptyShell the messages lane pads a fully-pruned
// message with a text block; that block must not be empty (11151 shape).
func TestBP299MessagesLaneEmptyShell(t *testing.T) {
	d := testDriver("http://unused")
	req := &ir.ChatRequest{
		Model: "glm-5.3",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
			// Assistant message whose only content is an empty-name tool call
			// (pruned) → empty shell.
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "x", Name: ""}}},
		},
	}
	body, err := d.buildMessagesBody(driver.ChatCall{Request: req}, testCreds())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"text":""`) {
		t.Fatalf("messages lane still emits an empty text block: %s", body)
	}
}

// TestBP299ClassifyEmptyContent 11151 is request-level: bad_request, 400,
// upstream words kept — no rotation, no account penalty.
func TestBP299ClassifyEmptyContent(t *testing.T) {
	e := classifyBusiness(11151, `a message has empty content`, "glm-5.3")
	if e.Class != uperr.ClassBadRequest {
		t.Fatalf("class = %s, want bad_request", e.Class)
	}
	if e.Status != 400 {
		t.Fatalf("status = %d, want 400", e.Status)
	}
	// Neighbour codes keep their own meanings.
	if e2 := classifyBusiness(11140, "request illegal", "glm-5.3"); e2.Class == uperr.ClassBadRequest {
		t.Fatalf("11140 must not be swallowed by the 11151 branch: %s", e2.Class)
	}
}
