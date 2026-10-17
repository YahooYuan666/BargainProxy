// BP-40 / BP-41: upstream credit accounting and thinking aggregation for the
// workbuddy channel (both ride the same final usage frame / reasoning deltas).
package workbuddy

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

func TestStreamCreditAndThinkingAggregation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"reasoning_content":"先想"},"finish_reason":null}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"reasoning_content":"再答"},"finish_reason":null}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"好"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":2,"credit":12.5}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	call := driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{
			Model:    "glm-5.3",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}
	var emitted strings.Builder
	resp, err := d.Stream(context.Background(), call, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvThinking {
			emitted.WriteString(ev.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if !resp.Usage.CreditKnown || resp.Usage.Credit != 12.5 {
		t.Fatalf("credit = %v known=%v, want 12.5/true (usage.credit 必须原样入库)", resp.Usage.Credit, resp.Usage.CreditKnown)
	}
	if resp.Thinking != "先想再答" {
		t.Fatalf("aggregated thinking = %q, want 先想再答", resp.Thinking)
	}
	if emitted.String() != "先想再答" {
		t.Fatalf("streamed thinking = %q, want 先想再答", emitted.String())
	}
}

// usage without a credit field must not look like "0 spent".
func TestStreamUsageWithoutCreditStaysUnknown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	call := driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{
			Model:    "glm-5.3",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}
	resp, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if resp.Usage.CreditKnown {
		t.Fatal("credit_known must stay false when upstream reports no credit field")
	}
}
