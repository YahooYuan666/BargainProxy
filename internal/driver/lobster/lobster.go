// Package lobster implements the NetEase Youdao LobsterAI (有道龙虾) channel.
//
// This is an UNOFFICIAL channel: the endpoints belong to the desktop client's
// own backend (lobsterai-server.youdao.com), not to a published developer API,
// and every call needs a user's own login. Clean-room implementation from the
// public LobsterAI desktop source (netease-youdao/LobsterAI, MIT) and the
// third-party opencode-lobster-auth plugin; see research/2026-10-22-lobster
// for the evidence trail.
//
// Three upstream behaviours shape this driver, and all three are traps for a
// naive openai-compat passthrough:
//
//  1. The proxy always answers with SSE, even when the request says
//     stream:false. A non-streaming client would otherwise hang waiting for a
//     JSON body that never arrives.
//  2. Errors arrive as `event: error` inside an HTTP 200 stream. Passed
//     through verbatim, the gateway would count a failed turn as success: no
//     penalty, no rotation, sticky pinned to a dead account. Every frame is
//     therefore inspected before it is trusted.
//  3. Thinking gears use a private `lobsterai_options` field, NOT
//     reasoning_effort. Worse, sending it for a model whose catalog entry has
//     no thinkingConfig is a hard 400 (code 4000) — 22 of the 30 catalog
//     models are in that state. The field is therefore sent only when the
//     model advertised a gear table.
package lobster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// limitWords matches the vendor's plain-language rate-limit wording, which
// arrives as a 400-shaped business error rather than a 429.
var limitWords = regexp.MustCompile(`(?i)限流|频率|过快|过于频繁|rate ?limit|too many`)

