package zcode

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/uperr"
)

// BP-254: a 200 carrying a JSON object is an error, not a stream.
//
// The start-plan endpoint answers quota refusals under HTTP 200:
//
//	{"code":1005,"msg":"exceed quota limit","logid":"..."}
//
// Fed into the SSE parser that becomes "stream ended without message_stop"
// (class=transport), which reads as a broken channel, burns 3 account attempts
// per request, and makes the per-model quota wall in classifyBusiness
// unreachable. Field 2026-11-01: every GLM turn went zcode(dead)→autoclaw→
// workbuddy→qoder, and deepseek's overflow reached lobster's 5MB wall.
func TestNonStream200IsClassifiedAsBusinessError(t *testing.T) {
	const quotaBody = `{"code":1005,"msg":"exceed quota limit","logid":"20261031174833abc"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, quotaBody)
	}))
	defer srv.Close()

	d := testDriver(srv.URL)
	call := driver.ChatCall{Account: testAccount(), UpstreamModel: "glm-5.3-flash",
		Request: probeRequest("glm-5.3-flash")}
	_, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("a 200 JSON error body must not be reported as success")
	}
	ue, ok := uperr.As(err)
	if !ok {
		t.Fatalf("want *uperr.Error, got %T: %v", err, err)
	}
	// 1005 is a QUOTA wall → model-scoped rate limit, never "transport".
	if ue.Class == uperr.ClassTransport {
		t.Fatalf("class=transport for a quota refusal; the channel looks broken "+
			"instead of the model being out of plan: %s", ue.Message)
	}
	if ue.Model == "" {
		t.Fatalf("Model is empty; the wall must be model-scoped so this channel's "+
			"OTHER models stay live: %s", ue.Message)
	}
	if !strings.Contains(strings.ToLower(ue.Message), "quota") {
		t.Fatalf("message should name the quota wall: %q", ue.Message)
	}
}

// A 200 whose body is neither a stream nor a recognisable business error must
// still fail clearly and name what arrived — never a silent empty success.
func TestNonStream200UnknownBodyFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html>gateway error</html>")
	}))
	defer srv.Close()

	d := testDriver(srv.URL)
	call := driver.ChatCall{Account: testAccount(), UpstreamModel: "glm-5.3-flash",
		Request: probeRequest("glm-5.3-flash")}
	_, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("an HTML page under 200 must not be reported as success")
	}
	ue, ok := uperr.As(err)
	if !ok {
		t.Fatalf("want *uperr.Error, got %T", err)
	}
	if !strings.Contains(ue.Message, "not a stream") {
		t.Fatalf("message should say the answer was not a stream: %q", ue.Message)
	}
}

// A real SSE stream must keep working — the fix must not treat every 200 as an
// error.
func TestRealStreamStillParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	d := testDriver(srv.URL)
	call := driver.ChatCall{Account: testAccount(), UpstreamModel: "glm-5.3-flash",
		Request: probeRequest("glm-5.3-flash")}
	resp, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("a real stream must parse: %v", err)
	}
	if ir.TextOf(resp.Message.Content) != "ok" {
		t.Fatalf("text = %q, want ok", ir.TextOf(resp.Message.Content))
	}
}

// probeRequest is a minimal streaming chat request.
func probeRequest(model string) *ir.ChatRequest {
	return &ir.ChatRequest{
		Model:     model,
		Stream:    true,
		MaxTokens: 64,
		Messages: []ir.Message{{
			Role:    ir.RoleUser,
			Content: []ir.ContentPart{{Kind: "text", Text: "Reply with exactly: ok"}},
		}},
	}
}
