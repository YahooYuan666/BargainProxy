// Command acprobe2 is a direct-to-upstream A/B probe for the AutoClaw CN
// managed gateway. It bypasses the running gateway entirely (its own HTTP
// client, its own header set), so protocol questions can be answered without
// restarting anything.
//
// It reads the account's credential from the local database (decrypting it with
// the same envelope the gateway uses) and reports, per variant, the HTTP status
// and the first bytes of the body. Each variant can be run N times so a
// transient answer is not mistaken for a property of the protocol.
//
// Usage:
//
//	go run ./scripts/acprobe2 -variant baseline -runs 3 -turns 6
package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"bargainproxy/internal/cryptox"
)

const (
	appID   = "100003"
	baseURL = "https://autoglm-acceleration-api.zhipuai.cn"
	chatPth = "/autoclaw-proxy/proxy/autoclaw/chat/completions"
	sysLine = "You are AutoClaw. Answer the user directly and concisely."
)

type creds struct {
	AccessToken string `json:"access_token"`
	UID         string `json:"uid"`
	Region      string `json:"region"`
}

func main() {
	dsn := flag.String("dsn", "postgres://bargainproxy@127.0.0.1:55432/bargainproxy", "database dsn")
	keyFile := flag.String("key", "data/master.key", "master key file")
	secret := flag.String("secret", "", "app secret (default: read from config.json)")
	cfgPath := flag.String("config", "config.json", "config.json for the app secret")
	variant := flag.String("variant", "baseline", "header/body variant name (see variants())")
	listModels := flag.Bool("list", false, "fetch the upstream model list and print every route id")
	model := flag.String("model", "zai_glm-5.3-flash", "route id")
	runs := flag.Int("runs", 2, "how many times to run this variant")
	turns := flag.Int("turns", 5, "how many turns of conversation history to build")
	toolTurns := flag.Bool("tools", true, "declare bash/read tools and include tool rounds in the history")
	promptPrefix := flag.String("prefix", "", "extra system text merged BEFORE the AutoClaw line (probes the prompt gate)")
	prefixFile := flag.String("prefix-file", "", "read the prefix from a file (a 13k prefix exceeds the Windows command line limit)")
	sysMode := flag.String("system", "ours", "system prompt shape: ours (prefix merged) | exact (AutoClaw line only) | user (system moved into the first user turn) | none (no system message)")
	padTokens := flag.Int("pad", 0, "pad the history to roughly this many prompt tokens (chars/2), to reproduce a long ZCode session")
	flag.Parse()

	if *prefixFile != "" {
		b, err := os.ReadFile(*prefixFile)
		if err != nil {
			log.Fatalf("prefix-file: %v", err)
		}
		*promptPrefix = string(b)
	}

	sec := *secret
	if sec == "" {
		sec = readSecret(*cfgPath)
	}
	if sec == "" {
		log.Fatal("no app secret: pass -secret or have channels.autoclaw.app_secret in config.json")
	}

	c, err := loadCreds(*dsn, *keyFile)
	if err != nil {
		log.Fatalf("creds: %v", err)
	}
	tok := strings.TrimPrefix(c.AccessToken, "Bearer ")
	fmt.Printf("account=%s region=%s token=%d chars\n", c.UID, c.Region, len(tok))

	if *listModels {
		listUpstreamModels(sec, tok)
		return
	}

	v, ok := variants()[*variant]
	if !ok {
		log.Fatalf("unknown variant %q; have: %s", *variant, variantNames())
	}

	for i := 0; i < *runs; i++ {
		st, body, elapsed, err := one(context.Background(), v, sec, tok, *model, *turns, *toolTurns, *promptPrefix, *sysMode, *padTokens)
		if err != nil {
			// Elapsed matters: a drop at a near-constant wall time is a
			// server-side cap, not network jitter (2026-10-23: the gateway's
			// autoclaw leg kept dying at ~30.4s).
			fmt.Printf("run %d: TRANSPORT after %s: %v\n", i+1, elapsed.Round(time.Millisecond), err)
			continue
		}
		fmt.Printf("run %d: HTTP %d  %s  %s\n", i+1, st, elapsed.Round(time.Millisecond), firstLine(body, 160))
		if i < *runs-1 {
			time.Sleep(1200 * time.Millisecond)
		}
	}
}

