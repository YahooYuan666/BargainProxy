package qoder

// stream：网关 SSE 信封帧 → OpenAI chunk 转换、流式回放与非流式聚合
// （移植 Qoder-2API-Go transform 的 ExtractDelta/StreamAccumulator）。

import (
	"encoding/json"
	"strings"
	"sync"
)

// usage 网关用量（OpenAI 标准字段 + Qoder credits 扩展）。
type usage struct {
	PromptTokens            int     `json:"prompt_tokens"`
	CompletionTokens        int     `json:"completion_tokens"`
	TotalTokens             int     `json:"total_tokens"`
	PromptTokensDetails     *detail `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *detail `json:"completion_tokens_details,omitempty"`
	Credits                 float64 `json:"credits"`
	OriginalCredits         float64 `json:"original_credits"`
	Billable                bool    `json:"-"`
}

type detail struct {
	CachedTokens    int `json:"cached_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

func extractUsageObj(v interface{}) *usage {
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	u := &usage{
		PromptTokens:     toInt(m["prompt_tokens"]),
		CompletionTokens: toInt(m["completion_tokens"]),
		TotalTokens:      toInt(m["total_tokens"]),
		Credits:          toFloat(m["credits"]),
		OriginalCredits:  toFloat(m["original_credits"]),
		Billable:         true,
	}
	if b, ok := m["billable"].(bool); ok {
		u.Billable = b
	}
	if d, ok := m["prompt_tokens_details"].(map[string]interface{}); ok {
		u.PromptTokensDetails = &detail{CachedTokens: toInt(d["cached_tokens"])}
	}
	if d, ok := m["completion_tokens_details"].(map[string]interface{}); ok {
		u.CompletionTokensDetails = &detail{ReasoningTokens: toInt(d["reasoning_tokens"])}
	}
	return u
}

func toFloat(v interface{}) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// bridgeDelta 单帧增量。
type bridgeDelta struct {
	Role             string
	Content          string
	ReasoningContent string
	FinishReason     string
	ToolCalls        []map[string]interface{}
	Usage            *usage
}

func (d *bridgeDelta) isEmpty() bool {
	return d.Role == "" && d.Content == "" && d.ReasoningContent == "" &&
		d.FinishReason == "" &&
		(d.ToolCalls == nil || len(d.ToolCalls) == 0) && d.Usage == nil
}

// extractDelta 解析网关信封帧 {"body":"<json字符串>"} → 增量。
func extractDelta(dataLine string) *bridgeDelta {
	delta := &bridgeDelta{}
	var wrapper map[string]interface{}
	if json.Unmarshal([]byte(dataLine), &wrapper) != nil {
		return delta
	}
	innerStr, ok := wrapper["body"].(string)
	if !ok || innerStr == "" {
		return delta
	}
	var innerJSON map[string]interface{}
	if json.Unmarshal([]byte(innerStr), &innerJSON) != nil {
		return delta
	}
	// usage 两种形态：末尾独立空 choices 帧（Qwen 风格）或挂在最后内容帧
	//（GLM 风格），两处都捕获。
	delta.Usage = extractUsageObj(innerJSON["usage"])
	choices, ok := innerJSON["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return delta
	}
	for _, ch := range choices {
		choice, ok := ch.(map[string]interface{})
		if !ok {
			continue
		}
		if finish, _ := choice["finish_reason"].(string); finish != "" {
			delta.FinishReason = finish
		}
		deltaMap, _ := choice["delta"].(map[string]interface{})
		role, _ := deltaMap["role"].(string)
		content, _ := deltaMap["content"].(string)
		reasoning, _ := deltaMap["reasoning_content"].(string)
		var toolCalls []map[string]interface{}
		if tc, hasTC := deltaMap["tool_calls"].([]interface{}); hasTC && len(tc) > 0 {
			toolCalls = interfaceSliceToMapSlice(tc)
		}
		if role != "" || content != "" || reasoning != "" || toolCalls != nil || delta.FinishReason != "" {
			delta.Role = role
			delta.Content = content
			delta.ReasoningContent = reasoning
			delta.ToolCalls = toolCalls
			return delta
		}
	}
	return delta
}

func interfaceSliceToMapSlice(slice []interface{}) []map[string]interface{} {
	result := []map[string]interface{}{}
	for _, item := range slice {
		if m, ok := item.(map[string]interface{}); ok {
			result = append(result, m)
		}
	}
	return result
}

// ---- 工具调用聚合 ----

const maxToolCalls = 128

type toolCallAccumulator struct {
	mu    sync.Mutex
	calls []map[string]interface{}
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{calls: []map[string]interface{}{}}
}

func (a *toolCallAccumulator) append(deltaCalls []map[string]interface{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, dc := range deltaCalls {
		if len(a.calls) >= maxToolCalls {
			return
		}
		idx, ok := dc["index"].(float64)
		if !ok {
			idx = float64(len(a.calls))
		}
		i := int(idx)
		if i < 0 || i > len(a.calls) || i >= maxToolCalls {
			i = len(a.calls)
		}
		for len(a.calls) <= i {
			a.calls = append(a.calls, map[string]interface{}{
				"id":       "",
				"type":     "function",
				"function": map[string]interface{}{"name": "", "arguments": ""},
			})
		}
		existing := a.calls[i]
		if id, ok := dc["id"].(string); ok {
			existing["id"] = id
		}
		if t, ok := dc["type"].(string); ok {
			existing["type"] = t
		}
		df, _ := dc["function"].(map[string]interface{})
		ef, _ := existing["function"].(map[string]interface{})
		if df != nil {
			if name, ok := df["name"].(string); ok {
				ef["name"] = name
			}
			if args, ok := df["arguments"].(string); ok {
				cur, _ := ef["arguments"].(string)
				ef["arguments"] = cur + args
			}
		}
	}
}

func (a *toolCallAccumulator) isEmpty() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls) == 0
}

