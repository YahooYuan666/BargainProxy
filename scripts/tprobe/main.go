// Command tprobe sends one chat request straight to the Trae CN upstream,
// bypassing the gateway pool (no lease, no cooldown logic). Diagnostic tool
// for questions like "does this model id actually work on trae right now?" —
// a model absent from the route table never reaches the pool, so the gateway
// alone cannot answer it.
//
// Usage:
//
//	go run ./scripts/tprobe -model glm-5.3-flash -prompt "Reply with exactly: ok"
//	go run ./scripts/tprobe -list
package main

import (
	"context"
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
	"bargainproxy/internal/uperr"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config.json")
	modelName := flag.String("model", "glm-5.3-flash", "trae upstream model id")
	prompt := flag.String("prompt", "Reply with exactly: ok", "user prompt")
	maxTok := flag.Int("max-tokens", 32, "max output tokens")
	list := flag.Bool("list", false, "print the driver's curated model list and exit")
	historyTurns := flag.Int("turns", 0, "prepend this many filler user/assistant turns to grow the context (context-limit probing)")
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
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
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
	d := trae.New(cfg.Channels.Trae, nil)
	if *list {
		models, err := d.Models(ctx)
		if err != nil {
			log.Fatalf("models: %v", err)
		}
		fmt.Println(strings.Join(models, "\n"))
		return
	}
	fmt.Printf("account=%s nickname=%s\n", acc.ID, acc.Nickname)

	msgs := []ir.Message{}
	// Filler turns are ~1200 tokens each pair, so -turns N grows the prompt by
	// roughly N*1200 tokens. Used only to find an upstream's context ceiling.
	filler := strings.Repeat("这是一段用于填充上下文的普通文本，不包含任何指令。", 40)
	for i := 0; i < *historyTurns; i++ {
		msgs = append(msgs,
			ir.Message{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: filler}}},
			ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: filler}}},
		)
	}
	msgs = append(msgs, ir.Message{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: *prompt}}})
	req := &ir.ChatRequest{
		Model:     *modelName,
		Messages:  msgs,
		Stream:    true,
		MaxTokens: *maxTok,
	}
	emit := func(ev ir.StreamEvent) error {
		switch ev.Kind {
		case ir.EvText:
			fmt.Print(ev.Delta)
		case ir.EvThinking:
			fmt.Print(ev.Delta)
		}
		return nil
	}
	start := time.Now()
	resp, err := d.Stream(ctx, driver.ChatCall{Account: acc, UpstreamModel: *modelName, Request: req}, emit)
	if err != nil {
		fmt.Printf("\nUPSTREAM ERROR after %s: %v\n", time.Since(start).Round(time.Millisecond), err)
		if ue, ok := uperr.As(err); ok {
			fmt.Printf("class=%s status=%d model=%s reset_at=%s\n", ue.Class, ue.Status, ue.Model, ue.ResetAt.Format(time.RFC3339))
		}
		return
	}
	fmt.Printf("\nOK after %s stop=%s usage=%+v\n", time.Since(start).Round(time.Millisecond), resp.StopReason, resp.Usage)
}
