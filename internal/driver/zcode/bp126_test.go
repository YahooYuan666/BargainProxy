package zcode

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

func bp126Call(model string) driver.ChatCall {
	return driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: model,
		Request: &ir.ChatRequest{
			Model:      model,
			SessionKey: "conversation-abc",
			Messages: []ir.Message{
				{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: "用户的自定义系统提示"}}},
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
			},
		},
	}
}

// BP-126: zcode.z.ai rejects a fourth system block with 405/3012 (magpie
// #343). The model declaration and the caller's system prompts must be merged
// into the last official block's text — the array stays at exactly three.
func TestSystemNeverExceedsThreeBlocks(t *testing.T) {
	d := testDriver("http://test")
	raw, _, _, err := d.normalizeBody(bp126Call("GLM-5.3"), &Creds{JWT: ""}, false)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		System []map[string]any `json:"system"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.System) != 3 {
		t.Fatalf("system blocks = %d, want exactly 3 (405/3012 beyond that)", len(doc.System))
	}
	third, _ := doc.System[2]["text"].(string)
	if !strings.Contains(third, "powered by the model named GLM-5.3") {
		t.Fatal("model declaration must be merged into the third block's text")
	}
	if !strings.Contains(third, "用户的自定义系统提示") {
		t.Fatal("user system text must be merged into the third block's text")
	}
}

// BP-126: metadata.user_id is a JSON STRING naming the device and the
// conversation's session id (magpie 0.1.30/#387) — not the bare JWT sub.
func TestMetadataUserIDNamesDeviceAndSession(t *testing.T) {
	d := testDriver("http://test")
	raw, _, _, err := d.normalizeBody(bp126Call("GLM-5.3"), &Creds{JWT: ""}, false)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var uid struct {
		DeviceID    string `json:"device_id"`
		AccountUUID string `json:"account_uuid"`
		SessionID   string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(doc.Metadata.UserID), &uid); err != nil {
		t.Fatalf("user_id must be a JSON object string, got %q: %v", doc.Metadata.UserID, err)
	}
	if uid.DeviceID == "" || uid.SessionID == "" || uid.AccountUUID != "" {
		t.Fatalf("user_id shape wrong: %+v", uid)
	}
	if uid.SessionID != bareUUID("conversation-abc") {
		t.Fatalf("session_id = %q, want the SessionKey-derived bare UUID", uid.SessionID)
	}
}

// The same conversation key must map to the same session id across turns,
// and a UUID-shaped key goes out untouched.
func TestSessionIDStableAndUUIDShaped(t *testing.T) {
	d := testDriver("http://test")
	_, _, _, err := d.normalizeBody(bp126Call("GLM-5.3"), &Creds{JWT: ""}, false)
	if err != nil {
		t.Fatal(err)
	}
	a := sessionIDFor(&Creds{JWT: ""}, testAccount().ID, "conversation-abc")
	b := sessionIDFor(&Creds{JWT: ""}, testAccount().ID, "conversation-abc")
	if a != b {
		t.Fatalf("session id unstable: %q vs %q", a, b)
	}
	fixed := "0f0e0d0c-0b0a-4009-8008-070605040302"
	if got := sessionIDFor(&Creds{JWT: ""}, testAccount().ID, fixed); got != fixed {
		t.Fatalf("uuid-shaped key must pass through, got %q", got)
	}
}

// BP-126: ToolSearch goes out as ToolSearchTool (the endpoint hides
// Bash/Read/Write when a tool named exactly ToolSearch is declared), in the
// tool list, in replayed assistant tool_use history, and in tool_choice.
func TestToolSearchRenamedOutbound(t *testing.T) {
	d := testDriver("http://test")
	call := bp126Call("GLM-5.3")
	call.Request.Tools = []ir.ToolDef{
		{Name: "ToolSearch", Description: "search tools"},
		{Name: "Read", Description: "read files"},
	}
	call.Request.ToolChoice = "required"
	call.Request.ToolChoiceName = "ToolSearch"
	call.Request.Messages = append(call.Request.Messages, ir.Message{
		Role:      ir.RoleAssistant,
		ToolCalls: []ir.ToolCall{{ID: "t1", Name: "ToolSearch", Arguments: `{}`}},
	})
	raw, _, _, err := d.normalizeBody(call, &Creds{JWT: ""}, false)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
		ToolChoice struct {
			Name string `json:"name"`
		} `json:"tool_choice"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Tools[0].Name != toolSearchAlias || doc.Tools[1].Name != "Read" {
		t.Fatalf("tools = [%s %s], want [%s Read]", doc.Tools[0].Name, doc.Tools[1].Name, toolSearchAlias)
	}
	if doc.ToolChoice.Name != toolSearchAlias {
		t.Fatalf("tool_choice name = %q, want %s", doc.ToolChoice.Name, toolSearchAlias)
	}
	found := false
	for _, m := range doc.Messages {
		for _, blk := range m.Content {
			if blk["type"] == "tool_use" && blk["name"] == toolSearchAlias {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("replayed tool_use must carry the alias name")
	}
}

// BP-126: the model calls the tool under the alias; the client must see
// ToolSearch again.
func TestToolSearchRestoredInbound(t *testing.T) {
	d := testDriver("http://test")
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start"}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t9","name":"ToolSearchTool"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	var got ir.ToolCall
	_, err := d.parseAnthropicSSE(context.Background(), strings.NewReader(sse), bp126Call("GLM-5.3"), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvToolStart && ev.ToolCall != nil {
			got = *ev.ToolCall
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != toolSearchName {
		t.Fatalf("inbound tool name = %q, want %s restored", got.Name, toolSearchName)
	}
}

// The x-session-id header carries the same bare UUID the body's metadata
// names — BigModel keys the prompt cache on it.
func TestSessionHeaderSet(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://test", nil)
	identityHeaders(req, &Creds{JWT: ""}, defaultAppVersion, "abc-uuid")
	if got := req.Header.Get("x-session-id"); got != "abc-uuid" {
		t.Fatalf("x-session-id = %q, want abc-uuid", got)
	}
}
