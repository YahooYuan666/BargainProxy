package qoder

// chat：OpenAI 请求 → Qoder 网关协议转换 + SSE 流回放/聚合（移植
// Qoder-2API-Go bridge/transform 包）。

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ---- 网关请求体 ----

type chatContextText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type extraModelConfig struct {
	IsReasoning bool   `json:"is_reasoning"`
	Key         string `json:"key"`
	IsVL        bool   `json:"is_vl,omitempty"`
}

type originalContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type chatContext struct {
	Extra chatContextExtra `json:"extra"`
	Text  chatContextText  `json:"text"`
}

type chatContextExtra struct {
	ModelConfig     extraModelConfig `json:"modelConfig"`
	OriginalContent originalContent  `json:"originalContent"`
}

type modelConfig struct {
	Key         string `json:"key"`
	IsVL        bool   `json:"is_vl"`
	IsReasoning bool   `json:"is_reasoning"`
	Source      string `json:"source"`
	// MaxInputTokens (BP-163): the official client declares the model's input
	// ceiling here; omitted means the server's ~200K default, which walls off
	// models opened to 1M.
	MaxInputTokens int `json:"max_input_tokens,omitempty"`
}

type business struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BeginAt any    `json:"begin_at"`
	// Stage is the official client's task lifecycle: "start" on the first
	// call of a task, "processing" afterwards (qoder-agent-sdk
	// AgentLifecycle). Omitted (empty) keeps the previous wire shape, so
	// tests pinning the old body still pass until the aggregation lands.
	Stage string `json:"stage,omitempty"`
}

// chatRequestBody 网关 chat 请求体（字段顺序对齐官方客户端模板）。
type chatRequestBody struct {
	RequestID         string          `json:"request_id"`
	RequestSetID      string          `json:"request_set_id"`
	ChatRecordID      string          `json:"chat_record_id"`
	Stream            bool            `json:"stream"`
	ChatTask          string          `json:"chat_task"`
	ChatContext       chatContext     `json:"chat_context"`
	SessionID         string          `json:"session_id"`
	Source            int             `json:"source"`
	Version           string          `json:"version"`
	AliyunUserType    string          `json:"aliyun_user_type"`
	SessionType       string          `json:"session_type"`
	AgentID           string          `json:"agent_id"`
	TaskID            string          `json:"task_id"`
	ModelConfig       modelConfig     `json:"model_config"`
	Messages          []qoderMessage  `json:"messages"`
	Business          business        `json:"business"`
	Parameters        json.RawMessage `json:"parameters"`
	Tools             json.RawMessage `json:"tools,omitempty"`
	ParallelToolCalls json.RawMessage `json:"parallel_tool_calls,omitempty"`
}

func newRequestBody() *chatRequestBody {
	return &chatRequestBody{
		Stream:   true,
		ChatTask: "FREE_INPUT",
		ChatContext: chatContext{
			Extra: chatContextExtra{
				ModelConfig:     extraModelConfig{IsReasoning: false, Key: "lite"},
				OriginalContent: originalContent{Type: "text", Text: ""},
			},
			Text: chatContextText{Type: "text", Text: ""},
		},
		Source:         1,
		Version:        "3",
		AliyunUserType: "personal_standard",
		SessionType:    "qodercli",
		AgentID:        "agent_common",
		TaskID:         "common",
		ModelConfig:    modelConfig{Key: "lite", IsVL: false, IsReasoning: false, Source: "system"},
		Messages:       []qoderMessage{},
		Business:       business{Name: "hi"},
	}
}

// ---- Qoder 消息 ----

type imageURLPart struct {
	URL string `json:"url"`
}

type contentPart struct {
	Type     string        `json:"type"`
	ImageURL *imageURLPart `json:"image_url,omitempty"`
	Text     string        `json:"text,omitempty"`
	// CacheControl is the official client's per-block cache mark
	// ({"type":"ephemeral"}); set on the last block of the last user message
	// by markQoderCache (BP-111).
	CacheControl map[string]string `json:"cache_control,omitempty"`
}

type normalizedToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type responseMeta struct {
	ID    string            `json:"id"`
	Usage responseMetaUsage `json:"usage"`
}

