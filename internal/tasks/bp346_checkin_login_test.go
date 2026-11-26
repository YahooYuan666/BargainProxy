// BP-346~349 through the scheduler: one AutoClaw check-in run must
//   1. raise the login-active event first (BP-346),
//   2. complete the inspiration task on the overseas lane only (BP-349),
//   3. still check in even when 1 or 2 is refused.
//
// The driver is a scripted fake so the ORDER and the tolerance are what the
// test pins; the wire shapes are pinned in the driver's own tests.
package tasks

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/autoclaw"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
)

// bp346Upstream records the order of the calls one check-in run makes.
type bp346Upstream struct {
	mu       sync.Mutex
	order    []string
	failUser bool // user-info answers a business error
	failInsp bool // inspiration answers a business error
	tasks    []string
}

func (u *bp346Upstream) seen(step string) {
	u.mu.Lock()
	u.order = append(u.order, step)
	u.mu.Unlock()
}

func (u *bp346Upstream) orderSnapshot() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.order...)
}

func (u *bp346Upstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/userapi/v1/user-info":
			u.seen("user-info")
			if u.failUser {
				_, _ = w.Write([]byte(`{"code":126001,"msg":"device not allowed"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"user_id":"u"}}`))
		case r.URL.Path == "/autoclaw-proxy/proxy/autoclaw-task-complete":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			id, _ := body["task_id"].(string)
			u.mu.Lock()
			u.tasks = append(u.tasks, id)
			u.mu.Unlock()
			if id == "daily_inspiration_center" {
				u.seen("inspiration")
				if u.failInsp {
					_, _ = w.Write([]byte(`{"code":650301,"msg":"not eligible"}`))
					return
				}
				_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"already_completed":false,"reward_points":200}}`))
				return
			}
			u.seen("checkin:" + id)
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"already_completed":false,"reward_points":200}}`))
		case strings.HasPrefix(r.URL.Path, "/agent-assetmgr/"):
			// Probe after the check-in.
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"total_balance":3264,"expiring_points":0}}`))
		default:
			t := r.URL.Path
			_ = t
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		}
	})
}

func bp346Scheduler(t *testing.T, up *bp346Upstream, realm string) (*Scheduler, store.Store) {
	t.Helper()
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Channels.AutoClaw.UserAPIBase = srv.URL
	cfg.Channels.AutoClaw.OverseasBase = srv.URL
	cfg.Channels.AutoClaw.AppSecret = "s"
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelAutoClaw: 2}, nil)
	drivers := map[string]driver.Driver{
		model.ChannelAutoClaw: mustAutoClawDriver(cfg),
	}
	gw := gateway.New(cfg, p, router.New(cfg), drivers, st)
	creds := &struct {
		AccessToken string `json:"access_token"`
		Region      string `json:"region"`
		DeviceID    string `json:"device_id"`
	}{AccessToken: "tok", Region: realm, DeviceID: "dev"}
	raw, _ := json.Marshal(creds)
	p.Upsert(&model.Account{
		ID: "autoclaw:u1", Channel: model.ChannelAutoClaw, Realm: realm,
		CredsJSON: raw, Nickname: "ac1",
	})
	return New(gw, st, p, log.New(io.Discard, "", 0)), st
}

func mustAutoClawDriver(cfg *config.Config) driver.Driver {
	return autoclaw.New(cfg.Channels.AutoClaw, nil)
}

// TestCheckinAutoClawRaisesLoginThenChecksIn pins the order the upstream
// plugin settled on: the login event first, the check-in second. Reversed, the
// reward is paid a day late (or not at all).
func TestCheckinAutoClawRaisesLoginThenChecksIn(t *testing.T) {
	up := &bp346Upstream{}
	sched, st := bp346Scheduler(t, up, "cn")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	detail, err := sched.RunTask(ctx, TaskCheckinAutoClaw, "")
	if err != nil {
		t.Fatalf("RunTask: %v (%s)", err, detail)
	}
	order := up.orderSnapshot()
	if len(order) < 2 || order[0] != "user-info" || order[1] != "checkin:daily_signin" {
		t.Fatalf("call order = %v, want user-info before the check-in", order)
	}
	// The CN lane has no inspiration task: it must never be attempted there.
	for _, step := range order {
		if step == "inspiration" {
			t.Fatalf("order = %v: the inspiration task exists on the overseas lane only", order)
		}
	}
	if !strings.Contains(detail, "checkin ok") {
		t.Fatalf("detail = %q", detail)
	}
	// A CN run says nothing about the login reward, so nothing is claimed.
	if strings.Contains(detail, "登录奖励") {
		t.Fatalf("detail = %q: a paid login event must not be announced", detail)
	}
	// Nor about the inspiration centre: the task does not exist on this lane,
	// and asking would put a daily "failed" line in the ledger for something
	// that cannot ever succeed.
	if strings.Contains(detail, "灵感中心") {
		t.Fatalf("detail = %q: the inspiration centre must not be attempted on the CN lane", detail)
	}
	runs, _ := st.RecentTaskRuns(10)
	if len(runs) != 1 || runs[0].Status != "ok" {
		t.Fatalf("task runs = %+v", runs)
	}
}

// The overseas lane runs the inspiration task as well, and reports what it
// paid.
func TestCheckinAutoClawOverseasAlsoCompletesInspiration(t *testing.T) {
	up := &bp346Upstream{}
	sched, _ := bp346Scheduler(t, up, "global")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	detail, err := sched.RunTask(ctx, TaskCheckinAutoClaw, "")
	if err != nil {
		t.Fatalf("RunTask: %v (%s)", err, detail)
	}
	order := up.orderSnapshot()
	if len(order) < 3 || order[0] != "user-info" || order[1] != "inspiration" || order[2] != "checkin:daily_signin" {
		t.Fatalf("call order = %v, want user-info, inspiration, check-in", order)
	}
	if !strings.Contains(detail, "灵感中心 +200 分") {
		t.Fatalf("detail = %q, want the inspiration reward stated", detail)
	}
}

// A refused login event or a refused inspiration task must NOT cost the
// account its check-in — they are side quests, the check-in is the errand.
func TestCheckinAutoClawSurvivesSideQuestFailures(t *testing.T) {
	up := &bp346Upstream{failUser: true, failInsp: true}
	sched, st := bp346Scheduler(t, up, "global")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	detail, err := sched.RunTask(ctx, TaskCheckinAutoClaw, "")
	if err != nil {
		t.Fatalf("RunTask: %v (%s) — a side quest must not fail the check-in", err, detail)
	}
	if !strings.Contains(detail, "checkin ok") {
		t.Fatalf("detail = %q, want the check-in to have happened", detail)
	}
	if !strings.Contains(detail, "登录奖励未触发") {
		t.Fatalf("detail = %q, want the refused login event reported", detail)
	}
	if !strings.Contains(detail, "灵感中心未完成") {
		t.Fatalf("detail = %q, want the refused inspiration task reported", detail)
	}
	runs, _ := st.RecentTaskRuns(10)
	if len(runs) != 1 || runs[0].Status != "ok" {
		t.Fatalf("task runs = %+v, want one ok row", runs)
	}
}

// The task ledger row must not grow without bound: an upstream error body is
// trimmed to one readable line, counted in characters so a Chinese refusal is
// not cut mid-character.
func TestCheckinAutoClawNotesStayOneLine(t *testing.T) {
	got := clipNote(strings.Repeat("额度", 200))
	if n := len([]rune(got)); n > 91 || !strings.HasSuffix(got, "…") {
		t.Fatalf("clipNote kept %d chars: %q", n, got)
	}
	if got := clipNote("a\nb"); strings.Contains(got, "\n") {
		t.Fatalf("clipNote kept a newline: %q", got)
	}
}
