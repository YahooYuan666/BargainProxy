// Output-token limits and request-side clamping.
//
// Two separate problems live here, both about max_tokens:
//
//  1. TOO SMALL. Some upstreams reject a tiny max_tokens with a 400 whose
//     wording carries the minimum ("max_tokens: 1 is less than the minimum of
//     16"). A client that sends max_tokens=1 as a liveness probe therefore
//     looks like a broken gateway. floorFromMessage recovers that number so the
//     call can be retried once with a legal value.
//
//  2. TOO LARGE. Every model has its own output ceiling. Sending
//     max_tokens=100000 to a model that stops at 32000 gets either a 400 or a
//     silent truncation, and a silent truncation is worse: the client sees a
//     short answer and has no idea it was our doing. clampOutput caps the
//     request at the model's real ceiling before it leaves the gateway.
//
// The table is deliberately conservative: a value is listed only when the
// model's published ceiling is known. An unlisted model is left untouched
// rather than given an invented limit (the same discipline as context.go:
// 不拉满伪装).

package config

import (
	"regexp"
	"strconv"
	"strings"
)

// outputLimits maps a canonical or upstream model id to its maximum output
// tokens. Values follow the vendor's published limits / models.dev entries.
var outputLimits = map[string]int{
	// DeepSeek: 8k default, 32k ceiling on the V4 family (the 32000 wall this
	// gateway has hit in the field is exactly this).
	"deepseek-v4-pro":     32000,
	"deepseek-v4-flash":   32000,
	"deepseek-v4.1-flash": 32000,
	"deepseek-flash":      32000,

	// GLM.
	"glm-5.3":        32000,
	"glm-5.3-flash":  32000,
	"glm-5.3-flashx": 32000,
	"glm-5.2":        32000,
	"glm-5.1":        32000,

	// Kimi.
	"kimi-k3":   32000,
	"kimi-k2.7": 32000,
	"kimi-k2.6": 32000,
	"kimi-k2.5": 32000,

	// Qwen.
	"qwen3.8-max":   32000,
	"qwen3.8-flash": 32000,
	"qwen3.5-plus":  32000,
	"qwen3.6-plus":  32000,
	"qwen3.7-max":   32000,
	"qwen3.7-plus":  32000,

	// Doubao.
	"doubao-seed-2-1-pro":   32000,
	"doubao-seed-2-1-turbo": 32000,

	// MiniMax.
	"minimax-m3":   32000,
	"minimax-m2.7": 32000,

	// Google: Gemini's output ceiling is much higher than the CN models.
	"gemini-3.8-flash":         65536,
	"gemini-3.5-flash":         65536,
	"gemini-claude-sonnet-4-6": 64000,
	"gemini-claude-opus-4-6":   64000,
}

// OutputLimit returns the model's maximum output tokens. ok=false when the
// ceiling is unknown — callers must then leave the request alone.
func OutputLimit(model string) (int, bool) {
	n, ok := outputLimits[strings.ToLower(strings.TrimSpace(model))]
	return n, ok
}

// ClampOutput caps maxTokens at the model's ceiling. It returns the value to
// send and whether it changed. model is tried first; canonical is the fallback
// (the router knows both names, the driver only one).
func ClampOutput(maxTokens int, model, canonical string) (int, bool) {
	if maxTokens <= 0 {
		return maxTokens, false // unset / unlimited: nothing to clamp
	}
	limit, ok := OutputLimit(model)
	if !ok {
		limit, ok = OutputLimit(canonical)
	}
	if !ok || maxTokens <= limit {
		return maxTokens, false
	}
	return limit, true
}

// minTokenFloor matches the wording several upstreams use when max_tokens is
// below their minimum. The number is captured so the request can be repaired
// instead of failing the turn.
//
//	"max_tokens: 1 is less than the minimum of 16"
//	"max_tokens must be greater than or equal to 16"
//	"max_tokens 参数必须大于等于 16"
//
// Escaped JSON (">" as "\u003e") is normalised before matching.
var minTokenFloor = regexp.MustCompile(
	`(?i)max[_ ]?(?:output[_ ]?|completion[_ ]?)?tokens?[^0-9]{0,40}?(?:at least|minimum of|greater than or equal to|must be >|>=|大于等于|至少)\s*[:：]?\s*(\d{1,6})`,
)

// alsoFloor catches the "is less than the minimum of N" ordering, where the
// number trails the phrase rather than following it.
var alsoFloor = regexp.MustCompile(
	`(?i)less than the minimum of\s*(\d{1,6})|小于最小值\s*[:：]?\s*(\d{1,6})|minimum is\s*(\d{1,6})`,
)

// FloorFromMessage extracts the minimum output-token count an upstream demands,
// from a 400 body. Returns 0 when the text carries no such number.
//
// A value above 4096 is rejected: real minimums are small (1/16/32), and a
// large number in an unrelated 400 message would otherwise cause a request to
// be silently inflated.
func FloorFromMessage(body string) int {
	if body == "" {
		return 0
	}
	text := strings.ReplaceAll(body, `\u003e`, ">")
	text = strings.ReplaceAll(text, `\u003c`, "<")
	for _, re := range []*regexp.Regexp{minTokenFloor, alsoFloor} {
		m := re.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		for _, g := range m[1:] {
			if g == "" {
				continue
			}
			n, err := strconv.Atoi(g)
			if err != nil || n <= 0 || n > 4096 {
				continue
			}
			return n
		}
	}
	return 0
}
