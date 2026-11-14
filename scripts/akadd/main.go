// Command akadd adds (or updates) aistudio accounts — Google AI Studio API
// keys — into the gateway's account store, offline. It is the command-line
// equivalent of the panel's 添加账号 → AI Studio form (BP-326): same stable
// identity rule (the hashed key is the account ID), same crypto envelope,
// because direct DB inserts cannot produce the store's encrypted CredsJSON.
//
// Usage:
//
//	go run ./scripts/akadd -key 'AQ.Ab8...' -label 号1
//	go run ./scripts/akadd -keys keys.json   // [{"key":"...","label":"..."}, ...]
//
// Re-adding an existing key updates the row (upsert by ID); labels are
// display-only. Keys are taken from arguments or a JSON file and never
// printed back.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/cryptox"
	"bargainproxy/internal/model"
	"bargainproxy/internal/panel"
	"bargainproxy/internal/store"
)

type keyEntry struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to config.json")
	key := flag.String("key", "", "single API key to add")
	label := flag.String("label", "", "display name for the single key")
	keysFile := flag.String("keys", "", "JSON file: [{\"key\":\"...\",\"label\":\"...\"}, ...]")
	flag.Parse()
	if *key == "" && *keysFile == "" {
		log.Fatal("key or keys file required")
	}
	var entries []keyEntry
	if *keysFile != "" {
		raw, err := os.ReadFile(*keysFile)
		if err != nil {
			log.Fatalf("keys file: %v", err)
		}
		if err := json.Unmarshal(raw, &entries); err != nil {
			log.Fatalf("keys file: %v", err)
		}
	} else {
		entries = []keyEntry{{Key: *key, Label: *label}}
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	k, err := cryptox.LoadOrCreateKey(cfg.KeyFile)
	if err != nil {
		log.Fatalf("cryptox: %v", err)
	}
	box, err := cryptox.New(k)
	if err != nil {
		log.Fatalf("cryptox: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := store.OpenPostgres(ctx, cfg.Database, box)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	added := 0
	for _, e := range entries {
		creds, _ := json.Marshal(map[string]string{"api_key": e.Key})
		acc, err := panel.NormalizeImportForTools(model.ChannelAIStudio, creds, e.Label)
		if err != nil {
			log.Fatalf("normalize: %v", err)
		}
		if err := st.SaveAccount(acc); err != nil {
			log.Fatalf("save %s: %v", acc.ID, err)
		}
		added++
	}
	fmt.Printf("aistudio: %d account(s) saved (visible after gateway restart)\n", added)
}
