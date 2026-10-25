// BP-107 end-to-end: a real WorkBuddy upstream that dies mid-answer must not
// end the client's turn. This goes through the actual HTTP handler and SSE
// renderer, so it also proves the client sees ONE well-formed stream (no second
// message_start, one terminal chunk, [DONE] once).
package server_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/workbuddy"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/server"
	"bargainproxy/internal/store"
)

// TestDroppedStreamIsContinuedEndToEnd drives the real HTTP surface.
//
// The fake upstream answers the first request with some text and then closes
// the connection WITHOUT a terminal frame — exactly the field shape
// (`wsarecv: An existing connection was forcibly closed by the remote host`,
// 2026-10-24). The second request (the continuation) answers normally.
func TestDroppedStreamIsContinuedEndToEnd(t *testing.T) {
	var calls atomic.Int32
	var contBody atomic.Value

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if n > 1 {
			contBody.Store(body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		if n == 1 {
			// First segment: plenty of text (past the sentinel buffer), then the
			// socket dies with no finish_reason and no [DONE].
			fmt.Fprintf(w, "data: %s\n\n", chunk("The first half of a long answer. "+strings.Repeat("padding ", 30)))
			if fl != nil {
				fl.Flush()
			}
			// Returning without a terminal frame is what a dropped socket looks
			// like to the reader: the body just ends.
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", chunk(" The second half."))
		fmt.Fprintf(w, "data: %s\n\n", usageChunk())
		fmt.Fprintln(w, "data: [DONE]")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Channels.WorkBuddy.ChatBaseCN = up.URL
	cfg.Models = []config.ModelBinding{
		{Canonical: "glm-5.3", Channel: model.ChannelWorkBuddy, UpstreamModel: "glm-5.3"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	creds, _ := json.Marshal(map[string]any{"access_token": testToken("continue"), "uid": "u-continue"})
	p.Upsert(&model.Account{ID: "wb-continue", Channel: model.ChannelWorkBuddy, CredsJSON: creds})
	drivers := map[string]driver.Driver{model.ChannelWorkBuddy: workbuddy.New(cfg.Channels.WorkBuddy, nil)}
	gw := gateway.New(cfg, p, router.New(cfg), drivers, st)
	srv := httptest.NewServer(server.New(cfg, gw).Handler())
	defer srv.Close()

	body := `{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"write a long answer"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (the turn must survive a dropped socket)", resp.StatusCode)
	}

	var text strings.Builder
	done := 0
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			done++
			continue
		}
		var doc struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &doc); err != nil {
			t.Fatalf("bad SSE frame %q: %v", payload, err)
		}
		if doc.Error != nil {
			t.Fatalf("client received an error frame after the drop: %s", doc.Error.Message)
		}
		for _, c := range doc.Choices {
			text.WriteString(c.Delta.Content)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read stream: %v", err)
	}

	if !strings.Contains(text.String(), "The first half") || !strings.Contains(text.String(), "The second half") {
		t.Fatalf("client text = %q; both halves must be present", text.String())
	}
	if done != 1 {
		t.Fatalf("[DONE] seen %d times, want exactly 1", done)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2 (initial + continuation)", calls.Load())
	}

	// The continuation must have replayed the already-sent text to the upstream
	// as an assistant turn — that is what makes the model resume instead of
	// starting over.
	raw, _ := contBody.Load().(map[string]any)
	if raw == nil {
		t.Fatal("continuation body was not captured")
	}
	msgs, _ := raw["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("continuation messages = %d, want at least 2", len(msgs))
	}
	last, _ := msgs[len(msgs)-2].(map[string]any)
	if last["role"] != "assistant" {
		t.Fatalf("second-to-last role = %v, want assistant", last["role"])
	}
	if c, _ := last["content"].(string); !strings.Contains(c, "The first half") {
		t.Fatalf("continuation did not replay the delivered text: %q", c)
	}
}

func chunk(text string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": text}}},
	})
	return string(b)
}

func usageChunk() string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 1000, "completion_tokens": 40},
	})
	return string(b)
}
