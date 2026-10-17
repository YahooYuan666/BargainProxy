// Command routemerge merges route rows into the live settings.model_routes
// table (the panel's PUT /panel/api/routes equivalent for operators without a
// browser session). Rows are matched by (model, channel, upstream_model);
// existing rows are replaced in place, new rows appended.
//
// Usage:
//
//	go run ./scripts/routemerge -rows '[{"model":"glm-5.3","channel":"qoder","upstream_model":"GLM-5.3","priority":1}]'
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/cryptox"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config.json")
	rowsJSON := flag.String("rows", "", "JSON array of router.Row to merge")
	deleteJSON := flag.String("delete", "", "JSON array of router.Row to delete (match model+channel+upstream_model)")
	flag.Parse()
	if *rowsJSON == "" && *deleteJSON == "" {
		log.Fatal("rows or delete required")
	}
	var add, del []router.Row
	if *rowsJSON != "" {
		if err := json.Unmarshal([]byte(*rowsJSON), &add); err != nil {
			log.Fatalf("rows: %v", err)
		}
	}
	if *deleteJSON != "" {
		if err := json.Unmarshal([]byte(*deleteJSON), &del); err != nil {
			log.Fatalf("delete: %v", err)
		}
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := store.OpenPostgres(ctx, cfg.Database, box)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	var rows []router.Row
	if v, ok, _ := st.GetSetting("model_routes"); ok {
		_ = json.Unmarshal([]byte(v), &rows)
	}
	keyOf := func(r router.Row) string { return r.Model + "|" + r.Channel + "|" + r.UpstreamModel }
	remove := map[string]bool{}
	for _, r := range del {
		remove[keyOf(r)] = true
	}
	if len(remove) > 0 {
		kept := rows[:0]
		for _, r := range rows {
			if !remove[keyOf(r)] {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	index := map[string]int{}
	for i, r := range rows {
		index[keyOf(r)] = i
	}
	added, replaced := 0, 0
	for _, r := range add {
		if i, ok := index[keyOf(r)]; ok {
			rows[i] = r
			replaced++
			continue
		}
		index[keyOf(r)] = len(rows)
		rows = append(rows, r)
		added++
	}
	raw, _ := json.Marshal(rows)
	if err := st.SetSetting("model_routes", string(raw)); err != nil {
		log.Fatalf("save: %v", err)
	}
	fmt.Printf("routes: %d total (%d added, %d replaced)\n", len(rows), added, replaced)
}
