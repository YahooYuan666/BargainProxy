// Command protoprobe runs the protocol conformance matrix against a running
// BargainProxy gateway: every usable OpenCode free model, through every
// downstream protocol (chat / responses / anthropic), across scenarios that
// get progressively more complex.
//
// Why: simple one-turn calls pass long before real traffic does. The expensive
// bugs live in the shape conversions — tool declarations reaching the wire,
// tool_use/tool_result round-trips, streaming framing. This tool makes those
// visible in one table.
//
// Target: this is a local diagnostic CLI. -gateway must be http/https and is
// expected to be the operator's own gateway (loopback by default); it never
// fetches third-party URLs itself — all upstream traffic goes through the
// gateway under test.
//
// Usage:
//
//	go run ./scripts/protoprobe                      # smoke over all usable models × 3 protocols
//	go run ./scripts/protoprobe -scenarios smoke,system,tools
//	go run ./scripts/protoprobe -models space-bunny-free -scenarios smoke,system,tools,stream -v
//	go run ./scripts/protoprobe -protos anthropic -models all -scenarios tools
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	maxTokens = 1024
	toolName  = "get_weather"
	toolDesc  = "Get the current weather for a city"
)

var toolSchema = map[string]any{
	"type":       "object",
	"properties": map[string]any{"city": map[string]any{"type": "string"}},
	"required":   []string{"city"},
}

type ctxKey struct{}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "protoprobe: "+format+"\n", args...)
	os.Exit(2)
}

func main() {
	gateway := flag.String("gateway", "http://127.0.0.1:13140", "gateway base URL (http/https)")
	modelsArg := flag.String("models", "all", "comma-separated model ids, or all")
	protosArg := flag.String("protos", "chat,responses,anthropic", "protocols: chat,responses,anthropic")
	scenArg := flag.String("scenarios", "smoke", "scenarios: smoke,system,tools,stream,forced,manytools,mismatch")
	timeout := flag.Duration("timeout", 120*time.Second, "per-request timeout")
	retries := flag.Int("retries", 1, "retries for transient 429/503 upstream capacity errors")
	verbose := flag.Bool("v", false, "print each request's outcome and raw body on failure")
	flag.Parse()

	base := strings.TrimRight(*gateway, "/")
	if err := validateGateway(base); err != nil {
		fatalf("%v", err)
	}
	retryN = *retries
	protos, err := parseProtos(*protosArg)
	if err != nil {
		fatalf("%v", err)
	}
	scenarios, err := parseScenarios(*scenArg)
	if err != nil {
		fatalf("%v", err)
	}
	models, err := pickModels(base, *modelsArg)
	if err != nil {
		fatalf("%v", err)
	}
	if len(models) == 0 {
		fatalf("no usable models reported by %s/panel/api/opencodezen", base)
	}
	fmt.Printf("gateway=%s models=%d protos=%d scenarios=%v\n\n", base, len(models), len(protos), scenarios)

	type row struct {
		model, proto, scenario string
		ok                     bool
		ms                     int64
		detail                 string
	}
	var rows []row
	for _, m := range models {
		for _, p := range protos {
			for _, sc := range scenarios {
				start := time.Now()
				err := runScenario(base, m, p, sc, *timeout)
				ms := time.Since(start).Milliseconds()
				r := row{model: m, proto: string(p), scenario: sc, ok: err == nil, ms: ms}
				if err != nil {
					r.detail = err.Error()
				}
				rows = append(rows, r)
				mark := "PASS"
				if !r.ok {
					mark = "FAIL"
				}
				if *verbose || !r.ok {
					fmt.Printf("%-4s %-34s %-9s %-7s %5dms  %s\n", mark, m, p, sc, ms, truncate(r.detail, 220))
				}
			}
		}
	}

	// Summary by proto and scenario.
	fmt.Println()
	fail := 0
	byProto := map[string][2]int{}
	byScen := map[string][2]int{}
	for _, r := range rows {
		p := byProto[r.proto]
		s := byScen[r.scenario]
		if r.ok {
			p[0]++
			s[0]++
		} else {
			p[1]++
			s[1]++
			fail++
		}
		byProto[r.proto] = p
		byScen[r.scenario] = s
	}
	for _, p := range []string{"chat", "responses", "anthropic"} {
		if v, ok := byProto[p]; ok {
			fmt.Printf("  %-9s %d ok / %d fail\n", p, v[0], v[1])
		}
	}
	for _, s := range scenarios {
		if v, ok := byScen[s]; ok {
			fmt.Printf("  %-9s %d ok / %d fail\n", s, v[0], v[1])
		}
	}
	fmt.Printf("\n%d cases, %d failed\n", len(rows), fail)
	if fail > 0 {
		os.Exit(1)
	}
}

