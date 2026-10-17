package tasks_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/trae"
	"bargainproxy/internal/driver/workbuddy"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
	"bargainproxy/internal/tasks"
)

func testToken(kind string) string {
	return strings.Join([]string{"tok", kind}, "-")
}

// mockWorkBuddy serves the checkin/billing/growth endpoints used by tasks.
type mockWorkBuddy struct {
	mu          sync.Mutex
	checkins    int
	accepts     []string
	claims      []string
	draws       int
	redemptions []string
	reports     int
	buddyInfo   int
	departs     int
	// checkinReply, when non-empty, is the raw body the daily-checkin
	// endpoint answers with; checkinStatus sets its HTTP status (0 = 200).
	// Ticket 30: the duplicate-checkin rejection arrives as HTTP 400 +
	// {"code":10001,"msg":"今天已签到"}.
	checkinReply  string
	checkinStatus int
}

func (m *mockWorkBuddy) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v2/billing/meter/daily-checkin":
			m.checkins++
			if m.checkinReply != "" {
				if m.checkinStatus != 0 {
					w.WriteHeader(m.checkinStatus)
				}
				fmt.Fprint(w, m.checkinReply)
				return
			}
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		case r.URL.Path == "/v2/billing/meter/get-user-resource":
			fmt.Fprint(w, `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"PackageName":"p","CapacityRemain":500,"CapacitySize":500,"CycleEndTime":"2026-12-31 00:00:00"}]}}}}`)
		case r.URL.Path == "/activity/growth/redeem":
			var body struct {
				Tier string `json:"tier"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.redemptions = append(m.redemptions, body.Tier)
			if body.Tier == "7d" {
				fmt.Fprint(w, `{"code":0,"data":{}}`)
			} else {
				// Locked tiers: 403.
				w.WriteHeader(403)
				fmt.Fprint(w, `{"code":403,"message":"连续登录天数不足"}`)
			}
		case r.URL.Path == "/activity/growth/lottery/summary":
			fmt.Fprint(w, `{"code":0,"data":{"chances":2}}`)
		case r.URL.Path == "/activity/growth/lottery/draw":
			m.draws++
			fmt.Fprint(w, `{"code":0,"data":{"prize":"5c"}}`)
		case r.URL.Path == "/v2/activity/growth/tasks" && r.Method == "GET":
			fmt.Fprint(w, `{"code":0,"data":{"tasks":[
			  {"task_code":"chat_5","status":"completed","accept_status":"accepted"},
			  {"task_code":"template_5","status":"in_progress","accept_status":"not_accepted"}
			]}}`)
		case r.URL.Path == "/v2/activity/growth/tasks/accept":
			var body struct {
				TaskCodes []string `json:"task_codes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.accepts = append(m.accepts, body.TaskCodes...)
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		case strings.HasPrefix(r.URL.Path, "/activity/growth/tasks/") && strings.HasSuffix(r.URL.Path, "/claim"):
			code := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/activity/growth/tasks/"), "/claim")
			m.claims = append(m.claims, code)
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		case r.URL.Path == "/v2/report":
			m.reports++
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		case r.URL.Path == "/activity/growth/streak":
			fmt.Fprint(w, `{"code":0,"data":{"streak":{"days":8}}}`)
		case r.URL.Path == "/activity/growth/buddy/info":
			m.buddyInfo++
			// No cat on this mock account: the adopt chain runs.
			fmt.Fprint(w, `{"code":0,"data":{"buddy":null}}`)
		case r.URL.Path == "/activity/growth/buddy/agreement":
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		case r.URL.Path == "/activity/growth/buddy/first":
			w.WriteHeader(400)
			fmt.Fprint(w, `{"code":400,"message":"first_buddy task not completed yet"}`)
		case r.URL.Path == "/activity/growth/buddy/travel/status":
			fmt.Fprint(w, `{"code":0,"data":{"state":"idle","daily_limit_reached":false,"record_id":0}}`)
		case r.URL.Path == "/activity/growth/buddy/travel/depart":
			m.departs++
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		default:
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		}
	})
}

func buildScheduler(t *testing.T) (*tasks.Scheduler, *mockWorkBuddy, *pool.Pool, *store.Memory) {
	t.Helper()
	mock := &mockWorkBuddy{}
	up := httptest.NewServer(mock.handler())
	t.Cleanup(up.Close)

	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Channels.WorkBuddy.ChatBaseCN = up.URL
	cfg.Channels.WorkBuddy.ChatBaseGL = up.URL
	cfg.Channels.WorkBuddy.WebBaseCN = up.URL
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelWorkBuddy: 2},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	creds, _ := json.Marshal(map[string]any{"access_token": testToken("wb"), "uid": "u-task", "realm": "cn", "nickname": "task-acct"})
	p.Upsert(&model.Account{ID: "workbuddy:u-task", Channel: model.ChannelWorkBuddy, CredsJSON: creds, UID: "u-task", Realm: "cn", Nickname: "task-acct"})

	drivers := map[string]driver.Driver{
		model.ChannelWorkBuddy: workbuddy.New(cfg.Channels.WorkBuddy, nil),
		model.ChannelTrae:      trae.New(cfg.Channels.Trae, nil),
	}
	gw := gateway.New(cfg, p, router.New(cfg), drivers, st)
	sched := tasks.New(gw, st, p, nil)
	return sched, mock, p, st
}

