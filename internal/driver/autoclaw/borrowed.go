// BP-346~349: the four things the upstream task design taught us
// (research/2026-11-25-AutoClaw任务设计对照.md).
//
//  1. BP-346 login reward — the desktop client raises a server-side "user
//     active" event by calling /userapi/v1/user-info with its own
//     fingerprint; that event is what pays the login campaign (CN +10000,
//     overseas +5000, measured on our own accounts). Our usual web headers
//     answer code=0 there and pay nothing.
//  2. BP-347 per-wallet balance + expiry — every wallet instance carries its
//     own balance and expires_at, so "which points burn on 11-29" becomes
//     visible instead of only a total.
//  3. BP-348 the domestic check-in card — the CN client moved to
//     checkin/card + checkin/claim. We read the card only (a GET): it shows
//     the day's reward, the streak and any limited-time events without
//     spending the day's single claim.
//  4. BP-349 overseas inspiration centre — entering it completes the
//     `daily_inspiration_center` client task, which is the same
//     task-complete endpoint we already use with another task_id.
package autoclaw

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bargainproxy/internal/httpx"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// Desktop fingerprint for the login-reward call.
//
// These are NOT free choices: measured 2026-11-06 by the upstream plugin on
// five accounts, a minimal header set answers code=0 and pays nothing, while
// this exact set pays the campaign. Kept as one block so a future client
// version can be bumped in a single place.
const (
	loginRewardVersion = "2.2.0"
	loginRewardTm      = "win-x64"
	loginRewardChannel = "AutoClaw20"
	loginRewardUA      = "AutoClaw/2.2.0 (win-x64)"
	loginRewardLang    = "zh-CN"
)

// loginRewardHeaders is the desktop fingerprint for /userapi/v1/user-info.
func loginRewardHeaders(token, secret string) http.Header {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "*/*")
	h.Set("User-Agent", loginRewardUA)
	h.Set("X-Version", loginRewardVersion)
	h.Set("X-Tm", loginRewardTm)
	h.Set("X-Product", "autoclaw")
	h.Set("X-Auth-Appid", appID)
	h.Set("X-Auth-TimeStamp", ts)
	h.Set("X-Auth-Sign", sign(ts, secret))
	h.Set("X-Trace-Id", traceID())
	h.Set("X-Lang", loginRewardLang)
	h.Set("X-Channel", loginRewardChannel)
	if token != "" {
		h.Set("Authorization", token)
	}
	return h
}

// ReportLoginActive tells the server this account's client is active, which is
// what raises the login-campaign reward. Best-effort by design: it returns an
// error so the caller can put one line in the task ledger, but nothing here
// may fail a check-in.
func (d *Driver) ReportLoginActive(ctx context.Context, acc *model.Account) error {
	cr, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return uperr.New(uperr.ClassAuth, 0, "autoclaw: %v", err)
	}
	secret := strings.TrimSpace(d.cfg.AppSecret)
	if secret == "" {
		return uperr.New(uperr.ClassInternal, 0, "autoclaw: channels.autoclaw.app_secret 未配置")
	}
	host := d.base()
	if cr.Region == realmOverseas {
		host = d.overseasBase()
	}
	raw, _ := json.Marshal(map[string]any{"source_id": "autoclaw", "device_id": cr.DeviceID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+userInfoPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header = loginRewardHeaders(cr.bearer(), secret)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "autoclaw login reward: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return classifyHTTP(resp.StatusCode, body)
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return uperr.New(uperr.ClassTransport, 0, "autoclaw login reward: parse: %v", err)
	}
	if env.Code != 0 {
		// The code is kept in the message on purpose: this call has no other
		// way to tell "the device is not accepted" from "the campaign is
		// paused", and the task ledger is where a human reads it.
		return uperr.New(uperr.ClassTransport, 0, "autoclaw login reward code=%d: %s", env.Code, env.Msg)
	}
	return nil
}

