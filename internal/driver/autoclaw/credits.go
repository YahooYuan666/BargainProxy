// AutoClaw points detail: balance, per-request ledger, expiring points.
//
// The reference implementation (agentpoints2 internal/autoclaw) shows the
// upstream asset manager exposes the same data the desktop client renders in
// its 积分明细 page:
//
//	GET /agent-assetmgr/api/v1/ledgers_std?page=&page_size=
//	GET /agent-assetmgr/api/v1/points/expiring
//	GET /agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw
//
// A ledger row is the authoritative answer to "how many points did this
// request cost": metadata.charged_amount is the deducted amount and
// metadata.token_usage carries the usage that was billed.
package autoclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// LedgerEntry is one points movement (negative amount = spend).
type LedgerEntry struct {
	Time        string `json:"time"`
	Amount      string `json:"amount"`
	Reason      string `json:"reason"`
	Model       string `json:"model"`
	ActualModel string `json:"actual_model"`
	Charged     string `json:"charged"`
	Cap         string `json:"cap"`
	TokenUsage  string `json:"token_usage"`
	Description string `json:"description"`
}

// ExpiringPoints is the upstream expiry summary ("本月将过期 N 点").
type ExpiringPoints struct {
	Points int64  `json:"points"`
	Total  int64  `json:"total"`
	Days   int64  `json:"days"`
	Text   string `json:"text,omitempty"`
}

// CreditDetail is the panel's AutoClaw points-detail payload.
type CreditDetail struct {
	Account  string          `json:"account,omitempty"`
	Balance  int64           `json:"balance"`
	Expiring *ExpiringPoints `json:"expiring,omitempty"`
	Entries  []LedgerEntry   `json:"entries"`
	Total    int64           `json:"total"`
}

// CreditDetail fetches balance, the latest ledger page and expiring points.
// A dead session is refreshed once before failing.
func (d *Driver) CreditDetail(ctx context.Context, acc *model.Account, page, pageSize int) (*CreditDetail, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}
	cr, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "autoclaw: %v", err)
	}
	run := func() (*CreditDetail, error) {
		out := &CreditDetail{Account: acc.ID, Entries: []LedgerEntry{}}

		var wallet struct {
			TotalBalance json.Number `json:"total_balance"`
		}
		if err := d.userapiCall(ctx, http.MethodGet, walletsPath, cr.bearer(), nil, &wallet); err != nil {
			return nil, err
		}
		out.Balance, _ = wallet.TotalBalance.Int64()

		var ledger struct {
			Entries []map[string]any `json:"entries"`
			Total   any              `json:"total"`
		}
		path := fmt.Sprintf("/agent-assetmgr/api/v1/ledgers_std?page=%d&page_size=%d", page, pageSize)
		if err := d.userapiCall(ctx, http.MethodGet, path, cr.bearer(), nil, &ledger); err != nil {
			return nil, err
		}
		out.Entries = parseLedgerEntries(ledger.Entries)
		out.Total = toInt64Any(ledger.Total)
		if out.Total == 0 {
			out.Total = int64(len(out.Entries))
		}

		var exp struct {
			Points      any    `json:"expiring_points"`
			TotalPoints any    `json:"total_points"`
			Days        any    `json:"expire_time_value"`
			Text        string `json:"expiring_points_text"`
		}
		if err := d.userapiCall(ctx, http.MethodGet, "/agent-assetmgr/api/v1/points/expiring", cr.bearer(), nil, &exp); err == nil {
			out.Expiring = &ExpiringPoints{
				Points: toInt64Any(exp.Points),
				Total:  toInt64Any(exp.TotalPoints),
				Days:   toInt64Any(exp.Days),
				Text:   exp.Text,
			}
		}
		return out, nil
	}
	out, err := run()
	if err == nil {
		return out, nil
	}
	if ue, ok := uperr.As(err); ok && ue.Class == uperr.ClassAuth {
		if rerr := d.Refresh(ctx, acc); rerr == nil {
			if cr2, perr := ParseCreds(acc.CredsJSON); perr == nil {
				cr = cr2
				return run()
			}
		}
	}
	return nil, err
}

// parseLedgerEntries flattens the upstream ledger rows. Metadata keys are
// optional: recharge rows carry no model/token fields.
func parseLedgerEntries(rows []map[string]any) []LedgerEntry {
	out := make([]LedgerEntry, 0, len(rows))
	for _, row := range rows {
		e := LedgerEntry{
			Time:        toStringAny(row["occurred_at"]),
			Amount:      toStringAny(row["amount"]),
			Description: toStringAny(row["description"]),
			Reason:      toStringAny(row["mutation_type"]),
		}
		if meta, ok := row["metadata"].(map[string]any); ok {
			e.Model = firstString(meta["model"], meta["display_model"])
			e.ActualModel = toStringAny(meta["actual_model"])
			if r := firstString(meta["reason"], row["mutation_type"]); r != "" {
				e.Reason = r
			}
			e.Charged = toStringAny(meta["charged_amount"])
			e.Cap = toStringAny(meta["request_point_cap"])
			e.TokenUsage = toStringAny(meta["token_usage"])
		}
		out = append(out, e)
	}
	return out
}

func firstString(vals ...any) string {
	for _, v := range vals {
		if s := toStringAny(v); s != "" {
			return s
		}
	}
	return ""
}

func toInt64Any(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case int64:
		return n
	case int:
		return int64(n)
	case string:
		var i int64
		fmt.Sscanf(n, "%d", &i)
		return i
	}
	return 0
}
