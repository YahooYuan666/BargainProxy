package autoclaw

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// TestIsDroppedSocket pins the narrow shape that may be resent blindly: the
// connection died before any response bytes arrived. A mid-stream read error
// must NOT qualify — resending then would duplicate output the caller already
// saw.
func TestIsDroppedSocket(t *testing.T) {
	dropped := []error{
		uperr.New(uperr.ClassTransport, 0, `autoclaw managed: Post "https://x/y": unexpected EOF`),
		uperr.New(uperr.ClassTransport, 0, "read tcp: connection reset by peer"),
		uperr.New(uperr.ClassTransport, 0, "http2: stream closed"),
		uperr.New(uperr.ClassTransport, 0, "broken pipe"),
	}
	for _, err := range dropped {
		if !isDroppedSocket(err) {
			t.Errorf("%v should be retryable", err)
		}
	}

	notDropped := []error{
		nil,
		// A different class: auth, quota and content failures are decisions,
		// not transport accidents.
		uperr.New(uperr.ClassAuth, 401, "unexpected EOF"),
		uperr.New(uperr.ClassBadRequest, 406, "refused"),
		uperr.New(uperr.ClassUpstreamTimeout, 0, "stalled"),
		// Transport, but the message names no dropped-socket shape.
		uperr.New(uperr.ClassTransport, 0, "something else entirely"),
		// Not an uperr at all.
		errors.New("unexpected EOF"),
	}
	for _, err := range notDropped {
		if isDroppedSocket(err) {
			t.Errorf("%v must NOT be retried blindly", err)
		}
	}
}

// TestDroppedSocketIsRetriedBeforeFailing pins the behaviour that made a
// healthy AutoClaw pool look dead: the CN managed gateway closes the
// connection with no bytes at all on roughly one request in five, and the very
// next identical request answers normally (field 2026-10-23: 2 of 8 failed at
// ~30s, then 16 consecutive 200s).
//
// Without this retry the dropped socket reached the pool, where it cost an
// account rest; the rest's expiry then put the account in the 2-minute
// sticky-only band, so ONE transient EOF blocked new conversations for two
// minutes. Here the retry is proven to happen inside the driver.
func TestDroppedSocketIsRetriedBeforeFailing(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// Drop the connection with no response at all: exactly what the
			// managed gateway does.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("test server cannot hijack")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	d := New(config.AutoClawSettings{AppSecret: "s", UserAPIBase: srv.URL}, srv.Client())
	acc := &model.Account{
		ID:        "ac-1",
		Channel:   model.ChannelAutoClaw,
		CredsJSON: []byte(`{"access_token":"Bearer t","uid":"u","region":"cn"}`),
	}
	call := driver.ChatCall{
		Account:       acc,
		UpstreamModel: "zai_glm-5.3-flash",
		Request: &ir.ChatRequest{
			Model:    "glm-5.3-flash",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}

	resp, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("Stream: %v (the dropped socket should have been resent)", err)
	}
	if resp == nil {
		t.Fatal("nil response")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one dropped, one retried)", got)
	}
}

// TestRetryGivesUpAfterTheBudget: a channel that keeps dropping must not be
// retried forever — the failure has to surface so the gateway can cascade.
func TestRetryGivesUpAfterTheBudget(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("test server cannot hijack")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		conn.Close()
	}))
	defer srv.Close()

	d := New(config.AutoClawSettings{AppSecret: "s", UserAPIBase: srv.URL}, srv.Client())
	acc := &model.Account{
		ID:        "ac-1",
		Channel:   model.ChannelAutoClaw,
		CredsJSON: []byte(`{"access_token":"Bearer t","uid":"u","region":"cn"}`),
	}
	call := driver.ChatCall{
		Account:       acc,
		UpstreamModel: "zai_glm-5.3-flash",
		Request: &ir.ChatRequest{
			Model:    "glm-5.3-flash",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}

	_, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("a channel that always drops must fail")
	}
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassTransport {
		t.Fatalf("class = %v, want transport", err)
	}
	// 1 initial + transportRetries resends.
	if want := int32(1 + transportRetries); atomic.LoadInt32(&calls) != want {
		t.Fatalf("upstream calls = %d, want %d (initial + retry budget)",
			atomic.LoadInt32(&calls), want)
	}
}

// Test406RetryIsNotTreatedAsDroppedSocket: the system-as-user retry is for a
// content refusal, and must not be conflated with a transport retry (that
// would resend a refused body up to transportRetries more times).
func Test406RetryIsNotTreatedAsDroppedSocket(t *testing.T) {
	err := uperr.New(uperr.ClassBadRequest, 406, "autoclaw: request refused by the gateway's content check (406): ")
	if isDroppedSocket(err) {
		t.Fatal("a 406 refusal must never be resent as a dropped socket")
	}
	if !strings.Contains(err.Error(), "406") {
		t.Fatal("sanity: the message should carry the status")
	}
}
