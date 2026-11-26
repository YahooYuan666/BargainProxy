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

// BP-345, end to end through the gateway — the field case of 2026-11-25
// 22:30–22:40 (glm-5.4 has AutoClaw as its only channel):
//
//	conversation 1: glm-5.3-flash on account A, a long turn, still running
//	conversation 2: glm-5.4, bound to A; A's glm-5.4 hits the daily wall (810000)
//	conversation 2 asks again
//
// What happened: conv-2's binding was dropped, the pool-busy pre-check saw A
// still interacting and SKIPPED the channel ("pool busy ... not bound here",
// 15 times), and — when it got past that — the pool refused at once ("one
// account per pool at a time", 3 times). Every turn of conv-2 ended in an
// error although account B was idle and had glm-5.4.
//
// What must happen: conv-2 waits for A to finish and is served by B. ONE
// account interacts at a time throughout — the rule the user set on
// 2026-10-20 and explicitly kept for this fix.

// bp345Driver stands in for the AutoClaw driver. It answers by (account,
// upstream model) from a scripted table, so the wall is raised exactly where
// the field raised it.
type bp345Driver struct {
	mu       sync.Mutex
	calls    map[string]int
	wall     map[string]bool // "account|model" -> answers 810000
	slow     map[string]bool // "account|model" -> holds the call
	inFlight map[string]int  // account -> calls currently running
	maxSeen  map[string]int  // account -> peak in-flight
	gate     chan struct{}   // closed to release held calls
}

func newBP345Driver() *bp345Driver {
	return &bp345Driver{
		calls:    map[string]int{},
		wall:     map[string]bool{},
		slow:     map[string]bool{},
		inFlight: map[string]int{},
		maxSeen:  map[string]int{},
		gate:     make(chan struct{}),
	}
}

func (d *bp345Driver) Channel() string { return model.ChannelAutoClaw }
func (d *bp345Driver) Models(context.Context) ([]string, error) {
	return []string{"glm-5.4", "glm-5.3-flash"}, nil
}

func (d *bp345Driver) callsFor(key string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[key]
}

func (d *bp345Driver) snapshotCalls() map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]int{}
	for k, v := range d.calls {
		out[k] = v
	}
	return out
}

func (d *bp345Driver) inFlightTotal() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, v := range d.inFlight {
		n += v
	}
	return n
}

func (d *bp345Driver) holderID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, v := range d.inFlight {
		if v > 0 {
			return id
		}
	}
	return ""
}

func (d *bp345Driver) peak(account string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.maxSeen[account]
}

func (d *bp345Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	key := call.Account.ID + "|" + call.UpstreamModel
	d.mu.Lock()
	d.calls[key]++
	d.inFlight[call.Account.ID]++
	if d.inFlight[call.Account.ID] > d.maxSeen[call.Account.ID] {
		d.maxSeen[call.Account.ID] = d.inFlight[call.Account.ID]
	}
	walled, slow := d.wall[key], d.slow[key]
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.inFlight[call.Account.ID]--
		d.mu.Unlock()
	}()
	if walled {
		e := uperr.New(uperr.ClassHardCredit, 402, "autoclaw: %s 今日用量额度已用完（810000）", call.UpstreamModel)
		e.Code = "810000"
		e.Model = call.UpstreamModel
		e.ResetAt = pool.NextDay4AM(time.Now())
		e.ResetHard = true
		return nil, e
	}
	if slow {
		select {
		case <-d.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	_ = emit(ir.StreamEvent{Kind: ir.EvText, Delta: "ok"})
	_ = emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "ok"}}},
		StopReason: ir.StopEnd,
	}, nil
}

