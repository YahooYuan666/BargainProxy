// Package autoclaw implements the Zhipu AutoClaw cloud-sandbox channel:
// phone+SMS account login, daily checkin points, and OpenAI-shaped chat via
// the upstream userapi proxy (primary) or the per-account sandbox OpenAI
// endpoint (fallback).
//
// Deliberately NOT implemented: the sandbox relay channel
// (/api/electron/agent/send + /api/events). It forwards only the latest user
// message (no history) and runs the OpenClaw agent, which was observed
// executing shell commands inside the sandbox — wrong semantics for a plain
// chat gateway.
package autoclaw

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

const (
	appID     = "100003"
	clientVer = "1.18.5"

	defaultUserAPIBase = "https://autoglm-acceleration-api.zhipuai.cn"

	// managedChatPath is AutoClaw's CN managed gateway (the AutoClaw 2.1.0
	// account-points channel). It has NO "/v1" segment — a /v1 path answers
	// 404 — and it takes the same X-Auth-* signature family the userapi calls
	// use. Verified live 2026-10-21: 0.8-2.7s per call, images included.
	managedChatPath = "/autoclaw-proxy/proxy/autoclaw/chat/completions"
	// managedChannel/managedVersion identify the 2.1.0 desktop client that
	// this gateway serves; a mismatch is refused.
	managedChannel = "AutoClaw26"
	managedVersion = "2.1.0"

	// The OVERSEAS lane (autoglm.ai) speaks the older 1.x contract, a
	// different dialect from the CN acceleration gateway above:
	//
	//   - X-Channel is "zai" (not "AutoClaw26");
	//   - X-Version must be FOUR segments (1.18.5.851). The gateway checks it
	//     and reports a mismatch as 401 "Invalid token", so a three-segment
	//     version reads as a dead credential (apmengzi/autoclaw-switch hit the
	//     same trap: "1.18.5 -> 401, 1.18.5.851 -> 200");
	//   - the token rides a lower-case "authorization" header;
	//   - no OpenAI SDK fingerprint headers, no X-Session-Id.
	//
	// The two lanes are separate credential universes: a CN token against the
	// overseas host answers 401 {"error":"Invalid token"} (verified live
	// 2026-10-24), which is what makes the overseas pool a genuinely separate
	// quota pool rather than another door into the same one.
	overseasChannel = "zai"
	// overseasVersion follows the installed client (2.2.2 at the time of
	// measurement). The gateway checks it, and reports a mismatch as 401
	// "Invalid token", so it must be bumped when the client is.
	overseasVersion = "2.2.2"
	overseasHost    = "https://autoglm-api.autoglm.ai"
	// overseasChatPath has a /v1 segment the CN gateway does not. Measured
	// 2026-10-24 against the real overseas account:
	//   .../autoclaw/chat/completions     -> 405 (Alibaba edge block page)
	//   .../autoclaw/v1/chat/completions  -> 200 with the same credential
	// so the /v1 segment is load-bearing, not cosmetic.
	overseasChatPath = "/autoclaw-proxy/proxy/autoclaw/v1/chat/completions"
	// realmOverseas is the Account.Realm value for that lane.
	realmOverseas = "global"
	// managedSystemPrompt is mandatory: the gateway checks that the caller
	// speaks for the official client. It is prefixed onto the caller's own
	// system message (or added as one when there is none).
	managedSystemPrompt = "You are AutoClaw. Answer the user directly and concisely."

	// harnessMarker is the exact line the official desktop client puts at the
	// head of its own system prompt, and the managed gateway's prompt gate
	// requires it verbatim. Field A/B 2026-10-24 (scripts/probe/gate_probe.go,
	// same account, same body, one variable changed per run):
	//
	//	marker only                       200
	//	no marker ("You are a helpful…")  406
	//	lower-cased marker                406
	//	truncated marker                  406
	//	"Extra preamble. " + marker       200   (position is NOT the gate)
	//	marker + benign instructions      200
	//	marker + "You are ZCode…"         406   (second gate, see below)
	//
	// So the gate is the marker's PRESENCE, not its position, and it is
	// case- and byte-exact. The reference project reached the same conclusion
	// from the other direction (apmengzi/autoclaw-switch, applyHarnessMarker:
	// "41 chars -> 406; 42 -> 200; all lower-case -> 406").
	//
	// This is a SEPARATE gate from the competing-agent identity check: a prompt
	// can carry the marker and still be refused for saying "You are ZCode…".
	// Both conditions have to hold at once.
	harnessMarker = "OpenClaw plugin-injected system context. This block is not workspace file content."

	// transportRetries is how many times a dropped socket (no response bytes
	// at all) is resent before the failure is handed to the gateway.
	//
	// Field characterisation 2026-10-23 (scripts/acprobe2, 6 fresh processes x
	// 3 requests, 20k-token history, non-trivial system prompt): the FIRST
	// request of a cold process dies with `unexpected EOF` after a near-fixed
	// 30.2s in most rounds, the second sometimes dies too, and every request
	// after that answers 200 in 2-4s. A 22-character system prompt never
	// showed it, so it is the gateway warming up on a large prompt, not the
	// prompt's content.
	//
	// Worst case observed is two consecutive drops (round 5), so three retries
	// covers it with margin while bounding the worst case to ~90s of retries.
	transportRetries = 3

	// proxyChatPath is the overseas direct channel. Kept for reference and
	// tests; the CN managed gateway above carries production traffic
	// (2026-10-21). A CN account's token is refused here with 401.
	proxyChatPath    = "/autoclaw-proxy/proxy/autoclaw/v1/chat/completions"
	sendCodePath     = "/userapi/v1/agent-send-code"
	loginPath        = "/userapi/v1/agent-login/"
	refreshPath      = "/userapi/v1/refresh"
	walletsPath      = "/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw"
	sandboxListPath  = "/agentdr/v2/assistant/sandbox/list"
	sandboxApplyPath = "/agentdr/v2/assistant/sandbox/apply"
	taskCompletePath = "/autoclaw-proxy/proxy/autoclaw-task-complete"
)

// CNModels is the static CN model catalogue. The tdpsk_deepseek-* ids are
// deliberately absent: the CN acceleration gateway answers 400 "非法模型" for
// all three candidates (tdpsk_deepseek-v4.1-flash-202606,
// tdpsk_deepseek-v4-pro-202606 and the reference project's
// tdpsk_deepseek-v4-flash-202605) while the same account serves glm-5.3-flash
// from it at the same moment — so the account has no DeepSeek entitlement
// there, and listing the ids only produced a doomed attempt on every request
// (field-verified 2026-10-23, long-conversation probe + forced-routing run).
//
// The reference project keeps them in a fallback list; that list is not proof
// they work today.
var CNModels = []string{
	"openclaw", "openclaw/default",
	"glm-5.3-flash", "glm-5.2",
	"zai_auto", "zai_auto-fast", "zai_glm-5.3-flash", "zaicoding_glm-5.3",
}

// Creds is the persisted credential blob (canonical snake_case keys; the
// reference project's camelCase files are accepted on import).
type Creds struct {
	AccessToken     string `json:"access_token"`
	RefreshToken    string `json:"refresh_token"`
	ExpiresAt       int64  `json:"expires_at"`
	UID             string `json:"uid"`
	UserName        string `json:"user_name"`
	Phone           string `json:"phone"`
	DeviceID        string `json:"device_id"`
	Region          string `json:"region"`
	SandboxID       string `json:"sandbox_id"`
	SandboxEndpoint string `json:"sandbox_endpoint"`
	EndTimestamp    int64  `json:"end_timestamp"`
}

