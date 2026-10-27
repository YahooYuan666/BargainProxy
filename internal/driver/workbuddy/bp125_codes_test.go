package workbuddy

import (
	"testing"
	"time"

	"bargainproxy/internal/uperr"
)

// BP-125: 6000 whose message names the escape hatch ("切换其他模型") is a
// model-scoped quota refusal (misakano cdad965) — same treatment as 6004.
func Test6000WithSwitchHintIsModelRate(t *testing.T) {
	e := classifyBusiness(6000, "当前模型调用次数已达上限，请切换其他模型后再试", "glm-5.3")
	if e.Class != uperr.ClassModelRate {
		t.Fatalf("class = %s, want model_rate", e.Class)
	}
	if e.Model != "glm-5.3" {
		t.Fatalf("e.Model = %q, want glm-5.3", e.Model)
	}
	if e.Code != "6000" {
		t.Fatalf("e.Code = %q, want 6000", e.Code)
	}
}

// Without the switch-model hint, 6000 is the account's own quota window:
// account-level rest, not a model hold, not a disable.
func Test6000WithoutHintIsAccountLevel(t *testing.T) {
	e := classifyBusiness(6000, "调用次数已达上限", "glm-5.3")
	if e.Class != uperr.ClassSoftRate {
		t.Fatalf("class = %s, want soft_rate", e.Class)
	}
	if e.Model != "" {
		t.Fatalf("e.Model = %q, want empty", e.Model)
	}
}

// BP-125: 6020 ("queue.waiting.title") is the same capacity event as 6021 one
// step earlier (misakano 1e70717) — wait, don't rotate, don't strike.
func Test6020QueueWaitingIsUpstreamBusy(t *testing.T) {
	e := classifyBusiness(6020, "queue.waiting.title", "glm-5.3")
	if e.Class != uperr.ClassUpstreamBusy {
		t.Fatalf("class = %s, want upstream_busy", e.Class)
	}
	if e.Model != "glm-5.3" {
		t.Fatalf("e.Model = %q, want glm-5.3", e.Model)
	}
}

// parseResetWallClock still feeds the 6000 model hold when the message
// carries a reset moment.
func Test6000WithResetWallClock(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).Format("2006-01-02 15:04:05")
	e := classifyBusiness(6000, "模型调用频率超限，将在 "+future+" 重置，请切换其他模型", "glm-5.3")
	if e.Class != uperr.ClassModelRate {
		t.Fatalf("class = %s, want model_rate", e.Class)
	}
	if e.ResetAt.IsZero() {
		t.Fatal("ResetAt should carry the upstream wall clock")
	}
}
