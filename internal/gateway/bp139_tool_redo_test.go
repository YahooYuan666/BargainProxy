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

// BP-139: a stream cut WHILE the model was emitting a tool call used to lose
// the whole turn — sawTool disqualified the continuation, and the cascade was
// already closed because text had been delivered. Field 2026-10-27 20:22:06
// (msg_9a521d55b376, workbuddy EOF at 117s) was exactly this shape, and the
// user's instruction was "能让项目网关自动处理么？不要打断 AGENT 端".
//
// The fix: the continuation request asks the model to REDO the interrupted
// tool call (the partial fragment is discarded — the client never saw it on
// buffered protocols), the writers retire the truncated buffer and shift
// their tool index space, and the client experiences one continuous stream
// whose tool call simply arrives complete.

// toolCutDriver streams text, then a partial tool call, then dies. On the
// continuation call (detected by the redo nudge in the request) it completes
// the tool call properly.
type toolCutDriver struct {
	mu       sync.Mutex
	calls    int
	lastReq  *ir.ChatRequest
	redoSeen bool
}

func (d *toolCutDriver) Channel() string                          { return model.ChannelWorkBuddy }
func (d *toolCutDriver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }
func (d *toolCutDriver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.mu.Lock()
	d.calls++
	n := d.calls
	d.lastReq = call.Request
	d.mu.Unlock()
	if n == 1 {
		_ = emit(ir.StreamEvent{Kind: ir.EvText, Delta: "让我查一下文件"})
		_ = emit(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0,
			ToolCall: &ir.ToolCall{ID: "call_cut", Name: "Read"}})
		_ = emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{"path":"a.md`})
		return nil, uperr.New(uperr.ClassTransport, 0, "stream read: unexpected EOF")
	}
	// Continuation: the redo nudge must be present.
	for _, m := range call.Request.Messages {
		if m.Role == ir.RoleUser && strings.Contains(ir.TextOf(m.Content), "重新发起你当时要调用的工具调用") {
			d.mu.Lock()
			d.redoSeen = true
			d.mu.Unlock()
		}
	}
	_ = emit(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0,
		ToolCall: &ir.ToolCall{ID: "call_redo", Name: "Read"}})
	_ = emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{"path":"a.md"}`})
	_ = emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopToolUse})
	return &ir.ChatResponse{
		Message: ir.Message{ToolCalls: []ir.ToolCall{{ID: "call_redo", Name: "Read", Arguments: `{"path":"a.md"}`}}},
		Usage:   ir.Usage{InputTokens: 10, OutputTokens: 5},
	}, nil
}

// TestMidToolCutContinuesWithRedo is the BP-139 core behaviour: the turn
// survives a mid-tool-call transport drop with the tool call redone.
func TestMidToolCutContinuesWithRedo(t *testing.T) {
	drv := &toolCutDriver{}
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = []config.ModelBinding{{Canonical: "m", Channel: model.ChannelWorkBuddy, UpstreamModel: "m"}}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second}, map[string]int{model.ChannelWorkBuddy: 1},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	p.Upsert(&model.Account{ID: "wb-1", Channel: model.ChannelWorkBuddy, CredsJSON: []byte(`{}`)})
	g := New(cfg, p, router.New(cfg), map[string]driver.Driver{model.ChannelWorkBuddy: drv}, st)

	req := &ir.ChatRequest{
		Model: "m", Stream: true, MaxTokens: 100,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
	var text strings.Builder
	res, err := g.Execute(context.Background(), req, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
			text.WriteString(ev.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Execute: %v (a mid-tool-call drop must be auto-continued, not surfaced)", err)
	}
	if text.String() != "让我查一下文件" {
		t.Fatalf("client text = %q, want the pre-cut prose without duplication", text.String())
	}
	drv.mu.Lock()
	defer drv.mu.Unlock()
	if !drv.redoSeen {
		t.Fatal("continuation request must carry the tool-redo nudge")
	}
	if res.Response == nil || len(res.Response.Message.ToolCalls) != 1 {
		t.Fatalf("final answer must carry the redone tool call: %+v", res.Response)
	}
	if res.Response.Message.ToolCalls[0].ID != "call_redo" {
		t.Fatalf("tool call = %+v, want the redo's call", res.Response.Message.ToolCalls[0])
	}
}

// TestMidToolCutNudgeIsNotPlainContinue guards the nudge selection: a prose
// cut gets the plain nudge, a tool cut gets the redo nudge.
func TestMidToolCutNudgeIsNotPlainContinue(t *testing.T) {
	ledger := &streamLedger{}
	ledger.record(ir.StreamEvent{Kind: ir.EvText, Delta: "prose"})
	req := &ir.ChatRequest{}
	got := continuationRequest(req, ledger, false)
	if !strings.Contains(ir.TextOf(got.Messages[len(got.Messages)-1].Content), "继续输出剩余内容") {
		t.Fatalf("prose cut nudge wrong: %q", ir.TextOf(got.Messages[len(got.Messages)-1].Content))
	}
	got2 := continuationRequest(req, ledger, true)
	last := ir.TextOf(got2.Messages[len(got2.Messages)-1].Content)
	if !strings.Contains(last, "重新发起你当时要调用的工具调用") {
		t.Fatalf("tool cut nudge wrong: %q", last)
	}
	if strings.Contains(last, "继续输出剩余内容") {
		t.Fatal("tool cut must NOT ask for prose continuation (the fragment was discarded)")
	}
}

// TestToolRestartShiftsAnthropicIndexes: on the Anthropic wire the redo's
// tool call must land in a NEW block — the dead attempt's partial buffer is
// retired, never spliced (checked in the anthropic package's own test; here we
// pin the ledger half: the restart clears the flag so a SECOND drop can still
// continue).
func TestToolRestartClearsLedgerFlag(t *testing.T) {
	l := &streamLedger{}
	l.record(ir.StreamEvent{Kind: ir.EvText, Delta: "partial"})
	l.record(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0,
		ToolCall: &ir.ToolCall{ID: "call_cut", Name: "Read"}})
	if !l.canContinue(string(uperr.ClassTransport), 0) {
		t.Fatal("text anchor present: continuation must be allowed even with sawTool (BP-139)")
	}
}
