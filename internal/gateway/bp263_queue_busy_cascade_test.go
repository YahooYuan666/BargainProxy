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
	"bargainproxy/internal/uperr"
)

type bp263BusyDriver struct {
	calls int
}

func (d *bp263BusyDriver) Channel() string                          { return model.ChannelWorkBuddy }
func (d *bp263BusyDriver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }
func (d *bp263BusyDriver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.calls++
	e := uperr.New(uperr.ClassUpstreamBusy, 429, "workbuddy: upstream queue busy: queue.waiting.title")
	e.ResetAt = time.Now().Add(40 * time.Second)
	return nil, e
}

type bp263SucceedDriver struct {
	calls int
}

func (d *bp263SucceedDriver) Channel() string                          { return model.ChannelQoder }
func (d *bp263SucceedDriver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }
func (d *bp263SucceedDriver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.calls++
	_ = emit(ir.StreamEvent{Kind: ir.EvText, Delta: "cascaded to qoder successfully"})
	_ = emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "cascaded to qoder successfully"}}},
		StopReason: ir.StopEnd,
	}, nil
}

// BP-263: when a channel answers ClassUpstreamBusy with a deep queue wait (e.g. 6020 retry_after=40s),
// it must NOT rotate accounts of the same pool, and must cascade to the next channel in the route.
func TestBP263QueueBusyCascadesToNextChannel(t *testing.T) {
	wbDrv := &bp263BusyDriver{}
	qdDrv := &bp263SucceedDriver{}

	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.FirstByteWindow = config.Duration(0)
	cfg.Models = []config.ModelBinding{
		{Canonical: "m", Channel: model.ChannelWorkBuddy, UpstreamModel: "m", Priority: 1},
		{Canonical: "m", Channel: model.ChannelQoder, UpstreamModel: "m", Priority: 2},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 5 * time.Second, SingleActivePerChannel: false},
		map[string]int{model.ChannelWorkBuddy: 2, model.ChannelQoder: 1},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })

	// Two accounts in workbuddy pool to verify no same-pool rotation
	p.Upsert(&model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy, CredsJSON: []byte(`{}`)})
	p.Upsert(&model.Account{ID: "wb-2", Channel: model.ChannelWorkBuddy, CredsJSON: []byte(`{}`)})
	p.Upsert(&model.Account{ID: "qd-1", Channel: model.ChannelQoder, CredsJSON: []byte(`{}`)})

	gw := New(cfg, p, router.New(cfg), map[string]driver.Driver{
		model.ChannelWorkBuddy: wbDrv,
		model.ChannelQoder:     qdDrv,
	}, st)

	req := &ir.ChatRequest{
		Model: "m",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
		},
		Stream: true,
	}

	res, err := gw.Execute(context.Background(), req, func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("expected cascade to succeed, got error: %v", err)
	}
	if res.Channel != model.ChannelQoder {
		t.Fatalf("expected response from channel %s, got %s", model.ChannelQoder, res.Channel)
	}
	// Workbuddy should only be called once: no inner retry and no same-pool rotation!
	if wbDrv.calls != 1 {
		t.Fatalf("expected workbuddy to be called exactly 1 time, got %d", wbDrv.calls)
	}
	if qdDrv.calls != 1 {
		t.Fatalf("expected qoder to be called 1 time, got %d", qdDrv.calls)
	}
}
