// ticket 37: the concrete upstream failure must surface, not a pool-level
// "no account available" — the client needs to see e.g. "re-login required".
package server_test

import (
	"encoding/json"
	"io"
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
	"bargainproxy/internal/store"
)

// TestConcreteAuthErrorSurfaces 单账号频道：首跳 auth 失败 → 同频道无号可轮
// → 客户端必须看到具体失败原因（授权/需重登），而不是泛化的 429 no account。
func TestConcreteAuthErrorSurfaces(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":1001,"message":"We're sorry, but we are not able to authenticate you."}`))
	}))
	t.Cleanup(up.Close)

	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Channels.Trae.AgentHost = up.URL
	cfg.Channels.Trae.UgHost = up.URL
	cfg.Models = []config.ModelBinding{
		{Canonical: "qwen3.8-flash", Channel: model.ChannelTrae, UpstreamModel: "qwen3.8-flash"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelTrae: 1}, nil)
	creds, _ := json.Marshal(map[string]any{"access_token": testToken("jwt"), "uid": "u-t"})
	p.Upsert(&model.Account{ID: "trae:1", Channel: model.ChannelTrae, CredsJSON: creds})

	drivers := map[string]driver.Driver{model.ChannelTrae: trae.New(cfg.Channels.Trae, nil)}
	gw := gateway.New(cfg, p, router.New(cfg), drivers, st)
	srv := httptest.NewServer(server.New(cfg, gw).Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"qwen3.8-flash","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// 401 = auth class reached the client; must NOT be a bare 429 quota wrapper
	// without the reason.
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d body=%s, want 401 with the concrete auth reason", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "authenticate") && !strings.Contains(string(body), "auth") {
		t.Fatalf("body must carry the upstream auth reason: %s", body)
	}
	if strings.Contains(string(body), "quota_exhausted") {
		t.Fatalf("pool-level error must not mask the concrete failure: %s", body)
	}
}
