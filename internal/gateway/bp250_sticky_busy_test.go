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
)

// BP-250: a conversation already bound to a pool must not be skipped when that
// pool is busy.
//
// The busY pre-check runs BEFORE Acquire, while the sticky binding lives INSIDE
// Acquire — so an in-flight request used to make every other request of the same
// conversation skip the pool, and a skipped binding can neither be served nor
// wait. Field 2026-11-01: an agent session sent a request every ~5s while each
// took 15s+, so zcode/autoclaw/workbuddy were permanently busy; nine consecutive
// turns cascaded zcode→autoclaw→workbuddy→qoder and deepseek fell to lobster.
type bp250SlowDriver struct {
	mu    sync.Mutex
	calls int
	delay time.Duration
}

func (d *bp250SlowDriver) Channel() string                          { return model.ChannelZCode }
func (d *bp250SlowDriver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }
func (d *bp250SlowDriver) callsN() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}
func (d *bp250SlowDriver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	_ = emit(ir.StreamEvent{Kind: ir.EvText, Delta: "ok"})
	_ = emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "ok"}}},
		StopReason: ir.StopEnd,
	}, nil
}

type bp250FastDriver struct {
	mu    sync.Mutex
	calls int
}

func (d *bp250FastDriver) Channel() string                          { return model.ChannelQoder }
func (d *bp250FastDriver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }
func (d *bp250FastDriver) callsN() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}
func (d *bp250FastDriver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
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

func TestBoundSessionKeepsItsBusyPool(t *testing.T) {
	slow := &bp250SlowDriver{delay: 400 * time.Millisecond}
	fast := &bp250FastDriver{}
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	// No first-byte window: the question here is routing, not timing.
	cfg.FirstByteWindow = config.Duration(0)
	cfg.Models = []config.ModelBinding{
		{Canonical: "m", Channel: model.ChannelZCode, UpstreamModel: "m", Priority: 1},
		{Canonical: "m", Channel: model.ChannelQoder, UpstreamModel: "m", Priority: 2},
	}
	st := store.NewMemory()
	// SingleActivePerChannel is ON: the pool-busy pre-check only exists under
	// this rule, so without it the test would not exercise the code path.
	p := pool.New(pool.Params{MaxQueueWait: 5 * time.Second, SingleActivePerChannel: true},
		map[string]int{model.ChannelZCode: 1, model.ChannelQoder: 1},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	p.Upsert(&model.Account{ID: "zc-1", Channel: model.ChannelZCode, CredsJSON: []byte(`{}`)})
	p.Upsert(&model.Account{ID: "qd-1", Channel: model.ChannelQoder, CredsJSON: []byte(`{}`)})
	g := New(cfg, p, router.New(cfg), map[string]driver.Driver{
		model.ChannelZCode: slow, model.ChannelQoder: fast,
	}, st)

	mkReq := func() *ir.ChatRequest {
		return &ir.ChatRequest{
			Model:      "m",
			Stream:     true,
			MaxTokens:  100,
			SessionKey: "one-conversation",
			Messages:   []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		}
	}

	// First turn of the conversation: binds it to zcode and stays in flight.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := g.Execute(context.Background(), mkReq(), func(ir.StreamEvent) error { return nil }); err != nil {
			t.Errorf("first turn: %v", err)
		}
	}()
	// Wait until zcode is actually in flight, so the second turn meets a BUSY pool.
	deadline := time.Now().Add(2 * time.Second)
	for slow.callsN() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if slow.callsN() == 0 {
		t.Fatal("the first turn never reached zcode")
	}

	// Second turn of the SAME conversation, while zcode is busy. It must stay on
	// its bound pool instead of skipping to qoder.
	if _, err := g.Execute(context.Background(), mkReq(), func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	<-done

	if slow.callsN() < 2 {
		t.Errorf("zcode handled %d calls, want 2 — the bound conversation was turned "+
			"away by the pool-busy check and drifted to another pool", slow.callsN())
	}
	if fast.callsN() != 0 {
		t.Errorf("qoder handled %d calls, want 0 — the conversation drifted off its "+
			"bound pool while that pool was merely busy", fast.callsN())
	}
}

// An UNBOUND conversation must still skip a busy pool: that is the concurrency
// protection (never start a second account on a pool), and BP-250 must not
// weaken it.
func TestUnboundSessionStillSkipsBusyPool(t *testing.T) {
	slow := &bp250SlowDriver{delay: 300 * time.Millisecond}
	fast := &bp250FastDriver{}
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.FirstByteWindow = config.Duration(0)
	cfg.Models = []config.ModelBinding{
		{Canonical: "m", Channel: model.ChannelZCode, UpstreamModel: "m", Priority: 1},
		{Canonical: "m", Channel: model.ChannelQoder, UpstreamModel: "m", Priority: 2},
	}
	st := store.NewMemory()
	// SingleActivePerChannel is ON: the pool-busy pre-check only exists under
	// this rule, so without it the test would not exercise the code path.
	p := pool.New(pool.Params{MaxQueueWait: 5 * time.Second, SingleActivePerChannel: true},
		map[string]int{model.ChannelZCode: 1, model.ChannelQoder: 1},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	p.Upsert(&model.Account{ID: "zc-2", Channel: model.ChannelZCode, CredsJSON: []byte(`{}`)})
	p.Upsert(&model.Account{ID: "qd-2", Channel: model.ChannelQoder, CredsJSON: []byte(`{}`)})
	g := New(cfg, p, router.New(cfg), map[string]driver.Driver{
		model.ChannelZCode: slow, model.ChannelQoder: fast,
	}, st)

	mkReq := func(key string) *ir.ChatRequest {
		return &ir.ChatRequest{
			Model: "m", Stream: true, MaxTokens: 100, SessionKey: key,
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = g.Execute(context.Background(), mkReq("conv-a"), func(ir.StreamEvent) error { return nil })
	}()
	deadline := time.Now().Add(2 * time.Second)
	for slow.callsN() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// A DIFFERENT conversation arrives while zcode is busy: it must cascade.
	if _, err := g.Execute(context.Background(), mkReq("conv-b"), func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("second conversation: %v", err)
	}
	<-done
	if slow.callsN() != 1 {
		t.Errorf("zcode handled %d calls, want 1 — an unbound conversation must not "+
			"be served by a pool that is already interacting", slow.callsN())
	}
	if fast.callsN() != 1 {
		t.Errorf("qoder handled %d calls, want 1 — the unbound conversation should "+
			"have cascaded", fast.callsN())
	}
}