// Overseas reports which lane the account belongs to. The credential's own
// region is the authority (BP-119); the account row's Realm is a display copy
// that older imports left empty.
func (d *Driver) Overseas(acc *model.Account) bool {
	cr, err := ParseCreds(acc.CredsJSON)
	return err == nil && cr.Region == realmOverseas
}

// CompleteInspiration completes the overseas lane's `daily_inspiration_center`
// client task — what the official client does when the inspiration centre
// opens. Only the overseas build has this task (the CN client has no
// occurrence of the id), so callers must not send CN accounts here.
func (d *Driver) CompleteInspiration(ctx context.Context, acc *model.Account) (alreadyDone bool, reward int64, err error) {
	cr, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return false, 0, uperr.New(uperr.ClassAuth, 0, "autoclaw: %v", err)
	}
	if cr.Region != realmOverseas {
		return false, 0, fmt.Errorf("autoclaw: inspiration centre exists on the overseas lane only")
	}
	host := d.overseasBase()
	body, _ := json.Marshal(map[string]any{"task_id": "daily_inspiration_center"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+taskCompletePath, bytes.NewReader(body))
	if err != nil {
		return false, 0, err
	}
	req.Header = checkinHeaders(cr.bearer())
	resp, err := d.rpc.Do(req)
	if err != nil {
		return false, 0, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "autoclaw inspiration: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return false, 0, classifyHTTP(resp.StatusCode, raw)
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AlreadyCompleted bool        `json:"already_completed"`
			RewardPoints     json.Number `json:"reward_points"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return false, 0, uperr.New(uperr.ClassTransport, 0, "autoclaw inspiration: parse: %v", err)
	}
	if env.Code != 0 {
		return false, 0, classifyBusiness(env.Code, env.Msg)
	}
	reward, _ = env.Data.RewardPoints.Int64()
	return env.Data.AlreadyCompleted, reward, nil
}

// CN card fingerprint. The domestic client's shared request layer stamps
// every call (the asset-operation service included) with its own build
// version and channel. The check-in card is the only endpoint here that
// belongs to that newer client, so it carries that build rather than the
// 1.12.1 web stamp the older userapi/wallet calls use.
const (
	cnCardVersion = "2.5.3"
	cnCardChannel = "official"
)

// cardHeaders is the domestic client's header set plus the per-request id the
// check-in endpoints require.
func cardHeaders(token, secret, requestID string) http.Header {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "*/*")
	h.Set("X-Version", cnCardVersion)
	h.Set("X-Tm", "win")
	h.Set("X-Product", "autoclaw")
	h.Set("X-Auth-Appid", appID)
	h.Set("X-Auth-TimeStamp", ts)
	h.Set("X-Auth-Sign", sign(ts, secret))
	h.Set("X-Lang", "zh-CN")
	h.Set("X-Channel", cnCardChannel)
	h.Set("X-Trace-Id", traceID())
	if requestID != "" {
		h.Set("X-Request-ID", requestID)
	}
	if token != "" {
		h.Set("Authorization", token)
	}
	return h
}

// WalletInstance is one wallet with its own balance and expiry.
type WalletInstance struct {
	WalletID  string    `json:"wallet_id"`
	Name      string    `json:"name"`
	Balance   int64     `json:"balance"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Scope     string    `json:"scope,omitempty"`
	CycleKey  string    `json:"cycle_key,omitempty"`
}

// WalletInstances lists the account's wallet instances (BP-347). Read-only.
//
// The CN client calls this as the fallback when /agent-assetmgr/api/v2/wallets
// has no per-wallet rows, and filters to status=active && display=true — the
// same filter is applied here, because an expired or hidden wallet in the
// panel would report points that cannot be spent.
func (d *Driver) WalletInstances(ctx context.Context, acc *model.Account) ([]WalletInstance, error) {
	cr, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "autoclaw: %v", err)
	}
	var data struct {
		Instances []struct {
			WalletID       string `json:"wallet_id"`
			DisplayName    string `json:"display_name"`
			PublicType     string `json:"public_wallet_type"`
			WalletScope    string `json:"wallet_scope"`
			CycleKey       string `json:"cycle_key"`
			Balance        any    `json:"balance"`
			ExpiresAt      any    `json:"expires_at"`
			Status         string `json:"status"`
			Display        *bool  `json:"display"`
			WalletTypeName string `json:"wallet_type_name"`
		} `json:"wallet_instances"`
	}
	if err := d.userapiCallFor(ctx, http.MethodGet, walletInstancesPath, cr.bearer(), nil, &data, cr.Region == realmOverseas); err != nil {
		return nil, err
	}
	out := make([]WalletInstance, 0, len(data.Instances))
	for _, w := range data.Instances {
		if s := strings.ToLower(strings.TrimSpace(w.Status)); s != "" && s != "active" {
			continue
		}
		if w.Display != nil && !*w.Display {
			continue
		}
		bal := intAny(w.Balance)
		if bal <= 0 {
			continue
		}
		inst := WalletInstance{
			WalletID:  w.WalletID,
			Name:      walletLabel(w.DisplayName, w.WalletScope, w.CycleKey),
			Balance:   bal,
			Scope:     w.WalletScope,
			CycleKey:  w.CycleKey,
			ExpiresAt: parseWalletExpiry(w.ExpiresAt),
		}
		out = append(out, inst)
	}
	return out, nil
}

