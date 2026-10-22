package qoder

import (
	"testing"
)

// Qoder-3 + Qoder-6 red-first tests (BP-98), translated from
// magpie-community/plugins packages/qoder 0.2.30 (freeOf/rateOf/sceneModels):
//   - price_factor is the credits multiple the client shows ("0.5×"); is_free
//     is marketing and only counts when no price exists at all (Qoder lists
//     is_free:true on Qwen3.8-Max at 0.5× — an off-peak discount, not free);
//   - an active promotion's 0 is a discount, not free, when a price existed
//     before it; a true limited-time free model shows original_price_factor
//     struck through;
//   - the app scene merges with chat (app entries win), because the app
//     scene carries models the CLI's chat scene omits (fast/ultimate tiers).

func listingFixture() map[string]interface{} {
	return map[string]interface{}{
		"app": []interface{}{
			map[string]interface{}{
				"key": "qfmodel_fast", "display_name": "Qwen3.8-Flash-Fast", "enable": true,
				"price_factor": 0.8,
			},
			map[string]interface{}{
				"key": "gmodel", "display_name": "GLM-5.3", "enable": true,
				"price_factor":          0.0,
				"original_price_factor": 0.1, // limited-time free: 0.1× struck through
			},
		},
		"chat": []interface{}{
			// Same key as an app entry: the app entry wins.
			map[string]interface{}{
				"key": "qfmodel_fast", "display_name": "Qwen3.8-Flash-Fast-CHAT", "enable": true,
				"price_factor": 9.9,
			},
			// chat-only entry keeps its place after the merged prefix.
			map[string]interface{}{
				"key": "kmodel_latest", "display_name": "Kimi-K3", "enable": true,
				"price_factor": 1.2,
			},
			// is_free:true with no price at all: really free.
			map[string]interface{}{
				"key": "freemodel", "display_name": "Free-Model", "enable": true,
				"is_free": true,
			},
		},
	}
}

func TestCatalogMergesAppAndChatScenes(t *testing.T) {
	cat := extractCatalog(listingFixture())
	if cat == nil {
		t.Fatal("catalog nil")
	}
	// The app entry wins over the chat twin.
	if cat.ModelMap["Qwen3.8-Flash-Fast-CHAT"] != "" {
		t.Fatal("chat twin of an app key must not add a second entry")
	}
	if cat.ModelMap["Qwen3.8-Flash-Fast"] != "qfmodel_fast" {
		t.Fatalf("app entry missing: %v", cat.ModelMap)
	}
	if cat.ModelMap["Kimi-K3"] != "kmodel_latest" {
		t.Fatal("chat-only entry lost in the merge")
	}
	if cat.ModelMap["Free-Model"] != "freemodel" {
		t.Fatal("free-model entry lost")
	}
}

func TestCatalogParsesPrices(t *testing.T) {
	cat := extractCatalog(listingFixture())
	cases := []struct {
		key     string
		rate    float64
		rateWas float64
		free    bool
	}{
		{"qfmodel_fast", 0.8, 0, false},
		{"gmodel", 0, 0.1, true}, // 0× with original 0.1× struck through: free
		{"kmodel_latest", 1.2, 0, false},
		{"freemodel", 0, 0, true}, // is_free with no price
	}
	for _, tc := range cases {
		caps := cat.Caps[tc.key]
		if caps == nil {
			t.Fatalf("no caps for %s", tc.key)
		}
		if caps.Rate != tc.rate {
			t.Fatalf("%s rate = %v, want %v", tc.key, caps.Rate, tc.rate)
		}
		if caps.RateWas != tc.rateWas {
			t.Fatalf("%s rateWas = %v, want %v", tc.key, caps.RateWas, tc.rateWas)
		}
		if caps.Free != tc.free {
			t.Fatalf("%s free = %v, want %v", tc.key, caps.Free, tc.free)
		}
	}
}

func TestPromotionZeroIsDiscountNotFree(t *testing.T) {
	raw := map[string]interface{}{
		"key": "x", "display_name": "X", "enable": true,
		"price_factor": 0.0,
		"promotion": map[string]interface{}{
			"active":                        true,
			"before_promotion_price_factor": 0.5,
			"discount_factor":               0.4,
		},
	}
	cat := extractCatalog(map[string]interface{}{"chat": []interface{}{raw}})
	caps := cat.Caps["x"]
	if caps == nil {
		t.Fatal("no caps")
	}
	if caps.Free {
		t.Fatal("a promotion's 0 with a price before it is a discount, not free")
	}
	// magpie rateOf: a discounted 0 reads as before*discount (0.5*0.4=0.2).
	if caps.Rate <= 0 || caps.Rate >= 0.5 {
		t.Fatalf("discounted rate = %v, want before*discount = 0.2", caps.Rate)
	}
	if caps.RateWas != 0.5 {
		t.Fatalf("rateWas = %v, want the pre-promotion 0.5", caps.RateWas)
	}
}
