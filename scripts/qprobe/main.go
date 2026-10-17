// Command qprobe prints the raw Qoder quota document for the stored account.
// Diagnostic tool for quota field mapping; it prints quota numbers only (no
// credentials).
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
	"bargainproxy/internal/driver/qoder"
	"bargainproxy/internal/model"
	"bargainproxy/internal/store"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config.json")
	catalog := flag.Bool("catalog", false, "print the live model catalogue instead of quota")
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
		if a.Channel == model.ChannelQoder {
			acc = a
			break
		}
	}
	if acc == nil {
		log.Fatal("no qoder account")
	}
	d := qoder.New(cfg.Channels.Qoder, nil)
	if *catalog {
		cat, err := d.CatalogMap(ctx, acc)
		if err != nil {
			log.Fatalf("catalog: %v", err)
		}
		raw, _ := json.MarshalIndent(cat, "", "  ")
		os.Stdout.Write(raw)
		return
	}
	doc, err := d.QuotaRaw(ctx, acc)
	if err != nil {
		log.Fatalf("quota: %v", err)
	}
	raw, _ := json.MarshalIndent(doc, "", "  ")
	os.Stdout.Write(raw)
}
