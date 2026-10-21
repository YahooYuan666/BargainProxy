package trae

import (
	"context"
	"testing"

	"bargainproxy/internal/config"
)

// TestModelsIncludeDeepSeek41Flash pins the 2026-10-21 addition: TraeCode
// exposes a lower-case `deepseek-v4.1-flash` config_name. The spelling must
// stay exactly like that — Trae matches config names verbatim, and an
// unknown/title-cased name makes it answer from its default model instead of
// failing, which is why the model has to be listed correctly rather than
// aliased.
func TestModelsIncludeDeepSeek41Flash(t *testing.T) {
	d := New(config.TraeSettings{Enabled: true}, nil)
	models, err := d.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	found := ""
	for _, m := range models {
		if m == "deepseek-v4.1-flash" {
			found = m
		}
		if m == "DeepSeek-V4.1-Flash" {
			t.Fatalf("the 4.1 config must not be title-cased: %q", m)
		}
	}
	if found == "" {
		t.Fatalf("deepseek-v4.1-flash missing from the model list: %v", models)
	}
}
