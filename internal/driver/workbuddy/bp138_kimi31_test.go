package workbuddy

import (
	"strings"
	"testing"
)

// BP-138: both Kimi 3.1 spellings must be offered — the CN catalogue answers
// `kimi-k3-1` and the global one `kimi-k3.1-pro` (verified against /v3/config
// 2026-10-27). A bare `kimi-k3.1` is answered 11158 "service info not found",
// which reads like an account-type limit but is a name error, so the wrong
// spelling must never appear.
func TestStaticModelsIncludeKimi31Spellings(t *testing.T) {
	models, err := (&Driver{}).Models(nil)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	joined := strings.Join(models, ",")
	for _, want := range []string{"kimi-k3-1", "kimi-k3.1-pro"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q missing from the WorkBuddy model list: %v", want, models)
		}
	}
	for _, bad := range []string{"kimi-k3.1,", "kimi-k3.1 "} {
		if strings.Contains(joined+",", bad) {
			t.Errorf("the invalid spelling kimi-k3.1 must not be listed (upstream answers 11158)")
		}
	}
}
