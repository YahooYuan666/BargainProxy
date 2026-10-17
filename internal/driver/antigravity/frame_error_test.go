// ticket 36: mid-stream error frames on a 200 stream must classify the
// account instead of passing as success.
package antigravity

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/uperr"
)

// TestErrorFrameResourceExhausted 帧内 RESOURCE_EXHAUSTED（200 已开流后
// 上游报配额/限流）→ SoftRate，不再当成功。
func TestErrorFrameResourceExhausted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, sseFrame(`{"text":"partial"}`, "", ""))
		fmt.Fprintln(w, `data: {"error":{"code":429,"message":"Quota exceeded for quota metric","status":"RESOURCE_EXHAUSTED"}}`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL+"/token")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := d.Stream(ctx, driver.ChatCall{
		Account: testAccount(t), UpstreamModel: "gemini-3.8-flash-high",
		Request: &ir.ChatRequest{Model: "gemini-3.8-flash-high", Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}, func(ev ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("error frame must not pass as success")
	}
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassSoftRate {
		t.Fatalf("class = %v (%v), want soft_rate", ue, err)
	}
}

// TestErrorFrameInvalidArgument 帧内 INVALID_ARGUMENT → BadRequest（请求级，
// 不罚号不轮转）。
func TestErrorFrameInvalidArgument(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"error":{"code":400,"message":"Invalid JSON payload received","status":"INVALID_ARGUMENT"}}`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL+"/token")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := d.Stream(ctx, driver.ChatCall{
		Account: testAccount(t), UpstreamModel: "gemini-3.8-flash-high",
		Request: &ir.ChatRequest{Model: "gemini-3.8-flash-high", Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}, func(ev ir.StreamEvent) error { return nil })
	ue, ok := uperr.As(err)
	if err == nil || !ok || ue.Class != uperr.ClassBadRequest {
		t.Fatalf("class = %v (%v), want bad_request", ue, err)
	}
}

// TestNormalStreamStillFine 正常流（无 error 帧）不受影响。
func TestNormalStreamStillFine(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, sseFrame(`{"text":"ok"}`, "", "STOP"))
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL+"/token")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := d.Stream(ctx, driver.ChatCall{
		Account: testAccount(t), UpstreamModel: "gemini-3.8-flash-high",
		Request: &ir.ChatRequest{Model: "gemini-3.8-flash-high", Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}, func(ev ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("normal stream: %v", err)
	}
	if resp.Message.Content[0].Text != "ok" {
		t.Fatalf("text = %q", resp.Message.Content[0].Text)
	}
}
