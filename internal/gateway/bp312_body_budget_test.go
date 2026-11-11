package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
	"bargainproxy/internal/uperr"
)

// BP-312: a request whose WIRE size exceeds a channel's measured byte ceiling
// must not be dispatched there at all, and a payload-too-large 413 must not
// be re-sent (the body cannot shrink by resending it).
//
// Field 2026-11-11 22:56: a conversation carrying 51 screenshots (8MB of
// base64, two user-pasted images alone = 4.1MB) burned workbuddy's 5.8s and
// trae's 19s dispatch windows on EOF before lobster's own 4MB gate answered
// in 10ms — and the flaky-400 guard then re-sent the same 6MB body for
// another 413.

type bp312Driver struct {
	channel string
	calls   *int
	fail    *uperr.Error
}

func (d *bp312Driver) Channel() string                          { return d.channel }
func (d *bp312Driver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }

func (d *bp312Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	*d.calls++
	if d.fail != nil {
		ue := *d.fail
		return nil, &ue
	}
	_ = emit(ir.StreamEvent{Kind: ir.EvText, Delta: "ok"})
	_ = emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "ok"}}},
		StopReason: ir.StopEnd,
	}, nil
}

// bigImageRequest builds a request whose base64 payload exceeds `mb` MB.
func bigImageRequest(mb int) *ir.ChatRequest {
	payload := make([]byte, mb*1024*1024)
	for i := range payload {
		payload[i] = 'A'
	}
	return &ir.ChatRequest{
		Model: "m", Stream: true, MaxTokens: 100,
		Messages: []ir.Message{{
			Role: ir.RoleUser,
			Content: []ir.ContentPart{
				{Kind: "text", Text: "see attached"},
				{Kind: "image", ImageMIME: "image/png", ImageB64: string(payload)},
			},
		}},
	}
}

// newBP312Gateway wires ONE driver behind TWO bindings (channels c1/c2) with
// per-channel byte ceilings from cfg.
func newBP312Gateway(t *testing.T, c1Ceiling int64, fail *uperr.Error) (*Gateway, map[string]*int) {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.ChannelMaxBodyBytes = map[string]int64{"workbuddy": c1Ceiling}
	cfg.Models = []config.ModelBinding{
		{Canonical: "m", Channel: "workbuddy", UpstreamModel: "m"},
		{Canonical: "m", Channel: "trae", UpstreamModel: "m"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second}, nil,
		func(a *model.Account) { _ = st.SaveAccount(a) })
	p.Upsert(&model.Account{ID: "c1-1", Channel: "workbuddy", CredsJSON: []byte(`{}`)})
	p.Upsert(&model.Account{ID: "c2-1", Channel: "trae", CredsJSON: []byte(`{}`)})
	c1Calls, c2Calls := new(int), new(int)
	return New(cfg, p, router.New(cfg), map[string]driver.Driver{
		"workbuddy": &bp312Driver{channel: "workbuddy", calls: c1Calls, fail: fail},
		"trae":      &bp312Driver{channel: "trae", calls: c2Calls},
	}, st), map[string]*int{"workbuddy": c1Calls, "trae": c2Calls}
}

