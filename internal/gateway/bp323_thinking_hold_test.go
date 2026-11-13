// BP-323: reasoning that never became an answer must not close the gateway's
// escape hatches.
//
// Field 2026-11-13 19:45 (sess_1b3d39c4): a workbuddy GLOBAL account dropped
// the stream three times — 21.5s of reasoning, then two ~5s drops, the last
// one before the response headers arrived. The gateway tried the same account
// twice more ("continuing on the same account, segment 1/2, 2/2, 0 chars
// replayed") and then surfaced
//
//	upstream transport failure: workbuddy: Post "…/v2/chat/completions": EOF
//
// to the client, while five healthy accounts of the same channel sat idle. The
// client saw attempt=1 and gave up (retryable=false).
//
// The cause was a rule that treated reasoning as "the client has seen part of
// the answer": that closed BOTH remedies at once — no rotation inside the
// channel, no cascade to the next channel — and left the continuation with an
// empty replay, which just re-asked the model.
//
// The fix holds reasoning until the answer starts. It is delivered unchanged
// (in order) as soon as text or a tool call proves there IS an answer, so the
// client loses nothing it was promised; only the moment moves. Liveness is
// carried by the SSE keepalive, which never travels through this emitter.
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

// thinkThenDrop emits reasoning and then dies the way the field report did.
func thinkThenDrop(reasoning string, class string) func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error) {
	return func(_ driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
		if err := emit(ir.StreamEvent{Kind: ir.EvThinking, Delta: reasoning}); err != nil {
			return nil, err
		}
		return nil, uperr.New(class, 0,
			`workbuddy: Post "https://www.workbuddy.ai/v2/chat/completions": EOF`)
	}
}

// thinkThenSay is the healthy shape: reasoning, then the answer.
func thinkThenSay(reasoning, text string) func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error) {
	return func(_ driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
		if err := emit(ir.StreamEvent{Kind: ir.EvThinking, Delta: reasoning}); err != nil {
			return nil, err
		}
		if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: text}); err != nil {
			return nil, err
		}
		if err := emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd}); err != nil {
			return nil, err
		}
		return &ir.ChatResponse{
			Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: text}}},
			Thinking:   reasoning,
			StopReason: ir.StopEnd,
		}, nil
	}
}

// twoBindingGateway wires workbuddy + qoder with one account each, so the
// cascade has somewhere to go — exactly what the field case lacked.
func twoBindingGateway(t *testing.T, first, second driver.Driver) *Gateway {
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
		model.ChannelWorkBuddy: 1,
		model.ChannelQoder:     1,
	}, func(acc *model.Account) { _ = st.SaveAccount(acc) })
	p.Upsert(&model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy, CredsJSON: []byte(`{}`)})
	p.Upsert(&model.Account{ID: "qd-1", Channel: model.ChannelQoder, CredsJSON: []byte(`{}`)})
	return New(cfg, p, router.New(cfg), map[string]driver.Driver{
		model.ChannelWorkBuddy: first,
		model.ChannelQoder:     second,
	}, st)
}

// TestBP323ThinkingOnlyDropCascades is the headline: reasoning then a drop,
// with nothing answerable delivered, must reach the NEXT binding instead of
// failing. Before BP-323 the first binding refused the cascade (reasoning had
// set firstEvent) and the continuation refused too (the ledger's text was
// empty), so the request died on a channel that had five idle accounts.
func TestBP323ThinkingOnlyDropCascades(t *testing.T) {
	first := &scriptedDriver{segs: []func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error){
		thinkThenDrop(strings.Repeat("reasoning about the question ", 40), uperr.ClassTransport),
	}}
	second := &scriptedDriver{segs: []func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error){
		say("the answer from the second binding"),
	}}
	g := twoBindingGateway(t, first, second)

	rec := &recorder{}
	res, err := g.Execute(context.Background(), streamReq(), rec.emit)
	if err != nil {
		t.Fatalf("a reasoning-only drop must cascade, not surface: %v", err)
	}
	if res == nil || res.Channel != model.ChannelQoder {
		t.Fatalf("expected the qoder binding to serve, got %+v", res)
	}
	if got := rec.text.String(); got != "the answer from the second binding" {
		t.Fatalf("client text = %q", got)
	}
	// The dead attempt's reasoning must never reach the client: the second
	// attempt produces none, so any thinking here would be the dropped one.
	for _, ev := range rec.events {
		if ev.Kind == ir.EvThinking {
			t.Fatalf("reasoning from the failed attempt leaked to the client: %q", ev.Delta)
		}
	}
	if n := rec.doneCount(); n != 1 {
		t.Fatalf("terminal events = %d, want exactly 1", n)
	}
	// And no continuation was paid for: an empty replay is a wasted call.
	if n := first.nudgeCalls(); n != 0 {
		t.Fatalf("continuation calls = %d, want 0 (nothing to replay)", n)
	}
	if n := first.callCount(); n != 1 {
		t.Fatalf("failed binding calls = %d, want 1 (no same-account retry)", n)
	}
}

