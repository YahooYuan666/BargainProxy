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
	"bargainproxy/internal/driver/workbuddy"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/server"
	"bargainproxy/internal/store"
	"bargainproxy/internal/uperr"
)

func uperrModelRate(modelName string, until time.Time) *uperr.Error {
	e := uperr.New(uperr.ClassModelRate, 200, "scripted 6004")
	e.Model = modelName
	e.ResetAt = until
	return e
}

// fakeWorkBuddy scripts an OpenAI-variant SSE upstream and records which
// account (via Authorization header) served each request.
type fakeWorkBuddy struct {
	srv      *httptest.Server
	servedBy map[string]int // auth header -> count
}

func newFakeWorkBuddy(t *testing.T) *fakeWorkBuddy {
	t.Helper()
	f := &fakeWorkBuddy{servedBy: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		f.servedBy[auth]++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func buildWorkBuddyServer(t *testing.T, up *fakeWorkBuddy, accountTokens []string) (*httptest.Server, *pool.Pool) {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Channels.WorkBuddy.ChatBaseCN = up.srv.URL
	cfg.Channels.WorkBuddy.ChatBaseGL = up.srv.URL
	cfg.Models = []config.ModelBinding{
		{Canonical: "glm-5.3", Channel: model.ChannelWorkBuddy, UpstreamModel: "glm-5.3"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second}, map[string]int{
		model.ChannelWorkBuddy: 2,
	}, func(acc *model.Account) { _ = st.SaveAccount(acc) })
	for i, tok := range accountTokens {
		creds := map[string]any{
			"access_token": tok,
			"uid":          fmt.Sprintf("uid-%d", i),
			"realm":        "cn",
		}
		raw, _ := json.Marshal(creds)
		p.Upsert(&model.Account{ID: fmt.Sprintf("wb-%d", i), Channel: model.ChannelWorkBuddy, CredsJSON: raw, UID: fmt.Sprintf("uid-%d", i)})
	}
	drivers := map[string]driver.Driver{
		model.ChannelWorkBuddy: workbuddy.New(cfg.Channels.WorkBuddy, nil),
	}
	r := router.New(cfg)
	gw := gateway.New(cfg, p, r, drivers, st)
	srv := httptest.NewServer(server.New(cfg, gw).Handler())
	t.Cleanup(srv.Close)
	return srv, p
}

func TestWorkBuddyCostTierPrefersFreeAccount(t *testing.T) {
	up := newFakeWorkBuddy(t)
	srv, p := buildWorkBuddyServer(t, up, []string{"tok-free", "tok-paid"})

	// Mark wb-0 as observed-free and wb-1 as observed-paid.
	accs := p.Accounts(model.ChannelWorkBuddy)
	for _, a := range accs {
		if a.ID == "wb-0" {
			p.RecordCostTier(a, "glm-5.3", 0)
		} else {
			p.RecordCostTier(a, "glm-5.3", 2)
		}
	}

	body := `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 5; i++ {
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		resp.Body.Close()
	}
	// All 5 requests should have landed on the token of the free account.
	if got := up.servedBy["Bearer tok-free"]; got != 5 {
		t.Fatalf("free account served %d/5; distribution: %v", got, up.servedBy)
	}
	if got := up.servedBy["Bearer tok-paid"]; got != 0 {
		t.Fatalf("paid account should not be used, got %d", got)
	}
}

func TestWorkBuddyModelCooldownIsolationEndToEnd(t *testing.T) {
	// A 6004 on one account for glm-5.3 must cool only that account+model;
	// a second account serves the next request.
	up := newFakeWorkBuddy(t)
	srv, p := buildWorkBuddyServer(t, up, []string{"tok-a", "tok-b"})

	// Cool wb-0 for glm-5.3 until tomorrow.
	accA := p.Accounts(model.ChannelWorkBuddy)[0]
	if accA.ID != "wb-0" {
		accA = p.Accounts(model.ChannelWorkBuddy)[1]
	}
	cooledUntil := time.Now().Add(2 * time.Hour)
	p.ApplyResult(accA, "glm-5.3", uperrModelRate("glm-5.3", cooledUntil))

	body := `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if up.servedBy["Bearer tok-a"]+up.servedBy["Bearer tok-b"] != 1 {
		t.Fatalf("expected exactly one upstream hit: %v", up.servedBy)
	}
	// The cooled account must not have served it.
	if up.servedBy["Bearer tok-a"] > 0 && accA.ID == "wb-0" {
		t.Fatal("cooled account served the request")
	}
}
