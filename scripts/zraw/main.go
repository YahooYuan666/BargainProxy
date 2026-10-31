// Command zraw asks the ZCode upstream one question and prints its raw answer.
//
// WHY (2026-11-01, BP-253): every zcode leg was dying in ~40ms with "stream
// ended without message_stop" — an HTTP 200 whose body the SSE parser could not
// use — and the pool then cooled the channel, so it could not be re-observed
// from the gateway. This bypasses the pool entirely (no lease, no cooldown, no
// store writes) and prints the upstream's own opening bytes, so the cause is
// answered by evidence.
//
// Read-only. It builds the request through the driver's own body builder and
// header set, so what is measured is the real wire request.
//
// Usage:
//
//	go run ./scripts/zraw -model glm-5.3-flash
//	go run ./scripts/zraw -model glm-5.3-flash -prompt "say ok"
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
	"bargainproxy/internal/driver/zcode"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/store"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config.json")
	modelName := flag.String("model", "glm-5.3-flash", "canonical/upstream model id")
	prompt := flag.String("prompt", "Reply with exactly: ok", "user prompt")
	maxTok := flag.Int("max-tokens", 64, "max output tokens")
	head := flag.Int("head", 4096, "bytes of the response to print")
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
		if a.Channel == model.ChannelZCode {
			acc = a
			break
		}
	}
	if acc == nil {
		log.Fatalf("no zcode account in store")
	}

	d := zcode.New(cfg.Channels.ZCode, nil)
	call := driver.ChatCall{
		Account:       acc,
		UpstreamModel: *modelName,
		Request: &ir.ChatRequest{
			Model:     *modelName,
			MaxTokens: *maxTok,
			Stream:    true,
			Messages: []ir.Message{{
				Role:    ir.RoleUser,
				Content: []ir.ContentPart{{Kind: "text", Text: *prompt}},
			}},
		},
	}

	fmt.Printf("account=%s nickname=%s model=%s host=%s\n",
		acc.ID, acc.Nickname, *modelName, cfg.Channels.ZCode.BaseURL)
	start := time.Now()
	status, body, err := zcode.ProbeRaw(ctx, d, call, *head)
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil {
		fmt.Printf("\nTRANSPORT ERROR after %s: %v\n", elapsed, err)
		return
	}
	fmt.Printf("\nHTTP %d after %s, %d bytes captured:\n---\n%s\n---\n", status, elapsed, len(body), body)
}
