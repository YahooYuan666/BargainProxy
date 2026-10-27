package trae

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// BP-137: the chat body MUST carry config_name alongside model.
//
// Measured 2026-10-27 on the CN account by reading the response's own
// timing_cost frame:
//
//	model=kimi-k3            -> provider_model_name=ep-20260802210216-sl4tw
//	model=glm-5.3-flash      -> provider_model_name=ep-20260802210216-sl4tw
//	model=___not_a_model___  -> provider_model_name=ep-20260802210216-sl4tw
//
// i.e. without config_name EVERY request is served by one fixed Volcengine
// endpoint no matter what model was asked for. With config_name the same
// requests answered ali-kimi-k3 / glm-5.3-flash. The channel therefore spent
// its whole life answering with the wrong model while returning 200 — which is
// also why "a fake model name still answers" was mistaken for upstream
// leniency.
func TestChatBodyCarriesConfigName(t *testing.T) {
	call := driver.ChatCall{
		UpstreamModel: "kimi-k3",
		Request: &ir.ChatRequest{
			Model:    "kimi-k3",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}
	raw, err := buildSOLOBody(call, functionSOLO)
	if err != nil {
		t.Fatalf("buildSOLOBody: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	model, _ := body["model"].(string)
	configName, hasConfig := body["config_name"].(string)
	if !hasConfig {
		t.Fatal("chat body has no config_name — the upstream then ignores the model and answers from one fixed endpoint")
	}
	if configName != "kimi-k3" {
		t.Fatalf("config_name = %q, want the requested model", configName)
	}
	if model != configName {
		t.Fatalf("model=%q and config_name=%q disagree; they must carry the same id", model, configName)
	}
}

// TestChatBodyConfigNameFollowsUpstreamModel: the two fields track the BINDING's
// upstream id, not the client-facing canonical name — a route may map
// "deepseek-v4.1-flash" to a differently-cased upstream id.
func TestChatBodyConfigNameFollowsUpstreamModel(t *testing.T) {
	call := driver.ChatCall{
		UpstreamModel: "DeepSeek-V4-Pro",
		Request: &ir.ChatRequest{
			Model:    "deepseek-v4-pro", // client-facing canonical name
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}
	raw, _ := buildSOLOBody(call, functionSOLO)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if got := body["config_name"]; got != "DeepSeek-V4-Pro" {
		t.Fatalf("config_name = %v, want the upstream id DeepSeek-V4-Pro", got)
	}
	if got := body["model"]; got != "DeepSeek-V4-Pro" {
		t.Fatalf("model = %v, want the upstream id", got)
	}
}
