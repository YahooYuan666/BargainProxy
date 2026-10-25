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
)

type starvedDriver struct {
	mu    sync.Mutex
	calls int
}

func (d *starvedDriver) Channel() string                          { return model.ChannelWorkBuddy }
func (d *starvedDriver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }
func (d *starvedDriver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.mu.Lock()
	d.calls++
	n := d.calls
	d.mu.Unlock()
	// 只对真的关了思考的重发给正文
	if call.Request.Thinking != nil && !call.Request.Thinking.Enabled {
		if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: "rescued"}); err != nil {
			return nil, err
		}
		if err := emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd}); err != nil {
			return nil, err
		}
		return &ir.ChatResponse{
			Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "rescued"}}},
			StopReason: ir.StopEnd,
		}, nil
	}
	// 第一次：思考吃光预算——注意我们不发 thinking 事件（客户端什么都看不到）
	_ = n
	if err := emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopLength}); err != nil {
		return nil, err
	}
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: ""}}},
		StopReason: ir.StopLength,
	}, nil
}

func TestStarvedRescueEndToEnd(t *testing.T) {
	drv := &starvedDriver{}
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = []config.ModelBinding{{Canonical: "m", Channel: model.ChannelWorkBuddy, UpstreamModel: "m"}}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second}, map[string]int{model.ChannelWorkBuddy: 1},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	p.Upsert(&model.Account{ID: "acct-1", Channel: model.ChannelWorkBuddy, CredsJSON: []byte(`{}`)})
	g := New(cfg, p, router.New(cfg), map[string]driver.Driver{model.ChannelWorkBuddy: drv}, st)

	req := &ir.ChatRequest{
		Model:     "m",
		Stream:    true,
		MaxTokens: 100,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
	var text strings.Builder
	res, err := g.Execute(context.Background(), req, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
			text.WriteString(ev.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if text.String() != "rescued" || res.Response.StopReason != ir.StopEnd {
		t.Fatalf("client should see the rescued answer, got text=%q stop=%s", text.String(), res.Response.StopReason)
	}
	if drv.calls != 2 {
		t.Fatalf("driver calls = %d, want 2 (original + rescue)", drv.calls)
	}
}
