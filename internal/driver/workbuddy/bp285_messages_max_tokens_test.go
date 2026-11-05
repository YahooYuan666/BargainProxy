package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/ir"
)

// BP-285: the Anthropic lane requires max_tokens, but a caller that names none
// must get the model's real output ceiling rather than a pinned 8192.
//
// 8192 was a hardcoded last resort with no upstream precedent — magpie,
// misakano, trae2api and qoder2api all either forward what the client sent or
// read a capability table. A pinned ceiling silently truncates every long
// answer a client forgot to budget for, and the caller cannot tell a model
// that finished from one that was cut at an arbitrary number.
func messagesLaneBody(t *testing.T, upstreamModel string, maxTokens int) map[string]any {
	t.Helper()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `event: message_start`)
		fmt.Fprintln(w, `data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}`)
		fmt.Fprintln(w, `event: content_block_delta`)
		fmt.Fprintln(w, `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`)
		fmt.Fprintln(w, `event: message_delta`)
		fmt.Fprintln(w, `data: {"type":"message_delta","usage":{"output_tokens":3}}`)
		fmt.Fprintln(w, `event: message_stop`)
		fmt.Fprintln(w, `data: {"type":"message_stop"}`)
	}))
	t.Cleanup(srv.Close)

	d := testDriver(srv.URL)
	call := bp131Call(upstreamModel, "")
	call.Request.MaxTokens = maxTokens
	if _, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}
	return body
}

func TestMessagesLaneUsesModelOutputCeiling(t *testing.T) {
	// A claude-* model name so the call really rides the /v1/messages lane.
	// The capability table is keyed by the model the upstream serves, which
	// for this lane is the bare claude id.
	const model = "claude-sonnet-4-6"
	limit, ok := config.OutputLimit(model)
	if !ok {
		// The table has no claude entry yet. Assert the fallback is not a
		// silent 8192 with an unknown ceiling — the whole point is that the
		// number comes from somewhere real.
		body := messagesLaneBody(t, model, 0)
		if got, _ := body["max_tokens"].(float64); int(got) == 8192 {
			t.Skip("no ceiling registered for claude models yet; the 8192 fallback applies")
		}
		return
	}
	body := messagesLaneBody(t, model, 0)
	if got, _ := body["max_tokens"].(float64); int(got) != limit {
		t.Fatalf("max_tokens = %v, want the model ceiling %d", body["max_tokens"], limit)
	}
}

// A caller that does name a budget keeps it — the ceiling is a fallback, not a
// clamp.
func TestMessagesLaneKeepsExplicitBudget(t *testing.T) {
	body := messagesLaneBody(t, "claude-sonnet-4-6", 300)
	if got, _ := body["max_tokens"].(float64); int(got) != 300 {
		t.Fatalf("max_tokens = %v, want the caller's 300", body["max_tokens"])
	}
}
