package trae

// Single-surface contract, adopted 2026-11-23.
//
// The driver used to split requests across two upstream surfaces: tool-bearing
// and small prompts stayed on llm_utils_chat, tool-free prompts above the
// (then believed) 262144 ceiling were routed to /api/remote/v1/chat_sessions.
// BP-103 introduced that split; both of its premises were later disproved:
//
//   - llm_utils_chat was re-measured on 2026-11-01 carrying ~1.02M tokens WITH
//     tools (1,020,179 passed with the needle echoed back; 1,800,000 answered
//     4022), so the 262144 wall the split dodged never existed.
//   - chat_sessions runs the vendor's OWN agent and ignores caller tool
//     declarations, so routing "large" traffic there silently changed how
//     answers were produced.
//
// The branch was also dead: it fired only for tool-free prompts above ~1M and
// no such request ever reached this driver.
//
// These tests pin the replacement contract:
//   1. No local size gate — a huge tool-bearing prompt is DISPATCHED.
//   2. The upstream's own overflow verdict is surfaced faithfully, carrying
//      ContextOverflow so the protocol writer can tell the client to compact.
//   3. Nothing routes to the removed session surface.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// bigToolRequest builds a prompt far larger than any historical gate, with a
// tool declaration. 4.5M text characters ≈ 1.5M estimated tokens, above the
// measured ~1.02M wall.
func bigToolRequest() *ir.ChatRequest {
	return &ir.ChatRequest{
		Model: "deepseek-v4.1-flash",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{
			{Kind: "text", Text: strings.Repeat("a", 4500000)},
		}}},
		Tools: []ir.ToolDef{{Name: "bash"}},
	}
}

// TestNoLocalSizeGateDispatchesHugePrompts pins contract point 1.
//
// The driver must get PAST any local check and reach the network. Pointed at an
// unroutable host, a dispatched request fails in TRANSPORT; a gated one would
// fail as model_unavailable with a ceiling message. That difference is what this
// asserts — no live upstream needed.
func TestNoLocalSizeGateDispatchesHugePrompts(t *testing.T) {
	d := testDriver("http://127.0.0.1:1", "http://127.0.0.1:1")
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "deepseek-v4.1-flash",
		Request:       bigToolRequest(),
	}, func(ir.StreamEvent) error { return nil })

	if err == nil {
		t.Fatal("expected a transport failure against an unroutable host")
	}
	ue, ok := uperr.As(err)
	if !ok {
		t.Fatalf("want *uperr.Error, got %T: %v", err, err)
	}
	if ue.Class == uperr.ClassModelUnavailable {
		t.Fatalf("a huge tool request was refused locally instead of dispatched: %v", err)
	}
	if strings.Contains(strings.ToLower(ue.Message), "ceiling") {
		t.Fatalf("a size-ceiling refusal is still in the dispatch path: %v", err)
	}
}

// TestUpstreamOverflowIsSurfacedWithContextOverflow pins contract point 2.
//
// 4022 ("your prompt tokens have exceeded the maximum limit") and 4026
// ("context length has exceeded the maximum limit") are the upstream's own
// verdicts. The driver must pass them through as a request-level 400 that
// carries ContextOverflow — a client that acts on `context_length_exceeded`
// compacts and retries; without the flag ZCode read the bare 400 as
// reason=unknown and resent the same oversized body (field 2026-11-23).
func TestUpstreamOverflowIsSurfacedWithContextOverflow(t *testing.T) {
	for _, code := range []int{4022, 4026} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				msg := "context length has exceeded the maximum limit"
				if code == 4022 {
					msg = "your prompt tokens have exceeded the maximum limit"
				}
				fmt.Fprintf(w, "event:error\ndata: {\"code\":%d,\"message\":%q}\n\n", code, msg)
			}))
			defer srv.Close()

			creds, _ := json.Marshal(map[string]any{"access_token": "jwt", "uid": "u1"})
			d := New(config.TraeSettings{AgentHost: srv.URL, UgHost: srv.URL}, nil)
			_, err := d.Stream(context.Background(), driver.ChatCall{
				Account:       &model.Account{ID: "trae:u1", CredsJSON: creds},
				UpstreamModel: "deepseek-v4.1-flash",
				Request: &ir.ChatRequest{Model: "deepseek-v4.1-flash", Messages: []ir.Message{
					{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
					Tools: []ir.ToolDef{{Name: "bash"}},
				},
			}, func(ir.StreamEvent) error { return nil })

			if err == nil {
				t.Fatalf("code %d must fail the turn", code)
			}
			ue, ok := uperr.As(err)
			if !ok {
				t.Fatalf("want *uperr.Error, got %T: %v", err, err)
			}
			if ue.Class != uperr.ClassBadRequest {
				t.Fatalf("class = %s, want %s (request-level, no account penalty)", ue.Class, uperr.ClassBadRequest)
			}
			if !ue.ContextOverflow {
				t.Fatalf("code %d did not set ContextOverflow; a client cannot tell this is compaction-worthy: %v", code, err)
			}
			if ue.Status != 400 {
				t.Fatalf("status = %d, want 400", ue.Status)
			}
		})
	}
}

