package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

func bp131Call(model, effort string) driver.ChatCall {
	return driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: model,
		Request: &ir.ChatRequest{
			Model: model, RequestedEffort: effort,
			Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
			},
		},
	}
}

// BP-131: CN claude-* models ride /v1/messages with output_config.effort —
// the ONLY lane where their thinking gears work (chat returns the same answer
// at every level). max must pass through untouched.
func TestClaudeRidesMessagesLane(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody = map[string]any{} // decode must not inherit the previous gear's keys
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `event: message_start`)
		fmt.Fprintln(w, `data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":4}}}`)
		fmt.Fprintln(w, `event: content_block_delta`)
		fmt.Fprintln(w, `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`)
		fmt.Fprintln(w, `event: message_delta`)
		fmt.Fprintln(w, `data: {"type":"message_delta","usage":{"output_tokens":3}}`)
		fmt.Fprintln(w, `event: message_stop`)
		fmt.Fprintln(w, `data: {"type":"message_stop"}`)
	}))
	defer up.Close()
	d := testDriver(up.URL)
	for _, gear := range []string{"max", "xhigh", "off", ""} {
		resp, err := d.Stream(context.Background(), bp131Call("claude-opus-5.5", gear), func(ir.StreamEvent) error { return nil })
		if err != nil {
			t.Fatalf("gear %q: %v", gear, err)
		}
		if gotPath != messagesPath {
			t.Fatalf("gear %q: path = %s, want %s", gear, gotPath, messagesPath)
		}
		if resp == nil || resp.StopReason != ir.StopEnd {
			t.Fatalf("gear %q: bad response %+v", gear, resp)
		}
		// Usage folding: Anthropic's input excludes cache; ir convention
		// includes it (BP-56).
		if resp.Usage.InputTokens != 14 || resp.Usage.CachedInputTokens != 4 || !resp.Usage.CacheKnown {
			t.Fatalf("usage = %+v, want input 14 with cache 4 known", resp.Usage)
		}
		oc, ok := gotBody["output_config"].(map[string]any)
		switch gear {
		case "max", "xhigh":
			if !ok || oc["effort"] != gear {
				t.Fatalf("gear %q: output_config = %v", gear, gotBody["output_config"])
			}
		case "off", "":
			if ok {
				t.Fatalf("gear %q must send no output_config, got %v", gear, oc)
			}
		}
	}
}

// Non-claude models and global-realm accounts stay on the chat lane.
func TestMessagesLaneGating(t *testing.T) {
	var paths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer up.Close()
	d := testDriver(up.URL)
	if _, err := d.Stream(context.Background(), bp131Call("glm-5.3", "high"), func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != chatCompletionsPath {
		t.Fatalf("glm paths = %v, want chat lane only", paths)
	}
	// Global realm: the route does not exist there (404 Route Not Found).
	gacc := testAccount()
	c, _ := ParseCreds(gacc.CredsJSON)
	c.Realm = "global"
	raw, _ := json.Marshal(c)
	gacc.CredsJSON = raw
	call := bp131Call("claude-opus-5.5", "max")
	call.Account = gacc
	paths = nil
	if _, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != chatCompletionsPath {
		t.Fatalf("global claude paths = %v, want chat lane only", paths)
	}
}

// A deployment without the route degrades once, then permanently for the
// process: the FIRST claude request probes /v1/messages, falls back to chat,
// and every later one goes straight to chat.
func TestMessagesLaneDegradesOn404(t *testing.T) {
	var paths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == messagesPath {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"404 Route Not Found"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer up.Close()
	d := testDriver(up.URL)
	for i := 0; i < 2; i++ {
		if _, err := d.Stream(context.Background(), bp131Call("claude-opus-5.5", "high"), func(ir.StreamEvent) error { return nil }); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}
	if strings.Join(paths, ",") != messagesPath+","+chatCompletionsPath+","+chatCompletionsPath {
		t.Fatalf("paths = %v, want probe + chat + straight chat", paths)
	}
	if d.messagesAllowed.Load() {
		t.Fatal("messagesAllowed must be pinned off after a route refusal")
	}
}

// Tool calls survive the lane: assistant tool_use and tool_result ride the
// Anthropic block shapes.
func TestMessagesToolRoundTrip(t *testing.T) {
	var gotBody map[string]any
	var gotRaw string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotRaw = string(b)
		gotBody = map[string]any{}
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `event: content_block_start`)
		fmt.Fprintln(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"read_file"}}`)
		fmt.Fprintln(w, `event: content_block_delta`)
		fmt.Fprintln(w, `data: {"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"path\":\"x\"}"}}`)
		fmt.Fprintln(w, `event: message_delta`)
		fmt.Fprintln(w, `data: {"type":"message_delta","usage":{"output_tokens":2}}`)
		fmt.Fprintln(w, `event: message_stop`)
		fmt.Fprintln(w, `data: {"type":"message_stop"}`)
	}))
	defer up.Close()
	d := testDriver(up.URL)
	call := bp131Call("claude-opus-5.5", "high")
	call.Request.Messages = append(call.Request.Messages,
		ir.Message{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "t0", Name: "read_file", Arguments: `{"path":"y","n":9007199254740993}`}}},
		ir.Message{Role: ir.RoleTool, ToolCallID: "t0", ToolName: "read_file",
			Content: []ir.ContentPart{{Kind: "text", Text: "contents"}}},
	)
	call.Request.Tools = []ir.ToolDef{{Name: "read_file", Description: "read", Parameters: map[string]any{"type": "object"}}}
	resp, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Name != "read_file" {
		t.Fatalf("tool calls = %+v", resp.Message.ToolCalls)
	}
	// Outbound: tool_result block paired with the call; arguments raw (the
	// 64-bit id must not round).
	msgs := gotBody["messages"].([]any)
	found := false
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] != "user" {
			continue
		}
		blocks, _ := mm["content"].([]any)
		if len(blocks) > 0 {
			if b, ok := blocks[0].(map[string]any); ok && b["type"] == "tool_result" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("tool_result block missing: %v", msgs)
	}
	// The digits check reads the RAW wire, not the test's own decode: decoding
	// into map[string]any rounds every number to float64.
	if !strings.Contains(gotRaw, "9007199254740993") {
		t.Fatalf("arguments rounded on the wire: %s", gotRaw)
	}
}