// ParseCreds decodes both our canonical keys and the reference project's
// camelCase credential files (auths/autoclaw-*.json).
func ParseCreds(raw []byte) (*Creds, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("creds: %w", err)
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				var s string
				if json.Unmarshal(v, &s) == nil && s != "" {
					return s
				}
			}
		}
		return ""
	}
	getInt := func(keys ...string) int64 {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				var n int64
				if json.Unmarshal(v, &n) == nil && n != 0 {
					return n
				}
			}
		}
		return 0
	}
	c := &Creds{
		AccessToken:     get("access_token", "accessToken"),
		RefreshToken:    get("refresh_token", "refreshToken"),
		ExpiresAt:       getInt("expires_at", "expiresAt"),
		UID:             get("uid", "userId"),
		UserName:        get("user_name", "userName"),
		Phone:           get("phone"),
		DeviceID:        get("device_id", "deviceId"),
		Region:          get("region"),
		SandboxID:       get("sandbox_id", "sandboxId"),
		SandboxEndpoint: get("sandbox_endpoint", "sandboxEndpoint"),
		EndTimestamp:    getInt("end_timestamp", "endTimestamp"),
	}
	if c.Region == "" {
		c.Region = "cn"
	}
	if c.AccessToken == "" {
		return nil, fmt.Errorf("access_token missing")
	}
	return c, nil
}

// Marshal encodes the canonical credential blob.
func (c *Creds) Marshal() []byte {
	raw, _ := json.Marshal(c)
	return raw
}

func (c *Creds) bearer() string {
	t := strings.TrimSpace(c.AccessToken)
	if strings.HasPrefix(strings.ToLower(t), "bearer ") {
		return t
	}
	return "Bearer " + t
}

func stripBearer(t string) string {
	t = strings.TrimSpace(t)
	if strings.HasPrefix(strings.ToLower(t), "bearer ") {
		return t[7:]
	}
	return t
}

func needsRefresh(c *Creds) bool {
	if c.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Unix()+600 >= c.ExpiresAt
}

// DeviceIDForPhone derives a stable device id from the phone number
// (reference parity: a persisted random UUID; deterministic here so the
// send-code/login pair survives restarts without extra storage).
func DeviceIDForPhone(phone string) string {
	sum := md5.Sum([]byte("autoclaw-device:" + strings.TrimSpace(phone)))
	h := hex.EncodeToString(sum[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// deviceIDFor returns the device id to sign a refresh with, deriving one from
// the account identity when the credential blob has none.
//
// WHY THIS IS NOT OPTIONAL (BP-119, field 2026-10-26): /userapi/v1/refresh
// answers `code=400001 "Request data error"` for an EMPTY device_id and 200 for
// any UUID-shaped one. Verified live against the real overseas account, one
// variable per run:
//
//	device_id ""          -> 400001 Request data error
//	device_id <uuid>      -> 200 {access_token, refresh_token}
//	device_id absent      -> 400001 Request data error
//
// Accounts imported from the desktop client's credential store have no
// device_id (the client keeps it elsewhere), so EVERY refresh failed silently:
// the hourly task logged "Request data error", the access token ran out, and
// from then on the panel's 刷新额度 read `410000 "Please log in to continue."`
// — which the UI renders as "session dead: Please log in to continue.", i.e.
// "your account is broken", when the account was fine all along.
//
// Deriving from the account id keeps it stable across restarts (a new random
// id per call also works upstream, but a stable one keeps the token's own
// device claim consistent with what we sign with).
func deviceIDFor(cr *Creds, acc *model.Account) string {
	if id := strings.TrimSpace(cr.DeviceID); id != "" {
		return id
	}
	seed := ""
	if acc != nil {
		seed = firstNonEmpty(acc.ID, acc.UID)
	}
	if seed == "" {
		seed = firstNonEmpty(cr.UID, cr.UserName, cr.Phone)
	}
	if seed == "" {
		// Last resort: a value the upstream accepts, at the cost of being
		// different per process. Better a working refresh than a silent 401.
		seed = "anonymous"
	}
	return DeviceIDForPhone(seed)
}

// ensureDeviceID fills in a missing device id and reports whether it changed,
// so the caller knows the credential blob is worth persisting.
func ensureDeviceID(cr *Creds, acc *model.Account) bool {
	if strings.TrimSpace(cr.DeviceID) != "" {
		return false
	}
	cr.DeviceID = deviceIDFor(cr, acc)
	return true
}

func sign(ts, secret string) string {
	sum := md5.Sum([]byte(appID + "&" + ts + "&" + secret))
	return hex.EncodeToString(sum[:])
}

func userapiHeaders(token, secret string) http.Header {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "*/*")
	h.Set("X-Version", "1.12.1")
	h.Set("X-Tm", "web")
	h.Set("X-Product", "autoclaw")
	h.Set("X-Client-Type", "web")
	h.Set("X-Channel", "official")
	h.Set("X-Auth-Appid", appID)
	h.Set("X-Auth-TimeStamp", ts)
	h.Set("X-Auth-Sign", sign(ts, secret))
	h.Set("X-Trace-Id", traceID())
	h.Set("X-Lang", "en")
	if token != "" {
		h.Set("Authorization", token)
	}
	return h
}

// managedChatHeaders builds the header family the CN managed gateway expects.
// Distilled from a live 2.1.0 client (2026-10-21): the X-Auth-* signature is
// mandatory (without it the gateway answers 401 "Invalid token"), and the
// channel/version must name the 2.1.0 build this gateway serves.
func managedChatHeaders(token, secret, model string) http.Header {
	return managedChatHeadersFor(token, secret, model, false)
}

// managedChatHeadersFor builds the header family for one lane.
//
// The overseas lane differs from the CN one in the channel name, the client
// version, and the absence of the CN gateway's client headers (Origin, Referer,
// X-Client-Type, X-Agent-Id). The auth header is the SAME on both:
// X-Authorization. Verified live 2026-10-24 with a real overseas account.
func managedChatHeadersFor(token, secret, model string, overseas bool) http.Header {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if overseas {
		h.Set("Accept", "*/*")
		h.Set("User-Agent", "AutoClaw/"+overseasVersion+" (win-amd64)")
		h.Set("X-Version", overseasVersion)
		h.Set("X-Tm", "win")
		h.Set("X-Product", "autoclaw")
		h.Set("X-Auth-Appid", appID)
		h.Set("X-Auth-TimeStamp", ts)
		h.Set("X-Auth-Sign", sign(ts, secret))
		h.Set("X-Trace-Id", traceID())
		h.Set("X-Lang", "zh-CN")
		h.Set("X-Channel", overseasChannel)
		h.Set("X-Request-Model", model)
		if token != "" {
			h.Set("X-Authorization", token)
		}
		return h
	}
	h.Set("Accept", "*/*")
	h.Set("User-Agent", "AutoClaw/"+managedVersion+" (win-amd64)")
	h.Set("X-Version", managedVersion)
	h.Set("X-Tm", "win")
	h.Set("X-Product", "autoclaw")
	h.Set("X-Auth-Appid", appID)
	h.Set("X-Auth-TimeStamp", ts)
	h.Set("X-Auth-Sign", sign(ts, secret))
	h.Set("X-Trace-Id", traceID())
	h.Set("x_trace_id", "autoclaw-desktop")
	h.Set("X-Lang", "zh-CN")
	h.Set("X-Channel", managedChannel)
	h.Set("X-Client-Type", "pc")
	h.Set("X-Request-Model", model)
	h.Set("X-Request-Id", traceID())
	h.Set("X-Agent-Id", "main")
	h.Set("Origin", "http://localhost:18432")
	h.Set("Referer", "http://localhost:18432/")
	if token != "" {
		h.Set("X-Authorization", token)
	}
	return h
}

// managedLead is what goes at the head of the system message: the harness
// marker first (the gate requires it verbatim), then the persona line.
//
// Both are needed and neither replaces the other: the marker satisfies the
// gateway's "is this our client" check, and the persona gives the model an
// identity that is not a competing product's name. The marker has to survive
// the caller's own prompt verbatim, so it is prepended here rather than
// merged into the caller's text.
func managedLead() string {
	return harnessMarker + "\n\n" + managedSystemPrompt
}

// managedBody rewrites the IR-built body into what the managed gateway takes:
// the official system prompt is mandatory, and max_tokens /
// max_completion_tokens are refused outright (verified 2026-10-21).
//
// bodyModel is what goes in body.model. It is passed separately from the route
// id because the two lanes disagree: both address the route through the
// X-Request-Model header (zai_glm-5.3-flash), but the overseas gateway wants
// the BARE name in the body (glm-5.3-flash) and answers 406 to the prefixed
// one. Verified live 2026-10-24.
func managedBody(body []byte, bodyModel string) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	obj["model"] = bodyModel
	delete(obj, "max_tokens")
	delete(obj, "max_completion_tokens")
	delete(obj, "user")

	msgs, _ := obj["messages"].([]any)
	prefixed := false
	if len(msgs) > 0 {
		if first, ok := msgs[0].(map[string]any); ok {
			if role, _ := first["role"].(string); role == "system" {
				// Prefix inside the caller's own system message: adding a
				// second system message is refused.
				switch c := first["content"].(type) {
				case string:
					first["content"] = managedLead() + "\n\n" + c
					prefixed = true
				case []any:
					parts := make([]any, 0, len(c)+1)
					parts = append(parts, map[string]any{"type": "text", "text": managedLead()})
					first["content"] = append(parts, c...)
					prefixed = true
				}
			}
		}
	}
	if !prefixed {
		obj["messages"] = append([]any{map[string]any{"role": "system", "content": managedLead()}}, msgs...)
	}
	return json.Marshal(obj)
}

// bodyModelFor is what goes in body.model, which the two lanes disagree about.
//
// The CN gateway takes the route id it was addressed with (zai_glm-5.3-flash).
// The overseas gateway wants the BARE model name in the body and the route id
// only in the X-Request-Model header. Measured 2026-10-24 with a real overseas
// account, one variable per run:
//
//	body.model "glm-5.3-flash"     + header zai_glm-5.3-flash  -> 200
//	body.model "zai_glm-5.3-flash" + header zai_glm-5.3-flash  -> 406
//
// The 406 is the same empty-body refusal the prompt gate uses, which is why the
// wrong model id first read as a prompt problem.
func bodyModelFor(cr *Creds, modelID string) string {
	if cr != nil && cr.Region == realmOverseas {
		return bareModelName(modelID)
	}
	return modelID
}

// bareModelName strips the routing prefix a route id carries, turning
// "zai_glm-5.3-flash" into "glm-5.3-flash". Ids without a known prefix are
// returned unchanged.
func bareModelName(id string) string {
	m := strings.TrimSpace(id)
	for _, p := range []string{"zai_", "zaicoding_", "tdpsk_"} {
		if strings.HasPrefix(m, p) {
			return strings.TrimPrefix(m, p)
		}
	}
	return m
}

// chatManaged performs one call on the CN managed gateway.
func (d *Driver) chatManaged(ctx context.Context, cr *Creds, body []byte, modelID string) (*http.Response, error) {
	wire, err := managedBody(body, bodyModelFor(cr, modelID))
	if err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "autoclaw managed body: %v", err)
	}
	secret := d.cfg.AppSecret
	if secret == "" {
		return nil, uperr.New(uperr.ClassInternal, 0, "autoclaw: channels.autoclaw.app_secret is not configured")
	}
	overseas := cr.Region == realmOverseas
	host, path := d.base(), managedChatPath
	if overseas {
		host, path = d.overseasBase(), overseasChatPath
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+path, bytes.NewReader(wire))
	if err != nil {
		return nil, err
	}
	req.Header = managedChatHeadersFor(cr.bearer(), secret, modelID, overseas)
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "autoclaw managed: %v", err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		// The model is passed so a per-route allowance wall (810000) rests only
		// that model instead of the whole account (BP-100).
		return nil, classifyHTTPFor(resp.StatusCode, raw, modelID)
	}
	return resp, nil
}

