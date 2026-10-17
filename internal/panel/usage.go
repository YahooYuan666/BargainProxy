// Usage statistics: aggregate request-log metadata into per-model / per-account
// / per-channel rollups for the panel. Metadata only — no prompt content.
package panel

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"bargainproxy/internal/model"
)

type usageRow struct {
	Key          string `json:"key"`
	Requests     int    `json:"requests"`
	OK           int    `json:"ok"`
	Failed       int    `json:"failed"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	AvgMs        int64  `json:"avg_ms"`
	// GenMs is the summed generation window (duration minus first-token wait
	// where measurable) — the honest denominator for throughput (ticket 29).
	GenMs int64 `json:"gen_ms"`
	// Credit sums upstream-reported credit consumption (workbuddy usage.credit);
	// CreditKnown distinguishes "no data" channels (quota-based) from zero
	// spend — BP-40. Points and credits are different currencies and must not
	// be added together: CreditPoints (分: workbuddy/trae) and CreditCredits
	// (Qoder) are tracked separately, Credit remains their arithmetic sum for
	// older consumers.
	Credit        float64 `json:"credit"`
	CreditKnown   bool    `json:"credit_known"`
	CreditPoints  float64 `json:"credit_points"`
	CreditCredits float64 `json:"credit_credits"`
	// Prefix-cache accounting (BP-53, agentpoints issue #92 parity): the hit
	// sum and the prompt-token sum of the requests that actually reported
	// cache data — the rate is CachedTokens/CacheKnownTokens, and the second
	// field keeps 0-hit samples (first turns) in the denominator.
	CachedTokens     int64 `json:"cached_tokens,omitempty"`
	CacheKnownTokens int64 `json:"cache_known_tokens,omitempty"`
	CacheKnown       bool  `json:"cache_known,omitempty"`
}

// minGenWindowMs: a remainder shorter than this after subtracting the
// first-token wait is not a measurable generation window (fake streaming /
// batched delivery — first and last frames nearly simultaneous). Fall back
// to end-to-end instead of dividing by a few milliseconds (agentpoints #127
// parity).
const minGenWindowMs = 200

// genMsOf returns the generation window for one request: total duration minus
// the measured first-token wait when at least minGenWindowMs remain; missing
// TTFB observation (upstream paths that don't record it) or a sub-window
// remainder fall back to end-to-end.
func genMsOf(l model.RequestLog) int64 {
	d := int64(l.DurationMs)
	if d <= 0 {
		return 0
	}
	if l.TTFBMs > 0 {
		if g := d - int64(l.TTFBMs); g >= minGenWindowMs {
			return g
		}
	}
	return d
}

// handleUsage summarizes the recent request logs into three dimensions plus
// a daily series. Query params: limit (rows scanned, default 2000),
// since_hours (window filter, 0 = all loaded rows). Read-only.
func (p *Panel) handleUsage(w http.ResponseWriter, r *http.Request) {
	limit := 2000
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 20000 {
			limit = n
		}
	}
	sinceHours := 0
	if v := r.URL.Query().Get("since_hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 24*365 {
			sinceHours = n
		}
	}
	logs, err := p.store.RecentRequestLogs(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if sinceHours > 0 {
		cutoff := time.Now().Add(-time.Duration(sinceHours) * time.Hour)
		filtered := logs[:0]
		for _, l := range logs {
			if l.Time.After(cutoff) {
				filtered = append(filtered, l)
			}
		}
		logs = filtered
	}
	byModel, byAccount, byChannel := map[string]*usageRow{}, map[string]*usageRow{}, map[string]*usageRow{}
	days := map[string]*usageRow{}
	ensure := func(m map[string]*usageRow, k string) *usageRow {
		if r, ok := m[k]; ok {
			return r
		}
		r := &usageRow{Key: k}
		m[k] = r
		return r
	}
	for _, l := range logs {
		for _, row := range []*usageRow{
			ensure(byModel, l.Model), ensure(byAccount, accountKey(l)), ensure(byChannel, chanKey(l)),
			ensure(days, l.Time.Format("01-02")),
		} {
			row.Requests++
			if l.Status == 200 {
				row.OK++
			} else {
				row.Failed++
			}
			row.InputTokens += int64(l.InputTokens)
			row.OutputTokens += int64(l.OutputTokens)
			row.AvgMs += int64(l.DurationMs)
			row.GenMs += genMsOf(l)
			if l.CreditKnown {
				row.Credit += l.Credit
				if l.Channel == model.ChannelQoder {
					row.CreditCredits += l.Credit
				} else {
					row.CreditPoints += l.Credit
				}
				row.CreditKnown = true
			}
			if l.CacheKnown {
				hit := int64(l.CachedTokens)
				if hit > int64(l.InputTokens) {
					hit = int64(l.InputTokens) // clamp upstream noise
				}
				row.CachedTokens += hit
				row.CacheKnownTokens += int64(l.InputTokens)
				row.CacheKnown = true
			}
		}
	}
	finalize := func(m map[string]*usageRow) []usageRow {
		out := make([]usageRow, 0, len(m))
		for _, r := range m {
			if r.Requests > 0 {
				r.AvgMs /= int64(r.Requests)
			}
			out = append(out, *r)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
		return out
	}
	// Daily series stays chronological.
	dayKeys := make([]string, 0, len(days))
	for k := range days {
		dayKeys = append(dayKeys, k)
	}
	sort.Strings(dayKeys)
	daily := make([]usageRow, 0, len(dayKeys))
	for _, k := range dayKeys {
		r := days[k]
		if r.Requests > 0 {
			r.AvgMs /= int64(r.Requests)
		}
		daily = append(daily, *r)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"window":     usageWindow(logs),
		"by_model":   finalize(byModel),
		"by_account": finalize(byAccount),
		"by_channel": finalize(byChannel),
		"daily":      daily,
		"series":     usageSeries(logs, sinceHours),
		// Channels whose successful requests never carry token counts. AutoClaw's
		// relay does not stream usage; the numbers are real 0s upstream-side, but
		// they must not read as "this channel used no tokens".
		"no_token_channels": channelsWithoutTokens(logs),
		"credit_units":      "分=WorkBuddy/Trae，credits=Qoder；两者不可相加",
	})
}

// channelsWithoutTokens reports channels whose 200 responses all have a zero
// token total — a telemetry gap, not an idle channel.
func channelsWithoutTokens(logs []model.RequestLog) []string {
	type stat struct{ ok, withTokens int }
	m := map[string]*stat{}
	for _, l := range logs {
		if l.Status != 200 || l.Channel == "" {
			continue
		}
		s := m[l.Channel]
		if s == nil {
			s = &stat{}
			m[l.Channel] = s
		}
		s.ok++
		if l.InputTokens+l.OutputTokens > 0 {
			s.withTokens++
		}
	}
	out := make([]string, 0, len(m))
	for ch, s := range m {
		// One zero-token success can be an upstream empty completion; a real
		// telemetry gap shows up across several successful requests.
		if s.ok >= 2 && s.withTokens == 0 {
			out = append(out, ch)
		}
	}
	sort.Strings(out)
	return out
}

// usageSeriesPoint is one time bucket for the token chart. t is local time:
// "2006-01-02T15" for hour scope, "2006-01-02" for day scope — the frontend
// draws a REAL time axis, so gaps between buckets must be visible, not
// equalized (agentpoints parity, BP-52).
type usageSeriesPoint struct {
	T                string `json:"t"`
	Scope            string `json:"scope"`
	Requests         int    `json:"requests"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
}

