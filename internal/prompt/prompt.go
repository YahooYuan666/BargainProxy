// Package prompt holds the gateway-side system prompt handling and outbound
// payload sanitization used by the workbuddy channel.
//
// The system prompt is the caller's most expensive asset: it carries the
// agent's identity, its tool discipline and its output format. Three modes
// exist, and the difference between them is whether that text survives:
//
//	custom      — the gateway replaces it with its own neutral line. The
//	              caller's text is DROPPED.
//	append      — the gateway adds its own line and leaves the caller's text
//	              untouched.
//	passthrough — the caller's messages are forwarded verbatim.
//
// Upstream (misakano7545/workbuddy2api-panel) defaults to passthrough and
// documents why: "Rewrite 用中性提示词替换 system，客户端辛苦写的人设/项目约定
// 就此消失". This package used to treat passthrough as "strip", which dropped
// the text in BOTH switch positions — see StripSystem's comment.
package prompt

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
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

// StripSystem removes system/developer messages entirely. It is NOT what
// passthrough means: passthrough forwards the caller's messages untouched.
// Kept only for the "custom + replacement off" combination, where the caller
// explicitly asked the gateway to own the system slot and the gateway has no
// prompt of its own to put there.
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

// SplitSystem splits the longest system/developer message into several system
// messages at sentence boundaries. It is the FIRST rescue rung for a 11128
// channel refusal (upstream misakano e4dd458c, measured 2026-10-28 on a CN
// account with deepseek-v4.1-flash): the edge matches the fingerprint over one
// continuous run of text, so breaking the run at a sentence boundary loses the
// match while the text itself is unchanged and the carrying channel is still
// system — the model's instruction priority does not drop.
//
// Measured ladder on that probe:
//
//	whole paragraph in system   -> 400 11128
//	reworded, still in system   -> 200 (the sanitizer already covers this)
//	split at sentence boundaries-> 200
//	one sentence per message    -> 200
//
// Only the longest message is split: cutting a short one turns a single
// instruction into two halves (a persona line split in two stops working).
// Returns false when no system message can be cut into 2+ parts, so the caller
// moves on to the next rung.
func SplitSystem(messages []map[string]any) ([]map[string]any, bool) {
	best, bestLen := -1, 0
	for i, m := range messages {
		role, _ := m["role"].(string)
		if role != "system" && role != "developer" {
			continue
		}
		txt, ok := m["content"].(string)
		if !ok || len(txt) <= bestLen {
			continue
		}
		best, bestLen = i, len(txt)
	}
	if best < 0 {
		return messages, false
	}
	parts := SplitSentences(messages[best]["content"].(string))
	if len(parts) < 2 {
		return messages, false
	}
	role, _ := messages[best]["role"].(string)
	out := make([]map[string]any, 0, len(messages)+len(parts)-1)
	out = append(out, messages[:best]...)
	for _, p := range parts {
		out = append(out, map[string]any{"role": role, "content": p})
	}
	out = append(out, messages[best+1:]...)
	return out, true
}

// SplitSentences cuts text at sentence boundaries, keeping the punctuation.
// Whitespace right after a boundary is dropped — the message boundary is the
// separator. Only . ! ? followed by whitespace count, so "3.5", "v1.2.3" and
// "example.com" stay whole. Over-splitting is harmless; under-splitting is
// what fails, so the cut errs toward more pieces.
func SplitSentences(text string) []string {
	var out []string
	start := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '.', '!', '?':
			if i+1 < len(text) && (text[i+1] == ' ' || text[i+1] == '\n' || text[i+1] == '\t') {
				if seg := strings.TrimSpace(text[start : i+1]); seg != "" {
					out = append(out, seg)
				}
				start = i + 1
			}
		}
	}
	if seg := strings.TrimSpace(text[start:]); seg != "" {
		out = append(out, seg)
	}
	return out
}

