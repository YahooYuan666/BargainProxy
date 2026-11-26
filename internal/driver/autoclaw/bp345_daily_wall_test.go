package autoclaw

import (
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/pool"
	"bargainproxy/internal/uperr"
)

// BP-345: the 810000 wall is a DAILY allowance, so the account's model rests
// until the next 04:00 Shanghai — not for the pool's 60-second probe.
//
// Field 2026-11-25 22:30–22:45: account 18191367452 ran GLM-5.4 156 times
// (~33M input tokens) and the gateway then answered 810000 on every call. The
// driver reported the wall with no reset moment, so the pool held the model
// for 60s (the BP-314 short probe, meant for a one-minute capacity wobble),
// expired, dispatched the same walled account again, and re-hit the wall —
// four times in fifteen minutes, each one ending a conversation's turn with
// "free allowance used up". The same account served GLM-5.3-Flash throughout
// (185 calls, billed from the very wallet that had paid for GLM-5.4 minutes
// earlier), so the points were never the problem.
//
// The user's ruling on the cadence: the allowance is counted per day.
func TestAllowanceWallRestsTheModelUntilNext4AM(t *testing.T) {
	e := classifyHTTPFor(402, []byte(`{"code":810000,"message":"GLM-5.4 免费额度已用完,开通连续包月会员继续使用"}`), "zai_glm-x-preview")
	if e.Class != uperr.ClassHardCredit || e.Model != "zai_glm-x-preview" {
		t.Fatalf("class=%s model=%q; the wall must stay model-scoped", e.Class, e.Model)
	}
	if e.ResetAt.IsZero() {
		t.Fatal("the wall carries no reset moment: the pool would fall back to its 60s probe and re-hit it every minute")
	}
	now := time.Now()
	if want := pool.NextDay4AM(now); !e.ResetAt.Equal(want) {
		t.Fatalf("ResetAt = %s, want next 04:00 Shanghai %s", e.ResetAt, want)
	}
	if !e.ResetHard {
		t.Fatal("ResetHard must be set: the moment is the allowance's own boundary and must not be capped or shortened")
	}
	// The other codes keep their meaning: a throttle is not a daily wall.
	if b := classifyHTTPFor(429, []byte(`{"code":810001}`), "zai_glm-5.3-flash"); b.Class != uperr.ClassSoftRate || !b.ResetAt.IsZero() {
		t.Fatalf("810001 class=%s reset=%s; a transient throttle must not rest until 04:00", b.Class, b.ResetAt)
	}
}

// The wall must not end the conversation: the message says what happened and
// what the gateway did about it, in the operator's terms. The upstream wording
// ("free allowance used up, subscribe to continue") was relayed verbatim and
// read as "my points are gone" — on an account holding 9101 points.
func TestAllowanceWallMessageNamesTheDailyLimit(t *testing.T) {
	e := classifyHTTPFor(402, []byte(`{"code":810000,"message":"GLM-5.4 免费额度已用完,开通连续包月会员继续使用"}`), "zai_glm-x-preview")
	msg := e.Message
	if !strings.Contains(msg, "今日") || !strings.Contains(msg, "积分") {
		t.Fatalf("message does not say it is a daily limit and that points are intact: %q", msg)
	}
	if strings.Contains(msg, "开通连续包月") {
		t.Fatalf("message relays the upstream sales pitch: %q", msg)
	}
	// A 402 with no code is a real empty balance and keeps its own wording.
	if b := classifyHTTPFor(402, []byte(`{"message":"积分不足"}`), "zai_glm-5.3-flash"); strings.Contains(b.Message, "今日") {
		t.Fatalf("a wording-only balance error must not be called a daily limit: %q", b.Message)
	}
}
