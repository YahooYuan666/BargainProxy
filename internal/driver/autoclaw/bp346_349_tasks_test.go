package autoclaw

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/model"
)

// BP-346: the login reward is paid only when the call carries the desktop
// fingerprint. Measured upstream 2026-11-06 on five accounts: a minimal header
// set answers code=0 and pays nothing. So the header set IS the feature — this
// test pins every one of the four headers that separate paying from silent.
func TestLoginRewardCarriesTheDesktopFingerprint(t *testing.T) {
	var got http.Header
	var gotPath string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		got = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"user_id":"u1"}}`))
	}))
	defer srv.Close()

	d := New(config.AutoClawSettings{Enabled: true, UserAPIBase: srv.URL, AppSecret: "s"}, nil)
	acc := &model.Account{ID: "autoclaw:u1", CredsJSON: testCreds()}
	if err := d.ReportLoginActive(context.Background(), acc); err != nil {
		t.Fatalf("ReportLoginActive: %v", err)
	}
	if gotPath != userInfoPath {
		t.Fatalf("path = %s, want %s", gotPath, userInfoPath)
	}
	// The four headers the upstream A/B singled out. A regression in any of
	// them turns a paying call into a silent one, which is invisible except
	// in the wallet ledger days later.
	for _, tc := range []struct{ name, want string }{
		{"X-Channel", loginRewardChannel},
		{"X-Version", loginRewardVersion},
		{"X-Tm", loginRewardTm},
		{"User-Agent", loginRewardUA},
	} {
		if v := got.Get(tc.name); v != tc.want {
			t.Fatalf("%s = %q, want %q (this header is what makes the reward pay)", tc.name, v, tc.want)
		}
	}
	if got.Get("Authorization") != "Bearer tok" {
		t.Fatalf("Authorization = %q", got.Get("Authorization"))
	}
	if got.Get("X-Auth-Sign") == "" {
		t.Fatal("X-Auth-Sign missing: the gateway answers 401 without the signature")
	}
	// The body is the identity call the client makes, not a task id.
	if body["source_id"] != "autoclaw" || body["device_id"] != "dev" {
		t.Fatalf("body = %v, want source_id/device_id", body)
	}
}

// The login reward is best-effort: its failure must be reportable without
// being mistaken for a check-in failure.
func TestLoginRewardRefusalIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":126001,"msg":"device not allowed"}`))
	}))
	defer srv.Close()
	d := New(config.AutoClawSettings{Enabled: true, UserAPIBase: srv.URL, AppSecret: "s"}, nil)
	err := d.ReportLoginActive(context.Background(), &model.Account{ID: "autoclaw:u1", CredsJSON: testCreds()})
	if err == nil {
		t.Fatal("a refusal must be reported")
	}
	if !strings.Contains(err.Error(), "126001") {
		t.Fatalf("error should name the upstream code, got %v", err)
	}
}

// The login reward uses the account's own lane: an overseas token sent to the
// CN host answers "session dead" and the reward would silently never pay for
// every global account (the exact bug BP-119 fixed for check-in).
func TestLoginRewardUsesTheAccountsLane(t *testing.T) {
	cnHits, ovHits := 0, 0
	cnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cnHits++
		_, _ = w.Write([]byte(`{"code":410000,"msg":"session dead"}`))
	}))
	defer cnSrv.Close()
	ovSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ovHits++
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
	}))
	defer ovSrv.Close()

	d := New(config.AutoClawSettings{Enabled: true, UserAPIBase: cnSrv.URL, OverseasBase: ovSrv.URL, AppSecret: "s"}, nil)
	acc := &model.Account{
		ID: "autoclaw:182537", Realm: realmOverseas,
		CredsJSON: (&Creds{AccessToken: "Bearer t", Region: realmOverseas, DeviceID: "dev"}).Marshal(),
	}
	if err := d.ReportLoginActive(context.Background(), acc); err != nil {
		t.Fatalf("ReportLoginActive: %v", err)
	}
	if cnHits != 0 || ovHits != 1 {
		t.Fatalf("lane hits cn=%d overseas=%d, want cn=0 overseas=1", cnHits, ovHits)
	}
}

