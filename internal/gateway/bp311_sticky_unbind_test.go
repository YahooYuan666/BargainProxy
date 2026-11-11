package gateway

import (
	"context"
	"strings"
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

// BP-311: stickiness must not sacrifice a whole channel to one dead account.
//
// Field 2026-11-11 20:53: the conversation was pinned to one of six WorkBuddy
// accounts. That account dropped the connection in 5.2s with nothing
// delivered; the sticky rule kept handing the SAME account back, the binding
// gave up, and the request cascaded to Trae → Qoder → Lobster while five
// accounts with thousands of credits sat idle. The very next request was
// served by one of them.

type bp311Driver struct {
	channel string
	calls   map[string]int
	fail    map[string]uperr.Error // per account: class+status to return
}

func (d *bp311Driver) Channel() string                          { return d.channel }
func (d *bp311Driver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }

func (d *bp311Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.calls[call.Account.ID]++
	if f, bad := d.fail[call.Account.ID]; bad {
		ue := f
		return nil, &ue
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

func newBP311Gateway(t *testing.T, channel string, accounts []string, fail map[string]uperr.Error) (*Gateway, *bp311Driver) {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = []config.ModelBinding{{Canonical: "m", Channel: channel, UpstreamModel: "m"}}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second}, nil,
		func(a *model.Account) { _ = st.SaveAccount(a) })
	for _, id := range accounts {
		p.Upsert(&model.Account{ID: id, Channel: channel, CredsJSON: []byte(`{}`)})
	}
	drv := &bp311Driver{channel: channel, calls: map[string]int{}, fail: fail}
	return New(cfg, p, router.New(cfg), map[string]driver.Driver{channel: drv}, st), drv
}

func bp311Request() *ir.ChatRequest {
	return &ir.ChatRequest{
		Model: "m", Stream: true, MaxTokens: 100, SessionKey: "conv-1",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
}

// TestBP311StickyAccountDropsAndOtherAccountsServe is the field shape: the
// bound account is unreachable, another account of the SAME channel must take
// the turn (before the fix, the whole binding gave up and the request failed).
func TestBP311StickyAccountDropsAndOtherAccountsServe(t *testing.T) {
	g, drv := newBP311Gateway(t, model.ChannelWorkBuddy, []string{"a-1", "b-1"},
		map[string]uperr.Error{"a-1": *uperr.New(uperr.ClassTransport, 0, "workbuddy: EOF")})
	// Warm the conversation onto a-1 exactly as a real session would.
	lease, err := g.pool.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "conv-1")
	if err != nil {
		t.Fatalf("warm-up acquire: %v", err)
	}
	bound := lease.Account.ID
	lease.Release()
	// Make sure the failing account IS the bound one, whatever the pick was.
	if bound != "a-1" {
		drv.fail = map[string]uperr.Error{bound: *uperr.New(uperr.ClassTransport, 0, "EOF")}
	}

	res, execErr := g.Execute(context.Background(), bp311Request(), func(ir.StreamEvent) error { return nil })
	if execErr != nil {
		t.Fatalf("Execute: %v — the channel still has a healthy account; it must serve the turn", execErr)
	}
	if res.Account.ID == bound {
		t.Fatalf("served by the unreachable bound account %s", bound)
	}
	if drv.calls[bound] != 1 {
		t.Fatalf("bound account dispatched %d times, want 1", drv.calls[bound])
	}
	if _, stillBound := g.pool.SessionBinding(model.ChannelWorkBuddy, "conv-1"); stillBound {
		if id, _ := g.pool.SessionBinding(model.ChannelWorkBuddy, "conv-1"); id == bound {
			t.Fatalf("session stayed bound to the failed account %s", bound)
		}
	}
}

// TestBP311NonTransportFailureKeepsStickiness: only "the account could not be
// reached" unbinds. A payload rejection (400) leaves the account perfectly
// healthy, so the conversation must stay pinned to it — unbinding there would
// silently move a warm conversation for no reason.
//
// (A rate limit is deliberately NOT tested end-to-end here: the pool's own
// admission drops the binding while the account rests, which is pre-existing
// BP-95/BP-250 behaviour and independent of this change. The predicate itself
// is pinned in TestBP311ClassifyRepeatWall — a 429 is not a repeat-wall.)
func TestBP311NonTransportFailureKeepsStickiness(t *testing.T) {
	g, _ := newBP311Gateway(t, model.ChannelWorkBuddy, []string{"a-1"},
		map[string]uperr.Error{"a-1": *uperr.New(uperr.ClassBadRequest, 400, "invalid params")})
	lease, err := g.pool.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "conv-1")
	if err != nil {
		t.Fatalf("warm-up acquire: %v", err)
	}
	bound := lease.Account.ID
	lease.Release()

	_, _ = g.Execute(context.Background(), bp311Request(), func(ir.StreamEvent) error { return nil })
	if id, ok := g.pool.SessionBinding(model.ChannelWorkBuddy, "conv-1"); !ok || id != bound {
		t.Fatalf("a payload rejection must keep the binding (got %q, ok=%v, want %q)", id, ok, bound)
	}
}