func proxyChatHeaders(token, model string) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", token)
	h.Set("X-Authorization", token)
	h.Set("X-Request-Model", model)
	h.Set("X-Tm", "mac")
	h.Set("X-Version", clientVer)
	h.Set("X-Product", "autoclaw")
	h.Set("X-Channel", "official")
	h.Set("X-Lang", "zh-CN")
	h.Set("X-Client-Type", "pc")
	h.Set("X-Trace-Id", traceID())
	h.Set("x_trace_id", "bargainproxy")
	h.Set("User-Agent", "AutoClaw/"+clientVer)
	return h
}

func checkinHeaders(token string) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", token)
	h.Set("X-Tm", "mac")
	h.Set("X-Version", clientVer)
	h.Set("X-Product", "autoclaw")
	h.Set("X-Channel", "official")
	h.Set("X-Lang", "zh-CN")
	h.Set("X-Client-Type", "pc")
	h.Set("X-Trace-Id", traceID())
	return h
}

func traceID() string {
	b := make([]byte, 16)
	now := time.Now().UnixNano()
	for i := range b {
		b[i] = byte(now >> ((i % 8) * 8))
	}
	return hex.EncodeToString(b)
}

// Driver implements driver.Driver for AutoClaw.
type Driver struct {
	cfg  config.AutoClawSettings
	http *http.Client // chat streams
	rpc  *http.Client // short userapi calls
}

// New builds the driver.
func New(cfg config.AutoClawSettings, httpClient *http.Client) *Driver {
	if httpClient == nil {
		httpClient = httpx.ChatClient()
	}
	return &Driver{cfg: cfg, http: httpClient, rpc: httpx.RPCClient()}
}

func (d *Driver) base() string {
	if d.cfg.UserAPIBase != "" {
		return strings.TrimRight(d.cfg.UserAPIBase, "/")
	}
	return defaultUserAPIBase
}

// overseasBase resolves the overseas lane host, honouring the config override
// (tests and host migrations both need it).
func (d *Driver) overseasBase() string {
	if d.cfg.OverseasBase != "" {
		return strings.TrimRight(d.cfg.OverseasBase, "/")
	}
	return overseasHost
}

// Channel implements driver.Driver.
func (d *Driver) Channel() string { return model.ChannelAutoClaw }

// Models implements driver.Driver.
func (d *Driver) Models(ctx context.Context) ([]string, error) {
	return append([]string(nil), CNModels...), nil
}

// Stream implements driver.Driver.
func (d *Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	cr, err := ParseCreds(call.Account.CredsJSON)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "autoclaw: %v", err)
	}
	// Two lanes are supported and they are separate credential universes: the
	// CN acceleration gateway and the overseas 1.x gateway. An unknown region
	// is refused rather than silently sent to the CN host, because a wrong
	// lane answers 401 "Invalid token" and reads as a dead account.
	switch cr.Region {
	case "", "cn", realmOverseas:
	default:
		return nil, uperr.New(uperr.ClassBadRequest, 400,
			"autoclaw: region %q is not supported (cn | global)", cr.Region)
	}
	if needsRefresh(cr) && cr.RefreshToken != "" {
		// Refresh failure is not fatal while the current token is still valid;
		// the upstream call will surface an auth error when it is not.
		_ = d.refresh(ctx, call.Account, cr)
	}

	// Scrub a refused identity claim up front rather than waiting for the 406.
	// The claim is a property of the caller's system prompt, not of this
	// request's luck: sending it once to be refused wastes ~100ms and, on the
	// retry path, risks the intermittent dropped connection the field saw
	// (2026-10-23). Scrubbing first makes the common case a single call.
	// buildBody already leaves every other instruction untouched.
	body, modelID, err := buildBodyScrubbed(call)
	if err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "autoclaw: %v", err)
	}
	// The CN managed gateway is the production path since 2026-10-21: it
	// answers in 0.8-2.7s and carries images. The overseas direct channel
	// refuses a CN token (401) and the sandbox OpenAI endpoint is not enabled
	// (404), so neither is tried first any more. The sandbox relay stays as a
	// last resort only when explicitly re-enabled (channels.autoclaw.relay),
	// because it is 5-38s slower and cannot see images at all.
	resp, err := d.chatManaged(ctx, cr, body, modelID)
	if err != nil {
		log.Printf("autoclaw: managed gateway unavailable: %v", err)
		// A 406 here is the channel check refusing a second-person identity
		// claim in the system prompt. Field matrix 2026-10-23: "You are Claude
		// Code…", "You are ZCode…" and "You are Codex…" are refused in ~100ms,
		// while "You are Claude.", "You are Cursor…", "You are Windsurf." and
		// "Claude Code is the assistant" all pass (BP-101, replacing BP-99's
		// whole-prompt move, which turned a fast 406 into an intermittent ~30s
		// dropped connection).
		//
		// The FIRST request already carried the scrubbed body, so a 406 at this
		// point means the claim named a product refusedIdentities does not know
		// yet. Resending the identical scrubbed body would earn the identical
		// 406; the fallback has to change something structural, so the system
		// slot drops to a neutral line. The caller's agent instructions are
		// lost for this one attempt — that is the price of not failing outright.
		if ue, ok := uperr.As(err); ok && ue.Status == http.StatusNotAcceptable {
			if neutral, _, mErr := buildBodyNeutral(call); mErr == nil {
				log.Printf("autoclaw: 406 channel refusal; resending with a neutral system prompt (the identity scrub did not cover it)")
				if resp2, err2 := d.chatManaged(ctx, cr, neutral, modelID); err2 == nil {
					resp, err = resp2, nil
				} else {
					err = err2
					log.Printf("autoclaw: managed gateway unavailable after the neutral-system retry: %v", err)
				}
			}
		}
		// A dropped socket is retried HERE, before it costs the gateway an
		// attempt and (previously) an account rest. Field evidence 2026-10-23:
		// the CN managed gateway closes the connection with no bytes at all
		// (`unexpected EOF`) on roughly one request in five — 15s and 30s were
		// both observed — and the very next identical request answers 200 in
		// 2-4s. Nothing has reached the client at this point, so resending is
		// invisible to the caller and turns an intermittent network event into
		// a normal answer instead of a channel failover.
		if err != nil && isDroppedSocket(err) {
			for try := 1; try <= transportRetries; try++ {
				log.Printf("autoclaw: dropped socket, resending (%d/%d)", try, transportRetries)
				if resp2, err2 := d.chatManaged(ctx, cr, body, modelID); err2 == nil {
					resp, err = resp2, nil
					break
				} else {
					err = err2
					if !isDroppedSocket(err) {
						break
					}
				}
			}
		}
		if err != nil {
			if !d.cfg.RelayEnabled {
				return nil, err
			}
			if re, ok := uperr.As(err); ok && !relayFallbackAllowed(re) {
				return nil, err
			}
			return d.chatRelay(ctx, call, cr, emit)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, classifyHTTP(resp.StatusCode, raw)
	}
	streamBody := httpx.WatchIdle(resp.Body, httpx.IdleTimeout())
	defer streamBody.Close()
	return parseSSE(ctx, streamBody, call, emit)
}

