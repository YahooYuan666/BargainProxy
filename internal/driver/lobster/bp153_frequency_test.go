package lobster

import (
	"testing"
	"time"

	"bargainproxy/internal/uperr"
)

// BP-153: the frequency walls (42900 "请求过于频繁" and the plain-wording
// limit messages) ask for seconds, not the pool's 10-minute soft base.
func TestBP153FrequencyWallsRestSeconds(t *testing.T) {
	for _, tc := range []struct {
		code    int
		message string
	}{
		{42900, "请求过于频繁"},
		{400, "请求频率过高，请稍后再试"},
	} {
		e := classifyCode(tc.code, tc.message)
		if e.Class != uperr.ClassSoftRate {
			t.Fatalf("code %d became %s, want soft_rate", tc.code, e.Class)
		}
		if diff := time.Until(e.ResetAt); diff <= 0 || diff > 10*time.Second {
			t.Fatalf("code %d ResetAt in %s, want a seconds-level rest", tc.code, diff)
		}
	}
}