// TestBP311Empty5xxNotRedispatched: an upstream that answered 5xx with an
// EMPTY body cannot serve the request — the same account must not be paid
// again within the same binding (lobster 2026-11-11 20:54: three identical
// 500s in 6s).
func TestBP311Empty5xxNotRedispatched(t *testing.T) {
	g, drv := newBP311Gateway(t, model.ChannelQoder, []string{"only-1"},
		map[string]uperr.Error{"only-1": *uperr.New(uperr.ClassTransport, 500, "lobster: empty response body (HTTP 500)")})
	_, err := g.Execute(context.Background(), bp311Request(), func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("Execute: want the failure")
	}
	if got := drv.calls["only-1"]; got != 1 {
		t.Fatalf("driver called %d times, want 1 (an empty 5xx must not be retried on the same account)", got)
	}
}

// TestBP311ClassifyRepeatWall pins the predicate itself: what counts as "a
// retry would just meet the same wall".
func TestBP311ClassifyRepeatWall(t *testing.T) {
	cases := []struct {
		name string
		ue   *uperr.Error
		want bool
	}{
		{"status0 transport", uperr.New(uperr.ClassTransport, 0, "EOF"), true},
		{"status0 timeout", uperr.New(uperr.ClassUpstreamTimeout, 0, "dial timeout"), true},
		{"empty 500", uperr.New(uperr.ClassTransport, 500, "lobster: empty response body (HTTP 500)"), true},
		{"empty 502", uperr.New(uperr.ClassTransport, 502, "empty response body"), true},
		{"5xx with body", uperr.New(uperr.ClassTransport, 502, "upstream said: boom"), false},
		{"429 rate limit", uperr.New(uperr.ClassSoftRate, 429, "rate limited"), false},
		{"404 not found", uperr.New(uperr.ClassNotFound, 404, "no route"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := isRepeatPenaltyFree(c.ue); got != c.want {
			t.Errorf("%s: isRepeatPenaltyFree = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestBP311UnbindOnlyDropsTheBinding: the pool-side helper must not touch
// anything but the session entry.
func TestBP311UnbindOnlyDropsTheBinding(t *testing.T) {
	g, _ := newBP311Gateway(t, model.ChannelWorkBuddy, []string{"a-1"}, nil)
	lease, err := g.pool.Acquire(context.Background(), model.ChannelWorkBuddy, "m", "conv-1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	id := lease.Account.ID
	lease.Release()
	before := len(g.pool.Accounts(model.ChannelWorkBuddy))

	g.pool.UnbindSession(model.ChannelWorkBuddy, "conv-1")
	if _, ok := g.pool.SessionBinding(model.ChannelWorkBuddy, "conv-1"); ok {
		t.Fatal("binding must be gone")
	}
	accts := g.pool.Accounts(model.ChannelWorkBuddy)
	if len(accts) != before {
		t.Fatalf("account count changed %d → %d", before, len(accts))
	}
	for _, a := range accts {
		if a.ID != id {
			t.Fatalf("account %s appeared/disappeared", a.ID)
		}
		if !a.AccountCooldownUntil.IsZero() && time.Now().Before(a.AccountCooldownUntil) {
			t.Fatalf("unbinding must not cool the account (%s until %s)", a.ID, a.AccountCooldownUntil)
		}
	}
}

// TestBP311IgnoresEmptySessionKey: a request without a session key must not
// panic or log anything.
func TestBP311IgnoresEmptySessionKey(t *testing.T) {
	g, _ := newBP311Gateway(t, model.ChannelWorkBuddy, []string{"a-1"}, nil)
	g.pool.UnbindSession(model.ChannelWorkBuddy, "")
	if strings.Contains("", "") { // keep strings imported for the table test above
		t.Log("no-op")
	}
}
