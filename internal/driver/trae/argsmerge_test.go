package trae

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
)

// TRAE-4 red-first tests (BP-98): SOLO tool-call arguments are snapshots that
// grow, not always fragments. The upstream may resend "everything so far"
// (punctuation or not) — a blind += welds snapshots into broken JSON and the
// agent tool turn dies downstream after a 200. Semantics translated from
// magpie-community/plugins packages/trae 0.2.19 (merge in parts(), resent(),
// done()).

// TestArgumentsSnapshotTakesPlaceOfPrefix: frame two is a snapshot that starts
// with frame one — it REPLACES it, not appends.
func TestArgumentsSnapshotTakesPlaceOfPrefix(t *testing.T) {
	prev := `{"path":"a}`
	next := `{"path":"a","x":1}`
	if got := mergeArgs(prev, next); got != next {
		t.Fatalf("snapshot must replace prefix: got %q", got)
	}
}

// TestArgumentsFragmentAppends: a true fragment (neither a snapshot nor a
// resend) still concatenates.
func TestArgumentsFragmentAppends(t *testing.T) {
	prev := `{"path":"a",`
	next := `"x":1}`
	if got := mergeArgs(prev, next); got != prev+next {
		t.Fatalf("true fragment must append: got %q", got)
	}
}

// TestArgumentsResentSnapshot: the upstream resends the whole arguments after
// a partial snapshot with different punctuation (plugins#24): same first key,
// next is a whole object, and prev+next is not whole — take next.
func TestArgumentsResentSnapshot(t *testing.T) {
	prev := `{"path": "a"`    // partial, unclosed
	next := `{ "path": "b" }` // whole object, same first key, different spacing
	if got := mergeArgs(prev, next); got != next {
		t.Fatalf("resent snapshot must replace: got %q", got)
	}
}

// TestArgumentsPrefixRetransmitIgnored: a retransmit of a strict prefix of
// prev (the stream restarted mid-snapshot) changes nothing. Reference
// semantics: `prev.startsWith(next)` is the only "nothing new" path — a
// shorter WHOLE frame after a whole prev is the newer snapshot and wins
// (magpie 0.2.19 done(prev) branch), so the stale case here must be the
// prefix shape.
func TestArgumentsPrefixRetransmitIgnored(t *testing.T) {
	prev := `{"path":"a","x":1}`
	next := `{"path":"a","x":1` // strict prefix, not whole
	if got := mergeArgs(prev, next); got != prev {
		t.Fatalf("prefix retransmit must be ignored: got %q", got)
	}
}

// TestArgumentsNewerSnapshotAfterWholePrev: prev is already a whole JSON
// value and a new frame arrives — the new frame is "the arguments so far"
// and wins (magpie 0.2.19 done(prev) branch).
func TestArgumentsNewerSnapshotAfterWholePrev(t *testing.T) {
	prev := `{"path":"a"}`
	next := `{"path":"a","x":1}`
	if got := mergeArgs(prev, next); got != next {
		t.Fatalf("newer snapshot after whole prev must win: got %q", got)
	}
}

// TestArgumentsEmptyNextNoop: an empty/absent arguments frame changes nothing.
func TestArgumentsEmptyNextNoop(t *testing.T) {
	if got := mergeArgs(`{"a":1}`, ""); got != `{"a":1}` {
		t.Fatalf("empty next must be a noop, got %q", got)
	}
}

// TestMergeToolDeltasEndToEnd: the SSE path with snapshot frames must yield
// ONE tool call whose arguments parse as JSON and carry the final values.
func TestMergeToolDeltasEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event:output\ndata: {\"response\":\"\",\"tool_calls\":{\"index\":0,\"id\":\"call_1\",\"function_call\":{\"name\":\"Read\",\"arguments\":\"{\\\"file_path\\\":\"}}}\n\n")
		fmt.Fprint(w, "event:output\ndata: {\"response\":\"\",\"tool_calls\":{\"index\":0,\"function_call\":{\"arguments\":\"{\\\"file_path\\\":\\\"D:/x.go\\\",\\\"limit\\\":10}\"}}}\n\n")
		fmt.Fprint(w, "event:done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n")
	}))
	defer srv.Close()

	creds, _ := json.Marshal(map[string]any{"access_token": "jwt", "uid": "u1"})
	d := New(config.TraeSettings{AgentHost: srv.URL, UgHost: srv.URL}, nil)
	var text strings.Builder
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       &model.Account{ID: "trae:u1", CredsJSON: creds},
		UpstreamModel: "glm-5.2",
		Request: &ir.ChatRequest{Model: "glm-5.2", Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
			text.WriteString(ev.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("want exactly one tool call, got %d", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	var parsed map[string]any
	if err := json.Unmarshal([]byte(tc.Arguments), &parsed); err != nil {
		t.Fatalf("final arguments must be valid JSON after snapshot merge: %q: %v", tc.Arguments, err)
	}
	if parsed["file_path"] != "D:/x.go" || parsed["limit"] != float64(10) {
		t.Fatalf("snapshot values lost: %q", tc.Arguments)
	}
}
