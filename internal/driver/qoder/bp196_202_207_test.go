package qoder

import (
	"encoding/json"
	"testing"
	"time"
)

// BP-196 (qoder2api-hub 45e76f9, issue #112): the upstream names the cache
// hit in model-family fields — DeepSeek answers prompt_cache_hit_tokens,
// Qwen tucks it under completion_tokens_details.cached_tokens — and neither
// fills the standard slot. Both fold in; the standard slot wins.
func TestBP196CacheFieldsFoldIntoTheStandardSlot(t *testing.T) {
	u := extractUsageObj(map[string]interface{}{
		"prompt_tokens":           100.0,
		"prompt_cache_hit_tokens": 64.0,
	})
	if u.PromptTokensDetails == nil || u.PromptTokensDetails.CachedTokens != 64 {
		t.Fatalf("the DeepSeek cache field must fold into the standard slot: %+v", u.PromptTokensDetails)
	}

	u2 := extractUsageObj(map[string]interface{}{
		"completion_tokens_details": map[string]interface{}{"cached_tokens": 32.0},
	})
	if u2.PromptTokensDetails == nil || u2.PromptTokensDetails.CachedTokens != 32 {
		t.Fatalf("the Qwen cache field must fold into the standard slot: %+v", u2.PromptTokensDetails)
	}

	u3 := extractUsageObj(map[string]interface{}{
		"prompt_tokens_details":   map[string]interface{}{"cached_tokens": 7.0},
		"prompt_cache_hit_tokens": 64.0,
	})
	if u3.PromptTokensDetails == nil || u3.PromptTokensDetails.CachedTokens != 7 {
		t.Fatalf("the standard slot must win when present: %+v", u3.PromptTokensDetails)
	}

	// The reasoning tokens must survive the fold.
	u4 := extractUsageObj(map[string]interface{}{
		"completion_tokens_details": map[string]interface{}{"reasoning_tokens": 11.0, "cached_tokens": 5.0},
	})
	if u4.CompletionTokensDetails == nil || u4.CompletionTokensDetails.ReasoningTokens != 11 {
		t.Fatalf("reasoning tokens lost in the fold: %+v", u4.CompletionTokensDetails)
	}
	if u4.PromptTokensDetails == nil || u4.PromptTokensDetails.CachedTokens != 5 {
		t.Fatalf("Qwen cache field lost: %+v", u4.PromptTokensDetails)
	}
}

// BP-202 (plugins cc3f000): the expiry may arrive as absolute time — number
// in seconds or millis, or a date string — and an absolute value has no unit
// ambiguity, so it wins over the relative expires_in.
func TestBP202AbsoluteExpire(t *testing.T) {
	ms := absoluteExpire(json.RawMessage("1794000000000"))
	if !ms.Equal(time.UnixMilli(1794000000000)) {
		t.Fatalf("millis epoch: %v", ms)
	}
	sec := absoluteExpire(json.RawMessage("1794000000"))
	if !sec.Equal(time.UnixMilli(1794000000000)) {
		t.Fatalf("seconds epoch must scale to millis: %v", sec)
	}
	str := absoluteExpire(json.RawMessage(`"2026-11-05 12:00:00"`))
	if str.IsZero() {
		t.Fatal("a date-string expiry must parse")
	}
	if !absoluteExpire(json.RawMessage("null"), json.RawMessage("garbage-ish")).IsZero() {
		t.Fatal("garbage is no answer")
	}
	if !absoluteExpire().IsZero() {
		t.Fatal("no input is no answer")
	}
}

// BP-207 (qoder2api-hub #113): the billed-model mismatch warn is one line
// per (requested, billed) pair per hour.
func TestBP207BilledWarnOnce(t *testing.T) {
	keyA, keyB := "bp207-test-a|auto", "bp207-test-b|auto"
	if !billedWarnOnce(keyA, "") || billedWarnOnce(keyA, "") {
		t.Fatal("the same pair must warn once, then stay quiet for the hour")
	}
	if !billedWarnOnce(keyB, "") {
		t.Fatal("a different pair still warns")
	}
}
