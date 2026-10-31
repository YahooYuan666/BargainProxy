package workbuddy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"bargainproxy/internal/ir"
	"bargainproxy/internal/uperr"
)

// BP-222: a 200 stream the vendor ends with finish_reason "error" is its word
// for a FAILED turn, not a finished one (magpie 86fef947 #2471). The old
// default mapped it to StopEnd, so the client got a half answer as a success
// and the gateway never cascaded. It must surface as ClassTransport — no
// account penalty, cascade or continuation decide (gateway side).
func TestBP222FinishReasonErrorFailsTheTurn(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"partial ans"}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"error"}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer up.Close()

	d := testDriver(up.URL)
	call := wbCall("glm-5.3", "")
	call.Account = testAccount()
	resp, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatalf("finish_reason \"error\" must fail the attempt, got a success with stop=%q", resp.StopReason)
	}
	ue, ok := err.(*uperr.Error)
	if !ok {
		t.Fatalf("want *uperr.Error, got %T: %v", err, err)
	}
	if ue.Class != uperr.ClassTransport {
		t.Fatalf("class = %s, want ClassTransport (no account penalty)", ue.Class)
	}
}

// The normal words keep mapping as before — the guard must not over-trigger.
func TestBP222FinishReasonStopStillSucceeds(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer up.Close()

	d := testDriver(up.URL)
	call := wbCall("glm-5.3", "")
	call.Account = testAccount()
	resp, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("ordinary stop must succeed: %v", err)
	}
	if resp.StopReason != ir.StopEnd {
		t.Fatalf("stop reason = %q, want %q", resp.StopReason, ir.StopEnd)
	}
}