// BP-347: every wallet comes back with its own balance and expiry, and the
// filter matches what the CN client applies (active + display). An expired or
// hidden wallet shown in the panel would report points that cannot be spent.
//
// The shape here is the one measured live on 2026-11-26, not the one read out
// of the client bundle: expires_at is an RFC3339 STRING (the ledger and
// points/expiring endpoints use epoch seconds on the same host), and
// public_wallet_type is "reward" on every wallet — a single-shape assumption
// fails the whole read, which is how this was found.
func TestWalletInstancesKeepBalanceAndExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/agent-assetmgr/api/v1/wallet-instances") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("wallet_type") != "all" || r.URL.Query().Get("wallet_scope") != "all" {
			t.Errorf("query = %s, want wallet_type=all&wallet_scope=all", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"SUCCESS","data":{"total_balance":10375,"wallet_instances":[
			{"wallet_id":"w1","wallet_name":"Earned Points","public_wallet_type":"reward","wallet_scope":"campaign","balance":9675,"balance_view":"9675","display":true,"status":"active","expires_at":"2026-11-29T23:59:59+08:00","cycle_key":"activity:autoclaw_user_active_reward_202609:517:2026-11-23"},
			{"wallet_id":"w2","wallet_name":"Earned Points","public_wallet_type":"reward","wallet_scope":"annual","balance":200,"display":true,"status":"active","expires_at":"2026-12-02T22:33:40+08:00","cycle_key":"reward:daily:daily_signin:933603:2026-11-25"},
			{"wallet_id":"w3","balance":500,"display":true,"status":"expired"},
			{"wallet_id":"w4","balance":900,"display":false,"status":"active"},
			{"wallet_id":"w5","balance":0,"display":true,"status":"active"},
			{"wallet_id":"w9","balance":5000,"display":true,"status":"active","expires_at":"2026-11-29T23:59:59+08:00","cycle_key":"activity:autoclaw_user_active_reward_202609:104:2026-11-23"}
		]}}`))
	}))
	defer srv.Close()
	d := New(config.AutoClawSettings{Enabled: true, UserAPIBase: srv.URL, AppSecret: "s"}, nil)
	wallets, err := d.WalletInstances(context.Background(), &model.Account{ID: "autoclaw:u1", CredsJSON: testCreds()})
	if err != nil {
		t.Fatalf("WalletInstances: %v (the live shape must parse)", err)
	}
	if len(wallets) != 3 {
		t.Fatalf("wallets = %d, want 3 (expired, hidden and empty dropped): %+v", len(wallets), wallets)
	}
	byID := map[string]WalletInstance{}
	for _, w := range wallets {
		byID[w.WalletID] = w
	}
	// The RFC3339 string lands as the moment it states, not as a zero time.
	w1, ok := byID["w1"]
	if !ok {
		t.Fatalf("w1 missing from %+v", wallets)
	}
	if w1.Balance != 9675 {
		t.Fatalf("w1 balance = %d, want 9675", w1.Balance)
	}
	wantExpiry, _ := time.Parse(time.RFC3339, "2026-11-29T23:59:59+08:00")
	if !w1.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("w1 expiry = %v, want %v", w1.ExpiresAt, wantExpiry)
	}
	// Every wallet's public type is "reward"; the label must still tell them
	// apart, or the drawer shows three identical rows.
	if w1.Name == byID["w2"].Name {
		t.Fatalf("both wallets named %q: the label must distinguish the programme", w1.Name)
	}
	if w1.Name != "活动奖励（登录）" || byID["w2"].Name != "每日签到" {
		t.Fatalf("labels = %q / %q, want 活动奖励（登录） / 每日签到", w1.Name, byID["w2"].Name)
	}
}

// Expiry arrives in more than one shape across the upstream's own endpoints,
// so the parser accepts each of them rather than failing the whole wallet read.
func TestWalletExpiryAcceptsBothShapes(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want string
	}{
		{"2026-11-29T23:59:59+08:00", "2026-11-29T23:59:59+08:00"},
		{"1796000000", "epoch"},
		{float64(1796000000), "epoch"},
		{json.Number("1796000000"), "epoch"},
	} {
		got := parseWalletExpiry(tc.in)
		if got.IsZero() {
			t.Fatalf("parseWalletExpiry(%v) = zero, want a moment", tc.in)
		}
		if tc.want != "epoch" && got.Format(time.RFC3339) != tc.want {
			t.Fatalf("parseWalletExpiry(%v) = %s, want %s", tc.in, got.Format(time.RFC3339), tc.want)
		}
	}
	if !parseWalletExpiry("").IsZero() || !parseWalletExpiry(nil).IsZero() {
		t.Fatal("an absent expiry must read as a zero time, not as 1970")
	}
}

// BP-348: the card is read-only and yields the day's numbers. The claim
// endpoint must NOT be touched — that is what spends the day's single claim.
//
// Payload is trimmed from a live read (2026-11-26): streak fields are
// current_streak_days / max_streak_days, and a fresh account reports
// max_streak_days=0 with a projected value — so the projection is what the
// drawer shows until a real streak exists.
func TestCheckinCardIsReadOnly(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if r.Header.Get("X-Request-ID") == "" {
			t.Error("X-Request-ID missing: the check-in endpoints require it")
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"success","data":{
			"biz_date":"2026-11-26","card_visible":true,"claim_status":"unclaimed",
			"claimed_amount":0,"total_claimable_amount":300,
			"daily":{"amount":300,"title":"每日签到","validity_seconds":604800,
			         "streak":{"current_streak_days":0,"max_streak_days":0,
			                   "projected_current_streak_days":1,"projected_max_streak_days":1,
			                   "today_checked_in":false}},
			"day_end_at":1795708799,"events":[],"identity":{"payment":"free","stage":"new"}
		}}`))
	}))
	defer srv.Close()
	d := New(config.AutoClawSettings{Enabled: true, UserAPIBase: srv.URL, AppSecret: "s"}, nil)
	card, err := d.CheckinCard(context.Background(), &model.Account{ID: "autoclaw:u1", CredsJSON: testCreds()})
	if err != nil {
		t.Fatalf("CheckinCard: %v", err)
	}
	if len(methods) != 1 || methods[0] != "GET "+checkinCardPath {
		t.Fatalf("calls = %v, want exactly one GET %s (the claim must not be made)", methods, checkinCardPath)
	}
	if card.DailyAmount != 300 || card.ClaimableAmount != 300 {
		t.Fatalf("card = %+v, want daily 300 / claimable 300", card)
	}
	if card.DailyTitle != "每日签到" {
		t.Fatalf("daily title = %q", card.DailyTitle)
	}
	if card.StreakDays != 1 {
		t.Fatalf("streak = %d, want the projected 1 (a fresh account reports 0 current)", card.StreakDays)
	}
	if card.ClaimStatus != "unclaimed" || card.BizDate != "2026-11-26" {
		t.Fatalf("card = %+v", card)
	}
}

