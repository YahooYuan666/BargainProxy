package qoder

import (
	"testing"

	"bargainproxy/internal/uperr"
)

// BP-311: the realm guard must resolve the DISPLAY name our route table uses
// ("DeepSeek-Flash") to the upstream key the catalogue is indexed by
// ("dfmodel"). Before this, every configured model was rejected with a 404
// "not on this account's listing" as soon as the listing was live: field
// 2026-11-11 20:53, verified live with `channel:qoder:qwen3.8-flash`.
func TestBP311GuardResolvesDisplayName(t *testing.T) {
	live := &ModelCatalog{
		ModelMap: map[string]string{
			"DeepSeek-Flash": "dfmodel",
			"Qwen3.8-Flash":  "qfmodel",
			"GLM-5.3-Flash":  "gfmodel",
			"Qwen3.8-Max":    "qmodel_38max",
		},
		Caps: map[string]*ModelCaps{
			"dfmodel": {}, "qfmodel": {}, "gfmodel": {}, "qmodel_38max": {},
		},
		Off:  map[string]bool{},
		Live: true,
	}
	for _, display := range []string{"DeepSeek-Flash", "Qwen3.8-Flash", "GLM-5.3-Flash", "Qwen3.8-Max"} {
		if err := guardRealmModel(live, display, realmCfg("cn")); err != nil {
			t.Fatalf("%s (display name) must pass the guard, got %v", display, err)
		}
	}
	// The raw upstream key keeps passing too (callers that already resolved).
	if err := guardRealmModel(live, "dfmodel", realmCfg("cn")); err != nil {
		t.Fatalf("upstream key must pass: %v", err)
	}
	// A model this realm never serves is still rejected — the guard's whole
	// point (BP-305 cross-region miss).
	err := guardRealmModel(live, "smodel", realmCfg("cn"))
	if ue, ok := uperr.As(err); err == nil || !ok || ue.Class != uperr.ClassNotFound {
		t.Fatalf("unserved model must stay ClassNotFound, got %v", err)
	}
	// A display name the catalogue maps but the realm marks disabled rides
	// through to the upstream (BP-304 credits recast).
	live.ModelMap["MiniMax-M3.1-Flash"] = "mmodel"
	live.Off["mmodel"] = true
	if err := guardRealmModel(live, "MiniMax-M3.1-Flash", realmCfg("cn")); err != nil {
		t.Fatalf("disabled-but-listed model must ride to the upstream 400: %v", err)
	}
}

// TestBP311StaticTableCarriesLiveModels pins the catalogue refresh: the ids
// the live listing answered with on 2026-11-11 must exist in the static
// fallback, so a realm whose listing fetch fails still routes them.
func TestBP311StaticTableCarriesLiveModels(t *testing.T) {
	for display, want := range map[string]string{
		"Qwen4-Max-Preview":  "qmodel_latest",
		"MiniMax-M3.1-Flash": "mmodel",
	} {
		if got := DefaultModelMap[display]; got != want {
			t.Fatalf("DefaultModelMap[%q] = %q, want %q", display, got, want)
		}
	}
	// The retired spelling must be gone, or two names would resolve to one key.
	if _, stale := DefaultModelMap["MiniMax-M2.7"]; stale {
		t.Fatal("MiniMax-M2.7 is no longer served; the static table must not carry it")
	}
}
