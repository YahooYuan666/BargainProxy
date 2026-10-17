package openai

import (
	"testing"

	"bargainproxy/internal/ir"
)

// BP-69: ZCode reads prompt_tokens_details.cached_tokens and
// completion_tokens_details.reasoning_tokens; the chat egress must carry them
// when the upstream reported the data, and stay silent when it did not.
func TestUsageFromIRCarriesCacheAndReasoningDetails(t *testing.T) {
	u := UsageFromIR(ir.Usage{InputTokens: 100, OutputTokens: 20, CachedInputTokens: 80, CacheKnown: true, ReasoningTokens: 7})
	if u.PromptTokensDetails == nil || u.PromptTokensDetails.CachedTokens != 80 {
		t.Fatalf("prompt details = %+v", u.PromptTokensDetails)
	}
	if u.CompletionTokensDetails == nil || u.CompletionTokensDetails.ReasoningTokens != 7 {
		t.Fatalf("completion details = %+v", u.CompletionTokensDetails)
	}
	unknown := UsageFromIR(ir.Usage{InputTokens: 10, OutputTokens: 2})
	if unknown.PromptTokensDetails != nil || unknown.CompletionTokensDetails != nil {
		t.Fatalf("unknown details must be omitempty: %+v", unknown)
	}
}

func TestParseChatNormalizesNamedToolChoice(t *testing.T) {
	req, err := ParseChatRequest([]byte(`{
		"model":"m",
		"messages":[{"role":"user","content":"hi"}],
		"tool_choice":{"type":"function","function":{"name":"get_weather"}}
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.ToolChoice != "required" || req.ToolChoiceName != "get_weather" {
		t.Fatalf("tool choice = %v name=%q", req.ToolChoice, req.ToolChoiceName)
	}
}