func TestBP345WalledModelDoesNotEndTheConversation(t *testing.T) {
	drv := newBP345Driver()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.FirstByteWindow = config.Duration(0)
	// Two canonical models, both served only by AutoClaw — glm-5.4's real
	// situation (no other channel has it yet).
	cfg.Models = []config.ModelBinding{
		{Canonical: "glm-5.4", Channel: model.ChannelAutoClaw, UpstreamModel: "zai_glm-x-preview", Priority: 1},
		{Canonical: "glm-5.3-flash", Channel: model.ChannelAutoClaw, UpstreamModel: "zai_glm-5.3-flash", Priority: 1},
	}
	st := store.NewMemory()
	// max_in_flight 1: one in-flight call leaves the account FULL, which is
	// when single-active starts answering "busy" (the live pool runs 2).
	p := pool.New(pool.Params{MaxQueueWait: 5 * time.Second, SingleActivePerChannel: true},
		map[string]int{model.ChannelAutoClaw: 1},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	// Realm is left CN: the international switch is off by default and the
	// question here is scheduling, not realm visibility.
	p.Upsert(&model.Account{ID: "ac-a", Channel: model.ChannelAutoClaw, CredsJSON: []byte(`{}`)})
	g := New(cfg, p, router.New(cfg), map[string]driver.Driver{model.ChannelAutoClaw: drv}, st)

	req := func(session, canonical string) *ir.ChatRequest {
		return &ir.ChatRequest{
			Model: canonical, Stream: true, MaxTokens: 100, SessionKey: session,
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		}
	}
	run := func(session, canonical string) error {
		_, err := g.Execute(context.Background(), req(session, canonical), func(ir.StreamEvent) error { return nil })
		return err
	}

	// 1. Conversation 1 binds to account A (the pool's only account so far):
	//    it has been riding A all along, as in the field.
	if err := run("conv-1", "glm-5.3-flash"); err != nil {
		t.Fatalf("conv-1 warm-up: %v", err)
	}

	// 2. Conversation 2 binds to A too (A is the holder; one account at a
	//    time) and its glm-5.4 hits the daily wall there. This first refusal
	//    is nobody's fault and is allowed to reach the client: nothing could
	//    know about the wall before it was hit.
	if err := run("conv-2", "glm-5.4"); err != nil {
		t.Fatalf("conv-2 before the wall: %v", err)
	}
	drv.mu.Lock()
	drv.wall["ac-a|zai_glm-x-preview"] = true
	drv.mu.Unlock()
	if err := run("conv-2", "glm-5.4"); err == nil {
		t.Fatal("conv-2 succeeded although account A's glm-5.4 is walled and no other account exists")
	}
	// A's glm-5.3-flash is untouched by the wall.
	if err := run("conv-1", "glm-5.3-flash"); err != nil {
		t.Fatalf("conv-1 after the glm-5.4 wall: %v (the wall is model-scoped)", err)
	}

	// 3. Account B joins the pool and conversation 1 starts a long
	//    glm-5.3-flash turn, leaving the pool held.
	p.Upsert(&model.Account{ID: "ac-b", Channel: model.ChannelAutoClaw, CredsJSON: []byte(`{}`)})
	drv.mu.Lock()
	drv.slow["ac-a|zai_glm-5.3-flash"] = true
	drv.slow["ac-b|zai_glm-5.3-flash"] = true
	drv.mu.Unlock()
	longDone := make(chan error, 1)
	go func() { longDone <- run("conv-1", "glm-5.3-flash") }()
	deadline := time.Now().Add(2 * time.Second)
	for drv.inFlightTotal() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if drv.inFlightTotal() == 0 {
		close(drv.gate)
		<-longDone
		t.Fatalf("the long turn never started; calls: %v", drv.snapshotCalls())
	}
	holder := drv.holderID()

	// 4. Conversation 2 asks again while the holder is still busy on
	//    conversation 1.
	//    This is the request that used to end with "pool busy".
	conv2 := make(chan error, 1)
	t0 := time.Now()
	go func() { conv2 <- run("conv-2", "glm-5.4") }()

	// It must not come back at once, and account B must not start while A is
	// interacting.
	select {
	case err := <-conv2:
		t.Fatalf("conv-2 returned after %v while account A was still interacting (err=%v); calls: %v",
			time.Since(t0), err, drv.snapshotCalls())
	case <-time.After(300 * time.Millisecond):
	}
	if n := drv.callsFor("ac-b|zai_glm-x-preview") + drv.callsFor("ac-a|zai_glm-x-preview"); n != 2 {
		t.Fatalf("glm-5.4 calls so far = %d, want 2 (both before the wall): a second account must not start while the holder interacts", n)
	}

	// 5. Conversation 1 finishes. The pool opens; conv-2 is served by the
	//    account that was not walled.
	close(drv.gate)
	if err := <-longDone; err != nil {
		t.Fatalf("conv-1 long turn: %v", err)
	}
	select {
	case err := <-conv2:
		if err != nil {
			t.Fatalf("conv-2 after the pool opened: %v — the walled account ended the conversation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("conv-2 was never served")
	}
	other := "ac-b"
	if holder == "ac-b" {
		other = "ac-a"
	}
	// The walled account is whichever one conv-2 hit the wall on: ac-a.
	if n := drv.callsFor("ac-a|zai_glm-x-preview"); n != 2 {
		t.Fatalf("account A took %d glm-5.4 calls, want 2 — a walled account must not be re-asked", n)
	}
	if n := drv.callsFor("ac-b|zai_glm-x-preview"); n != 1 {
		t.Fatalf("account B took %d glm-5.4 calls, want 1 (it serves conv-2 once the pool opens)", n)
	}
	_ = other
	// The long turn stayed on ONE account: whichever it started on.
	if pk := drv.peak(holder); pk != 1 {
		t.Fatalf("holder %s peaked at %d concurrent calls, want 1", holder, pk)
	}
	// Account B never overlapped with the long turn: B's glm-5.4 call began
	// only after the holder's call ended (asserted by the 300ms window above
	// plus B's single call here).
	if drv.peak("ac-b") != 1 {
		t.Fatalf("account B peaked at %d concurrent calls, want 1", drv.peak("ac-b"))
	}
}

// The guard the fix must not loosen: an ordinary unbound conversation still
// skips a busy pool at once (it must not wait for a pool whose holder CAN
// serve its model). TestUnboundSessionStillSkipsBusyPool pins this across two
// channels; this is the same rule inside one channel with a walled model
// standing by — the newcomer asks for a model the holder serves fine.
func TestBP345UnwalledNewcomerStillSkipsBusyPool(t *testing.T) {
	drv := newBP345Driver()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.FirstByteWindow = config.Duration(0)
	cfg.Models = []config.ModelBinding{
		{Canonical: "glm-5.4", Channel: model.ChannelAutoClaw, UpstreamModel: "zai_glm-x-preview", Priority: 1},
		{Canonical: "glm-5.3-flash", Channel: model.ChannelAutoClaw, UpstreamModel: "zai_glm-5.3-flash", Priority: 1},
	}
	st := store.NewMemory()
	// max_in_flight 1: the holder is full as soon as it takes one call.
	p := pool.New(pool.Params{MaxQueueWait: 5 * time.Second, SingleActivePerChannel: true},
		map[string]int{model.ChannelAutoClaw: 1},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	p.Upsert(&model.Account{ID: "ac-a", Channel: model.ChannelAutoClaw, CredsJSON: []byte(`{}`)})
	p.Upsert(&model.Account{ID: "ac-b", Channel: model.ChannelAutoClaw, CredsJSON: []byte(`{}`)})
	g := New(cfg, p, router.New(cfg), map[string]driver.Driver{model.ChannelAutoClaw: drv}, st)

	req := func(session, canonical string) *ir.ChatRequest {
		return &ir.ChatRequest{
			Model: canonical, Stream: true, MaxTokens: 100, SessionKey: session,
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		}
	}

	drv.mu.Lock()
	drv.slow["ac-a|zai_glm-5.3-flash"] = true
	drv.slow["ac-b|zai_glm-5.3-flash"] = true
	drv.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = g.Execute(context.Background(), req("conv-1", "glm-5.3-flash"), func(ir.StreamEvent) error { return nil })
	}()
	// Which account the first conversation lands on is not the point (two
	// idle accounts, no binding: the pool picks). What matters is that
	// exactly ONE of them starts and that the newcomer does not start the
	// other.
	deadline := time.Now().Add(2 * time.Second)
	for drv.inFlightTotal() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if drv.inFlightTotal() == 0 {
		t.Fatal("conv-1 never reached an account")
	}
	holder := drv.holderID()
	other := "ac-a"
	if holder == "ac-a" {
		other = "ac-b"
	}

	// A stranger asks for the SAME model the holder is busy with. The holder
	// could serve it (it is only full), so the newcomer is refused at once —
	// not queued for five seconds, and not started on the other account.
	start := time.Now()
	err := func() error {
		_, err := g.Execute(context.Background(), req("conv-9", "glm-5.3-flash"), func(ir.StreamEvent) error { return nil })
		return err
	}()
	close(drv.gate)
	<-done
	if err == nil {
		t.Fatal("a newcomer must not be served while the holder is full")
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("the newcomer waited %s: an ordinary busy refusal must not queue", el)
	}
	if n := drv.callsFor(other + "|zai_glm-5.3-flash"); n != 0 {
		t.Fatalf("%s took %d calls: a second account must not start for an ordinary busy refusal (holder was %s)", other, n, holder)
	}
}
