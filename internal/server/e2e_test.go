package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/antigravity"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/server"
	"bargainproxy/internal/store"
)

// fakeUpstream is a scripted antigravity upstream: it serves SSE frames and
// can be told to fail with a status for the first N requests.
type fakeUpstream struct {
	srv        *httptest.Server
	failFirst  int
	failStatus int
	requests   int
	lastBody   map[string]any
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests++
		if f.failFirst > 0 {
			f.failFirst--
			w.WriteHeader(f.failStatus)
			if f.failStatus == 429 {
				fmt.Fprint(w, `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"60s"}]}}`)
			} else {
				fmt.Fprint(w, `{"error":{"message":"scripted failure"}}`)
			}
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.lastBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1}}}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func buildTestServer(t *testing.T, up *fakeUpstream, accounts int) (*httptest.Server, *pool.Pool) {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Channels.Antigravity.BaseURL = up.srv.URL
	cfg.Channels.Antigravity.DailyURL = up.srv.URL
	cfg.Channels.Antigravity.TokenURL = up.srv.URL + "/token"
	cfg.Channels.Antigravity.MaxInFlight = 3
	cfg.Models = []config.ModelBinding{
		{Canonical: "gemini-3-pro", Channel: model.ChannelAntigravity, UpstreamModel: "gemini-pro-agent"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 3 * time.Second}, map[string]int{
		model.ChannelAntigravity: 3,
	}, func(acc *model.Account) { _ = st.SaveAccount(acc) })

	for i := 0; i < accounts; i++ {
		creds := map[string]any{
			"access_token":  strings.Join([]string{"tok", fmt.Sprint(i)}, "-"),
			"refresh_token": strings.Join([]string{"ref", fmt.Sprint(i)}, "-"),
			"expiry":        time.Now().Add(24 * time.Hour).Format(time.RFC3339),
			"project_id":    "p",
		}
		raw, _ := json.Marshal(creds)
		p.Upsert(&model.Account{
			ID:        fmt.Sprintf("acct-%d", i),
			Channel:   model.ChannelAntigravity,
			CredsJSON: raw,
		})
	}

	drivers := map[string]driver.Driver{
		model.ChannelAntigravity: antigravity.New(cfg.Channels.Antigravity, nil),
	}
	r := router.New(cfg)
	gw := gateway.New(cfg, p, r, drivers, st)
	srv := httptest.NewServer(server.New(cfg, gw).Handler())
	t.Cleanup(srv.Close)
	return srv, p
}

func TestChatCompletionsNonStreaming(t *testing.T) {
	up := newFakeUpstream(t)
	srv, _ := buildTestServer(t, up, 1)

	body := `{"model":"gemini-3-pro","messages":[{"role":"user","content":"ping"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "pong" {
		t.Fatalf("choices = %+v", out.Choices)
	}
	if out.Usage.PromptTokens != 3 || out.Usage.CompletionTokens != 1 {
		t.Fatalf("usage = %+v", out.Usage)
	}
}

func TestChatCompletionsStreaming(t *testing.T) {
	up := newFakeUpstream(t)
	srv, _ := buildTestServer(t, up, 1)

	body := `{"model":"gemini-3-pro","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"ping"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	sc := bufio.NewScanner(resp.Body)
	var text string
	var sawDone, sawUsage, sawFinish bool
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens int `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", payload, err)
		}
		for _, c := range chunk.Choices {
			text += c.Delta.Content
			if c.FinishReason != nil {
				sawFinish = true
			}
		}
		if chunk.Usage != nil && chunk.Usage.PromptTokens == 3 {
			sawUsage = true
		}
	}
	if text != "pong" {
		t.Fatalf("streamed text = %q", text)
	}
	if !sawFinish || !sawDone {
		t.Fatalf("missing finish/done: finish=%v done=%v", sawFinish, sawDone)
	}
	if !sawUsage {
		t.Fatal("include_usage requested but no usage chunk seen")
	}
}

func TestQuotaExhaustedAndRotation(t *testing.T) {
	// First upstream call fails 429; gateway must rotate to account two and
	// succeed. Note: rotation is per-account; the fake fails only once so the
	// second account's request succeeds.
	up := newFakeUpstream(t)
	up.failFirst = 1
	up.failStatus = 429
	srv, p := buildTestServer(t, up, 2)

	body := `{"model":"gemini-3-pro","messages":[{"role":"user","content":"ping"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw := make([]byte, 512)
		n, _ := resp.Body.Read(raw)
		t.Fatalf("status %d body %s", resp.StatusCode, raw[:n])
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Choices) == 0 || out.Choices[0].Message.Content != "pong" {
		t.Fatalf("expected rotated success, got %+v", out)
	}
	if up.requests < 2 {
		t.Fatalf("expected 2 upstream attempts, got %d", up.requests)
	}
	// BP-125: an antigravity 429 holds the MODEL, not the account — Gemini
	// and the 3P family run separate quota ledgers upstream, so a Gemini 429
	// must not freeze the account's healthy Claude quota. The failing
	// account must carry exactly one model-scoped hold (keyed by the
	// UPSTREAM model id, e.g. gemini-pro-agent) and no account-level
	// cooldown; re-dispatching the same model there would just 429 again.
	cooling := 0
	for _, a := range p.Accounts(model.ChannelAntigravity) {
		if time.Now().Before(a.AccountCooldownUntil) {
			cooling++
		}
		for k, mc := range a.ModelCooldowns {
			if time.Now().Before(mc.Until) {
				cooling++
				t.Logf("model hold: acct=%s model=%s until=%s", a.ID, k, mc.Until)
			}
		}
	}
	if cooling != 1 {
		t.Fatalf("expected exactly 1 cooling scope (model-level since BP-125), got %d", cooling)
	}
}

func TestUnknownModel404(t *testing.T) {
	up := newFakeUpstream(t)
	srv, _ := buildTestServer(t, up, 1)
	body := `{"model":"no-such-model","messages":[{"role":"user","content":"x"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestUnsupportedImageURLRejected(t *testing.T) {
	up := newFakeUpstream(t)
	srv, _ := buildTestServer(t, up, 1)
	body := `{"model":"gemini-3-pro","messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400 (no server-side image fetch)", resp.StatusCode)
	}
}

func TestModelsEndpoint(t *testing.T) {
	up := newFakeUpstream(t)
	srv, _ := buildTestServer(t, up, 1)
	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "gemini-3-pro" || out.Data[0].OwnedBy != model.ChannelAntigravity {
		t.Fatalf("models = %+v", out.Data)
	}
}

func TestSessionStickinessEndToEnd(t *testing.T) {
	up := newFakeUpstream(t)
	srv, p := buildTestServer(t, up, 2)

	body := `{"model":"gemini-3-pro","conversation_id":"conv-42","messages":[{"role":"user","content":"ping"}]}`
	for i := 0; i < 3; i++ {
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
		resp.Body.Close()
	}
	bound, ok := p.SessionBinding(model.ChannelAntigravity, "conv:conv-42")
	if !ok {
		t.Fatal("expected a session binding after 3 requests")
	}
	_ = context.Background()
	if bound == "" {
		t.Fatal("empty binding")
	}
}
