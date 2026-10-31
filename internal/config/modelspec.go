// Model capability declarations for /v1/models, in the models.dev vocabulary.
//
// WHY THIS EXISTS (2026-11-01, BP-243): the gateway used to publish only its
// own dialect — id/object/created/owned_by plus a flat context_length and a
// pricing note. That is readable by us and by clients that happen to know our
// keys, but a client that reads the models.dev vocabulary (magpie and the
// OpenCode family, both of which route through gateways like this one) gets
// nothing: it cannot tell whether a model takes images, whether it calls tools,
// or which reasoning gears it offers. Field evidence from misakano7545's panel
// (commit 7490cfbf): against a gateway publishing only flat keys, magpie showed
// Context/Output (it reads context_length) but images and efforts came out
// empty/default — the models silently degraded to text-only.
//
// THREE RULES, and they are the whole contract of this file:
//
//  1. PURELY ADDITIVE. These helpers only supply models.dev-shaped keys; the
//     caller keeps writing id/object/created/owned_by/context_length/pricing
//     exactly as before. Nothing here overwrites an existing key.
//  2. ONE SOURCE. Values are derived from the same spec table the panel's
//     "模型与档位" page uses, so the two can never drift apart. The panel reads
//     this table; /v1/models reads this table. Neither recomputes.
//  3. NEVER INVENT. A field with no source is omitted, not defaulted. There is
//     no cost/attachment/release_date data here, so none is written — a wrong
//     value is worse than a missing one for any client that branches on
//     presence.
package config

// ModelSpec is one model's capability declaration in models.dev terms.
//
// InputModes are the models.dev input modality words ("text", "image",
// "video", "audio", "pdf"). Reasoning lists the effort gears the model accepts,
// with "off"/"关闭" meaning thinking can be turned off (models.dev expresses
// that as a toggle, see ReasoningOptions).
type ModelSpec struct {
	ContextLen int
	MaxOutput  int
	InputModes []string
	// Reasoning holds the gear names the upstream accepts, e.g. "low",
	// "high", "max". An empty slice means "no gear information", which is
	// different from "no reasoning": the former omits the keys entirely.
	Reasoning []string
	// ThinkingOff marks a model whose thinking can be disabled. models.dev
	// spells this as a toggle option beside the effort list.
	ThinkingOff bool
	ToolCall    bool
	Structured  bool
}

