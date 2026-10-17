// 国模积分情况 (credit overview + WorkBuddy pack composition).
//
// Two layers:
//   - overview: every domestic credit pool's current balance in one table
//     (WorkBuddy packs, Trae points, ZCode plan, AutoClaw points, Qoder
//     credits). Values are probed live, bounded concurrency.
//   - accounts: the WorkBuddy per-pack detail used by the expiry reminders and
//     composition bars (the only pool with per-pack expiry data).
package panel

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/model"
)

// contextWithTimeout derives from the request context (cancels on disconnect).
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

type packageAccountView struct {
	ID       string           `json:"id"`
	Nickname string           `json:"nickname,omitempty"`
	Remain   int64            `json:"remain"`
	Size     int64            `json:"size"`
	Packages []driver.Package `json:"packages,omitempty"`
	Error    string           `json:"error,omitempty"`
}

// cleanProbeError turns noisy upstream errors (e.g. raw nginx 401 HTML
// bodies from APISIX) into human-readable one-liners suitable for panel
// cards and table cells. It strips HTML tags, collapsing whitespace, and
// maps common HTTP codes to plain explanations (BP-71).
func cleanProbeError(s string) string {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	switch {
	case strings.Contains(lower, "401") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "authorization required"):
		return "401 凭据失效（需重新登录）"
	case strings.Contains(lower, "403") || strings.Contains(lower, "forbidden"):
		return "403 权限不足或被风控拦截"
	case strings.Contains(lower, "429") || strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests"):
		return "429 频率受限 / 额度耗尽"
	case strings.Contains(lower, "502") || strings.Contains(lower, "bad gateway"):
		return "502 上游网关错误"
	case strings.Contains(lower, "503") || strings.Contains(lower, "service unavailable"):
		return "503 上游服务暂不可用"
	case strings.Contains(lower, "504") || strings.Contains(lower, "gateway timeout"):
		return "504 上游响应超时"
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded"):
		return "探测超时（上游未在时限内响应）"
	}
	// Strip HTML if any remains.
	if strings.Contains(s, "<") && strings.Contains(s, ">") {
		var b strings.Builder
		inTag := false
		for _, r := range s {
			if r == '<' {
				inTag = true
				continue
			}
			if r == '>' {
				inTag = false
				continue
			}
			if !inTag {
				b.WriteRune(r)
			}
		}
		s = strings.Join(strings.Fields(b.String()), " ")
	}
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	if s == "" {
		return "探测失败"
	}
	return s
}

