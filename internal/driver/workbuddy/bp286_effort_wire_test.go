package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"bargainproxy/internal/ir"
)

// BP-286, the far end of the chain: whatever gear the ingress parsed must come
// out of the driver as reasoning_effort.
//
// The ingress fix alone would not be enough — a driver that re-derives or
// clamps the gear would undo it. This pins the value that actually leaves the
// gateway, per gear, so the two halves cannot drift apart.
func TestEffortReachesUpstreamBody(t *testing.T) {
	for _, gear := range []string{"low", "medium", "high", "xhigh", "max"} {
		t.Run(gear, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body = map[string]any{}
				_ = json.NewDecoder(r.Body).Decode(&body)
				fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`)
				fmt.Fprintln(w, `data: [DONE]`)
			}))
			defer srv.Close()

			d := testDriver(srv.URL)
			call := bp131Call("deepseek-v4.1-flash", gear)
			call.Request.Thinking = &ir.ThinkingConfig{Enabled: true, Level: gear}
			if _, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil }); err != nil {
				t.Fatalf("stream: %v", err)
			}
			if got, _ := body["reasoning_effort"].(string); got != gear {
				t.Fatalf("upstream reasoning_effort = %q, want %q", got, gear)
			}
		})
	}
}

// A gear the client named must not be re-clamped for the CN realm. Only the
// international deployment pins "high" (it rejects other values upstream);
// the CN one has no such restriction, and pinning there is what made every
// user choice look identical.
func TestCNRealmKeepsClientGear(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()

	d := testDriver(srv.URL) // testAccount is realm=cn
	call := bp131Call("deepseek-v4.1-flash", "max")
	call.Request.Thinking = &ir.ThinkingConfig{Enabled: true, Level: "max"}
	if _, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got, _ := body["reasoning_effort"].(string); got != "max" {
		t.Fatalf("CN realm reasoning_effort = %q, want max", got)
	}
}
