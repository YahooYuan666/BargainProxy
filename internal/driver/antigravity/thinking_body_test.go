// BP-41: antigravity thinkingConfig body shape — camelCase includeThoughts
// only (proto oneof double-set is a 400, AIClient2API 95e29ca), budget for
// Claude models and levels for the Gemini 3.x family.
package antigravity

import (
	"encoding/json"
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

func baseReq() *ir.ChatRequest {
	return &ir.ChatRequest{
		Model:    "x",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
}

func buildBody(t *testing.T, upstreamModel string, req *ir.ChatRequest) string {
	t.Helper()
	raw, err := buildRequest("proj-1", upstreamModel, req, nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	return string(raw)
}

func thinkingConfigOf(t *testing.T, body string) map[string]any {
	t.Helper()
	var outer struct {
		Request struct {
			GenerationConfig struct {
				ThinkingConfig map[string]any `json:"thinkingConfig"`
			} `json:"generationConfig"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(body), &outer); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	return outer.Request.GenerationConfig.ThinkingConfig
}

func TestThinkingBodyClaudeUsesBudgetCamelCaseOnly(t *testing.T) {
	req := baseReq()
	req.Thinking = &ir.ThinkingConfig{Enabled: true, BudgetTokens: 8192, Level: "medium", Include: true}
	body := buildBody(t, "gemini-claude-sonnet-4-6", req)
	cfg := thinkingConfigOf(t, body)
	if cfg["includeThoughts"] != true {
		t.Fatalf("includeThoughts = %v, want true", cfg["includeThoughts"])
	}
	if cfg["thinkingBudget"] != float64(8192) {
		t.Fatalf("thinkingBudget = %v, want 8192 (claude 走预算)", cfg["thinkingBudget"])
	}
	if _, ok := cfg["thinkingLevel"]; ok {
		t.Fatalf("thinkingLevel must not be set for claude models: %v", cfg)
	}
	// Snake-case keys address the same proto oneof and must never appear.
	if strings.Contains(body, "include_thoughts") || strings.Contains(body, "thinking_budget") {
		t.Fatalf("snake_case thinking keys leaked: %s", body)
	}
}

func TestThinkingBodyGeminiUsesLevelFromBudget(t *testing.T) {
	req := baseReq()
	req.Thinking = &ir.ThinkingConfig{Enabled: true, BudgetTokens: 20000, Include: true}
	body := buildBody(t, "gemini-3.8-flash-high", req)
	cfg := thinkingConfigOf(t, body)
	if cfg["includeThoughts"] != true {
		t.Fatalf("includeThoughts = %v, want true", cfg["includeThoughts"])
	}
	if cfg["thinkingLevel"] != "HIGH" {
		t.Fatalf("thinkingLevel = %v, want HIGH (budget 20000 → high 带)", cfg["thinkingLevel"])
	}
	if _, ok := cfg["thinkingBudget"]; ok {
		t.Fatalf("thinkingBudget must not be set for gemini 3.x models: %v", cfg)
	}
}

func TestThinkingBodyGeminiExplicitLevelWins(t *testing.T) {
	req := baseReq()
	req.Thinking = &ir.ThinkingConfig{Enabled: true, Level: "low", BudgetTokens: 20000, Include: true}
	cfg := thinkingConfigOf(t, buildBody(t, "gemini-3.8-flash-high", req))
	if cfg["thinkingLevel"] != "LOW" {
		t.Fatalf("thinkingLevel = %v, want LOW (显式档位优先于预算推导)", cfg["thinkingLevel"])
	}
}

func TestRequestedEffortBranchIncludesThoughts(t *testing.T) {
	req := baseReq()
	req.RequestedEffort = "high"
	cfg := thinkingConfigOf(t, buildBody(t, "gemini-3.8-flash-high", req))
	if cfg["thinkingLevel"] != "HIGH" || cfg["includeThoughts"] != true {
		t.Fatalf("effort branch config = %v, want level HIGH + includeThoughts true", cfg)
	}
}

func TestNoThinkingMeansNoThinkingConfig(t *testing.T) {
	cfg := thinkingConfigOf(t, buildBody(t, "gemini-3.8-flash-high", baseReq()))
	if len(cfg) != 0 {
		t.Fatalf("thinkingConfig = %v, want absent when the client asked for nothing", cfg)
	}
}
