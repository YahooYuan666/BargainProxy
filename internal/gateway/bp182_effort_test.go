package gateway

import (
	"context"
	"strings"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/uperr"
	"bargainproxy/internal/model"
)

// effortDriver refuses the effort gear once (wording that names it), then
// answers; it records what effort each call carried.
type effortDriver struct {
	calls   int
	efforts []string
}

func (d *effortDriver) Channel() string                          { return model.ChannelWorkBuddy }
func (d *effortDriver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }
func (d *effortDriver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	d.calls++
	d.efforts = append(d.efforts, call.Request.RequestedEffort)
	if d.calls == 1 {
		return nil, badReqErr("workbuddy: reasoning_effort 'max' is not supported by this model")
	}
	_ = emit(ir.StreamEvent{Kind: ir.EvText, Delta: "ok"})
	_ = emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "ok"}}},
		StopReason: ir.StopEnd,
	}, nil
}

func badReqErr(msg string) error {
	return uperr.New(uperr.ClassBadRequest, 400, "%s", msg)
}

// BP-182: an effort-gear refusal downgrades ONE level on the same account —
// no rotation, no identical resend.
func TestBP182EffortDowngradeRetry(t *testing.T) {
	drv := &effortDriver{}
	g := newGatewayWithTwoBindings(t, drv)

	req := bp168Request()
	req.RequestedEffort = "max"
	var text strings.Builder
	if _, err := g.Execute(context.Background(), req, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
			text.WriteString(ev.Delta)
		}
		return nil
	}); err != nil {
		t.Fatalf("downgrade retry failed: %v", err)
	}
	if text.String() != "ok" {
		t.Fatalf("text = %q", text.String())
	}
	if drv.calls != 2 {
		t.Fatalf("calls = %d, want 2 (original + one downgrade)", drv.calls)
	}
	if drv.efforts[1] != "xhigh" {
		t.Fatalf("retry effort = %q, want xhigh", drv.efforts[1])
	}
}

// The bottom of the ladder strips the gear entirely.
func TestBP182DowngradeLadder(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		"max":  {"xhigh", true},
		"high": {"medium", true},
		"low":  {"", true},
		"off":  {"", false},
	}
	for in, want := range cases {
		got, ok := downgradeEffort(in)
		if got != want.want || ok != want.ok {
			t.Errorf("downgradeEffort(%q) = %q,%v want %q,%v", in, got, ok, want.want, want.ok)
		}
	}
}
