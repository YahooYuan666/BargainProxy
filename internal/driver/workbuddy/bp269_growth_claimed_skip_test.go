package workbuddy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// BP-269 (misakano 5866d6fe01, issue #149): the stage-0 snapshot already knows
// which tasks are claimed (accept_status=claimed) — running every report chain
// for them anyway is ~30 dead upstream calls per account per day. Claimed
// tasks are skipped from the snapshot; the snapshot-vs-per-item discipline of
// BP-228 (one list fetch, fail-open) is unchanged.
func TestBP269GrowthPipelineSkipsClaimedFromSnapshot(t *testing.T) {
	var calls int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == growthTasksPath {
			fmt.Fprint(w, `{"code":0,"data":{"tasks":[`+
				`{"task_code":"chat_5","accept_status":"claimed","current":5,"target":5},`+
				`{"task_code":"template_5","accept_status":"claimed","current":5,"target":5}]}}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":400,"msg":"no"}`)
	}))
	defer up.Close()

	d := testDriver(up.URL)
	results, _ := d.RunGrowthPipeline(context.Background(), testAccount())
	byStep := map[string]growthStepResult{}
	for _, r := range results {
		byStep[r.Step] = r
	}
	for _, step := range []string{"chat_5", "template_5"} {
		r := byStep[step]
		if !strings.Contains(r.Note, "claimed") {
			t.Fatalf("claimed task %s must be skipped with a claimed note, got %+v", step, r)
		}
		if !r.OK {
			t.Fatalf("a claimed skip is not a failure: %s = %+v", step, r)
		}
	}
	// Without the snapshot skip, chat_5 alone would fire 5 report calls plus
	// template_5's 5 chains. With it: the two list fetches (snapshot +
	// claim_pass) and nothing else.
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2 (two list fetches, zero dead reports)", calls)
	}
}

// BP-269: every pipeline step is spaced by growthStepGap — query-type and
// skip-type steps used to run back-to-back, the same burst shape that earned
// upstream #149 its 429. The gap is a package var so tests can zero it.
func TestBP269GrowthPipelineSpacesSteps(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"code":503,"msg":"down"}`)
	}))
	defer up.Close()

	old := growthStepGap
	t.Cleanup(func() { growthStepGap = old })

	growthStepGap = 0
	d := testDriver(up.URL)
	start := time.Now()
	d.RunGrowthPipeline(context.Background(), testAccount())
	baseline := time.Since(start)
	if baseline > time.Second {
		t.Fatalf("gap=0 baseline took %s; the fake upstream should answer in ms", baseline)
	}

	growthStepGap = 100 * time.Millisecond
	start = time.Now()
	results, _ := d.RunGrowthPipeline(context.Background(), testAccount())
	if got := time.Since(start); got < 8*growthStepGap {
		t.Fatalf("elapsed %s with gap=%s over %d steps: steps are not being spaced", got.Round(time.Millisecond), growthStepGap, len(results))
	}
}
