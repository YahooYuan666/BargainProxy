// Daily check-in for the lobster channel.
//
// The vendor runs the check-in as a "client activity" (the same mechanism that
// renders the 每日积分礼 card in the desktop sidebar), not as a simple
// /api/checkin endpoint. Three calls are involved and all three are needed:
//
//	GET  /api/client-activities/slot?placement=desktop_sidebar&clientVersion=…
//	GET  /api/client-activities/{code}/context?configRevision=…
//	POST /api/client-activities/{code}/actions/check_in
//
// Two gates matter and both are silent when wrong:
//
//   - clientVersion (query param AND User-Agent) must be recent enough. An old
//     value makes the slot answer slotState=empty, which looks exactly like
//     "this account has no check-in programme" rather than an error.
//   - the action POST needs a fresh idempotencyKey per call; the server also
//     enforces one claim per day via claimedToday, so a retry cannot double-pay.
//
// Reward is a fixed 100 credits/day, granted into the campaign ledger and
// visible as a "每日登录奖励" credit item.

package lobster

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"bargainproxy/internal/httpx"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// Business codes the activity endpoints return.
const (
	actCodeLoginRequired  = 51102
	actCodeAlreadyClaimed = 51104
)

// checkinClientVersion is the desktop client version advertised to the
// activity endpoints. The upstream hides the whole activity when this is too
// old, so it is a hard gate, not a cosmetic header. 2026.10.16 is the
// currently shipped build (modelmux PROTOCOL.md: 2026.8.1 → slotState=empty,
// 2026.10.16 → available); the previous constant 2026.9.4 still worked but
// dies the moment the server tightens the floor (BP-166).
const checkinClientVersion = "2026.10.16"

// CheckinResult is the outcome of one check-in attempt.
type CheckinResult struct {
	// AlreadyClaimed is true when the account had already claimed today.
	AlreadyClaimed bool
	// CreditsGranted is the amount credited by THIS call (0 when already done).
	CreditsGranted int64
	// ClaimedDays / TotalDays come from the post-claim context.
	ClaimedDays int64
	TotalDays   int64
	// PeriodKey is the server's day key ("2026-10-23"), in Asia/Shanghai.
	PeriodKey string
	// claimedTodaySeen is the claimedToday flag from the claim response's own
	// context, when the response carried one (nil = absent).
	claimedTodaySeen *bool
}

// Checkin performs the daily check-in for one account.
//
// It is idempotent: an account that already claimed today returns
// AlreadyClaimed=true with zero granted credits instead of an error, so the
// scheduler can run the task twice a day without special-casing.
func (d *Driver) Checkin(ctx context.Context, acc *model.Account) (*CheckinResult, error) {
	c, err := d.credsOf(acc)
	if err != nil {
		return nil, err
	}

	slot, err := d.activitySlot(ctx, c.AccessToken)
	if err != nil {
		return nil, err
	}
	code, revision := slot.activityCode, slot.configRevision
	if code == "" {
		return nil, uperr.New(uperr.ClassNotFound, 0,
			"lobster: no check-in activity offered (slotState=%s)", slot.slotState)
	}

	// Read the state first: this both tells us whether today is already
	// claimed (skip the POST entirely) and gives the values for the note.
	state, err := d.activityContext(ctx, c.AccessToken, code, revision)
	if err != nil {
		return nil, err
	}
	if state.claimedToday {
		return &CheckinResult{
			AlreadyClaimed: true,
			ClaimedDays:    state.claimedDays,
			TotalDays:      state.totalDays,
		}, nil
	}

	res, err := d.activityCheckIn(ctx, c.AccessToken, code, revision)
	if err != nil {
		// The server's own already-claimed answer is not a failure: a second
		// scheduled run in the same day lands here.
		if ue, ok := uperr.As(err); ok && ue.Code == fmt.Sprint(actCodeAlreadyClaimed) {
			return &CheckinResult{AlreadyClaimed: true, ClaimedDays: state.claimedDays, TotalDays: state.totalDays}, nil
		}
		return nil, err
	}
	// BP-157: a claim POST can answer 200 with nothing granted (idempotent
	// replay, activity reconfigured mid-flight — magpie-veyra 0e35525). When
	// the response's own context says claimedToday is still false, re-read
	// once; if it still says false, the day did NOT land and reporting
	// success would hide a missing reward. An unreadable context keeps the
	// old "POST success means claimed" behaviour.
	if res.claimedTodaySeen != nil && !*res.claimedTodaySeen {
		st2, rerr := d.activityContext(ctx, c.AccessToken, code, revision)
		if rerr == nil {
			if !st2.claimedToday {
				return nil, uperr.New(uperr.ClassTransport, 0,
					"lobster: check-in POST answered ok but claimedToday is still false; treated as not landed")
			}
			res.ClaimedDays, res.TotalDays = st2.claimedDays, st2.totalDays
		}
	}
	return res, nil
}

// --- activity endpoints ---

