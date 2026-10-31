package openaicompat

import (
	"testing"
	"time"

	"bargainproxy/internal/uperr"
)

// A frequency wall wearing quota wording is a throttle, not a balance death:
// the account must rest seconds, not freeze (magpie d83f887a, sub2api #8286).
// The help link's "quotaincrease" must not decide the class.
func TestBP153MinuteWindowBeatQuotaWording(t *testing.T) {
	body := []byte(`{"error":{"message":"Quota exceeded for requests_per_minute. See https://aka.ms/oai/quotaincrease.","type":"quota_exceeded"}}`)
	e := classifyHTTP(402, body, "m")
	if e.Class != uperr.ClassSoftRate {
		t.Fatalf("TPM wall became %s, want soft_rate", e.Class)
	}
	if diff := time.Until(e.ResetAt); diff <= 0 || diff > 10*time.Second {
		t.Fatalf("ResetAt in %s, want a seconds-level rest", diff)
	}
}

// A plain 429 carrying the TPM wording rests seconds instead of the pool's
// 10-minute soft base.
func TestBP153TPM429RestsSeconds(t *testing.T) {
	body := []byte(`{"error":{"message":"Allocated quota exceeded, please increase your quota limit.","type":"insufficient_quota"}}`)
	e := classifyHTTP(429, body, "m")
	if e.Class != uperr.ClassSoftRate {
		t.Fatalf("TPM 429 became %s, want soft_rate", e.Class)
	}
	if diff := time.Until(e.ResetAt); diff <= 0 || diff > 10*time.Second {
		t.Fatalf("ResetAt in %s, want a seconds-level rest", diff)
	}
}

// Gemini's per-minute wall says only "quota" and "PerMinute"; a mid-stream
// frame with that shape used to fall into the 402 branch.
func TestBP153FrameErrorPerMinuteStays429(t *testing.T) {
	frame := `{"error":{"code":"insufficient_quota","message":"You exceeded your current quota. quotaId: projects/123/locations/us/requests/PerMinute."}}`
	e := detectFrameError(frame)
	if e == nil {
		t.Fatal("frame error not detected")
	}
	if e.Class != uperr.ClassSoftRate {
		t.Fatalf("per-minute frame became %s, want soft_rate", e.Class)
	}
}

// OpenAI's genuine balance death (no window wording) keeps the old class.
func TestBP153GenuineInsufficientQuotaStillHardCredit(t *testing.T) {
	e := classifyHTTP(402, []byte(`{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota"}}`), "m")
	if e.Class != uperr.ClassHardCredit {
		t.Fatalf("balance death became %s, want hard_credit", e.Class)
	}
}

// A plan reset that also says "quota" must not be shrunk to seconds.
func TestBP153LongWindowStillHardCredit(t *testing.T) {
	e := classifyHTTP(402, []byte(`{"error":{"message":"Monthly quota exhausted for this plan (per day: 100).","type":"insufficient_quota"}}`), "m")
	if e.Class != uperr.ClassHardCredit {
		t.Fatalf("long-window wall became %s, want hard_credit", e.Class)
	}
}
