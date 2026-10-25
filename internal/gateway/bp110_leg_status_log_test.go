// BP-110: a failed leg must log the upstream's HTTP status.
//
// Field evidence 2026-10-25: six lobster legs logged
//
//	class=transport: lobster:
//
// — nothing after the colon, because the driver words a non-200 as
// "lobster: <truncated body>" and the upstream had sent an EMPTY body. The
// status was in ue.Status the whole time but the log line printed only the
// message, so "500 and said nothing" and "502 and said nothing" were
// indistinguishable, and the first question on every one of these — "what did
// the upstream actually answer?" — could not be answered from the logs.
//
// This test drives a real failing leg through Execute and captures the log
// output, so the line itself is pinned rather than assumed.
package gateway

import (
	"bytes"
	"context"
	"log"
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

// statusDriver fails every call with a chosen class+status, the shape a driver
// produces when the upstream answers non-200.
type statusDriver struct {
	class  string
	status int
	msg    string
}

func (d *statusDriver) Channel() string                          { return model.ChannelQoder }
func (d *statusDriver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }

func (d *statusDriver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	return nil, uperr.New(d.class, d.status, "%s", d.msg)
}

// TestFailedLegLogsTheHTTPStatus: a leg that fails with status 502 must produce
// a "leg failed" line carrying status=502. Without it the operator cannot tell
// an upstream 5xx from a driver that produced no message.
func TestFailedLegLogsTheHTTPStatus(t *testing.T) {
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = []config.ModelBinding{
		{Canonical: "m", Channel: model.ChannelQoder, UpstreamModel: "m"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelQoder: 1},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	p.Upsert(&model.Account{ID: "qd-1", Channel: model.ChannelQoder, CredsJSON: []byte(`{}`)})

	g := New(cfg, p, router.New(cfg), map[string]driver.Driver{
		model.ChannelQoder: &statusDriver{
			class: uperr.ClassTransport, status: 502,
			msg: "lobster: empty response body (HTTP 502)",
		},
	}, st)

	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) }()

	req := &ir.ChatRequest{
		Model:    "m",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		Stream:   false,
	}
	_, err := g.Execute(context.Background(), req, func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("the failing driver must surface an error")
	}

	out := buf.String()
	if !strings.Contains(out, "leg failed") {
		t.Fatalf("no leg-failed line was logged; got:\n%s", out)
	}
	if !strings.Contains(out, "status=502") {
		t.Fatalf("leg-failed line omits the HTTP status, so a 500 and a 502 read the same:\n%s", out)
	}
	if !strings.Contains(out, "class=transport") {
		t.Fatalf("leg-failed line omits the class:\n%s", out)
	}
}

// A leg that never got a response must say status=0 rather than dropping the
// field: "no response at all" is a different diagnosis from "the upstream
// answered 5xx", and both must be visible.
func TestFailedLegWithNoResponseLogsStatusZero(t *testing.T) {
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = []config.ModelBinding{
		{Canonical: "m", Channel: model.ChannelQoder, UpstreamModel: "m"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelQoder: 1},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	p.Upsert(&model.Account{ID: "qd-1", Channel: model.ChannelQoder, CredsJSON: []byte(`{}`)})

	g := New(cfg, p, router.New(cfg), map[string]driver.Driver{
		model.ChannelQoder: &statusDriver{
			class: uperr.ClassTransport, status: 0, msg: "connection reset",
		},
	}, st)

	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) }()

	req := &ir.ChatRequest{
		Model:    "m",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		Stream:   false,
	}
	_, _ = g.Execute(context.Background(), req, func(ir.StreamEvent) error { return nil })

	if !strings.Contains(buf.String(), "status=0") {
		t.Fatalf("a leg with no upstream response must still print status=0:\n%s", buf.String())
	}
}
