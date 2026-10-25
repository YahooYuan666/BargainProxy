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
)

// BP-108: the ladder must contain the function the vendor catalogue actually
// names for the 1M models, and the tail entries must not disturb the order the
// existing four rungs are tried in.
//
// The catalogue we captured on 2026-10-23 (research/2026-10-24-trae-dual-channel-1m.md
// §2.1) lists every {dev:200000, max:1000000} model under `fn=solo_agent_remote`.
// The ladder shipped four functions and never asked that one, so the note
// "we tried every function" was wrong: the only function the catalogue points
// at was missing.
func TestLadderCarriesCatalogueFunctionAndKeepsOrder(t *testing.T) {
	// The four original rungs keep their positions: a new tail entry may only
	// add a fallback, never change which function answers an existing request.
	wantHead := []string{"solo_work_lite", "chat_v3", "solo_agent", "solo_agent_lite"}
	if len(functionsTrae) < len(wantHead) {
		t.Fatalf("ladder shrank to %v", functionsTrae)
	}
	for i, want := range wantHead {
		if functionsTrae[i] != want {
			t.Fatalf("rung %d = %q, want %q (order must not change: %v)",
				i, functionsTrae[i], want, functionsTrae)
		}
	}
	tail := functionsTrae[len(wantHead):]
	has := func(name string) bool {
		for _, f := range tail {
			if f == name {
				return true
			}
		}
		return false
	}
	if !has("solo_agent_remote") {
		t.Fatalf("ladder is missing solo_agent_remote, the function the catalogue names for 1M models: %v", functionsTrae)
	}
	if !has("solo_coder") {
		t.Fatalf("ladder is missing solo_coder: %v", functionsTrae)
	}
}

// BP-108: SOLO reports the prefix-cache hit as `cache_read_input_tokens` at the
// top level of token_usage. The driver only decoded the three counts, so the
// cache column read "unknown" for every Trae request even when the upstream
// reported a hit — the channel looked like it published no cache data at all.
func TestTokenUsageCacheReadIsRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event:output\ndata: {\"response\":\"ok\"}\n\n")
		fmt.Fprint(w, "event:token_usage\ndata: {\"prompt_tokens\":1000,\"completion_tokens\":20,\"reasoning_tokens\":0,\"cache_read_input_tokens\":768}\n\n")
		fmt.Fprint(w, "event:done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	}))
	defer srv.Close()

	resp := runOneTraeStream(t, srv.URL)
	if !resp.Usage.CacheKnown {
		t.Fatal("cache_read_input_tokens present but CacheKnown is false: the panel would show 0% instead of a hit")
	}
	if resp.Usage.CachedInputTokens != 768 {
		t.Fatalf("cached = %d, want 768", resp.Usage.CachedInputTokens)
	}
	if resp.Usage.InputTokens != 1000 {
		t.Fatalf("input = %d, want 1000 (the FULL prompt, cache hits included)", resp.Usage.InputTokens)
	}
}

// A token_usage without any cache field must stay UNKNOWN, not become a 0%
// hit rate: "not reported" and "reported zero" are different answers (BP-53).
func TestTokenUsageWithoutCacheStaysUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event:output\ndata: {\"response\":\"ok\"}\n\n")
		fmt.Fprint(w, "event:token_usage\ndata: {\"prompt_tokens\":500,\"completion_tokens\":10}\n\n")
		fmt.Fprint(w, "event:done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	}))
	defer srv.Close()

	resp := runOneTraeStream(t, srv.URL)
	if resp.Usage.CacheKnown {
		t.Fatalf("no cache field in token_usage, yet CacheKnown is true (cached=%d)", resp.Usage.CachedInputTokens)
	}
}

// A reported zero IS a hit rate of zero and must be marked known.
func TestTokenUsageCacheZeroIsKnown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event:output\ndata: {\"response\":\"ok\"}\n\n")
		fmt.Fprint(w, "event:token_usage\ndata: {\"prompt_tokens\":500,\"completion_tokens\":10,\"cache_read_input_tokens\":0}\n\n")
		fmt.Fprint(w, "event:done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	}))
	defer srv.Close()

	resp := runOneTraeStream(t, srv.URL)
	if !resp.Usage.CacheKnown || resp.Usage.CachedInputTokens != 0 {
		t.Fatalf("reported zero must be known/0, got known=%v cached=%d",
			resp.Usage.CacheKnown, resp.Usage.CachedInputTokens)
	}
}

// The OpenAI spelling is accepted too, so an upstream move to
// prompt_tokens_details.cached_tokens keeps working.
func TestTokenUsageOpenAICacheShapeAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event:output\ndata: {\"response\":\"ok\"}\n\n")
		fmt.Fprint(w, "event:token_usage\ndata: {\"prompt_tokens\":900,\"completion_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":640}}\n\n")
		fmt.Fprint(w, "event:done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	}))
	defer srv.Close()

	resp := runOneTraeStream(t, srv.URL)
	if !resp.Usage.CacheKnown || resp.Usage.CachedInputTokens != 640 {
		t.Fatalf("openai shape: known=%v cached=%d, want true/640",
			resp.Usage.CacheKnown, resp.Usage.CachedInputTokens)
	}
}

func runOneTraeStream(t *testing.T, host string) *ir.ChatResponse {
	t.Helper()
	creds, _ := json.Marshal(map[string]any{"access_token": "jwt", "uid": "u1"})
	d := New(config.TraeSettings{AgentHost: host, UgHost: host}, nil)
	call := driver.ChatCall{
		Account:       &model.Account{ID: "trae:u1", CredsJSON: creds},
		UpstreamModel: "deepseek-v4.1-flash",
		Request: &ir.ChatRequest{
			Model:    "deepseek-v4.1-flash",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
			Stream:   true,
		},
	}
	resp, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	return resp
}
