// Command wbmodels registers the BargainProxy gateway models into the
// WorkBuddy desktop app's custom-model file (%USERPROFILE%\.workbuddy\
// models.json). The logic lives in internal/wbregister, shared with the
// panel's 「登记进 WorkBuddy」 button (BP-105); this CLI is the headless/
// scriptable form of the same operation.
//
// Usage:
//
//	go run ./scripts/wbmodels            # dry run: print the plan
//	go run ./scripts/wbmodels -write     # backup + merge-write models.json
//
// WorkBuddy watches the file (fs.watch + 1s debounce) and picks the models
// up without a restart.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"bargainproxy/internal/wbregister"
)

func main() {
	panel := flag.String("panel", "http://127.0.0.1:13140", "bargainproxy panel base url")
	configPath := flag.String("config", "config.json", "config.json path (to read the gateway api_key)")
	out := flag.String("out", "", "target models.json (default %USERPROFILE%\\.workbuddy\\models.json)")
	write := flag.Bool("write", false, "apply changes (default: dry run)")
	prune := flag.Bool("prune", true, "drop stale BargainProxy entries no longer routed")
	timeout := flag.Duration("timeout", 20*time.Second, "panel request timeout")
	flag.Parse()

	apiKey := readAPIKey(*configPath)
	if *out == "" {
		p, err := wbregister.DefaultPath()
		if err != nil {
			fatal("home dir: %v", err)
		}
		*out = p
	}

	generated, err := wbregister.Build(*panel, apiKey, *timeout)
	if err != nil {
		fatal("%v", err)
	}
	if len(generated) == 0 {
		fatal("panel reported no routed models — nothing to register")
	}

	existing, _, err := wbregister.ReadModelsJSON(*out)
	if err != nil {
		fatal("%v", err)
	}
	merged, added, updated, kept, removed, skipped := wbregister.Merge(existing, generated, *prune)

	fmt.Printf("gateway models: %d | file entries: existing %d → merged %d (add %d, refresh %d, keep %d, skip-user %d, prune %d)\n",
		len(generated), len(existing), len(merged), added, updated, kept, skipped, removed)
	for _, m := range generated {
		fmt.Printf("  %-36s ctx %-8d out %-7d img %-5v efforts %s\n",
			m.ID, m.MaxInputTokens, m.MaxOutputTokens, m.SupportsImages, strings.Join(m.Reasoning.SupportedEfforts, "/"))
	}
	if !*write {
		fmt.Println("\ndry run — pass -write to apply (a timestamped backup is made first).")
		return
	}
	// Reuse Apply's write path so CLI and panel stay byte-identical.
	summary, err := wbregister.Apply(*panel, apiKey, *out, *prune, *timeout)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Printf("\n%s\nWorkBuddy picks it up within a second, no restart needed.\n", summary)
}

// readAPIKey reads the gateway api_key from config.json; an empty key is
// fine (the gateway then skips auth) — WorkBuddy still requires a non-empty
// string in the form, so a neutral placeholder is stored.
func readAPIKey(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "bp-local"
	}
	var cfg struct {
		APIKey string `json:"api_key"`
	}
	if json.Unmarshal(b, &cfg) != nil || cfg.APIKey == "" {
		return "bp-local"
	}
	return cfg.APIKey
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "wbmodels: "+format+"\n", args...)
	os.Exit(1)
}
