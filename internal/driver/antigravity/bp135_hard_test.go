package antigravity

import (
	"testing"
	"time"
)

// BP-135: the reset the 429 body names is the quota bucket's own boundary —
// the driver must flag it hard so the pool holds to the exact moment instead
// of capping at 2h.
func Test429ParsedResetIsFlaggedHard(t *testing.T) {
	e := classifyHTTP(429, []byte(`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"14400s"}]}}`), "gemini-3-pro")
	if e.ResetAt.IsZero() {
		t.Fatal("parsed reset lost")
	}
	if ttl := time.Until(e.ResetAt); ttl < 3*time.Hour {
		t.Fatalf("reset ttl = %s, want ~4h from the body", ttl)
	}
	if !e.ResetHard {
		t.Fatal("parsed upstream reset must be flagged hard (no 2h cap on top)")
	}
}

// No stated reset: no hard flag, pool keeps its fallback behavior.
func Test429WithoutResetNotHard(t *testing.T) {
	e := classifyHTTP(429, []byte(`{"error":{"status":"RESOURCE_EXHAUSTED"}}`), "gemini-3-pro")
	if !e.ResetAt.IsZero() {
		t.Fatalf("unexpected reset %v", e.ResetAt)
	}
	if e.ResetHard {
		t.Fatal("nothing stated — nothing to flag hard")
	}
}
