// Package prompt holds the gateway-side system prompt replacement and
// outbound payload sanitization used by the workbuddy channel (panel
// parity: system replacement kills upstream content misclassification;
// sanitization strips client fingerprints from requests).
package prompt

import (
	"encoding/json"
	"strings"
)

// DefaultSystemPrompt replaces client system messages in "custom" mode. It is
// deliberately neutral: it must never trip upstream content classifiers.
const DefaultSystemPrompt = "You are a helpful AI assistant. Follow the user's instructions and respond directly."

// DegradedSystemPrompt is the neutral fallback used when a request with a
// passthrough prompt gets content-blocked (retry once with this).
const DegradedSystemPrompt = "You are a helpful assistant. Please answer the user's question."

// ReplaceSystem rewrites the conversation so that the client's system /
// developer messages are removed and the gateway prompt is injected once at
// the head. Non-system messages keep their order.
func ReplaceSystem(messages []map[string]any, gatewayPrompt string) []map[string]any {
	if gatewayPrompt == "" {
		gatewayPrompt = DefaultSystemPrompt
	}
	out := make([]map[string]any, 0, len(messages)+1)
	out = append(out, map[string]any{"role": "system", "content": gatewayPrompt})
	for _, m := range messages {
		role, _ := m["role"].(string)
		if role == "system" || role == "developer" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// StripSystem removes system/developer messages entirely (passthrough mode
// keeps the rest as-is).
func StripSystem(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		role, _ := m["role"].(string)
		if role == "system" || role == "developer" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// sanitizeKeyPrefixes lists JSON key prefixes scrubbed from outbound bodies.
var sanitizeKeyPrefixes = []string{"cc_", "x-anthropic-", "anthropic_", "x-stainless-", "x-codex-"}

// SanitizeBody removes client fingerprint keys from a JSON request body
// (recursively) and normalizes known client template phrases. Returns the
// rewritten body.
func SanitizeBody(raw []byte) []byte {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return raw
	}
	sanitizeMap(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}

func sanitizeMap(m map[string]any) {
	for k, v := range m {
		lower := strings.ToLower(k)
		drop := false
		for _, p := range sanitizeKeyPrefixes {
			if strings.HasPrefix(lower, p) {
				drop = true
				break
			}
		}
		if drop {
			delete(m, k)
			continue
		}
		switch tv := v.(type) {
		case map[string]any:
			sanitizeMap(tv)
		case []any:
			for _, item := range tv {
				if sub, ok := item.(map[string]any); ok {
					sanitizeMap(sub)
				}
			}
		}
	}
}

// InjectPromptCacheKey sets a stable prompt_cache_key derived from the
// account uid and the conversation id (panel parity: isolates upstream
// prefix caching per account, which is a significant cost reduction).
//
// No conversation identity means NO cache key: falling back to the bare uid
// gives every unrelated conversation one shared prefix slot, which is exactly
// the cross-conversation contamination seen on 2026-10-20. Omitting the field
// only costs cache hits; claiming a shared slot costs correctness.
func InjectPromptCacheKey(body map[string]any, uid, conversationID string) {
	if uid == "" || conversationID == "" {
		return
	}
	body["prompt_cache_key"] = uid + ":" + conversationID
}
