// BP-53: Gemini reports prefix-cache hits as usageMetadata.cachedContentTokenCount.
// The field is a pointer end-to-end: an omitted field means "not reported" and
// must not read as a reported zero (the panel's hit-rate denominator depends on
// telling those apart).
package antigravity

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

func streamWithUsage(t *testing.T, usage string) *ir.ChatResponse {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, sseFrame(`{"text":"ok"}`, usage, "STOP"))
	}))
	t.Cleanup(upstream.Close)
	d := testDriver(upstream.URL, upstream.URL+"/token")
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(t),
		UpstreamModel: "gemini-3.8-flash-high",
		Request: &ir.ChatRequest{
			Model:    "gemini-3.8-flash-high",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}, func(ev ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	return resp
}

func TestStreamRecordsCachedContentTokens(t *testing.T) {
	resp := streamWithUsage(t, `{"promptTokenCount":10000,"candidatesTokenCount":5,"cachedContentTokenCount":8192}`)
	if !resp.Usage.CacheKnown || resp.Usage.CachedInputTokens != 8192 {
		t.Fatalf("cache = %d known=%v, want 8192/true", resp.Usage.CachedInputTokens, resp.Usage.CacheKnown)
	}
}

func TestStreamWithoutCachedFieldStaysUnknown(t *testing.T) {
	resp := streamWithUsage(t, `{"promptTokenCount":10000,"candidatesTokenCount":5}`)
	if resp.Usage.CacheKnown {
		t.Fatalf("cache must stay unknown when the field is absent: %+v", resp.Usage)
	}
}

func TestStreamReportsZeroCacheAsKnown(t *testing.T) {
	resp := streamWithUsage(t, `{"promptTokenCount":10000,"candidatesTokenCount":5,"cachedContentTokenCount":0}`)
	if !resp.Usage.CacheKnown || resp.Usage.CachedInputTokens != 0 {
		t.Fatalf("cache = %d known=%v, want 0/true", resp.Usage.CachedInputTokens, resp.Usage.CacheKnown)
	}
}
