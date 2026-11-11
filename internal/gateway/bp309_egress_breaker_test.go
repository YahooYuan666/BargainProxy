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

// BP-309: a request that cascades across channels must not keep paying every
// channel's timeout once several DIFFERENT channels have failed before their
// first HTTP byte. Field 2026-11-11 16:15: the local Wi-Fi dropped; the
// gateway walked workbuddy→trae→qoder→lobster, and the client was told
// "lobster: EOF" — a misattribution of a dead local egress.

// bp309Driver answers (or fails) per channel and counts its calls.
type bp309Driver struct {
	channel string
	calls   *int
	fail    bool
	class   string
	status  int
}

func (d *bp309Driver) Channel() string                          { return d.channel }
func (d *bp309Driver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }

func (d *bp309Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	*d.calls++
	if d.fail {
		return nil, uperr.New(d.class, d.status, "%s: no answer", d.channel)
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

type bp309Leg struct {
	channel string
	fail    bool
	class   string
	status  int
}

// newBP309Gateway wires one account per channel behind cfg.Models bindings in
// the order given, so the cascade order is exactly `legs`.
func newBP309Gateway(t *testing.T, legs []bp309Leg) (*Gateway, map[string]*int) {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = nil
	counts := map[string]*int{}
	drivers := map[string]driver.Driver{}
	for _, leg := range legs {
		cfg.Models = append(cfg.Models, config.ModelBinding{
			Canonical: "m", Channel: leg.channel, UpstreamModel: "m",
		})
		c := new(int)
		counts[leg.channel] = c
		drivers[leg.channel] = &bp309Driver{
			channel: leg.channel, calls: c, fail: leg.fail, class: leg.class, status: leg.status,
		}
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second}, nil,
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	for _, leg := range legs {
		p.Upsert(&model.Account{ID: leg.channel + "-1", Channel: leg.channel, CredsJSON: []byte(`{}`)})
	}
	return New(cfg, p, router.New(cfg), drivers, st), counts
}

func bp309Request(modelName string) *ir.ChatRequest {
	return &ir.ChatRequest{
		Model: modelName, Stream: true, MaxTokens: 100,
		ToolDeltasBuffered: true,
		Messages:           []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
}

func bp309Calls(counts map[string]*int, channel string) int {
	if c := counts[channel]; c != nil {
		return *c
	}
	return 0
}

// TestBP309TripsAfterThreeNoResponseLegs: three different channels fail with
// status 0 and nothing delivered; the fourth must NOT be dispatched, and the
// error must name the tried channels and claim the egress, not the last leg.
func TestBP309TripsAfterThreeNoResponseLegs(t *testing.T) {
	g, counts := newBP309Gateway(t, []bp309Leg{
		{channel: model.ChannelWorkBuddy, fail: true, class: uperr.ClassTransport, status: 0},
		{channel: model.ChannelTrae, fail: true, class: uperr.ClassTransport, status: 0},
		{channel: model.ChannelQoder, fail: true, class: uperr.ClassTransport, status: 0},
		{channel: model.ChannelLobster, fail: true, class: uperr.ClassTransport, status: 0},
	})
	_, err := g.Execute(context.Background(), bp309Request("m"), func(ir.StreamEvent) error { return nil })
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassEgressDown {
		t.Fatalf("error = %v (class %v), want ClassEgressDown", err, err)
	}
	if got := bp309Calls(counts, model.ChannelLobster); got != 0 {
		t.Fatalf("lobster dispatched %d times, want 0 (the breaker must stop the cascade)", got)
	}
	for _, ch := range []string{model.ChannelWorkBuddy, model.ChannelTrae, model.ChannelQoder} {
		if bp309Calls(counts, ch) == 0 {
			t.Fatalf("%s was never dispatched, want it tried before the breaker trips", ch)
		}
	}
	for _, want := range []string{"workbuddy", "trae", "qoder", "本地网络出口疑似中断", "已停止尝试其余 1 个渠道"} {
		if !strings.Contains(ue.Message, want) {
			t.Fatalf("message %q does not contain %q", ue.Message, want)
		}
	}
	if ue.Message == "" || strings.Contains(ue.Message, "lobster: no answer") {
		t.Fatalf("message must not blame the untried last leg: %q", ue.Message)
	}
}

// TestBP309HTTP5xxDoesNotCount: an upstream that ANSWERS (even with 5xx) has
// proven the network path works. Three such legs must not trip the breaker;
// the fourth channel still gets its chance and succeeds.
func TestBP309HTTP5xxDoesNotCount(t *testing.T) {
	g, counts := newBP309Gateway(t, []bp309Leg{
		{channel: model.ChannelWorkBuddy, fail: true, class: uperr.ClassTransport, status: 502},
		{channel: model.ChannelTrae, fail: true, class: uperr.ClassTransport, status: 502},
		{channel: model.ChannelQoder, fail: true, class: uperr.ClassTransport, status: 502},
		{channel: model.ChannelLobster},
	})
	res, err := g.Execute(context.Background(), bp309Request("m"), func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("Execute: %v (HTTP 5xx legs must not trip the egress breaker)", err)
	}
	if res.Channel != model.ChannelLobster {
		t.Fatalf("served by %s, want lobster", res.Channel)
	}
	if got := bp309Calls(counts, model.ChannelLobster); got == 0 {
		t.Fatal("lobster was never dispatched")
	}
}

// TestBP309WindowCancelDoesNotCount: our own first-byte window expiry is
// ClassCanceled, not a network verdict. One window expiry plus two dead legs
// must not trip the breaker (the quiet fourth channel still gets a chance).
func TestBP309WindowCancelDoesNotCount(t *testing.T) {
	g, _ := newBP309Gateway(t, []bp309Leg{
		{channel: model.ChannelWorkBuddy, fail: true, class: uperr.ClassCanceled, status: 0},
		{channel: model.ChannelTrae, fail: true, class: uperr.ClassTransport, status: 0},
		{channel: model.ChannelQoder, fail: true, class: uperr.ClassTransport, status: 0},
		{channel: model.ChannelLobster},
	})
	res, err := g.Execute(context.Background(), bp309Request("m"), func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("Execute: %v (a first-byte window cancel is not an egress verdict)", err)
	}
	if res.Channel != model.ChannelLobster {
		t.Fatalf("served by %s, want lobster", res.Channel)
	}
}

// TestBP309FastFailWindow: after a trip, an immediate retry must not dispatch
// anything; once the window passes, normal routing resumes. A pinned request
// is exempt (the caller explicitly asked for that channel's honest answer).
func TestBP309FastFailWindow(t *testing.T) {
	g, counts := newBP309Gateway(t, []bp309Leg{
		{channel: model.ChannelWorkBuddy},
	})
	g.egressDownUntil.Store(time.Now().Add(egressDownWindow).UnixNano())
	_, err := g.Execute(context.Background(), bp309Request("m"), func(ir.StreamEvent) error { return nil })
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassEgressDown {
		t.Fatalf("error = %v, want ClassEgressDown inside the suspect window", err)
	}
	if got := bp309Calls(counts, model.ChannelWorkBuddy); got != 0 {
		t.Fatalf("driver dispatched %d times inside the window, want 0", got)
	}

	// Expired window: routing resumes and the success clears the state.
	g.egressDownUntil.Store(time.Now().Add(-time.Second).UnixNano())
	if _, err := g.Execute(context.Background(), bp309Request("m"), func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("Execute after window: %v", err)
	}
	if got := bp309Calls(counts, model.ChannelWorkBuddy); got == 0 {
		t.Fatal("driver was not dispatched after the window expired")
	}
	if g.egressDownUntil.Load() != 0 {
		t.Fatal("a successful leg must clear the egress suspect state")
	}

	// Pinned requests bypass the fast-fail.
	g.egressDownUntil.Store(time.Now().Add(egressDownWindow).UnixNano())
	res, err := g.Execute(context.Background(),
		bp309Request("channel:"+model.ChannelWorkBuddy+":m"), func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("pinned Execute: %v (pinned channels are exempt from the fast-fail)", err)
	}
	if res.Channel != model.ChannelWorkBuddy {
		t.Fatalf("pinned request served by %s, want workbuddy", res.Channel)
	}
}

// TestBP309AllLegsNoResponseIsSummarized: when the route runs out before the
// streak limit, the final error must still name every channel that produced
// nothing instead of quoting only the last one.
func TestBP309AllLegsNoResponseIsSummarized(t *testing.T) {
	g, _ := newBP309Gateway(t, []bp309Leg{
		{channel: model.ChannelWorkBuddy, fail: true, class: uperr.ClassTransport, status: 0},
		{channel: model.ChannelTrae, fail: true, class: uperr.ClassTransport, status: 0},
	})
	_, err := g.Execute(context.Background(), bp309Request("m"), func(ir.StreamEvent) error { return nil })
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassEgressDown {
		t.Fatalf("error = %v, want ClassEgressDown", err)
	}
	for _, want := range []string{"workbuddy", "trae", "该模型的渠道已全部试完"} {
		if !strings.Contains(ue.Message, want) {
			t.Fatalf("message %q does not contain %q", ue.Message, want)
		}
	}
}
