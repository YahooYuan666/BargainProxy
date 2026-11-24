// BP-337: a tool's returned images must be told AS a tool's, never as the
// user's.
//
// BP-308 moved the images a tool returned onto a user turn, with a bare
// carrier line "（工具返回的图片如下）" and nothing tying them to the tool
// call they came from. The shape reads as a NEW user message: on 2026-11-23
// deepseek-v4.1-flash spent eight consecutive turns working out who sent
// them ("The user sent ... the user is showing them back to me"), and
// re-stated the same four screenshots every turn.
//
// magpie (internal/gateway/chat.go seeLater/showSeen, 2026-11 reads) does the
// same move but tells the model what it is: the tool message ends with
// "[The tool returned an image; it follows in the next message.]" and the
// carrying user message opens with "[From the result of <name> (tool call
// <id>):]". The image still rides a user message (OpenAI chat's tool
// message holds text only) — the difference is that its provenance travels
// with it, so the model reads a tool's output rather than a user's request.
package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// bp337Body builds the read-then-answer shape and returns the wire body.
func bp337Body(t *testing.T, withFollowingUserTurn bool) map[string]any {
	t.Helper()
	msgs := []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "读取 blue.png 并告诉我颜色"}}},
		{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "call_x", Name: "Read", Arguments: `{"file_path":"blue.png"}`}}},
		{Role: ir.RoleTool, ToolCallID: "call_x", ToolName: "Read", Content: []ir.ContentPart{
			{Kind: "image", ImageMIME: "image/png", ImageB64: bp308PNG},
		}},
	}
	if withFollowingUserTurn {
		msgs = append(msgs, ir.Message{Role: ir.RoleUser,
			Content: []ir.ContentPart{{Kind: "text", Text: "读到了，它是什么颜色？"}}})
	}
	req := &ir.ChatRequest{Model: "deepseek-v4.1-flash", Messages: msgs, MaxTokens: 100}
	raw, err := testDriver("http://unused").buildBody(driver.ChatCall{Request: req}, testCreds(), false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return body
}

// bp337Messages returns the wire messages as maps.
func bp337Messages(body map[string]any) []map[string]any {
	raw, _ := body["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		if mm, ok := m.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

// bp337TextOf returns the text of a message: string content, or the text
// parts of a parts array, joined.
func bp337TextOf(m map[string]any) string {
	switch c := m["content"].(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			pm, _ := p.(map[string]any)
			if pm["type"] == "text" {
				if s, ok := pm["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// TestBP337ToolMessageAnnouncesFollowingImages: the tool message must say an
// image follows, so the model reads the next message as that tool's output.
func TestBP337ToolMessageAnnouncesFollowingImages(t *testing.T) {
	body := bp337Body(t, false)
	var tool map[string]any
	for _, m := range bp337Messages(body) {
		if m["role"] == "tool" {
			tool = m
		}
	}
	if tool == nil {
		t.Fatal("no tool message in wire body")
	}
	text := bp337TextOf(tool)
	if !strings.Contains(text, "follows in the next message") &&
		!strings.Contains(text, "follow in the next message") {
		t.Errorf("tool message does not announce the images that follow it: %q", text)
	}
}

// TestBP337CarrierNamesTheToolCall: the carrying user message must say which
// tool result the images belong to, not read as a bare user statement.
func TestBP337CarrierNamesTheToolCall(t *testing.T) {
	body := bp337Body(t, false)
	var carrier map[string]any
	for _, m := range bp337Messages(body) {
		if m["role"] != "user" {
			continue
		}
		if strings.Contains(bp337TextOf(m), "From the result of") {
			carrier = m
		}
	}
	if carrier == nil {
		t.Fatal("no user message attributing the images to a tool result")
	}
	text := bp337TextOf(carrier)
	if !strings.Contains(text, "Read") {
		t.Errorf("carrier does not name the tool: %q", text)
	}
	if !strings.Contains(text, "call_x") {
		t.Errorf("carrier does not name the tool call id: %q", text)
	}
}

// TestBP337OldBareCarrierGone: the un-attributed carrier line must not ride
// the wire any more — it is what read as the user speaking.
func TestBP337OldBareCarrierGone(t *testing.T) {
	for _, withTurn := range []bool{false, true} {
		body := bp337Body(t, withTurn)
		for _, m := range bp337Messages(body) {
			if strings.Contains(bp337TextOf(m), "（工具返回的图片如下）") {
				t.Errorf("withFollowingUserTurn=%v: bare carrier still on the wire: %q",
					withTurn, bp337TextOf(m))
			}
		}
	}
}

// TestBP337ImageStillReachesTheModel: the fix must not regress BP-308 — the
// image itself still has to arrive.
func TestBP337ImageStillReachesTheModel(t *testing.T) {
	for _, withTurn := range []bool{false, true} {
		body := bp337Body(t, withTurn)
		n := 0
		for _, m := range bp337Messages(body) {
			parts, _ := m["content"].([]any)
			for _, p := range parts {
				pm, _ := p.(map[string]any)
				if pm["type"] == "image_url" {
					n++
				}
			}
		}
		if n != 1 {
			t.Errorf("withFollowingUserTurn=%v: image_url parts = %d, want 1", withTurn, n)
		}
	}
}

// TestBP337CarrierMergesIntoTheUsersNextTurn: when the client's own user text
// follows, the images join THAT message (one user turn, not two in a row —
// some chat templates turn two away) and still name their tool.
func TestBP337CarrierMergesIntoTheUsersNextTurn(t *testing.T) {
	body := bp337Body(t, true)
	msgs := bp337Messages(body)
	var users []map[string]any
	for _, m := range msgs {
		if m["role"] == "user" {
			users = append(users, m)
		}
	}
	if len(users) < 2 {
		t.Fatalf("user messages = %d, want at least 2 (original + carrier)", len(users))
	}
	last := users[len(users)-1]
	text := bp337TextOf(last)
	if !strings.Contains(text, "From the result of") {
		t.Errorf("the user's own turn does not carry the attribution: %q", text)
	}
	if !strings.Contains(text, "读到了，它是什么颜色？") {
		t.Errorf("the user's own words were lost: %q", text)
	}
}

// TestBP337PlainToolResultUnchanged: a text-only tool result must not gain an
// announcement or a carrier turn — zero impact on ordinary requests.
func TestBP337PlainToolResultUnchanged(t *testing.T) {
	msgs := []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "读 a.txt"}}},
		{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "call_1", Name: "Read", Arguments: `{}`}}},
		{Role: ir.RoleTool, ToolCallID: "call_1", ToolName: "Read",
			Content: []ir.ContentPart{{Kind: "text", Text: "hello"}}},
	}
	req := &ir.ChatRequest{Model: "deepseek-v4.1-flash", Messages: msgs, MaxTokens: 100}
	raw, err := testDriver("http://unused").buildBody(driver.ChatCall{Request: req}, testCreds(), false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, m := range bp337Messages(body) {
		text := bp337TextOf(m)
		if strings.Contains(text, "follows in the next message") ||
			strings.Contains(text, "From the result of") {
			t.Errorf("text-only tool result gained an announcement: %q", text)
		}
	}
	for _, m := range bp337Messages(body) {
		if m["role"] != "tool" {
			continue
		}
		if got := bp337TextOf(m); got != "hello" {
			t.Errorf("tool text = %q, want %q (must stay byte-identical)", got, "hello")
		}
	}
}