type responseMetaUsage struct {
	PromptTokens            int `json:"prompt_tokens"`
	CompletionTokens        int `json:"completion_tokens"`
	TotalTokens             int `json:"total_tokens"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func blankResponseMeta() responseMeta { return responseMeta{} }

// qoderMessage 网关消息（字段顺序对齐 Python 参考实现）。
type qoderMessage struct {
	Role                string       `json:"role"`
	Content             string       `json:"content"`
	ResponseMeta        responseMeta `json:"response_meta"`
	ReasoningContentSig string       `json:"reasoning_content_signature"`
	// ReasoningContent (BP-172) is an earlier assistant turn's thinking, as
	// Qoder's own converter sets it. Empty/blank stays off the wire.
	ReasoningContent string               `json:"reasoning_content,omitempty"`
	Contents         []contentPart        `json:"contents,omitempty"`
	ToolCalls        []normalizedToolCall `json:"tool_calls,omitempty"`
	Name             string               `json:"name,omitempty"`
	ToolCallID       string               `json:"tool_call_id,omitempty"`
}

func buildUserMessage(text string, images []string) qoderMessage {
	parts := []contentPart{}
	for _, u := range images {
		parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURLPart{URL: u}})
	}
	if strings.TrimSpace(text) != "" {
		parts = append(parts, contentPart{Type: "text", Text: text})
	}
	if len(parts) == 0 {
		parts = append(parts, contentPart{Type: "text", Text: ""})
	}
	return qoderMessage{Role: "user", ResponseMeta: blankResponseMeta(), Contents: parts}
}

func buildStructuredMessage(role, text string) qoderMessage {
	return qoderMessage{Role: role, Content: text, ResponseMeta: blankResponseMeta()}
}

// applyReasoning copies an assistant message's historical thinking onto the
// qoder message (BP-172). Only non-blank text rides the wire, and the field
// is set ONLY on assistant turns — the official client sends none back for
// anything else.
func applyReasoning(message map[string]interface{}, msg *qoderMessage) {
	if msg == nil || msg.Role != "assistant" {
		return
	}
	rc, _ := message["reasoning_content"].(string)
	if strings.TrimSpace(rc) == "" {
		rc, _ = message["reasoning"].(string)
	}
	if strings.TrimSpace(rc) == "" {
		return
	}
	msg.ReasoningContent = rc
}

// ---- OpenAI 消息规范化 ----

func normalizeContent(content interface{}) string {
	if content == nil {
		return ""
	}
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		parts := []string{}
		for _, item := range c {
			if p := normalizeContentPart(item); p != "" {
				parts = append(parts, p)
			}
		}
		return strings.Join(parts, "\n\n")
	case map[string]interface{}:
		return normalizeContentPart(c)
	default:
		return fmt.Sprintf("%v", content)
	}
}

func normalizeContentPart(item interface{}) string {
	if item == nil {
		return ""
	}
	switch v := item.(type) {
	case string:
		return v
	case map[string]interface{}:
		if text, ok := v["text"].(string); ok {
			return text
		}
		t, _ := v["type"].(string)
		if t == "image_url" || t == "input_image" {
			iu, ok := v["image_url"].(map[string]interface{})
			if ok {
				if url, _ := iu["url"].(string); url != "" {
					return "[image] " + url
				}
			}
			if url, _ := v["url"].(string); url != "" {
				return "[image] " + url
			}
		}
		raw, _ := json.Marshal(v)
		return string(raw)
	default:
		return fmt.Sprintf("%v", item)
	}
}

func extractImageDataURL(part map[string]interface{}) string {
	t, _ := part["type"].(string)
	if t != "image_url" && t != "input_image" {
		return ""
	}
	switch v := part["image_url"].(type) {
	case map[string]interface{}:
		if url, _ := v["url"].(string); url != "" {
			return url
		}
	case string:
		return v
	}
	url, _ := part["url"].(string)
	return url
}

func extractMessageImages(message map[string]interface{}) []string {
	content, ok := message["content"].([]interface{})
	if !ok {
		if content, ok = message["contents"].([]interface{}); !ok {
			return nil
		}
	}
	urls := []string{}
	for _, item := range content {
		if part, ok := item.(map[string]interface{}); ok {
			if url := extractImageDataURL(part); url != "" {
				urls = append(urls, url)
			}
		}
	}
	return urls
}

func normalizeMessageText(message map[string]interface{}) string {
	text := normalizeContent(message["content"])
	if strings.TrimSpace(text) == "" {
		text = normalizeContent(message["contents"])
	}
	return text
}

func normalizeToolArguments(arguments interface{}) string {
	if arguments == nil {
		return ""
	}
	if s, ok := arguments.(string); ok {
		return s
	}
	raw, _ := json.Marshal(arguments)
	return string(raw)
}

func normalizeToolCalls(rawToolCalls []interface{}) []normalizedToolCall {
	normalized := []normalizedToolCall{}
	for _, rtc := range rawToolCalls {
		m, ok := rtc.(map[string]interface{})
		if !ok {
			continue
		}
		funcMap, _ := m["function"].(map[string]interface{})
		name, _ := funcMap["name"].(string)
		arguments := normalizeToolArguments(funcMap["arguments"])
		if name == "" && arguments == "" {
			continue
		}
		var tc normalizedToolCall
		tc.ID, _ = m["id"].(string)
		tc.Type, _ = m["type"].(string)
		if tc.Type == "" {
			tc.Type = "function"
		}
		tc.Function.Name = name
		tc.Function.Arguments = arguments
		normalized = append(normalized, tc)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

// extractAnyToolCalls 从消息取 tool_calls（tools 未启用时返回 nil）。
func extractAnyToolCalls(message map[string]interface{}, text string, toolsEnabled bool) []normalizedToolCall {
	if !toolsEnabled {
		return nil
	}
	if tc, ok := message["tool_calls"].([]interface{}); ok && len(tc) > 0 {
		return normalizeToolCalls(tc)
	}
	return parseToolCallsText(text)
}

// parseToolCallsText 解析 "Tool calls: [...]" 文本格式（部分模型回退形态），
// 以及文本形态的工具调用块（BP-98 Qoder-8：<tool_call>{json}</tool_call>
// 与 XML function/parameter 形——模型偶发把它们写成正文，块就是本轮的
// tool calls，不是散文；magpie-community/plugins packages/qoder 0.2.30
// Splitter/parseCall）。
func parseToolCallsText(text string) []normalizedToolCall {
	if text == "" {
		return nil
	}
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "Tool calls:") {
		if calls, _ := parseToolCallBlocks(text); len(calls) > 0 {
			return calls
		}
		return nil
	}
	payload := strings.TrimSpace(trimmed[len("Tool calls:"):])
	if strings.HasPrefix(payload, "```") && strings.HasSuffix(payload, "```") {
		if newline := strings.Index(payload, "\n"); newline >= 0 {
			payload = strings.TrimSpace(payload[newline+1 : len(payload)-3])
		}
	}
	if !strings.HasPrefix(payload, "[") {
		return nil
	}
	var parsed []interface{}
	if json.Unmarshal([]byte(payload), &parsed) != nil {
		return nil
	}
	return normalizeToolCalls(parsed)
}

// text-tool-call block markers and shapes (magpie 0.2.30 CALL_OPEN/FUNC/
// PARAM).
const tcCallOpen = "<tool_call>"
const tcCallClose = "</tool_call>"

var tcFuncRe = regexp.MustCompile(`<function=([^>]+)>([\s\S]*?)</function>`)
var tcParamRe = regexp.MustCompile(`<parameter=([^>]+)>([\s\S]*?)</parameter>`)

// parseToolCallBlocks lifts every <tool_call>…</tool_call> block out of the
// text and parses it as a call: JSON {name,arguments} first, then the XML
// function/parameter form (magpie 0.2.30 parseCall). Returns the calls and
// the remaining text. Guards, both from magpie: a block inside a code fence
// or an open inline code span is prose ABOUT a call and stays text; a
// truncated block (stream ended inside) stays text rather than failing the
// turn — our accumulator re-feeds the whole buffered text, so nothing is
// lost by keeping it.
func parseToolCallBlocks(text string) ([]normalizedToolCall, string) {
	var calls []normalizedToolCall
	var out strings.Builder
	rest := text
	for {
		i := strings.Index(rest, tcCallOpen)
		if i < 0 {
			out.WriteString(rest)
			break
		}
		j := strings.Index(rest[i:], tcCallClose)
		if j < 0 {
			// Stream ended inside a block: keep it as text.
			out.WriteString(rest)
			break
		}
		body := rest[i+len(tcCallOpen) : i+j]
		blockEnd := i + j + len(tcCallClose) // j is relative to rest[i:]
		after := rest[blockEnd:]
		call, ok := parseTextToolCall(body)
		if !ok || inCodeFence(rest[:i]) {
			// Unreadable or quoted: prose, keep verbatim.
			out.WriteString(rest[:blockEnd])
			rest = after
			continue
		}
		calls = append(calls, call)
		out.WriteString(rest[:i])
		rest = after
	}
	return calls, out.String()
}

// parseTextToolCall reads one block body: JSON {name, arguments-object}, or
// the XML <function=name><parameter=k>v</parameter>… form.
func parseTextToolCall(body string) (normalizedToolCall, bool) {
	var call normalizedToolCall
	call.Type = "function"
	trimmed := strings.TrimSpace(body)
	var name, args string
	var f struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal([]byte(trimmed), &f) == nil && strings.TrimSpace(f.Name) != "" && len(f.Arguments) > 0 && json.Valid(f.Arguments) {
		name = strings.TrimSpace(f.Name)
		args = string(f.Arguments)
	} else if m := tcFuncRe.FindStringSubmatch(trimmed); m != nil && strings.TrimSpace(m[1]) != "" {
		name = strings.TrimSpace(m[1])
		argsMap := map[string]interface{}{}
		for _, pm := range tcParamRe.FindAllStringSubmatch(m[2], -1) {
			key := strings.TrimSpace(pm[1])
			if key == "" {
				continue
			}
			val := strings.TrimSpace(pm[2])
			var jv interface{}
			if json.Unmarshal([]byte(val), &jv) == nil {
				argsMap[key] = jv
			} else {
				argsMap[key] = val
			}
		}
		raw, err := json.Marshal(argsMap)
		if err != nil {
			return call, false
		}
		args = string(raw)
	} else {
		return call, false
	}
	call.Function.Name = name
	call.Function.Arguments = args
	call.ID = "fc_" + newUUID()
	return call, true
}

// inCodeFence says the text before index i ends inside a code fence or an
// open inline code span — a <tool_call> there is prose about one (magpie
// 0.2.30 quoted()).
func inCodeFence(text string) bool {
	if strings.Count(text, "```")%2 == 1 {
		return true
	}
	line := text[strings.LastIndexByte(text, '\n')+1:]
	line = strings.ReplaceAll(line, "```", "")
	return strings.Count(line, "`")%2 == 1
}