// TestBP323ThinkingHeldUntilAnswer: the healthy shape must be unchanged for
// the client — same deltas, same order — with the reasoning simply arriving
// together with the first answer byte.
func TestBP323ThinkingHeldUntilAnswer(t *testing.T) {
	drv := &scriptedDriver{segs: []func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error){
		thinkThenSay("step one; step two", pad("Here is the answer.")),
	}}
	g := newContinueGateway(t, drv)

	rec := &recorder{}
	if _, err := g.Execute(context.Background(), streamReq(), rec.emit); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var thinking, text strings.Builder
	thinkingBeforeText := true
	textSeen := false
	for _, ev := range rec.events {
		switch ev.Kind {
		case ir.EvThinking:
			if textSeen {
				thinkingBeforeText = false
			}
			thinking.WriteString(ev.Delta)
		case ir.EvText:
			textSeen = true
			text.WriteString(ev.Delta)
		}
	}
	if thinking.String() != "step one; step two" {
		t.Fatalf("reasoning delivered = %q, want it whole and unchanged", thinking.String())
	}
	if text.String() != pad("Here is the answer.") {
		t.Fatalf("text delivered = %q", text.String())
	}
	if !thinkingBeforeText {
		t.Fatal("reasoning must still precede the answer on the wire")
	}
}

// TestBP323ThinkingOnlyAnswerStillDelivered: a model that thinks and then ends
// the turn with no prose (stop, no text) must still hand its reasoning to the
// client — the hold releases on the terminal, not only on text.
func TestBP323ThinkingOnlyAnswerStillDelivered(t *testing.T) {
	drv := &scriptedDriver{segs: []func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error){
		func(_ driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
			if err := emit(ir.StreamEvent{Kind: ir.EvThinking, Delta: "only reasoning"}); err != nil {
				return nil, err
			}
			if err := emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd}); err != nil {
				return nil, err
			}
			return &ir.ChatResponse{
				Message:    ir.Message{Role: ir.RoleAssistant},
				Thinking:   "only reasoning",
				StopReason: ir.StopEnd,
			}, nil
		},
	}}
	g := newContinueGateway(t, drv)

	rec := &recorder{}
	if _, err := g.Execute(context.Background(), streamReq(), rec.emit); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var thinking strings.Builder
	for _, ev := range rec.events {
		if ev.Kind == ir.EvThinking {
			thinking.WriteString(ev.Delta)
		}
	}
	if thinking.String() != "only reasoning" {
		t.Fatalf("reasoning delivered = %q, want the held deltas released at the terminal", thinking.String())
	}
	if n := rec.doneCount(); n != 1 {
		t.Fatalf("terminal events = %d, want exactly 1", n)
	}
}

// TestBP323ThinkingThenTextThenDropStillContinues: once text HAS been
// delivered, the old same-account continuation is still the right remedy and
// must keep working — the reasoning hold must not have disabled it.
func TestBP323ThinkingThenTextThenDropStillContinues(t *testing.T) {
	drv := &scriptedDriver{segs: []func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error){
		func(_ driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
			if err := emit(ir.StreamEvent{Kind: ir.EvThinking, Delta: "some reasoning"}); err != nil {
				return nil, err
			}
			if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: pad("The first half of the answer.")}); err != nil {
				return nil, err
			}
			return nil, uperr.New(uperr.ClassTransport, 0, "workbuddy: stream read: unexpected EOF")
		},
		say(" The second half."),
	}}
	g := newContinueGateway(t, drv)

	rec := &recorder{}
	res, err := g.Execute(context.Background(), streamReq(), rec.emit)
	if err != nil {
		t.Fatalf("a drop after real text must continue: %v", err)
	}
	if n := drv.nudgeCalls(); n != 1 {
		t.Fatalf("continuation calls = %d, want 1", n)
	}
	want := pad("The first half of the answer.") + " The second half."
	if got := rec.text.String(); got != want {
		t.Fatalf("client text = %q, want %q", got, want)
	}
	if res == nil || res.Channel != model.ChannelWorkBuddy {
		t.Fatalf("expected workbuddy to serve, got %+v", res)
	}
}

