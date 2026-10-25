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

// BP-118: only CLIENT-VISIBLE content may count as "the client has already
// seen part of the answer". Usage and terminal events are bookkeeping.
//
// Field 2026-10-26 01:02: a workbuddy leg died with "stream read: unexpected
// EOF" at 27s having emitted only bookkeeping. firstEvent was already true
// (so the cross-channel cascade was refused), and the ledger held no text (so
// the same-account continuation was refused) — the request failed with BOTH
// escape hatches shut, while the same conversation answered 200 on the same
// channel seconds before and after.
func TestBookkeepingEventsDoNotBlockCascade(t *testing.T) {
	// Binding 1: emits usage then dies. Binding 2: answers.
	first := &scriptedDriver{segs: []func(call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error){
		func(_ driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
			u := ir.Usage{InputTokens: 100, OutputTokens: 0}
			if err := emit(ir.StreamEvent{Kind: ir.EvUsage, Usage: &u}); err != nil {
				return nil, err
			}
			return nil, uperr.New(uperr.ClassTransport, 0, "workbuddy: stream read: unexpected EOF")
		},
	}}
	second := &scriptedDriver{segs: []func(call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error){say("from the second binding")}}

	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = []config.ModelBinding{
		{Canonical: "m", Channel: model.ChannelWorkBuddy, UpstreamModel: "m"},
		{Canonical: "m", Channel: model.ChannelQoder, UpstreamModel: "m"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second}, map[string]int{
		model.ChannelWorkBuddy: 1,
		model.ChannelQoder:     1,
	}, func(acc *model.Account) { _ = st.SaveAccount(acc) })
	p.Upsert(&model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy, CredsJSON: []byte(`{}`)})
	p.Upsert(&model.Account{ID: "qd-1", Channel: model.ChannelQoder, CredsJSON: []byte(`{}`)})
	g := New(cfg, p, router.New(cfg), map[string]driver.Driver{
		model.ChannelWorkBuddy: first,
		model.ChannelQoder:     second,
	}, st)

	rec := &recorder{}
	res, err := g.Execute(context.Background(), streamReq(), rec.emit)
	if err != nil {
		t.Fatalf("a bookkeeping-only failure must cascade, not surface: %v", err)
	}
	if res == nil || res.Channel != model.ChannelQoder {
		t.Fatalf("expected the second binding to serve, got %+v", res)
	}
	if got := rec.text.String(); got != "from the second binding" {
		t.Fatalf("client text = %q", got)
	}
}

// The opposite must still hold: once REAL content reached the client, a
// mid-stream failure surfaces instead of replaying another binding.
func TestRealContentStillBlocksCascade(t *testing.T) {
	first := &scriptedDriver{segs: []func(call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error){
		sayThenDrop(pad("partial answer"), uperr.ClassTransport),
		sayThenDrop(pad("partial answer"), uperr.ClassTransport),
		sayThenDrop(pad("partial answer"), uperr.ClassTransport),
	}}
	second := &scriptedDriver{segs: []func(call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error){say("must not be reached")}}

	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = []config.ModelBinding{
		{Canonical: "m", Channel: model.ChannelWorkBuddy, UpstreamModel: "m"},
		{Canonical: "m", Channel: model.ChannelQoder, UpstreamModel: "m"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second}, map[string]int{
		model.ChannelWorkBuddy: 1,
		model.ChannelQoder:     1,
	}, func(acc *model.Account) { _ = st.SaveAccount(acc) })
	p.Upsert(&model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy, CredsJSON: []byte(`{}`)})
	p.Upsert(&model.Account{ID: "qd-1", Channel: model.ChannelQoder, CredsJSON: []byte(`{}`)})
	g := New(cfg, p, router.New(cfg), map[string]driver.Driver{
		model.ChannelWorkBuddy: first,
		model.ChannelQoder:     second,
	}, st)

	rec := &recorder{}
	if _, err := g.Execute(context.Background(), streamReq(), rec.emit); err == nil {
		t.Fatal("a mid-stream failure after real content must surface")
	}
	if second.callCount() != 0 {
		t.Fatalf("second binding was dispatched %d times; real content must block the cascade", second.callCount())
	}
}