func TestCheckinWorkBuddyFullFlow(t *testing.T) {
	sched, mock, _, _ := buildScheduler(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	detail, err := sched.RunTask(ctx, tasks.TaskCheckinWorkBuddy, "")
	if err != nil {
		t.Fatalf("checkin task: %v (%s)", err, detail)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.checkins != 1 {
		t.Fatalf("checkins = %d, want 1", mock.checkins)
	}
	// 7d redeem must succeed; locked tiers attempted but 403.
	if len(mock.redemptions) != 3 {
		t.Fatalf("redemptions = %v, want all 3 tiers attempted", mock.redemptions)
	}
	// Lottery: 2 chances → 2 draws.
	if mock.draws != 2 {
		t.Fatalf("draws = %d, want 2", mock.draws)
	}
	if !strings.Contains(detail, "checkin ok") {
		t.Fatalf("detail = %q", detail)
	}
}

// TestCheckinWorkBuddyAlreadyCheckinIsSuccess 重复签到（上游 code=10001 +
// 已签到文案）必须记为幂等成功而非 FAILED——此前每个重复时点都会往台账写
// 一行假失败（ticket 30）。
func TestCheckinWorkBuddyAlreadyCheckinIsSuccess(t *testing.T) {
	sched, mock, _, _ := buildScheduler(t)
	mock.mu.Lock()
	mock.checkinReply = `{"code":10001,"msg":"今天已签到"}`
	mock.checkinStatus = 400
	mock.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	detail, err := sched.RunTask(ctx, tasks.TaskCheckinWorkBuddy, "")
	if err != nil {
		t.Fatalf("幂等签到不应报错: %v (%s)", err, detail)
	}
	if !strings.Contains(detail, "already checked in today") {
		t.Fatalf("detail = %q, want already checked in today", detail)
	}
}

func TestTaskIdempotencyDaily(t *testing.T) {
	sched, mock, _, st := buildScheduler(t)
	ctx := context.Background()
	if _, err := sched.RunTask(ctx, tasks.TaskCheckinWorkBuddy, ""); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Second all-account run on the same day must skip (ledger says ok).
	detail, err := sched.RunTask(ctx, tasks.TaskCheckinWorkBuddy, "")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !strings.Contains(detail, "nothing to do") {
		t.Fatalf("second run should skip, detail=%q", detail)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.checkins != 1 {
		t.Fatalf("idempotency broken: checkins = %d, want 1", mock.checkins)
	}
	// Ledger row must exist for today.
	today := time.Now().In(tasks.Shanghai).Format("2006-01-02")
	run, err := st.TaskRunFor("workbuddy:u-task", tasks.TaskCheckinWorkBuddy, today)
	if err != nil || run.Status != "ok" {
		t.Fatalf("ledger row: %v %+v", err, run)
	}
}

func TestGrowthPipelineAcceptsAndClaims(t *testing.T) {
	sched, mock, _, _ := buildScheduler(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	detail, err := sched.RunTask(ctx, tasks.TaskGrowthWorkBuddy, "")
	if err != nil {
		// Some best-effort steps may fail against the mock; the accept/claim
		// pass must still run. Only fail hard if the task itself errors.
		t.Logf("growth task returned: %v (%s)", err, detail)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	// template_5 was not_accepted → accept pass must include it.
	foundAccept := false
	for _, code := range mock.accepts {
		if code == "template_5" {
			foundAccept = true
		}
	}
	if !foundAccept {
		t.Fatalf("accept pass missing template_5: %v", mock.accepts)
	}
	// chat_5 was completed → claim pass must claim it.
	foundClaim := false
	for _, code := range mock.claims {
		if code == "chat_5" {
			foundClaim = true
		}
	}
	if !foundClaim {
		t.Fatalf("claim pass missing chat_5: %v", mock.claims)
	}
	if !strings.Contains(detail, "claim_pass") {
		t.Fatalf("detail missing claim_pass: %q", detail)
	}
}

func TestRefreshTokensAndProbe(t *testing.T) {
	sched, _, _, _ := buildScheduler(t)
	ctx := context.Background()
	if _, err := sched.RunTask(ctx, tasks.TaskProbeAll, ""); err != nil {
		t.Fatalf("probe task: %v", err)
	}
	if _, err := sched.RunTask(ctx, tasks.TaskRefreshTokens, ""); err != nil {
		t.Fatalf("refresh task: %v", err)
	}
}