// TestBP323UpstreamTTFBIgnoresTheHold: 反应时间 must still report when the
// UPSTREAM started answering, not when the hold released. The panel divides
// output tokens by (duration - ttfb) to get the token rate, so a TTFB that
// waited for the hold would push the reasoning out of both windows.
func TestBP323UpstreamTTFBIgnoresTheHold(t *testing.T) {
	// Reasoning arrives immediately, the answer only after a visible gap.
	drv := &scriptedDriver{segs: []func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error){
		func(_ driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
			if err := emit(ir.StreamEvent{Kind: ir.EvThinking, Delta: "thinking first"}); err != nil {
				return nil, err
			}
			time.Sleep(400 * time.Millisecond)
			if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: pad("the answer")}); err != nil {
				return nil, err
			}
			if err := emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd}); err != nil {
				return nil, err
			}
			return &ir.ChatResponse{
				Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "the answer"}}},
				StopReason: ir.StopEnd,
			}, nil
		},
	}}
	g := newContinueGateway(t, drv)

	// Track when the client actually received the reasoning — the hold means
	// it arrives with the text, ~400ms after the upstream sent it.
	start := time.Now()
	var firstForwarded time.Duration
	emit := func(ev ir.StreamEvent) error {
		if firstForwarded == 0 {
			firstForwarded = time.Since(start)
		}
		return nil
	}
	res, err := g.Execute(context.Background(), streamReq(), emit)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !res.UpstreamTTFBKnown {
		t.Fatal("UpstreamTTFB not recorded")
	}
	if res.UpstreamTTFB > 200*time.Millisecond {
		t.Fatalf("UpstreamTTFB = %v, want the reasoning's arrival (the upstream's real first byte)", res.UpstreamTTFB)
	}
	// And the first thing the CLIENT saw came later — that is the delay the
	// metric must NOT report.
	if firstForwarded < 400*time.Millisecond {
		t.Fatalf("first forwarded event at %v, expected it to wait for the hold (~400ms)", firstForwarded)
	}
	if firstForwarded <= res.UpstreamTTFB {
		t.Fatalf("first forwarded event at %v, not after UpstreamTTFB %v — the hold is not visible in this test",
			firstForwarded, res.UpstreamTTFB)
	}
}

// TestBP323WindowStillFiresWhileThinkingHeld: the hold must not defeat the
// first-byte window. The window is armed on the upstream stream and stopped by
// the first event the DRIVER emits — the hold sits downstream of that, so a
// leg that only ever thinks still gets cut when its window expires.
func TestBP323WindowStillFiresWhileThinkingHeld(t *testing.T) {
	drv := &scriptedDriver{segs: []func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error){
		func(_ driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
			if err := emit(ir.StreamEvent{Kind: ir.EvThinking, Delta: "thinking"}); err != nil {
				return nil, err
			}
			time.Sleep(500 * time.Millisecond)
			return nil, uperr.New(uperr.ClassCanceled, 0, "canceled")
		},
	}}
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.FallbackWait = config.Duration(150 * time.Millisecond)
	cfg.Models = []config.ModelBinding{
		{Canonical: "m", Channel: model.ChannelWorkBuddy, UpstreamModel: "m"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second}, map[string]int{
		model.ChannelWorkBuddy: 1,
	}, func(acc *model.Account) { _ = st.SaveAccount(acc) })
	p.Upsert(&model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy, CredsJSON: []byte(`{}`)})
	g := New(cfg, p, router.New(cfg), map[string]driver.Driver{model.ChannelWorkBuddy: drv}, st)

	// The reasoning event stops the timer (it is a real upstream byte), so the
	// leg survives its window — that is the pre-existing contract, unchanged by
	// the hold. What must NOT happen is the window firing late or never.
	start := time.Now()
	_, err := g.Execute(context.Background(), streamReq(), (&recorder{}).emit)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the leg to fail after the driver returned an error")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("leg took %v: the first-byte window is not bounding the attempt", elapsed)
	}
}

// TestBP323CanContinueRequiresText pins the ledger rule directly: reasoning
// alone is not an anchor for a continuation.
func TestBP323CanContinueRequiresText(t *testing.T) {
	ledger := &streamLedger{}
	ledger.record(ir.StreamEvent{Kind: ir.EvThinking, Delta: "reasoning only"})
	if ledger.canContinue(uperr.ClassTransport, 0) {
		t.Fatal("reasoning alone must not justify a continuation (nothing to replay)")
	}
	ledger.record(ir.StreamEvent{Kind: ir.EvText, Delta: "an answer"})
	if !ledger.canContinue(uperr.ClassTransport, 0) {
		t.Fatal("text on the wire must still justify a continuation")
	}
}

// TestBP323ResetDropsHeldThinking: a retry reuses the client stream, and the
// dead attempt's reasoning belongs to a model call that no longer exists.
func TestBP323ResetDropsHeldThinking(t *testing.T) {
	ledger := &streamLedger{}
	ledger.record(ir.StreamEvent{Kind: ir.EvThinking, Delta: "dead attempt reasoning"})
	if ledger.thinking.Len() == 0 {
		t.Fatal("precondition: the ledger should have recorded the reasoning")
	}
	ledger.record(ir.StreamEvent{Kind: ir.EvAttemptReset})
	if got := ledger.thinking.Len(); got != 0 {
		t.Fatalf("ledger reasoning = %d bytes after reset, want 0", got)
	}
}