// usageSeries buckets the loaded logs for the chart. Granularity mirrors the
// convention the reference panel shows by default: hourly for windows up to
// 3 days (its default preset) — and for "all time" when the data itself spans
// at most 3 days — daily beyond. Only buckets with requests are emitted (the
// real time axis shows the gaps); the tail is capped so a very long history
// stays renderable.
func usageSeries(logs []model.RequestLog, sinceHours int) []usageSeriesPoint {
	const hourWindowHours = 72
	scope := "day"
	switch {
	case sinceHours > 0 && sinceHours <= hourWindowHours:
		scope = "hour"
	case sinceHours == 0 && len(logs) > 1:
		lo, hi := logs[0].Time, logs[0].Time
		for _, l := range logs[1:] {
			if l.Time.Before(lo) {
				lo = l.Time
			}
			if l.Time.After(hi) {
				hi = l.Time
			}
		}
		if hi.Sub(lo) <= hourWindowHours*time.Hour {
			scope = "hour"
		}
	}
	buckets := map[string]*usageSeriesPoint{}
	var keys []string
	for _, l := range logs {
		key := l.Time.Format("2006-01-02")
		if scope == "hour" {
			key = l.Time.Format("2006-01-02T15")
		}
		pt, ok := buckets[key]
		if !ok {
			pt = &usageSeriesPoint{T: key, Scope: scope}
			buckets[key] = pt
			keys = append(keys, key)
		}
		pt.Requests++
		pt.PromptTokens += int64(l.InputTokens)
		pt.CompletionTokens += int64(l.OutputTokens)
	}
	sort.Strings(keys)
	out := make([]usageSeriesPoint, 0, len(keys))
	for _, k := range keys {
		pt := buckets[k]
		pt.TotalTokens = pt.PromptTokens + pt.CompletionTokens
		out = append(out, *pt)
	}
	if len(out) > 400 {
		out = out[len(out)-400:]
	}
	return out
}

func accountKey(l model.RequestLog) string {
	if l.AccountLabel != "" {
		return l.AccountLabel
	}
	if l.AccountID != "" {
		return l.AccountID
	}
	return "—"
}

func chanKey(l model.RequestLog) string {
	if l.Channel != "" {
		return l.Channel
	}
	return "—"
}

func usageWindow(logs []model.RequestLog) string {
	if len(logs) == 0 {
		return "无数据"
	}
	newest, oldest := logs[0].Time, logs[len(logs)-1].Time
	for _, l := range logs {
		if l.Time.After(newest) {
			newest = l.Time
		}
		if l.Time.Before(oldest) {
			oldest = l.Time
		}
	}
	return oldest.Format("01-02 15:04") + " ~ " + newest.Format("01-02 15:04")
}
