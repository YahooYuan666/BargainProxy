// Catalog access and quota mapping for the lobster channel.
//
// The thinking-gear field (lobsterai_options) is legal ONLY for models whose
// catalog entry declares a thinkingConfig. Sending it for any other model is a
// hard 400 (code 4000, "model does not have a valid thinkingConfig") — and 22
// of the 30 catalog models are in that state. The driver therefore keeps a
// cached set of thinking-capable model ids and consults it before writing the
// field; guessing from a client-supplied effort would break the majority of
// the catalog.

package lobster

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// catalogTTL bounds how long a fetched catalog is trusted. The catalog is
// public (no login) and changes only when the vendor adds models.
const catalogTTL = 30 * time.Minute

type catalogEntry struct {
	ModelID          string          `json:"modelId"`
	ModelName        string          `json:"modelName"`
	SupportsThinking bool            `json:"supportsThinking"`
	SupportsImage    bool            `json:"supportsImage"`
	ContextWindow    int             `json:"contextWindow"`
	CostMultiplier   float64         `json:"costMultiplier"`
	FreeAccess       bool            `json:"freeAccess"`
	ThinkingConfig   json.RawMessage `json:"thinkingConfig"`
}

// catalog caches the public model catalog and derives the thinking-gear set.
type catalog struct {
	mu        sync.Mutex
	models    map[string]catalogEntry // by modelId
	fetchedAt time.Time
}

// thinkingModels returns the set of upstream model ids that accept the private
// thinking field. On a catalog failure it returns nil, meaning "do not send the
// field" — failing closed keeps a request working (upstream default thinking)
// instead of turning it into a 400.
func (d *Driver) thinkingModels(ctx context.Context) map[string]bool {
	entries := d.catalogEntries(ctx)
	if entries == nil {
		return nil
	}
	out := make(map[string]bool, len(entries))
	for id, e := range entries {
		// thinkingConfig must be a non-empty object; "null" and absent are the
		// same thing here.
		if len(e.ThinkingConfig) > 0 && string(e.ThinkingConfig) != "null" {
			out[id] = true
		}
	}
	return out
}

// catalogEntries returns the cached catalog, refreshing it when stale.
func (d *Driver) catalogEntries(ctx context.Context) map[string]catalogEntry {
	d.cat.mu.Lock()
	if d.cat.models != nil && time.Since(d.cat.fetchedAt) < catalogTTL {
		out := d.cat.models
		d.cat.mu.Unlock()
		return out
	}
	d.cat.mu.Unlock()

	entries, err := d.fetchCatalog(ctx)
	if err != nil {
		// Serve a stale copy rather than nothing: a catalog blip must not
		// silently disable thinking control for the whole channel.
		d.cat.mu.Lock()
		stale := d.cat.models
		d.cat.mu.Unlock()
		return stale
	}
	d.cat.mu.Lock()
	d.cat.models = entries
	d.cat.fetchedAt = time.Now()
	d.cat.mu.Unlock()
	return entries
}

// fetchCatalog reads /api/models/pricing-catalog (public, no auth).
func (d *Driver) fetchCatalog(ctx context.Context) (map[string]catalogEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		d.apiURL("/models/pricing-catalog"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "lobster: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, classifyHTTP(resp.StatusCode, raw)
	}
	var env struct {
		Data struct {
			TextModels []catalogEntry `json:"textModels"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if len(env.Data.TextModels) == 0 {
		return nil, uperr.New(uperr.ClassNotFound, 0, "lobster: catalog empty")
	}
	out := make(map[string]catalogEntry, len(env.Data.TextModels))
	for _, e := range env.Data.TextModels {
		if e.ModelID != "" {
			out[e.ModelID] = e
		}
	}
	return out, nil
}

// probeFrom maps the quota + profile-summary payloads onto the gateway's
// account state.
//
// The upstream exposes a free-credit allowance (quota) and a point balance
// (profile-summary). Points are the pool's real currency, so they become the
// Credits block; the free-credit allowance becomes a window so the panel can
// show both without adding them together (they are different currencies —
// the same mistake BP-67 fixed for Qoder's credits).
func probeFrom(quota, profile map[string]any) *driver.ProbeResult {
	res := &driver.ProbeResult{Windows: map[string]model.Window{}}

	remain := toInt64(profile["totalCreditsRemaining"])
	if remain > 0 || profile["totalCreditsRemaining"] != nil {
		// Points have no issued-total endpoint, so Total equals the current
		// balance and the panel renders it as a plain balance rather than a
		// percentage bar that would always read 100% (the AutoClaw lesson).
		res.Credits = &model.Credits{
			Remain:      remain,
			Total:       remain,
			BalanceOnly: true,
			UpdatedAt:   time.Now(),
		}
	}

	total := toInt64(quota["freeCreditsTotal"])
	used := toInt64(quota["freeCreditsUsed"])
	if total > 0 {
		frac := float64(total-used) / float64(total)
		if frac < 0 {
			frac = 0
		}
		if frac > 1 {
			frac = 1
		}
		res.Windows["free_credits"] = model.Window{
			RemainingFraction: frac,
			UpdatedAt:         time.Now(),
		}
	}

	note := ""
	if plan := toStr(quota["planName"]); plan != "" {
		note = "plan " + plan
	}
	if nick := toStr(profile["nickname"]); nick != "" {
		note = orElse(note+" ", "") + "user " + nick
	}
	if note == "" {
		note = "quota ok"
	}
	res.Note = note
	return res
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		if out, err := n.Int64(); err == nil {
			return out
		}
	}
	return 0
}
