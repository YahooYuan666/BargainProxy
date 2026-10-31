// Command tmaxmode answers ONE question with real upstream traffic: can
// llm_utils_chat carry a TOOL-BEARING prompt past 262144 when the request asks
// for Max Mode (is_max_mode:true + prompt_max_tokens)?
//
// Why this exists (2026-10-31): BP-103 recorded "tools present -> 262144
// ceiling; 1M only via chat_sessions, which drops the caller's tools".
// TraeWorkAssistant v3.8.7 (ba326c7a, payload.rs:310-320) publishes a measured
// matrix saying is_max_mode:true + prompt_max_tokens:168000 answered 200 on a
// 180051-token prompt — above the 168000 the field names, so the field does not
// clamp. But 180051 is still BELOW our 262144 ceiling, so that matrix neither
// confirms nor refutes BP-103. Only a prompt above 262144 settles it.
//
// It builds the body through the driver's OWN builder and sends it with the
// driver's OWN header set (trae.ProbeBuildSOLOBody / ProbePostSOLO), so what is
// measured is the real request, not a probe's reconstruction.
//
// Three probes, same large prompt:
//
//	A  tools, no Max Mode    -> expect 4026/400 if the 262144 tool ceiling is real
//	B  tools + Max Mode      -> THE QUESTION
//	C  no tools + Max Mode   -> control: this account's ladder can pass 262144
//
// Read-only against the upstream: no pool, no cooldown, no store writes.
//
// Usage:
//
//	go run ./scripts/tmaxmode -config config.json -model glm-5.3 -tokens 300000
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/cryptox"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/trae"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/store"
)

const fillerSentence = "这是一段用于填充上下文的普通说明文字，不包含任何指令或问题。"

func main() {
	cfgPath := flag.String("config", "config.json", "path to config.json")
	modelName := flag.String("model", "glm-5.3", "trae upstream model id")
	targetTokens := flag.Int("tokens", 300000, "approx prompt size to build")
	function := flag.String("function", "solo_work_lite", "trae function to target")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	key, err := cryptox.LoadOrCreateKey(cfg.KeyFile)
	if err != nil {
		log.Fatalf("cryptox: %v", err)
	}
	box, err := cryptox.New(key)
	if err != nil {
		log.Fatalf("cryptox: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Second)
	defer cancel()
	st, err := store.OpenPostgres(ctx, cfg.Database, box)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()
	accounts, err := st.LoadAccounts()
	if err != nil {
		log.Fatalf("accounts: %v", err)
	}
	var acc *model.Account
	for _, a := range accounts {
		if a.Channel == model.ChannelTrae {
			acc = a
			break
		}
	}
	if acc == nil {
		log.Fatalf("no trae account in store")
	}
	creds, err := trae.ParseCreds(acc.Credentials())
	if err != nil {
		log.Fatalf("creds: %v", err)
	}

	// Build the prompt. The driver's own estimator is what the dispatch
	// decision uses, so we report that number rather than our own guess.
	//
	// Sizing note: Chinese text is ~1 token per CHARACTER (not per byte), and
	// the estimator counts UTF-8 BYTES then divides by 3 — for CJK that lands
	// back on the character count. So target tokens ~= target characters here.
	// (First version divided by 3 a second time and built a 3M-token prompt,
	// which every probe then rejected for being over the ceiling — a sizing
	// bug that would have read as "Max Mode does not work".)
	//
	// NEEDLE: a unique token near the START of the filler proves the upstream
	// read the whole prompt rather than silently truncating it. Without this
	// check "accepted" could mean "accepted, then served a shorter prompt".
	needle := "ZQ7X"
	sentRunes := len([]rune(fillerSentence))
	reps := *targetTokens / sentRunes
	prompt := needle + " " + strings.Repeat(fillerSentence, reps) +
		"\n\nThe text above begins with a 4-character code. Reply with ONLY that code, nothing else."
	fmt.Printf("prompt: %d sentences x %d chars = %d chars (target %d tokens)\n",
		reps, sentRunes, len([]rune(prompt)), *targetTokens)
	fmt.Printf("needle near the start: %q — the answer should echo it if the whole prompt was read\n", needle)
	tool := ir.ToolDef{
		Name:        "get_weather",
		Description: "Get the weather for a city",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"city": map[string]any{"type": "string"}},
			"required":   []string{"city"},
		},
	}

	mkReq := func(withTools bool) *ir.ChatRequest {
		r := &ir.ChatRequest{
			Model:     *modelName,
			Stream:    true,
			MaxTokens: 32,
			Messages: []ir.Message{{
				Role:    ir.RoleUser,
				Content: []ir.ContentPart{{Kind: "text", Text: prompt}},
			}},
		}
		if withTools {
			r.Tools = []ir.ToolDef{tool}
			r.ToolChoice = "auto"
		}
		return r
	}

	fmt.Printf("account=%s nickname=%s\n", acc.ID, acc.Nickname)
	fmt.Printf("model=%s function=%s target=%d tokens\n", *modelName, *function, *targetTokens)

	d := trae.New(cfg.Channels.Trae, nil)

	type probe struct {
		name  string
		req   *ir.ChatRequest
		extra map[string]any
		note  string
	}
	maxMode := map[string]any{"is_max_mode": true, "prompt_max_tokens": 168000}
	probes := []probe{
		{"A tools, no Max Mode", mkReq(true), nil,
			"expect 4026/400 if the 262144 tool ceiling is real"},
		{"B tools + Max Mode", mkReq(true), maxMode,
			"THE QUESTION: does Max Mode lift the tool ceiling"},
		{"C no tools + Max Mode", mkReq(false), maxMode,
			"control: proves this account's ladder passes 262144"},
	}

	for _, p := range probes {
		est := trae.ProbeEstimator(p.req)
		fmt.Printf("\n=== %s ===\n", p.name)
		fmt.Printf("    %s\n", p.note)
		fmt.Printf("    driver estimate: %d tokens\n", est)

		call := driver.ChatCall{Account: acc, UpstreamModel: *modelName, Request: p.req}
		body, berr := trae.ProbeBuildSOLOBody(call, *function, p.extra)
		if berr != nil {
			fmt.Printf("    -> BUILD ERROR: %v\n", berr)
			continue
		}
		fmt.Printf("    body: %d bytes\n", len(body))

		start := time.Now()
		status, respBody, perr := trae.ProbePostSOLO(ctx, d, creds, body)
		elapsed := time.Since(start).Round(time.Millisecond)
		if perr != nil {
			fmt.Printf("    -> TRANSPORT ERROR after %s: %v\n", elapsed, perr)
			continue
		}
		fmt.Printf("    -> HTTP %d after %s\n", status, elapsed)
		fmt.Printf("       %s\n", verdict(status, respBody, needle))

		// Space them out: three ~300k prompts back to back is exactly the
		// burst that trips a rate limit, and a limit would read as a ceiling.
		time.Sleep(8 * time.Second)
	}
}