func (a *toolCallAccumulator) snapshot() []map[string]interface{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]map[string]interface{}, len(a.calls))
	copy(out, a.calls)
	return out
}

// withToolCallIndices 补 index（并行工具调用依赖）。
func withToolCallIndices(raw []map[string]interface{}) []map[string]interface{} {
	indexed := []map[string]interface{}{}
	for i, tc := range raw {
		cp := map[string]interface{}{}
		for k, v := range tc {
			cp[k] = v
		}
		if _, ok := cp["index"]; !ok {
			cp["index"] = float64(i)
		}
		indexed = append(indexed, cp)
	}
	return indexed
}

func isPotentialToolCallText(text string) bool {
	candidate := strings.TrimLeft(text, " \t\n\r")
	if candidate == "" {
		return true
	}
	return strings.HasPrefix("Tool calls:", candidate) || strings.HasPrefix(candidate, "Tool calls:")
}

// ---- SSE chunk 构造 ----

func makeChunk(reqID string, created int64, model string) map[string]interface{} {
	return map[string]interface{}{
		"id":      reqID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]interface{}{
			{"index": 0, "delta": map[string]interface{}{}, "finish_reason": nil},
		},
	}
}

func makeSSEChunk(reqID string, created int64, model, role, content, reasoningContent string, toolCalls []map[string]interface{}) string {
	chunk := makeChunk(reqID, created, model)
	choices, _ := chunk["choices"].([]map[string]interface{})
	if len(choices) == 0 {
		return ""
	}
	delta := choices[0]["delta"].(map[string]interface{})
	if role != "" {
		delta["role"] = role
	}
	if content != "" {
		delta["content"] = content
	}
	if reasoningContent != "" {
		delta["reasoning_content"] = reasoningContent
	}
	if len(toolCalls) > 0 {
		delta["tool_calls"] = toolCalls
	}
	raw, err := marshalNoEscape(chunk)
	if err != nil {
		return ""
	}
	return "data: " + string(raw) + "\n\n"
}

// ---- 流累加器 ----

// streamAccumulator 缓冲 potential-tool-call 文本、即时回放其余增量。
type streamAccumulator struct {
	reqID            string
	created          int64
	model            string
	toolCallFallback bool
	toolCalls        *toolCallAccumulator
	pendingContent   []string
	pendingRole      string
	finishReason     string
	emitted          bool
	streamingText    bool
	emitFn           func(string)
}

func newStreamAccumulator(reqID string, created int64, model string, toolCallFallback bool, emitFn func(string)) *streamAccumulator {
	return &streamAccumulator{
		reqID:            reqID,
		created:          created,
		model:            model,
		toolCallFallback: toolCallFallback,
		toolCalls:        newToolCallAccumulator(),
		pendingContent:   []string{},
		pendingRole:      "assistant",
		emitFn:           emitFn,
	}
}

func (a *streamAccumulator) accept(delta *bridgeDelta) {
	if delta.Role != "" {
		a.pendingRole = delta.Role
	}
	if delta.FinishReason != "" {
		a.finishReason = delta.FinishReason
	}
	if delta.ReasoningContent != "" {
		a.emit("", delta.ReasoningContent, nil)
	}
	if len(delta.ToolCalls) > 0 {
		a.discardBufferedToolCallText()
		a.toolCalls.append(delta.ToolCalls)
		a.emit("", "", withToolCallIndices(delta.ToolCalls))
		return
	}
	if delta.Content == "" {
		return
	}
	if !a.toolCallFallback || a.streamingText {
		a.streamingText = true
		a.emit(delta.Content, "", nil)
		return
	}
	a.pendingContent = append(a.pendingContent, delta.Content)
	text := strings.Join(a.pendingContent, "")
	if isPotentialToolCallText(text) {
		return
	}
	a.streamingText = true
	a.emitBufferedText()
}

