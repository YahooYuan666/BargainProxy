// ticket 29: throughput denominator (generation window) unit tests.
package panel

import (
	"testing"

	"bargainproxy/internal/model"
)

// TestGenMsOf 钉住吐字速率分母口径（agentpoints #34/#127 对齐）：
// 扣除首 token 等待，但只在有 TTFB 观测且余量 ≥200ms 时才扣；否则退回端到端。
func TestGenMsOf(t *testing.T) {
	cases := []struct {
		name string
		l    model.RequestLog
		want int64
	}{
		{"无 TTFB 观测（非流式上游路径）→ 端到端", model.RequestLog{DurationMs: 1000}, 1000},
		{"正常扣除首 token 等待", model.RequestLog{DurationMs: 1000, TTFBMs: 400}, 600},
		{"扣除后恰好 200ms → 照常扣除", model.RequestLog{DurationMs: 1000, TTFBMs: 800}, 200},
		{"扣除后不足 200ms（假流式/攒批）→ 退回端到端", model.RequestLog{DurationMs: 5000, TTFBMs: 4950}, 5000},
		{"TTFB 超总耗时（时钟粒度）→ 端到端，不得负/零分母", model.RequestLog{DurationMs: 500, TTFBMs: 900}, 500},
		{"零耗时", model.RequestLog{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := genMsOf(c.l); got != c.want {
				t.Fatalf("genMsOf(%+v) = %d, want %d", c.l, got, c.want)
			}
		})
	}
}
