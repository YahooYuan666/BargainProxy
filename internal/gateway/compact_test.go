// BP-307: compaction requests are stripped of tool declarations before
// dispatch, so the model has nothing to call and the client's compaction
// runner cannot reject the turn for a tool call.
package gateway

import (
	"context"
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

// compactInstruction is the exact pair of sentences ZCode appends to a
// compaction request (captured from the 2026-11-10 failing turn).
const compactInstruction = "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\n\n" +
	"- Do NOT use Read, Bash, Grep, Glob, Edit, Write, or ANY other tool.\n" +
	"- Tool calls will be REJECTED and will waste your only turn.\n\n" +
	"Your task is to create a detailed summary of the conversation so far."

func userTurn(text string) ir.Message {
	return ir.Message{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: text}}}
}

func assistantTurn(text string) ir.Message {
	return ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: text}}}
}

// tooledRequest builds a request shaped like the real one: a long history plus
// declared tools and a tool_choice.
func tooledRequest(last ir.Message) *ir.ChatRequest {
	req := &ir.ChatRequest{
		Model:      "m",
		Messages:   []ir.Message{userTurn("hello"), assistantTurn("hi"), last},
		ToolChoice: "auto",
	}
	for _, n := range []string{"Bash", "Read", "Write"} {
		req.Tools = append(req.Tools, ir.ToolDef{Name: n})
	}
	return req
}

// TestBP306DetectionFingerprint pins the detection rule: BOTH sentences, on
// the LAST user message.
func TestBP306DetectionFingerprint(t *testing.T) {
	if !isCompactionRequest(tooledRequest(userTurn(compactInstruction))) {
		t.Fatal("a real compaction instruction was not detected")
	}
	// One sentence alone is prose about compaction, not a compaction turn.
	if isCompactionRequest(tooledRequest(userTurn("Respond with TEXT ONLY. Summarise this."))) {
		t.Fatal("half the fingerprint must not count as compaction")
	}
	if isCompactionRequest(tooledRequest(userTurn("Do NOT call any tools here, just answer."))) {
		t.Fatal("half the fingerprint must not count as compaction")
	}
	// The instruction earlier in the history is the conversation DISCUSSING
	// compaction; the model still has work to do and needs its tools.
	req := &ir.ChatRequest{Messages: []ir.Message{
		userTurn(compactInstruction),
		assistantTurn("understood"),
		userTurn("now fix the CSS"),
	}}
	if isCompactionRequest(req) {
		t.Fatal("an earlier occurrence must not mark the turn as compaction")
	}
	// No user message at all.
	if isCompactionRequest(&ir.ChatRequest{Messages: []ir.Message{assistantTurn("x")}}) {
		t.Fatal("a request with no user message cannot be compaction")
	}
}

// TestBP306StripsAllFourFields every field that could let the model emit a
// tool call is cleared: Tools alone is not enough, because the drivers write
// tool_choice independently of len(req.Tools).
func TestBP306StripsAllFourFields(t *testing.T) {
	req := tooledRequest(userTurn(compactInstruction))
	req.ToolChoiceName = "Bash"
	req.DisableParallel = true

	if !stripToolsForCompaction(req) {
		t.Fatal("strip reported no change on a fully tooled request")
	}
	if len(req.Tools) != 0 || req.ToolChoice != nil || req.ToolChoiceName != "" || req.DisableParallel {
		t.Fatalf("leftover tool state: tools=%d choice=%v name=%q parallel=%v",
			len(req.Tools), req.ToolChoice, req.ToolChoiceName, req.DisableParallel)
	}
	// Idempotent: a second pass has nothing to do.
	if stripToolsForCompaction(req) {
		t.Fatal("second strip reported a change on an already-clean request")
	}
}

