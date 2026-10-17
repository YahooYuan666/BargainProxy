// BP-47: an upstream model-scoped rate limit (workbuddy business code 6004)
// must surface as 429 rate_limit_error + Retry-After(seconds to reset), and
// the pool must remember the cooldown so follow-up requests fail fast with
// 400 model_unavailable instead of hammering upstream.
package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/model"
)

func TestUpstreamModelRateLimitSurfaces429WithRetryAfter(t *testing.T) {
	// Wall clock 6h ahead in CST — the shape upstream actually sends
	// ("将在 2026-10-16 22:16:56 UTC+8 重置").
	cst := time.FixedZone("CST", 8*3600)
	reset := time.Now().Add(6 * time.Hour).In(cst)
	resetWall := reset.Format("2006-01-02 15:04:05")
	upstreamMsg := fmt.Sprintf("您的使用量已超出频率限制，将在 %s UTC+8 重置，您也可以切换其他模型继续使用。", resetWall)

	up := &fakeWorkBuddy{servedBy: map[string]int{}}
	up.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.servedBy[r.Header.Get("Authorization")]++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"code\":6004,\"msg\":%q}\n", upstreamMsg)
	}))
	t.Cleanup(up.srv.Close)

	srv, p := buildWorkBuddyServer(t, up, []string{"tok-a"})
	body := `{"model":"glm-5.3","max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d body=%s, want 429", resp.StatusCode, raw)
	}
	ra := resp.Header.Get("Retry-After")
	if n, err := strconv.Atoi(ra); err != nil || n <= 0 {
		t.Fatalf("Retry-After = %q, want positive seconds", ra)
	}

	var errBody struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &errBody); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if errBody.Error.Type != "rate_limit_error" {
		t.Fatalf("anthropic error type = %q body=%s", errBody.Error.Type, raw)
	}
	for _, want := range []string{"upstream model rate limit", "glm-5.3", reset.Format(time.RFC3339), "频率限制"} {
		if !strings.Contains(errBody.Error.Message, want) {
			t.Fatalf("message %q missing %q", errBody.Error.Message, want)
		}
	}

	// The pool folded the 6004 into a model hold: the routing barrier is capped
	// (BP-49, default 2h) so a later request probes for an early recovery, while
	// the upstream reset is kept for the panel.
	accs := p.Accounts(model.ChannelWorkBuddy)
	if len(accs) != 1 {
		t.Fatalf("accounts = %d", len(accs))
	}
	p.Lock()
	mc, ok := accs[0].ModelCooldowns["glm-5.3"]
	p.Unlock()
	if !ok {
		t.Fatal("model hold not recorded")
	}
	if mc.Until.After(time.Now().Add(2*time.Hour+time.Minute)) || !mc.Until.After(time.Now().Add(2*time.Hour-time.Minute)) {
		t.Fatalf("routing hold = %v, want ~2h from now (capped)", mc.Until)
	}
	if reset.Sub(mc.ResetAt) > time.Minute || mc.ResetAt.Sub(reset) > time.Minute {
		t.Fatalf("upstream reset kept = %v, want %v", mc.ResetAt, reset)
	}
	if mc.Reason != "6004 model rate limit" {
		t.Fatalf("reason = %q", mc.Reason)
	}
	if !strings.Contains(mc.Message, "频率限制") {
		t.Fatalf("upstream message lost: %q", mc.Message)
	}

	// Follow-up request: fail fast, no upstream hit, switch-models guidance.
	// (Anthropic egress carries the class in the message, not the body keys —
	// assert the pool wording, which is what the user sees.)
	hits := up.servedBy["Bearer tok-a"]
	resp2, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post2: %v", err)
	}
	raw2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Fatalf("follow-up status = %d body=%s, want 400", resp2.StatusCode, raw2)
	}
	for _, want := range []string{"cooling down", "glm-5.3", "earliest unlock", mc.Until.Format(time.RFC3339)} {
		if !strings.Contains(string(raw2), want) {
			t.Fatalf("follow-up body %s missing %q", raw2, want)
		}
	}
	if up.servedBy["Bearer tok-a"] != hits {
		t.Fatalf("cooled model must not touch upstream again: hits %d -> %d", hits, up.servedBy["Bearer tok-a"])
	}
}