// variant is one header/body shape to try.
type variant struct {
	desc  string
	apply func(h http.Header, body map[string]any, tok, secret, model string, stream bool)
}

// variants maps a name to one shape. The baseline is what our driver sends
// today; each other name changes exactly one thing so a difference is
// attributable.
func variants() map[string]variant {
	return map[string]variant{
		"baseline": {desc: "what our driver sends today", apply: func(h http.Header, b map[string]any, tok, sec, model string, _ bool) {
			base(h, tok, sec, model)
			h.Set("X-Channel", "AutoClaw26")
			h.Set("Origin", "http://localhost:18432")
			h.Set("Referer", "http://localhost:18432/")
			delete(b, "max_completion_tokens")
			delete(b, "user")
		}},
		"channel-zai": {desc: "X-Channel: zai (2.x reference value)", apply: func(h http.Header, b map[string]any, tok, sec, model string, _ bool) {
			base(h, tok, sec, model)
			h.Set("X-Channel", "zai")
			delete(b, "max_completion_tokens")
		}},
		"channel-official": {desc: "X-Channel: official (1.x reference value)", apply: func(h http.Header, b map[string]any, tok, sec, model string, _ bool) {
			base(h, tok, sec, model)
			h.Set("X-Channel", "official")
			delete(b, "max_completion_tokens")
		}},
		"with-session": {desc: "adds X-Session-Id (two repos say its absence is 406)", apply: func(h http.Header, b map[string]any, tok, sec, model string, _ bool) {
			base(h, tok, sec, model)
			h.Set("X-Channel", "zai")
			h.Set("X-Session-Id", uuid())
			delete(b, "max_completion_tokens")
		}},
		"no-origin": {desc: "drops Origin/Referer (2.x chat sends neither)", apply: func(h http.Header, b map[string]any, tok, sec, model string, _ bool) {
			base(h, tok, sec, model)
			h.Set("X-Channel", "zai")
			delete(b, "max_completion_tokens")
		}},
		"with-mct": {desc: "sends max_completion_tokens>=307200 + store:false + effort high", apply: func(h http.Header, b map[string]any, tok, sec, model string, _ bool) {
			base(h, tok, sec, model)
			h.Set("X-Channel", "zai")
			b["max_completion_tokens"] = 307200
			b["store"] = false
			b["reasoning_effort"] = "high"
		}},
		"keep-user": {desc: "keeps the user field our driver deletes", apply: func(h http.Header, b map[string]any, tok, sec, model string, _ bool) {
			base(h, tok, sec, model)
			h.Set("X-Channel", "zai")
			delete(b, "max_completion_tokens")
		}},
		"version-4seg": {desc: "X-Version 2.1.0.1 (four segments)", apply: func(h http.Header, b map[string]any, tok, sec, model string, _ bool) {
			base(h, tok, sec, model)
			h.Set("X-Channel", "zai")
			h.Set("X-Version", "2.1.0.1")
			delete(b, "max_completion_tokens")
		}},
		"sdk-fp": {desc: "OpenAI/JS SDK fingerprint UA + x-stainless-* + x-agent-id", apply: func(h http.Header, b map[string]any, tok, sec, model string, _ bool) {
			base(h, tok, sec, model)
			h.Set("X-Channel", "zai")
			h.Set("User-Agent", "OpenAI/JS 6.26.0")
			h.Set("x-agent-id", "main")
			h.Set("x-stainless-lang", "js")
			h.Set("x-stainless-package-version", "6.26.0")
			h.Set("x-stainless-os", "Windows")
			h.Set("x-stainless-arch", "x64")
			h.Set("x-stainless-runtime", "node")
			h.Set("x-stainless-runtime-version", "v22.21.1")
			delete(b, "max_completion_tokens")
		}},
	}
}

