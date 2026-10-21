package qoder

import (
	"testing"
	"time"
)

// TestQueueWaitSeconds pins the 10605 retry contract (qoder2api-hub #38/#57
// parity): the upstream queue answer carries retryAfterSeconds, and the driver
// must wait THAT long (plus a small jitter) instead of failing the request
// after one fixed 2s retry — the fixed wait is why the deepseek-v4.1-flash p1
// binding kept failing with "gateway busy (10605)".
func TestQueueWaitSeconds(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Duration
	}{
		{"with retryAfterSeconds", `{"code":"10605","isQueued":true,"retryAfterSeconds":30}`, 30 * time.Second},
		{"missing field", `{"code":"10605","isQueued":true}`, defaultQueueWait},
		// 0/negative is not a usable duration: fall back to the conservative
		// default rather than hammering the queue in a tight loop.
		{"zero falls back", `{"code":"10605","retryAfterSeconds":0}`, defaultQueueWait},
		{"garbage number", `{"code":"10605","retryAfterSeconds":"soon"}`, defaultQueueWait},
		{"not a queue answer", `{"code":"403","msg":"no"}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := queueWaitSeconds(tc.body); got != tc.want {
				t.Fatalf("queueWaitSeconds = %v, want %v", got, tc.want)
			}
		})
	}
}

// The single-round wait is capped: a huge retryAfterSeconds must not park one
// attempt for hours; the total budget bounds repeated rounds.
func TestQueueWaitCaps(t *testing.T) {
	if got := queueWaitSeconds(`{"code":"10605","retryAfterSeconds":99999}`); got != queueWaitMax {
		t.Fatalf("single wait = %v, want cap %v", got, queueWaitMax)
	}
	if queueWaitMax > queueWaitBudget {
		t.Fatalf("per-round cap %v exceeds total budget %v", queueWaitMax, queueWaitBudget)
	}
}