func toolsCallsToMaps(calls []normalizedToolCall) []map[string]interface{} {
	out := []map[string]interface{}{}
	for _, tc := range calls {
		out = append(out, map[string]interface{}{
			"id":   tc.ID,
			"type": tc.Type,
			"function": map[string]interface{}{
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
			},
		})
	}
	return out
}

func renderToolCalls(toolCalls []normalizedToolCall) string {
	return "Tool calls:\n" + mustJSON(toolsCallsToMaps(toolCalls))
}

func renderToolResult(name, toolCallID, text string) string {
	parts := []string{"Tool result"}
	if name != "" {
		parts = append(parts, fmt.Sprintf(" (%s)", name))
	}
	if toolCallID != "" {
		parts = append(parts, fmt.Sprintf(" [%s]", toolCallID))
	}
	if strings.TrimSpace(text) != "" {
		parts = append(parts, fmt.Sprintf(":\n%s", text))
	}
	return strings.Join(parts, "")
}

func summarizeUnresolvedToolCalls(toolCalls []normalizedToolCall) string {
	sb := []string{"Previously planned but unexecuted tool calls"}
	limit := len(toolCalls)
	if limit > 6 {
		limit = 6
	}
	names := []string{}
	for i := 0; i < limit; i++ {
		name := toolCalls[i].Function.Name
		if name == "" {
			name = "unknown"
		}
		names = append(names, name)
	}
	if len(names) > 0 {
		sb = append(sb, ": ")
		sb = append(sb, strings.Join(names, ", "))
	}
	if len(toolCalls) > limit {
		sb = append(sb, fmt.Sprintf(" and %d more", len(toolCalls)-limit))
	}
	sb = append(sb, ".")
	return strings.Join(sb, "")
}