func variantNames() string {
	var names []string
	for k := range variants() {
		names = append(names, k)
	}
	return strings.Join(names, ", ")
}

// base sets the headers every variant shares: the ones no reference project
// disputes.
func base(h http.Header, tok, secret, routeModel string) {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("User-Agent", "AutoClaw/2.1.0 (win-amd64)")
	h.Set("X-Version", "2.1.0")
	h.Set("X-Tm", "win")
	h.Set("X-Product", "autoclaw")
	h.Set("X-Client-Type", "pc")
	h.Set("X-Lang", "zh-CN")
	h.Set("X-Authorization", "Bearer "+tok)
	h.Set("X-Request-Id", uuid())
	h.Set("X-Trace-Id", uuid())
	h.Set("x_trace_id", "autoclaw-desktop")
	h.Set("X-Auth-Appid", appID)
	h.Set("X-Auth-TimeStamp", ts)
	sum := md5.Sum([]byte(appID + "&" + ts + "&" + secret))
	h.Set("X-Auth-Sign", hex.EncodeToString(sum[:]))
	// X-Request-Model selects the route; without it the gateway answers
	// "非法模型" for every id (field check 2026-10-23).
	h.Set("X-Request-Model", routeModel)
	h.Set("X-Agent-Id", "main")
}

// one sends one request and returns the status plus the first bytes of the
// reply (or of the first SSE frame).
func one(ctx context.Context, v variant, secret, tok, model string, turns int, tools bool, promptPrefix, sysMode string, padTokens int) (int, string, time.Duration, error) {
	body := buildBody(model, turns, tools, promptPrefix, sysMode, padTokens)
	h := http.Header{}
	v.apply(h, body, tok, secret, model, true)

	raw, err := json.Marshal(body)
	if err != nil {
		return 0, "", 0, err
	}
	fmt.Printf("  request: %d bytes (~%dk prompt tokens by chars/2)\n", len(raw), len(raw)/2/1000)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+chatPth, bytes.NewReader(raw))
	if err != nil {
		return 0, "", 0, err
	}
	req.Header = h

	start := time.Now()
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return 0, "", time.Since(start), err
	}
	defer resp.Body.Close()
	// Read a bounded slice: a healthy answer is an SSE stream that never ends
	// on its own, so taking the whole body would hang.
	buf := make([]byte, 2000)
	n, _ := io.ReadAtLeast(resp.Body, buf, 1)
	if n == 0 {
		n, _ = resp.Body.Read(buf)
	}
	return resp.StatusCode, string(buf[:n]), time.Since(start), nil
}

