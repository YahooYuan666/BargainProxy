package gateway

// BP-327：续写/救援的分段记账（misakano def074c / ab67974）。
//
// 两本账，各管一件事：
//   - usage（客户端可见）：prompt 侧取**末段**值。续写每段都会把整段上下文重发，
//     逐段求和会把客户端的上下文占用虚报成 N 倍，导致它提前触发压缩。
//   - billed（成本）：每段都是单独计费的一次上游调用，**被丢弃的段也算钱**
//     （续写失败、attempt 重置），漏记就是少报支出。
//
// 修复前的形态：只有 usage 一本账，且段落边界上不做任何提交——续写失败那段
// 的 usage 随 resp 一起丢弃，日志行记 0。

import (
	"testing"

	"bargainproxy/internal/ir"
)

// 单段：两本账相等。
func TestBP327LedgerSingleSegment(t *testing.T) {
	l := &streamLedger{}
	l.addUsage(ir.Usage{InputTokens: 1000, OutputTokens: 200, CacheKnown: true, CachedInputTokens: 800})
	if got := l.usage; got.InputTokens != 1000 || got.OutputTokens != 200 {
		t.Fatalf("client book = %+v", got)
	}
	if got := l.billingUsage(); got.InputTokens != 1000 || got.OutputTokens != 200 || !got.CacheKnown {
		t.Fatalf("billing book must equal the single segment: %+v", got)
	}
}

// 两段成功：客户端 book 的 prompt 取末段，billing book 逐段求和。
func TestBP327LedgerTwoSegmentsDivergePromptOnly(t *testing.T) {
	l := &streamLedger{}
	l.addUsage(ir.Usage{InputTokens: 1000, OutputTokens: 200})
	l.commitSegment()
	l.addUsage(ir.Usage{InputTokens: 1500, OutputTokens: 300})

	if l.usage.InputTokens != 1500 {
		t.Fatalf("client prompt must be the LAST segment (1500), got %d", l.usage.InputTokens)
	}
	if l.usage.OutputTokens != 500 {
		t.Fatalf("client output accumulates across segments (500), got %d", l.usage.OutputTokens)
	}
	billed := l.billingUsage()
	if billed.InputTokens != 2500 {
		t.Fatalf("billing prompt must SUM segments (1000+1500=2500), got %d", billed.InputTokens)
	}
	if billed.OutputTokens != 500 {
		t.Fatalf("billing output = %d, want 500", billed.OutputTokens)
	}
}

// 被丢弃的段（attempt reset）仍然计费 —— 这是本条最要紧的断言。
//
// 关键在于**边界提交**：reset 到达时必须先把该段落进 billed 再清空。若只在读取
// 时把「当前段」补进去，两腿的 usage 会挤在同一个 seg 里，下一腿的 usage 会覆盖
// 而不是追加（真实驱动的 usage 帧是覆盖式上报，同一段内不会重复累加）。
func TestBP327DiscardedSegmentStillBilled(t *testing.T) {
	l := &streamLedger{}
	// 第一腿跑了，报了 usage，然后被 attempt reset 丢弃（该段输出不会给客户端）。
	l.addUsage(ir.Usage{InputTokens: 900, OutputTokens: 120, Credit: 3, CreditKnown: true})
	l.record(ir.StreamEvent{Kind: ir.EvAttemptReset})
	// 边界提交必须已经发生：此刻 billed 就是第一腿的账，seg 必须已清空。
	if l.billed.InputTokens != 900 || l.billed.OutputTokens != 120 {
		t.Fatalf("reset must commit the discarded segment to the billing book: billed=%+v", l.billed)
	}
	if l.segUsage {
		t.Fatalf("reset must clear the segment accumulator so the next leg cannot inherit it")
	}
	// 第二腿正常答完。
	l.addUsage(ir.Usage{InputTokens: 1000, OutputTokens: 80, Credit: 2, CreditKnown: true})

	if l.usage.InputTokens != 1000 {
		t.Fatalf("client book prompt = %d, want the live segment only (1000)", l.usage.InputTokens)
	}
	billed := l.billingUsage()
	if billed.InputTokens != 1900 {
		t.Fatalf("a discarded leg still cost its prompt: want 1900, got %d", billed.InputTokens)
	}
	if billed.OutputTokens != 200 {
		t.Fatalf("a discarded leg's output is still billed: want 200, got %d", billed.OutputTokens)
	}
	if billed.Credit != 5 || !billed.CreditKnown {
		t.Fatalf("credit must sum across legs and stay known: %+v", billed)
	}
}

// 没有 usage 上报时两本账都退化为空（不能凭空造数）。
func TestBP327LedgerNoUsageIsZero(t *testing.T) {
	l := &streamLedger{}
	if got := l.billingUsage(); got.InputTokens != 0 || got.OutputTokens != 0 || got.CreditKnown || got.CacheKnown {
		t.Fatalf("no usage reported must stay zero, got %+v", got)
	}
}
