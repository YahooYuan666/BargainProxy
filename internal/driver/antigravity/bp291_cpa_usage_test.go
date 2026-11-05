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

// BP-291 (CLIProxyAPI be69162): some antigravity builds carry the usage
// under cpaUsageMetadata instead of usageMetadata. The parser reads it as a
// fallback so those frames don't book as "no usage reported"; a real
// usageMetadata still wins.

func bp291Stream(t *testing.T, frame string) *ir.ChatResponse {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, frame)
	}))
	t.Cleanup(upstream.Close)
	resp, err := testDriver(upstream.URL, upstream.URL+"/token").Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(t),
		UpstreamModel: "gemini-3.8-flash-high",
		Request: &ir.ChatRequest{
			Model:    "gemini-3.8-flash-high",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}, func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	return resp
}

func TestBP291CpaUsageMetadataFallback(t *testing.T) {
	resp := bp291Stream(t, `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"cpaUsageMetadata":{"promptTokenCount":12,"candidatesTokenCount":7,"thoughtsTokenCount":3},"modelVersion":"test"}}`)
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 7 || resp.Usage.ReasoningTokens != 3 {
		t.Fatalf("usage = %+v, want in=12 out=7 reasoning=3 from cpaUsageMetadata", resp.Usage)
	}
}

func TestBP291UsageMetadataStillWins(t *testing.T) {
	resp := bp291Stream(t, `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2},"cpaUsageMetadata":{"promptTokenCount":99,"candidatesTokenCount":99},"modelVersion":"test"}}`)
	if resp.Usage.InputTokens != 5 || resp.Usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v, want usageMetadata (5/2) to win over cpaUsageMetadata", resp.Usage)
	}
}

func TestBP291CpaOnUnwrappedFrame(t *testing.T) {
	resp := bp291Stream(t, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"cpaUsageMetadata":{"promptTokenCount":8,"candidatesTokenCount":4},"modelVersion":"test"}`)
	if resp.Usage.InputTokens != 8 || resp.Usage.OutputTokens != 4 {
		t.Fatalf("usage = %+v, want in=8 out=4 from the unwrapped cpaUsageMetadata", resp.Usage)
	}
}