// buildBody makes a realistic multi-turn conversation: system + alternating
// user/assistant turns, optionally with tool call rounds, ending in a short
// user question. This is the shape the "long conversation" A/B needs.
func buildBody(model string, turns int, tools bool, promptPrefix, sysMode string, padTokens int) map[string]any {
	system := sysLine
	if promptPrefix != "" {
		system = promptPrefix + "\n\n" + sysLine
	}
	var msgs []any
	switch sysMode {
	case "none":
		// no system message at all
	case "only-refused":
		// the refused line alone, without the AutoClaw line
		msgs = append(msgs, map[string]any{"role": "system", "content": promptPrefix})
	case "as-user":
		// BP-99's retry shape: a neutral system line, the caller's text as the
		// first thing the user says.
		msgs = append(msgs, map[string]any{"role": "system", "content": "You are a helpful assistant. Please answer the user's question."})
		if promptPrefix != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": "<instructions>\n" + promptPrefix + "\n</instructions>"})
			msgs = append(msgs, map[string]any{"role": "assistant", "content": "好的，我已了解你的指令。"})
		}
	case "wrapper-only":
		// The refused text inside <instructions> tags, nothing else different.
		// Isolates the wrapper from the text.
		msgs = append(msgs, map[string]any{"role": "system", "content": sysLine})
		if promptPrefix != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": "<instructions>\n" + promptPrefix + "\n</instructions>"})
			msgs = append(msgs, map[string]any{"role": "assistant", "content": "好的。"})
		}
	case "wrapper-harmless":
		// <instructions> tags around harmless text: is the wrapper itself refused?
		msgs = append(msgs, map[string]any{"role": "system", "content": sysLine})
		msgs = append(msgs, map[string]any{"role": "user", "content": "<instructions>\n" + promptPrefix + "\n</instructions>"})
		msgs = append(msgs, map[string]any{"role": "assistant", "content": "好的。"})
	case "wrapped-user":
		// the refused text in a user message WITH a wrapper tag, to separate
		// "the text" from "the wrapper shape" as the gate's trigger.
		msgs = append(msgs, map[string]any{"role": "system", "content": sysLine})
		if promptPrefix != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": "<instructions>\n" + promptPrefix + "\n</instructions>"})
			msgs = append(msgs, map[string]any{"role": "assistant", "content": "好的。"})
		}
	case "persona-first":
		// persona line FIRST, caller text after it: exactly what our driver's
		// managedBody builds. Tests whether ordering is what matters.
		msgs = append(msgs, map[string]any{"role": "system", "content": sysLine + "\n\n" + promptPrefix})
	case "plain-user":
		// The refused text in an ordinary user message, no wrapper: isolates
		// "is the text itself refused anywhere" from "is the wrapper wrong".
		msgs = append(msgs, map[string]any{"role": "system", "content": sysLine})
		if promptPrefix != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": promptPrefix})
			msgs = append(msgs, map[string]any{"role": "assistant", "content": "好的。"})
		}
	case "mid-system":
		// A second system message further down: the shape magpie's checker
		// inspects (system AND assistant messages, anywhere).
		msgs = append(msgs, map[string]any{"role": "system", "content": sysLine})
		msgs = append(msgs, map[string]any{"role": "user", "content": "你好"})
		msgs = append(msgs, map[string]any{"role": "assistant", "content": "你好。"})
		if promptPrefix != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": promptPrefix})
		}
	default:
		msgs = append(msgs, map[string]any{"role": "system", "content": system})
	}
	for i := 0; i < turns; i++ {
		msgs = append(msgs, map[string]any{
			"role":    "user",
			"content": fmt.Sprintf("第 %d 轮：请简述你对 Go 语言并发模型的理解，两三句话即可。", i+1),
		})
		if tools && i%2 == 1 {
			callID := fmt.Sprintf("call_%d", i)
			msgs = append(msgs, map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{map[string]any{
					"id": callID, "type": "function",
					"function": map[string]any{"name": "bash", "arguments": `{"command":"go version"}`},
				}},
			})
			msgs = append(msgs, map[string]any{
				"role": "tool", "tool_call_id": callID, "content": "go version go1.22.0 windows/amd64",
			})
			msgs = append(msgs, map[string]any{
				"role": "assistant", "content": "本机是 Go 1.22。",
			})
		} else {
			msgs = append(msgs, map[string]any{
				"role":    "assistant",
				"content": "Go 用 goroutine 和 channel 表达并发：goroutine 是轻量线程，channel 负责在它们之间安全地传递数据，而不是共享内存。",
			})
		}
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "用一句话总结前面的内容。"})
	if padTokens > 0 {
		msgs = padHistory(msgs, padTokens)
	}

	body := map[string]any{"model": model, "messages": msgs, "stream": true}
	if tools {
		body["tools"] = []any{
			map[string]any{"type": "function", "function": map[string]any{
				"name": "bash", "description": "run a shell command",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{
					"command": map[string]any{"type": "string"},
				}, "required": []any{"command"}},
			}},
			map[string]any{"type": "function", "function": map[string]any{
				"name": "read", "description": "read a file",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{
					"path": map[string]any{"type": "string"},
				}, "required": []any{"path"}},
			}},
		}
	}
	return body
}

