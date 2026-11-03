package trae

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// BP-274 (traework2api-panel c5aec914ff): the vendor's parallel tool-call
// stream carries index-only placeholder entries. mergeToolDeltas only gates
// the EVENT emission (no EvToolStart without a name/id) — but the final
// aggregation used to append EVERY accumulator, so the non-stream response
// shipped {"id":"","name":"","arguments":""} and strict clients failed the
// whole round.
func TestParallelToolPlaceholderDroppedFromAggregation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, strings.Join([]string{
			"event:output",
			`data:{"response":"","tool_calls":[{"index":0,"id":"call_1","type":"function","function_call":{"name":"get_weather","arguments":"{\"city\":\"BJ\"}"}}]}`,
			"",
			"event:output",
			`data:{"response":"","tool_calls":[{"index":1}]}`,
			"",
			"event:done",
			`data:{"finish_reason":"tool_calls"}`,
			"",
		}, "\n"))
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL)
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3-flashx",
		Request:       &ir.ChatRequest{Model: "glm-5.3-flashx", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}}},
	}, func(ev ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want exactly the real call (placeholder dropped)", resp.Message.ToolCalls)
	}
	if resp.Message.ToolCalls[0].Name != "get_weather" || resp.Message.ToolCalls[0].ID != "call_1" {
		t.Fatalf("real call mangled: %+v", resp.Message.ToolCalls[0])
	}
}

// A placeholder-only stream aggregates to zero tool calls without an error.
func TestPlaceholderOnlyStreamAggregatesEmpty(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Join([]string{
			`data:{"choices":[{"delta":{"tool_calls":[{"index":0}]}}]}`,
			`data:{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}, "\n"))
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL)
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3-flashx",
		Request:       &ir.ChatRequest{Model: "glm-5.3-flashx", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}}},
	}, func(ev ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(resp.Message.ToolCalls) != 0 {
		t.Fatalf("tool calls = %+v, want none", resp.Message.ToolCalls)
	}
}
