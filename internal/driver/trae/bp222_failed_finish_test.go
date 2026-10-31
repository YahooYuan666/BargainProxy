package trae

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// BP-222: trae's SOLO done event can carry finish_reason "error" — the
// vendor's word for a failed turn (magpie 86fef947 #2471). The old mapFinish
// default turned it into StopEnd: a half answer reported as a success. Both
// trae wire forms (SOLO named events and plain OpenAI chunks) must fail the
// attempt as ClassTransport.
func TestBP222SOLODoneFinishReasonErrorFailsTheTurn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event:output\ndata: {\"response\":\"partial\"}\n\n")
		fmt.Fprint(w, "event:done\ndata: {\"finish_reason\":\"error\"}\n\n")
	}))
	defer srv.Close()

	creds, _ := json.Marshal(map[string]any{"access_token": "jwt", "uid": "u1"})
	d := New(config.TraeSettings{AgentHost: srv.URL, UgHost: srv.URL}, nil)
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       &model.Account{ID: "trae:u1", CredsJSON: creds},
		UpstreamModel: "glm-5.2",
		Request: &ir.ChatRequest{Model: "glm-5.2", Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}, func(ev ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatalf("SOLO done finish_reason \"error\" must fail the attempt, got a success with stop=%q", resp.StopReason)
	}
	ue, ok := err.(*uperr.Error)
	if !ok || ue.Class != uperr.ClassTransport {
		t.Fatalf("want ClassTransport, got %T: %v", err, err)
	}
}

// The OpenAI-chunk fallback form carries the same word.
func TestBP222ChunkFinishReasonErrorFailsTheTurn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"partial"}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"error"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()

	creds, _ := json.Marshal(map[string]any{"access_token": "jwt", "uid": "u1"})
	d := New(config.TraeSettings{AgentHost: srv.URL, UgHost: srv.URL}, nil)
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       &model.Account{ID: "trae:u1", CredsJSON: creds},
		UpstreamModel: "glm-5.2",
		Request: &ir.ChatRequest{Model: "glm-5.2", Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}, func(ev ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("chunk-form finish_reason \"error\" must fail the attempt")
	}
	ue, ok := err.(*uperr.Error)
	if !ok || ue.Class != uperr.ClassTransport {
		t.Fatalf("want ClassTransport, got %T: %v", err, err)
	}
}
