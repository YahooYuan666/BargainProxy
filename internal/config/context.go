// Static context-window table for /v1/models metadata.
//
// Ported from the MIT-licensed workbuddy2api-panel context catalog
// (internal/upstream/context_catalog.go; see NOTICE). Values are upstream
// maxInputTokens measurements recorded there (CN side) plus models.dev
// consensus entries; each family keeps the reference's source annotation.
//
// Unknown models are omitted entirely — unlike the reference, this gateway
// does not advertise a padded 1M default (ticket 13C: 不拉满伪装).
package config

import "strings"

var contextLengths = map[string]int{
	// GLM (z-ai): 实测 CN maxInputTokens + models.dev 共识.
	"glm-5.3":       1000000,
	"glm-5.3-flash": 1000000,
	"glm-5.2":       1000000,
	"glm-5.1":       200000,

	// Kimi (moonshot).
	"kimi-k3":   1048576, // models.dev 官方
	"kimi-k2.7": 256000,  // 实测 CN
	"kimi-k2.6": 256000,  // 实测 CN
	"kimi-k2.5": 164000,  // 实测（global 侧同 id 外推）

	// MiniMax (tencent).
	"minimax-m3": 512000, // 实测 CN

	// DeepSeek.
	"deepseek-v4-pro":     1000000, // 实测 CN
	"deepseek-v4-flash":   1000000, // 实测 CN
	"deepseek-v4.1-flash": 1000000, // 实测外推 + models.dev 共识

	// Google.
	"gemini-3.5-flash": 1048576, // models.dev 共识
}

// ContextLength returns the advertised context window (in tokens) for a model
// id. ok=false for models absent from the table (callers omit the field).
func ContextLength(model string) (int, bool) {
	n, ok := contextLengths[strings.ToLower(strings.TrimSpace(model))]
	return n, ok
}