// A limited-time event on the card is surfaced with its amount, because that
// is the part the old protocol never paid.
func TestCheckinCardSurfacesLimitedTimeEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"success","data":{
			"biz_date":"2026-11-26","card_visible":true,"claim_status":"unclaimed",
			"daily":{"amount":300,"streak":{"current_streak_days":3,"max_streak_days":7}},
			"events":[{"event_id":"double11_2026","title":"双十一限时奖励","amount":3000}]
		}}`))
	}))
	defer srv.Close()
	d := New(config.AutoClawSettings{Enabled: true, UserAPIBase: srv.URL, AppSecret: "s"}, nil)
	card, err := d.CheckinCard(context.Background(), &model.Account{ID: "autoclaw:u1", CredsJSON: testCreds()})
	if err != nil {
		t.Fatalf("CheckinCard: %v", err)
	}
	if len(card.Events) != 1 || !strings.Contains(card.Events[0], "双十一限时奖励") || !strings.Contains(card.Events[0], "3000") {
		t.Fatalf("events = %v, want the title and its amount", card.Events)
	}
	if card.StreakDays != 3 || card.StreakMaxDays != 7 {
		t.Fatalf("streak = %d/%d, want 3/7", card.StreakDays, card.StreakMaxDays)
	}
}

// A card the server hides (A/B control group) is an answer, not a failure.
func TestCheckinCardHiddenIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"biz_date":"2026-11-26","experiment_group":"A","card_visible":false}}`))
	}))
	defer srv.Close()
	d := New(config.AutoClawSettings{Enabled: true, UserAPIBase: srv.URL, AppSecret: "s"}, nil)
	card, err := d.CheckinCard(context.Background(), &model.Account{ID: "autoclaw:u1", CredsJSON: testCreds()})
	if err != nil {
		t.Fatalf("CheckinCard: %v", err)
	}
	if card.CardVisible {
		t.Fatalf("card = %+v, want card_visible=false", card)
	}
}

// BP-349: the inspiration task exists on the overseas lane only. Sending a CN
// account there is a local refusal, not an upstream 404 (the CN gateway has no
// such task, and a doomed call would land in the ledger as a daily failure).
func TestInspirationIsOverseasOnly(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"code":0,"data":{"already_completed":false,"reward_points":200}}`))
	}))
	defer srv.Close()
	d := New(config.AutoClawSettings{Enabled: true, UserAPIBase: srv.URL, OverseasBase: srv.URL, AppSecret: "s"}, nil)

	_, _, err := d.CompleteInspiration(context.Background(), &model.Account{ID: "autoclaw:u1", CredsJSON: testCreds()})
	if err == nil {
		t.Fatal("a CN account must be refused locally")
	}
	if hits != 0 {
		t.Fatalf("upstream was called %d times for a CN account", hits)
	}

	ov := &model.Account{
		ID: "autoclaw:182537", Realm: realmOverseas,
		CredsJSON: (&Creds{AccessToken: "Bearer t", Region: realmOverseas}).Marshal(),
	}
	already, reward, err := d.CompleteInspiration(context.Background(), ov)
	if err != nil {
		t.Fatalf("CompleteInspiration: %v", err)
	}
	if already || reward != 200 {
		t.Fatalf("(already, reward) = (%v, %d), want (false, 200)", already, reward)
	}
}

// Overseas() reads the credential, not the display column: an import that left
// Account.Realm empty must not send a global account down the CN path.
func TestOverseasFollowsTheCredential(t *testing.T) {
	d := New(config.AutoClawSettings{Enabled: true, AppSecret: "s"}, nil)
	ov := &model.Account{ID: "autoclaw:1", CredsJSON: (&Creds{AccessToken: "t", Region: realmOverseas}).Marshal()}
	if !d.Overseas(ov) {
		t.Fatal("overseas credential must read as overseas even with an empty Account.Realm")
	}
	cn := &model.Account{ID: "autoclaw:2", Realm: "global", CredsJSON: testCreds()}
	if d.Overseas(cn) {
		t.Fatal("a CN credential must read as CN even if the display column says global")
	}
}
