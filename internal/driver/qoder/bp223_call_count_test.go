package qoder

import (
	"testing"
	"time"

	"bargainproxy/internal/uperr"
)

// BP-223: the nested CALL_COUNT_LIMIT wall rides the same 403 envelope as
// RESOURCE_LIMIT. Queued answers are the wait-and-repoll family of 10605
// (SoftRate + marker), spent answers a per-model hold (ModelRate) — the old
// fallthrough classified both as ClassAuth and killed the account for a
// quota that resets on its own (Zhengyuuuui/qoder2api 8d49aa6e).
func TestBP223CallCountQueuedIsSoftRate(t *testing.T) {
	body := `{"code":"403","message":"{\"subCode\":\"CALL_COUNT_LIMIT\",\"isQueued\":true,\"retryAfterSeconds\":12}"}`
	e := classifyQoder(403, body)
	if e.Class != uperr.ClassSoftRate {
		t.Fatalf("queued CALL_COUNT_LIMIT = %s, want ClassSoftRate", e.Class)
	}
	if !qoderQueueRetryable(e.Class, e.Message) {
		t.Fatalf("message %q must engage the queue machinery", e.Message)
	}
}

// A direct nested object (not a string-wrapped message) parses the same way.
func TestBP223CallCountDirectObjectIsSoftRate(t *testing.T) {
	body := `{"message":{"subCode":"CALL_COUNT_LIMIT","isQueued":true,"retryAfterSeconds":5}}`
	e := classifyQoder(403, body)
	if e.Class != uperr.ClassSoftRate {
		t.Fatalf("queued CALL_COUNT_LIMIT = %s, want ClassSoftRate", e.Class)
	}
}

// Positive retryAfterSeconds means waitable even without the isQueued flag.
func TestBP223CallCountRetryAfterAloneIsSoftRate(t *testing.T) {
	body := `{"code":"403","message":"{\"subCode\":\"CALL_COUNT_LIMIT\",\"isQueued\":false,\"retryAfterSeconds\":30}"}`
	e := classifyQoder(403, body)
	if e.Class != uperr.ClassSoftRate {
		t.Fatalf("retryable CALL_COUNT_LIMIT = %s, want ClassSoftRate", e.Class)
	}
}

// Spent quota (not queued, no wait) is a per-model hold with an hour re-probe.
func TestBP223CallCountSpentIsModelRate(t *testing.T) {
	body := `{"code":"403","message":"{\"subCode\":\"CALL_COUNT_LIMIT\",\"isQueued\":false,\"retryAfterSeconds\":0}"}`
	e := classifyQoder(403, body)
	if e.Class != uperr.ClassModelRate {
		t.Fatalf("spent CALL_COUNT_LIMIT = %s, want ClassModelRate", e.Class)
	}
	if e.ResetAt.IsZero() || e.ResetAt.Before(time.Now().Add(30*time.Minute)) {
		t.Fatalf("ResetAt must hold for about an hour, got %v", e.ResetAt)
	}
}

// No CALL_COUNT_LIMIT shape may ever reach the credential verdict.
func TestBP223CallCountNeverAuth(t *testing.T) {
	shapes := []string{
		`{"code":"403","message":"{\"subCode\":\"CALL_COUNT_LIMIT\",\"isQueued\":true,\"retryAfterSeconds\":12}"}`,
		`{"code":"403","message":"{\"subCode\":\"CALL_COUNT_LIMIT\",\"isQueued\":false,\"retryAfterSeconds\":0}"}`,
		`{"message":{"subCode":"CALL_COUNT_LIMIT","isQueued":false}}`,
	}
	for _, body := range shapes {
		if e := classifyQoder(403, body); e.Class == uperr.ClassAuth {
			t.Fatalf("CALL_COUNT_LIMIT fell to ClassAuth: %s", body)
		}
	}
}

// Other subCodes keep the old behavior: the RESOURCE_LIMIT permission wall is
// untouched, unknown subCodes still take the 403 fallthrough.
func TestBP223ResourceLimitAndUnknownUnchanged(t *testing.T) {
	if e := classifyQoder(403, `{"message":"{\"subCode\":\"RESOURCE_LIMIT\",\"isQueued\":false}"}`); e.Class != uperr.ClassModelRate {
		t.Fatalf("RESOURCE_LIMIT = %s, want ClassModelRate (regression)", e.Class)
	}
	if e := classifyQoder(403, `{"message":"{\"subCode\":\"OTHER\"}"}`); e.Class != uperr.ClassAuth {
		t.Fatalf("unknown subCode = %s, want ClassAuth (fallthrough unchanged)", e.Class)
	}
}

// The queue-machinery gate: SoftRate + either marker engages; other classes
// and marker-less messages do not.
func TestBP223QueueRetryableGate(t *testing.T) {
	if !qoderQueueRetryable(uperr.ClassSoftRate, "qoder: x (10605)") {
		t.Fatal("10605 marker must engage")
	}
	if !qoderQueueRetryable(uperr.ClassSoftRate, "qoder: tool call quota queued (CALL_COUNT_LIMIT)") {
		t.Fatal("CALL_COUNT_LIMIT marker must engage")
	}
	if qoderQueueRetryable(uperr.ClassAuth, "CALL_COUNT_LIMIT") {
		t.Fatal("non-soft classes must not engage")
	}
	if qoderQueueRetryable(uperr.ClassSoftRate, "qoder: plain text") {
		t.Fatal("marker-less soft rate must not engage")
	}
}