// Creds is the persisted credential shape for a lobster account.
type Creds struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpiresAt is the JWT's own exp in unix seconds (0 = unknown).
	ExpiresAt int64  `json:"expires_at,omitempty"`
	UID       string `json:"uid,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
}

// Driver implements driver.Driver for the lobster channel.
type Driver struct {
	cfg  config.LobsterSettings
	http *http.Client // streaming chat calls (no total timeout + watchdogs)
	rpc  *http.Client // short quota/model calls (total timeout)
	cat  catalog      // cached public model catalog (thinking-gear capability)
}

// New builds the driver.
func New(cfg config.LobsterSettings, httpClient *http.Client) *Driver {
	if httpClient == nil {
		httpClient = httpx.ChatClient()
	}
	return &Driver{cfg: cfg, http: httpClient, rpc: httpx.RPCClient()}
}

// Channel implements driver.Driver.
func (d *Driver) Channel() string { return model.ChannelLobster }

// ParseCreds decodes a credential blob.
func ParseCreds(raw []byte) (*Creds, error) {
	var c Creds
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("lobster: parse creds: %w", err)
	}
	if c.AccessToken == "" {
		return nil, fmt.Errorf("lobster: access_token missing")
	}
	return &c, nil
}

func (d *Driver) serverBase() string { return strings.TrimRight(d.cfg.ServerBase, "/") }
func (d *Driver) portalBase() string { return strings.TrimRight(d.cfg.PortalBase, "/") }
func (d *Driver) chatURL() string    { return d.serverBase() + "/api/proxy/v1/chat/completions" }
func (d *Driver) apiURL(path string) string {
	return d.serverBase() + "/api" + path
}

// clientHeaders sets the desktop client's advertised identity. Both headers
// are the vendor's own public constants and are optional upstream — they gate
// feature switches (thinking-level control, K3 agentic mode), not access.
func (d *Driver) clientHeaders(req *http.Request) {
	if v := d.cfg.ClientVersion; v != "" {
		req.Header.Set("X-LobsterAI-Client-Version", v)
	}
	if v := d.cfg.ClientCapabilities; v != "" {
		req.Header.Set("X-LobsterAI-Client-Capabilities", v)
	}
}

// --- JWT helpers ---

// jwtPayload decodes a JWT payload without verifying the signature. The token
// came straight from the upstream over TLS; verification is the upstream's
// job, and all this driver needs is the stable account identity and exp.
func jwtPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	s := strings.NewReplacer("-", "+", "_", "/").Replace(parts[1])
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

// JWTPayload exposes the decoded payload for the panel's import path.
func JWTPayload(token string) map[string]any { return jwtPayload(token) }

// JWTAudience extracts the stable upstream identity: the `yid` claim when
// present, else the JWT subject. Returns "" when the token is unreadable.
func JWTAudience(token string) string {
	p := jwtPayload(token)
	if p == nil {
		return ""
	}
	if v, ok := p["yid"]; ok {
		if s := toStr(v); s != "" {
			return s
		}
	}
	if v, ok := p["sub"]; ok {
		return toStr(v)
	}
	return ""
}

// jwtExp returns the token's expiry in unix seconds, or 0 when unreadable.
func jwtExp(token string) int64 {
	p := jwtPayload(token)
	if p == nil {
		return 0
	}
	switch n := p["exp"].(type) {
	case float64:
		return int64(n)
	case json.Number:
		if v, err := n.Int64(); err == nil {
			return v
		}
	}
	return 0
}

// JWTExp exposes the token expiry (unix seconds, 0 = unreadable) so the
// panel's login flow can persist it without duplicating the decode logic.
func JWTExp(token string) int64 { return jwtExp(token) }

// --- account helpers ---

func (d *Driver) credsOf(acc *model.Account) (*Creds, error) {
	if acc == nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "lobster: no account bound")
	}
	return ParseCreds(acc.CredsJSON)
}

// --- upstream error handling ---

// envelope is the /api/* JSON envelope: business errors arrive with HTTP 200
// and a non-zero code.
type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// classifyCode maps a LobsterAI business code to a uperr class. The vendor's
// codes are the only reliable signal: the transport status is 200 for most
// business failures.
func classifyCode(code int, message string) *uperr.Error {
	switch code {
	case 40100:
		return uperr.New(uperr.ClassAuth, 401, "lobster: %s", message)
	case 40300:
		return uperr.New(uperr.ClassWAF, 403, "lobster: %s", message)
	case 42900:
		// "请求过于频繁" is a rolling-window throttle, not an allowance:
		// seconds-level rest (sub2api #8286) instead of the pool's 10-minute
		// soft base (BP-153).
		e := uperr.New(uperr.ClassSoftRate, 429, "lobster: %s", message)
		e.ResetAt = time.Now().Add(5 * time.Second)
		return e
	case 40301:
		// Model not entitled for this account (subscription / plan gate).
		return uperr.New(uperr.ClassHardCredit, 403, "lobster: %s", message)
	case 40201:
		// BP-156: the free allowance is used up ("免费额度已用完，请升级套餐"),
		// arriving inside an event:error frame on an HTTP 200 stream
		// (jacek4yang/lobsterai-proxy f1b4b1a). It used to fall through to
		// ClassBadRequest — no cooldown, the drained account stayed
		// selectable and the client saw a bare 400.
		return uperr.New(uperr.ClassHardCredit, 403, "lobster: %s", message)
	case 40302:
		// Server-side risk-control ban ("账号已被禁用"). Coding2API ran the
		// same experiment on 2026-10-26 and reverted the channel: a fresh
		// account answered 40302 on its FIRST call, client-shape fixes did not
		// help, and the login endpoint keeps issuing tokens regardless — so
		// this is terminal for the account, not a request fault. Leave routing
		// instead of hammering a dead number.
		return uperr.New(uperr.ClassAccountRestricted, 403, "lobster: account banned upstream: %s", message)
	}
	// Limit wording appears in plain 400-shaped messages too; treat it as a
	// rate event so the pool rests instead of striking the account. The
	// wording here is purely frequency-shaped (限流/频率/过快/rate limit), so a
	// seconds-level rest matches what the wall actually asks for (BP-153).
	if limitWords.MatchString(message) {
		e := uperr.New(uperr.ClassSoftRate, 429, "lobster: %s", message)
		e.ResetAt = time.Now().Add(5 * time.Second)
		return e
	}
	return uperr.New(uperr.ClassBadRequest, 400, "lobster: %s", message)
}

// saidOrStatus renders what the upstream said, and — when it said NOTHING —
// the HTTP status instead.
//
// A 5xx with an EMPTY body is a real shape here (field 2026-10-25: six legs
// logged `class=transport: lobster: ` with nothing after the colon). The
// status was in ue.Status the whole time, but the log line only prints the
// message, so "upstream returned 500 and said nothing" and "upstream returned
// 502 and said nothing" were indistinguishable from the logs — which is why
// the first question on every one of these is "what did the upstream actually
// answer?". An empty body is information; print it as such.
func saidOrStatus(status int, body []byte) string {
	if s := strings.TrimSpace(truncate(body, 300)); s != "" {
		return s
	}
	return fmt.Sprintf("empty response body (HTTP %d)", status)
}

// classifyHTTP maps a non-200 transport status.
func classifyHTTP(status int, body []byte) *uperr.Error {
	// A business envelope may ride a non-200 status; the code is authoritative.
	var env envelope
	if json.Unmarshal(body, &env) == nil && env.Code != 0 {
		return uperr.WithFields(classifyCode(env.Code, orElse(env.Message, truncate(body, 300))), body)
	}
	said := saidOrStatus(status, body)
	switch {
	case status == 401:
		return uperr.WithFields(uperr.New(uperr.ClassAuth, status, "lobster: %s", said), body)
	case status == 403:
		if uperr.IsEdgeBlock(body) {
			return uperr.WithFields(uperr.EdgeBlocked(status, "lobster: edge blocked: %s", said), body)
		}
		return uperr.WithFields(uperr.New(uperr.ClassWAF, status, "lobster: %s", said), body)
	case status == 429:
		return uperr.WithFields(uperr.New(uperr.ClassSoftRate, status, "lobster: %s", said), body)
	case status == 402:
		return uperr.WithFields(uperr.New(uperr.ClassHardCredit, status, "lobster: %s", said), body)
	case status == 404:
		return uperr.WithFields(uperr.New(uperr.ClassNotFound, status, "lobster: %s", said), body)
	case status == 400:
		return uperr.WithFields(uperr.New(uperr.ClassBadRequest, status, "lobster: %s", said), body)
	case status >= 500:
		return uperr.WithFields(uperr.New(uperr.ClassTransport, status, "lobster: %s", said), body)
	default:
		return uperr.WithFields(uperr.New(uperr.ClassInternal, status, "lobster: %s", said), body)
	}
}

// frameErrorOf extracts an error from an SSE `event: error` frame. The upstream
// sends `{"error":{...}}` or a bare `{"code":..,"message":..}`.
func frameErrorOf(data string) *uperr.Error {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" || trimmed == "[DONE]" {
		return nil
	}
	var doc struct {
		Error *struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Code    any    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(trimmed), &doc) != nil {
		return nil
	}
	if doc.Error != nil {
		code := toInt(doc.Error.Code)
		return classifyCode(code, orElse(doc.Error.Message, "上游返回错误"))
	}
	// A bare code without a message is not an error frame (chunks carry
	// "object"/"choices" instead), so require a message or a non-zero code.
	code := toInt(doc.Code)
	if code != 0 || doc.Message != "" {
		if code == 0 && doc.Message == "" {
			return nil
		}
		return classifyCode(code, orElse(doc.Message, "上游返回错误"))
	}
	return nil
}

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		var out int
		if _, err := fmt.Sscanf(n, "%d", &out); err == nil {
			return out
		}
	}
	return 0
}

func orElse(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// Models returns the channel's model list. The public pricing catalog needs no
// login, which lets the router validate bindings before an account exists.
func (d *Driver) Models(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		d.apiURL("/models/pricing-catalog"), nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "lobster: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, classifyHTTP(resp.StatusCode, raw)
	}
	var env struct {
		Data struct {
			TextModels []struct {
				ModelID string `json:"modelId"`
			} `json:"textModels"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, uperr.New(uperr.ClassTransport, 0, "lobster: catalog decode: %v", err)
	}
	out := make([]string, 0, len(env.Data.TextModels))
	for _, m := range env.Data.TextModels {
		if m.ModelID != "" {
			out = append(out, m.ModelID)
		}
	}
	if len(out) == 0 {
		return nil, uperr.New(uperr.ClassNotFound, 0, "lobster: catalog empty")
	}
	return out, nil
}