// padHistory grows the conversation to roughly padTokens prompt tokens by
// appending long assistant turns. A real ZCode session reaches the gateway
// with 100k+ input tokens; the field failures were all on those, so the probe
// has to reproduce the size, not just the shape.
func padHistory(msgs []any, padTokens int) []any {
	// Roughly 2 chars per token; one filler turn of ~8k chars ≈ 4k tokens.
	const chunk = 8000
	need := padTokens * 2
	filler := strings.Repeat("这段文字用于模拟真实会话中的长上下文内容，不包含任何指令。", chunk/33)
	for need > 0 {
		n := chunk
		if need < n {
			n = need
		}
		text := filler
		if n < len(filler) {
			text = filler[:n]
		}
		msgs = append(msgs,
			map[string]any{"role": "user", "content": "补充一段上下文。"},
			map[string]any{"role": "assistant", "content": text},
		)
		need -= n
	}
	return msgs
}

// loadCreds reads the autoclaw account row and decrypts it.
func loadCreds(dsn, keyFile string) (*creds, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	var enc string
	if err := conn.QueryRow(ctx,
		"SELECT creds_enc FROM accounts WHERE channel='autoclaw' AND disabled=false LIMIT 1").Scan(&enc); err != nil {
		return nil, err
	}
	key, err := cryptox.LoadOrCreateKey(keyFile)
	if err != nil {
		return nil, err
	}
	box, err := cryptox.New(key)
	if err != nil {
		return nil, err
	}
	plain, err := box.Open(enc)
	if err != nil {
		return nil, err
	}
	var c creds
	if err := json.Unmarshal(plain, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// readSecret pulls the app secret out of config.json.
func readSecret(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cfg struct {
		Channels struct {
			AutoClaw struct {
				AppSecret string `json:"app_secret"`
			} `json:"autoclaw"`
		} `json:"channels"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return ""
	}
	return cfg.Channels.AutoClaw.AppSecret
}

func uuid() string {
	b := make([]byte, 16)
	now := time.Now().UnixNano()
	for i := range b {
		b[i] = byte(now >> (uint(i%8) * 8))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func firstLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// listUpstreamModels prints the route ids the gateway publishes. This is the
// only way to learn which ids exist: sending an id it does not know answers
// "非法模型", which says nothing about what it would accept instead.
func listUpstreamModels(secret, tok string) {
	paths := []string{
		"/autoclaw-proxy/proxy/autoclaw-model-config",
		"/autoclaw-proxy/proxy/autoclaw/model-config",
	}
	hosts := []string{baseURL, "https://autoglm-api.autoglm.ai"}
	for _, host := range hosts {
		for _, path := range paths {
			ts := fmt.Sprintf("%d", time.Now().Unix())
			sum := md5.Sum([]byte(appID + "&" + ts + "&" + secret))
			req, err := http.NewRequest(http.MethodGet, host+path, nil)
			if err != nil {
				continue
			}
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", "AutoClaw/2.1.0 (win-amd64)")
			req.Header.Set("X-Version", "2.1.0")
			req.Header.Set("X-Tm", "win")
			req.Header.Set("X-Product", "autoclaw")
			req.Header.Set("X-Client-Type", "pc")
			req.Header.Set("X-Lang", "zh-CN")
			req.Header.Set("X-Channel", "zai")
			req.Header.Set("X-Authorization", "Bearer "+tok)
			req.Header.Set("X-Auth-Appid", appID)
			req.Header.Set("X-Auth-TimeStamp", ts)
			req.Header.Set("X-Auth-Sign", hex.EncodeToString(sum[:]))
			resp, err := (&http.Client{Timeout: 25 * time.Second}).Do(req)
			if err != nil {
				fmt.Printf("%s%s -> %v\n", host, path, err)
				continue
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			fmt.Printf("%s%s -> HTTP %d (%d bytes)\n", host, path, resp.StatusCode, len(body))
			if resp.StatusCode == 200 {
				fmt.Println(string(body))
			}
		}
	}
}
