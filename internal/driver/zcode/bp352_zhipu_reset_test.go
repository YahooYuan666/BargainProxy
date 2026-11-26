// BP-352 (magpie 82127a85, #3013): Zhipu's 1308/1310 name when the window
// reopens; resting past that moment refuses an account Zhipu has already
// released. 1302 (per-minute) must stay a momentary cap.
package zcode

import (
	"testing"
	"time"

	"bargainproxy/internal/uperr"
)

// The reporter's own sentence: 1308 through the Anthropic endpoint.
func TestBP352ParsesChineseResetSentence(t *testing.T) {
	now := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	msg := "[1308][已达到 5 小时的使用上限。您的限额将在 2026-11-10 16:01:04 重置。]"
	at, ok := zhipuResetAt(msg, now)
	if !ok {
		t.Fatalf("reset moment not parsed from %q", msg)
	}
	// 16:01:04 UTC+8 == 08:01:04 UTC — 61 seconds after `now`.
	want := time.Date(2026, 11, 10, 8, 1, 4, 0, time.UTC)
	if !at.Equal(want) {
		t.Fatalf("parsed %s, want %s (the sentence carries NO zone; it is Beijing's)", at.UTC(), want)
	}
}

// The English form on the OpenAI-shaped surfaces.
func TestBP352ParsesEnglishResetSentence(t *testing.T) {
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	at, ok := zhipuResetAt("Usage limit reached for 5 hour. Your limit will reset at 2026-11-10 16:01:04", now)
	if !ok {
		t.Fatal("english reset moment not parsed")
	}
	if got := at.UTC(); !got.Equal(time.Date(2026, 11, 10, 8, 1, 4, 0, time.UTC)) {
		t.Fatalf("parsed %s", got)
	}
}

// A stale sentence is worse than the fallback: it would rest the account on a
// moment that has already passed.
func TestBP352RejectsPastAndMalformedMoments(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) // 20:00 Beijing
	for _, msg := range []string{
		"您的限额将在 2026-11-10 16:01:04 重置。",   // already past
		"您的限额将在 2026-02-30 16:01:04 重置。",   // not a real date
		"已达到 5 小时的使用上限。",             // no moment at all
		"[1310][已达到每周使用上限。" ,           // ditto
	} {
		if _, ok := zhipuResetAt(msg, now); ok {
			t.Fatalf("accepted an unusable moment from %q", msg)
		}
	}
}

// The fallback is the window's own length, not a flat 6 hours.
func TestBP352WindowFallbackPerCode(t *testing.T) {
	if d := zhipuWindowFallback(1308); d != 5*time.Hour {
		t.Fatalf("1308 fallback = %v, want 5h", d)
	}
	if d := zhipuWindowFallback(1310); d != 7*24*time.Hour {
		t.Fatalf("1310 fallback = %v, want 7d", d)
	}
}

// The classification itself: a named moment becomes a HARD ResetAt.
func TestBP352Classify1308UsesNamedMoment(t *testing.T) {
	msg := "[1308][已达到 5 小时的使用上限。您的限额将在 2099-01-02 03:04:05 重置。]"
	e := classifyBusiness(1308, msg, "glm-5.4")
	if e == nil || e.Class != uperr.ClassSoftRate {
		t.Fatalf("class = %v", e)
	}
	if !e.ResetHard {
		t.Fatal("a vendor-stated moment must be a hard window")
	}
	want := time.Date(2099, 1, 2, 3, 4, 5, 0, zhipuZone)
	if !e.ResetAt.Equal(want) {
		t.Fatalf("ResetAt = %s, want %s", e.ResetAt, want)
	}
}

// No usable moment → the window length, not 6h.
func TestBP352Classify1308FallsBackToWindow(t *testing.T) {
	before := time.Now()
	e := classifyBusiness(1308, "[1308][已达到 5 小时的使用上限]", "glm-5.4")
	after := time.Now()
	if e.ResetHard {
		t.Fatal("a fallback is our estimate, not a hard window")
	}
	lo, hi := before.Add(5*time.Hour), after.Add(5*time.Hour)
	if e.ResetAt.Before(lo) || e.ResetAt.After(hi) {
		t.Fatalf("ResetAt = %s, want ≈now+5h", e.ResetAt)
	}
}

// 1302 is a per-minute cap: a minute, same account, no strike.
func TestBP352Classify1302IsPerMinute(t *testing.T) {
	before := time.Now()
	e := classifyBusiness(1302, "[1302] 速率限制", "glm-5.4")
	after := time.Now()
	if e.Class != uperr.ClassSoftRate {
		t.Fatalf("class = %v, want soft_rate (a 1302 used to fall to ClassInternal "+
			"and charge the account a strike)", e.Class)
	}
	lo, hi := before.Add(time.Minute), after.Add(time.Minute)
	if e.ResetAt.Before(lo) || e.ResetAt.After(hi) {
		t.Fatalf("ResetAt = %s, want ≈now+1m", e.ResetAt)
	}
}

// THE test magpie pinned: 「每分钟使用上限」 contains 「使用上限」, so a naive
// "使用上限 ⇒ 5-hour window" rule would turn a one-minute blip into hours of
// downtime. The minute branch must win.
func TestBP352MinuteWordingWinsOverWindowWording(t *testing.T) {
	before := time.Now()
	e := classifyBusiness(1308, "[1308] 每分钟使用上限已达到，请稍后再试", "glm-5.4")
	after := time.Now()
	lo, hi := before.Add(time.Minute), after.Add(time.Minute)
	if e.ResetAt.Before(lo) || e.ResetAt.After(hi) {
		t.Fatalf("ResetAt = %s — a per-minute refusal was rested as a window", e.ResetAt)
	}
}

// A plain window refusal must NOT be swallowed by the minute branch.
func TestBP352WindowWordingStaysWindow(t *testing.T) {
	e := classifyBusiness(1308, "[1308][已达到 5 小时的使用上限。您的限额将在 2099-01-02 03:04:05 重置。]", "glm-5.4")
	if !e.ResetHard {
		t.Fatal("a 5-hour window refusal was read as a rate limit")
	}
}
