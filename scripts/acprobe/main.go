// Command acprobe sends one chat request through the AutoClaw driver using a
// credential file from the reference project's auths directory. Diagnostic
// tool: it bypasses the gateway pool and shows the driver's real upstream
// behaviour (proxy channel first, sandbox fallback).
//
// Usage:
//
//	go run ./scripts/acprobe -creds "D:/Projects/agentpoints/autoclaw2api/auths/autoclaw-*.json" -model glm-5.3-flash
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"bargainproxy/internal/config"
	"bargainproxy/internal/cryptox"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/autoclaw"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

func main() {
	credsPath := flag.String("creds", "", "AutoClaw credential file (reference auths/*.json)")
	modelName := flag.String("model", "glm-5.3-flash", "upstream model id")
	prompt := flag.String("prompt", "Reply with exactly: ok", "user prompt")
	uid := flag.String("uid", "", "load creds from the local DB account with this uid (overrides -creds)")
	system := flag.String("system", "", "extra system message prepended before the user turn (prompt-gate bisect)")
	flag.Parse()
	var raw []byte
	var err error
	if *uid != "" {
		raw, err = loadCredsFromDB(*uid)
		if err != nil {
			log.Fatalf("creds from db: %v", err)
		}
	} else {
		if *credsPath == "" {
			log.Fatal("creds required (or -uid)")
		}
		raw, err = os.ReadFile(*credsPath)
		if err != nil {
			log.Fatalf("read creds: %v", err)
		}
	}
	c, err := autoclaw.ParseCreds(raw)
	if err != nil {
		log.Fatalf("parse creds: %v", err)
	}
	cfg, err := config.Load("config.json")
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	ac := cfg.Channels.AutoClaw
	acc := &model.Account{ID: "autoclaw:" + c.UID, Channel: model.ChannelAutoClaw, CredsJSON: raw}
	d := autoclaw.New(ac, nil)
	msgs := []ir.Message{}
	if *system != "" {
		msgs = append(msgs, ir.Message{Role: ir.RoleSystem, Content: []ir.ContentPart{{Kind: "text", Text: *system}}})
	}
	msgs = append(msgs, ir.Message{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: *prompt}}})
	req := &ir.ChatRequest{
		Model:    *modelName,
		Messages: msgs,
		Stream:   true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	start := time.Now()
	var text []rune
	resp, err := d.Stream(ctx, driver.ChatCall{Account: acc, UpstreamModel: *modelName, Request: req}, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvText {
			text = append(text, []rune(ev.Delta)...)
		}
		return nil
	})
	if err != nil {
		fmt.Printf("ERROR after %s: %v\n", time.Since(start).Round(time.Millisecond), err)
		if ue, ok := uperr.As(err); ok {
			fmt.Printf("class=%s status=%d\n", ue.Class, ue.Status)
		}
		os.Exit(1)
	}
	fmt.Printf("OK %s stop=%s usage=%+v text=%q\n", time.Since(start).Round(time.Millisecond), resp.StopReason, resp.Usage, string(text))
}

// loadCredsFromDB decrypts the live autoclaw account row, same envelope the
// gateway uses, so a probe rides the same fresh token the driver refreshed.
func loadCredsFromDB(uid string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, "postgres://bargainproxy@127.0.0.1:55432/bargainproxy")
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	var enc string
	if err := conn.QueryRow(ctx,
		"SELECT creds_enc FROM accounts WHERE channel='autoclaw' AND disabled=false AND uid=$1", uid).Scan(&enc); err != nil {
		return nil, err
	}
	key, err := cryptox.LoadOrCreateKey("data/master.key")
	if err != nil {
		return nil, err
	}
	box, err := cryptox.New(key)
	if err != nil {
		return nil, err
	}
	plain, err := box.Open(enc)
	if err != nil {
		return nil, err
	}
	// The creds row carries access/refresh tokens; hand the driver the same
	// JSON it would read from a creds file.
	var pretty map[string]any
	if json.Unmarshal(plain, &pretty) != nil {
		return plain, nil
	}
	return plain, nil
}