func (d *Driver) chatProxy(ctx context.Context, cr *Creds, body []byte, modelID string) (*http.Response, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "autoclaw: body: %v", err)
	}
	obj["model"] = modelID
	delete(obj, "user")
	wire, _ := json.Marshal(obj)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base()+proxyChatPath, bytes.NewReader(wire))
	if err != nil {
		return nil, err
	}
	req.Header = proxyChatHeaders(cr.bearer(), modelID)
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "autoclaw proxy: %v", err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, classifyHTTP(resp.StatusCode, raw)
	}
	return resp, nil
}

func (d *Driver) chatSandbox(ctx context.Context, acc *model.Account, cr *Creds, body []byte, modelID string) (*http.Response, error) {
	endpoint, err := d.ensureSandbox(ctx, acc, cr)
	if err != nil {
		return nil, err
	}
	// Wake is best-effort: a freshly created sandbox may already be awake.
	if bare, ok := bareRelayBase(endpoint); ok && cr.SandboxID != "" {
		_ = d.wakeSandbox(ctx, cr, bare)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "autoclaw: body: %v", err)
	}
	headers := http.Header{}
	headers.Set("Accept", "application/json, text/plain, */*")
	headers.Set("Content-Type", "application/json")
	headers.Set("Authorization", cr.bearer())
	if agentTarget(modelID) {
		obj["model"] = modelID
	} else {
		obj["model"] = "openclaw"
		headers.Set("x-openclaw-model", modelID)
	}
	wire, _ := json.Marshal(obj)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/v1/chat/completions", bytes.NewReader(wire))
	if err != nil {
		return nil, err
	}
	req.Header = headers
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "autoclaw sandbox: %v", err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, classifyHTTP(resp.StatusCode, raw)
	}
	return resp, nil
}

// ensureSandbox returns the account's sandbox proxy endpoint, creating one on
// the upstream when needed, and persists the refreshed coordinates.
func (d *Driver) ensureSandbox(ctx context.Context, acc *model.Account, cr *Creds) (string, error) {
	now := time.Now().Unix()
	if cr.SandboxID != "" && cr.SandboxEndpoint != "" && (cr.EndTimestamp == 0 || cr.EndTimestamp > now) {
		cr.SandboxEndpoint = normalizeSandboxProxy(cr.SandboxEndpoint, cr.SandboxID)
		d.saveCreds(acc, cr)
		return cr.SandboxEndpoint, nil
	}
	list, err := d.listSandboxes(ctx, cr)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		if err := d.userapiCall(ctx, http.MethodPost, sandboxApplyPath, cr.bearer(), map[string]any{"sandbox_name": "default"}, nil); err != nil {
			return "", err
		}
		if list, err = d.listSandboxes(ctx, cr); err != nil {
			return "", err
		}
	}
	if len(list) == 0 {
		return "", uperr.New(uperr.ClassTransport, 0, "autoclaw: no sandbox available after apply")
	}
	pick := list[0]
	for _, s := range list {
		exp := s.EndTimestamp
		pexp := pick.EndTimestamp
		if (exp == 0 || exp > now) && (pick.SandboxID == "" || (pexp != 0 && pexp <= now)) {
			pick = s
		}
	}
	if pick.SandboxID == "" {
		return "", uperr.New(uperr.ClassTransport, 0, "autoclaw: sandbox response missing sandbox_id")
	}
	cr.SandboxID = pick.SandboxID
	cr.SandboxEndpoint = normalizeSandboxProxy(pick.SandboxEndpoint, pick.SandboxID)
	cr.EndTimestamp = pick.EndTimestamp
	d.saveCreds(acc, cr)
	return cr.SandboxEndpoint, nil
}

type sandboxInfo struct {
	SandboxID       string `json:"sandbox_id"`
	SandboxEndpoint string `json:"sandbox_endpoint"`
	EndTimestamp    int64  `json:"end_timestamp"`
}

func (d *Driver) listSandboxes(ctx context.Context, cr *Creds) ([]sandboxInfo, error) {
	var data json.RawMessage
	if err := d.userapiCall(ctx, http.MethodGet, sandboxListPath, cr.bearer(), nil, &data); err != nil {
		return nil, err
	}
	var arr []sandboxInfo
	if json.Unmarshal(data, &arr) == nil {
		return arr, nil
	}
	var wrap struct {
		SandboxList []sandboxInfo `json:"sandbox_list"`
	}
	if json.Unmarshal(data, &wrap) == nil {
		return wrap.SandboxList, nil
	}
	return nil, uperr.New(uperr.ClassTransport, 0, "autoclaw: unexpected sandbox list shape")
}

func (d *Driver) wakeSandbox(ctx context.Context, cr *Creds, bare string) error {
	body, _ := json.Marshal(map[string]any{
		"access_hold_ms":               600000,
		"reason":                       "bargainproxy chat request",
		"apply_business_busy_cooldown": true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(bare, "/")+"/v1/sandboxes/"+cr.SandboxID+"/wake", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Authorization", cr.bearer())
	resp, err := d.rpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

func (d *Driver) saveCreds(acc *model.Account, cr *Creds) {
	acc.CredsJSON = cr.Marshal()
	acc.CredsDirty = true
}

// Refresh implements driver.Refresher.
func (d *Driver) Refresh(ctx context.Context, acc *model.Account) error {
	cr, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return uperr.New(uperr.ClassAuth, 0, "autoclaw: %v", err)
	}
	return d.refresh(ctx, acc, cr)
}

func (d *Driver) refresh(ctx context.Context, acc *model.Account, cr *Creds) error {
	if strings.TrimSpace(cr.RefreshToken) == "" {
		return uperr.New(uperr.ClassAuth, 0, "autoclaw: no refresh_token")
	}
	// A missing device id makes the upstream refuse the refresh outright
	// (400001), which used to leave the account silently dead — see
	// deviceIDFor. Derive one and persist it with the new token.
	ensureDeviceID(cr, acc)
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	body := map[string]any{
		"source_id":     "autoclaw",
		"device_id":     cr.DeviceID,
		"refresh_token": stripBearer(cr.RefreshToken),
	}
	if err := d.userapiCallFor(ctx, http.MethodPost, refreshPath, cr.bearer(), body, &data, cr.Region == realmOverseas); err != nil {
		return err
	}
	if data.AccessToken == "" {
		return uperr.New(uperr.ClassAuth, 401, "autoclaw: refresh returned no access_token")
	}
	cr.AccessToken = data.AccessToken
	if data.RefreshToken != "" {
		cr.RefreshToken = data.RefreshToken
	}
	d.saveCreds(acc, cr)
	return nil
}

// Probe implements driver.Prober: fetch the points balance.
//
// A dead access token is refreshed once and the read retried (BP-119): the
// panel's 刷新额度 button is the operator's only way to see a balance, and
// answering it with "session dead: Please log in to continue." while the
// account is perfectly healthy — only its token aged out — is the exact wrong
// conclusion. CreditDetail already did this; Probe did not, so the account card
// stayed broken even after the ledger drawer recovered.
func (d *Driver) Probe(ctx context.Context, acc *model.Account) (*driver.ProbeResult, error) {
	cr, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "autoclaw: %v", err)
	}
	res, err := d.probeWith(ctx, cr)
	if err == nil {
		return res, nil
	}
	if ue, ok := uperr.As(err); !ok || ue.Class != uperr.ClassAuth {
		return nil, err
	}
	if rerr := d.refresh(ctx, acc, cr); rerr != nil {
		return nil, err // report the original read failure, not the refresh's
	}
	return d.probeWith(ctx, cr)
}

