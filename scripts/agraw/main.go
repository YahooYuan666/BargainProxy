// Command agraw asks BOTH antigravity hosts the same question, for EVERY
// antigravity account, and prints what each one did.
//
// WHY (2026-11-01, BP-256): gemini requests hung for minutes with "http2:
// timeout awaiting response headers" and were then canceled by the client, so
// the log never said WHICH host or WHICH account was hanging. The driver fails
// over only on transport errors and 5xx — a hang is neither, so the fallback
// host was never reached, and when the client's context was finally canceled
// the fallback ran with that same dead context.
//
// Read-only: builds the driver's own body and posts it to each host with a
// per-host deadline. No pool, no cooldown, no store, no dump.
//
// Usage:
//
//	go run ./scripts/agraw -model gemini-3.8-flash-high -timeout 40s
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/cryptox"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/antigravity"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/store"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config.json")
	modelName := flag.String("model", "gemini-3.8-flash-high", "upstream model id")
	prompt := flag.String("prompt", "你好", "user prompt")
	perHost := flag.Duration("timeout", 40*time.Second, "per-host deadline")
	nTools := flag.Int("tools", 0, "how many dummy tools to declare")
	maxTok := flag.Int("max-tokens", 64, "max output tokens")
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
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
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
	var accs []*model.Account
	for _, a := range accounts {
		if a.Channel == model.ChannelAntigravity {
			accs = append(accs, a)
		}
	}
	if len(accs) == 0 {
		log.Fatalf("no antigravity account in store")
	}

	d := antigravity.New(cfg.Channels.Antigravity, nil)
	fmt.Printf("model=%s per-host timeout=%s accounts=%d\n", *modelName, *perHost, len(accs))

	for _, acc := range accs {
		call := driver.ChatCall{
			Account:       acc,
			UpstreamModel: *modelName,
			Request: &ir.ChatRequest{
				Model:     *modelName,
				Stream:    true,
				MaxTokens: *maxTok,
				Tools:     buildTools(*nTools),
				Messages: []ir.Message{{
					Role:    ir.RoleUser,
					Content: []ir.ContentPart{{Kind: "text", Text: *prompt}},
				}},
			},
		}
		fmt.Printf("\n=== account=%s ===\n", acc.Nickname)
		results, perr := antigravity.ProbeHosts(ctx, d, call, *perHost, 512)
		if perr != nil {
			fmt.Printf("  probe error: %v\n", perr)
			continue
		}
		for _, r := range results {
			fmt.Println("  " + r.Describe())
		}
	}
}

// buildTools makes n dummy tools shaped like a real agent's (name, description,
// one object schema with two fields).
func buildTools(n int) []ir.ToolDef {
	if n <= 0 {
		return nil
	}
	out := make([]ir.ToolDef, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ir.ToolDef{
			Name:        fmt.Sprintf("tool_%d", i),
			Description: "A tool used for routine workspace operations, listing files or reading a small file.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":  map[string]any{"type": "string"},
					"limit": map[string]any{"type": "integer"},
				},
				"required": []any{"path"},
			},
		})
	}
	return out
}
