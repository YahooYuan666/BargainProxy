// BP-47: a model-scoped upstream rate limit (workbuddy 6004) is rate
// limiting by nature — DescribeError must answer 429 with the model and the
// reset moment. Before this case existed it fell through to the default
// branch, where the classifier's status 200 clamped to 500 and an honest
// upstream limit read as a gateway bug in both the client and the panel.
package gateway

import (
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/uperr"
)

func TestDescribeErrorModelRateIsRateLimit(t *testing.T) {
	reset := time.Date(2026, 10, 16, 22, 16, 56, 0, time.FixedZone("CST", 8*3600))
	ue := uperr.New(uperr.ClassModelRate, 200,
		"workbuddy: 您的使用量已超出频率限制，将在 2026-10-16 22:16:56 UTC+8 重置，您也可以切换其他模型继续使用。")
	ue.Model = "deepseek-v4.1-flash"
	ue.ResetAt = reset

	status, class, msg := DescribeError(ue)
	if status != 429 {
		t.Fatalf("status = %d, want 429; msg=%q", status, msg)
	}
	if class != uperr.ClassModelRate {
		t.Fatalf("class = %q, want %q", class, uperr.ClassModelRate)
	}
	for _, want := range []string{"upstream model rate limit", "deepseek-v4.1-flash", "2026-10-16T22:16:56+08:00", "频率限制"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q missing %q", msg, want)
		}
	}
}

// Defensive: without a model name or reset moment (e.g. a trae 404 mapped to
// model_rate) the mapping stays 429 with a generic prefix.
func TestDescribeErrorModelRateWithoutReset(t *testing.T) {
	ue := uperr.New(uperr.ClassModelRate, 200, "trae: model endpoint gone")
	status, _, msg := DescribeError(ue)
	if status != 429 {
		t.Fatalf("status = %d, want 429; msg=%q", status, msg)
	}
	if !strings.Contains(msg, "upstream model rate limit") || strings.Contains(msg, "resets at") {
		t.Fatalf("unexpected message %q", msg)
	}
}