func (a *streamAccumulator) flush() {
	if len(a.pendingContent) == 0 {
		return
	}
	buffered := strings.Join(a.pendingContent, "")
	a.pendingContent = nil
	var parsed []normalizedToolCall
	if a.toolCallFallback {
		parsed = parseToolCallsText(buffered)
	}
	if parsed != nil {
		maps := toolsCallsToMaps(parsed)
		a.toolCalls.append(maps)
		a.emit("", "", withToolCallIndices(maps))
		return
	}
	a.streamingText = true
	a.emit(buffered, "", nil)
}

func (a *streamAccumulator) finalFinishReason() string {
	if !a.toolCalls.isEmpty() {
		return "tool_calls"
	}
	if a.finishReason != "" {
		return a.finishReason
	}
	return "stop"
}

func (a *streamAccumulator) emitBufferedText() {
	if len(a.pendingContent) == 0 {
		return
	}
	buffered := strings.Join(a.pendingContent, "")
	a.pendingContent = nil
	a.emit(buffered, "", nil)
}

func (a *streamAccumulator) discardBufferedToolCallText() {
	if len(a.pendingContent) == 0 {
		return
	}
	buffered := strings.Join(a.pendingContent, "")
	a.pendingContent = nil
	if a.toolCallFallback && isPotentialToolCallText(buffered) {
		return
	}
	a.streamingText = true
	a.emit(buffered, "", nil)
}

func (a *streamAccumulator) emit(content, reasoningContent string, toolCalls []map[string]interface{}) {
	role := ""
	if !a.emitted {
		role = a.pendingRole
		if role == "" {
			role = "assistant"
		}
	}
	chunk := makeSSEChunk(a.reqID, a.created, a.model, role, content, reasoningContent, toolCalls)
	if a.emitFn != nil && chunk != "" {
		a.emitFn(chunk)
	}
	a.emitted = true
}

// listWriter 非流式聚合缓冲（存 SSE chunk 行）。
type listWriter struct {
	mu     sync.Mutex
	chunks []string
}

func (l *listWriter) writeChunk(sse string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.chunks = append(l.chunks, sse)
}

func (l *listWriter) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.chunks))
	copy(out, l.chunks)
	return out
}

// aggregateNonStream 聚合 chunks 为非流式 chat.completion 响应。
func aggregateNonStream(chunks []string, reqID string, created int64, model string, toolsEnabled bool) map[string]interface{} {
	var fullContent, fullReasoning []string
	toolCalls := newToolCallAccumulator()
	var usageObj *usage
	for _, chunk := range chunks {
		if !strings.HasPrefix(chunk, "data: ") {
			continue
		}
		payload := strings.TrimSpace(chunk[len("data: "):])
		if payload == "[DONE]" {
			break
		}
		d := extractDelta(payload)
		if d.Usage != nil {
			usageObj = d.Usage
		}
		if d.ReasoningContent != "" {
			fullReasoning = append(fullReasoning, d.ReasoningContent)
		}
		if d.Content != "" {
			fullContent = append(fullContent, d.Content)
		}
		if len(d.ToolCalls) > 0 {
			toolCalls.append(d.ToolCalls)
		}
	}
	fullText := strings.Join(fullContent, "")
	var fallbackToolCalls []map[string]interface{}
	if toolCalls.isEmpty() && toolsEnabled {
		if parsed := parseToolCallsText(fullText); parsed != nil {
			fallbackToolCalls = toolsCallsToMaps(parsed)
		}
	}
	msg := map[string]interface{}{"role": "assistant"}
	if fallbackToolCalls != nil {
		msg["content"] = nil
		msg["tool_calls"] = fallbackToolCalls
	} else if fullText == "" && !toolCalls.isEmpty() {
		msg["content"] = nil
	} else {
		msg["content"] = fullText
	}
	if len(fullReasoning) > 0 {
		msg["reasoning_content"] = strings.Join(fullReasoning, "")
	}
	if !toolCalls.isEmpty() {
		msg["tool_calls"] = toolCalls.snapshot()
	}
	finishReason := "stop"
	if !toolCalls.isEmpty() || fallbackToolCalls != nil {
		finishReason = "tool_calls"
	}
	resp := map[string]interface{}{
		"id":      reqID,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]interface{}{
			{"index": 0, "message": msg, "finish_reason": finishReason},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}
	if usageObj != nil {
		resp["usage"] = usageObj
	}
	return resp
}
