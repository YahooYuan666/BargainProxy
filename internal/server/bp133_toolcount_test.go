package server

import (
	"testing"
	"time"

	"bargainproxy/internal/gateway"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

// BP-133: the log row records how many tools the request declared vs how many
// the answer actually called — the "said stop but never called a tool" rate
// becomes observable (misakano ce6e753, issue #44).
func TestLogResultRecordsToolCounts(t *testing.T) {
	row := &model.RequestLog{}
	result := &gateway.Result{
		Channel:       model.ChannelWorkBuddy,
		Account:       &model.Account{ID: "wb-1"},
		ToolsDeclared: 3,
		Response: &ir.ChatResponse{
			Message: ir.Message{ToolCalls: []ir.ToolCall{{ID: "t1", Name: "a"}, {ID: "t2", Name: "b"}}},
		},
	}
	logResult(row, result, time.Now())
	if row.ToolsDeclared != 3 || row.ToolCalls != 2 {
		t.Fatalf("tools declared=%d calls=%d, want 3/2", row.ToolsDeclared, row.ToolCalls)
	}
	// A failed leg (no response) still records what was declared.
	row2 := &model.RequestLog{}
	logResult(row2, &gateway.Result{Channel: model.ChannelTrae, Account: &model.Account{ID: "tr-1"}, ToolsDeclared: 5}, time.Now())
	if row2.ToolsDeclared != 5 || row2.ToolCalls != 0 {
		t.Fatalf("failed leg declared=%d calls=%d, want 5/0", row2.ToolsDeclared, row2.ToolCalls)
	}
}