// creditOverviewRow is one domestic pool/account at a glance. Kind tells the
// UI how to render the numbers: packs/points/credits/plan.
type creditOverviewRow struct {
	Channel     string    `json:"channel"`
	ID          string    `json:"id"`
	Nickname    string    `json:"nickname,omitempty"`
	Remain      int64     `json:"remain"`
	Total       int64     `json:"total"`
	Bonus       int64     `json:"bonus,omitempty"`
	Expiring    int64     `json:"expiring,omitempty"`
	BalanceOnly bool      `json:"balance_only,omitempty"`
	Kind        string    `json:"kind"`
	Note        string    `json:"note,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
	Error       string    `json:"error,omitempty"`
}

var domesticCreditChannels = []string{
	model.ChannelWorkBuddy, model.ChannelTrae, model.ChannelZCode,
	model.ChannelAutoClaw, model.ChannelQoder,
}

func creditKind(channel string) string {
	switch channel {
	case model.ChannelWorkBuddy:
		return "packs"
	case model.ChannelAutoClaw:
		return "points"
	case model.ChannelQoder:
		return "credits"
	case model.ChannelZCode:
		return "plan"
	default:
		return "points"
	}
}

// handlePackages probes every domestic credit pool and returns the overview
// plus the WorkBuddy pack detail used by the expiry view.
func (p *Panel) handlePackages(w http.ResponseWriter, r *http.Request) {
	pl := p.gw.Pool()
	pl.Lock()
	byChannel := map[string][]*model.Account{}
	for _, ch := range domesticCreditChannels {
		byChannel[ch] = pl.AccountsLocked(ch)
	}
	pl.Unlock()

	wbAccounts := byChannel[model.ChannelWorkBuddy]
	out := make([]packageAccountView, len(wbAccounts))
	sem := make(chan struct{}, 3)
	done := make(chan int, len(wbAccounts))
	for i, acc := range wbAccounts {
		go func(i int, acc *model.Account) {
			sem <- struct{}{}
			defer func() { <-sem }()
			v := packageAccountView{ID: acc.ID, Nickname: acc.Nickname}
			prober, ok := p.gw.Driver(model.ChannelWorkBuddy).(driver.PackagesProber)
			if !ok {
				v.Error = "packages not supported"
				out[i] = v
				done <- i
				return
			}
			ctx, cancel := contextWithTimeout(r, 30*time.Second)
			defer cancel()
			packs, err := prober.CreditPackages(ctx, acc)
			if err != nil {
				v.Error = cleanProbeError(err.Error())
				out[i] = v
				done <- i
				return
			}
			sort.Slice(packs, func(a, b int) bool { return packs[a].ExpireAt.Before(packs[b].ExpireAt) })
			for _, pk := range packs {
				v.Remain += pk.Remain
				v.Size += pk.Size
			}
			v.Packages = packs
			out[i] = v
			done <- i
		}(i, acc)
	}
	for range wbAccounts {
		<-done
	}

	overview := make([]creditOverviewRow, 0, 8)
	for i, acc := range wbAccounts {
		v := out[i]
		row := creditOverviewRow{
			Channel: model.ChannelWorkBuddy, ID: acc.ID, Nickname: acc.Nickname,
			Remain: v.Remain, Total: v.Size, Kind: creditKind(model.ChannelWorkBuddy),
			Error: v.Error,
		}
		if v.Error == "" {
			row.UpdatedAt = time.Now()
		}
		overview = append(overview, row)
	}

	type probeResult struct {
		row creditOverviewRow
	}
	other := make([]*model.Account, 0, 8)
	for _, ch := range domesticCreditChannels {
		if ch == model.ChannelWorkBuddy {
			continue
		}
		other = append(other, byChannel[ch]...)
	}
	results := make(chan probeResult, len(other))
	for _, acc := range other {
		go func(acc *model.Account) {
			sem <- struct{}{}
			defer func() { <-sem }()
			row := creditOverviewRow{
				Channel: acc.Channel, ID: acc.ID, Nickname: acc.Nickname,
				Kind: creditKind(acc.Channel),
			}
			prober, ok := p.gw.Driver(acc.Channel).(driver.Prober)
			if !ok {
				row.Error = "该渠道不支持额度探测"
				results <- probeResult{row: row}
				return
			}
			ctx, cancel := contextWithTimeout(r, 20*time.Second)
			defer cancel()
			res, err := prober.Probe(ctx, acc)
			if err != nil {
				row.Error = cleanProbeError(err.Error())
				results <- probeResult{row: row}
				return
			}
			row.UpdatedAt = time.Now()
			if res != nil {
				row.Note = res.Note
			}
			if res != nil && res.Credits != nil {
				row.Remain = res.Credits.Remain
				row.Total = res.Credits.Total
				row.Bonus = res.Credits.Bonus
				row.Expiring = res.Credits.Expiring
				row.BalanceOnly = res.Credits.BalanceOnly
			}
			results <- probeResult{row: row}
		}(acc)
	}
	for range other {
		overview = append(overview, (<-results).row)
	}
	// Stable order: domestic channel order, then nickname/id.
	rank := map[string]int{}
	for i, ch := range domesticCreditChannels {
		rank[ch] = i
	}
	sort.SliceStable(overview, func(i, j int) bool {
		if rank[overview[i].Channel] != rank[overview[j].Channel] {
			return rank[overview[i].Channel] < rank[overview[j].Channel]
		}
		ni, nj := overview[i].Nickname, overview[j].Nickname
		if ni != nj {
			return ni < nj
		}
		return overview[i].ID < overview[j].ID
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": out,
		"overview": overview,
	})
}
