package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Upstream switched `progress` from a flat int to {"current":N,"target":M}
// and stopped sending `status` (verified 2026-10-18). A plain int field made
// the whole task list fail to unmarshal, which is what broke the night-owl
// task with "cannot unmarshal object into Go struct field .tasks.progress of
// type int".
func TestGrowthTasksParsesProgressObjectShape(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/activity/growth/tasks" {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"code":0,"data":{"tasks":[
		  {"task_code":"black_cat","accept_status":"not_accepted","progress":{"current":1,"target":3}},
		  {"task_code":"chat_5","accept_status":"claimed","progress":{"current":5,"target":5}}
		]}}`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	acc := testAccount()
	tasks, err := d.GrowthTasks(context.Background(), acc)
	if err != nil {
		t.Fatalf("GrowthTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	night := tasks[0]
	if night.Progress != 1 || night.Target != 3 {
		t.Fatalf("black_cat progress = %d/%d, want 1/3", night.Progress, night.Target)
	}
	if night.Done() {
		t.Fatalf("black_cat at 1/3 must not report done")
	}
	if night.Claimed() {
		t.Fatalf("black_cat not_accepted must not report claimed")
	}
	if !tasks[1].Done() || !tasks[1].Claimed() {
		t.Fatalf("chat_5 at 5/5 claimed: Done=%v Claimed=%v, want true/true",
			tasks[1].Done(), tasks[1].Claimed())
	}
}

// The older flat shape (current/target at the top level) must keep working so
// a partial upstream rollout cannot break the list again.
func TestGrowthTasksParsesFlatShape(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{"tasks":[
		  {"task_code":"black_cat","accept_status":"accepted","current":2,"target":3},
		  {"task_code":"legacy_progress","progress":4,"target":4}
		]}}`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	tasks, err := d.GrowthTasks(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("GrowthTasks: %v", err)
	}
	if tasks[0].Progress != 2 || tasks[0].Target != 3 {
		t.Fatalf("flat current/target = %d/%d, want 2/3", tasks[0].Progress, tasks[0].Target)
	}
	if tasks[1].Progress != 4 || !tasks[1].Done() {
		t.Fatalf("bare numeric progress = %d (done=%v), want 4/true",
			tasks[1].Progress, tasks[1].Done())
	}
}

func TestParseGrowthTaskProgressShapes(t *testing.T) {
	cases := []struct {
		name         string
		raw          string
		flatCur      int
		flatTarget   int
		wantProgress int
		wantTarget   int
	}{
		{"object", `{"current":1,"target":3}`, 0, 0, 1, 3},
		{"object partial", `{"current":2}`, 0, 7, 2, 7},
		{"bare number", `4`, 0, 5, 4, 5},
		{"null", `null`, 2, 5, 2, 5},
		{"empty", ``, 1, 2, 1, 2},
		{"flat wins when absent", ``, 0, 0, 0, 0},
	}
	for _, c := range cases {
		var raw json.RawMessage
		if c.raw != "" {
			raw = json.RawMessage(c.raw)
		}
		p, tg := parseGrowthTaskProgress(raw, c.flatCur, c.flatTarget)
		if p != c.wantProgress || tg != c.wantTarget {
			t.Fatalf("%s: got %d/%d, want %d/%d", c.name, p, tg, c.wantProgress, c.wantTarget)
		}
	}
}

// The edge (APISIX) occasionally answers a perfectly valid token with an HTML
// 401 page; the same request succeeds on retry. That must not fail the task.
func TestGrowthCallRetriesEdgeJitter401(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, "<html><head><title>401 Authorization Required</title></head><body>APISIX</body></html>")
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{"streak":{"days":7}}}`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	days, err := d.StreakStatus(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("StreakStatus after edge 401: %v", err)
	}
	if days != 7 {
		t.Fatalf("streak days = %d, want 7", days)
	}
	if hits != 2 {
		t.Fatalf("upstream hits = %d, want 2 (one 401 + one retry)", hits)
	}
}

// A JSON 401 (business/auth rejection) must NOT be retried as an edge jitter:
// it is a real credential problem, so it has to reach the caller as auth-dead
// and trigger the refresh path instead.
func TestGrowthCallDoesNotRetryReal401(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"code":12153,"msg":"12153:offline session invalid"}`)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL)
	_, err := d.StreakStatus(context.Background(), testAccount())
	if err == nil {
		t.Fatal("expected auth error")
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (no retry for a real 401)", hits)
	}
	if !IsAuthDead(err) {
		t.Fatalf("IsAuthDead = false for a JSON 401: %v", err)
	}
}