type activitySlot struct {
	slotState      string
	activityCode   string
	configRevision int64
}

func (d *Driver) activitySlot(ctx context.Context, token string) (*activitySlot, error) {
	q := "?placement=desktop_sidebar&clientVersion=" + checkinClientVersion +
		"&containerApiVersion=2&platform=win32"
	data, err := d.activityGet(ctx, "/client-activities/slot"+q, token)
	if err != nil {
		return nil, err
	}
	var out activitySlot
	out.slotState = toStr(data["slotState"])
	if act, ok := data["activity"].(map[string]any); ok {
		out.activityCode = toStr(act["activityCode"])
		out.configRevision = toInt64(act["configRevision"])
	}
	if out.configRevision == 0 {
		out.configRevision = 1
	}
	return &out, nil
}

type activityState struct {
	claimedToday bool
	claimedDays  int64
	totalDays    int64
	reward       int64
}

func (d *Driver) activityContext(ctx context.Context, token, code string, revision int64) (*activityState, error) {
	path := "/client-activities/" + code + "/context?configRevision=" + fmt.Sprint(revision)
	data, err := d.activityGet(ctx, path, token)
	if err != nil {
		return nil, err
	}
	out := &activityState{}
	if st, ok := data["state"].(map[string]any); ok {
		out.claimedToday, _ = st["claimedToday"].(bool)
		out.claimedDays = toInt64(st["claimedDays"])
		out.totalDays = toInt64(st["totalDays"])
		out.reward = toInt64(st["rewardCredits"])
	}
	return out, nil
}

// activityCheckIn performs the claim and reads the granted amount back.
func (d *Driver) activityCheckIn(ctx context.Context, token, code string, revision int64) (*CheckinResult, error) {
	body, _ := json.Marshal(map[string]any{
		"configRevision": revision,
		"idempotencyKey": newUUID(),
		"payload":        map[string]any{},
	})
	path := "/client-activities/" + code + "/actions/check_in"
	data, err := d.activityPost(ctx, path, token, body)
	if err != nil {
		return nil, err
	}
	out := &CheckinResult{}
	if r, ok := data["result"].(map[string]any); ok {
		out.CreditsGranted = toInt64(r["creditsGranted"])
		out.ClaimedDays = toInt64(r["claimedDays"])
		out.TotalDays = toInt64(r["totalDays"])
		out.PeriodKey = toStr(r["periodKey"])
	}
	if ctxg, ok := data["context"].(map[string]any); ok {
		if st, ok := ctxg["state"].(map[string]any); ok {
			out.ClaimedDays = toInt64(st["claimedDays"])
			out.TotalDays = toInt64(st["totalDays"])
			if v, ok := st["claimedToday"].(bool); ok {
				out.claimedTodaySeen = &v
			}
		}
	}
	// A successful POST with a replayed flag and no new credits still means
	// today is done; report it as already-claimed so the note reads right.
	if replayed, _ := data["replayed"].(bool); replayed && out.CreditsGranted == 0 {
		out.AlreadyClaimed = true
	}
	return out, nil
}

// activityGet issues one GET against the activity API with the client identity
// headers the vendor gates the feature on.
func (d *Driver) activityGet(ctx context.Context, path, token string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.apiURL(path), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-store")
	req.Header.Set("User-Agent", "LobsterAI/"+checkinClientVersion)
	d.clientHeaders(req)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return d.doActivity(req, path)
}

func (d *Driver) activityPost(ctx context.Context, path, token string, body []byte) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.apiURL(path), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-store")
	req.Header.Set("User-Agent", "LobsterAI/"+checkinClientVersion)
	d.clientHeaders(req)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return d.doActivity(req, path)
}

func (d *Driver) doActivity(req *http.Request, path string) (map[string]any, error) {
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(req.Context(), err), 0, "lobster: activity: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, classifyHTTP(resp.StatusCode, raw)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, uperr.New(uperr.ClassTransport, 0, "lobster: activity %s decode: %v", path, err)
	}
	if env.Code != 0 {
		// 51102 means the token is dead as far as the activity API is
		// concerned — report it as auth so the pool refreshes/retires the
		// account instead of retrying forever.
		if env.Code == actCodeLoginRequired {
			return nil, uperr.WithFields(uperr.New(uperr.ClassAuth, 401,
				"lobster: activity requires login: %s", orElse(env.Message, "请先登录")), raw)
		}
		e := classifyCode(env.Code, orElse(env.Message, "活动接口返回错误"))
		e.Code = fmt.Sprint(env.Code)
		return nil, uperr.WithFields(e, raw)
	}
	var data map[string]any
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &data)
	}
	if data == nil {
		data = map[string]any{}
	}
	return data, nil
}

// newUUID returns a random RFC-4122 v4 UUID string for idempotency keys.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A failed random read must not silently reuse a key (that would make
		// the server treat a real claim as a replay). Fall back to the clock.
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return strings.Join([]string{s[0:8], s[8:12], s[12:16], s[16:20], s[20:32]}, "-")
}
