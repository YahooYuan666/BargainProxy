package server

import (
	"testing"
	"time"

	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
)

// BP-287: the request log must record the thinking gear that actually reached
// upstream.
//
// The gear was invisible for three weeks. The ingress stopped reading the field
// clients send (output_config.effort) and silently fell back to "high"; with
// nothing in the log there was no way to tell the client's choice from the
// gateway's rewrite, and output-length measurements could not see it either
// (run-to-run noise is wider than the gear difference).
//
// The value recorded is the one the GATEWAY sent — after any downgrade it
// applied — not the one the caller asked for. That distinction is the whole
// point: a row showing "high" when the user picked "max" is exactly the symptom
// this column exists to make visible.
func TestLogRowRecordsEffortSent(t *testing.T) {
	row := &model.RequestLog{}
	logResult(row, &gateway.Result{
		Channel: model.ChannelWorkBuddy,
		Account: &model.Account{ID: "wb-1"},
		Effort:  "max",
	}, time.Now())
	if row.Effort != "max" {
		t.Fatalf("Effort = %q, want max", row.Effort)
	}
}

// A request that named no gear records empty, not a fabricated default. The
// panel renders empty as "—" (this request asked for none), which is a
// different fact from "high was applied".
func TestLogRowRecordsEmptyEffortAsEmpty(t *testing.T) {
	row := &model.RequestLog{}
	logResult(row, &gateway.Result{
		Channel: model.ChannelWorkBuddy,
		Account: &model.Account{ID: "wb-1"},
	}, time.Now())
	if row.Effort != "" {
		t.Fatalf("Effort = %q, want empty (no gear was named)", row.Effort)
	}
}
