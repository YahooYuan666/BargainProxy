// Command creditprobe prints the credit detail payloads the panel uses for
// AutoClaw (points ledger / expiring) and Qoder (quota breakdown / check-in).
// Diagnostic tool: reads the stored accounts, never prints credentials.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
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
	pages := flag.Int("pages", 1, "autoclaw ledger pages to pull (pageSize 100 each)")
	sum := flag.Bool("sum", false, "sum autoclaw model_usage charges by day across the pulled pages")
	dump := flag.Bool("dump", false, "print one compact line per model_usage entry (time, charged, model, input_tokens)")
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
			if *sum {
				printAutoclawDaySum(ctx, acd, acc, *pages)
			}
			if *dump {
				dumpAutoclawEntries(ctx, acd, acc, *pages)
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

// printAutoclawDaySum pages the points ledger and prints per-day spend totals
// for model_usage entries, plus the running balance each page reports.
func printAutoclawDaySum(ctx context.Context, acd *autoclaw.Driver, acc *model.Account, pages int) {
	if pages < 1 {
		pages = 1
	}
	if pages > 30 {
		pages = 30
	}
	dayTotals := map[string]int64{}
	dayCounts := map[string]int{}
	modelTotals := map[string]int64{}
	var grand int64
	for p := 1; p <= pages; p++ {
		det, err := acd.CreditDetail(ctx, acc, p, 100)
		if err != nil {
			fmt.Printf("page %d: error %v\n", p, err)
			return
		}
		for _, e := range det.Entries {
			if e.Reason != "model_usage" {
				continue
			}
			var amt float64
			fmt.Sscanf(strings.ReplaceAll(e.Charged, ",", ""), "%f", &amt)
			day := e.Time
			if len(day) >= 10 {
				day = day[:10]
			}
			dayTotals[day] += int64(amt)
			dayCounts[day]++
			modelTotals[e.Model] += int64(amt)
			grand += int64(amt)
		}
		if det.Balance != 0 || p == pages {
			fmt.Printf("page %d: balance=%d\n", p, det.Balance)
		}
		if len(det.Entries) < 100 {
			break
		}
	}
	fmt.Println("--- per-day model_usage spend (newest first) ---")
	days := make([]string, 0, len(dayTotals))
	for d := range dayTotals {
		days = append(days, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	for _, d := range days {
		fmt.Printf("%s: %d points / %d requests\n", d, dayTotals[d], dayCounts[d])
	}
	fmt.Println("--- per-model total ---")
	for m, v := range modelTotals {
		fmt.Printf("%s: %d\n", m, v)
	}
	fmt.Printf("grand total over %d page(s): %d points\n", pages, grand)
}

// dumpAutoclawEntries prints one compact line per model_usage entry across the
// pulled pages: time, charged, model, input tokens. Forensics for "where did
// the points go" without pulling the JSON into other tools.
func dumpAutoclawEntries(ctx context.Context, acd *autoclaw.Driver, acc *model.Account, pages int) {
	if pages < 1 {
		pages = 1
	}
	if pages > 30 {
		pages = 30
	}
	for p := 1; p <= pages; p++ {
		det, err := acd.CreditDetail(ctx, acc, p, 100)
		if err != nil {
			fmt.Printf("page %d: error %v\n", p, err)
			return
		}
		for _, e := range det.Entries {
			if e.Reason != "model_usage" {
				continue
			}
			input := ""
			if i := strings.Index(e.TokenUsage, "input_tokens:"); i >= 0 {
				input = e.TokenUsage[i+len("input_tokens:"):]
				if c := strings.Index(input, ","); c >= 0 {
					input = input[:c]
				}
			}
			fmt.Printf("%s,%s,%s,%s\n", e.Time, e.Charged, e.Model, input)
		}
		if len(det.Entries) < 100 {
			break
		}
	}
}
