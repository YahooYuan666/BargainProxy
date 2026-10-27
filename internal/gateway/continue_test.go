// BP-107: a stream that already reached the client and then died must not end
// the turn. Field evidence 2026-10-24: five WorkBuddy requests were killed
// mid-answer (`wsarecv: An existing connection was forcibly closed`) after
// 1–2 minutes of output on 180k-token conversations; the gateway surfaced the
// error, and for an agent client one dropped socket means the whole turn is
// lost even though most of the answer had already arrived.
//
// The fix continues on the SAME account and channel: append what the client
// already saw as an assistant message plus a "carry on" instruction, resend,
// and splice the new events into the same stream. The client sees one
// continuous answer.
package gateway

import (
	"context"
	"errors"
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

// pad makes test text longer than the sentinel guard's buffering window (128
// chars). Below that window the guard has not handed anything to the client, so
// a drop legitimately means "the client saw nothing" and the ordinary cascade —
// not the continuation — is the right remedy. Asserting on short text would
// therefore test the wrong branch.
func pad(s string) string {
	if len(s) >= 160 {
		return s
	}
	return s + strings.Repeat(".", 160-len(s))
}

// scriptedDriver plays a fixed list of segments. Each segment is a function of
// the call it receives, so a test can both script output and assert what the
// continuation request actually carried.
type scriptedDriver struct {
	mu    sync.Mutex
	segs  []func(call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error)
	calls []driver.ChatCall
}

func (d *scriptedDriver) Channel() string { return model.ChannelWorkBuddy }

func (d *scriptedDriver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }

func (d *scriptedDriver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.mu.Lock()
	d.calls = append(d.calls, call)
	idx := len(d.calls) - 1
	d.mu.Unlock()
	if idx >= len(d.segs) {
		return nil, uperr.New(uperr.ClassInternal, 0, "scripted driver: unexpected call %d", idx)
	}
	return d.segs[idx](call, emit)
}

func (d *scriptedDriver) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

func (d *scriptedDriver) call(i int) driver.ChatCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[i]
}

// nudgeCalls counts how many upstream calls carried the continuation
// instruction. This is the robust way to assert "a continuation happened":
// the raw call count also moves with ordinary account rotation inside the
// binding (transport is rotatable by design), which has nothing to do with
// this feature.
func (d *scriptedDriver) nudgeCalls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.calls {
		if c.Request == nil {
			continue
		}
		for _, m := range c.Request.Messages {
			// Either nudge counts (BP-139 added the tool-redo variant).
			if m.Role == ir.RoleUser &&
				(strings.Contains(ir.TextOf(m.Content), continueNudge) ||
					strings.Contains(ir.TextOf(m.Content), continueToolNudge)) {
				n++
				break
			}
		}
	}
	return n
}

// say emits text plus the terminal event, the way a real driver does.
func say(text string) func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error) {
	return func(_ driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
		if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: text}); err != nil {
			return nil, err
		}
		u := ir.Usage{InputTokens: 100, OutputTokens: 10}
		if err := emit(ir.StreamEvent{Kind: ir.EvUsage, Usage: &u}); err != nil {
			return nil, err
		}
		if err := emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd}); err != nil {
			return nil, err
		}
		return &ir.ChatResponse{
			Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: text}}},
			StopReason: ir.StopEnd,
			Usage:      u,
		}, nil
	}
}

// sayThenDrop emits text and then dies the way the field report did.
func sayThenDrop(text string, class string) func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error) {
	return func(_ driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
		if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: text}); err != nil {
			return nil, err
		}
		return nil, uperr.New(class, 0,
			"workbuddy: stream read: read tcp 192.0.2.9:1->203.0.113.7:443: wsarecv: An existing connection was forcibly closed by the remote host.")
	}
}

func newContinueGateway(t *testing.T, drv driver.Driver) *Gateway {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = []config.ModelBinding{
		{Canonical: "m", Channel: model.ChannelWorkBuddy, UpstreamModel: "m"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second}, map[string]int{
		model.ChannelWorkBuddy: 2,
	}, func(acc *model.Account) { _ = st.SaveAccount(acc) })
	p.Upsert(&model.Account{ID: "acct-1", Channel: model.ChannelWorkBuddy, CredsJSON: []byte(`{}`)})
	return New(cfg, p, router.New(cfg), map[string]driver.Driver{
		model.ChannelWorkBuddy: drv,
	}, st)
}

// recorder collects what the CLIENT would have seen.
type recorder struct {
	text   strings.Builder
	events []ir.StreamEvent
}

func (r *recorder) emit(ev ir.StreamEvent) error {
	r.events = append(r.events, ev)
	if ev.Kind == ir.EvText {
		r.text.WriteString(ev.Delta)
	}
	return nil
}