func joinSections(first, second string) string {
	if strings.TrimSpace(first) == "" {
		return second
	}
	if strings.TrimSpace(second) == "" {
		return first
	}
	return first + "\n\n" + second
}

// hasResolvedToolResponse 判断 assistant 消息后是否紧跟 tool 结果。
func hasResolvedToolResponse(messages []map[string]interface{}, assistantIndex int) bool {
	message := messages[assistantIndex]
	role, _ := message["role"].(string)
	if role != "assistant" {
		return false
	}
	hasToolCalls := false
	if tc, ok := message["tool_calls"].([]interface{}); ok && len(tc) > 0 {
		hasToolCalls = true
	}
	if !hasToolCalls && parseToolCallsText(normalizeMessageText(message)) != nil {
		hasToolCalls = true
	}
	if !hasToolCalls {
		return false
	}
	for i := assistantIndex + 1; i < len(messages); i++ {
		nextRole, _ := messages[i]["role"].(string)
		if nextRole == "tool" {
			return true
		}
		if nextRole == "assistant" || nextRole == "user" || nextRole == "system" {
			return false
		}
	}
	return false
}

// convertIncomingMessage OpenAI 消息 → Qoder 消息。
func convertIncomingMessage(message map[string]interface{}, toolsEnabled, allowStructuredToolCalls bool) *qoderMessage {
	role, _ := message["role"].(string)
	if role == "" {
		role = "user"
	}
	if role == "developer" { // 网关不接受 developer 角色
		role = "system"
	}
	text := normalizeMessageText(message)
	anyToolCalls := extractAnyToolCalls(message, text, toolsEnabled)
	var structuredToolCalls []normalizedToolCall
	if toolsEnabled && allowStructuredToolCalls {
		structuredToolCalls = extractAnyToolCalls(message, text, true)
	}

	if role == "assistant" && structuredToolCalls != nil {
		content := text
		if parseToolCallsText(content) != nil {
			content = ""
		}
		msg := buildStructuredMessage("assistant", content)
		msg.ToolCalls = structuredToolCalls
		applyReasoning(message, &msg)
		return &msg
	}
	if role == "assistant" && anyToolCalls != nil && !allowStructuredToolCalls {
		msg := buildStructuredMessage("assistant", summarizeUnresolvedToolCalls(anyToolCalls))
		applyReasoning(message, &msg)
		return &msg
	}

	if tc, ok := message["tool_calls"].([]interface{}); !toolsEnabled && ok && len(tc) > 0 {
		if ntc := normalizeToolCalls(tc); ntc != nil {
			text = joinSections(text, renderToolCalls(ntc))
		}
	}

	if role == "tool" {
		if toolsEnabled {
			name, _ := message["name"].(string)
			toolCallID, _ := message["tool_call_id"].(string)
			msg := buildStructuredMessage("tool", text)
			msg.Name = name
			msg.ToolCallID = toolCallID
			return &msg
		}
		role = "user"
		name, _ := message["name"].(string)
		toolCallID, _ := message["tool_call_id"].(string)
		text = renderToolResult(name, toolCallID, text)
	}

	var images []string
	if role == "user" {
		images = extractMessageImages(message)
	}
	if len(images) > 0 {
		segments := strings.Split(text, "\n\n")
		filtered := []string{}
		for _, seg := range segments {
			s := strings.TrimSpace(seg)
			if s != "" && !strings.HasPrefix(s, "[image] ") {
				filtered = append(filtered, s)
			}
		}
		text = strings.Join(filtered, "\n\n")
	}
	if strings.TrimSpace(text) == "" && len(images) == 0 {
		return nil
	}
	if role == "user" {
		msg := buildUserMessage(text, images)
		return &msg
	}
	msg := buildStructuredMessage(role, text)
	applyReasoning(message, &msg)
	return &msg
}

