package workbuddy

// BP-327: 11134 「模型供应商暂时不可用」是新业务码（misakano c06cd51 +
// RobbsLuo d273491，2026-11-14）。它与 6020/6021 同属容量族，处置必须一致
// （不轮转、不罚号），但成因不同：整个供应商过载 vs 队列走得慢。
//
// 修复前的行为（本用例要钉住的正是这段）：11134 落进 classifyBusiness 的
// default 分支，最终成 ClassInternal —— pool.ApplyResult 的 default 分支会给账号
// ConsecutiveFails++，而 uperr.Rotatable 对 ClassInternal 返回 true，于是网关会在
// 同渠道内换号重试。上游实测一次供应商过载 15 分钟命中 6 个号，换号纯属白烧。

import (
	"testing"

	"bargainproxy/internal/uperr"
)

// 结构化 11134 必须归 ClassUpstreamBusy（容量族），且带上 model 供上层换模型建议。
func TestBP327ProviderOverloadedIsUpstreamBusy(t *testing.T) {
	e := classifyBusiness(11134, "the model provider is temporarily unavailable", "deepseek-v4.1-flash")
	if e == nil {
		t.Fatal("11134 must classify, got nil")
	}
	if e.Class != uperr.ClassUpstreamBusy {
		t.Fatalf("11134 class = %q, want %q (capacity family: no rotation, no account penalty)",
			e.Class, uperr.ClassUpstreamBusy)
	}
	if e.Model != "deepseek-v4.1-flash" {
		t.Fatalf("11134 must name the model so the caller can advise switching, got %q", e.Model)
	}
	// Rotatable must be false: rotating accounts inside one channel is the
	// exact action the overload punishes.
	if uperr.Rotatable(e.Class) {
		t.Fatalf("11134 must NOT be rotatable within the channel")
	}
	if e.Code != "11134" {
		t.Fatalf("11134 must carry its business code for the renderer, got %q", e.Code)
	}
}

// 队列族的处置不能被这次改动动摇：6020/6021 仍走「队列满」的路径（不设 Code，
// 否则上层的 11134 文案分支会把它们也当成供应商过载）。
func TestBP327QueueFamilyUnchanged(t *testing.T) {
	for _, code := range []int{6020, 6021} {
		e := classifyBusiness(code, "queue.waiting.title", "deepseek-v4.1-flash")
		if e.Class != uperr.ClassUpstreamBusy {
			t.Fatalf("%d class = %q, want upstream_busy", code, e.Class)
		}
		if e.Code == "11134" {
			t.Fatalf("%d must not be tagged as provider-overloaded", code)
		}
	}
}

// 默认分支仍然把未识别的码归 ClassInternal —— 这次只加一个码，不放宽兜底。
func TestBP327UnknownCodeStillInternal(t *testing.T) {
	e := classifyBusiness(99999, "something we have never seen", "m")
	if e.Class != uperr.ClassInternal {
		t.Fatalf("unknown code class = %q, want internal (unchanged)", e.Class)
	}
}
