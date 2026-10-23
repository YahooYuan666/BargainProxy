// Command wbprobe sends one chat request straight to the workbuddy upstream,
// bypassing the gateway pool (no lease, no cooldown logic). Diagnostic tool
// for questions like "is this model rate-limited upstream right now?" — a
// cooled model never leaves the pool, so the gateway cannot show upstream's
// own words for it.
//
// Usage:
//
//	go run ./scripts/wbprobe -model deepseek-v4.1-flash -prompt ping
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
	"bargainproxy/internal/driver/workbuddy"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/store"
	"bargainproxy/internal/uperr"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config.json")
	modelName := flag.String("model", "deepseek-v4.1-flash", "workbuddy upstream model id")
	prompt := flag.String("prompt", "ping", "user prompt")
	maxTok := flag.Int("max-tokens", 16, "max output tokens")
	timeout := flag.Duration("timeout", 90*time.Second, "overall deadline; raise it to measure long generations")
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
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
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
		if a.Channel == model.ChannelWorkBuddy {
			acc = a
			break
		}
	}
	if acc == nil {
		log.Fatalf("no workbuddy account in store")
	}
	fmt.Printf("account=%s nickname=%s\n", acc.ID, acc.Nickname)
	if len(acc.ModelCooldowns) > 0 {
		fmt.Printf("persisted model cooldowns (this probe ignores them): %v\n", acc.ModelCooldowns)
	}

	d := workbuddy.New(cfg.Channels.WorkBuddy, nil)
	req := &ir.ChatRequest{
		Model:     *modelName,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: *prompt}}}},
		Stream:    true,
		MaxTokens: *maxTok,
	}
	emit := func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
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
