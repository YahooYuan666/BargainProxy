// Per-account credit details for the panel's 积分详情 drawer.
//
// AutoClaw: the upstream points ledger (every request's charged_amount and
// token usage) plus expiring points — the answer to "how many points did this
// cost". Qoder: plan/add-on/org quota breakdown and daily check-in state.
// Both are fetched on demand; the account table stays cheap.
package panel

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"bargainproxy/internal/model"
)

type observedCreditRow struct {
	Model string  `json:"model"`
	N     int     `json:"n"`
	Avg   float64 `json:"avg"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
}

// observedCredits aggregates the upstream-reported per-request deduction for
// one account (last 7 days). Channels that do not report credit per request
// return an empty slice — the UI then shows the upstream ledger instead.
func (p *Panel) observedCreditsFor(acc *model.Account) []observedCreditRow {
	if p.store == nil || acc == nil {
		return nil
	}
	logs, err := p.store.RecentRequestLogs(20000)
	if err != nil {
		return nil
	}
	type agg struct {
		n           int
		sum, lo, hi float64
	}
	m := map[string]*agg{}
	since := time.Now().Add(-7 * 24 * time.Hour)
	for _, l := range logs {
		if !l.CreditKnown || l.Status != 200 || l.Time.Before(since) {
			continue
		}
		if l.AccountID != acc.ID && (l.AccountLabel == "" || l.AccountLabel != acc.Nickname) {
			continue
		}
		a := m[l.Model]
		if a == nil {
			a = &agg{}
			m[l.Model] = a
		}
		c := l.Credit
		if a.n == 0 || c < a.lo {
			a.lo = c
		}
		if a.n == 0 || c > a.hi {
			a.hi = c
		}
		a.sum += c
		a.n++
	}
	out := make([]observedCreditRow, 0, len(m))
	for mdl, a := range m {
		out = append(out, observedCreditRow{
			Model: mdl, N: a.n,
			Avg: round2(a.sum / float64(a.n)),
			Min: round2(a.lo), Max: round2(a.hi),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N > out[j].N })
	return out
}

// handleAccountCredits serves GET /panel/api/accounts/credits?channel=&id=.
func (p *Panel) handleAccountCredits(w http.ResponseWriter, r *http.Request) {
	channel := strings.TrimSpace(r.URL.Query().Get("channel"))
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if channel == "" || id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "channel and id are required"})
		return
	}
	pl := p.gw.Pool()
	pl.Lock()
	var acc *model.Account
	for _, a := range pl.AccountsLocked(channel) {
		if a.ID == id {
			acc = a
			break
		}
	}
	pl.Unlock()
	if acc == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found"})
		return
	}
	ctx, cancel := contextWithTimeout(r, 45*time.Second)
	defer cancel()

	out := map[string]any{"channel": channel, "id": id, "observed": p.observedCreditsFor(acc)}
	switch channel {
	case model.ChannelAutoClaw:
		d, ok := p.autoClawDriver()
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "AutoClaw 渠道未启用"})
			return
		}
		det, err := d.CreditDetail(ctx, acc, 1, 50)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		spent, n := int64(0), 0
		for _, e := range det.Entries {
			raw := strings.TrimSpace(e.Amount)
			if !strings.HasPrefix(raw, "-") {
				continue
			}
			v, perr := strconv.ParseInt(strings.TrimPrefix(raw, "-"), 10, 64)
			if perr == nil {
				spent += v
				n++
			}
		}
		out["detail"] = det
		out["recent_spent"] = spent
		out["recent_count"] = n
	case model.ChannelQoder:
		d, ok := p.qoderDriver()
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Qoder 渠道未启用"})
			return
		}
		det, err := d.QuotaDetail(ctx, acc)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		out["detail"] = det
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "该渠道暂无积分详情"})
		return
	}
	writeJSON(w, http.StatusOK, out)
}
