package trae

import (
	"strings"
	"testing"
)

// BP-138: the static model list must offer Kimi 3.1. The live catalogue names it
// `kimi-k3.1-pro`; leaving only `kimi-k3` meant the channel could not be asked
// for the current model even though the upstream serves it.
func TestStaticModelsIncludeKimi31(t *testing.T) {
	models, err := (&Driver{}).Models(nil)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	var found bool
	for _, m := range models {
		if m == "kimi-k3.1-pro" {
			found = true
		}
	}
	if !found {
		t.Fatalf("kimi-k3.1-pro missing from the Trae model list: %v", models)
	}
	// The old name must stay: existing routes still point at it.
	joined := strings.Join(models, ",")
	if !strings.Contains(joined, "kimi-k3,") && !strings.HasSuffix(joined, "kimi-k3") {
		t.Fatalf("kimi-k3 was dropped, breaking existing routes: %v", models)
	}
}
