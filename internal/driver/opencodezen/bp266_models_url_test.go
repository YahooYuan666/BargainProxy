package opencodezen

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestModelsURLResolution(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://opencode.ai/inference/openai/v1", "https://opencode.ai/inference/v1/models"},
		{"https://opencode.ai/inference/openai/v1/", "https://opencode.ai/inference/v1/models"},
		{"https://opencode.ai/inference/v1", "https://opencode.ai/inference/v1/models"},
		{"https://opencode.ai/zen", "https://opencode.ai/inference/v1/models"},
		{"http://127.0.0.1:8080/custom/v1", "http://127.0.0.1:8080/custom/v1/models"},
		{"http://localhost:8080", "http://localhost:8080/models"},
	}
	for _, tc := range cases {
		got := modelsURL(tc.in)
		if got != tc.want {
			t.Errorf("modelsURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCatalogFetchLiveUsesModelsURL(t *testing.T) {
	calledPath := ""
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"m1"}]}`)
	}))
	defer ts.Close()

	// Given a base ending in /openai/v1, modelsURL strips /openai/v1 and points to /v1/models
	c := NewCatalog(ts.Client(), nil, ts.URL+"/openai/v1")
	live, _, err := c.fetchLive(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchLive: %v", err)
	}
	if len(live) != 1 || live[0] != "m1" {
		t.Fatalf("got live models %v, want [m1]", live)
	}
	if calledPath != "/v1/models" {
		t.Fatalf("calledPath = %q, want /v1/models", calledPath)
	}
}

func TestCatalogLiveErrorFallbacksToPersistedLiveList(t *testing.T) {
	c := NewCatalog(http.DefaultClient, nil, "http://127.0.0.1:12345")
	c.liveList = []string{"persisted-model-free"}
	c.prices = map[string]metaPrice{
		"persisted-model-free": {Input: nil},
	}

	// Simulated live fetch error with non-nil meta: should keep candidate rather than wiping it
	prevMeta := metadataURL
	metaTs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"opencode":{"models":{"persisted-model-free":{"cost":{"input":0}}}}}`)
	}))
	defer metaTs.Close()
	metadataURL = metaTs.URL
	defer func() { metadataURL = prevMeta }()

	err := c.Sync(context.Background())
	if err == nil {
		t.Fatal("expected error from failed live fetch")
	}
	snap := c.Snapshot()
	if snap.LiveCount != 1 {
		t.Fatalf("LiveCount = %d, want 1 (fallback)", snap.LiveCount)
	}
	candidates := c.Candidates()
	if len(candidates) != 1 || candidates[0] != "persisted-model-free" {
		t.Fatalf("Candidates = %v, want [persisted-model-free]", candidates)
	}
}

func TestLiveCatalogFetchDefaultBase(t *testing.T) {
	if os.Getenv("BARGAINPROXY_LIVE_ZEN") != "1" {
		t.Skip("live network check; set BARGAINPROXY_LIVE_ZEN=1 to run")
	}
	c := NewCatalog(http.DefaultClient, nil, "")
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	snap := c.Snapshot()
	if snap.LiveCount == 0 {
		t.Fatalf("LiveCount is 0, fetchLive failed: %v", snap.LastSyncError)
	}
	candidates := c.Candidates()
	if len(candidates) == 0 {
		t.Fatalf("expected candidates > 0, got 0")
	}
	t.Logf("Successfully fetched %d live models and %d candidates: %v", snap.LiveCount, len(candidates), candidates)
}
