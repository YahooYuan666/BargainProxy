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

	proxyChatPath    = "/autoclaw-proxy/proxy/autoclaw/v1/chat/completions"
	sendCodePath     = "/userapi/v1/agent-send-code"
	loginPath        = "/userapi/v1/agent-login/"
	refreshPath      = "/userapi/v1/refresh"
	walletsPath      = "/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw"
	sandboxListPath  = "/agentdr/v2/assistant/sandbox/list"
	sandboxApplyPath = "/agentdr/v2/assistant/sandbox/apply"
	taskCompletePath = "/autoclaw-proxy/proxy/autoclaw-task-complete"
)

// CNModels is the static CN model catalogue (verified live 2026-10-17).
var CNModels = []string{
	"openclaw", "openclaw/default",
	"glm-5.3-flash", "glm-5.2",
	"zai_auto", "zai_auto-fast", "zai_glm-5.3-flash", "zaicoding_glm-5.3",
	"tdpsk_deepseek-v4-flash-202605", "tdpsk_deepseek-v4-pro-202606",
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
	if cr.Region != "" && cr.Region != "cn" {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "autoclaw: region %q is not supported (cn only)", cr.Region)
	}
	if needsRefresh(cr) && cr.RefreshToken != "" {
		// Refresh failure is not fatal while the current token is still valid;
		// the upstream call will surface an auth error when it is not.
		_ = d.refresh(ctx, call.Account, cr)
	}

	body, modelID, err := buildBody(call)
	if err != nil {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "autoclaw: %v", err)
	}
	resp, err := d.chatProxy(ctx, cr, body, modelID)
	if err != nil {
		var serr error
		resp, serr = d.chatSandbox(ctx, call.Account, cr, body, modelID)
		if serr != nil {
			// Current deployments: the userapi proxy is WAF-blocked (406) and
			// the sandbox OpenAI endpoint is not enabled (404). The sandbox
			// relay is the practical path; auth/credit/rate failures are
			// terminal and must not be retried through it.
			if relayFallbackAllowed(serr) {
				return d.chatRelay(ctx, call, cr, emit)
			}
			return nil, serr
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
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	body := map[string]any{
		"source_id":     "autoclaw",
		"device_id":     cr.DeviceID,
		"refresh_token": stripBearer(cr.RefreshToken),
	}
	if err := d.userapiCall(ctx, http.MethodPost, refreshPath, cr.bearer(), body, &data); err != nil {
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
func (d *Driver) Probe(ctx context.Context, acc *model.Account) (*driver.ProbeResult, error) {
	cr, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "autoclaw: %v", err)
	}
	var data struct {
		TotalBalance json.Number `json:"total_balance"`
	}
	if err := d.userapiCall(ctx, http.MethodGet, walletsPath, cr.bearer(), nil, &data); err != nil {
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
	if err := d.userapiCall(ctx, http.MethodGet, "/agent-assetmgr/api/v1/points/expiring", cr.bearer(), nil, &exp); err == nil {
		credits.Expiring = toInt64Any(exp.Points)
	}
	note := "points refreshed"
	if credits.Expiring > 0 && exp.Text != "" {
		note = exp.Text
	}
	return &driver.ProbeResult{Credits: credits, Note: note}, nil
}

// Checkin implements the daily_signin task (idempotent upstream).
func (d *Driver) Checkin(ctx context.Context, acc *model.Account) (checkedIn bool, credits int64, enable bool, err error) {
	cr, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return false, 0, false, uperr.New(uperr.ClassAuth, 0, "autoclaw: %v", err)
	}
	body, _ := json.Marshal(map[string]any{"task_id": "daily_signin"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base()+taskCompletePath, bytes.NewReader(body))
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
	req, err := http.NewRequestWithContext(ctx, method, d.base()+path, rd)
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
	req := call.Request
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
		}
		if len(m.ToolCalls) > 0 {
			var tcs []map[string]any
			for _, tc := range m.ToolCalls {
				tcs = append(tcs, map[string]any{
					"id": tc.ID, "type": "function",
					"function": map[string]any{"name": tc.Name, "arguments": tc.Arguments},
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
}

var deadMarkers = []string{
	"not logged in", "unauthorized", "invalid token", "token expired",
	"login required", "session expired", "offline user session",
	"未登录", "登录已过期", "请先登录", "token 失效",
}

func classifyHTTP(status int, body []byte) *uperr.Error {
	text := string(body)
	return classify(status, 0, text, strings.ToLower(text))
}

func classifyBusiness(code int, msg string) *uperr.Error {
	return classify(0, code, msg, strings.ToLower(msg))
}

func classify(status, code int, text, lower string) *uperr.Error {
	clipped := clipText(text, 300)
	switch {
	case code == 410000:
		return uperr.New(uperr.ClassAuth, 401, "autoclaw: session dead: %s", clipped)
	case hasMarker(lower, hardMarkers):
		return uperr.New(uperr.ClassHardCredit, 402, "autoclaw: %s", clipped)
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
