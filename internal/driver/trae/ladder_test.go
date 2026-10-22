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

// TestFunctionLadderRetriesOnWrongFunction: the upstream answers 400 with a
// business code when a SOLO function will not take the payload; the driver must
// walk the ladder instead of failing the turn (2026-10-21 field bug
// msg_5cdb126709: the same session answered 200 seconds earlier).
func TestFunctionLadderRetriesOnWrongFunction(t *testing.T) {
	var tried []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fn, _ := body["function"].(string)
		tried = append(tried, fn)
		if fn != functionsTrae[len(functionsTrae)-1] {
			// Refuse every function but the last one.
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"code":4001,"message":"bad request: json: cannot unmarshal"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event:output\ndata: {\"response\":\"ok\"}\n\n")
		fmt.Fprint(w, "event:done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	}))
	defer srv.Close()

	creds, _ := json.Marshal(map[string]any{"access_token": "jwt", "uid": "u1"})
	d := New(config.TraeSettings{AgentHost: srv.URL, UgHost: srv.URL}, nil)
	call := driver.ChatCall{
		Account:       &model.Account{ID: "trae:u1", CredsJSON: creds},
		UpstreamModel: "deepseek-v4.1-flash",
		Request: &ir.ChatRequest{
			Model:    "deepseek-v4.1-flash",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
			Stream:   true,
		},
	}
	var got strings.Builder
	resp, err := d.Stream(context.Background(), call, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
			got.WriteString(ev.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v (tried %v)", err, tried)
	}
	if got.String() != "ok" {
		t.Fatalf("text = %q", got.String())
	}
	if resp == nil {
		t.Fatal("nil response")
	}
	if len(tried) != len(functionsTrae) {
		t.Fatalf("expected the whole ladder to be walked, tried %v", tried)
	}
	if tried[0] != functionsTrae[0] {
		t.Fatalf("first attempt must be the preferred function, got %q", tried[0])
	}
}

// TestFunctionLadderStopsOnRealBadRequest: a 400 that is NOT a function refusal
// must not be retried four times — a genuinely broken payload stays broken.
func TestFunctionLadderStopsOnRealBadRequest(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":9999,"message":"something else entirely"}`)
	}))
	defer srv.Close()

	creds, _ := json.Marshal(map[string]any{"access_token": "jwt", "uid": "u1"})
	d := New(config.TraeSettings{AgentHost: srv.URL, UgHost: srv.URL}, nil)
	call := driver.ChatCall{
		Account:       &model.Account{ID: "trae:u1", CredsJSON: creds},
		UpstreamModel: "deepseek-v4.1-flash",
		Request: &ir.ChatRequest{
			Model:    "deepseek-v4.1-flash",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
			Stream:   true,
		},
	}
	if _, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil }); err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Fatalf("non-function refusal must not be retried, calls=%d", calls)
	}
}
