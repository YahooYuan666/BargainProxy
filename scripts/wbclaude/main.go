// Command wbclaude settles "does the WorkBuddy CN deployment serve claude
// models at all" with first-hand evidence, in three steps:
//
//  1. GET  {cn}/v3/config          — the model catalog (free, no credits)
//  2. POST {cn}/v1/messages        — the Anthropic-native route magpie #346
//     measured (claude-opus-5.5 / claude-sonnet-5.5 / claude-opus-5 /
//     claude-opus-4.8-1m answering in Anthropic's stream, 2026-10-24)
//  3. POST {cn}/v2/chat/completions — the chat lane, for contrast
//
// Steps 2/3 send ONE tiny request each (max_tokens 16). A refusal is as
// valid an answer as a 200 — the point is to stop arguing from common sense
// and read what the endpoint actually says.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/cryptox"
	"bargainproxy/internal/driver/workbuddy"
	"bargainproxy/internal/model"
	"bargainproxy/internal/store"
)

func main() {
	cfg, err := config.Load("config.json")
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
	// Prefer a CN-realm account: the whole question is about the CN
	// deployment, and the two realms' tokens do not interoperate (edge 401
	// before any model routing — the first run's confound).
	var acc *model.Account
	var globalAcc *model.Account
	for _, a := range accounts {
		if a.Channel != model.ChannelWorkBuddy {
			continue
		}
		c, err := workbuddy.ParseCreds(a.Credentials())
		if err != nil {
			continue
		}
		if c.RealmOrDefault() != "global" && acc == nil {
			acc = a
		}
		if globalAcc == nil {
			globalAcc = a
		}
	}
	if acc == nil {
		acc = globalAcc
	}
	if acc == nil {
		log.Fatalf("no workbuddy account in store")
	}
	var creds *workbuddy.Creds
	{
		c, err := workbuddy.ParseCreds(acc.Credentials())
		if err != nil {
			log.Fatalf("creds: %v", err)
		}
		creds = c
	}
	base := strings.TrimRight(cfg.Channels.WorkBuddy.ChatBaseCN, "/")
	if base == "" {
		base = "https://www.workbuddy.cn"
	}
	fmt.Printf("account=%s realm=%s base=%s\n", acc.ID, creds.RealmOrDefault(), base)

	client := &http.Client{Timeout: 45 * time.Second}
	hdrs := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+creds.AccessToken)
		if creds.DeviceToken != "" {
			r.Header.Set("X-Device-Token", creds.DeviceToken)
		}
		r.Header.Set("X-CodeBuddy-Request", "1")
		r.Header.Set("X-Product", "SaaS")
		r.Header.Set("X-Agent-Purpose", "conversation")
		r.Header.Set("X-IDE-Name", "WorkBuddy")
		r.Header.Set("X-IDE-Type", "WorkBuddy")
		r.Header.Set("X-Agent-Type", "main")
		r.Header.Set("X-Agent-Intent", "craft")
		r.Header.Set("User-Agent", "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.176.0")
		r.Header.Set("Accept", "application/json")
	}
	dump := func(name string, resp *http.Response, body []byte) {
		fmt.Printf("\n=== %s -> HTTP %d (%d bytes)\n", name, resp.StatusCode, len(body))
		snippet := string(body)
		if len(snippet) > 700 {
			snippet = snippet[:700] + "…"
		}
		fmt.Println(snippet)
	}

	// 1. catalog
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v3/config", nil)
	hdrs(req)
	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("v3/config: %v", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	dump("GET /v3/config", resp, raw)
	claudeRe := regexp.MustCompile(`"[a-zA-Z0-9._-]*claude[a-zA-Z0-9._-]*"`)
	seen := map[string]bool{}
	var claudeIDs []string
	for _, m := range claudeRe.FindAllStringSubmatch(string(raw), -1) {
		id := strings.Trim(m[0], `"`)
		if !seen[id] {
			seen[id] = true
			claudeIDs = append(claudeIDs, id)
		}
	}
	fmt.Printf("\nclaude-ish ids in catalog: %v\n", claudeIDs)
	probeModel := "claude-opus-5.5"
	if len(claudeIDs) > 0 {
		probeModel = claudeIDs[0]
	}

	// 2. Anthropic-native route
	payload, _ := json.Marshal(map[string]any{
		"model":      probeModel,
		"max_tokens": 16,
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
	})
	req2, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", bytes.NewReader(payload))
	hdrs(req2)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := client.Do(req2)
	if err != nil {
		fmt.Printf("\nPOST /v1/messages transport error: %v\n", err)
	} else {
		raw2, _ := io.ReadAll(io.LimitReader(resp2.Body, 1<<20))
		resp2.Body.Close()
		dump("POST /v1/messages model="+probeModel, resp2, raw2)
	}

	// 3. chat lane, for contrast
	payload3, _ := json.Marshal(map[string]any{
		"model":      probeModel,
		"max_tokens": 16,
		"stream":     true,
		"messages":   []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "ping"}}}},
	})
	req3, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v2/chat/completions", bytes.NewReader(payload3))
	hdrs(req3)
	req3.Header.Set("Content-Type", "application/json")
	resp3, err := client.Do(req3)
	if err != nil {
		fmt.Printf("\nPOST /v2/chat/completions transport error: %v\n", err)
	} else {
		raw3, _ := io.ReadAll(io.LimitReader(resp3.Body, 1<<20))
		resp3.Body.Close()
		dump("POST /v2/chat/completions model="+probeModel, resp3, raw3)
	}
}
