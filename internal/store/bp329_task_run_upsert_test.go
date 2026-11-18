// BP-329: task_runs is keyed by (account_id, task_type, run_date), so the
// SECOND run of the same task on the same day collides with the first. The
// upsert used to refresh status/detail/credits/finished_at but leave
// started_at at the first attempt, which made the panel render
// "00:15 started / 04:37 finished / 262 minutes" as one run — two different
// executions spliced together (field evidence 2026-11-18, xchu2693 activity).
//
// The in-memory store overwrote the whole struct, so the two backends
// disagreed: the bug only existed on Postgres (the only backend production
// uses). The Postgres half therefore needs a real database — a test that only
// exercises the memory map cannot see a missing SET clause.
package store_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"bargainproxy/internal/cryptox"
	"bargainproxy/internal/model"
	"bargainproxy/internal/store"
)

const (
	bp329Task = "activity_report_workbuddy"
	bp329Date = "2026-11-18"
)

// bp329AccountID is unique per test process run. A fixed id would make the
// test lie: the row left behind by a previous run already carries the SECOND
// attempt's started_at, so a database missing the SET clause would still read
// back the right value and the mutation would never turn red.
func bp329AccountID(t *testing.T) string {
	t.Helper()
	return "workbuddy:bp329-upsert-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// bp329First is the failed 00:15 attempt; bp329Second is the 04:37 rerun that
// succeeded. Their gap is the nonsense duration the panel showed.
func bp329First() time.Time  { return time.Date(2026, 11, 18, 0, 15, 52, 0, time.Local) }
func bp329Second() time.Time { return time.Date(2026, 11, 18, 4, 37, 38, 0, time.Local) }

func bp329Run(account string, started, finished time.Time, status, detail string) *model.TaskRun {
	return &model.TaskRun{
		AccountID:  account,
		TaskType:   bp329Task,
		RunDate:    bp329Date,
		Status:     status,
		Detail:     detail,
		StartedAt:  started,
		FinishedAt: finished,
	}
}

// bp329UpsertTwice writes the failed attempt, then the successful rerun — the
// exact sequence that produced the spliced row in production.
func bp329UpsertTwice(t *testing.T, account string, upsert func(*model.TaskRun) error) *model.TaskRun {
	t.Helper()
	first := bp329Run(account, bp329First(), bp329First().Add(5*time.Second), "failed", "activity report failed: EOF")
	second := bp329Run(account, bp329Second(), bp329Second().Add(3*time.Second), "ok", "activity reported, streak 1d")
	if err := upsert(first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := upsert(second); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	return second
}

// bp329AssertRewriteFor requires every field the row may change to be the
// second attempt's — started_at included.
func bp329AssertRewriteFor(t *testing.T, got, want *model.TaskRun) {
	t.Helper()
	if got.Status != want.Status {
		t.Fatalf("status = %q, want the second attempt's %q", got.Status, want.Status)
	}
	if got.Detail != want.Detail {
		t.Fatalf("detail = %q, want the second attempt's %q", got.Detail, want.Detail)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Fatalf("started_at = %s, want the second attempt's %s — the row still shows when the FAILED attempt started, so the panel reports a %v run that never happened (BP-329)",
			got.StartedAt.Format(time.RFC3339), want.StartedAt.Format(time.RFC3339),
			got.FinishedAt.Sub(got.StartedAt).Round(time.Minute))
	}
	if !got.FinishedAt.Equal(want.FinishedAt) {
		t.Fatalf("finished_at = %s, want the second attempt's %s",
			got.FinishedAt.Format(time.RFC3339), want.FinishedAt.Format(time.RFC3339))
	}
}

// TestBP329MemoryUpsertRewritesStartedAt pins the backend that was already
// correct, so a future optimisation cannot make the pair diverge again.
func TestBP329MemoryUpsertRewritesStartedAt(t *testing.T) {
	st := store.NewMemory()
	account := bp329AccountID(t)
	want := bp329UpsertTwice(t, account, st.UpsertTaskRun)
	got, err := st.TaskRunFor(account, bp329Task, bp329Date)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	bp329AssertRewriteFor(t, got, want)
}

// TestBP329PostgresUpsertRewritesStartedAt runs the same sequence against a
// real database. Set BP_TEST_POSTGRES_DSN to enable it, e.g.
//
//	BP_TEST_POSTGRES_DSN='postgres://bargainproxy@127.0.0.1:55432/bargainproxy?sslmode=disable' \
//	  go test ./internal/store -run BP329
//
// The rows live under a dedicated account id, so pointing it at a development
// database cannot disturb real ledgers.
func TestBP329PostgresUpsertRewritesStartedAt(t *testing.T) {
	dsn := os.Getenv("BP_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BP_TEST_POSTGRES_DSN to run the Postgres half of BP-329 (the memory map cannot see a missing SET clause)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := store.EnsureDatabase(ctx, dsn); err != nil {
		t.Fatalf("ensure database: %v", err)
	}
	box, err := cryptox.New(make([]byte, 32))
	if err != nil {
		t.Fatalf("crypto box: %v", err)
	}
	st, err := store.OpenPostgres(ctx, dsn, box)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer st.Close()

	want := bp329UpsertTwice(t, bp329AccountID(t), st.UpsertTaskRun)
	account := want.AccountID
	got, err := st.TaskRunFor(account, bp329Task, bp329Date)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	bp329AssertRewriteFor(t, got, want)
}
