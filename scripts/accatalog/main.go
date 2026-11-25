// Command accatalog probes the AutoClaw managed model catalog
// (GET /autoclaw-proxy/proxy/autoclaw-model-config) with several header
// shapes, on both lanes, using a real account credential from the local DB.
//
// The endpoint exists on both hosts (401 rather than 404), so the question is
// which header set the gateway accepts — not whether the route is there.
//
// Usage:
//
//	go run ./scripts/accatalog -uid <uid>
//	go run ./scripts/accatalog            # first non-disabled account
package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"bargainproxy/internal/cryptox"
)

const (
	appID = "100003"
	path  = "/autoclaw-proxy/proxy/autoclaw-model-config"
)

var hosts = []struct{ name, base string }{
	{"cn-accel", "https://autoglm-acceleration-api.zhipuai.cn"},
	{"cn-ident", "https://autoglm-api.zhipuai.cn"},
	{"overseas", "https://autoglm-api.autoglm.ai"},
}

type shape struct {
	name string
	// apply sets the headers; tok is the bare token (no "Bearer ").
	apply func(h http.Header, tok, secret string)
}

func main() {
	dsn := flag.String("dsn", "postgres://bargainproxy@127.0.0.1:55432/bargainproxy", "database dsn")
	keyFile := flag.String("key", "data/master.key", "master key file")
	uid := flag.String("uid", "", "account uid (default: first non-disabled)")
	cfgPath := flag.String("config", "config.json", "config.json for the app secret")
	ver := flag.String("version", "2.1.0", "X-Version to claim (the catalog is version-gated)")
	idsOnly := flag.Bool("ids", false, "print model ids only")
	onlyShape := flag.String("shape", "", "run only this shape name")
	onlyHost := flag.String("host", "", "run only this host name")
	chanOverride := flag.String("channel", "", "override X-Channel on every shape")
	rawOut := flag.Bool("raw", false, "print the raw response body (200 only)")
	specs := flag.Bool("specs", false, "print one spec line per model (200 only)")
	flag.Parse()

	secret := readSecret(*cfgPath)
	if secret == "" {
		log.Fatal("no app secret")
	}
	tok, region, gotUID, err := loadCreds(*dsn, *keyFile, *uid)
	if err != nil {
		log.Fatalf("creds: %v", err)
	}
	fmt.Printf("account=%s region=%q token=%d chars\n\n", gotUID, region, len(tok))

	for _, h := range hosts {
		if *onlyHost != "" && h.name != *onlyHost {
			continue
		}
		for _, sh := range shapes() {
			if *onlyShape != "" && sh.name != *onlyShape {
				continue
			}
			status, body, elapsed := get(h.base+path, tok, secret, sh, *ver, *idsOnly, *chanOverride)
			fmt.Printf("%-9s %-28s HTTP %-4d %6s  %s\n",
				h.name, sh.name, status, elapsed.Round(time.Millisecond), trim(body, 220))
			if status == 200 && *specs {
				printSpecs(body)
			} else if status == 200 && *rawOut {
				os.Stdout.WriteString(body)
			} else if status == 200 && !*idsOnly {
				pretty(body)
			}
			time.Sleep(400 * time.Millisecond)
		}
		fmt.Println()
	}
}

func shapes() []shape {
	ts := func() string { return fmt.Sprintf("%d", time.Now().Unix()) }
	sign := func(secret, t string) string {
		sum := md5.Sum([]byte(appID + "&" + t + "&" + secret))
		return hex.EncodeToString(sum[:])
	}
	// biz: the identity-API header family (app secret signature).
	biz := func(h http.Header, tok, secret string) {
		t := ts()
		h.Set("Content-Type", "application/json")
		h.Set("Accept", "*/*")
		h.Set("User-Agent", "AutoClaw/2.1.0 (win-amd64)")
		h.Set("X-Version", "2.1.0")
		h.Set("X-Tm", "win")
		h.Set("X-Product", "autoclaw")
		h.Set("X-Auth-Appid", appID)
		h.Set("X-Auth-TimeStamp", t)
		h.Set("X-Auth-Sign", sign(secret, t))
		h.Set("X-Lang", "zh-CN")
		h.Set("X-Trace-Id", "0123456789abcdef")
		h.Set("Authorization", "Bearer "+tok)
	}
	return []shape{
		// What the probe did before: X-CHANNEL zai + X-Authorization.
		{"ours(zai,X-Authz)", func(h http.Header, tok, secret string) {
			biz(h, tok, secret)
			h.Set("X-Channel", "zai")
			h.Set("X-Client-Type", "pc")
			h.Del("Authorization")
			h.Set("X-Authorization", "Bearer "+tok)
		}},
		// CN managed gateway as magpie's catalogHeaders builds it.
		{"cn(AutoClaw26)", func(h http.Header, tok, secret string) {
			biz(h, tok, secret)
			h.Set("X-Channel", "AutoClaw26")
			h.Del("Origin")
			h.Del("Referer")
		}},
		// Overseas official client.
		{"overseas(official)", func(h http.Header, tok, secret string) {
			biz(h, tok, secret)
			h.Set("X-Version", "2.0.3")
			h.Set("X-Channel", "official")
			h.Set("X-Lang", "en")
			h.Set("User-Agent", "AutoClaw/1.18.1 (win-amd64)")
			h.Del("Origin")
			h.Del("Referer")
		}},
		// Older 1.x client shape (channel "zai" but the 1.18 client version).
		{"1.x(zai,1.18.1)", func(h http.Header, tok, secret string) {
			biz(h, tok, secret)
			h.Set("X-Version", "1.18.1")
			h.Set("X-Channel", "zai")
		}},
	}
}