// TestBP306HistoryUntouched the tool_use/tool_result pairs inside the history
// ARE the material being summarised; only the declarations go.
func TestBP306HistoryUntouched(t *testing.T) {
	history := []ir.Message{
		userTurn("run it"),
		{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "c1", Name: "Bash", Arguments: `{"command":"ls"}`}}},
		{Role: ir.RoleTool, ToolCallID: "c1", ToolName: "Bash", Content: []ir.ContentPart{{Kind: "text", Text: "a.go"}}},
		userTurn(compactInstruction),
	}
	req := &ir.ChatRequest{Model: "m", Messages: history, Tools: []ir.ToolDef{{Name: "Bash"}}}

	if !isCompactionRequest(req) {
		t.Fatal("compaction not detected alongside tool history")
	}
	stripToolsForCompaction(req)

	if len(req.Messages) != len(history) {
		t.Fatalf("history length changed: %d → %d", len(history), len(req.Messages))
	}
	if len(req.Messages[1].ToolCalls) != 1 || req.Messages[1].ToolCalls[0].Name != "Bash" {
		t.Fatal("history tool_use was stripped; the summary would lose its material")
	}
	if req.Messages[2].ToolCallID != "c1" {
		t.Fatal("history tool_result was stripped")
	}
}

// TestBP306ExecutionStripsBeforeDispatch runs the real Execute path and checks
// what the DRIVER actually received — helper-level assertions cannot catch a
// missing wire-up in Execute (BP-112: tests that only exercise the helper pass
// while production never calls it).
func TestBP306ExecutionStripsBeforeDispatch(t *testing.T) {
	cases := []struct {
		name     string
		last     ir.Message
		wantTool int
	}{
		{"compaction", userTurn(compactInstruction), 0},
		{"ordinary turn", userTurn("now fix the CSS"), 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			drv := &compactFake{}
			g := newCompactGateway(t, drv)

			if _, err := g.Execute(context.Background(), tooledRequest(tc.last), func(ir.StreamEvent) error { return nil }); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if len(drv.seen) == 0 {
				t.Fatal("driver was never called")
			}
			got := drv.seen[0]
			if len(got.Tools) != tc.wantTool {
				t.Fatalf("driver saw %d tools, want %d", len(got.Tools), tc.wantTool)
			}
			if tc.wantTool == 0 && got.ToolChoice != nil {
				t.Fatalf("driver saw a leftover tool_choice %v on a tool-free request", got.ToolChoice)
			}
		})
	}
}

// TestBP306LogRowCountsWhatLeft the request log must record that the tools did
// not leave (tools_declared=0) — that column is how this went unnoticed.
func TestBP306LogRowCountsWhatLeft(t *testing.T) {
	drv := &compactFake{}
	g := newCompactGateway(t, drv)
	res, err := g.Execute(context.Background(), tooledRequest(userTurn(compactInstruction)), func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.ToolsDeclared != 0 {
		t.Fatalf("ToolsDeclared = %d, want 0 (log would claim tools were sent)", res.ToolsDeclared)
	}
}

// compactFake records the request each driver call received.
type compactFake struct {
	seen []*ir.ChatRequest
}

func (d *compactFake) Channel() string                          { return model.ChannelWorkBuddy }
func (d *compactFake) Models(context.Context) ([]string, error) { return []string{"m"}, nil }

func (d *compactFake) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.seen = append(d.seen, call.Request)
	return &ir.ChatResponse{StopReason: ir.StopEnd}, nil
}

// newCompactGateway builds a single-binding gateway over the fake driver.
func newCompactGateway(t *testing.T, drv *compactFake) *Gateway {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = []config.ModelBinding{
		{Canonical: "m", Channel: model.ChannelWorkBuddy, UpstreamModel: "m"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second},
		map[string]int{model.ChannelWorkBuddy: 1},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	p.Upsert(&model.Account{ID: "acct-1", Channel: model.ChannelWorkBuddy, CredsJSON: []byte(`{}`)})
	return New(cfg, p, router.New(cfg), map[string]driver.Driver{model.ChannelWorkBuddy: drv}, st)
}
