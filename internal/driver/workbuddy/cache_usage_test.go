// BP-53: prefix-cache accounting on the workbuddy channel — the final usage
// frame's prompt_cache_hit_tokens / prompt_cache_miss_tokens drive the panel's
// cache-hit-rate column; "absent" must stay distinguishable from a reported 0.
package workbuddy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

func cacheStream(t *testing.T, usage string) *ir.ChatResponse {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: {"choices":[],"usage":`+usage+`}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	t.Cleanup(upstream.Close)
	d := testDriver(upstream.URL)
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{
			Model:    "glm-5.3",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}, func(ev ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	return resp
}

func TestStreamRecordsCacheHitTokens(t *testing.T) {
	resp := cacheStream(t, `{"prompt_tokens":8000,"completion_tokens":2,"prompt_cache_hit_tokens":7808,"prompt_cache_miss_tokens":192}`)
	if !resp.Usage.CacheKnown || resp.Usage.CachedInputTokens != 7808 {
		t.Fatalf("cache = %d known=%v, want 7808/true", resp.Usage.CachedInputTokens, resp.Usage.CacheKnown)
	}
}

// A reported zero is a sample (first turn), not "no data".
func TestStreamReportsZeroHitAsKnown(t *testing.T) {
	resp := cacheStream(t, `{"prompt_tokens":8000,"completion_tokens":2,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":8000}`)
	if !resp.Usage.CacheKnown || resp.Usage.CachedInputTokens != 0 {
		t.Fatalf("cache = %d known=%v, want 0/true", resp.Usage.CachedInputTokens, resp.Usage.CacheKnown)
	}
}

// Miss-only frames derive the hit size from the prompt remainder.
func TestStreamDerivesHitFromMissOnly(t *testing.T) {
	resp := cacheStream(t, `{"prompt_tokens":8000,"completion_tokens":2,"prompt_cache_miss_tokens":2000}`)
	if !resp.Usage.CacheKnown || resp.Usage.CachedInputTokens != 6000 {
		t.Fatalf("cache = %d known=%v, want 6000/true", resp.Usage.CachedInputTokens, resp.Usage.CacheKnown)
	}
}

// No cache fields at all → unknown (must not count in the rate denominator).
func TestStreamWithoutCacheFieldsStaysUnknown(t *testing.T) {
	resp := cacheStream(t, `{"prompt_tokens":8000,"completion_tokens":2}`)
	if resp.Usage.CacheKnown {
		t.Fatalf("cache must stay unknown: %+v", resp.Usage)
	}
}
