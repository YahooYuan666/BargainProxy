package gateway

import (
	"context"
	"sync"
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

// BP-217: the first-byte window fires on OUR timer while the client is still
// connected and the upstream has sent zero bytes. One such stall is a slow
// response; two on the same channel+model are a silently hung upstream — and
// before BP-217 every later request re-paid the full window on the dead leg
// (field 2026-10-30 22:43–23:03: 17 requests waited 60s each on a zero-byte
// workbuddy before cascading to trae). After the second stall the pool pauses
// the pair, and the next request cascades without touching the hung channel.
type bp217HungDriver struct {
	mu    sync.Mutex
	calls int
}

func (d *bp217HungDriver) Channel() string                          { return model.ChannelWorkBuddy }
func (d *bp217HungDriver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }
func (d *bp217HungDriver) callsN() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}
func (d *bp217HungDriver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	// Zero bytes for a whole window, then die with the attempt context —
	// exactly how the real drivers report our timer's cancel (class=canceled).
	<-ctx.Done()
	return nil, uperr.New(uperr.ClassCanceled, 0, "workbuddy: %v", ctx.Err())
}

type bp217OkDriver struct {
	mu    sync.Mutex
	calls int
}

func (d *bp217OkDriver) Channel() string                          { return model.ChannelAutoClaw }
func (d *bp217OkDriver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }
func (d *bp217OkDriver) callsN() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}
func (d *bp217OkDriver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	_ = emit(ir.StreamEvent{Kind: ir.EvText, Delta: "ok"})
	_ = emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "ok"}}},
		StopReason: ir.StopEnd,
	}, nil
}

func TestBP217SecondWindowExpirySkipsTheHungChannel(t *testing.T) {
	wb := &bp217HungDriver{}
	ac := &bp217OkDriver{}
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = []config.ModelBinding{
		{Canonical: "m", Channel: model.ChannelWorkBuddy, UpstreamModel: "m", Priority: 1},
		{Canonical: "m", Channel: model.ChannelAutoClaw, UpstreamModel: "m", Priority: 2},
	}
	// A tiny per-channel ceiling makes the window expire in milliseconds.
	cfg.ChannelFirstByteWindows = map[string]config.Duration{
		model.ChannelWorkBuddy: config.Duration(150 * time.Millisecond),
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second},
		map[string]int{model.ChannelWorkBuddy: 1, model.ChannelAutoClaw: 1},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	p.Upsert(&model.Account{ID: "wb-acct", Channel: model.ChannelWorkBuddy, CredsJSON: []byte(`{}`)})
	p.Upsert(&model.Account{ID: "ac-acct", Channel: model.ChannelAutoClaw, CredsJSON: []byte(`{}`)})
	g := New(cfg, p, router.New(cfg), map[string]driver.Driver{model.ChannelWorkBuddy: wb, model.ChannelAutoClaw: ac}, st)

	req := &ir.ChatRequest{
		Model:     "m",
		Stream:    true,
		MaxTokens: 100,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
	run := func() {
		t.Helper()
		res, err := g.Execute(context.Background(), req, func(ev ir.StreamEvent) error { return nil })
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if res == nil || res.Channel != model.ChannelAutoClaw {
			t.Fatalf("request must land on the healthy channel, got %+v", res)
		}
	}

	// Request 1: one stall (the window fired once) — no pause yet.
	run()
	if _, ok := p.ChannelBusyUntil(model.ChannelWorkBuddy, "m", time.Now()); ok {
		t.Fatal("ONE window expiry paused the channel — a single slow response must never pause anything")
	}
	// Request 2: second stall — the pair trips.
	run()
	if _, ok := p.ChannelBusyUntil(model.ChannelWorkBuddy, "m", time.Now()); !ok {
		t.Fatal("second window expiry did not pause the pair — later requests would re-pay the window on the hung leg")
	}
	// Request 3: workbuddy+m is paused, so the request cascades WITHOUT
	// another stall on the hung driver. This is the whole point: the dead leg
	// is skipped, not re-paid.
	run()
	if n := wb.callsN(); n != 2 {
		t.Fatalf("hung driver called %d times (want 2) — the pause did not stop the re-probe storm", n)
	}
	if n := ac.callsN(); n != 3 {
		t.Fatalf("healthy driver served %d requests (want 3)", n)
	}
}
