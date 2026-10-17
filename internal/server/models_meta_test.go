package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/server"
	"bargainproxy/internal/store"
)

// buildModelsServer wires a minimal gateway for /v1/models assertions.
func buildModelsServer(t *testing.T) (*httptest.Server, *pool.Pool) {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Models = []config.ModelBinding{
		{Canonical: "glm-5.2", Channel: model.ChannelWorkBuddy, UpstreamModel: "glm-5.2"},
		{Canonical: "claude-sonnet-4-6", Channel: model.ChannelZCode, UpstreamModel: "claude-sonnet-4-6"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: time.Second}, map[string]int{
		model.ChannelWorkBuddy: 2, model.ChannelZCode: 2,
	}, func(acc *model.Account) { _ = st.SaveAccount(acc) })
	creds, _ := json.Marshal(map[string]any{"access_token": testToken("meta"), "uid": "u1", "realm": "cn"})
	acc := &model.Account{ID: "workbuddy:u1", Channel: model.ChannelWorkBuddy, CredsJSON: creds, UID: "u1"}
	p.Upsert(acc)
	// One observed-free classification for glm-5.2; claude-sonnet-4-6 stays
	// unobserved.
	p.RecordCostTier(acc, "glm-5.2", 0)
	gw := gateway.New(cfg, p, router.New(cfg), map[string]driver.Driver{}, st)
	srv := httptest.NewServer(server.New(cfg, gw).Handler())
	t.Cleanup(srv.Close)
	return srv, p
}

type modelEntry struct {
	ID            string         `json:"id"`
	OwnedBy       string         `json:"owned_by"`
	ContextLength *int           `json:"context_length"`
	Pricing       map[string]any `json:"pricing"`
}

// TestModelsRichMetadata: context_length comes from the static table (real
// values, unknown models omit the field) and pricing reflects observed cost
// tiers only.
func TestModelsRichMetadata(t *testing.T) {
	srv, _ := buildModelsServer(t)

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Data []modelEntry `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]modelEntry{}
	for _, m := range out.Data {
		byID[m.ID] = m
	}

	known, ok := byID["glm-5.2"]
	if !ok {
		t.Fatalf("glm-5.2 missing: %+v", byID)
	}
	if known.ContextLength == nil || *known.ContextLength != 1000000 {
		t.Fatalf("glm-5.2 context_length = %v, want 1000000 (static table)", known.ContextLength)
	}
	if known.OwnedBy != model.ChannelWorkBuddy {
		t.Fatalf("glm-5.2 owned_by = %q", known.OwnedBy)
	}
	if known.Pricing == nil || known.Pricing["tier"].(float64) != 0 ||
		!strings.Contains(known.Pricing["note"].(string), "free") {
		t.Fatalf("glm-5.2 pricing = %+v, want observed-free tier 0", known.Pricing)
	}

	unknown, ok := byID["claude-sonnet-4-6"]
	if !ok {
		t.Fatalf("claude-sonnet-4-6 missing: %+v", byID)
	}
	if unknown.ContextLength != nil {
		t.Fatalf("unknown model must omit context_length, got %v", *unknown.ContextLength)
	}
	if unknown.Pricing != nil {
		t.Fatalf("unobserved model must omit pricing, got %+v", unknown.Pricing)
	}
	if unknown.OwnedBy != model.ChannelZCode {
		t.Fatalf("claude-sonnet-4-6 owned_by = %q", unknown.OwnedBy)
	}
}