// parseWalletExpiry reads an expiry the two ways the upstream states it.
//
// Measured live 2026-11-26 on both lanes: wallet-instances answers
// expires_at as an RFC3339 string ("2026-11-29T23:59:59+08:00"), while the
// ledger and points/expiring endpoints use epoch seconds. A single shape
// assumption here fails the whole wallet read with a decode error, which is
// how this was found — the panel showed "钱包明细读取失败" instead of the
// wallets it had actually received.
func parseWalletExpiry(v any) time.Time {
	switch x := v.(type) {
	case string:
		x = strings.TrimSpace(x)
		if x == "" {
			return time.Time{}
		}
		for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
			if ts, err := time.Parse(layout, x); err == nil {
				return ts.Local()
			}
		}
		// A string holding plain epoch seconds.
		if secs, err := strconv.ParseInt(x, 10, 64); err == nil && secs > 0 {
			return time.Unix(secs, 0).Local()
		}
		return time.Time{}
	case float64:
		if x > 0 {
			return time.Unix(int64(x), 0).Local()
		}
	case json.Number:
		if secs, err := x.Int64(); err == nil && secs > 0 {
			return time.Unix(secs, 0).Local()
		}
	}
	return time.Time{}
}

// walletLabel names a wallet for a human.
//
// Measured live 2026-11-26: public_wallet_type is "reward" on EVERY wallet and
// wallet_name is the constant "Earned Points", so neither distinguishes the
// four wallets an account can hold. What does is the cycle key's prefix — that
// is the upstream's own statement of which programme paid the points.
func walletLabel(displayName, scope, cycleKey string) string {
	if n := strings.TrimSpace(displayName); n != "" && n != "Earned Points" {
		return n
	}
	switch {
	case strings.HasPrefix(cycleKey, "reward:daily:daily_signin"):
		return "每日签到"
	case strings.HasPrefix(cycleKey, "task_reward:"):
		return "任务奖励"
	case strings.HasPrefix(cycleKey, "activity:"):
		return "活动奖励（登录）"
	case strings.Contains(cycleKey, "permanent"):
		return "长期积分"
	}
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "annual", "long_term":
		return "长期积分"
	case "campaign":
		return "活动积分"
	case "daily":
		return "每日积分"
	case "monthly":
		return "月度积分"
	}
	if cycleKey != "" {
		return cycleKey
	}
	return "积分钱包"
}

