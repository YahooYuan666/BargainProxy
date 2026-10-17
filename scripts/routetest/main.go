// Command routetest exercises every configured route binding through the
// local gateway, one binding at a time (pinned with the channel: prefix), and
// reports first-token latency, output token rate and the channel's credit /
// quota delta. Two turns per binding: a long generation and a short follow-up
// that proves multi-turn context.
//
// Usage:
//
//	go run ./scripts/routetest -gateway http://127.0.0.1:13140 -out routes.json
package main

import (
	"bufio"
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
	"sync"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/cryptox"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/antigravity"
	"bargainproxy/internal/driver/autoclaw"
	"bargainproxy/internal/driver/opencodezen"
	"bargainproxy/internal/driver/qoder"
	"bargainproxy/internal/driver/trae"
	"bargainproxy/internal/driver/workbuddy"
	"bargainproxy/internal/driver/zcode"
	"bargainproxy/internal/model"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
)

const longPrompt = "请写一篇约800字的技术说明，主题是：自建模型网关如何在不稳定的免费上游之间做路由与降级。要求分四小节：1) 渠道健康与冷却 2) 会话粘性与缓存 3) 失败级联与重试 4) 成本与额度核算。每节给出具体做法和常见坑，语言务实、不要客套。"

type turnResult struct {
	Status       int     `json:"status"`
	Error        string  `json:"error,omitempty"`
	TTFBMs       int64   `json:"ttfb_ms"`       // first delta of any kind
	FirstTextMs  int64   `json:"first_text_ms"` // first visible text
	DurationMs   int64   `json:"duration_ms"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CachedTokens int     `json:"cached_tokens,omitempty"`
	TokPerSec    float64 `json:"tok_per_sec"`
	FinishReason string  `json:"finish_reason,omitempty"`
	TextChars    int     `json:"text_chars"`
	Text         string  `json:"text,omitempty"`
}

type snapshot struct {
	Channel string             `json:"channel"`
	OK      bool               `json:"ok"`
	Error   string             `json:"error,omitempty"`
	Remain  int64              `json:"remain"`
	Total   int64              `json:"total"`
	Note    string             `json:"note,omitempty"`
	Windows map[string]float64 `json:"windows,omitempty"`
}

type result struct {
	Model      string     `json:"model"`
	Channel    string     `json:"channel"`
	Pinned     string     `json:"pinned"`
	Before     snapshot   `json:"before"`
	Turn1      turnResult `json:"turn1"`
	Turn2      turnResult `json:"turn2"`
	After      snapshot   `json:"after"`
	CreditUsed int64      `json:"credit_used"`
	Continuity bool       `json:"continuity"`
	Note       string     `json:"note,omitempty"`
}

func main() {
	gw := flag.String("gateway", "http://127.0.0.1:13140", "gateway base url")
	cfgPath := flag.String("config", "config.json", "path to config.json")
	outPath := flag.String("out", "logs/routetest.json", "result json path")
	includeZen := flag.Bool("include-zen", false, "also test opencodezen bindings")
	only := flag.String("only", "", "comma-separated model filter")
	chans := flag.String("channels", "", "comma-separated channel filter")
	maxTokens := flag.Int("max-tokens", 1200, "turn-1 max output tokens")
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
	if len(rows) == 0 {
		log.Fatal("no model_routes in settings")
	}
	accounts, _ := st.LoadAccounts()
	byChan := map[string][]*model.Account{}
	for _, a := range accounts {
		byChan[a.Channel] = append(byChan[a.Channel], a)
	}
	filter := map[string]bool{}
	if *only != "" {
		for _, m := range strings.Split(*only, ",") {
			filter[strings.TrimSpace(m)] = true
		}
	}
	chanFilter := map[string]bool{}
	if *chans != "" {
		for _, c := range strings.Split(*chans, ",") {
			chanFilter[strings.TrimSpace(c)] = true
		}
	}

	h := &harness{gw: strings.TrimRight(*gw, "/"), cfg: cfg, accounts: byChan, hc: &http.Client{Timeout: 240 * time.Second}}
	var results []result
	for _, row := range rows {
		if row.Channel == model.ChannelOpenCodeZen && !*includeZen {
			continue
		}
		if len(filter) > 0 && !filter[row.Model] {
			continue
		}
		if len(chanFilter) > 0 && !chanFilter[row.Channel] {
			continue
		}
		if len(byChan[row.Channel]) == 0 {
			fmt.Printf("skip %s/%s: no accounts\n", row.Model, row.Channel)
			continue
		}
		fmt.Printf("== %s @ %s ...\n", row.Model, row.Channel)
		r := h.run(row, *maxTokens)
		results = append(results, r)
		if r.Turn1.Error != "" || r.Turn2.Error != "" {
			fmt.Printf("   fail: %s %s\n", r.Turn1.Error, r.Turn2.Error)
		} else {
			fmt.Printf("   t1 first=%dms %.1f tok/s; t2 first=%dms\n", r.Turn1.FirstTextMs, r.Turn1.TokPerSec, r.Turn2.FirstTextMs)
		}
	}
	raw, _ := json.MarshalIndent(results, "", "  ")
	if err := os.WriteFile(*outPath, raw, 0o600); err != nil {
		log.Printf("write %s: %v", *outPath, err)
	}
	printTable(results)
}

type harness struct {
	gw       string
	cfg      *config.Config
	accounts map[string][]*model.Account
	hc       *http.Client
	mu       sync.Mutex
}

func (h *harness) run(row router.Row, maxTokens int) result {
	res := result{Model: row.Model, Channel: row.Channel, Pinned: "channel:" + row.Channel + ":" + row.Model}
	res.Before = h.snapshot(row.Channel)
	msgs := []map[string]any{{"role": "user", "content": longPrompt}}
	res.Turn1 = h.chat(res.Pinned, msgs, maxTokens)
	if res.Turn1.Error == "" && strings.TrimSpace(res.Turn1.Text) != "" {
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": res.Turn1.Text},
			map[string]any{"role": "user", "content": "请用一句话总结你刚才的回答，不超过40字。"},
		)
		res.Turn2 = h.chat(res.Pinned, msgs, 200)
	}
	res.After = h.snapshot(row.Channel)
	if res.Before.OK && res.After.OK && res.Before.Remain > 0 {
		res.CreditUsed = res.Before.Remain - res.After.Remain
	}
	if res.Turn2.Error == "" {
		// A crude but honest continuity check: the summary should mention a
		// concept from the first answer (routing, 降级, 冷却, 缓存…).
		joined := res.Turn2.Text
		for _, kw := range []string{"路由", "降级", "冷却", "缓存", "渠道", "网关", "额度"} {
			if strings.Contains(joined, kw) {
				res.Continuity = true
				break
			}
		}
	}
	return res
}

func (h *harness) chat(modelName string, msgs []map[string]any, maxTok int) turnResult {
	tr := turnResult{}
	body, _ := json.Marshal(map[string]any{
		"model": modelName, "messages": msgs, "stream": true, "max_tokens": maxTok,
		"stream_options": map[string]any{"include_usage": true},
	})
	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, h.gw+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		tr.Error = err.Error()
		return tr
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.hc.Do(req)
	if err != nil {
		tr.Error = err.Error()
		return tr
	}
	defer resp.Body.Close()
	tr.Status = resp.StatusCode
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		tr.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(raw))
		return tr
	}
	var (
		buf     strings.Builder
		firstAt time.Time
		textAt  time.Time
		lastAt  time.Time
		usage   struct {
			PromptTokens    int `json:"prompt_tokens"`
			Completion      int `json:"completion_tokens"`
			PromptTokensDet *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		}
	)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens        int `json:"prompt_tokens"`
				CompletionTokens    int `json:"completion_tokens"`
				PromptTokensDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.ReasoningContent != "" || ch.Delta.Content != "" {
				now := time.Now()
				if firstAt.IsZero() {
					firstAt = now
					tr.TTFBMs = now.Sub(start).Milliseconds()
				}
				lastAt = now
			}
			if ch.Delta.Content != "" {
				if textAt.IsZero() {
					textAt = time.Now()
					tr.FirstTextMs = textAt.Sub(start).Milliseconds()
				}
				buf.WriteString(ch.Delta.Content)
			}
			if ch.FinishReason != "" {
				tr.FinishReason = ch.FinishReason
			}
		}
		if chunk.Usage != nil {
			usage.PromptTokens = chunk.Usage.PromptTokens
			usage.Completion = chunk.Usage.CompletionTokens
			if chunk.Usage.PromptTokensDetails != nil {
				usage.PromptTokensDet = chunk.Usage.PromptTokensDetails
			}
		}
	}
	tr.DurationMs = time.Since(start).Milliseconds()
	tr.Text = buf.String()
	tr.TextChars = len([]rune(tr.Text))
	tr.InputTokens = usage.PromptTokens
	tr.OutputTokens = usage.Completion
	if usage.PromptTokensDet != nil {
		tr.CachedTokens = usage.PromptTokensDet.CachedTokens
	}
	if tr.OutputTokens > 0 && !firstAt.IsZero() && lastAt.After(firstAt) {
		tr.TokPerSec = float64(tr.OutputTokens) / lastAt.Sub(firstAt).Seconds()
	}
	return tr
}

func (h *harness) snapshot(channel string) snapshot {
	s := snapshot{Channel: channel}
	drv := h.driver(channel)
	if drv == nil {
		s.Error = "no driver"
		return s
	}
	prober, ok := drv.(driver.Prober)
	if !ok {
		s.Error = "no prober"
		return s
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	s.Windows = map[string]float64{}
	var firstErr string
	for _, a := range h.accounts[channel] {
		res, err := prober.Probe(ctx, a)
		if err != nil {
			if firstErr == "" {
				firstErr = err.Error()
			}
			continue
		}
		s.OK = true
		if res.Credits != nil {
			s.Remain += res.Credits.Remain
			s.Total += res.Credits.Total
		}
		for k, w := range res.Windows {
			s.Windows[k] += w.RemainingFraction
		}
		if res.Note != "" {
			s.Note = res.Note
		}
	}
	if n := float64(len(h.accounts[channel])); n > 0 {
		for k := range s.Windows {
			s.Windows[k] /= n
		}
	}
	if !s.OK {
		s.Error = firstErr
	}
	return s
}

func (h *harness) driver(channel string) driver.Driver {
	c := h.cfg.Channels
	switch channel {
	case model.ChannelAntigravity:
		return antigravity.New(c.Antigravity, nil)
	case model.ChannelWorkBuddy:
		return workbuddy.New(c.WorkBuddy, nil)
	case model.ChannelTrae:
		return trae.New(c.Trae, nil)
	case model.ChannelZCode:
		return zcode.New(c.ZCode, nil)
	case model.ChannelAutoClaw:
		return autoclaw.New(c.AutoClaw, nil)
	case model.ChannelQoder:
		return qoder.New(c.Qoder, nil)
	case model.ChannelOpenCodeZen:
		return opencodezen.New(c.OpenCodeZen, nil)
	}
	return nil
}

func printTable(results []result) {
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Model == results[j].Model {
			return results[i].Channel < results[j].Channel
		}
		return results[i].Model < results[j].Model
	})
	fmt.Printf("\n| model | channel | t1 首字(ms) | t1 tok/s | t1 out tok | t2 首字(ms) | 连续 | 积分差 | 备注 |\n")
	fmt.Printf("|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range results {
		note := r.Note
		if r.Turn1.Error != "" {
			note = "FAIL: " + trunc(r.Turn1.Error, 60)
		} else if r.Turn2.Error != "" {
			note = "turn2 fail: " + trunc(r.Turn2.Error, 50)
		} else if !r.Continuity {
			note = "summary lacks context keywords"
		}
		fmt.Printf("| %s | %s | %d | %.1f | %d | %d | %v | %d | %s |\n",
			r.Model, r.Channel, r.Turn1.FirstTextMs, r.Turn1.TokPerSec, r.Turn1.OutputTokens,
			r.Turn2.FirstTextMs, r.Continuity, r.CreditUsed, note)
	}
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
