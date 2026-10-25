// BP-110: a binding skipped in pre-flight must still be named in the trace.
//
// Field evidence 2026-10-25 14:56: one 409k-token conversation held every
// in-flight slot of its own workbuddy account (single-active mode), so the
// workbuddy binding was skipped as busy, trae refused the size, and lobster
// returned an empty 5xx. The chain recorded `trae→lobster` — workbuddy was
// never mentioned, and "why didn't it use workbuddy? it was fine" became
// unanswerable from the panel. The pool WAS considered; the chain must say so.
package gateway

import (
	"context"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
)

// okDriver answers on any channel with one text event; it exists so the
// request can SUCCEED on the second binding.
type okDriver struct{ calls int }

func (d *okDriver) Channel() string                          { return model.ChannelQoder }
func (d *okDriver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }
func (d *okDriver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.calls++
	if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: "ok"}); err != nil {
		return nil, err
	}
	if err := emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd}); err != nil {
		return nil, err
	}
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "ok"}}},
		StopReason: ir.StopEnd,
	}, nil
}

// newTraceGateway wires workbuddy (max_in_flight = busyMax) ahead of qoder,
// both served by the same driver.
func newTraceGateway(t *testing.T, drv driver.Driver, busyMax int) (*Gateway, *pool.Pool) {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = []config.ModelBinding{
		{Canonical: "m", Channel: model.ChannelWorkBuddy, UpstreamModel: "m"},
		{Canonical: "m", Channel: model.ChannelQoder, UpstreamModel: "m"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second}, map[string]int{
		model.ChannelWorkBuddy: busyMax,
		model.ChannelQoder:     1,
	}, func(acc *model.Account) { _ = st.SaveAccount(acc) })
	p.Upsert(&model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy, CredsJSON: []byte(`{}`)})
	p.Upsert(&model.Account{ID: "qd-1", Channel: model.ChannelQoder, CredsJSON: []byte(`{}`)})
	return New(cfg, p, router.New(cfg), map[string]driver.Driver{
		model.ChannelWorkBuddy: drv,
		model.ChannelQoder:     drv,
	}, st), p
}

func traceReq() *ir.ChatRequest {
	return &ir.ChatRequest{
		Model:  "m",
		Stream: true,
		Messages: []ir.Message{{
			Role:    ir.RoleUser,
			Content: []ir.ContentPart{{Kind: "text", Text: "hi"}},
		}},
	}
}

// TestSkippedBusyBindingIsNamedInTheTrace: workbuddy is full (its one slot is
// held by a lease), the request succeeds on qoder, and the chain must read
// workbuddy→qoder — the busy binding is part of the story even though no
// upstream ever saw the request.
func TestSkippedBusyBindingIsNamedInTheTrace(t *testing.T) {
	drv := &okDriver{}
	g, p := newTraceGateway(t, drv, 1)

	lease, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "")
	if err != nil {
		t.Fatalf("acquire the only workbuddy slot: %v", err)
	}
	defer lease.Release()

	tr := &Trace{}
	res, err := g.Execute(WithTrace(context.Background(), tr), traceReq(), func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("qoder should have served the request: %v", err)
	}
	if res == nil || res.Channel != model.ChannelQoder {
		t.Fatalf("expected qoder to serve, got %+v", res)
	}
	if len(tr.Channels) < 2 || tr.Channels[0] != model.ChannelWorkBuddy || tr.Channels[1] != model.ChannelQoder {
		t.Fatalf("attempt chain = %v, want [%s %s]: a busy binding skipped in pre-flight must still be named",
			tr.Channels, model.ChannelWorkBuddy, model.ChannelQoder)
	}
	if drv.calls != 1 {
		t.Fatalf("driver called %d times, want 1 (the busy binding must be skipped, not dispatched)", drv.calls)
	}
}
