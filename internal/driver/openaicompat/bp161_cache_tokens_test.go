package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

// bp161Stream runs a full Stream over the given SSE frames and returns the
// aggregated usage.
func bp161Stream(t *testing.T, frames ...string) ir.Usage {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprintln(w, f)
		}
	}))
	t.Cleanup(up.Close)

	d := New(config.OpenAICompatSettings{Enabled: true, BaseURL: up.URL + "/v1", APIKey: "k"}, nil)
	creds, _ := json.Marshal(map[string]string{"api_key": "k"})
	acc := &model.Account{ID: "compat-1", Channel: model.ChannelOpenAICompat, CredsJSON: creds}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := d.Stream(ctx, driver.ChatCall{
		Account: acc, UpstreamModel: "deepseek-v4.1-flash",
		Request: &ir.ChatRequest{Model: "deepseek-v4.1-flash", Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}, func(ev ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	return out.Usage
}

// BP-161: DeepSeek's flat prompt_cache_hit_tokens must land in
// CachedInputTokens — through this generic driver every cached turn used to
// report zero hits and the sticky session's cache metrics went blind.
func TestBP161DeepSeekCacheHitTokens(t *testing.T) {
	usage := bp161Stream(t,
		`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_cache_hit_tokens":86}}`,
		`data: [DONE]`,
	)
	if usage.CachedInputTokens != 86 {
		t.Fatalf("cached = %d, want 86", usage.CachedInputTokens)
	}
	if !usage.CacheKnown {
		t.Fatal("cache must be known, not absent")
	}
	if usage.InputTokens != 100 || usage.OutputTokens != 5 {
		t.Fatalf("base tokens broken: %+v", usage)
	}
}

// The OpenAI details shape wins when both are present.
func TestBP161OpenAICachedDetailsShape(t *testing.T) {
	usage := bp161Stream(t,
		`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":64}}}`,
		`data: [DONE]`,
	)
	if usage.CachedInputTokens != 64 || !usage.CacheKnown {
		t.Fatalf("cached = %d known=%v, want 64/true", usage.CachedInputTokens, usage.CacheKnown)
	}
}
