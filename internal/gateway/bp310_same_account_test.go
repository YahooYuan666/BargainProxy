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

// BP-310: a rotatable failure used to re-enter Acquire and, in a single-account
// pool (or when stickiness/recency handed the same account back), dispatch the
// SAME account again — up to three identical transport timeouts per leg. The
// rotation must mean "try a DIFFERENT account"; when none is left, the binding
// returns its last failure and the outer loop cascades.

type bp310Driver struct {
	channel string
	calls   map[string]int
	fail    map[string]bool
}

func (d *bp310Driver) Channel() string                          { return d.channel }
func (d *bp310Driver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }

func (d *bp310Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.calls[call.Account.ID]++
	if d.fail[call.Account.ID] {
		return nil, uperr.New(uperr.ClassTransport, 0, "%s: no answer", call.Account.ID)
	}
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

// newBP310Gateway wires ONE binding (channel qoder = plain LRU selection) with
// the given accounts, so which account is picked next is deterministic.
func newBP310Gateway(t *testing.T, accounts []string, fail map[string]bool) (*Gateway, *bp310Driver) {
	t.Helper()
	channel := model.ChannelQoder
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = []config.ModelBinding{{Canonical: "m", Channel: channel, UpstreamModel: "m"}}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second}, nil,
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	for _, id := range accounts {
		p.Upsert(&model.Account{ID: id, Channel: channel, CredsJSON: []byte(`{}`)})
	}
	drv := &bp310Driver{channel: channel, calls: map[string]int{}, fail: fail}
	g := New(cfg, p, router.New(cfg), map[string]driver.Driver{channel: drv}, st)
	return g, drv
}

func bp310Request() *ir.ChatRequest {
	return &ir.ChatRequest{
		Model: "m", Stream: true, MaxTokens: 100,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
}

// TestBP310SingleAccountNotRedispatched is the exact field shape: one account
// on the channel, a transport failure. The driver must be called ONCE (it was
// three times), and the failure surfaces unchanged.
func TestBP310SingleAccountNotRedispatched(t *testing.T) {
	g, drv := newBP310Gateway(t, []string{"only-1"}, map[string]bool{"only-1": true})
	_, err := g.Execute(context.Background(), bp310Request(), func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("Execute: want the transport failure")
	}
	if ue, ok := uperr.As(err); !ok || ue.Class != uperr.ClassTransport {
		t.Fatalf("error = %v, want the original transport failure", err)
	}
	if got := drv.calls["only-1"]; got != 1 {
		t.Fatalf("driver called %d times, want 1 (the same account must not be re-dispatched)", got)
	}
}

// TestBP310RotatesToADifferentAccount: two accounts, the first fails — the
// second must be tried (rotation still works), and it must serve the turn.
func TestBP310RotatesToADifferentAccount(t *testing.T) {
	g, drv := newBP310Gateway(t, []string{"a-1", "b-1"}, map[string]bool{"a-1": true})
	res, err := g.Execute(context.Background(), bp310Request(), func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("Execute: %v (the second account should serve the turn)", err)
	}
	if res.Account.ID != "b-1" {
		t.Fatalf("served by %s, want b-1", res.Account.ID)
	}
	if drv.calls["a-1"] != 1 || drv.calls["b-1"] != 1 {
		t.Fatalf("calls = %v, want a-1:1 b-1:1 (each account paid once)", drv.calls)
	}
}

// TestBP310AllAccountsFailNoRevisit: both accounts fail; each is dispatched
// once, then the binding gives up instead of cycling back to the first.
func TestBP310AllAccountsFailNoRevisit(t *testing.T) {
	g, drv := newBP310Gateway(t, []string{"a-1", "b-1"},
		map[string]bool{"a-1": true, "b-1": true})
	_, err := g.Execute(context.Background(), bp310Request(), func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("Execute: want a failure")
	}
	if drv.calls["a-1"] != 1 || drv.calls["b-1"] != 1 {
		t.Fatalf("calls = %v, want a-1:1 b-1:1 (no account may be revisited)", drv.calls)
	}
}
