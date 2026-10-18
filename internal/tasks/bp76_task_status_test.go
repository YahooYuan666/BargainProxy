package tasks_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func bp76Scheduler(t *testing.T, handler http.Handler) (*tasks.Scheduler, *store.Memory) {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Channels.WorkBuddy.ChatBaseCN = up.URL
	cfg.Channels.WorkBuddy.ChatBaseGL = up.URL
	cfg.Channels.WorkBuddy.WebBaseCN = up.URL
	st := store.NewMemory()
	p := pool.New(pool.Params{}, map[string]int{model.ChannelWorkBuddy: 2}, func(*model.Account) {})
	creds, _ := json.Marshal(map[string]any{"access_token": "audit-access", "refresh_token": "audit-refresh", "uid": "audit-uid", "realm": "cn"})
	p.Upsert(&model.Account{ID: "workbuddy:audit-uid", Channel: model.ChannelWorkBuddy, Realm: "cn", UID: "audit-uid", CredsJSON: creds})
	drivers := map[string]driver.Driver{model.ChannelWorkBuddy: workbuddy.New(cfg.Channels.WorkBuddy, nil)}
	gw := gateway.New(cfg, p, router.New(cfg), drivers, st)
	return tasks.New(gw, st, p, nil), st
}

func TestBP76TravelPatrolCanRunAgainSameDay(t *testing.T) {
	var statusHits atomic.Int32
	var claimHits atomic.Int32
	s, _ := bp76Scheduler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/activity/growth/buddy/info":
			fmt.Fprint(w, `{"code":0,"data":{"buddy":{"id":1}}}`)
		case "/activity/growth/buddy/travel/status":
			if statusHits.Add(1) == 1 {
				fmt.Fprint(w, `{"code":0,"data":{"state":"idle","daily_limit_reached":false}}`)
			} else {
				fmt.Fprint(w, `{"code":0,"data":{"state":"arrived","record_id":7}}`)
			}
		case "/activity/growth/buddy/travel/claim":
			claimHits.Add(1)
			fmt.Fprint(w, `{"code":0,"data":{"reward_credit":25}}`)
		default:
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		}
	}))
	if note, err := s.RunTask(context.Background(), tasks.TaskTravelWorkBuddy, ""); err != nil {
		t.Fatalf("first patrol: %s %v", note, err)
	}
	note, err := s.RunTask(context.Background(), tasks.TaskTravelWorkBuddy, "")
	if err != nil {
		t.Fatalf("second patrol: %s %v", note, err)
	}
	if statusHits.Load() != 2 || claimHits.Load() != 1 {
		t.Fatalf("second same-day patrol skipped by daily ledger: status checks=%d claims=%d note=%q; want recheck and claim after arrival", statusHits.Load(), claimHits.Load(), note)
	}
}

func TestBP76GrowthAllFailuresCannotRecordOK(t *testing.T) {
	s, st := bp76Scheduler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"code":500,"message":"audit upstream unavailable"}`)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	note, err := s.RunTask(ctx, tasks.TaskGrowthWorkBuddy, "")
	run, lookupErr := st.TaskRunFor("workbuddy:audit-uid", tasks.TaskGrowthWorkBuddy, time.Now().In(tasks.Shanghai).Format("2006-01-02"))
	if lookupErr != nil {
		t.Fatal(lookupErr)
	}
	if err == nil || run.Status == "ok" {
		t.Fatalf("all-failed growth was recorded successful: err=%v status=%q note=%q; want failure/partial status", err, run.Status, note)
	}
}

func TestBP76TravelClaimErrorCannotRecordOK(t *testing.T) {
	s, st := bp76Scheduler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/buddy/info"):
			fmt.Fprint(w, `{"code":0,"data":{"buddy":{"id":1}}}`)
		case strings.HasSuffix(r.URL.Path, "/travel/status"):
			fmt.Fprint(w, `{"code":0,"data":{"state":"arrived","record_id":7}}`)
		case strings.HasSuffix(r.URL.Path, "/travel/claim"):
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"code":500,"message":"audit claim unavailable"}`)
		default:
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		}
	}))
	note, err := s.RunTask(context.Background(), tasks.TaskTravelWorkBuddy, "")
	run, lookupErr := st.TaskRunFor("workbuddy:audit-uid", tasks.TaskTravelWorkBuddy, time.Now().In(tasks.Shanghai).Format("2006-01-02"))
	if lookupErr != nil {
		t.Fatal(lookupErr)
	}
	if err == nil || run.Status == "ok" {
		t.Fatalf("failed travel claim was recorded successful: err=%v status=%q note=%q; want failure and retry eligibility", err, run.Status, note)
	}
}