// CheckinCard is the domestic check-in card as the server describes it today
// (BP-348). Read-only.
type CheckinCard struct {
	BizDate         string    `json:"biz_date"`
	ExperimentGroup string    `json:"experiment_group,omitempty"`
	CardVisible     bool      `json:"card_visible"`
	ClaimStatus     string    `json:"claim_status,omitempty"` // unclaimed | processing | claimed
	ClaimableAmount int64     `json:"claimable_amount,omitempty"`
	ClaimedAmount   int64     `json:"claimed_amount,omitempty"`
	DailyAmount     int64     `json:"daily_amount,omitempty"`
	DailyTitle      string    `json:"daily_title,omitempty"`
	StreakDays      int       `json:"streak_days,omitempty"`
	StreakMaxDays   int       `json:"streak_max_days,omitempty"`
	Events          []string  `json:"events,omitempty"`
	FetchedAt       time.Time `json:"fetched_at,omitempty"`
}

// CheckinCard reads the domestic check-in card. It is a GET and spends
// nothing: the day's single claim is spent by checkin/claim, which this
// gateway deliberately does not call while the old daily_signin task still
// pays (research 2026-11-25 §5 item 4).
func (d *Driver) CheckinCard(ctx context.Context, acc *model.Account) (*CheckinCard, error) {
	cr, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "autoclaw: %v", err)
	}
	rid := requestUUID()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base()+checkinCardPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header = cardHeaders(cr.bearer(), strings.TrimSpace(d.cfg.AppSecret), rid)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "autoclaw checkin card: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, classifyHTTP(resp.StatusCode, raw)
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, uperr.New(uperr.ClassTransport, 0, "autoclaw checkin card: parse: %v", err)
	}
	if env.Code != 0 {
		return nil, classifyBusiness(env.Code, env.Msg)
	}
	card, err := parseCheckinCard(env.Data)
	if err != nil {
		return nil, uperr.New(uperr.ClassTransport, 0, "autoclaw checkin card: %v", err)
	}
	return card, nil
}

// parseCheckinCard reads the card payload tolerantly: the outer keys are
// snake_case in the server's own shape, but the streak sub-object arrives in
// whichever case the experiment's build uses, so both spellings are accepted.
func parseCheckinCard(raw []byte) (*CheckinCard, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	card := &CheckinCard{FetchedAt: time.Now()}
	card.BizDate = strAny(m["biz_date"])
	card.ExperimentGroup = strAny(m["experiment_group"])
	card.CardVisible = boolAny(m["card_visible"])
	card.ClaimStatus = strAny(m["claim_status"])
	card.ClaimableAmount = intAny(m["total_claimable_amount"], m["claimable_amount"])
	card.ClaimedAmount = intAny(m["claimed_amount"])
	if daily, ok := m["daily"].(map[string]any); ok {
		card.DailyAmount = intAny(daily["amount"])
		card.DailyTitle = strAny(daily["title"])
		if streak, ok := daily["streak"].(map[string]any); ok {
			// Measured live 2026-11-26: current_streak_days / max_streak_days.
			// max_streak_days reads 0 until the account has a streak, so the
			// projected value is the useful upper bound for a fresh account.
			card.StreakDays = int(intAny(streak["current_streak_days"], streak["current_days"], streak["currentDays"]))
			card.StreakMaxDays = int(intAny(streak["max_streak_days"], streak["max_days"], streak["maxDays"]))
			if card.StreakDays == 0 {
				card.StreakDays = int(intAny(streak["projected_current_streak_days"]))
			}
		}
	}
	if evs, ok := m["events"].([]any); ok {
		for _, e := range evs {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			title := strAny(em["title"])
			amount := intAny(em["amount"])
			if title == "" && amount == 0 {
				continue
			}
			if amount > 0 {
				card.Events = append(card.Events, fmt.Sprintf("%s +%d", title, amount))
			} else {
				card.Events = append(card.Events, title)
			}
		}
	}
	return card, nil
}

func strAny(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func boolAny(v any) bool {
	b, _ := v.(bool)
	return b
}

// intAny returns the first key that holds a usable number.
func intAny(vals ...any) int64 {
	for _, v := range vals {
		switch n := v.(type) {
		case float64:
			return int64(n)
		case json.Number:
			i, err := n.Int64()
			if err == nil {
				return i
			}
		case string:
			if i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
				return i
			}
		}
	}
	return 0
}