// TestNoSessionSurfaceRemains pins contract point 3: the removed surface must
// not be reachable. A tool-free prompt far above the old ceiling dispatches to
// llm_utils_chat like everything else — it must NOT create a session.
func TestNoSessionSurfaceRemains(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event:output\ndata: {\"response\":\"ok\"}\n\n")
		fmt.Fprint(w, "event:done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	}))
	defer srv.Close()

	creds, _ := json.Marshal(map[string]any{"access_token": "jwt", "uid": "u1"})
	d := New(config.TraeSettings{AgentHost: srv.URL, UgHost: srv.URL}, nil)
	// Tool-FREE and ~1.5M estimated tokens: exactly the shape the old split
	// routed to chat_sessions.
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       &model.Account{ID: "trae:u1", CredsJSON: creds},
		UpstreamModel: "glm-5.2",
		Request: &ir.ChatRequest{Model: "glm-5.2", Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{
				{Kind: "text", Text: strings.Repeat("a", 4500000)},
			}}},
		},
	}, func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("a large tool-free prompt must be served by llm_utils_chat, got: %v", err)
	}
	for _, p := range paths {
		if strings.Contains(p, "chat_sessions") {
			t.Fatalf("request reached the removed session surface: %v", paths)
		}
	}
	if len(paths) == 0 {
		t.Fatal("no upstream call was made")
	}
	if !strings.Contains(paths[0], "llm_utils_chat") {
		t.Fatalf("first call went to %q, want llm_utils_chat", paths[0])
	}
}

// TestProbeEstimatorCountsOnlyWhatIsSent pins the diagnostic estimator's
// semantics after the 2026-11-23 field failure.
//
// The estimator is diagnostic-only now, but it must still be honest: it counts
// what buildSOLOBody WRITES. History thinking is not written, so counting it
// inflated a real request 2.85× (393434 real tokens read as 1120971) and that
// inflated number was what refused the turn.
func TestProbeEstimatorCountsOnlyWhatIsSent(t *testing.T) {
	base := &ir.ChatRequest{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: strings.Repeat("a", 3000)}}},
	}}
	withThinking := &ir.ChatRequest{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: strings.Repeat("a", 3000)}}},
		{Role: ir.RoleAssistant,
			Thinking: []ir.ThinkingBlock{{Text: strings.Repeat("z", 300000)}},
			Content:  []ir.ContentPart{{Kind: "text", Text: "done"}}},
	}}

	plain := ProbeEstimator(base)
	thinky := ProbeEstimator(withThinking)
	if plain <= 0 {
		t.Fatalf("estimate of a plain prompt = %d, want > 0", plain)
	}
	// Thinking text must not move the number: +300k thinking chars may only
	// show up as the 4 characters of "done".
	if thinky-plain > 10 {
		t.Fatalf("thinking inflated the estimate by %d tokens (plain=%d, withThinking=%d); "+
			"buildSOLOBody never sends thinking", thinky-plain, plain, thinky)
	}

	// Images cost a flat allowance; base64 length is not a token count.
	withImage := &ir.ChatRequest{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{
			{Kind: "text", Text: "hi"},
			{Kind: "image", ImageMIME: "image/png", ImageB64: strings.Repeat("x", 500000)},
		}},
	}}
	if got := ProbeEstimator(withImage); got > 2000 {
		t.Fatalf("image estimate = %d; base64 length must not be counted as tokens", got)
	}

	if got := ProbeEstimator(nil); got != 0 {
		t.Fatalf("nil request = %d, want 0", got)
	}
}

// TestMeasuredCeilingStaysOnTheRecord keeps the measurement from being lost
// now that nothing enforces it. The number is referenced by ProbeEstimator's
// documentation and by the comment block in trae.go; if someone changes it
// they must go re-measure.
func TestMeasuredCeilingStaysOnTheRecord(t *testing.T) {
	if traeMeasuredPromptCeiling != 1024000 {
		t.Fatalf("measured ceiling = %d, want the 2026-11-01 measurement 1024000", traeMeasuredPromptCeiling)
	}
}