// --- account state ---

// Probe implements driver.Prober: reads the quota and profile-summary
// endpoints so the panel shows a balance without spending a chat request.
func (d *Driver) Probe(ctx context.Context, acc *model.Account) (*driver.ProbeResult, error) {
	c, err := d.credsOf(acc)
	if err != nil {
		return nil, err
	}
	quota, err := d.apiGet(ctx, "/user/quota", c.AccessToken)
	if err != nil {
		return nil, err
	}
	profile, err := d.apiGet(ctx, "/user/profile-summary", c.AccessToken)
	if err != nil {
		// The balance is nice-to-have; the quota read already proved the
		// credential works, so surface a note instead of failing the probe.
		return &driver.ProbeResult{Note: "quota ok; profile unavailable"}, nil
	}
	return probeFrom(quota, profile), nil
}

// apiGet performs one envelope-API GET and returns the decoded data block.
func (d *Driver) apiGet(ctx context.Context, path, token string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.apiURL(path), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	d.clientHeaders(req)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "lobster: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, classifyHTTP(resp.StatusCode, raw)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, uperr.New(uperr.ClassTransport, 0, "lobster: %s decode: %v", path, err)
	}
	if env.Code != 0 {
		return nil, classifyCode(env.Code, orElse(env.Message, "上游返回错误"))
	}
	var out map[string]any
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &out)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// Refresh implements driver.Refresher.
func (d *Driver) Refresh(ctx context.Context, acc *model.Account) error {
	c, err := d.credsOf(acc)
	if err != nil {
		return err
	}
	if c.RefreshToken == "" {
		// A token with no refresh path is not an error by itself: the access
		// token may still be valid for weeks. Report auth only when expired.
		if exp := jwtExp(c.AccessToken); exp > 0 && time.Now().Unix() >= exp {
			return uperr.New(uperr.ClassAuth, 401, "lobster: access token expired and no refresh token")
		}
		return nil
	}
	// Refresh early (6h lead, matching the reference plugin) so a request
	// never races the expiry.
	if exp := jwtExp(c.AccessToken); exp > 0 && time.Now().Add(6*time.Hour).Unix() < exp {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"refreshToken": c.RefreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		d.apiURL("/auth/refresh"), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	d.clientHeaders(req)
	resp, err := d.rpc.Do(req)
	if err != nil {
		return uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "lobster: refresh: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return classifyHTTP(resp.StatusCode, raw)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return uperr.New(uperr.ClassTransport, 0, "lobster: refresh decode: %v", err)
	}
	if env.Code != 0 {
		return classifyCode(env.Code, orElse(env.Message, "刷新失败"))
	}
	var data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil || data.AccessToken == "" {
		return uperr.New(uperr.ClassAuth, 401, "lobster: refresh returned no access token")
	}
	c.AccessToken = data.AccessToken
	if data.RefreshToken != "" {
		c.RefreshToken = data.RefreshToken
	}
	c.ExpiresAt = jwtExp(data.AccessToken)
	if uid := JWTAudience(data.AccessToken); uid != "" {
		c.UID = uid
	}
	raw2, _ := json.Marshal(c)
	acc.CredsJSON = raw2
	acc.CredsDirty = true
	return nil
}
