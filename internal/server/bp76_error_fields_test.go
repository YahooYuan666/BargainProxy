package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/openaicompat"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/server"
	"bargainproxy/internal/store"
)

func TestBP76NativeCompatErrorRetainsMachineReadableFields(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"type":"invalid_request_error","code":"invalid_parameter","param":"temperature","message":"invalid temperature"}}`)
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Channels.OpenAICompat = config.OpenAICompatSettings{Enabled: true, BaseURL: up.URL + "/v1"}
	cfg.Models = []config.ModelBinding{{Canonical: "audit-model", Channel: model.ChannelOpenAICompat, UpstreamModel: "audit-model"}}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{model.ChannelOpenAICompat: 4}, func(*model.Account) {})
	p.Upsert(&model.Account{ID: "audit-compat", Channel: model.ChannelOpenAICompat})
	gw := gateway.New(cfg, p, router.New(cfg), map[string]driver.Driver{model.ChannelOpenAICompat: openaicompat.New(cfg.Channels.OpenAICompat, nil)}, st)
	srv := httptest.NewServer(server.New(cfg, gw).Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"audit-model","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Error struct {
			Type  string `json:"type"`
			Code  string `json:"code"`
			Param string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "invalid_request_error" || body.Error.Code != "invalid_parameter" || body.Error.Param != "temperature" {
		t.Fatalf("machine-readable upstream error fields were lost: status=%d body=%s", resp.StatusCode, raw)
	}
}
