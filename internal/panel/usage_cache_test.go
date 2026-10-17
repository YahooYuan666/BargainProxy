// BP-53: the usage API aggregates prefix-cache accounting per dimension — the
// hit sum plus the prompt-token sum of only those requests that reported cache
// data (the rate's denominator), with upstream noise clamped.
package panel_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"bargainproxy/internal/model"
)

func TestUsageAggregatesCacheTokens(t *testing.T) {
	srv, _, _, st := buildPanelEnv(t, nil)
	now := time.Now()
	rows := []*model.RequestLog{
		// A cache-reporting hit row.
		{Time: now, Protocol: "openai-chat", Model: "glm-5.3", Status: 200, InputTokens: 8000, OutputTokens: 10,
			Channel: "workbuddy", CachedTokens: 7808, CacheKnown: true},
		// A cache-reporting zero-hit row (first turn) — must stay in the denominator.
		{Time: now, Protocol: "openai-chat", Model: "glm-5.3", Status: 200, InputTokens: 2000, OutputTokens: 5,
			Channel: "workbuddy", CachedTokens: 0, CacheKnown: true},
		// A channel that reports nothing — must not join the denominator.
		{Time: now, Protocol: "openai-chat", Model: "kimi-k3", Status: 200, InputTokens: 50000, OutputTokens: 5,
			Channel: "trae"},
		// Upstream noise: hit larger than the prompt → clamped.
		{Time: now, Protocol: "openai-chat", Model: "glm-5.3", Status: 200, InputTokens: 100, OutputTokens: 1,
			Channel: "workbuddy", CachedTokens: 500, CacheKnown: true},
	}
	for _, r := range rows {
		if err := st.AppendRequestLog(r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	resp, err := http.Get(srv.URL + "/panel/api/usage")
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	defer resp.Body.Close()
	var d struct {
		ByModel []struct {
			Key              string `json:"key"`
			CachedTokens     int64  `json:"cached_tokens"`
			CacheKnownTokens int64  `json:"cache_known_tokens"`
			CacheKnown       bool   `json:"cache_known"`
		} `json:"by_model"`
		ByChannel []struct {
			Key              string `json:"key"`
			CacheKnownTokens int64  `json:"cache_known_tokens"`
			CacheKnown       bool   `json:"cache_known"`
		} `json:"by_channel"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byModel := map[string]int64{}
	for _, r := range d.ByModel {
		byModel[r.Key] = r.CacheKnownTokens
	}
	if byModel["glm-5.3"] != 10100 {
		t.Fatalf("glm-5.3 cache denominator = %d, want 10100 (8000+2000+100)", byModel["glm-5.3"])
	}
	if byModel["kimi-k3"] != 0 {
		t.Fatalf("non-reporting channel must not join the denominator: kimi-k3 = %d", byModel["kimi-k3"])
	}
	for _, r := range d.ByModel {
		if r.Key == "glm-5.3" {
			if !r.CacheKnown || r.CachedTokens != 7808+100 {
				t.Fatalf("glm-5.3 row = %+v", r)
			}
		}
	}
	for _, r := range d.ByChannel {
		if r.Key == "workbuddy" && (!r.CacheKnown || r.CacheKnownTokens != 10100) {
			t.Fatalf("workbuddy row = %+v", r)
		}
		if r.Key == "trae" && (r.CacheKnown || r.CacheKnownTokens != 0) {
			t.Fatalf("trae row = %+v", r)
		}
	}
}
