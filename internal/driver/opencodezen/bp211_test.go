package opencodezen

import (
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/uperr"
)

// BP-211: 429 FreeUsageLimitError is the anonymous lane's DAILY allowance
// being spent (resets 00:00 UTC), not transient queue capacity — it must
// ride ClassSoftRate with the real reset instead of the "queue full, retry
// shortly" lie ClassUpstreamBusy rendered.
func TestBP211FreeUsageLimitIsASoftRateWithTheUTCReset(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached. Free models work in OpenCode"}}`)
	e := classifyHTTP(429, body)
	if e.Class != uperr.ClassSoftRate {
		t.Fatalf("class = %s, want soft rate", e.Class)
	}
	want := nextUTCMidnight(time.Now())
	if e.ResetAt.IsZero() || !e.ResetAt.Equal(want) {
		t.Fatalf("ResetAt = %v, want the next UTC midnight %v", e.ResetAt, want)
	}
	if !strings.Contains(e.Message, "resets at 00:00 UTC") {
		t.Fatalf("message must say the reset plainly: %s", e.Message)
	}
	if u := want.UTC(); u.Hour() != 0 || u.Minute() != 0 || u.Second() != 0 {
		t.Fatalf("reset must be UTC midnight, got %v", u)
	}
}

// Other 429 wordings stay the transient-capacity class they always were.
func TestBP211Other429StaysUpstreamBusy(t *testing.T) {
	e := classifyHTTP(429, []byte(`{"error":{"message":"Endpoint is unavailable"}}`))
	if e.Class != uperr.ClassUpstreamBusy {
		t.Fatalf("class = %s, want upstream busy", e.Class)
	}
	if !e.ResetAt.IsZero() {
		t.Fatalf("a capacity 429 must not invent a reset, got %v", e.ResetAt)
	}
}

func TestBP211NextUTCMidnight(t *testing.T) {
	now := time.Date(2026, 10, 30, 16, 33, 0, 0, time.UTC)
	want := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	if got := nextUTCMidnight(now); !got.Equal(want) {
		t.Fatalf("nextUTCMidnight = %v, want %v", got, want)
	}
	// 23:59:59 UTC rolls to the next day's midnight.
	late := time.Date(2026, 10, 30, 23, 59, 59, 0, time.UTC)
	if got := nextUTCMidnight(late); !got.Equal(time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("late-day reset wrong: %v", got)
	}
}