func (r *recorder) doneCount() int {
	n := 0
	for _, ev := range r.events {
		if ev.Kind == ir.EvDone {
			n++
		}
	}
	return n
}

func streamReq() *ir.ChatRequest {
	return &ir.ChatRequest{
		Model:  "m",
		Stream: true,
		Messages: []ir.Message{{
			Role:    ir.RoleUser,
			Content: []ir.ContentPart{{Kind: "text", Text: "write me a long answer"}},
		}},
	}
}

// TestDroppedStreamIsContinuedOnTheSameChannel is the headline behaviour: the
// client gets the rest of the answer instead of an error, and sees exactly one
// terminal event.
func TestDroppedStreamIsContinuedOnTheSameChannel(t *testing.T) {
	first := pad("The first half of the answer.")
	second := " The second half."
	drv := &scriptedDriver{segs: []func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error){
		sayThenDrop(first, uperr.ClassTransport),
		say(second),
	}}
	g := newContinueGateway(t, drv)
	rec := &recorder{}

	res, err := g.Execute(context.Background(), streamReq(), rec.emit)
	if err != nil {
		t.Fatalf("a dropped stream must be continued, not surfaced: %v", err)
	}
	if got, want := rec.text.String(), first+second; got != want {
		t.Fatalf("client saw %q, want %q", got, want)
	}
	if n := rec.doneCount(); n != 1 {
		t.Fatalf("client saw %d terminal events, want exactly 1", n)
	}
	if drv.nudgeCalls() != 1 {
		t.Fatalf("continuation calls = %d, want 1", drv.nudgeCalls())
	}
	// The aggregated response must describe what the client saw, not just the
	// last segment — otherwise a non-stream renderer loses the first half.
	if got := ir.TextOf(res.Response.Message.Content); got != first+second {
		t.Fatalf("aggregated response = %q", got)
	}

	// The continuation request must carry the already-sent text plus an
	// instruction to carry on, and must be aimed at the same upstream model and
	// the same account.
	cont := drv.call(1)
	msgs := cont.Request.Messages
	if len(msgs) != len(streamReq().Messages)+2 {
		t.Fatalf("continuation carried %d messages, want original+2", len(msgs))
	}
	assistant := msgs[len(msgs)-2]
	if assistant.Role != ir.RoleAssistant {
		t.Fatalf("second-to-last message role = %q, want assistant", assistant.Role)
	}
	if got := ir.TextOf(assistant.Content); got != first {
		t.Fatalf("assistant replay = %q, want exactly what the client already saw", got)
	}
	nudge := msgs[len(msgs)-1]
	if nudge.Role != ir.RoleUser || !strings.Contains(ir.TextOf(nudge.Content), continueNudge) {
		t.Fatalf("last message must be the continuation nudge, got %+v", nudge)
	}
	if cont.UpstreamModel != "m" {
		t.Fatalf("continuation must stay on the same upstream model, got %q", cont.UpstreamModel)
	}
	if cont.Account == nil || cont.Account.ID != "acct-1" {
		t.Fatalf("continuation must reuse the same account, got %+v", cont.Account)
	}
}