// EnsureSystemFirst guarantees messages[0] is a system message, which the
// upstream channel check requires outright (misakano issue #231, measured on
// the international build 2026-10-20): a first message that is not a system
// prompt answers 400 11128 "first message is not system prompt", and a first
// developer message answers 11128 "unapproved channel". Neither is a content
// policy and neither is an account problem — the role of the first message is
// itself part of the channel check.
//
// This only matters for passthrough: custom/append modes already start with a
// gateway system line, but a passthrough client that sends no system at all
// (plenty of SDKs and hand-rolled curl calls) starts with a user message and
// would then be refused on every single request, cooling the whole pool and
// looking like "every account is banned while the website still works".
//
// Scope: a system/developer first message is left alone except developer→system
// normalization (the upstream role whitelist has no developer); anything else
// gets one gateway line prepended, all other messages untouched.
func EnsureSystemFirst(messages []map[string]any, fallback string) []map[string]any {
	if len(messages) == 0 {
		return messages
	}
	role, _ := messages[0]["role"].(string)
	switch role {
	case "system":
		return messages
	case "developer":
		out := make([]map[string]any, len(messages))
		copy(out, messages)
		out[0] = map[string]any{"role": "system", "content": messages[0]["content"]}
		return out
	}
	line := fallback
	if line == "" {
		line = DegradedSystemPrompt
	}
	out := make([]map[string]any, 0, len(messages)+1)
	out = append(out, map[string]any{"role": "system", "content": line})
	out = append(out, messages...)
	return out
}

// Append inserts a gateway system message right after the leading run of
// system/developer messages, leaving every existing message byte-identical
// (upstream misakano issue #129 "append" mode). The client's own conventions
// and the gateway's line then both apply.
//
// The insert point is after the leading run only: a system message further
// down the conversation belongs to some mid-conversation event and must not be
// separated from it. A zero-length run puts the gateway line first.
func Append(messages []map[string]any, gatewayPrompt string) []map[string]any {
	if len(messages) == 0 || gatewayPrompt == "" {
		return messages
	}
	at := 0
	for _, m := range messages {
		role, _ := m["role"].(string)
		if role != "system" && role != "developer" {
			break
		}
		at++
	}
	out := make([]map[string]any, 0, len(messages)+1)
	out = append(out, messages[:at]...)
	out = append(out, map[string]any{"role": "system", "content": gatewayPrompt})
	out = append(out, messages[at:]...)
	return out
}

// MoveSystemToUser relocates the caller's system text VERBATIM into the first
// user message and puts a neutral line in the system slot. It is the SECOND
// rescue rung for a 11128 refusal, used when SplitSystem cannot cut anything.
//
// The difference from ReplaceSystem is loss: ReplaceSystem deletes the text,
// this only changes the carrying channel (system → user) and loses no
// character. The edge's 11128 test reads system messages, so the same text
// inside a user turn passes — this is a channel change, not an audit bypass.
//
// Returns false (do not touch the request) when there is no system text, when
// the system content is not plain text, when there is no user message, or when
// the first user message is multimodal — inserting a text block into parts
// would reorder them.
func MoveSystemToUser(messages []map[string]any, neutral string) ([]map[string]any, bool) {
	if len(messages) == 0 || neutral == "" {
		return messages, false
	}
	sysText, ok := messages[0]["content"].(string)
	if !ok || strings.TrimSpace(sysText) == "" {
		return messages, false
	}
	role, _ := messages[0]["role"].(string)
	if role != "system" && role != "developer" {
		return messages, false
	}
	for i := 1; i < len(messages); i++ {
		if r, _ := messages[i]["role"].(string); r != "user" {
			continue
		}
		txt, ok := messages[i]["content"].(string)
		if !ok {
			return messages, false // multimodal parts: prepending would reorder them
		}
		out := make([]map[string]any, len(messages))
		copy(out, messages)
		out[i] = map[string]any{"role": "user", "content": sysText + "\n\n" + txt}
		out[0] = map[string]any{"role": "system", "content": neutral}
		return out, true
	}
	return messages, false
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
// The value is a UUID-v4-shaped hash, not "<uid>:<conversation>". The real
// client sends a UUID here; a gateway that writes the account uid in the
// clear into a request body is self-identifying, and the uid is exactly the
// field a risk-control rule would key on (BP-90, following
// misakano7545/workbuddy2api-panel issue #51). The uid still participates in
// the hash, so cross-account isolation is unchanged.
//
// No conversation identity means NO cache key: falling back to the bare uid
// gives every unrelated conversation one shared prefix slot, which is exactly
// the cross-conversation contamination seen on 2026-10-20. Omitting the field
// only costs cache hits; claiming a shared slot costs correctness.
func InjectPromptCacheKey(body map[string]any, uid, conversationID string) {
	if uid == "" || conversationID == "" {
		return
	}
	body["prompt_cache_key"] = StableUUID(uid + "|" + conversationID)
}

// StableUUID derives a UUID-v4-shaped (36-char, dashed) value from a seed.
//
// The version and variant nibbles are set per RFC 4122: leaving them as they
// fall would put impossible values in the `4xxx` / `[89ab]xxx` positions,
// which is a distinguishable trait rather than a disguise.
func StableUUID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
