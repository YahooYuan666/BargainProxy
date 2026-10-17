// Command creditprobe prints the credit detail payloads the panel uses for
// AutoClaw (points ledger / expiring) and Qoder (quota breakdown / check-in).
// Diagnostic tool: reads the stored accounts, never prints credentials.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/cryptox"
	"bargainproxy/internal/driver/autoclaw"
	"bargainproxy/internal/driver/qoder"
	"bargainproxy/internal/model"
	"bargainproxy/internal/store"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config.json")
	channel := flag.String("channel", "", "autoclaw | qoder (empty = both)")
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
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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
	acd := autoclaw.New(cfg.Channels.AutoClaw, nil)
	qd := qoder.New(cfg.Channels.Qoder, nil)
	out := map[string]any{}
	for _, acc := range accounts {
		if acc.Channel == model.ChannelAutoClaw && (*channel == "" || *channel == "autoclaw") {
			det, err := acd.CreditDetail(ctx, acc, 1, 20)
			if err != nil {
				out["autoclaw"] = map[string]any{"error": err.Error()}
			} else {
				out["autoclaw"] = det
			}
		}
		if acc.Channel == model.ChannelQoder && (*channel == "" || *channel == "qoder") {
			det, err := qd.QuotaDetail(ctx, acc)
			if err != nil {
				out["qoder"] = map[string]any{"error": err.Error()}
			} else {
				out["qoder"] = det
			}
		}
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	os.Stdout.Write(raw)
}
