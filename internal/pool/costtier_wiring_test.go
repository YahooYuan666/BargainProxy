package pool

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
)

// Qoder-1 wiring tests (BP-98): the gateway feeds Qoder's per-request
// credits into RecordCostTier (0 → tier 0 free, >0 → tier 2 paid). These
// lock the pool-side semantics that wiring relies on.

func TestRecordCostTierFreeThenPaid(t *testing.T) {
	p := New(Params{}, map[string]int{}, nil)
	acc := &model.Account{ID: "qoder:t", Channel: model.ChannelQoder}
	p.Upsert(acc)
	p.RecordCostTier(acc, "Qwen3.8-Flash", 0)
	if got := p.CostTier(acc, "Qwen3.8-Flash"); got != 0 {
		t.Fatalf("0 credits must record tier 0 (free), got %d", got)
	}
	p.RecordCostTier(acc, "GLM-5.3", 2)
	if got := p.CostTier(acc, "GLM-5.3"); got != 2 {
		t.Fatalf("paid credits must record tier 2, got %d", got)
	}
}

func TestCostTierUnknownReadsOne(t *testing.T) {
	p := New(Params{}, map[string]int{}, nil)
	acc := &model.Account{ID: "qoder:t", Channel: model.ChannelQoder}
	p.Upsert(acc)
	if got := p.CostTier(acc, "never-seen"); got != 1 {
		t.Fatalf("unknown model must read tier 1 (unknown), got %d", got)
	}
}

func TestCostTierExpires(t *testing.T) {
	p := New(Params{CostTTL: 10 * time.Millisecond}, map[string]int{}, nil)
	acc := &model.Account{ID: "qoder:t", Channel: model.ChannelQoder}
	p.Upsert(acc)
	p.RecordCostTier(acc, "GLM-5.3", 2)
	time.Sleep(30 * time.Millisecond)
	if got := p.CostTier(acc, "GLM-5.3"); got != 1 {
		t.Fatalf("expired observation must fall back to tier 1, got %d", got)
	}
}
