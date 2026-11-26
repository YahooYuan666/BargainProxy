// BP-347/348 through the panel: the points drawer must carry the per-wallet
// breakdown and the domestic check-in card, and a CN/Overseas split must be
// made by the credential rather than by the display column.
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/autoclaw"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
)

// bp347Panel wires a panel whose AutoClaw driver talks to the given upstream.
func bp347Panel(t *testing.T, handler http.Handler, credsJSON []byte) (*Panel, *model.Account) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.Channels.AutoClaw.UserAPIBase = srv.URL
	cfg.Channels.AutoClaw.OverseasBase = srv.URL
	cfg.Channels.AutoClaw.AppSecret = "s"

	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 0}, map[string]int{model.ChannelAutoClaw: 2}, nil)
	acc := &model.Account{
		ID: "autoclaw:u1", Channel: model.ChannelAutoClaw, CredsJSON: credsJSON, Nickname: "ac1",
	}
	p.Upsert(acc)
	drivers := map[string]driver.Driver{model.ChannelAutoClaw: autoclaw.New(cfg.Channels.AutoClaw, nil)}
	gw := gateway.New(cfg, p, router.New(cfg), drivers, st)
	return New(cfg, gw, st, nil), acc
}

func bp347Handler(cardHits *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/agent-assetmgr/api/v2/wallets":
			_, _ = w.Write([]byte(`{"code":0,"data":{"total_balance":3264}}`))
		case strings.HasPrefix(r.URL.Path, "/agent-assetmgr/api/v1/ledgers_std"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"entries":[],"total":0}}`))
		case r.URL.Path == "/agent-assetmgr/api/v1/points/expiring":
			_, _ = w.Write([]byte(`{"code":0,"data":{"expiring_points":10000,"total_points":10000,"expire_time_value":3}}`))
		case strings.HasPrefix(r.URL.Path, "/agent-assetmgr/api/v1/wallet-instances"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"wallet_instances":[
				{"wallet_id":"w1","wallet_name":"Earned Points","public_wallet_type":"reward","wallet_scope":"campaign","balance":9675,"expires_at":"2026-11-29T23:59:59+08:00","status":"active","display":true,"cycle_key":"activity:autoclaw_user_active_reward_202609:517:2026-11-23"},
				{"wallet_id":"w6","wallet_scope":"annual","balance":3264,"status":"active","display":true,"cycle_key":"permanent"}
			]}}`))
		case r.URL.Path == "/asset-operation/api/v1/checkin/card":
			if cardHits != nil {
				*cardHits++
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"biz_date":"2026-11-26","card_visible":true,"claim_status":"unclaimed","daily":{"amount":200,"streak":{"current_days":13,"max_days":7}}}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	})
}

// TestCreditDetailCarriesWalletsAndCard: one drawer load answers "how much is
// left", "which wallet burns when", and "what would the new check-in pay".
func TestCreditDetailCarriesWalletsAndCard(t *testing.T) {
	cardHits := 0
	p, acc := bp347Panel(t, bp347Handler(&cardHits),
		[]byte(`{"access_token":"tok","region":"cn","device_id":"dev"}`))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/accounts/credits?channel=autoclaw&id="+acc.ID, nil)
	p.handleAccountCredits(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Detail struct {
			Balance  int64 `json:"balance"`
			Wallets  []struct {
				WalletID  string `json:"wallet_id"`
				Balance   int64  `json:"balance"`
				ExpiresAt string `json:"expires_at"`
			} `json:"wallets"`
			WalletError string `json:"wallet_error"`
		} `json:"detail"`
		CheckinCard *struct {
			CardVisible bool `json:"card_visible"`
			DailyAmount int64 `json:"daily_amount"`
			StreakDays  int   `json:"streak_days"`
		} `json:"checkin_card"`
		CheckinCardError string `json:"checkin_card_error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if out.Detail.Balance != 3264 {
		t.Fatalf("balance = %d, want 3264", out.Detail.Balance)
	}
	if len(out.Detail.Wallets) != 2 {
		t.Fatalf("wallets = %+v, want two", out.Detail.Wallets)
	}
	if out.Detail.WalletError != "" {
		t.Fatalf("wallet_error = %q", out.Detail.WalletError)
	}
	if out.CheckinCard == nil || out.CheckinCard.DailyAmount != 200 || out.CheckinCard.StreakDays != 13 {
		t.Fatalf("checkin_card = %+v (err %q)", out.CheckinCard, out.CheckinCardError)
	}
	// Exactly one read: the drawer must not spend the day's claim.
	if cardHits != 1 {
		t.Fatalf("card reads = %d, want 1", cardHits)
	}
}

// The overseas lane has no check-in card. Probing it would send a doomed CN
// request per drawer open, so the read is skipped entirely.
func TestCreditDetailSkipsCardOnOverseas(t *testing.T) {
	cardHits := 0
	p, acc := bp347Panel(t, bp347Handler(&cardHits),
		[]byte(`{"access_token":"tok","region":"global","device_id":"dev"}`))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/accounts/credits?channel=autoclaw&id="+acc.ID, nil)
	p.handleAccountCredits(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if cardHits != 0 {
		t.Fatalf("card reads = %d, want 0 on the overseas lane", cardHits)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := out["checkin_card"]; ok {
		t.Fatal("an overseas drawer must not carry a checkin_card")
	}
	if _, ok := out["checkin_card_error"]; ok {
		t.Fatal("an overseas drawer must not report a card error either: nothing was attempted")
	}
}

// A failing wallet read must leave the balance and ledger intact — it is an
// extra section, not the errand.
func TestCreditDetailSurvivesWalletFailure(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/agent-assetmgr/api/v1/wallet-instances"):
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`<html><h1>502 Bad Gateway</h1></html>`))
		case r.URL.Path == "/agent-assetmgr/api/v2/wallets":
			_, _ = w.Write([]byte(`{"code":0,"data":{"total_balance":3264}}`))
		case strings.HasPrefix(r.URL.Path, "/agent-assetmgr/api/v1/ledgers_std"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"entries":[],"total":0}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	})
	p, acc := bp347Panel(t, handler, []byte(`{"access_token":"tok","region":"cn","device_id":"dev"}`))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/accounts/credits?channel=autoclaw&id="+acc.ID, nil)
	p.handleAccountCredits(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s — a wallet failure must not cost the drawer", rec.Code, rec.Body.String())
	}
	var out struct {
		Detail struct {
			Balance     int64  `json:"balance"`
			WalletError string `json:"wallet_error"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Detail.Balance != 3264 {
		t.Fatalf("balance = %d, want it kept", out.Detail.Balance)
	}
	if out.Detail.WalletError == "" {
		t.Fatal("the failed wallet read must be named, not silently look like an empty account")
	}
	if strings.Contains(out.Detail.WalletError, "<") {
		t.Fatalf("wallet_error carries HTML: %q", out.Detail.WalletError)
	}
}
