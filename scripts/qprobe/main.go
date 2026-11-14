// Command qprobe prints the raw Qoder quota document for the stored account.
// Diagnostic tool for quota field mapping; it prints quota numbers only (no
// credentials).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
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
	sash := flag.Bool("sash", false, "probe the hub intl candidate matrix (/sash/api/v1/me/*) with a fresh jobToken")
	uid := flag.String("uid", "", "account uid to probe (default: first qoder row)")
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
		if a.Channel != model.ChannelQoder {
			continue
		}
		if *uid != "" && a.UID != *uid {
			continue
		}
		acc = a
		break
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
	if *sash {
		sashTest(ctx, acc)
		return
	}
	doc, err := d.QuotaRaw(ctx, acc)
	if err != nil {
		log.Fatalf("quota: %v", err)
	}
	raw, _ := json.MarshalIndent(doc, "", "  ")
	os.Stdout.Write(raw)
}

// sashTest replays the hub reference's intl candidate matrix: the PAT is
// exchanged into a jobToken (the same bearer the check-in path uses) and the
// /sash/api/v1/me/* candidates are probed on the intl openapi host. Only
// status codes and top-level key names are printed — no personal values.
func sashTest(ctx context.Context, acc *model.Account) {
	cr, err := qoder.ParseCreds(acc.Credentials())
	if err != nil {
		log.Fatalf("creds: %v", err)
	}
	base := "https://openapi.qoder.sh"
	body, _ := json.Marshal(map[string]string{"personal_token": cr.PAT})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/jobToken/exchange", bytes.NewReader(body))
	if err != nil {
		log.Fatalf("req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		log.Fatalf("exchange: %v", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	fmt.Printf("exchange: HTTP %d %s\n", resp.StatusCode, firstLine(raw))
	if resp.StatusCode != 200 {
		return
	}
	var tok struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &tok) != nil || tok.Token == "" {
		log.Fatal("exchange returned no token")
	}
	for _, p := range []string{"/sash/api/v1/me/quota", "/sash/api/v1/me/credits", "/sash/api/v1/me/summary", "/api/v2/quota/usage"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+p, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		r2, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			fmt.Printf("%s: TRANSPORT %v\n", p, err)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(r2.Body, 1<<20))
		r2.Body.Close()
		fmt.Printf("%s: HTTP %d keys=%v\n", p, r2.StatusCode, topKeys(b))
		if r2.StatusCode == 200 && p == "/api/v2/quota/usage" {
			printQuotaNumbers(b)
		}
	}
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

func topKeys(b []byte) []string {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return []string{"(not an object)"}
	}
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func printQuotaNumbers(b []byte) {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return
	}
	if uq, ok := m["userQuota"].(map[string]any); ok {
		fmt.Printf("  userQuota: remaining=%v total=%v used=%v unit=%v\n",
			uq["remaining"], uq["total"], uq["used"], uq["unit"])
	}
	if aq, ok := m["addOnQuota"].(map[string]any); ok {
		fmt.Printf("  addOnQuota: remaining=%v total=%v\n", aq["remaining"], aq["total"])
	}
	fmt.Printf("  userType=%v totalUsagePercentage=%v exceeded=%v\n",
		m["userType"], m["totalUsagePercentage"], m["isQuotaExceeded"])
}
