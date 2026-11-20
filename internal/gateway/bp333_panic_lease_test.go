package gateway

// BP-333: a panic inside a driver must not leak the pool's in-flight slot.
//
// Field 2026-11-20: prompt.CoerceSchemaKeywordTypes panicked inside
// workbuddy's buildBody on two requests. Every release of a lease was an
// explicit call on a normal return path, so the panic unwound past all of
// them and the in-flight counter for account y1313 stayed at 2/2 with nothing
// actually running. Single-active mode then read that leaked holder as "the
// pool is interacting", so every subsequent request skipped WorkBuddy and
// cascaded to Trae — for hours, across a restart-free period, with eight
// healthy accounts idle. The user saw "WorkBuddy has accounts but it keeps
// going to Trae" and "Trae cache 0%".
//
// These tests pin the invariant: after a panicking driver, the pool must be
// idle again and the channel must be usable by the very next request.

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

// bp333Driver panics on the first call and answers normally afterwards, so one
// gateway instance can show both the leak and its absence.
type bp333Driver struct {
	channel string
	panics  bool
	calls   int
}

func (d *bp333Driver) Channel() string                          { return d.channel }
func (d *bp333Driver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }

func (d *bp333Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.calls++
	if d.panics {
		d.panics = false
		panic("boom: driver blew up mid-request")
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

func newBP333Gateway(t *testing.T, accounts []string, panicFirst bool) (*Gateway, *pool.Pool, *bp333Driver) {
	t.Helper()
	channel := model.ChannelWorkBuddy
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = []config.ModelBinding{{Canonical: "m", Channel: channel, UpstreamModel: "m"}}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second}, nil,
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	for _, id := range accounts {
		p.Upsert(&model.Account{ID: id, Channel: channel, Realm: "cn", CredsJSON: []byte(`{}`)})
	}
	drv := &bp333Driver{channel: channel, panics: panicFirst}
	g := New(cfg, p, router.New(cfg), map[string]driver.Driver{channel: drv}, st)
	return g, p, drv
}

// mustPanic runs Execute and reports whether it panicked, so the test can
// assert the leak WITHOUT swallowing the panic silently: the panic itself is
// the trigger, the leak is what we are testing.
func mustPanic(t *testing.T, g *Gateway) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Execute did not panic; the test needs a panicking driver to be meaningful")
		}
	}()
	_, _ = g.Execute(context.Background(), bp310Request(), func(ir.StreamEvent) error { return nil })
}

// The core invariant: after a panicking driver, the account's in-flight count
// is back to zero. Before the fix it stayed at 1 forever.
func TestBP333PanicDoesNotLeakInflightSlot(t *testing.T) {
	g, p, _ := newBP333Gateway(t, []string{"wb-1"}, true)
	mustPanic(t, g)
	if got := p.InFlight("wb-1"); got != 0 {
		t.Fatalf("in-flight after a panic = %d, want 0 (the slot must be returned on the panic path)", got)
	}
}

// The user-visible symptom: the channel must not read as "busy" after a panic.
// Before the fix single-active mode saw the leaked holder and skipped the
// whole pool on every later request.
func TestBP333PanicDoesNotLeaveChannelBusy(t *testing.T) {
	g, p, _ := newBP333Gateway(t, []string{"wb-1"}, true)
	mustPanic(t, g)
	if busy, who := p.ChannelBusy(model.ChannelWorkBuddy); busy {
		t.Fatalf("channel still busy after a panic (holder %q): the next request would cascade away", who)
	}
}

// End to end: the request after the panic must be served by WorkBuddy, not
// fall through to another channel. This is the exact regression the user
// reported ("WorkBuddy has accounts but it keeps going to Trae").
func TestBP333NextRequestAfterPanicIsServed(t *testing.T) {
	g, _, drv := newBP333Gateway(t, []string{"wb-1"}, true)
	mustPanic(t, g)
	res, err := g.Execute(context.Background(), bp310Request(), func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("the request after the panic failed: %v (the slot was not returned)", err)
	}
	if res.Channel != model.ChannelWorkBuddy {
		t.Fatalf("served by %s, want workbuddy (a leaked slot would force a cascade)", res.Channel)
	}
	if drv.calls != 2 {
		t.Fatalf("driver calls = %d, want 2 (one panicking, one serving)", drv.calls)
	}
}

// The explicit releases must keep working exactly as before: a normal success
// still returns the slot (the defer must not double-release into a negative
// count, and must not break the success path).
func TestBP333SuccessStillReleasesOnce(t *testing.T) {
	g, p, _ := newBP333Gateway(t, []string{"wb-1"}, false)
	if _, err := g.Execute(context.Background(), bp310Request(), func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := p.InFlight("wb-1"); got != 0 {
		t.Fatalf("in-flight after success = %d, want 0 (no leak, no negative count)", got)
	}
	if busy, who := p.ChannelBusy(model.ChannelWorkBuddy); busy {
		t.Fatalf("channel busy after a successful request (holder %q)", who)
	}
}

// A panic must not be turned into a hang either: the request goroutine still
// unwinds promptly (net/http recovers it) and the pool lock is not held.
func TestBP333PanicDoesNotDeadlockThePool(t *testing.T) {
	g, p, _ := newBP333Gateway(t, []string{"wb-1"}, true)
	mustPanic(t, g)
	done := make(chan struct{})
	go func() {
		p.InFlight("wb-1") // takes the pool lock; would block forever if the panic left it held
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the pool lock was not released by the panic path")
	}
}

// Guard against the panic text leaking into the client as a 200 with no body:
// the panic unwinds the request goroutine, which net/http turns into a closed
// connection — but the gateway must not have marked the account unhealthy for
// a bug in OUR code. This asserts the account is still schedulable.
func TestBP333PanicDoesNotPunishTheAccount(t *testing.T) {
	g, p, _ := newBP333Gateway(t, []string{"wb-1"}, true)
	mustPanic(t, g)
	if !p.HasAccounts(model.ChannelWorkBuddy) {
		t.Fatal("the channel must still have its account after a gateway-side panic")
	}
	res, err := g.Execute(context.Background(), bp310Request(), func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("account unusable after a gateway-side panic: %v", err)
	}
	if res.Account == nil || res.Account.ID != "wb-1" {
		t.Fatalf("served by %v, want wb-1", res.Account)
	}
}
