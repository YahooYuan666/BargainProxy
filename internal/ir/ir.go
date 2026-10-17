// Package ir defines the unified intermediate representation (IR) shared by
// protocol ingress/egress and upstream drivers. Every client request is
// normalized into this shape before routing, and every upstream response is
// converted back through it.
package ir

import "strings"

// Role values for messages in the normalized conversation.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ContentPart is one element of a message. Kind is "text" or "image".
type ContentPart struct {
	Kind      string `json:"kind"` // "text" | "image"
	Text      string `json:"text,omitempty"`
	ImageMIME string `json:"image_mime,omitempty"` // e.g. image/png
	ImageB64  string `json:"image_b64,omitempty"`  // base64 payload without data: prefix
}

// ToolCall is an assistant-issued function call. Arguments is a JSON string
// (possibly partial during streaming).
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ThinkingBlock is one assistant reasoning block from history. It is kept
// separate from Content so OpenAI-shaped drivers (which have no replayable
// thinking concept) never mistake it for body text. Signature is the
// upstream's opaque proof; empty means the provider did not issue one (BP-69).
type ThinkingBlock struct {
	Text      string `json:"text,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// Message is one normalized conversation message.
type Message struct {
	Role       Role          `json:"role"`
	Content    []ContentPart `json:"content,omitempty"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`   // assistant messages
	ToolCallID string        `json:"tool_call_id,omitempty"` // tool messages
	ToolName   string        `json:"tool_name,omitempty"`    // tool messages
	// Thinking carries replayable assistant reasoning blocks (Anthropic
	// history). Only anthropic-shaped upstream drivers may replay them; the
	// ingress never drops them before routing (BP-69).
	Thinking []ThinkingBlock `json:"thinking,omitempty"`
}

// ToolDef declares an available function tool.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ThinkingConfig carries reasoning controls from the client protocol.
type ThinkingConfig struct {
	Enabled      bool
	BudgetTokens int
	Level        string // minimal|low|medium|high — channel-specific mapping
	// Include marks "the client wants the reasoning text back" (Anthropic
	// thinking enabled; Gemini includeThoughts). Gates the thinking egress
	// for protocols where it is opt-in — BP-41.
	Include bool
}

// LevelFromBudget maps an Anthropic-style thinking budget to a coarse effort
// level for channels that only speak gears (workbuddy reasoning_effort,
// Gemini thinkingLevel). The AIClient2API thresholds (50/200) sit below
// Anthropic's 1024 minimum, so this uses sane bands instead. Returns "" when
// no budget is known.
func LevelFromBudget(budget int) string {
	switch {
	case budget <= 0:
		return ""
	case budget < 4096:
		return "low"
	case budget < 16384:
		return "medium"
	default:
		return "high"
	}
}

// BudgetFromLevel is the inverse of LevelFromBudget for channels whose wire
// speaks Anthropic thinking budgets but whose client only sent a gear name
// (ZCode's Chat/Responses ingress sends reasoning_effort, not budget). The
// values map back into the same bands LevelFromBudget defines (BP-69).
func BudgetFromLevel(level string) int {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "minimal":
		return 1024
	case "low":
		return 2048
	case "medium":
		return 8192
	case "high":
		return 16384
	case "xhigh", "max":
		return 32768
	default:
		return 0
	}
}

// NormalizeToolChoice folds every ingress dialect (OpenAI chat object,
// Responses flat object, Anthropic {"type":"tool","name":...}) into the
// portable string vocabulary the drivers share: "auto" | "none" | "required".
// The specific tool name and disable_parallel_tool_use ride alongside instead
// of inside an object — workbuddy/trae upstreams reject objects outright
// (BP-69).
func NormalizeToolChoice(v any) (choice any, name string, disableParallel bool) {
	switch t := v.(type) {
	case nil:
		return nil, "", false
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "auto":
			return "auto", "", false
		case "none":
			return "none", "", false
		case "required", "any", "tool":
			return "required", "", false
		default:
			return nil, "", false
		}
	case map[string]any:
		dp, _ := t["disable_parallel_tool_use"].(bool)
		switch typ, _ := t["type"].(string); typ {
		case "function":
			if fn, ok := t["function"].(map[string]any); ok {
				if n, _ := fn["name"].(string); n != "" {
					return "required", n, dp
				}
			}
			if n, _ := t["name"].(string); n != "" {
				return "required", n, dp
			}
			return "required", "", dp
		case "tool":
			n, _ := t["name"].(string)
			return "required", n, dp
		case "any", "required":
			return "required", "", dp
		case "auto":
			return "auto", "", dp
		case "none":
			return "none", "", dp
		}
		return nil, "", dp
	default:
		return nil, "", false
	}
}

// ChatRequest is the normalized chat request.
type ChatRequest struct {
	Model      string
	Messages   []Message
	Tools      []ToolDef
	ToolChoice any
	// ToolChoiceName is the specific tool an Anthropic-style
	// {"type":"tool","name":"x"} asked for; ToolChoice itself is normalized to
	// the portable "required"/"auto"/"none" vocabulary (BP-69).
	ToolChoiceName string
	// DisableParallel carries Anthropic tool_choice.disable_parallel_tool_use.
	// Only anthropic-shaped upstreams consume it today.
	DisableParallel bool
	Temperature     *float64
	TopP            *float64
	// TopK is Anthropic/Gemini-only sampling; OpenAI-shaped drivers ignore it.
	TopK      *int
	MaxTokens int
	Stop      []string
	Stream    bool
	Thinking  *ThinkingConfig

	// RequestedEffort mirrors OpenAI reasoning_effort when supplied.
	RequestedEffort string

	// SessionKey drives sticky routing. The ingress derives it with a
	// three-level fallback: explicit protocol metadata ids → cache_control
	// marked-block digest → system+first-user prefix digest. Empty means
	// "no stable identity" (never stick).
	SessionKey string

	// Raw carries protocol-specific extras (rarely needed downstream).
	Raw map[string]any
}

// StopReason values in the normalized namespace.
type StopReason string

const (
	StopEnd           StopReason = "stop"
	StopLength        StopReason = "length"
	StopToolUse       StopReason = "tool_use"
	StopContentFilter StopReason = "content_filter"
)

// Usage is the normalized token accounting block.
//
// InputTokens is the FULL prompt, cache hits included, for every channel:
// OpenAI's prompt_tokens, Gemini's promptTokenCount and Anthropic's
// input_tokens + cache_creation + cache_read all normalize to it.
// CachedInputTokens is the subset served from the prefix cache (only
// meaningful together with CacheKnown). A renderer whose wire protocol splits
// the prompt differently (Anthropic) must re-split from these fields instead
// of copying InputTokens verbatim — see protocol/anthropic.UsageFromIR (BP-56).
type Usage struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`
	// CacheKnown marks whether the upstream reported prefix-cache accounting
	// for this request at all. A reported 0-hit request must still count in the
	// hit-rate denominator (BP-53) — without the flag those samples would be
	// silently dropped and the rate inflated.
	CacheKnown      bool `json:"cache_known,omitempty"`
	ReasoningTokens int  `json:"reasoning_tokens,omitempty"`
	// Credit is the upstream-reported credit consumption for this request
	// (workbuddy final-frame usage.credit — agentpoints logging.go parity).
	// CreditKnown marks whether the upstream actually reported the field;
	// zero without it means "no data" (quota-based channels), not "0 spent"
	// (BP-40).
	Credit      float64 `json:"credit,omitempty"`
	CreditKnown bool    `json:"credit_known,omitempty"`
}

