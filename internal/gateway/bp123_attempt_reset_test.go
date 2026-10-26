package gateway

import (
	"context"
	"strings"
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

// BP-123 regression: a stream that dies after emitting ONLY tool-call
// fragments, on a protocol that buffers tool blocks, must be retried on
// another channel.
//
// Field 2026-10-26 22:19 (session_c93c21ff, msg_8c1d79d1f50e): a workbuddy leg
// died with "stream read: unexpected EOF" 92s in. Proof by elimination that the
// emitted events were tool-only: ttfb=2295ms (an event WAS emitted), no
// continuation log (the ledger had no text/thinking — those would have anchored
// one), attempts=1 (the cascade was refused). The client — an Anthropic-ingress
// agent — had received nothing but message_start, because the Anthropic writer
// buffers tool blocks until the turn ends. Both escape hatches were shut by
// guards that assumed the fragments had reached the client. The turn was lost.

// toolDropDriver emits tool-call fragments then dies on its first call; on a
// later call it answers properly.
type toolDropDriver struct{ calls int }

func (d *toolDropDriver) Channel() string                          { return model.ChannelWorkBuddy }
func (d *toolDropDriver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }
func (d *toolDropDriver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.calls++
	if d.calls == 1 {
		if err := emit(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0,
			ToolCall: &ir.ToolCall{ID: "call_dead", Name: "Bash"}}); err != nil {
			return nil, err
		}
		if err := emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{"command":"cd /tm`}); err != nil {
			return nil, err
		}
		return nil, uperr.New(uperr.ClassTransport, 0, "stream read: unexpected EOF")
	}
	if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: "recovered"}); err != nil {
		return nil, err
	}
	if err := emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd}); err != nil {
		return nil, err
	}
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "recovered"}}},
		StopReason: ir.StopEnd,
	}, nil
}

// newGatewayWithTwoBindings wires one driver behind TWO bindings so a failed
// first leg cascades to the second.
func newGatewayWithTwoBindings(t *testing.T, drv driver.Driver) *Gateway {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = []config.ModelBinding{
		{Canonical: "m", Channel: model.ChannelWorkBuddy, UpstreamModel: "m"},
		{Canonical: "m", Channel: model.ChannelTrae, UpstreamModel: "m"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second},
		map[string]int{model.ChannelWorkBuddy: 1, model.ChannelTrae: 1},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	p.Upsert(&model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy, CredsJSON: []byte(`{}`)})
	p.Upsert(&model.Account{ID: "tr-1", Channel: model.ChannelTrae, CredsJSON: []byte(`{}`)})
	return New(cfg, p, router.New(cfg), map[string]driver.Driver{
		model.ChannelWorkBuddy: drv, model.ChannelTrae: drv,
	}, st)
}

func bufferedToolRequest() *ir.ChatRequest {
	return &ir.ChatRequest{
		Model: "m", Stream: true, MaxTokens: 100,
		// The Anthropic/Gemini ingress shape: tool deltas never reach the client.
		ToolDeltasBuffered: true,
		Messages:           []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
}

// TestToolOnlyDropCascadesOnBufferedProtocols: the core BP-123 behaviour.
func TestToolOnlyDropCascadesOnBufferedProtocols(t *testing.T) {
	drv := &toolDropDriver{}
	g := newGatewayWithTwoBindings(t, drv)
	var text strings.Builder
	_, err := g.Execute(context.Background(), bufferedToolRequest(), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
			text.WriteString(ev.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Execute: %v (a tool-only drop on a buffered protocol must be retried — account rotation or cascade — not fail the turn)", err)
	}
	if text.String() != "recovered" {
		t.Fatalf("client saw %q, want the retry's answer", text.String())
	}
	// Either remedy is a win: another account of the same channel, or the
	// next binding. The field case died with attempts=1 and nothing. The
	// driver answers on its second call, so SOME retry must have happened.
	if drv.calls < 2 {
		t.Fatalf("driver called %d times, want >=2 (the dead first attempt must be retried)", drv.calls)
	}
}

// TestToolOnlyDropStillFailsOnStreamingProtocols guards the other direction:
// OpenAI-shaped ingress streams tool deltas immediately, so the fragments DID
// reach the client and a retry would duplicate them — the error must surface.
func TestToolOnlyDropStillFailsOnStreamingProtocols(t *testing.T) {
	drv := &toolDropDriver{}
	g := newGatewayWithTwoBindings(t, drv)
	req := bufferedToolRequest()
	req.ToolDeltasBuffered = false // OpenAI chat / Responses shape
	_, err := g.Execute(context.Background(), req, func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("a tool-only drop on a streaming protocol must fail the request (a retry would duplicate the fragments)")
	}
}

// TestResetReachesRetryAttempt: the reset event must flow to the next
// attempt's emitter chain.
func TestResetReachesRetryAttempt(t *testing.T) {
	var mu sync.Mutex
	resetSeen := 0
	drv := &resetRecordingDriver{onEvent: func(k ir.StreamEventKind) {
		if k == ir.EvAttemptReset {
			mu.Lock()
			resetSeen++
			mu.Unlock()
		}
	}}
	g := newGatewayWithTwoBindings(t, drv)
	// The reset flows DOWNSTREAM from the gateway (sentinel guard -> ledger ->
	// server emit), so it is observed at the Execute emitter, not inside a
	// driver — drivers only ever see what they themselves emit.
	if _, err := g.Execute(context.Background(), bufferedToolRequest(), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvAttemptReset {
			mu.Lock()
			resetSeen++
			mu.Unlock()
		}
		return nil
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if resetSeen == 0 {
		t.Fatal("the retry attempt never received EvAttemptReset")
	}
}

// resetRecordingDriver wraps the same first-call-dies shape but watches every
// event that flows through its emitter.
type resetRecordingDriver struct {
	onEvent func(ir.StreamEventKind)
	calls   int
}

func (d *resetRecordingDriver) Channel() string                          { return model.ChannelWorkBuddy }
func (d *resetRecordingDriver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }
func (d *resetRecordingDriver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.calls++
	wrapped := func(ev ir.StreamEvent) error {
		d.onEvent(ev.Kind)
		return emit(ev)
	}
	if d.calls == 1 {
		_ = wrapped(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0,
			ToolCall: &ir.ToolCall{ID: "call_dead", Name: "Bash"}})
		return nil, uperr.New(uperr.ClassTransport, 0, "stream read: unexpected EOF")
	}
	_ = wrapped(ir.StreamEvent{Kind: ir.EvText, Delta: "ok"})
	_ = wrapped(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "ok"}}},
		StopReason: ir.StopEnd,
	}, nil
}