func (d *Driver) probeWith(ctx context.Context, cr *Creds) (*driver.ProbeResult, error) {
	var data struct {
		TotalBalance json.Number `json:"total_balance"`
	}
	if err := d.userapiCallFor(ctx, http.MethodGet, walletsPath, cr.bearer(), nil, &data, cr.Region == realmOverseas); err != nil {
		return nil, err
	}
	points, _ := data.TotalBalance.Int64()
	credits := &model.Credits{
		Remain: points, Total: points, BalanceOnly: true, UpdatedAt: time.Now(),
	}
	// Expiring points are part of the same snapshot: the panel shows them in
	// the account card so "how much is left and how much will burn" is visible
	// without opening the ledger drawer.
	var exp struct {
		Points any    `json:"expiring_points"`
		Text   string `json:"expiring_points_text"`
	}
	if err := d.userapiCallFor(ctx, http.MethodGet, "/agent-assetmgr/api/v1/points/expiring", cr.bearer(), nil, &exp, cr.Region == realmOverseas); err == nil {
		credits.Expiring = toInt64Any(exp.Points)
	}
	note := "points refreshed"
	if credits.Expiring > 0 && exp.Text != "" {
		note = exp.Text
	}
	return &driver.ProbeResult{Credits: credits, Note: note}, nil
}

// Checkin implements the daily_signin task (idempotent upstream).
//
// The host is the account's own lane (BP-119). This used to hard-code the CN
// gateway, so an overseas account's daily check-in was refused with
// 401 {"error":"Invalid token"} every single day — visible in the task ledger
// as "autoclaw checkin failed ... 401 Invalid token" for the global account
// while the CN account checked in fine. Verified live 2026-10-26: the same
// token that answers 401 on the CN host answers
// {"data":{"already_completed":false,"reward_points":400,"success":true}} on
// the overseas host.
func (d *Driver) Checkin(ctx context.Context, acc *model.Account) (checkedIn bool, credits int64, enable bool, err error) {
	cr, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return false, 0, false, uperr.New(uperr.ClassAuth, 0, "autoclaw: %v", err)
	}
	host := d.base()
	if cr.Region == realmOverseas {
		host = d.overseasBase()
	}
	body, _ := json.Marshal(map[string]any{"task_id": "daily_signin"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+taskCompletePath, bytes.NewReader(body))
	if err != nil {
		return false, 0, false, err
	}
	req.Header = checkinHeaders(cr.bearer())
	resp, err := d.rpc.Do(req)
	if err != nil {
		return false, 0, false, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "autoclaw checkin: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return false, 0, false, classifyHTTP(resp.StatusCode, raw)
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
		return false, 0, false, uperr.New(uperr.ClassTransport, 0, "autoclaw checkin: parse: %v", err)
	}
	if env.Code != 0 {
		return false, 0, false, uperr.New(uperr.ClassTransport, 0, "autoclaw checkin code=%d: %s", env.Code, env.Msg)
	}
	reward, _ := env.Data.RewardPoints.Int64()
	return env.Data.AlreadyCompleted, reward, true, nil
}

// SendCode sends the SMS verification code (panel onboarding).
func (d *Driver) SendCode(ctx context.Context, phone string) error {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return fmt.Errorf("phone required")
	}
	body := map[string]any{"source_id": "autoclaw", "device_id": DeviceIDForPhone(phone), "phone": phone}
	return d.userapiCall(ctx, http.MethodPost, sendCodePath, "", body, nil)
}