// buildQoderMessages 批量转换。
func buildQoderMessages(incomingMessages []map[string]interface{}, prompt string, toolsEnabled bool) []qoderMessage {
	rebuilt := []qoderMessage{}
	for i, message := range incomingMessages {
		allowStructured := toolsEnabled && hasResolvedToolResponse(incomingMessages, i)
		if converted := convertIncomingMessage(message, toolsEnabled, allowStructured); converted != nil {
			rebuilt = append(rebuilt, *converted)
		}
	}
	if len(rebuilt) == 0 && strings.TrimSpace(prompt) != "" {
		rebuilt = append(rebuilt, buildUserMessage(prompt, nil))
	}
	return rebuilt
}

func extractLatestUserPrompt(messages []map[string]interface{}) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if role, _ := messages[i]["role"].(string); role == "user" {
			if text := normalizeMessageText(messages[i]); strings.TrimSpace(text) != "" {
				return text
			}
		}
	}
	return ""
}

func extractMessages(raw interface{}) []map[string]interface{} {
	list, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	result := make([]map[string]interface{}, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]interface{}); ok {
			result = append(result, m)
		}
	}
	return result
}

func mustJSON(v interface{}) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// ---- 参数（verbatim 透传策略）----

func resolveReasoningEffort(reqBody map[string]interface{}) (string, error) {
	if raw, present := reqBody["reasoning_effort"]; present {
		s, ok := raw.(string)
		if !ok {
			return "", fmt.Errorf("reasoning_effort must be a string")
		}
		return s, nil
	}
	if reasoning, ok := reqBody["reasoning"].(map[string]interface{}); ok {
		if raw, present := reasoning["effort"]; present {
			s, ok := raw.(string)
			if !ok {
				return "", fmt.Errorf("reasoning.effort must be a string")
			}
			return s, nil
		}
	}
	return "", nil
}

