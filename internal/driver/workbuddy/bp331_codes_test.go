package workbuddy

// BP-331: the request-level rejection family registers in classifyBusiness —
// the same body fails on every account, so the class must be bad_request
// (no rotation, no account penalty), never the generic internal bucket that
// feeds the loss-streak and rotates the pool (misakano 733942a7 + PR #251,
// RobbsLuo 18153486).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/uperr"
)

func TestBP331RequestLevelCodesAreBadRequest(t *testing.T) {
	cases := []struct {
		code int
		msg  string
	}{
		{11129, "invalid function call parameters"},
		{11152, "the tool name is invalid or duplicated"},
		{11133, "the request parameters were rejected by the model provider"},
		{11148, "tool calls and tool results do not match"},
		{11135, "invalid image"},
	}
	for _, tc := range cases {
		e := classifyBusiness(tc.code, tc.msg, "glm-5.3")
		if e.Class != uperr.ClassBadRequest {
			t.Errorf("code %d: class = %s, want bad_request (request-level, no rotation)", tc.code, e.Class)
		}
		if e.Status != 400 {
			t.Errorf("code %d: status = %d, want 400", tc.code, e.Status)
		}
	}
	// The tool-definition wording must NAME the tools, not read like an
	// account fault (misakano's GatewayHint rationale).
	if e := classifyBusiness(11152, "x", "glm-5.3"); !strings.Contains(e.Message, "tool") {
		t.Fatalf("11129/11152 message must point at the tools: %q", e.Message)
	}
}

// A code that merely CONTAINS 11129/11152 as digits must not be caught
// (misakano's adjacency guard: 111290 / 111520 stay in the generic bucket).
func TestBP331AdjacentCodesNotMisjudged(t *testing.T) {
	for _, code := range []int{111290, 111520, 111330} {
		if e := classifyBusiness(code, "whatever", "glm-5.3"); e.Class == uperr.ClassBadRequest {
			t.Errorf("code %d must not match its substring sibling", code)
		}
	}
}

// A 400 whose wording names tool definitions but carries no parseable code is
// still request-level (misakano badParamsMarkers11129).
func TestBP331ToolSchemaWordingMarker(t *testing.T) {
	for _, body := range []string{
		`invalid function call parameters`,
		`invalid_function_call_parameters`,
		`{"msg":"x","displayMsg":{"zh":"工具定义不合规"}}`,
	} {
		e := classifyHTTP(400, []byte(body), "glm-5.3")
		if e.Class != uperr.ClassBadRequest {
			t.Errorf("body %q: class = %s, want bad_request", body, e.Class)
		}
	}
	// An unrelated 400 keeps its own path.
	if e := classifyHTTP(400, []byte(`{"code":11101,"msg":"Unmarshal chat params failed"}`), "glm-5.3"); e.Class != uperr.ClassBadRequest {
		t.Errorf("11101 must stay bad_request: %s", e.Class)
	}
}

// BP-331 (plugins #1213, 2026-11-19): kimi-k3.1-pro rejects a call-only
// assistant turn (tool_calls, content null). The chat lane must fill the
// content with one space — the messages lane already does (BP-299).
func TestBP331CallOnlyAssistantGetsSpace(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"好"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()

	d := testDriver(srv.URL)
	req := &ir.ChatRequest{
		Model: "kimi-k3.1-pro",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
			// The call-only assistant turn: no content parts, one tool call.
			{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "c1", Name: "shell", Arguments: "{}"}}},
			{Role: ir.RoleTool, ToolCallID: "c1", Content: []ir.ContentPart{{Kind: "text", Text: "done"}}},
		},
	}
	call := driver.ChatCall{Account: testAccount(), UpstreamModel: "kimi-k3.1-pro", Request: req}
	if _, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}
	msgs, _ := gotBody["messages"].([]any)
	// Locate the call-only assistant turn by role (the builder may prepend a
	// system message depending on the prompt mode).
	var asst map[string]any
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if r, _ := mm["role"].(string); r == "assistant" {
			if _, has := mm["tool_calls"]; has {
				asst = mm
			}
		}
	}
	if asst == nil {
		t.Fatalf("no call-only assistant turn in the wire: %v", gotBody["messages"])
	}
	if c, _ := asst["content"].(string); c != " " {
		t.Fatalf("call-only assistant content = %v, want a single space (kimi-k3.1-pro 400s on null)", asst["content"])
	}

	// A NORMAL assistant text turn is untouched (content keeps its text).
	req2 := &ir.ChatRequest{
		Model: "kimi-k3.1-pro",
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "看完了"}}},
		},
	}
	gotBody = nil
	call2 := driver.ChatCall{Account: testAccount(), UpstreamModel: "kimi-k3.1-pro", Request: req2}
	if _, err := d.Stream(context.Background(), call2, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("stream2: %v", err)
	}
	msgs2, _ := gotBody["messages"].([]any)
	var asst2 map[string]any
	for _, m := range msgs2 {
		mm, _ := m.(map[string]any)
		if r, _ := mm["role"].(string); r == "assistant" {
			asst2 = mm
		}
	}
	if c, _ := asst2["content"].(string); c != "看完了" {
		t.Fatalf("text assistant content must stay intact: %v", asst2["content"])
	}
}
