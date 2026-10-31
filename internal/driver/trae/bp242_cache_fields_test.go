package trae

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// BP-242: Trae's token_usage must read DeepSeek's two cache spellings.
//
// plugins trae 0.2.26 (magpie #3991/#3979): DeepSeek reports the prefix-cache
// hit as a flat `prompt_cache_hit_tokens` (or a bare top-level `cached_tokens`),
// and Trae stopped sending `cache_read_input_tokens` around 10-28. Decoding
// only the old field made every DeepSeek request on this channel read as
// "cache unknown" while the upstream WAS reporting a hit.
//
// These run through the real Stream path (runOneTraeStream), not the parser in
// isolation: the field has to survive the whole driver, which is where the
// previous cache fixes actually failed.
func TestTokenUsageReadsDeepSeekCacheFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage string
		want  int
	}{
		{"prompt_cache_hit_tokens (DeepSeek flat)", `{"prompt_tokens":3000,"completion_tokens":2,"prompt_cache_hit_tokens":2048}`, 2048},
		{"cached_tokens (bare top level)", `{"prompt_tokens":3000,"completion_tokens":2,"cached_tokens":2048}`, 2048},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event:output\ndata: {\"response\":\"ok\"}\n\n")
				fmt.Fprintf(w, "event:token_usage\ndata: %s\n\n", tc.usage)
				fmt.Fprint(w, "event:done\ndata: {\"finish_reason\":\"stop\"}\n\n")
			}))
			defer srv.Close()

			resp := runOneTraeStream(t, srv.URL)
			if !resp.Usage.CacheKnown {
				t.Fatalf("CacheKnown=false: the upstream DID name a cache count in %s", tc.usage)
			}
			if resp.Usage.CachedInputTokens != tc.want {
				t.Fatalf("CachedInputTokens = %d, want %d", resp.Usage.CachedInputTokens, tc.want)
			}
		})
	}
}

// The pre-existing spellings must keep working — this is the regression guard
// for the fix above, not a restatement of it.
func TestTokenUsageOldCacheSpellingsStillWork(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage string
	}{
		{"cache_read_input_tokens (SOLO's own)", `{"prompt_tokens":3000,"completion_tokens":2,"cache_read_input_tokens":2048}`},
		{"prompt_tokens_details.cached_tokens (OpenAI shape)", `{"prompt_tokens":3000,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":2048}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event:output\ndata: {\"response\":\"ok\"}\n\n")
				fmt.Fprintf(w, "event:token_usage\ndata: %s\n\n", tc.usage)
				fmt.Fprint(w, "event:done\ndata: {\"finish_reason\":\"stop\"}\n\n")
			}))
			defer srv.Close()

			resp := runOneTraeStream(t, srv.URL)
			if !resp.Usage.CacheKnown || resp.Usage.CachedInputTokens != 2048 {
				t.Fatalf("known=%v cached=%d, want known/2048", resp.Usage.CacheKnown, resp.Usage.CachedInputTokens)
			}
		})
	}
}