// buildParameters 组网关 parameters 对象（顺序对齐官方客户端）。
func buildParameters(reqBody map[string]interface{}, effort string, catalogMaxTokens int) (json.RawMessage, error) {
	obj := newOrderedObject()

	maxRaw, err := maxTokensField(reqBody)
	if err != nil {
		return nil, err
	}
	if maxRaw == nil {
		maxRaw = json.RawMessage(fmt.Sprintf("%d", catalogMaxTokens))
	}
	obj.set("max_tokens", maxRaw)

	if raw, present := reqBody["max_thinking_tokens"]; present {
		switch n := raw.(type) {
		case float64:
			obj.set("max_thinking_tokens", n)
		case json.Number:
			obj.set("max_thinking_tokens", n)
		case json.RawMessage:
			obj.set("max_thinking_tokens", n)
		default:
			return nil, fmt.Errorf("max_thinking_tokens must be a number")
		}
	} else if effort == "none" {
		obj.set("max_thinking_tokens", json.RawMessage("0"))
	}
	if effort != "" {
		obj.set("reasoning_effort", effort)
	}
	if tc, present := reqBody["tool_choice"]; present {
		obj.set("tool_choice", tc)
	}
	for _, key := range []string{"frequency_penalty", "logit_bias", "n", "presence_penalty",
		"response_format", "seed", "stop", "temperature", "top_p"} {
		if raw, present := reqBody[key]; present {
			obj.set(key, raw)
		}
	}
	return obj.marshal()
}

func maxTokensField(reqBody map[string]interface{}) (json.RawMessage, error) {
	var chosen json.RawMessage
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		raw, present := reqBody[key]
		if !present {
			continue
		}
		switch n := raw.(type) {
		case float64:
			b, _ := json.Marshal(n)
			if chosen == nil {
				chosen = b
			}
		case json.Number:
			if chosen == nil {
				chosen = json.RawMessage(n)
			}
		case json.RawMessage:
			if chosen == nil {
				chosen = n
			}
		default:
			return nil, fmt.Errorf("%s must be a number", key)
		}
	}
	return chosen, nil
}

func streamIncludeUsage(reqBody map[string]interface{}) bool {
	so, ok := reqBody["stream_options"].(map[string]interface{})
	if !ok {
		return false
	}
	v, _ := so["include_usage"].(bool)
	return v
}

// orderedObject 保序 JSON 对象（官方客户端参数键序）。
type orderedObject struct {
	keys []string
	vals map[string]interface{}
}

func newOrderedObject() *orderedObject {
	return &orderedObject{vals: map[string]interface{}{}}
}

func (o *orderedObject) set(key string, val interface{}) {
	if _, exists := o.vals[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = val
}

func (o *orderedObject) marshal() (json.RawMessage, error) {
	parts := make([]string, 0, len(o.keys))
	for _, k := range o.keys {
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		vb, err := marshalNoEscape(o.vals[k])
		if err != nil {
			return nil, err
		}
		parts = append(parts, string(kb)+":"+string(vb))
	}
	return json.RawMessage("{" + strings.Join(parts, ",") + "}"), nil
}

// applyToolConfig tools 透传（返回 toolsEnabled）。
func applyToolConfig(body *chatRequestBody, reqBody map[string]interface{}) bool {
	toolsEnabled := false
	if tools, ok := reqBody["tools"].([]interface{}); ok && len(tools) > 0 {
		toolsEnabled = true
		b, _ := marshalNoEscape(tools)
		body.Tools = b
	}
	if ptc, ok := reqBody["parallel_tool_calls"]; ok {
		b, _ := marshalNoEscape(ptc)
		body.ParallelToolCalls = b
	}
	return toolsEnabled
}
