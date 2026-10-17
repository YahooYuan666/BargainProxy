// ticket 27: upstream watchdog classification — header timeout, idle stall,
// client cancellation must be told apart from plain transport jitter.
package workbuddy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/uperr"
)

// withTimeouts injects small watchdogs and restores the defaults afterwards.
// Must run BEFORE driver construction: the chat client captures the header
// timeout when it is built.
func withTimeouts(t *testing.T, header, idle time.Duration) {
	t.Helper()
	httpx.Configure(0, header, idle)
	t.Cleanup(func() {
		httpx.Configure(httpx.DefaultRPCTimeout, httpx.DefaultHeaderTimeout, httpx.DefaultIdleTimeout)
	})
}

func watchdogDriver(chatBase string) *Driver {
	return New(config.WorkBuddySettings{
		Enabled:    true,
		ChatBaseCN: chatBase,
		ChatBaseGL: chatBase,
		PromptMode: "custom",
	}, nil)
}

func classOf(t *testing.T, err error) string {
	t.Helper()
	ue, ok := uperr.As(err)
	if !ok {
		t.Fatalf("not a *uperr.Error: %v", err)
	}
	return ue.Class
}

// stalledServer builds a test upstream whose handlers park until the test
// releases them (done) — httptest.Server.Close waits for in-flight handlers,
// so cleanup must close done before up.Close (LIFO registration order).
func stalledServer(t *testing.T, firstChunk bool) *httptest.Server {
	t.Helper()
	done := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if firstChunk {
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"a"}}]}`)
			fl.Flush()
		}
		select {
		case <-done:
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(up.Close)               // LIFO: runs second
	t.Cleanup(func() { close(done) }) // LIFO: runs first (unblocks handlers)
	return up
}

func streamOnce(t *testing.T, d *Driver, ctx context.Context) error {
	t.Helper()
	_, err := d.Stream(ctx, driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{
			Model:    "glm-5.3",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}, func(ev ir.StreamEvent) error { return nil })
	return err
}

// TestStreamHeaderTimeoutIsUpstreamTimeout: 上游接了连接但不回响应头 →
// 看门狗掐断 → ClassUpstreamTimeout（不轮转不罚号的前提分类）。
func TestStreamHeaderTimeoutIsUpstreamTimeout(t *testing.T) {
	up := stalledServer(t, false)
	withTimeouts(t, 80*time.Millisecond, 0)

	d := watchdogDriver(up.URL)
	start := time.Now()
	err := streamOnce(t, d, context.Background())
	if err == nil {
		t.Fatal("want header timeout error")
	}
	if got := classOf(t, err); got != uperr.ClassUpstreamTimeout {
		t.Fatalf("class = %s, want %s (err: %v)", got, uperr.ClassUpstreamTimeout, err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("header watchdog took %v", elapsed)
	}
}

// TestStreamIdleStallIsUpstreamTimeout: 首帧后静默 → 空闲看门狗掐流 →
// ClassUpstreamTimeout（区别于普通读错 ClassTransport）。
func TestStreamIdleStallIsUpstreamTimeout(t *testing.T) {
	up := stalledServer(t, true)
	withTimeouts(t, 0, 90*time.Millisecond)

	d := watchdogDriver(up.URL)
	err := streamOnce(t, d, context.Background())
	if err == nil {
		t.Fatal("want idle-stall error")
	}
	if got := classOf(t, err); got != uperr.ClassUpstreamTimeout {
		t.Fatalf("class = %s, want %s (err: %v)", got, uperr.ClassUpstreamTimeout, err)
	}
}

// TestStreamClientCanceledIsCanceledClass: 调用方取消 ctx（客户端断连 /
// fbWait 首包窗口）→ ClassCanceled——零惩罚、由 gateway 决定是否级联。
func TestStreamClientCanceledIsCanceledClass(t *testing.T) {
	up := stalledServer(t, false)

	d := watchdogDriver(up.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(60 * time.Millisecond)
		cancel()
	}()
	err := streamOnce(t, d, ctx)
	if err == nil {
		t.Fatal("want cancellation error")
	}
	if got := classOf(t, err); got != uperr.ClassCanceled {
		t.Fatalf("class = %s, want %s (err: %v)", got, uperr.ClassCanceled, err)
	}
}
