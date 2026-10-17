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
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"bargainproxy/internal/config"
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
	flag.Parse()
	if *credsPath == "" {
		log.Fatal("creds required")
	}
	raw, err := os.ReadFile(*credsPath)
	if err != nil {
		log.Fatalf("read creds: %v", err)
	}
	c, err := autoclaw.ParseCreds(raw)
	if err != nil {
		log.Fatalf("parse creds: %v", err)
	}
	cfg := config.Default().Channels.AutoClaw
	acc := &model.Account{ID: "autoclaw:" + c.UID, Channel: model.ChannelAutoClaw, CredsJSON: raw}
	d := autoclaw.New(cfg, nil)
	req := &ir.ChatRequest{
		Model:    *modelName,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: *prompt}}}},
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
