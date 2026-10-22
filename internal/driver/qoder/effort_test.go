package qoder

import (
	"testing"
)

// Qoder-9 red-first tests (BP-98): a requested effort the model does not
// name must snap to the nearest supported level (magpie-community/plugins
// packages/qoder 0.2.30 fitEffort + EFFORT_RANK) instead of passing through
// and being ignored or refused upstream. Rank order:
// none < minimal < low < medium < high < xhigh < max < ultra.

func TestFitEffortExact(t *testing.T) {
	levels := []string{"low", "high", "max"}
	if got := fitEffort("high", levels); got != "high" {
		t.Fatalf("exact level must pass through, got %q", got)
	}
}

func TestFitEffortNearest(t *testing.T) {
	levels := []string{"low", "high"}
	if got := fitEffort("medium", levels); got != "high" {
		t.Fatalf("medium over [low,high] must snap to high (tie goes up), got %q", got)
	}
	if got := fitEffort("minimal", levels); got != "low" {
		t.Fatalf("minimal must snap to low, got %q", got)
	}
	if got := fitEffort("ultra", []string{"low", "high", "xhigh"}); got != "xhigh" {
		t.Fatalf("ultra must snap to xhigh, got %q", got)
	}
}

func TestFitEffortUltraAliasesMax(t *testing.T) {
	if got := fitEffort("ultra", []string{"low", "max"}); got != "max" {
		t.Fatalf("ultra with max available must pick max, got %q", got)
	}
}

func TestFitEffortEmptyLevelsPassThrough(t *testing.T) {
	if got := fitEffort("medium", nil); got != "medium" {
		t.Fatalf("no levels: pass through, got %q", got)
	}
}

func TestCatalogParsesEfforts(t *testing.T) {
	raw := map[string]interface{}{
		"chat": []interface{}{
			map[string]interface{}{
				"key": "qmodel_38max", "display_name": "Qwen3.8-Max", "enable": true,
				"price_factor": 0.5,
				"thinking_config": map[string]interface{}{
					"enabled": map[string]interface{}{
						"efforts": map[string]interface{}{
							"low":    map[string]interface{}{"is_default": false},
							"medium": map[string]interface{}{"is_default": true},
							"xhigh":  map[string]interface{}{"is_default": false},
						},
					},
				},
			},
		},
	}
	cat := extractCatalog(raw)
	caps := cat.Caps["qmodel_38max"]
	if caps == nil {
		t.Fatal("no caps")
	}
	if len(caps.Efforts) != 3 {
		t.Fatalf("efforts = %v, want 3 parsed levels", caps.Efforts)
	}
	if caps.DefaultEffort != "medium" {
		t.Fatalf("defaultEffort = %q, want medium", caps.DefaultEffort)
	}
}