// Login exchanges the SMS code for credentials and returns a canonical
// account (panel onboarding).
func (d *Driver) Login(ctx context.Context, phone, code, label string) (*model.Account, error) {
	phone = strings.TrimSpace(phone)
	code = strings.TrimSpace(code)
	if phone == "" || code == "" {
		return nil, fmt.Errorf("phone and code required")
	}
	codeNum, err := strconv.Atoi(code)
	if err != nil {
		return nil, fmt.Errorf("code must be numeric")
	}
	deviceID := DeviceIDForPhone(phone)
	body := map[string]any{
		"source_id": "autoclaw", "device_id": deviceID,
		"phone": phone, "code": codeNum, "platform": "web",
	}
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		UserID       string `json:"user_id"`
		UserName     string `json:"user_name"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := d.userapiCall(ctx, http.MethodPost, loginPath, "", body, &data); err != nil {
		return nil, err
	}
	if data.AccessToken == "" {
		return nil, fmt.Errorf("login failed: no access_token")
	}
	if data.UserID == "" {
		return nil, fmt.Errorf("login failed: no user_id (stable identity required)")
	}
	exp := time.Now().Add(28 * 24 * time.Hour).Unix()
	if data.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(data.ExpiresIn) * time.Second).Unix()
	}
	cr := &Creds{
		AccessToken: data.AccessToken, RefreshToken: data.RefreshToken, ExpiresAt: exp,
		UID: data.UserID, UserName: data.UserName, Phone: phone, DeviceID: deviceID, Region: "cn",
	}
	acc := &model.Account{
		ID: model.ChannelAutoClaw + ":" + data.UserID, Channel: model.ChannelAutoClaw,
		UID: data.UserID, Nickname: firstNonEmpty(label, data.UserName, phone),
		Realm: "cn", CredsJSON: cr.Marshal(),
	}
	return acc, nil
}

// userapiCall performs one signed userapi request and unwraps {code,msg,data}.
func (d *Driver) userapiCall(ctx context.Context, method, path, token string, body any, out any) error {
	return d.userapiCallFor(ctx, method, path, token, body, out, false)
}

// userapiCallFor is userapiCall with an explicit lane. The two credential
// universes do not share an identity API either: an overseas token against the
// CN host answers 410000 "session dead", which reads as an expired login. The
// overseas host serves the same paths, so only the host changes.
func (d *Driver) userapiCallFor(ctx context.Context, method, path, token string, body any, out any, overseas bool) error {
	secret := strings.TrimSpace(d.cfg.AppSecret)
	if secret == "" {
		return uperr.New(uperr.ClassInternal, 0,
			"autoclaw: channels.autoclaw.app_secret 未配置（从所用官方客户端获取后写入 config.json；源码不随附该常量）")
	}
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	host := d.base()
	if overseas {
		host = d.overseasBase()
	}
	req, err := http.NewRequestWithContext(ctx, method, host+path, rd)
	if err != nil {
		return err
	}
	req.Header = userapiHeaders(token, secret)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "autoclaw: %v", err)
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if rerr != nil {
		return uperr.New(uperr.ClassTransport, resp.StatusCode, "autoclaw: read body: %v", rerr)
	}
	if resp.StatusCode != http.StatusOK {
		return classifyHTTP(resp.StatusCode, raw)
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return uperr.New(uperr.ClassTransport, resp.StatusCode, "autoclaw: decode envelope: %v", err)
	}
	if env.Code != 0 {
		return classifyBusiness(env.Code, env.Msg)
	}
	if out != nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return uperr.New(uperr.ClassTransport, resp.StatusCode, "autoclaw: decode data: %v", err)
		}
	}
	return nil
}

func buildBody(call driver.ChatCall) ([]byte, string, error) {
	return buildBodyWith(call, false)
}

// buildBodyScrubbed is the production entry point: the same body with any
// refused identity claim removed from the system prompt.
//
// Scrubbing is unconditional rather than reactive because the claim is a
// property of the caller's prompt, not of one attempt: sending it to be
// refused costs a round trip every single time, and the 406 retry path is
// exactly where the intermittent ~30s dropped connection showed up
// (2026-10-23). Every other instruction is preserved.
func buildBodyScrubbed(call driver.ChatCall) ([]byte, string, error) {
	return buildBodyWith(call, true)
}

// buildBodyNeutral is the last-resort shape: every system message is replaced
// by one neutral line, so no identity claim can survive whatever wording the
// caller used.
//
// Only used after a scrubbed body is STILL refused with 406 — i.e. the claim
// named a product the refusedIdentities list does not know yet. Resending the
// identical scrubbed body would earn the identical 406, so the fallback has to
// change something structural. Trade-off: the caller's agent instructions are
// lost for this one attempt, which is why it is the last resort and not the
// default.
func buildBodyNeutral(call driver.ChatCall) ([]byte, string, error) {
	req := call.Request
	clone := *req
	msgs := make([]ir.Message, 0, len(req.Messages))
	put := false
	for _, m := range req.Messages {
		if m.Role != ir.RoleSystem {
			msgs = append(msgs, m)
			continue
		}
		if put {
			continue // collapse to a single neutral line
		}
		msgs = append(msgs, ir.Message{
			Role:    ir.RoleSystem,
			Content: []ir.ContentPart{{Kind: "text", Text: neutralSystemLine}},
		})
		put = true
	}
	if !put {
		msgs = append([]ir.Message{{
			Role:    ir.RoleSystem,
			Content: []ir.ContentPart{{Kind: "text", Text: neutralSystemLine}},
		}}, msgs...)
	}
	clone.Messages = msgs
	return buildBodyWith(driver.ChatCall{Account: call.Account, UpstreamModel: call.UpstreamModel, Request: &clone}, false)
}

// neutralSystemLine is what the last-resort attempt sends in the system slot.
// It deliberately makes no identity claim at all.
const neutralSystemLine = "You are a helpful assistant. Please answer the user's question."

// refusedIdentities are the product names the CN managed gateway's channel
// check refuses when they appear as a second-person identity claim.
//
// Field matrix 2026-10-23 (scripts/acprobe2, 2 runs each, 20k-token history,
// system text only differing in one line):
//
//	REFUSED (406, ~100ms)     PASSED (200)
//	You are Claude Code…      You are Claude.
//	you are claude code.      You are Cursor, an AI code editor.
//	You are ZCode, a coding…  You are Windsurf.
//	You are Codex, OpenAI's…  You are Gemini CLI, Google's…
//	                          You are Cline / Qwen Code / Amp / Copilot
//	                          Claude Code is the assistant…  (no "you are")
//
// So the trigger is a second-person claim naming one of these competing
// agents — not a generic "you are a helpful assistant", and not merely
// mentioning the product. Each entry is matched case-insensitively anywhere in
// a system message, because "…preamble. You are Claude Code…" was refused too.
var refusedIdentities = []string{
	"claude code",
	"zcode",
	"codex",
}

// refusedIdentityClaim reports whether text carries a second-person identity
// claim naming a refused product, and returns the text with just that claim
// removed.
//
// The removal is deliberately surgical: only the offending sentence goes. The
// caller's other instructions (tool policy, tone, task management) are the
// agent's behaviour and must survive — field A/B 2026-10-23 showed that
// stripping ONLY the identity sentence answers 200 in 4/5 runs while the full
// instruction body is kept, whereas the old whole-prompt move produced 406 on
// every attempt.
func scrubRefusedIdentity(text string) string {
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "you are") {
		return text
	}
	for _, name := range refusedIdentities {
		if !strings.Contains(lower, name) {
			continue
		}
		out := stripClaimSentence(text, name)
		if out != text {
			return out
		}
	}
	return text
}

// stripClaimSentence removes the sentences that both assert "you are" and name
// the product. Sentences are split on sentence-ending punctuation; a dropped
// sentence takes its own terminator with it, and the rest of the text is
// preserved byte for byte.
func stripClaimSentence(text, name string) string {
	var kept []string
	start := 0
	for i := 0; i <= len(text); i++ {
		if i < len(text) {
			switch text[i] {
			case '.', '!', '?', '\n':
			default:
				continue
			}
		}
		sentence := text[start:i]
		low := strings.ToLower(sentence)
		drop := strings.Contains(low, "you are") && strings.Contains(low, name)
		if !drop {
			kept = append(kept, sentence)
			// Keep the terminator only for a sentence that survives, so a
			// removed claim does not leave a dangling "." behind.
			if i < len(text) {
				kept = append(kept, string(text[i]))
			}
		}
		start = i + 1
	}
	out := strings.Join(kept, "")
	return strings.TrimSpace(out)
}

// buildBodySystemAsUser renders the same request with the refused identity
// claim scrubbed out of the caller's system prompt, and the gateway's own
// neutral line in front.
//
// Why not move the whole prompt into a user turn (BP-99)? Field A/B 2026-10-23
// on long conversations: the move converts a fast, deterministic 406 into an
// intermittent ~30s dropped connection (2 of 8 runs), because the gateway then
// inspects a large user message instead of rejecting outright. Scrubbing just
// the claim keeps every other instruction and answers 200 in 4/5 runs.
func buildBodySystemAsUser(call driver.ChatCall) ([]byte, error) {
	wire, _, err := buildBodyWith(call, true)
	return wire, err
}

func buildBodyWith(call driver.ChatCall, scrubIdentity bool) ([]byte, string, error) {
	req := call.Request
	// The scrub rewrites the request's own messages, so a copy is taken first:
	// the caller's IR must not be mutated for the next attempt or the panel.
	if scrubIdentity {
		clone := *req
		clone.Messages = scrubMessages(req.Messages)
		req = &clone
	}
	var messages []map[string]any
	for _, m := range req.Messages {
		wire := map[string]any{"role": string(m.Role)}
		if m.Role == ir.RoleTool {
			wire["content"] = ir.TextOf(m.Content)
		} else if len(m.Content) == 1 && m.Content[0].Kind == "text" {
			wire["content"] = m.Content[0].Text
		} else if len(m.Content) > 0 {
			var parts []map[string]any
			for _, p := range m.Content {
				switch p.Kind {
				case "text":
					parts = append(parts, map[string]any{"type": "text", "text": p.Text})
				case "image":
					parts = append(parts, map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "data:" + p.ImageMIME + ";base64," + p.ImageB64},
					})
				}
			}
			wire["content"] = parts
		} else {
			// BP-160: the CN gateway's parser throws 500 {"message":"parse
			// response failed"} on a message with a null/missing content
			// (plugins autoclaw 0.2.9 tidyBody) — an assistant turn that only
			// carries tool calls is exactly that shape. Always send the key.
			wire["content"] = ""
		}
		if len(m.ToolCalls) > 0 {
			var tcs []map[string]any
			for _, tc := range m.ToolCalls {
				args := tc.Arguments
				if strings.TrimSpace(args) == "" {
					// A tool call with empty arguments must carry "{}": an
					// empty string is not JSON and the gateway 500s on it
					// (plugins autoclaw 0.2.9, issue #437).
					args = "{}"
				}
				tcs = append(tcs, map[string]any{
					"id": tc.ID, "type": "function",
					"function": map[string]any{"name": tc.Name, "arguments": args},
				})
			}
			wire["tool_calls"] = tcs
		}
		if m.ToolCallID != "" {
			wire["tool_call_id"] = m.ToolCallID
		}
		messages = append(messages, wire)
	}
	body := map[string]any{"messages": messages, "stream": true}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if t.Parameters != nil {
				fn["parameters"] = t.Parameters
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		body["tools"] = tools
	}
	if req.RequestedEffort != "" {
		body["reasoning_effort"] = req.RequestedEffort
	}
	wire, err := json.Marshal(body)
	return wire, stripRoutePrefix(strings.TrimSpace(call.UpstreamModel)), err
}

// scrubMessages rewrites only the system messages, removing any second-person
// claim naming a refused product (see refusedIdentities) and leaving every
// other instruction intact. Non-system messages are untouched.
//
// Why scrub rather than move: field A/B 2026-10-23 (long conversations) showed
// that moving the whole prompt into a user turn turns a deterministic 406 into
// an intermittent ~30s dropped connection, while deleting just the offending
// sentence answers 200 and keeps the agent's actual behaviour.
func scrubMessages(messages []ir.Message) []ir.Message {
	out := make([]ir.Message, 0, len(messages))
	for _, m := range messages {
		if m.Role != ir.RoleSystem {
			out = append(out, m)
			continue
		}
		text := ir.TextOf(m.Content)
		cleaned := scrubRefusedIdentity(text)
		if cleaned == text {
			out = append(out, m)
			continue
		}
		m.Content = []ir.ContentPart{{Kind: "text", Text: cleaned}}
		out = append(out, m)
	}
	return out
}

func stripRoutePrefix(m string) string {
	for _, p := range []string{"global/", "cn/"} {
		if strings.HasPrefix(m, p) {
			return m[len(p):]
		}
	}
	return m
}

func agentTarget(m string) bool {
	m = strings.ToLower(strings.TrimSpace(m))
	return m == "openclaw" || m == "openclaw/default" || strings.HasPrefix(m, "openclaw/") || strings.HasPrefix(m, "openclaw:") || strings.HasPrefix(m, "agent:")
}

// --- SSE (OpenAI shape) ---

func parseSSE(ctx context.Context, r io.Reader, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	out := &ir.ChatResponse{Model: call.Request.Model, CreatedAt: time.Now().Unix(), Message: ir.Message{Role: ir.RoleAssistant}}
	var (
		textBuf    strings.Builder
		thinkBuf   strings.Builder
		tools      = map[int]*ir.ToolCall{}
		toolOrder  []int
		usage      ir.Usage
		stop       ir.StopReason
		gotUsage   bool
		terminated bool
	)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "autoclaw: stream canceled: %v", err)
		}
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			terminated = true
			break
		}
		if fe := detectFrameError(payload); fe != nil {
			return nil, fe
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
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
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.ReasoningContent != "" {
				thinkBuf.WriteString(ch.Delta.ReasoningContent)
				if err := emit(ir.StreamEvent{Kind: ir.EvThinking, Delta: ch.Delta.ReasoningContent}); err != nil {
					return nil, err
				}
			}
			if ch.Delta.Content != "" {
				textBuf.WriteString(ch.Delta.Content)
				if err := emit(ir.StreamEvent{Kind: ir.EvText, Delta: ch.Delta.Content}); err != nil {
					return nil, err
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				acc, ok := tools[tc.Index]
				if !ok {
					acc = &ir.ToolCall{ID: tc.ID, Name: tc.Function.Name}
					tools[tc.Index] = acc
					toolOrder = append(toolOrder, tc.Index)
					if err := emit(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: tc.Index, ToolCall: &ir.ToolCall{ID: acc.ID, Name: acc.Name}}); err != nil {
						return nil, err
					}
				}
				if tc.Function.Arguments != "" {
					acc.Arguments += tc.Function.Arguments
					if err := emit(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: tc.Index, Delta: tc.Function.Arguments}); err != nil {
						return nil, err
					}
				}
			}
			if ch.FinishReason != "" {
				terminated = true
				stop = mapFinish(ch.FinishReason)
			}
		}
		if chunk.Usage != nil {
			usage = ir.Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
			if chunk.Usage.PromptTokensDetails != nil {
				usage.CachedInputTokens = chunk.Usage.PromptTokensDetails.CachedTokens
				usage.CacheKnown = true
			}
			gotUsage = true
		}
	}
	if err := sc.Err(); err != nil {
		if httpx.IdleFired(r) {
			return nil, uperr.New(uperr.ClassUpstreamTimeout, 0, "autoclaw: stream stalled (no data for %s)", httpx.IdleTimeout())
		}
		if ctx.Err() != nil {
			return nil, uperr.New(uperr.ClassCanceled, 0, "autoclaw: stream canceled: %v", ctx.Err())
		}
		return nil, uperr.New(uperr.ClassTransport, 0, "autoclaw: stream read: %v", err)
	}
	if ctx.Err() != nil {
		return nil, uperr.New(uperr.ClassCanceled, 0, "autoclaw: stream canceled: %v", ctx.Err())
	}
	if !terminated {
		return nil, uperr.New(uperr.ClassTransport, 0, "autoclaw: stream ended without terminal signal")
	}

	for _, idx := range toolOrder {
		out.Message.ToolCalls = append(out.Message.ToolCalls, *tools[idx])
	}
	if len(out.Message.ToolCalls) > 0 && stop == ir.StopEnd {
		stop = ir.StopToolUse
	}
	if stop == "" {
		stop = ir.StopEnd
	}
	out.Message.Content = []ir.ContentPart{{Kind: "text", Text: textBuf.String()}}
	out.Thinking = thinkBuf.String()
	out.StopReason = stop
	out.Usage = usage
	if gotUsage {
		if err := emit(ir.StreamEvent{Kind: ir.EvUsage, Usage: &usage}); err != nil {
			return nil, err
		}
	}
	if err := emit(ir.StreamEvent{Kind: ir.EvDone, Stop: stop}); err != nil {
		return nil, err
	}
	return out, nil
}

func mapFinish(reason string) ir.StopReason {
	switch strings.ToLower(reason) {
	case "length":
		return ir.StopLength
	case "tool_calls":
		return ir.StopToolUse
	case "content_filter":
		return ir.StopContentFilter
	default:
		return ir.StopEnd
	}
}

func detectFrameError(payload string) *uperr.Error {
	if !strings.Contains(payload, `"error"`) {
		return nil
	}
	var doc struct {
		Error struct {
			Code    any    `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		return nil
	}
	if doc.Error.Message == "" && doc.Error.Type == "" && doc.Error.Code == nil {
		return nil
	}
	status := 500
	if n, ok := doc.Error.Code.(float64); ok && n >= 400 && n <= 599 {
		status = int(n)
	} else {
		hint := strings.ToLower(doc.Error.Type + " " + fmt.Sprint(doc.Error.Code))
		switch {
		case strings.Contains(hint, "rate") || strings.Contains(hint, "limit") || strings.Contains(hint, "overload"):
			status = 429
		case strings.Contains(hint, "quota") || strings.Contains(hint, "insufficient"):
			status = 402
		case strings.Contains(hint, "invalid") || strings.Contains(hint, "param"):
			status = 400
		case strings.Contains(hint, "auth"):
			status = 401
		}
	}
	return classifyHTTP(status, []byte(payload))
}