// TestLedgerClearsToolFlagOnReset: a tool-fragment death must not permanently
// disqualify the continuation for a later attempt that streams prose.
func TestLedgerClearsToolFlagOnReset(t *testing.T) {
	l := &streamLedger{}
	l.record(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0,
		ToolCall: &ir.ToolCall{ID: "call_dead", Name: "Bash"}})
	if !l.sawTool {
		t.Fatal("precondition: tool event sets the flag")
	}
	l.record(ir.StreamEvent{Kind: ir.EvAttemptReset})
	if l.sawTool {
		t.Fatal("reset must clear the tool flag (it belongs to the discarded attempt)")
	}
	// The continuation anchors on prose: a retry that streamed text and then
	// dropped must be continuable, and the PREVIOUS attempt's tool death must
	// not block it (that is exactly what the reset fixes).
	l.record(ir.StreamEvent{Kind: ir.EvText, Delta: "partial answer"})
	if !l.canContinue(string(uperr.ClassTransport), 0) {
		t.Fatal("after a reset, a later attempt's text must anchor a continuation despite the earlier tool death")
	}
}

// TestSentinelDropsBufferedTextOnReset: text the guard was still matching must
// be discarded by a reset, not flushed — otherwise a retried stream would open
// with the dead attempt's words and then the retry's own.
func TestSentinelDropsBufferedTextOnReset(t *testing.T) {
	var delivered strings.Builder
	guard := sentinelGuard(func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
			delivered.WriteString(ev.Delta)
		}
		return nil
	}, []string{"REJECTED"})
	// Attempt 1: a short opening line (below the 128-char window) then death.
	if err := guard(ir.StreamEvent{Kind: ir.EvText, Delta: "hi"}); err != nil {
		t.Fatalf("attempt-1 text: %v", err)
	}
	if delivered.Len() != 0 {
		t.Fatalf("precondition: buffered text must not be delivered yet, got %q", delivered.String())
	}
	// Retry: the reset must DROP the dead attempt's text.
	if err := guard(ir.StreamEvent{Kind: ir.EvAttemptReset}); err != nil {
		t.Fatalf("reset: %v", err)
	}
	// Attempt 2 streams its own text — which the guard buffers AGAIN for
	// sentinel matching (correct: the retry deserves the same check), so the
	// turn must end for it to flush.
	if err := guard(ir.StreamEvent{Kind: ir.EvText, Delta: "fresh answer"}); err != nil {
		t.Fatalf("attempt-2 text: %v", err)
	}
	if err := guard(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd}); err != nil {
		t.Fatalf("attempt-2 done: %v", err)
	}
	if delivered.String() != "fresh answer" {
		t.Fatalf("client saw %q, want only the retry's text (the dead attempt's opening must be dropped)", delivered.String())
	}
}