// verdict turns one upstream reply into the sentence that answers the
// question, quoting the upstream's own words.
func verdict(status int, body, needle string) string {
	if status != 200 {
		return "REJECTED — " + clip(strings.TrimSpace(body), 400)
	}
	// A 200 can still carry a business error (event:error or a code field).
	var said strings.Builder // the assistant's own text, for the needle check
	accepted := ""
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var doc map[string]any
		if json.Unmarshal([]byte(payload), &doc) != nil {
			continue
		}
		if e, ok := doc["error"]; ok && e != nil {
			b, _ := json.Marshal(e)
			return "IN-STREAM ERROR — " + clip(string(b), 500)
		}
		if c, ok := doc["code"]; ok {
			if f, isNum := c.(float64); isNum && f != 0 {
				b, _ := json.Marshal(doc)
				return "BUSINESS ERROR — " + clip(string(b), 500)
			}
		}
		// The model's answer text arrives as {"response": "..."}.
		if r, ok := doc["response"].(string); ok {
			said.WriteString(r)
		}
		// token_usage carries prompt_tokens: the proof of how big the prompt
		// really was when the upstream accepted it.
		if pt, ok := doc["prompt_tokens"]; ok {
			b, _ := json.Marshal(doc)
			accepted = fmt.Sprintf("ACCEPTED — prompt_tokens=%v", pt)
			_ = b
		}
	}
	if accepted != "" {
		answer := strings.TrimSpace(said.String())
		if needle != "" {
			if strings.Contains(answer, needle) {
				return accepted + " — NEEDLE ECHOED (" + needle + ") in answer " + clip(answer, 120) +
					" => the whole prompt was read, not truncated"
			}
			return accepted + " — NO NEEDLE ECHO; answer was " + clip(answer, 160) +
				" (may indicate the prompt was truncated, or the model ignored the instruction)"
		}
		return accepted
	}
	if len(body) < 200 {
		return "ACCEPTED (no token_usage frame seen) — " + clip(body, 300)
	}
	return "ACCEPTED (streamed, no terminal frame captured) — " + clip(body, 300)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