var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "not enough credit",
	"insufficient balance", "balance not enough", "limit reached", "usage limit",
	"余额不足", "额度不足", "积分不足", "积分用完", "额度用尽", "没有积分", "配额不足",
	// 810000 is how the gateway says "this model's free allowance is used up".
	// It arrives as a business code rather than as wording, and its absence
	// from this list used to push the answer into the generic bucket, where the
	// account was charged a failure for a quota wall every account hits
	// (BP-100; evidence: 2642086672/autoclaw-oneclick assets/server_2x.mjs).
	"810000",
}

var deadMarkers = []string{
	"not logged in", "unauthorized", "invalid token", "token expired",
	"login required", "session expired", "offline user session",
	"未登录", "登录已过期", "请先登录", "token 失效",
}

// isDroppedSocket reports whether err is a connection that died before any
// response bytes arrived. Only that shape is safe to resend blindly: the
// upstream never started answering, so no work was charged and nothing has
// reached the client.
//
// Deliberately narrow — a read error mid-stream is NOT here. Once frames are
// flowing, resending would duplicate output the caller already received.
func isDroppedSocket(err error) bool {
	if err == nil {
		return false
	}
	ue, ok := uperr.As(err)
	if !ok {
		return false
	}
	if ue.Class != uperr.ClassTransport {
		return false
	}
	msg := strings.ToLower(ue.Message)
	for _, m := range []string{
		"unexpected eof",
		"connection reset by peer",
		"http2: stream closed",
		"server closed idle connection",
		"broken pipe",
		"eof",
	} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// quotaCodes are the gateway's own codes for "this model's allowance is out",
// as opposed to the account being broken. They are matched as codes, never as
// substrings of an arbitrary body, so a message that merely mentions a number
// cannot trip them.
//
//	810000  a model's free allowance is used up (allowances are per route: one
//	        model being out says nothing about the account's other models)
//	810001  peak-hour throttle, retry in a moment — not an allowance wall
//	810002  the same throttle, on another path
var quotaCodes = map[string]bool{"810000": true}

// busyCodes are transient throttles: never treated as an allowance wall.
var busyCodes = map[string]bool{"810001": true, "810002": true}

// bannedCodes are the gateway's own codes for "this account is refused", as
// opposed to "this sign-in expired". 410004 arrives as
// {"code":410004,"message":"The account has been banned"} on every route
// (magpie-community/plugins 0.2.5, 2026-10-22, where reading it as an expired
// sign-in dropped the token while the user never saw why).
var bannedCodes = map[string]bool{"410004": true}

// bannedMarkers are the same refusal stated in words, for bodies without a
// code envelope. Kept narrower than magpie's regex: each entry is an
// account-level refusal, never a per-request rejection.
var bannedMarkers = []string{
	"account has been banned", "account is banned", "account was banned",
	"account banned", "account suspended", "account is suspended",
	"account frozen", "account disabled", "account blocked",
	"账号异常", "账户异常", "封禁", "封号", "账号被冻结", "账号停用",
}

// codeOf pulls the gateway's business code out of a body, as a string. It
// reads the shapes seen in the field: {"code":"810000"}, {"code":810000},
// {"error":{"code":...}} and {"data":{"code":...}}.
func codeOf(body []byte) string {
	var doc struct {
		Code  any `json:"code"`
		Error struct {
			Code any `json:"code"`
		} `json:"error"`
		Data struct {
			Code any `json:"code"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	for _, v := range []any{doc.Code, doc.Error.Code, doc.Data.Code} {
		switch c := v.(type) {
		case string:
			if c != "" && c != "0" {
				return c
			}
		case float64:
			if c != 0 {
				return strconv.FormatInt(int64(c), 10)
			}
		}
	}
	return ""
}

func classifyHTTP(status int, body []byte) *uperr.Error {
	text := string(body)
	return classify(status, "", text, strings.ToLower(text))
}

// classifyHTTPFor is classifyHTTP with the route the request asked for, so a
// per-model allowance wall can name the model it belongs to. Without that the
// pool must rest the whole account, which is wrong for a channel whose
// allowances are counted per route (BP-100).
func classifyHTTPFor(status int, body []byte, model string) *uperr.Error {
	e := classifyHTTP(status, body)
	// Only a code-scoped wall names a model. A balance error recognized from
	// wording means the account's shared balance is spent, which is every
	// model's problem — scoping that to one route would keep the other routes
	// hammering an empty account (BP-100).
	if e != nil && e.Model == "" && model != "" && e.Code != "" && quotaCodes[e.Code] {
		e.Model = model
	}
	return e
}

func classifyBusiness(code int, msg string) *uperr.Error {
	return classify(0, strconv.Itoa(code), msg, strings.ToLower(msg))
}

func classify(status int, code, text, lower string) *uperr.Error {
	clipped := clipText(text, 300)
	// A business code is judged before any wording: the gateway answers an
	// allowance wall as a code, and matching words instead would both miss it
	// and create false positives from unrelated text (BP-100).
	if code == "" {
		code = codeOf([]byte(text))
	}
	switch {
	case code == "410000":
		return uperr.New(uperr.ClassAuth, 401, "autoclaw: session dead: %s", clipped)
	case bannedCodes[code]:
		// 410004 "The account has been banned" — the ACCOUNT is refused on
		// every route, not its sign-in. Refreshing gets the same account back
		// with the same refusal, so filing this as ClassAuth made the pool rest
		// the account for 10 minutes, let the refresh task try, fail, and
		// repeat forever. ClassAccountRestricted is the terminal state the pool
		// already understands (it is what workbuddy's 403/11140 uses): the
		// account leaves routing and the panel shows it for a human to decide.
		//
		// Judged by code before the 401/403 fallback, and by wording as well,
		// because the ban is also reported without a code envelope
		// (magpie-community/plugins 0.2.5, 2026-10-22, same code and wording).
		return uperr.New(uperr.ClassAccountRestricted, 403,
			"autoclaw: account banned by upstream (%s): %s", code, clipped)
	case quotaCodes[code]:
		// Model-scoped, not account-scoped: allowances are per route, so the
		// error carries the model it ran out on. CooldownScope turns that into
		// a single-model hold instead of stopping the whole account.
		e := uperr.New(uperr.ClassHardCredit, 402, "autoclaw: %s allowance used up (%s): %s", clipped, code, clipped)
		e.Code = code
		return e
	case busyCodes[code]:
		// A transient throttle: retry in a moment, no allowance consumed.
		e := uperr.New(uperr.ClassSoftRate, 429, "autoclaw: throttled (%s): %s", code, clipped)
		e.Code = code
		return e
	case hasMarker(lower, hardMarkers):
		return uperr.New(uperr.ClassHardCredit, 402, "autoclaw: %s", clipped)
	case hasMarker(lower, bannedMarkers):
		// The ban also arrives without a code envelope. Judged BEFORE the
		// dead markers: "account suspended" and "session expired" are both
		// 401/403 shapes, and only one of them is worth a refresh.
		return uperr.New(uperr.ClassAccountRestricted, 403, "autoclaw: account banned by upstream: %s", clipped)
	case hasMarker(lower, deadMarkers):
		return uperr.New(uperr.ClassAuth, 401, "autoclaw: %s", clipped)
	case status == 402:
		return uperr.New(uperr.ClassHardCredit, status, "autoclaw: %s", clipped)
	case status == 429:
		return uperr.New(uperr.ClassSoftRate, status, "autoclaw: %s", clipped)
	case status == 401 || status == 403:
		return uperr.New(uperr.ClassAuth, status, "autoclaw: %s", clipped)
	case status == 400 || status == 404:
		return uperr.New(uperr.ClassBadRequest, status, "autoclaw: %s", clipped)
	// 406 Not Acceptable with an empty body is the CN managed gateway's content
	// check refusing the request, not a broken channel: the same request
	// answers 200 once the refused text moves out of the system message (field
	// isolation 2026-10-22 — a system prompt opening with "You are Claude Code."
	// is refused every time, while the identical text after a "Hi.\n" prefix, or
	// any other system text, passes). Classifying it as ClassInternal charged
	// the account a failure for every such request: 153 strikes in one evening
	// (BP-98).
	case status == 406:
		return uperr.New(uperr.ClassBadRequest, status,
			"autoclaw: request refused by the gateway's content check (406): %s", clipped)
	case status >= 500:
		return uperr.New(uperr.ClassTransport, status, "autoclaw: %s", clipped)
	case status != 0:
		return uperr.New(uperr.ClassInternal, status, "autoclaw: %s", clipped)
	default:
		return uperr.New(uperr.ClassTransport, 0, "autoclaw: %s", clipped)
	}
}

func hasMarker(lower string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func normalizeSandboxProxy(endpoint, id string) string {
	e := strings.TrimSpace(endpoint)
	switch {
	case strings.HasPrefix(e, "wss://"):
		e = "https://" + e[len("wss://"):]
	case strings.HasPrefix(e, "ws://"):
		e = "http://" + e[len("ws://"):]
	}
	const suffix = "/autoclaw-cloud"
	if i := strings.Index(e, suffix); i >= 0 {
		e = e[:i+len(suffix)]
	}
	return strings.TrimRight(e, "/") + "/proxy/" + id
}

func bareRelayBase(proxy string) (string, bool) {
	i := strings.Index(proxy, "/proxy/")
	if i < 0 {
		return strings.TrimRight(proxy, "/"), false
	}
	return proxy[:i], true
}
