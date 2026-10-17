// BP-39 / BP-40: usage rollups carry upstream credit, and the embedded panel
// serves the human-readable duration formatting + the new log columns.
package panel

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/store"
)

func TestUsageAggregatesCreditOnlyWhenKnown(t *testing.T) {
	st := store.NewMemory()
	now := time.Now()
	for _, l := range []*model.RequestLog{
		{Time: now, Model: "glm-5.3", Status: 200, DurationMs: 1000, TTFBMs: 400, OutputTokens: 100,
			Credit: 3.5, CreditKnown: true},
		{Time: now, Model: "glm-5.3", Status: 200, DurationMs: 2000, OutputTokens: 50,
			Credit: 1.25, CreditKnown: true},
		// quota-based channel: no credit reported → must not be counted as 0.
		{Time: now, Model: "gemini-3.8-flash-high", Status: 200, DurationMs: 500, OutputTokens: 10},
	} {
		if err := st.AppendRequestLog(l); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	p := &Panel{store: st}
	rec := httptest.NewRecorder()
	p.handleUsage(rec, httptest.NewRequest("GET", "/panel/api/usage", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		ByModel []usageRow `json:"by_model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byModel := map[string]usageRow{}
	for _, r := range out.ByModel {
		byModel[r.Key] = r
	}
	glm := byModel["glm-5.3"]
	if !glm.CreditKnown || glm.Credit != 4.75 {
		t.Fatalf("glm credit = %v known=%v, want 4.75/true", glm.Credit, glm.CreditKnown)
	}
	gem := byModel["gemini-3.8-flash-high"]
	if gem.CreditKnown || gem.Credit != 0 {
		t.Fatalf("gemini credit = %v known=%v, want 0/false（无上报不得算 0 消耗）", gem.Credit, gem.CreditKnown)
	}
}

// The embedded panel must ship the duration formatter and use it at every
// duration render site (BP-39), plus the attempts/credit/efficiency columns
// (BP-38/BP-40).
func TestEmbeddedPanelShipsNewLogColumnsAndDurationFormat(t *testing.T) {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded panel: %v", err)
	}
	html := string(raw)
	for _, want := range []string{
		"function fmtDur(ms)",
		"fmtDur(avgMs)",         // usage totals card (平均延迟)
		"fmtDur(r.avg_ms)",      // usage detail + daily tables
		"fmtDur(l.ttfb_ms)",     // request log 首字
		"fmtDur(l.duration_ms)", // request log 耗时
		"function fmtCredit(l)",
		"function effTps(l)",
		"attempt_channels",
		`<th class="mono">尝试</th>`,
		`<th class="mono">消耗</th>`,
		`<th class="mono">效率</th>`,
		"上游消耗",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("embedded panel missing %q", want)
		}
	}
	// The old raw-ms renderings must be gone.
	for _, gone := range []string{`avg_ms)}ms`, `ttfb_ms||0)}ms`, `duration_ms||0)}ms`, `avgMs+'ms'`} {
		if strings.Contains(html, gone) {
			t.Fatalf("embedded panel still renders raw ms via %q", gone)
		}
	}
}

// BP-67: 分 (WorkBuddy/Trae) and credits (Qoder) are different currencies.
// The same request_logs.credit column must be split, never summed into one
// "消耗积分" number. Channels whose 200s carry no token counts are reported
// separately (AutoClaw relay).
func TestUsageSeparatesCreditUnitsAndNoTokenChannels(t *testing.T) {
	st := store.NewMemory()
	now := time.Now()
	for _, l := range []*model.RequestLog{
		{Time: now, Model: "glm-5.3", Channel: "workbuddy", Status: 200, Credit: 1.5, CreditKnown: true, InputTokens: 100, OutputTokens: 10},
		{Time: now, Model: "glm-5.3", Channel: "qoder", Status: 200, Credit: 2.25, CreditKnown: true, InputTokens: 100, OutputTokens: 10},
		{Time: now, Model: "glm-5.3-flash", Channel: "autoclaw", Status: 200},
		{Time: now, Model: "glm-5.3-flash", Channel: "autoclaw", Status: 200},
	} {
		if err := st.AppendRequestLog(l); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	p := &Panel{store: st}
	rec := httptest.NewRecorder()
	p.handleUsage(rec, httptest.NewRequest("GET", "/panel/api/usage", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		ByChannel []usageRow `json:"by_channel"`
		NoToken   []string   `json:"no_token_channels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byCh := map[string]usageRow{}
	for _, r := range out.ByChannel {
		byCh[r.Key] = r
	}
	if got := byCh["workbuddy"]; got.CreditPoints != 1.5 || got.CreditCredits != 0 || !got.CreditKnown {
		t.Fatalf("workbuddy = %+v", got)
	}
	if got := byCh["qoder"]; got.CreditPoints != 0 || got.CreditCredits != 2.25 || !got.CreditKnown {
		t.Fatalf("qoder = %+v", got)
	}
	if len(out.NoToken) != 1 || out.NoToken[0] != "autoclaw" {
		t.Fatalf("no_token_channels = %v, want [autoclaw]", out.NoToken)
	}
}

func TestChannelsWithoutTokensIgnoresFailures(t *testing.T) {
	logs := []model.RequestLog{
		{Channel: "autoclaw", Status: 200},
		{Channel: "autoclaw", Status: 200},
		{Channel: "zcode", Status: 429},
		{Channel: "workbuddy", Status: 200, InputTokens: 5},
	}
	got := channelsWithoutTokens(logs)
	if len(got) != 1 || got[0] != "autoclaw" {
		t.Fatalf("got %v, want [autoclaw]", got)
	}
}