// TestTruncatedToolArgumentsAreRedone (BP-139 supersedes the old refusal):
// half a JSON arguments blob cannot be spliced, but it does not have to be —
// the client never saw the fragment (buffered protocols), so the gateway
// discards it and the continuation asks the model to REDO the call from
// scratch. The turn survives; the client sees one continuous stream whose
// tool call arrives complete.
//
// The segment deliberately carries BOTH prose and a truncated tool call: that
// is the real shape (the model narrates, then starts a tool).
func TestTruncatedToolArgumentsAreRedone(t *testing.T) {
	drv := &scriptedDriver{segs: []func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error){
		func(_ driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
			if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: pad("Let me write that file.")}); err != nil {
				return nil, err
			}
			if err := emit(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0,
				ToolCall: &ir.ToolCall{ID: "call_1", Name: "write_file"}}); err != nil {
				return nil, err
			}
			if err := emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{"path":"a.txt","content":"half`}); err != nil {
				return nil, err
			}
			return nil, uperr.New(uperr.ClassTransport, 0, "connection reset")
		},
		func(call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
			// The continuation request must carry the REDO nudge, not the
			// prose one.
			found := false
			for _, m := range call.Request.Messages {
				if m.Role == ir.RoleUser && strings.Contains(ir.TextOf(m.Content), "重新发起你当时要调用的工具调用") {
					found = true
				}
			}
			if !found {
				t.Fatal("continuation after a mid-call cut must ask for a tool REDO")
			}
			if err := emit(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0,
				ToolCall: &ir.ToolCall{ID: "call_2", Name: "write_file"}}); err != nil {
				return nil, err
			}
			if err := emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{"path":"a.txt","content":"whole"}`}); err != nil {
				return nil, err
			}
			_ = emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopToolUse})
			return &ir.ChatResponse{
				Message:    ir.Message{ToolCalls: []ir.ToolCall{{ID: "call_2", Name: "write_file", Arguments: `{"path":"a.txt","content":"whole"}`}}},
				StopReason: ir.StopToolUse,
			}, nil
		},
	}}
	g := newContinueGateway(t, drv)
	rec := &recorder{}

	res, err := g.Execute(context.Background(), streamReq(), rec.emit)
	if err != nil {
		t.Fatalf("a mid-call cut must be auto-continued with a redo: %v", err)
	}
	if n := drv.nudgeCalls(); n != 1 {
		t.Fatalf("continuation calls = %d, want 1 (the redo)", n)
	}
	if res.Response == nil || len(res.Response.Message.ToolCalls) != 1 {
		t.Fatalf("final answer must carry the redone call: %+v", res.Response)
	}
	// The truncated fragment must NOT leak into the client stream or the
	// aggregated answer.
	if strings.Contains(rec.text.String(), "half") {
		t.Fatalf("partial fragment leaked to the client: %q", rec.text.String())
	}
	if len(res.Response.Message.ToolCalls) == 1 && strings.Contains(res.Response.Message.ToolCalls[0].Arguments, "half") {
		t.Fatalf("partial fragment leaked into the final tool call: %+v", res.Response.Message.ToolCalls[0])
	}
}

// TestNoTextNoContinuation: if nothing reached the client there is nothing to
// anchor a continuation on; the ordinary cascade must handle it. The cascade
// may still rotate accounts inside the binding (transport is rotatable), so the
// assertion is about the nudge, not the raw call count.
func TestNoTextNoContinuation(t *testing.T) {
	var segs []func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error)
	for i := 0; i < 6; i++ {
		segs = append(segs, func(_ driver.ChatCall, _ driver.Emit) (*ir.ChatResponse, error) {
			return nil, uperr.New(uperr.ClassTransport, 0, "connection reset")
		})
	}
	drv := &scriptedDriver{segs: segs}
	g := newContinueGateway(t, drv)
	rec := &recorder{}
	if _, err := g.Execute(context.Background(), streamReq(), rec.emit); err == nil {
		t.Fatal("want an error")
	}
	if n := drv.nudgeCalls(); n != 0 {
		t.Fatalf("continuation calls = %d, want 0 (nothing was on the wire)", n)
	}
}

// TestContinuationIsBounded: an upstream that keeps dropping must not make the
// gateway loop forever paying for segments.
func TestContinuationIsBounded(t *testing.T) {
	var segs []func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error)
	for i := 0; i < 10; i++ {
		segs = append(segs, sayThenDrop(pad("chunk"), uperr.ClassTransport))
	}
	drv := &scriptedDriver{segs: segs}
	g := newContinueGateway(t, drv)
	rec := &recorder{}
	if _, err := g.Execute(context.Background(), streamReq(), rec.emit); err == nil {
		t.Fatal("want an error after the continuation budget is spent")
	}
	if got := drv.nudgeCalls(); got != continueMaxSegments {
		t.Fatalf("continuation calls = %d, want the %d-segment budget", got, continueMaxSegments)
	}
}

// TestContinuationDoesNotFireForNonStreamingClients: a buffered request has
// nothing on the wire, so the ordinary cross-channel cascade is strictly
// better than re-asking the same account.
func TestContinuationDoesNotFireForNonStreamingClients(t *testing.T) {
	var segs []func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error)
	for i := 0; i < 6; i++ {
		segs = append(segs, func(_ driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
			_ = emit(ir.StreamEvent{Kind: ir.EvText, Delta: pad("partial")})
			return nil, uperr.New(uperr.ClassTransport, 0, "connection reset")
		})
	}
	drv := &scriptedDriver{segs: segs}
	g := newContinueGateway(t, drv)
	req := streamReq()
	req.Stream = false
	if _, err := g.Execute(context.Background(), req, func(ir.StreamEvent) error { return nil }); err == nil {
		t.Fatal("want an error")
	}
	if n := drv.nudgeCalls(); n != 0 {
		t.Fatalf("continuation calls = %d, want 0 (buffered request)", n)
	}
}

// TestContinuationSkipsNonTransportFailures: a content rejection or an auth
// failure will say the same thing again; only a dropped/stalled stream is worth
// resuming.
func TestContinuationSkipsNonTransportFailures(t *testing.T) {
	for _, class := range []string{
		uperr.ClassContentBlocked, uperr.ClassAuth, uperr.ClassBadRequest,
		uperr.ClassModelRate, uperr.ClassTransportDead,
	} {
		drv := &scriptedDriver{segs: []func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error){
			sayThenDrop(pad("partial"), class),
		}}
		g := newContinueGateway(t, drv)
		rec := &recorder{}
		if _, err := g.Execute(context.Background(), streamReq(), rec.emit); err == nil {
			t.Fatalf("class %s: want an error", class)
		}
		if n := drv.nudgeCalls(); n != 0 {
			t.Fatalf("class %s: continuation calls = %d, want 0", class, n)
		}
	}
}

// TestContinuationOfAnIdleStallIsAllowed pins the second eligible class: the
// idle watchdog fires when the upstream stops sending, which is the same
// user-visible failure as a dropped socket.
func TestContinuationOfAnIdleStallIsAllowed(t *testing.T) {
	first := pad("up to the stall")
	drv := &scriptedDriver{segs: []func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error){
		sayThenDrop(first, uperr.ClassUpstreamTimeout),
		say(" and the rest."),
	}}
	g := newContinueGateway(t, drv)
	rec := &recorder{}
	if _, err := g.Execute(context.Background(), streamReq(), rec.emit); err != nil {
		t.Fatalf("an idle stall after output must be continued: %v", err)
	}
	if got, want := rec.text.String(), first+" and the rest."; got != want {
		t.Fatalf("client saw %q, want %q", got, want)
	}
}

// TestContinuationPreservesTheCallerRequest guards the aliasing hazard: the
// gateway clones the request for the continuation, so the caller's message
// slice must be untouched even after several segments. It also pins that each
// continuation replays EVERYTHING already sent, not just the previous segment.
func TestContinuationPreservesTheCallerRequest(t *testing.T) {
	one, two := pad("one"), pad("two")
	drv := &scriptedDriver{segs: []func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error){
		sayThenDrop(one, uperr.ClassTransport),
		sayThenDrop(two, uperr.ClassTransport),
		say("three"),
	}}
	g := newContinueGateway(t, drv)
	req := streamReq()
	rec := &recorder{}
	if _, err := g.Execute(context.Background(), req, rec.emit); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("caller request grew to %d messages; the continuation must clone", len(req.Messages))
	}
	if got, want := rec.text.String(), one+two+"three"; got != want {
		t.Fatalf("client saw %q, want %q", got, want)
	}
	last := drv.call(2).Request.Messages
	assistant := last[len(last)-2]
	if got := ir.TextOf(assistant.Content); got != one+two {
		t.Fatalf("second continuation replayed %q, want the whole accumulated text", got)
	}
}

// TestContinuationErrorClassificationIsUnchangedWhenBudgetExhausted: after the
// budget is spent the client must still get a meaningful class, not a bare
// internal error.
func TestContinuationErrorClassificationIsUnchangedWhenBudgetExhausted(t *testing.T) {
	var segs []func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error)
	for i := 0; i < 5; i++ {
		segs = append(segs, sayThenDrop(pad("x"), uperr.ClassTransport))
	}
	drv := &scriptedDriver{segs: segs}
	g := newContinueGateway(t, drv)
	_, err := g.Execute(context.Background(), streamReq(), (&recorder{}).emit)
	if err == nil {
		t.Fatal("want an error")
	}
	var ue *uperr.Error
	if !errors.As(err, &ue) || ue.Class != uperr.ClassTransport {
		t.Fatalf("error = %v, want class transport", err)
	}
}

// TestContinuationMergesUsage: each segment is a separately billed upstream
// call, so the aggregate must sum them or the cost columns under-report what
// the turn actually spent.
func TestContinuationMergesUsage(t *testing.T) {
	drv := &scriptedDriver{segs: []func(driver.ChatCall, driver.Emit) (*ir.ChatResponse, error){
		func(_ driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
			if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: pad("first")}); err != nil {
				return nil, err
			}
			if err := emit(ir.StreamEvent{Kind: ir.EvUsage, Usage: &ir.Usage{InputTokens: 1000, OutputTokens: 40}}); err != nil {
				return nil, err
			}
			return nil, uperr.New(uperr.ClassTransport, 0, "connection reset")
		},
		func(_ driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
			if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: "second"}); err != nil {
				return nil, err
			}
			u := ir.Usage{InputTokens: 1100, OutputTokens: 60}
			if err := emit(ir.StreamEvent{Kind: ir.EvUsage, Usage: &u}); err != nil {
				return nil, err
			}
			return &ir.ChatResponse{StopReason: ir.StopEnd, Usage: u}, nil
		},
	}}
	g := newContinueGateway(t, drv)
	res, err := g.Execute(context.Background(), streamReq(), (&recorder{}).emit)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := res.Response.Usage.InputTokens, 2100; got != want {
		t.Fatalf("input tokens = %d, want %d (both segments were billed)", got, want)
	}
	if got, want := res.Response.Usage.OutputTokens, 100; got != want {
		t.Fatalf("output tokens = %d, want %d", got, want)
	}
}