// ChatResponse is a fully materialized (non-streamed) response.
type ChatResponse struct {
	Message    Message    `json:"message"`
	StopReason StopReason `json:"stop_reason"`
	Usage      Usage      `json:"usage"`
	Model      string     `json:"model"`
	UpstreamID string     `json:"upstream_id,omitempty"`
	CreatedAt  int64      `json:"created_at,omitempty"`
	// Thinking is the aggregated reasoning text for non-streamed rendering
	// (Anthropic thinking block / OpenAI reasoning_content / Gemini thought
	// part). Empty when the upstream produced none — BP-41.
	Thinking string `json:"thinking,omitempty"`
	// ThinkingSignature is the upstream-issued signature for Thinking. Only
	// anthropic-shaped upstreams issue one; when present the Anthropic egress
	// renders it and the client can replay the block (BP-69).
	ThinkingSignature string `json:"thinking_signature,omitempty"`
}

// StreamEventKind enumerates incremental stream events.
type StreamEventKind string

const (
	EvText     StreamEventKind = "text"     // Delta = text fragment
	EvThinking StreamEventKind = "thinking" // Delta = reasoning fragment
	// EvThinkingSignature carries the upstream's signature_delta for the
	// currently open thinking block (Anthropic wire only). Delta = signature.
	EvThinkingSignature StreamEventKind = "thinking_signature"
	EvToolStart         StreamEventKind = "tool_start" // ToolCall = {ID,Name}; ToolIndex set
	EvToolArgs          StreamEventKind = "tool_args"  // Delta = arguments fragment; ToolIndex identifies the call
	EvUsage             StreamEventKind = "usage"      // Usage set
	EvDone              StreamEventKind = "done"       // Stop set (terminal event)
)

// StreamEvent is one incremental event from a driver stream.
type StreamEvent struct {
	Kind      StreamEventKind
	Delta     string
	ToolIndex int
	ToolCall  *ToolCall
	Usage     *Usage
	Stop      StopReason
}
