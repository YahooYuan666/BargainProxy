package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/trae"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/server"
	"bargainproxy/internal/sessionfp"
	"bargainproxy/internal/store"
)

// sseFrameUpstream records the Authorization header of every chat call and
// answers with an OpenAI-style SSE stream.
func sseFrameUpstream(t *testing.T, hits map[string]int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.Header.Get("Authorization")]++
		sseOK(w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func buildTraeServer(t *testing.T, up *httptest.Server, tokens []string) (*httptest.Server, *pool.Pool) {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Channels.Trae.AgentHost = up.URL
	cfg.Channels.Trae.UgHost = up.URL
	cfg.Models = []config.ModelBinding{
		{Canonical: "glm-5.3", Channel: model.ChannelTrae, UpstreamModel: "glm-5.2"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second}, map[string]int{
		model.ChannelTrae: 1, // serial free tier
	}, func(acc *model.Account) { _ = st.SaveAccount(acc) })
	for i, tok := range tokens {
		creds, _ := json.Marshal(map[string]any{"access_token": tok, "uid": fmt.Sprintf("tu-%d", i)})
		p.Upsert(&model.Account{
			ID:        fmt.Sprintf("trae-%d", i),
			Channel:   model.ChannelTrae,
			CredsJSON: creds,
			UID:       fmt.Sprintf("tu-%d", i),
		})
	}
	drivers := map[string]driver.Driver{model.ChannelTrae: trae.New(cfg.Channels.Trae, nil)}
	gw := gateway.New(cfg, p, router.New(cfg), drivers, st)
	srv := httptest.NewServer(server.New(cfg, gw).Handler())
	t.Cleanup(srv.Close)
	return srv, p
}

// TestStickyFingerprintWorkBuddy: a no-explicit-id conversation must stay on
// one account across turns (level-3 fingerprint), and unbind+reselect once
// that account is cooled for the model.
func TestStickyFingerprintWorkBuddy(t *testing.T) {
	up := newFakeWorkBuddy(t)
	srv, p := buildWorkBuddyServer(t, up, []string{"tok-a", "tok-b"})

	turn1 := `{"model":"glm-5.3","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello world"}]}`
	turn2 := `{"model":"glm-5.3","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello world"},{"role":"assistant","content":"hi"},{"role":"user","content":"follow up"}]}`

	post := func(body string) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}

	post(turn1)
	first := servedToken(up.servedBy)
	if first == "" {
		t.Fatal("no upstream call recorded for turn 1")
	}

	// Turn 2 (same prefix, longer tail): must land on the same account.
	post(turn2)
	if got := up.servedBy[first]; got != 2 {
		t.Fatalf("sticky drift on workbuddy: %v", up.servedBy)
	}

	// Cool the serving account for glm-5.3; turn 3 must switch and rebind.
	acc := accountForToken(t, p, model.ChannelWorkBuddy, first)
	p.ApplyResult(acc, "glm-5.3", uperrModelRate("glm-5.3", time.Now().Add(2*time.Hour)))
	post(turn2)
	second := otherToken(up.servedBy, first)
	if second == "" || up.servedBy[second] != 1 {
		t.Fatalf("cooled account must hand over: %v", up.servedBy)
	}
	key := sessionfp.Prefix("sys", "hello world")
	if id, ok := p.SessionBinding(model.ChannelWorkBuddy, key); !ok || id != accForTokenID(t, p, model.ChannelWorkBuddy, second) {
		t.Fatalf("binding not moved to the new account: %q %v (second=%s)", id, ok, second)
	}
}

// TestStickyFingerprintTrae: same two-turn contract on the serial Trae
// channel (concurrency 1 makes drift the default failure mode).
func TestStickyFingerprintTrae(t *testing.T) {
	hits := map[string]int{}
	up := sseFrameUpstream(t, hits)
	srv, p := buildTraeServer(t, up, []string{"ttok-a", "ttok-b"})

	turn1 := `{"model":"glm-5.3","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello world"}]}`
	turn2 := `{"model":"glm-5.3","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello world"},{"role":"assistant","content":"hi"},{"role":"user","content":"follow up"}]}`

	post := func(body string) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}

	post(turn1)
	first := servedToken(hits)
	if first == "" {
		t.Fatal("no upstream call recorded for turn 1")
	}

	post(turn2)
	if got := hits[first]; got != 2 {
		t.Fatalf("sticky drift on trae: %v", hits)
	}

	acc := accountForToken(t, p, model.ChannelTrae, first)
	p.ApplyResult(acc, "glm-5.3", uperrModelRate("glm-5.3", time.Now().Add(12*time.Hour)))
	post(turn2)
	second := otherToken(hits, first)
	if second == "" || hits[second] != 1 {
		t.Fatalf("cooled trae account must hand over: %v", hits)
	}
	key := sessionfp.Prefix("sys", "hello world")
	if id, ok := p.SessionBinding(model.ChannelTrae, key); !ok || id != accForTokenID(t, p, model.ChannelTrae, second) {
		t.Fatalf("trae binding not moved: %q %v (second=%s)", id, ok, second)
	}
}

// --- helpers ---

func servedToken(hits map[string]int) string {
	for tok, n := range hits {
		if n > 0 {
			return tok
		}
	}
	return ""
}

func otherToken(hits map[string]int, first string) string {
	for tok := range hits {
		if tok != first {
			return tok
		}
	}
	return ""
}

// accountForToken maps an Authorization header back to the account whose
// credential produced it.
func accountForToken(t *testing.T, p *pool.Pool, channel, auth string) *model.Account {
	t.Helper()
	id := accForTokenID(t, p, channel, auth)
	for _, a := range p.Accounts(channel) {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no account for auth %q", auth)
	return nil
}

func accForTokenID(t *testing.T, p *pool.Pool, channel, auth string) string {
	t.Helper()
	// Driver auth shapes: workbuddy "Bearer <token>"; trae "Cloud-IDE-JWT <token>".
	tok := auth
	tok = strings.TrimPrefix(tok, "Bearer ")
	tok = strings.TrimPrefix(tok, "Cloud-IDE-JWT ")
	for _, a := range p.Accounts(channel) {
		var creds map[string]any
		if json.Unmarshal(a.CredsJSON, &creds) != nil {
			continue
		}
		if s, _ := creds["access_token"].(string); s == tok {
			return a.ID
		}
	}
	t.Fatalf("no account matches token %q", tok)
	return ""
}
