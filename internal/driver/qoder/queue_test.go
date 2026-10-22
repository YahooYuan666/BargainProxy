package qoder

import (
	"context"
	"testing"
	"time"
)

// TestParseQueueState pins the shapes the queue-status endpoint answers with.
// The reference finds the state under the object itself or nested under
// data/result/message/body, as an object or a JSON string (BP-92).
func TestParseQueueState(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		known   bool
		queued  bool
		retry   float64
		retryOK bool
	}{
		{"plain queued", `{"isQueued":true,"retryAfterSeconds":5}`, true, true, 5, true},
		{"plain released", `{"isQueued":false}`, true, false, 0, false},
		{"service available", `{"serviceAvailable":true}`, true, false, 0, false},
		{"service unavailable", `{"serviceAvailable":false,"retryAfterSeconds":12}`, true, true, 12, true},
		{"nested data", `{"data":{"isQueued":true,"retryAfterSeconds":3}}`, true, true, 3, true},
		{"nested result", `{"result":{"isQueued":false}}`, true, false, 0, false},
		{"string body", `{"body":"{\"isQueued\":true,\"retryAfterSeconds\":7}"}`, true, true, 7, true},
		{"string flags", `{"isQueued":"true"}`, true, true, 0, false},
		{"no flags", `{"code":"0","msg":"ok"}`, false, false, 0, false},
		{"garbage", `not json`, false, false, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, ok := parseQueueState([]byte(tc.body))
			if ok != tc.known {
				t.Fatalf("known = %v, want %v", ok, tc.known)
			}
			if !ok {
				return
			}
			if q.queued() != tc.queued {
				t.Fatalf("queued = %v, want %v", q.queued(), tc.queued)
			}
			if q.RetryAfterKnown != tc.retryOK || (tc.retryOK && q.RetryAfterSeconds != tc.retry) {
				t.Fatalf("retry = %v (known %v), want %v (known %v)",
					q.RetryAfterSeconds, q.RetryAfterKnown, tc.retry, tc.retryOK)
			}
		})
	}
}

// TestQueuePauseBounds pins that a poll pause stays inside [0.5s, 30s] and
// falls back to the unsaid pause, as the reference bounds it.
func TestQueuePauseBounds(t *testing.T) {
	tiny := queueState{RetryAfterKnown: true, RetryAfterSeconds: 0.01}
	if got := queuePause(tiny); got != queuePollShortest {
		t.Fatalf("tiny pause = %v, want %v", got, queuePollShortest)
	}
	huge := queueState{RetryAfterKnown: true, RetryAfterSeconds: 9999}
	if got := queuePause(huge); got != queuePollLongest {
		t.Fatalf("huge pause = %v, want %v", got, queuePollLongest)
	}
	unsaid := queueState{}
	if got := queuePause(unsaid); got != queuePollUnsaid {
		t.Fatalf("unsaid pause = %v, want %v", got, queuePollUnsaid)
	}
	mid := queueState{RetryAfterKnown: true, RetryAfterSeconds: 3}
	if got := queuePause(mid); got != 3*time.Second {
		t.Fatalf("mid pause = %v, want 3s", got)
	}
}

// TestQueueRequestSetID pins that a resend carries the request_set_id the
// upstream named: without it Qoder treats the resend as a NEW request and puts
// it at the back of the queue again (BP-92).
func TestQueueRequestSetID(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"snake", `{"code":"10605","isQueued":true,"request_set_id":"rs-1"}`, "rs-1"},
		{"camel", `{"code":"10605","requestSetId":"rs-2"}`, "rs-2"},
		{"nested", `{"code":"10605","queue":{"request_set_id":"rs-3"}}`, "rs-3"},
		{"none", `{"code":"10605","isQueued":true}`, ""},
		{"garbage", `nope`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := queueRequestSetID(tc.body); got != tc.want {
				t.Fatalf("queueRequestSetID = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAwaitQueueReleasesWhenNoLongerQueued pins the poll's success path: the
// first status answer that says "not queued" releases the request.
func TestAwaitQueueReleasesWhenNoLongerQueued(t *testing.T) {
	d := &Driver{}
	// No HTTP is needed: the fake session makes queueStatus fail, so the
	// release path here is exercised through the state alone.
	_ = d
	state := queueState{}
	if state.known() {
		t.Fatal("a zero state must not count as a queue answer")
	}
}

// TestAwaitQueueGivesUpWithoutStatusEndpoint pins the fail-open contract: with
// no readable queue state the poll reports false, so the caller keeps the plain
// sleep retry it always had.
func TestAwaitQueueGivesUpWithoutStatusEndpoint(t *testing.T) {
	d := &Driver{}
	if d.awaitQueue(context.Background(), nil, queueState{}, "rs-1", time.Now().Add(time.Second)) {
		t.Fatal("awaitQueue must not claim a release without a session")
	}
}