func get(url, tok, secret string, sh shape, ver string, idsOnly bool, chanOverride string) (int, string, time.Duration) {
	start := time.Now()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err.Error(), 0
	}
	sh.apply(req.Header, tok, secret)
	if ver != "" {
		req.Header.Set("X-Version", ver)
	}
	if chanOverride != "" {
		req.Header.Set("X-Channel", chanOverride)
	}
	resp, err := (&http.Client{Timeout: 25 * time.Second}).Do(req)
	if err != nil {
		return 0, "transport: " + err.Error(), time.Since(start)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if idsOnly && resp.StatusCode == 200 {
		return resp.StatusCode, modelIDs(body), time.Since(start)
	}
	return resp.StatusCode, string(body), time.Since(start)
}

// printSpecs prints one line per model: id, display name, limits, cost, levels.
func printSpecs(body string) {
	var doc struct {
		Models []struct {
			ID             string   `json:"id"`
			Name           string   `json:"name"`
			ContextWindow  int      `json:"contextWindow"`
			MaxTokens      int      `json:"maxTokens"`
			CreditCost     float64  `json:"creditCostFactor"`
			Input          []string `json:"input"`
			ReasoningLevel []string `json:"reasoningLevel"`
			Metadata       struct {
				Gates          []any `json:"gates"`
				ForbiddenNotice any  `json:"forbiddenNotice"`
			} `json:"metadata"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		fmt.Println("  parse failed:", err)
		return
	}
	for _, m := range doc.Models {
		gate := ""
		if len(m.Metadata.Gates) > 0 {
			gate = fmt.Sprintf(" gates=%v", len(m.Metadata.Gates))
		}
		if m.Metadata.ForbiddenNotice != nil {
			gate += " FORBIDDEN_NOTICE"
		}
		fmt.Printf("  %-30s %-18s ctx=%-8d out=%-7d cost=%.2f in=%v lvl=%v%s\n",
			m.ID, m.Name, m.ContextWindow, m.MaxTokens, m.CreditCost, m.Input, m.ReasoningLevel, gate)
	}
}

// modelIDs reduces a catalog body to "id(costFactor)" lines.
func modelIDs(body []byte) string {
	var doc struct {
		Models []struct {
			ID   string  `json:"id"`
			Name string  `json:"name"`
			Cost float64 `json:"creditCostFactor"`
		} `json:"models"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return "unparsed"
	}
	var out []string
	for _, m := range doc.Models {
		out = append(out, fmt.Sprintf("%s [%s] cost=%.2f", m.ID, m.Name, m.Cost))
	}
	return fmt.Sprintf("%d models: %s", len(doc.Models), strings.Join(out, "  "))
}

func pretty(body string) {
	var v any
	if json.Unmarshal([]byte(body), &v) != nil {
		return
	}
	out, _ := json.MarshalIndent(v, "    ", "  ")
	fmt.Printf("    %s\n", strings.ReplaceAll(string(out), "\n", "\n    "))
}

func trim(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func readSecret(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc struct {
		Channels map[string]struct {
			AppSecret string `json:"app_secret"`
		} `json:"channels"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	return strings.TrimSpace(doc.Channels["autoclaw"].AppSecret)
}

func loadCreds(dsn, keyFile, wantUID string) (token, region, uid string, err error) {
	key, err := cryptox.LoadOrCreateKey(keyFile)
	if err != nil {
		return "", "", "", err
	}
	box, err := cryptox.New(key)
	if err != nil {
		return "", "", "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return "", "", "", err
	}
	defer conn.Close(ctx)

	q := `select uid, coalesce(realm,''), creds_enc from accounts where channel='autoclaw' and disabled=false`
	args := []any{}
	if wantUID != "" {
		q += ` and uid=$1`
		args = append(args, wantUID)
	}
	q += ` order by uid limit 1`
	var enc string
	if err := conn.QueryRow(ctx, q, args...).Scan(&uid, &region, &enc); err != nil {
		return "", "", "", err
	}
	plain, err := box.Open(enc)
	if err != nil {
		return "", "", "", fmt.Errorf("decrypt: %w", err)
	}
	var c struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(plain, &c); err != nil {
		return "", "", "", err
	}
	return strings.TrimPrefix(c.AccessToken, "Bearer "), region, uid, nil
}