// TestBP312ByteCeilingSkipsBeforeDispatch: workbuddy's ceiling is 1MB, the request
// carries 2MB of base64 → workbuddy is skipped WITHOUT a driver call; trae serves it.
func TestBP312ByteCeilingSkipsBeforeDispatch(t *testing.T) {
	g, counts := newBP312Gateway(t, 1<<20, nil)
	res, err := g.Execute(context.Background(), bigImageRequest(2), func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Channel != "trae" {
		t.Fatalf("served by %s, want trae", res.Channel)
	}
	if got := *counts["workbuddy"]; got != 0 {
		t.Fatalf("workbuddy dispatched %d times, want 0 (byte ceiling must skip before dispatch)", got)
	}
}

// TestBP312ByteCeilingUnderFits: a request under the ceiling is dispatched
// normally — the estimate must not over-reject.
func TestBP312ByteCeilingUnderFits(t *testing.T) {
	g, counts := newBP312Gateway(t, 1<<20, nil)
	req := bigImageRequest(0) // text only, tiny
	if _, err := g.Execute(context.Background(), req, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := *counts["workbuddy"]; got != 1 {
		t.Fatalf("workbuddy dispatched %d times, want 1 (small requests must not be skipped)", got)
	}
}

// TestBP312PinnedBypassesByteGate: a pinned channel is attempted even when
// the body is over its ceiling — the caller asked for its honest answer.
func TestBP312PinnedBypassesByteGate(t *testing.T) {
	g, counts := newBP312Gateway(t, 1<<20, nil)
	req := bigImageRequest(2)
	req.Model = "channel:workbuddy:m"
	res, err := g.Execute(context.Background(), req, func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("Execute: %v (pinned channels must keep their honest attempt)", err)
	}
	if res.Channel != "workbuddy" {
		t.Fatalf("served by %s, want workbuddy", res.Channel)
	}
	if got := *counts["workbuddy"]; got != 1 {
		t.Fatalf("workbuddy dispatched %d times, want 1", got)
	}
}

// TestBP312Four13NotResent: a 413 from the first binding must NOT trigger the
// flaky-400 resend; the second binding still gets its turn and serves.
func TestBP312Four13NotResent(t *testing.T) {
	fail := uperr.New(uperr.ClassBadRequest, 413, "workbuddy: request body of 6034505 bytes exceeds the proxy ceiling of 4194304 bytes")
	g, counts := newBP312Gateway(t, 0, fail)
	res, err := g.Execute(context.Background(), bigImageRequest(0), func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Channel != "trae" {
		t.Fatalf("served by %s, want trae", res.Channel)
	}
	if got := *counts["workbuddy"]; got != 1 {
		t.Fatalf("workbuddy dispatched %d times, want 1 (a deterministic 413 must not be re-sent)", got)
	}
}

// TestBP312Describe413Passthrough pins the client-facing status.
func TestBP312Describe413Passthrough(t *testing.T) {
	status, _, msg := DescribeError(uperr.New(uperr.ClassBadRequest, 413, "lobster: request body of 6034505 bytes exceeds the proxy ceiling"))
	if status != 413 {
		t.Fatalf("status = %d, want 413 (payload-too-large must not masquerade as 400)", status)
	}
	if !strings.Contains(msg, "exceeds the proxy ceiling") {
		t.Fatalf("message lost: %q", msg)
	}
}

// TestBP312EstimateRequestBytes pins the byte proxy on the shape that matters:
// base64 rides the wire, and EstimatePromptTokens deliberately ignores it.
func TestBP312EstimateRequestBytes(t *testing.T) {
	req := &ir.ChatRequest{
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Thinking: []ir.ThinkingBlock{{Text: "12345"}}, // 5 bytes
				Content: []ir.ContentPart{
					{Kind: "text", Text: "0123456789"},                              // 10 bytes
					{Kind: "image", ImageMIME: "image/png", ImageB64: "abcdefghij"}, // 10 bytes
				}},
		},
		Tools: []ir.ToolDef{{Name: "nm", Description: "desc"}},
	}
	got := ir.EstimateRequestBytes(req)
	if got != 10+10+5+len("nm")+len("desc") {
		t.Fatalf("EstimateRequestBytes = %d, want %d", got, 10+10+5+len("nm")+len("desc"))
	}
	// And it must see what the token estimator cannot: a 1MB base64 image.
	big := &ir.ChatRequest{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{
		{Kind: "image", ImageMIME: "image/png", ImageB64: strings.Repeat("A", 1<<20)},
	}}}}
	if ir.EstimatePromptTokens(big) > 2000 {
		t.Fatalf("EstimatePromptTokens counts images by tiles; %d shows the metric changed", ir.EstimatePromptTokens(big))
	}
	if ir.EstimateRequestBytes(big) < 1<<20 {
		t.Fatalf("EstimateRequestBytes = %d, must reflect the base64 payload", ir.EstimateRequestBytes(big))
	}
}
