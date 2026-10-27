package config

import (
	"testing"
	"time"
)

// BP-129: Qoder answers a too-large max_tokens with
// "Range of max_tokens should be [1, N]" and its catalog does not disclose N
// (qoderhub #100/#101). The gateway learns N from the 400 and clamps later
// requests upfront.
func TestCeilingFromMessage(t *testing.T) {
	if got := CeilingFromMessage(`{"error":{"message":"Range of max_tokens should be [1, 32768]"}}`); got != 32768 {
		t.Fatalf("ceiling = %d, want 32768", got)
	}
	if got := CeilingFromMessage(`range of max output tokens should be [1, 16384]`); got != 16384 {
		t.Fatalf("loose wording ceiling = %d, want 16384", got)
	}
	if got := CeilingFromMessage("unrelated 400"); got != 0 {
		t.Fatalf("unrelated body = %d, want 0", got)
	}
	// A tiny "ceiling" is a floor in disguise; out-of-band numbers are refused.
	if got := CeilingFromMessage("Range of max_tokens should be [1, 100]"); got != 0 {
		t.Fatalf("tiny ceiling = %d, want 0", got)
	}
}

func TestLearnedCeilingRoundTripAndTTL(t *testing.T) {
	LearnOutputCeiling("qoder", "Qwen3.8-Flash", 32768)
	if got := LearnedOutputCeiling("qoder", "qwen3.8-flash"); got != 32768 {
		t.Fatalf("learned = %d, want 32768 (case-insensitive key)", got)
	}
	if got := LearnedOutputCeiling("qoder", "other-model"); got != 0 {
		t.Fatalf("cross-model leak = %d, want 0", got)
	}
	// Expired entries stop binding.
	key := ceilingKey("qoder", "ttl-model")
	learnedCeilings.Store(key, learnedCeiling{cap: 4096, until: time.Now().Add(-time.Minute)})
	if got := LearnedOutputCeiling("qoder", "ttl-model"); got != 0 {
		t.Fatalf("expired ceiling = %d, want 0", got)
	}
}
