// BP-330: one short transport hiccup on /v2/report used to cost a whole day of
// streak.
//
// Field evidence (2026-11-17 04:30–04:39): four international accounts failed
// back-to-back with `Post "https://www.workbuddy.ai/v2/report": EOF`, each
// after 5.01–5.03s. The streak went 15d/16d → 1d, because the activity report
// runs once per slot and an expired day cannot be backfilled. Meanwhile the CN
// accounts on workbuddy.cn reported fine — the .ai host was simply flaky for a
// few minutes.
//
// sendReport retries only the edge-jitter HTML 401; transport errors gave up on
// the first try. These tests pin which failures must be retried and — just as
// important — which must not: retrying a quota wall makes the risk control
// worse, not better.
package tasks_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/workbuddy"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
	"bargainproxy/internal/tasks"
)

// bp330Upstream is a WorkBuddy stand-in whose /v2/report answers according to
// a per-attempt plan; every other path (streak read-back included) succeeds.
type bp330Upstream struct {
	mu      sync.Mutex
	reports int
	// reportReplies[i] is what the i-th /v2/report call does.
	reportReplies []bp330Reply
}

// bp330Reply is one planned /v2/report answer. eof cuts the connection
// mid-response (the 2026-11-17 outage shape); otherwise status+body are
// served, status 0 meaning 200.
type bp330Reply struct {
	eof    bool
	status int
	body   string
}

func (r bp330Reply) String() string {
	if r.eof {
		return "eof"
	}
	return r.body
}

func bp330OK() bp330Reply { return bp330Reply{body: `{"code":0,"data":{}}`} }

func bp330EOF() bp330Reply { return bp330Reply{eof: true} }

func (u *bp330Upstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/report":
			u.mu.Lock()
			n := u.reports
			u.reports++
			reply := bp330OK()
			if n < len(u.reportReplies) {
				reply = u.reportReplies[n]
			}
			u.mu.Unlock()
			if reply.eof {
				// Hijack + close: the client sees an unexpected EOF on read,
				// the same shape as the 2026-11-17 outage.
				hj, ok := w.(http.Hijacker)
				if !ok {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
				}
				return
			}
			status := reply.status
			if status == 0 {
				status = http.StatusOK
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(reply.body))
		case "/activity/growth/streak":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":3}}}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	})
}

func (u *bp330Upstream) reportCalls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.reports
}

func bp330Scheduler(t *testing.T, up *bp330Upstream) (*tasks.Scheduler, *store.Memory, *pool.Pool) {
	t.Helper()
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Channels.WorkBuddy.ChatBaseCN = srv.URL
	cfg.Channels.WorkBuddy.ChatBaseGL = srv.URL
	cfg.Channels.WorkBuddy.WebBaseCN = srv.URL

	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelWorkBuddy: 2},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	creds, _ := json.Marshal(map[string]any{
		"access_token": "bp330-access", "refresh_token": "bp330-refresh",
		"uid": "bp330-uid", "realm": "global", "nickname": "bp330-glb",
	})
	p.Upsert(&model.Account{
		ID: "workbuddy:bp330-uid", Channel: model.ChannelWorkBuddy,
		CredsJSON: creds, UID: "bp330-uid", Realm: "global", Nickname: "bp330-glb",
	})

	drivers := map[string]driver.Driver{model.ChannelWorkBuddy: workbuddy.New(cfg.Channels.WorkBuddy, nil)}
	gw := gateway.New(cfg, p, router.New(cfg), drivers, st)
	return tasks.New(gw, st, p, nil), st, p
}

// TestBP330TransportFailureIsRetried: a mid-response EOF on the first attempt
// must not cost the day. The retry succeeds, the ledger row is ok, and the
// detail carries the streak the read-back reported.
func TestBP330TransportFailureIsRetried(t *testing.T) {
	up := &bp330Upstream{reportReplies: []bp330Reply{bp330EOF(), bp330OK()}}
	sched, st, _ := bp330Scheduler(t, up)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	detail, err := sched.RunTask(ctx, tasks.TaskActivityWorkBuddy, "")
	if err != nil {
		t.Fatalf("activity task failed after a single retryable EOF: %v (%s) — one 5-second transport hiccup must not cost the day's streak (BP-330)", err, detail)
	}
	if got := up.reportCalls(); got != 2 {
		t.Fatalf("/v2/report calls = %d, want 2 (first attempt EOF, one retry)", got)
	}
	if !strings.Contains(detail, "streak 3d") {
		t.Fatalf("detail = %q, want the streak read-back to be recorded", detail)
	}
	run, err := st.TaskRunFor("workbuddy:bp330-uid", tasks.TaskActivityWorkBuddy, time.Now().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ledger row: %v", err)
	}
	if run.Status != "ok" {
		t.Fatalf("ledger status = %q, want ok", run.Status)
	}
}

// TestBP330QuotaWallIsNotRetried: the upstream's own rate-limit jargon must
// not be hammered. Retrying a frequency wall deepens the risk control, so the
// attempt count stays at one.
func TestBP330QuotaWallIsNotRetried(t *testing.T) {
	// The real 429 shape from the field (2026-11-18 23:13, y200901152022):
	// HTTP 429 with the vendor's "reset at ..." wording.
	wall := bp330Reply{status: http.StatusTooManyRequests,
		body: `{"error":{"code":429,"msg":"usage exceeds frequency limit, your usage will reset at 2026-11-19 05:10:02"}}`}
	up := &bp330Upstream{reportReplies: []bp330Reply{wall, bp330OK()}}
	sched, _, _ := bp330Scheduler(t, up)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	detail, err := sched.RunTask(ctx, tasks.TaskActivityWorkBuddy, "")
	if err == nil {
		t.Fatalf("a rate wall must surface as a failure, got %q", detail)
	}
	if got := up.reportCalls(); got != 1 {
		t.Fatalf("/v2/report calls = %d, want 1 — a quota/rate wall must not be retried (BP-330)", got)
	}
}

// TestBP330AllAttemptsFailKeepsAttemptCount: when every attempt fails the
// ledger must say how many tries happened, so the panel can distinguish "one
// blip" from "the host is gone".
func TestBP330AllAttemptsFailKeepsAttemptCount(t *testing.T) {
	up := &bp330Upstream{reportReplies: []bp330Reply{bp330EOF(), bp330EOF(), bp330EOF(), bp330EOF(), bp330EOF()}}
	sched, st, _ := bp330Scheduler(t, up)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	detail, err := sched.RunTask(ctx, tasks.TaskActivityWorkBuddy, "")
	if err == nil {
		t.Fatalf("all attempts EOF: want failure, got %q", detail)
	}
	if got := up.reportCalls(); got < 2 {
		t.Fatalf("/v2/report calls = %d, want >= 2 (the retry never happened)", got)
	}
	if !strings.Contains(detail, "after ") || !strings.Contains(detail, "attempt") {
		t.Fatalf("detail = %q, want the attempt count recorded so the panel shows the retries", detail)
	}
	run, err := st.TaskRunFor("workbuddy:bp330-uid", tasks.TaskActivityWorkBuddy, time.Now().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ledger row: %v", err)
	}
	if run.Status != "failed" {
		t.Fatalf("ledger status = %q, want failed", run.Status)
	}
}
