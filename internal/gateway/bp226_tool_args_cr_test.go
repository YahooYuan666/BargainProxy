package gateway

import (
	"context"
	"encoding/json"
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
)

// BP-226: lone-CR scrubbing for tool-call arguments (magpie dd279e93 #4256).
func TestBP226DropLoneCR(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a\rb", "ab"},                         // lone CR inside a string — the live bug
		{"a\r\nb", "a\r\nb"},                   // CRLF pair stays (legal whitespace)
		{"a\r", "a"},                           // trailing lone CR
		{"\r\r\n", "\r\n"},                     // only the paired one survives
		{"{\"p\":\"a\rb\"}", "{\"p\":\"ab\"}"}, // raw CR byte inside a JSON string value
		{`{"p":"a\\rb"}`, `{"p":"a\\rb"}`},     // escaped \r text is NOT a raw byte
		{"plain", "plain"},
		{"", ""},
	}
	for _, c := range cases {
		if got := dropLoneCR(c.in); got != c.want {
			t.Fatalf("dropLoneCR(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The stream form: a \r at a delta boundary must wait for the next delta
// before deciding, and a \r that pairs across the boundary survives.
func TestBP226CRTidyStream(t *testing.T) {
	// Lone CR at a boundary, next delta is ordinary → dropped.
	c := &crTidy{}
	if got := c.write("args\r"); got != "args" {
		t.Fatalf("write 1 = %q, want the held \\r withheld", got)
	}
	if got := c.write(" more"); got != " more" {
		t.Fatalf("write 2 = %q, the held \\r must be dropped", got)
	}
	c.flush()

	// CRLF split across deltas → reunited.
	c2 := &crTidy{}
	if got := c2.write("a\r"); got != "a" {
		t.Fatalf("write 1 = %q", got)
	}
	if got := c2.write("\nb"); got != "\r\nb" {
		t.Fatalf("write 2 = %q, want the CRLF reunited", got)
	}

	// A \r whose next byte is another \r: first is lone (dropped), second
	// pairs with the following \n.
	c3 := &crTidy{}
	if got := c3.write("\r\r\n"); got != "\r\n" {
		t.Fatalf("write 1 = %q, want the lone one dropped and CRLF kept", got)
	}
	if got := c3.write("x"); got != "x" {
		t.Fatalf("write 2 = %q, want clean x", got)
	}

	// A delta that is exactly "\r" holds; flush drops it (end of args).
	c4 := &crTidy{}
	if got := c4.write("\r"); got != "" {
		t.Fatalf("write 1 = %q", got)
	}
	c4.flush() // nothing may surface afterwards
	if got := c4.write("y"); got != "y" {
		t.Fatalf("post-flush write = %q, want clean y", got)
	}
}

// bp226CRDriver streams tool args with a raw lone CR split across deltas and
// returns the same dirty arguments aggregated — the exact wire shape the
// glm-5.3-flash* replicas produce (magpie dd279e93 #4256).
type bp226CRDriver struct{}

func (d *bp226CRDriver) Channel() string                          { return model.ChannelWorkBuddy }
func (d *bp226CRDriver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }

func (d *bp226CRDriver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	_ = emit(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "t1", Name: "write"}})
	_ = emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: "{\"path\":\"a\r"})
	_ = emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: ".go\"}"})
	_ = emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopToolUse})
	return &ir.ChatResponse{
		Message: ir.Message{Role: ir.RoleAssistant,
			ToolCalls: []ir.ToolCall{{ID: "t1", Name: "write", Arguments: "{\"path\":\"a\r.go\"}"}}},
		StopReason: ir.StopToolUse,
	}, nil
}

// The wiring test: the scrubber must sit in the gateway's event sink and on
// the final response — a helper tested in isolation proves nothing about the
// path a real request takes.
func TestBP226GatewayScrubsToolArgsCR(t *testing.T) {
	wb := &bp226CRDriver{}
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = []config.ModelBinding{
		{Canonical: "m", Channel: model.ChannelWorkBuddy, UpstreamModel: "m", Priority: 1},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second},
		map[string]int{model.ChannelWorkBuddy: 1},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	p.Upsert(&model.Account{ID: "wb-acct", Channel: model.ChannelWorkBuddy, CredsJSON: []byte(`{}`)})
	g := New(cfg, p, router.New(cfg), map[string]driver.Driver{model.ChannelWorkBuddy: wb}, st)

	var deltas []string
	req := &ir.ChatRequest{
		Model: "m", Stream: true, MaxTokens: 100,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
	res, err := g.Execute(context.Background(), req, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvToolArgs {
			deltas = append(deltas, ev.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	joined := strings.Join(deltas, "")
	if strings.Contains(joined, "\r") {
		t.Fatalf("client-visible tool-arg deltas still carry a raw CR: %q", joined)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(joined), &parsed); err != nil {
		t.Fatalf("scrubbed deltas must assemble into valid JSON: %q: %v", joined, err)
	}
	if parsed["path"] != "a.go" {
		t.Fatalf("path = %v, want a.go (content intact)", parsed["path"])
	}
	if args := res.Response.Message.ToolCalls[0].Arguments; strings.Contains(args, "\r") {
		t.Fatalf("final aggregated arguments still carry a raw CR: %q", args)
	}
}
