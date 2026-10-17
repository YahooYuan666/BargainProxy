// BP-38: cascade attempts are visible in the request log (attempts +
// channel trail), so "did this request probe another channel first?" is
// answerable from the panel instead of guesswork.
package server_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/store"
)

func lastRequestLog(t *testing.T, st *store.Memory) model.RequestLog {
	t.Helper()
	rows, err := st.RecentRequestLogs(10)
	if err != nil {
		t.Fatalf("RecentRequestLogs: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no request log rows recorded")
	}
	return rows[0]
}

// A first-byte-window cascade must log attempts=2 and the trail
// "workbuddy→openaicompat" while the success row keeps the serving channel.
func TestCascadeLogsAttemptsAndTrail(t *testing.T) {
	done := make(chan struct{})
	var wbHits, compatHits atomic.Int64
	withUpstreamTimeouts(t, 10*time.Second, 0)
	srv, _, st := twoBindingHarness(t, stalledHandler(done, &wbHits), healthyCompatHandler(&compatHits), 120*time.Millisecond)
	t.Cleanup(func() { close(done) })

	status, content := postGLM53(t, srv)
	if status != 200 || content != "fallback-ok" {
		t.Fatalf("status=%d content=%q, want 200 fallback-ok", status, content)
	}
	row := lastRequestLog(t, st)
	if row.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (workbuddy 首包窗口 → compat)", row.Attempts)
	}
	if row.AttemptChannels != "workbuddy→openaicompat" {
		t.Fatalf("attempt_channels = %q, want workbuddy→openaicompat", row.AttemptChannels)
	}
	if row.Channel != model.ChannelOpenAICompat || row.Status != 200 {
		t.Fatalf("row channel=%q status=%d, want openaicompat/200", row.Channel, row.Status)
	}
}

// A straight first-binding success stays at attempts=1 with no trail arrow.
func TestSingleBindingSuccessLogsOneAttempt(t *testing.T) {
	var wbHits, compatHits atomic.Int64
	wbHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wbHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"direct-ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	})
	srv, _, st := twoBindingHarness(t, wbHandler, healthyCompatHandler(&compatHits), 5*time.Second)

	status, content := postGLM53(t, srv)
	if status != 200 || content != "direct-ok" {
		t.Fatalf("status=%d content=%q, want 200 direct-ok", status, content)
	}
	if got := compatHits.Load(); got != 0 {
		t.Fatalf("compat hits = %d, want 0 (单腿直达不得探测备用腿)", got)
	}
	row := lastRequestLog(t, st)
	if row.Attempts != 1 || row.AttemptChannels != "workbuddy" {
		t.Fatalf("attempts=%d trail=%q, want 1/workbuddy", row.Attempts, row.AttemptChannels)
	}
}

// A failed cascade still records how far it got: the row keeps the last
// attempted channel instead of an empty channel column.
func TestFailedCascadeLogsChannelAndAttempts(t *testing.T) {
	rateLimited := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"code":6004,"msg":"rate limited"}}`)
	})
	badCompat := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"bad payload"}}`)
	})
	srv, _, st := twoBindingHarness(t, rateLimited, badCompat, 5*time.Second)

	status, _ := postGLM53(t, srv)
	if status == 200 {
		t.Fatal("want a failure status")
	}
	row := lastRequestLog(t, st)
	if row.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", row.Attempts)
	}
	if row.AttemptChannels != "workbuddy→openaicompat" {
		t.Fatalf("attempt_channels = %q, want workbuddy→openaicompat", row.AttemptChannels)
	}
	if row.Channel != model.ChannelOpenAICompat {
		t.Fatalf("channel = %q, want openaicompat (最后尝试渠道，失败行不再空渠道)", row.Channel)
	}
}

// Streaming requests on the Anthropic protocol must record first-byte time
// too (BP-40: 反应时间 was only measured on non-streamed paths).
func TestAnthropicStreamRecordsTTFB(t *testing.T) {
	var wbHits, compatHits atomic.Int64
	wbHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wbHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		time.Sleep(250 * time.Millisecond) // first byte deliberately delayed
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
		fl.Flush()
	})
	srv, _, st := twoBindingHarness(t, wbHandler, healthyCompatHandler(&compatHits), 5*time.Second)

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"glm-5.3","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	row := lastRequestLog(t, st)
	if row.Protocol != "anthropic-messages" || !row.Stream {
		t.Fatalf("row = %+v, want anthropic-messages stream", row)
	}
	if row.TTFBMs < 200 {
		t.Fatalf("ttfb_ms = %d, want ≥200 (首帧延迟 250ms 必须被记录)", row.TTFBMs)
	}
	if row.DurationMs < row.TTFBMs {
		t.Fatalf("duration %d < ttfb %d", row.DurationMs, row.TTFBMs)
	}
}