// modelSpecs is the single capability table. Every entry is sourced from the
// same material the panel's tier catalog was built from (models.dev consensus,
// upstream /v1/models probes, and the vendor docs recorded in
// research/2026-10-18-三渠道模型倍率核实.md). Only add a row when the numbers
// have a recorded source; an absent row is honest.
var modelSpecs = map[string]ModelSpec{
	// --- GLM (z-ai) ---
	"glm-5.3":       {ContextLen: 1000000, MaxOutput: 131072, InputModes: []string{"text"}, Reasoning: []string{"low", "high", "max"}, ToolCall: true, Structured: true},
	"glm-5.3-flash": {ContextLen: 1000000, MaxOutput: 131072, InputModes: []string{"text", "image", "video", "pdf"}, Reasoning: []string{"low", "high", "max"}, ToolCall: true, Structured: true},
	"glm-5.2":       {ContextLen: 1000000, MaxOutput: 131072, InputModes: []string{"text"}, Reasoning: []string{"high", "max"}, ToolCall: true, Structured: true},
	"glm-5.1":       {ContextLen: 200000, MaxOutput: 131072, InputModes: []string{"text"}, ToolCall: true, Structured: true},

	// --- DeepSeek ---
	"deepseek-v4-pro":   {ContextLen: 1000000, MaxOutput: 393216, InputModes: []string{"text"}, Reasoning: []string{"high", "max"}, ThinkingOff: true, ToolCall: true, Structured: true},
	"deepseek-v4-flash": {ContextLen: 1000000, MaxOutput: 393216, InputModes: []string{"text", "image"}, Reasoning: []string{"high"}, ThinkingOff: true, ToolCall: true, Structured: true},
	// 4.1-flash: measured 2026-11-01 to accept ~1.02M input tokens on Trae's
	// tool surface; the context here is the catalogue's own max.
	"deepseek-v4.1-flash": {ContextLen: 1000000, MaxOutput: 393216, InputModes: []string{"text", "image"}, Reasoning: []string{"low", "high", "max"}, ThinkingOff: true, ToolCall: true, Structured: true},

	// --- Kimi (moonshot) ---
	"kimi-k3":       {ContextLen: 1048576, MaxOutput: 1048576, InputModes: []string{"text", "image", "video"}, Reasoning: []string{"low", "high", "max"}, ToolCall: true, Structured: true},
	"kimi-k3-1":     {ContextLen: 1048576, MaxOutput: 1048576, InputModes: []string{"text", "image"}, Reasoning: []string{"low", "high", "max"}, ToolCall: true, Structured: true},
	"kimi-k3.1-pro": {ContextLen: 1048576, MaxOutput: 1048576, InputModes: []string{"text", "image"}, Reasoning: []string{"low", "high", "max"}, ToolCall: true, Structured: true},
	"kimi-k2.7":     {ContextLen: 262144, MaxOutput: 262144, InputModes: []string{"text", "image", "video"}, ThinkingOff: true, ToolCall: true, Structured: true},
	"kimi-k2.6":     {ContextLen: 262144, MaxOutput: 262144, InputModes: []string{"text", "image", "video"}, ThinkingOff: true, ToolCall: true, Structured: true},

	// --- MiniMax ---
	"minimax-m3":         {ContextLen: 1048576, MaxOutput: 512000, InputModes: []string{"text", "image", "video"}, ThinkingOff: true, ToolCall: true, Structured: true},
	"minimax-m2.7":       {ContextLen: 1000000, MaxOutput: 512000, InputModes: []string{"text", "image", "video"}, ToolCall: true, Structured: true},
	"minimax-m3.1-flash": {ContextLen: 1000000, MaxOutput: 512000, InputModes: []string{"text"}, ToolCall: true, Structured: true},

	// --- Qwen ---
	"qwen3.8-max":   {ContextLen: 1000000, MaxOutput: 131072, InputModes: []string{"text", "image", "video"}, Reasoning: []string{"low", "medium", "xhigh"}, ThinkingOff: true, ToolCall: true, Structured: true},
	"qwen3.8-flash": {ContextLen: 1000000, MaxOutput: 131072, InputModes: []string{"text", "image", "video"}, Reasoning: []string{"low", "medium", "xhigh"}, ThinkingOff: true, ToolCall: true, Structured: true},
	"qwen-3.5":      {ContextLen: 1000000, MaxOutput: 131072, InputModes: []string{"text"}, ToolCall: true, Structured: true},
	"qwen-3.6-plus": {ContextLen: 1000000, MaxOutput: 131072, InputModes: []string{"text"}, ToolCall: true, Structured: true},

	// --- ByteDance Doubao ---
	"doubao-seed-code":    {ContextLen: 256000, MaxOutput: 131072, InputModes: []string{"text"}, ToolCall: true, Structured: true},
	"doubao-seed-2.1-pro": {ContextLen: 256000, MaxOutput: 128000, InputModes: []string{"text", "image", "video"}, Reasoning: []string{"low", "high"}, ThinkingOff: true, ToolCall: true, Structured: true},

	// --- Others seen in the CN pools ---
	"mimo-v2.6-flash": {ContextLen: 1000000, MaxOutput: 131072, InputModes: []string{"text"}, ToolCall: true, Structured: true},
	"mimo-v2.6-pro":   {ContextLen: 1000000, MaxOutput: 131072, InputModes: []string{"text"}, ToolCall: true, Structured: true},
	"glm-5.3-flashx":  {ContextLen: 1000000, MaxOutput: 131072, InputModes: []string{"text", "image", "video", "pdf"}, Reasoning: []string{"low", "high", "max"}, ToolCall: true, Structured: true},
}

// ModelSpecFor resolves a canonical model name to its capability declaration.
// A native binding name is tried after the canonical one, so a route whose
// canonical id differs from the upstream id still finds its spec.
func ModelSpecFor(canonical string, natives []string) (ModelSpec, bool) {
	if s, ok := modelSpecs[canonical]; ok {
		return s, true
	}
	for _, n := range natives {
		if s, ok := modelSpecs[n]; ok {
			return s, true
		}
	}
	return ModelSpec{}, false
}