// validateGateway accepts only http/https with a plain host (the target is the
// operator's local gateway; this tool never touches third-party URLs itself).
func validateGateway(base string) error {
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("bad -gateway: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("-gateway must be http/https, got %q", u.Scheme)
	}
	if u.Host == "" || u.User != nil {
		return fmt.Errorf("-gateway must carry a plain host, got %q", base)
	}
	return nil
}

type proto string

const (
	protoChat      proto = "chat"
	protoResponses proto = "responses"
	protoAnthropic proto = "anthropic"
)

func parseProtos(s string) ([]proto, error) {
	var out []proto
	for _, part := range strings.Split(s, ",") {
		switch p := proto(strings.TrimSpace(part)); p {
		case protoChat, protoResponses, protoAnthropic:
			out = append(out, p)
		case "":
		default:
			return nil, fmt.Errorf("unknown protocol %q", p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-protos is empty")
	}
	return out, nil
}

func parseScenarios(s string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		switch p {
		case "smoke", "system", "tools", "stream", "forced", "manytools", "mismatch":
			out = append(out, p)
		case "":
		default:
			return nil, fmt.Errorf("unknown scenario %q", p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-scenarios is empty")
	}
	return out, nil
}

// pickModels resolves "all" from the gateway's own usable set (candidates
// probed OK — the same rule the panel and the route table use).
func pickModels(base, arg string) ([]string, error) {
	if arg != "all" {
		var out []string
		for _, m := range strings.Split(arg, ",") {
			if m = strings.TrimSpace(m); m != "" {
				out = append(out, m)
			}
		}
		sort.Strings(out)
		return out, nil
	}
	resp, err := http.Get(base + "/panel/api/opencodezen")
	if err != nil {
		return nil, fmt.Errorf("read usable models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("read usable models: HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Snapshot struct {
			Models []struct {
				ID          string `json:"id"`
				Candidate   bool   `json:"candidate"`
				ProbeStatus string `json:"probe_status"`
			} `json:"models"`
		} `json:"snapshot"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range doc.Snapshot.Models {
		if m.Candidate && m.ProbeStatus == "ok" {
			out = append(out, m.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// --- transport -------------------------------------------------------------

// retryN is the number of extra attempts for transient upstream capacity
// errors (429/503). Set from -retries.
var retryN = 1

func postJSON(base, path string, body any, headers map[string]string, timeout time.Duration) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	status, out, err := postOnce(base, path, raw, headers, timeout)
	// The free lane hands out capacity blips (429/503 "Endpoint is
	// unavailable") that have nothing to do with protocol translation; retry
	// them so the matrix reports translation bugs, not weather.
	for attempt := 0; attempt < retryN && err == nil && (status == 429 || status == 503); attempt++ {
		time.Sleep(3 * time.Second)
		status, out, err = postOnce(base, path, raw, headers, timeout)
	}
	return status, out, err
}

func postOnce(base, path string, raw []byte, headers map[string]string, timeout time.Duration) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, out, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// --- scenario drivers ------------------------------------------------------

type toolCall struct {
	ID        string
	Name      string
	Arguments string
}

type callOut struct {
	text     string
	thinking string
	calls    []toolCall
	stop     string
}

func runScenario(base, model string, p proto, scenario string, timeout time.Duration) error {
	switch scenario {
	case "smoke":
		out, err := call(base, model, p, turnSimple(), nil, false, timeout)
		if err != nil {
			return err
		}
		if strings.TrimSpace(out.text) == "" {
			return fmt.Errorf("empty text (stop=%q calls=%d)", out.stop, len(out.calls))
		}
		return nil
	case "system":
		return runSystem(base, model, p, timeout)
	case "tools":
		return runTools(base, model, p, timeout)
	case "stream":
		out, err := call(base, model, p, turnSimple(), nil, true, timeout)
		if err != nil {
			return err
		}
		if strings.TrimSpace(out.text) == "" {
			return fmt.Errorf("stream produced no text (stop=%q)", out.stop)
		}
		return nil
	case "manytools":
		// Real clients (ZCode + a few MCP servers) send well over the upstream's
		// 128-tool cap. The gateway must keep such a turn alive (capped) rather
		// than fail it, and the tool the conversation needs should still work.
		// get_weather is declared first so the deterministic cap keeps it.
		c := conversation{
			items: []convItem{{role: "user", text: "Call get_weather for Berlin. Do not answer without calling it."}},
		}
		out, err := call(base, model, p, c, manyToolDecls(p, 140), false, 240*time.Second)
		if err != nil {
			return err
		}
		for _, tc := range out.calls {
			if tc.Name == toolName {
				return nil
			}
		}
		return fmt.Errorf("no %s call with 140 declared tools (calls=%v text=%q)", toolName, callNames(out.calls), truncate(out.text, 60))
	case "mismatch":
		// What an interrupted turn or a mid-session model switch leaves behind:
		// an assistant tool call with no result plus a stray result for an id
		// nobody called. Strict DeepSeek-family upstreams answer 11148 and the
		// conversation cannot continue; the gateway must repair the pairing.
		c := conversation{items: []convItem{
			{role: "user", text: "Check the weather in Paris with get_weather."},
			{role: "assistant", callID: "call_orphan", callName: toolName, callArgs: `{"city":"Paris"}`},
			{role: "user", text: "Never mind. Reply with exactly: OK"},
			{role: "tool", callID: "call_ghost", text: "ghost result"},
		}}
		out, err := call(base, model, p, c, toolDecls(p), false, timeout)
		if err != nil {
			return err
		}
		if strings.TrimSpace(out.text) == "" {
			return fmt.Errorf("empty answer after pairing repair")
		}
		return nil
	case "forced":
		// Force a tool call through each protocol's own tool_choice dialect
		// (Anthropic "any", OpenAI "required"): this is what exercises the
		// choice mapping — forwarding the raw dialect upstream is a 400.
		c := conversation{
			items:      []convItem{{role: "user", text: "Call get_weather for Paris."}},
			toolChoice: forceChoice(p),
		}
		out, err := call(base, model, p, c, toolDecls(p), false, timeout)
		if err != nil {
			return err
		}
		for _, tc := range out.calls {
			if tc.Name == toolName {
				return nil
			}
		}
		return fmt.Errorf("no %s call under forced tool_choice (calls=%v text=%q)", toolName, callNames(out.calls), truncate(out.text, 60))
	}
	return fmt.Errorf("unknown scenario %q", scenario)
}

// conversation is the protocol-neutral turn list.
type conversation struct {
	system string
	// toolChoice is the protocol's own spelling of "you must call a tool".
	toolChoice any
	// messages in our own neutral form: role + text, plus optional tool call /
	// tool result links.
	items []convItem
}

// forceChoice returns each protocol's dialect for "a tool call is required".
func forceChoice(p proto) any {
	switch p {
	case protoAnthropic:
		return map[string]any{"type": "any"}
	case protoResponses:
		return "required"
	default:
		return "required"
	}
}

type convItem struct {
	role     string // user | assistant | tool
	text     string
	callID   string
	callName string
	callArgs string
}

func turnSimple() conversation {
	return conversation{items: []convItem{{role: "user", text: "Reply with exactly: OK"}}}
}

func runSystem(base, model string, p proto, timeout time.Duration) error {
	c := conversation{
		system: "You are a terse assistant. Answer with at most two words.",
		items:  []convItem{{role: "user", text: "What is 2+2?"}},
	}
	out, err := call(base, model, p, c, nil, false, timeout)
	if err != nil {
		return fmt.Errorf("round1: %w", err)
	}
	if strings.TrimSpace(out.text) == "" {
		return fmt.Errorf("round1: empty text")
	}
	// Multi-turn continuation: the assistant answer must be renderable and the
	// whole history accepted on the way back in.
	c.items = append(c.items, convItem{role: "assistant", text: out.text})
	c.items = append(c.items, convItem{role: "user", text: "And 3+3?"})
	out2, err := call(base, model, p, c, nil, false, timeout)
	if err != nil {
		return fmt.Errorf("round2: %w", err)
	}
	if strings.TrimSpace(out2.text) == "" {
		return fmt.Errorf("round2: empty text")
	}
	return nil
}

func runTools(base, model string, p proto, timeout time.Duration) error {
	c := conversation{items: []convItem{{
		role: "user",
		text: "Call the get_weather tool for San Francisco, then report the temperature it returns.",
	}}}
	out, err := call(base, model, p, c, toolDecls(p), false, timeout)
	if err != nil {
		return fmt.Errorf("round1: %w", err)
	}
	var got *toolCall
	for i := range out.calls {
		if out.calls[i].Name == toolName {
			got = &out.calls[i]
			break
		}
	}
	if got == nil {
		return fmt.Errorf("round1: no %s call (text=%q calls=%v)", toolName, truncate(out.text, 60), callNames(out.calls))
	}
	// Round 2: replay the call and answer it. This is the shape that broke in
	// production (tool declarations dropped → upstream 400 invalid_request).
	c.items = append(c.items,
		// Real clients replay the assistant turn as text + tool_use together.
		convItem{role: "assistant", text: "Let me check that.", callID: got.ID, callName: got.Name, callArgs: got.Arguments},
		convItem{role: "tool", callID: got.ID, text: "18C, sunny"},
	)
	out2, err := call(base, model, p, c, toolDecls(p), false, timeout)
	if err != nil {
		return fmt.Errorf("round2: %w", err)
	}
	if strings.TrimSpace(out2.text) == "" && len(out2.calls) == 0 {
		return fmt.Errorf("round2: empty final answer")
	}
	return nil
}

func callNames(cs []toolCall) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

// manyToolDecls builds n declarations: the real target tool first, then
// fillers with trivial schemas.
func manyToolDecls(p proto, n int) []map[string]any {
	out := toolDecls(p)
	for i := 0; len(out) < n; i++ {
		name := fmt.Sprintf("aux_tool_%02d", i)
		params := map[string]any{"type": "object", "properties": map[string]any{}}
		switch p {
		case protoChat:
			out = append(out, map[string]any{"type": "function", "function": map[string]any{
				"name": name, "description": "auxiliary tool", "parameters": params}})
		case protoAnthropic:
			out = append(out, map[string]any{"name": name, "description": "auxiliary tool", "input_schema": params})
		default:
			out = append(out, map[string]any{"type": "function", "name": name,
				"description": "auxiliary tool", "parameters": params})
		}
	}
	return out
}

// toolDecls returns the protocol's tool declaration array.
func toolDecls(p proto) []map[string]any {
	switch p {
	case protoChat:
		return []map[string]any{{"type": "function", "function": map[string]any{
			"name": toolName, "description": toolDesc, "parameters": toolSchema}}}
	case protoAnthropic:
		return []map[string]any{{"name": toolName, "description": toolDesc, "input_schema": toolSchema}}
	case protoResponses:
		return []map[string]any{{"type": "function", "name": toolName, "description": toolDesc, "parameters": toolSchema}}
	}
	return nil
}

// --- per-protocol request/response handling --------------------------------

func call(base, model string, p proto, c conversation, tools []map[string]any, stream bool, timeout time.Duration) (*callOut, error) {
	switch p {
	case protoChat:
		return callChat(base, model, c, tools, stream, timeout)
	case protoAnthropic:
		return callAnthropic(base, model, c, tools, stream, timeout)
	case protoResponses:
		return callResponses(base, model, c, tools, stream, timeout)
	}
	return nil, fmt.Errorf("unknown protocol")
}

func errFromBody(status int, raw []byte) error {
	return fmt.Errorf("HTTP %d: %s", status, truncate(strings.TrimSpace(string(raw)), 300))
}

func callChat(base, model string, c conversation, tools []map[string]any, stream bool, timeout time.Duration) (*callOut, error) {
	var msgs []map[string]any
	if c.system != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": c.system})
	}
	for _, it := range c.items {
		switch it.role {
		case "user":
			msgs = append(msgs, map[string]any{"role": "user", "content": it.text})
		case "assistant":
			if it.callName != "" {
				var content any
				if it.text != "" {
					content = it.text
				}
				msgs = append(msgs, map[string]any{"role": "assistant", "content": content, "tool_calls": []any{
					map[string]any{"id": it.callID, "type": "function", "function": map[string]any{
						"name": it.callName, "arguments": it.callArgs}}}})
			} else {
				msgs = append(msgs, map[string]any{"role": "assistant", "content": it.text})
			}
		case "tool":
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": it.callID, "content": it.text})
		}
	}
	body := map[string]any{"model": model, "messages": msgs, "max_tokens": maxTokens, "stream": stream}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if c.toolChoice != nil {
		body["tool_choice"] = c.toolChoice
	}
	status, raw, err := postJSON(base, "/v1/chat/completions", body, nil, timeout)
	if err != nil {
		return nil, err
	}
	if stream {
		if status != 200 {
			return nil, errFromBody(status, raw)
		}
		return parseChatStream(raw)
	}
	if status != 200 {
		return nil, errFromBody(status, raw)
	}
	var doc struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("bad json: %w (%s)", err, truncate(string(raw), 160))
	}
	if len(doc.Choices) == 0 {
		return nil, fmt.Errorf("no choices: %s", truncate(string(raw), 200))
	}
	out := &callOut{stop: doc.Choices[0].FinishReason}
	if doc.Choices[0].Message.Content != nil {
		out.text = *doc.Choices[0].Message.Content
	}
	for _, tc := range doc.Choices[0].Message.ToolCalls {
		out.calls = append(out.calls, toolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return out, nil
}

func parseChatStream(raw []byte) (*callOut, error) {
	out := &callOut{}
	var text strings.Builder
	done := false
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			done = true
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			text.WriteString(ch.Delta.Content)
		}
	}
	if !done {
		return nil, fmt.Errorf("stream ended without [DONE]")
	}
	out.text = text.String()
	return out, nil
}

func callAnthropic(base, model string, c conversation, tools []map[string]any, stream bool, timeout time.Duration) (*callOut, error) {
	var msgs []map[string]any
	for _, it := range c.items {
		switch it.role {
		case "user":
			msgs = append(msgs, map[string]any{"role": "user", "content": it.text})
		case "assistant":
			if it.callName != "" {
				var args map[string]any
				_ = json.Unmarshal([]byte(it.callArgs), &args)
				if args == nil {
					args = map[string]any{}
				}
				var blocks []any
				if it.text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": it.text})
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": it.callID, "name": it.callName, "input": args})
				msgs = append(msgs, map[string]any{"role": "assistant", "content": blocks})
			} else {
				msgs = append(msgs, map[string]any{"role": "assistant", "content": it.text})
			}
		case "tool":
			msgs = append(msgs, map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": it.callID, "content": it.text}}})
		}
	}
	body := map[string]any{"model": model, "max_tokens": maxTokens, "messages": msgs, "stream": stream}
	if c.system != "" {
		body["system"] = c.system
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if c.toolChoice != nil {
		body["tool_choice"] = c.toolChoice
	}
	status, raw, err := postJSON(base, "/v1/messages", body, map[string]string{"anthropic-version": "2023-06-01"}, timeout)
	if err != nil {
		return nil, err
	}
	if stream {
		if status != 200 {
			return nil, errFromBody(status, raw)
		}
		return parseAnthropicStream(raw)
	}
	if status != 200 {
		return nil, errFromBody(status, raw)
	}
	var doc struct {
		Content []struct {
			Type     string         `json:"type"`
			Text     string         `json:"text"`
			Thinking string         `json:"thinking"`
			ID       string         `json:"id"`
			Name     string         `json:"name"`
			Input    map[string]any `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("bad json: %w (%s)", err, truncate(string(raw), 160))
	}
	out := &callOut{stop: doc.StopReason}
	for _, b := range doc.Content {
		switch b.Type {
		case "text":
			out.text += b.Text
		case "thinking":
			out.thinking += b.Thinking
		case "tool_use":
			args, _ := json.Marshal(b.Input)
			out.calls = append(out.calls, toolCall{ID: b.ID, Name: b.Name, Arguments: string(args)})
		}
	}
	return out, nil
}

func parseAnthropicStream(raw []byte) (*callOut, error) {
	out := &callOut{}
	var text strings.Builder
	sawStop := false
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "content_block_delta":
			text.WriteString(ev.Delta.Text)
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" {
				out.calls = append(out.calls, toolCall{ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name})
			}
		case "message_stop":
			sawStop = true
		}
	}
	if !sawStop {
		return nil, fmt.Errorf("stream ended without message_stop")
	}
	out.text = text.String()
	return out, nil
}

func callResponses(base, model string, c conversation, tools []map[string]any, stream bool, timeout time.Duration) (*callOut, error) {
	var input []map[string]any
	if c.system != "" {
		input = append(input, map[string]any{"type": "message", "role": "system",
			"content": []any{map[string]any{"type": "input_text", "text": c.system}}})
	}
	for _, it := range c.items {
		switch it.role {
		case "user":
			input = append(input, map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": it.text}}})
		case "assistant":
			if it.callName != "" {
				if it.text != "" {
					input = append(input, map[string]any{"type": "message", "role": "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": it.text}}})
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": it.callID,
					"name": it.callName, "arguments": it.callArgs})
			} else {
				input = append(input, map[string]any{"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": it.text}}})
			}
		case "tool":
			input = append(input, map[string]any{"type": "function_call_output",
				"call_id": it.callID, "output": it.text})
		}
	}
	body := map[string]any{"model": model, "input": input, "max_output_tokens": maxTokens, "stream": stream}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if c.toolChoice != nil {
		body["tool_choice"] = c.toolChoice
	}
	status, raw, err := postJSON(base, "/v1/responses", body, nil, timeout)
	if err != nil {
		return nil, err
	}
	if stream {
		if status != 200 {
			return nil, errFromBody(status, raw)
		}
		return parseResponsesStream(raw)
	}
	if status != 200 {
		return nil, errFromBody(status, raw)
	}
	var doc struct {
		Status string `json:"status"`
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("bad json: %w (%s)", err, truncate(string(raw), 160))
	}
	if doc.Error != nil && doc.Error.Message != "" {
		return nil, fmt.Errorf("upstream error: %s", doc.Error.Message)
	}
	out := &callOut{stop: doc.Status}
	for _, item := range doc.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				out.text += part.Text
			}
		case "function_call":
			out.calls = append(out.calls, toolCall{ID: item.CallID, Name: item.Name, Arguments: item.Arguments})
		}
	}
	return out, nil
}

func parseResponsesStream(raw []byte) (*callOut, error) {
	out := &callOut{}
	var text strings.Builder
	sawDone := false
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev struct {
			Type     string `json:"type"`
			Delta    string `json:"delta"`
			Response *struct {
				Status string `json:"status"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "response.output_text.delta":
			text.WriteString(ev.Delta)
		case "response.completed", "response.incomplete":
			sawDone = true
			if ev.Response != nil {
				out.stop = ev.Response.Status
			}
		}
	}
	if !sawDone {
		return nil, fmt.Errorf("stream ended without response.completed")
	}
	out.text = text.String()
	return out, nil
}
