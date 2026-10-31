// zgift probe: run the BP-231 gift-plan claim OUTSIDE the gateway (background
// context, no client-side abort) and print the raw per-account outcome, so a
// gateway-side hang is distinguishable from a driver bug.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/cryptox"
	"bargainproxy/internal/driver/zcode"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/model"
	"bargainproxy/internal/store"
)

func main() {
	realMain()
}

func realMain() {
	cfg, err := config.Load("config.json")
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	key, err := cryptox.LoadOrCreateKey(cfg.KeyFile)
	if err != nil {
		log.Fatalf("key: %v", err)
	}
	box, err := cryptox.New(key)
	if err != nil {
		log.Fatalf("box: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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
	fmt.Printf("rpc timeout: %s\n", httpx.RPCTimeout())
	rawDump(ctx, cfg, st)
	d := zcode.New(cfg.Channels.ZCode, nil)
	n := 0
	for _, a := range accounts {
		if a.Channel != model.ChannelZCode {
			continue
		}
		n++
		start := time.Now()
		res, err := d.ClaimGiftPlans(ctx, a)
		elapsed := time.Since(start).Round(time.Millisecond)
		if err != nil {
			fmt.Printf("[%s] %s ERROR after %s: %v\n", a.ID, a.Nickname, elapsed, err)
			continue
		}
		fmt.Printf("[%s] %s ok after %s: claimed=%d already=%d captcha=%d failed=%d\n",
			a.ID, a.Nickname, elapsed, res.Claimed, res.Already, res.Captcha, res.Failed)
		for _, note := range res.Notes {
			fmt.Printf("    - %s\n", note)
		}
	}
	if n == 0 {
		log.Fatalf("no zcode account in store")
	}
}

func rawDump(ctx context.Context, cfg *config.Config, st store.Store) {
	accounts, _ := st.LoadAccounts()
	var acc *model.Account
	for _, a := range accounts {
		if a.Channel == model.ChannelZCode {
			acc = a
			break
		}
	}
	if acc == nil {
		fmt.Println("raw dump: no zcode account")
		return
	}
	creds, err := zcode.ParseCreds(acc.CredsJSON)
	if err != nil {
		fmt.Println("raw dump creds:", err)
		return
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cfg.Channels.ZCode.BaseURL+"/api/v1/zcode-plan/billing/preview?app_version=3.16.3&platform=win32-x64", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ZCode/3.16.3")
	req.Header.Set("Authorization", "Bearer "+creds.JWT)
	req.Header.Set("X-Device-Mid", zcode.GiftDeviceMid(creds, acc.ID))
	req.Header.Set("X-ZCode-App-Version", "3.16.3")
	req.Header.Set("X-Platform", "win32-x64")
	resp, err := httpx.RPCClient().Do(req)
	if err != nil {
		fmt.Println("raw dump transport:", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	fmt.Printf("RAW GET status=%d\n", resp.StatusCode)
	for k, v := range resp.Header {
		if k == "Server" || strings.HasPrefix(k, "X") || k == "Content-Type" || k == "Date" {
			fmt.Printf("  %s: %v\n", k, v)
		}
	}
	fmt.Printf("  body: %.600s\n", body)
}
