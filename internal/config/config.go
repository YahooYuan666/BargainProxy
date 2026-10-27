// Package config holds the gateway configuration and its loading rules.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is the top-level gateway configuration (config.json).
type Config struct {
	Listen string `json:"listen"`  // default 127.0.0.1:13140
	APIKey string `json:"api_key"` // empty = no auth (local use)

	Store       string `json:"store"`         // "memory" | "postgres"
	Database    string `json:"database"`      // postgres dsn
	DataDir     string `json:"data_dir"`      // data dir for key file, snapshots
	KeyFile     string `json:"key_file"`      // master key path (default <data_dir>/master.key)
	LogKeepDays int    `json:"log_keep_days"` // request log retention (default 14)

	// Channel defaults
	Channels ChannelSettings `json:"channels"`

	// Model routing: canonical model -> explicit channel bindings.
	Models []ModelBinding `json:"models"`

	// PrimaryModels curates what the panel shows in the model catalog and
	// route editor (empty = show everything). Routing is unaffected: models
	// not listed keep serving — this only hides retired/experimental rows.
	PrimaryModels []string `json:"primary_models"`

	// Panel settings
	PanelPasswordHash string `json:"panel_password_hash"` // bcrypt; empty = panel disabled
	WorkBuddyGlobal   bool   `json:"workbuddy_global"`    // international accounts switch (default off)

	// Sentinels are HTTP-200 plain-text rejection markers some upstreams
	// return in place of an error (content-policy refusal). Matched reply
	// text is surfaced as a content rejection without retrying.
	Sentinels []string `json:"sentinels"`

	// Downstream protection bounds (not quotas — pure upstream protection)
	MaxQueueWait Duration `json:"max_queue_wait"` // default 120s
	FallbackWait Duration `json:"fallback_wait"`  // first-byte window before cascading to the next binding, default 8s
	SessionTTL   Duration `json:"session_ttl"`    // sticky binding TTL, default 30m
	CostTTL      Duration `json:"cost_ttl"`       // cost observation TTL, default 6h
	LogRetention Duration `json:"log_retention"`  // default 336h (14d)

	// SingleActivePerChannel: at most one account of a channel may interact at
	// a time (default true, 2026-10-20). A second request does not wait and is
	// not sent to another account of the same pool — it walks to the next
	// binding; when no pool is free the request fails with 并行数量超发. Using
	// several accounts of one pool simultaneously is the pattern upstream risk
	// control reads as abnormal activity.
	SingleActivePerChannel bool `json:"single_active_per_channel"`

	// TaskAccountGap is the pause between accounts inside one scheduled task
	// run (probe / check-in / activity). Upstream task traffic for several
	// accounts in a burst is a risk-control signal; the reference projects use
	// ~0.8s between accounts and 1.05s between report chunks. 0 disables.
	TaskAccountGap Duration `json:"task_account_gap"`
	// TaskAccountSpread spreads the accounts of one automatic task run over a
	// window instead of firing them back to back (BP-96). Several accounts of
	// one pool doing the same thing within seconds is the most recognizable
	// time shape there is; a fixed gap only changes "all at once" into "at a
	// fixed beat". Each account's slot is derived from (task, account, date),
	// so it is stable for the day and a restart does not reshuffle it. Manual
	// (panel-clicked) runs are never spread. 0 disables spreading.
	TaskAccountSpread Duration `json:"task_account_spread"`
	// TaskAccountSpreadMinGap is the least distance between two accounts'
	// slots in that window; colliding slots are pushed apart by it.
	TaskAccountSpreadMinGap Duration `json:"task_account_spread_min_gap"`

	// FirstByteWindow is the ceiling for the per-binding first-byte wait,
	// applied to EVERY channel (BP-72). The wait is fallback_wait + 1s per
	// 2k estimated prompt tokens, capped here. Before BP-72 the cap was a
	// hard 30s for every channel except AutoClaw, so a WorkBuddy binding
	// prefilling a 300k-token ZCode session died at 30s while the upstream
	// was healthy and the request cascaded down the whole chain. 0 = the
	// built-in 90s default. Requests that answer quickly are unaffected:
	// the timer is cancelled by the first stream event.
	FirstByteWindow Duration `json:"first_byte_window"`

	// ChannelFirstByteWindows overrides FirstByteWindow per channel, e.g.
	// {"workbuddy": "120s", "trae": "45s"}. Unknown channel names are
	// ignored (they can never be a binding).
	ChannelFirstByteWindows map[string]Duration `json:"channel_first_byte_windows"`

	// Upstream watchdogs (ticket 27; applied at startup — restart to change):
	//   RPCTimeout    total timeout for short billing/refresh/probe calls (default 120s);
	//   HeaderTimeout time to first byte on chat streams (default 300s);
	//   IdleTimeout   quiet gap mid-stream before declaring the upstream stalled
	//                 (default 300s).
	RPCTimeout    Duration `json:"rpc_timeout"`
	HeaderTimeout Duration `json:"header_timeout"`
	IdleTimeout   Duration `json:"idle_timeout"`
}

// ChannelSettings groups per-channel knobs.
type ChannelSettings struct {
	Antigravity  AntigravitySettings  `json:"antigravity"`
	WorkBuddy    WorkBuddySettings    `json:"workbuddy"`
	Trae         TraeSettings         `json:"trae"`
	ZCode        ZCodeSettings        `json:"zcode"`
	AutoClaw     AutoClawSettings     `json:"autoclaw"`
	Qoder        QoderSettings        `json:"qoder"`
	OpenAICompat OpenAICompatSettings `json:"openaicompat"`
	OpenCodeZen  OpenCodeZenSettings  `json:"opencodezen"`
	Lobster      LobsterSettings      `json:"lobster"`
}

// AntigravitySettings configures the Google Cloud Code Assist channel.
//
// GoogleOAuthClientID / GoogleOAuthClientSecret are client credentials for
// the Cloud Code Assist OAuth flow. They are NOT shipped in source; provide
// them in config.json (the panel import helper can copy them from an
// existing AIClient2API installation).
type AntigravitySettings struct {
	Enabled                 bool   `json:"enabled"`
	BaseURL                 string `json:"base_url"`  // default https://cloudcode-pa.googleapis.com (tests override)
	DailyURL                string `json:"daily_url"` // fallback https://daily-cloudcode-pa.googleapis.com
	TokenURL                string `json:"token_url"` // default https://oauth2.googleapis.com/token
	GoogleOAuthClientID     string `json:"google_oauth_client_id"`
	GoogleOAuthClientSecret string `json:"google_oauth_client_secret"`
	MaxInFlight             int    `json:"max_in_flight"` // default 3
	// DumpDir, when non-empty, writes one self-contained file per FAILED chat
	// request: the exact body we sent plus the upstream's raw answer (BP-115).
	// Empty = off, because a body contains the user's prompts. This exists
	// because the upstream's generic "Request contains an invalid argument"
	// names no field, and every shape rebuilt by hand answers 200 (probed
	// 2026-10-25 with tools, 500 tool round-trips, and signature replay), so
	// the difference lives in the real request and nowhere else.
	DumpDir string `json:"dump_dir"`
	// DumpMaxBytes caps the outbound slice per file (default 262144). Long
	// sessions put the interesting part hundreds of messages in, and a small
	// cap truncates exactly what is worth reading.
	DumpMaxBytes int `json:"dump_max_bytes"`
	// DumpOK also writes SUCCESSFUL requests. Diagnostic only: the point is to
	// diff a working request against a failing one when the upstream's answer
	// names no field. Off by default (a body is a user's whole conversation).
	DumpOK bool `json:"dump_ok"`
}

// WorkBuddySettings configures the Tencent CodeBuddy channel.
type WorkBuddySettings struct {
	Enabled     bool   `json:"enabled"`
	ChatBaseCN  string `json:"chat_base_cn"`  // default https://www.workbuddy.cn (BP-130: the official desktop client's primary CN endpoint; copilot.tencent.com is only the fallback)
	ChatBaseGL  string `json:"chat_base_gl"`  // default https://www.workbuddy.ai
	WebBaseCN   string `json:"web_base_cn"`   // default https://www.workbuddy.cn (growth claim / web events)
	MaxInFlight int    `json:"max_in_flight"` // default 2
	PromptMode  string `json:"prompt_mode"`   // "custom" | "passthrough"
	// DisableClientFingerprint turns off the official-client header family on
	// chat requests. Default false: the edge applies channel/risk-control
	// checks, and bare requests are what upstream later flags as abnormal
	// activity (2026-10-20 field finding). Set true only for A/B diagnosis.
	DisableClientFingerprint bool `json:"disable_client_fingerprint"`
	// SendBrowserHeaders re-adds the browser/CLI-shaped headers (Origin,
	// Referer, Accept-Language, X-Machine-ID, X-Session-ID, the X-No-*
	// sentinels). The real desktop client does NOT send them on the chat path
	// (BP-90, both official packages); they were inherited from the CodeBuddy
	// CLI's conventions, and an extra header is itself a fingerprint. Kept as
	// an escape hatch for A/B diagnosis.
	SendBrowserHeaders bool `json:"send_browser_headers"`
}

// TraeSettings configures the Trae CN channel.
type TraeSettings struct {
	Enabled     bool   `json:"enabled"`
	AgentHost   string `json:"agent_host"`    // default https://trae-api-cn.mchost.guru
	UgHost      string `json:"ug_host"`       // default https://api.trae.cn
	MaxInFlight int    `json:"max_in_flight"` // forced 1 (serial queue)
}

// ZCodeSettings configures the Z.AI ZCode channel.
type ZCodeSettings struct {
	Enabled     bool   `json:"enabled"`
	BaseURL     string `json:"base_url"` // default https://zcode.z.ai (tests override)
	MaxInFlight int    `json:"max_in_flight"`
}

// AutoClawSettings configures the Zhipu AutoClaw cloud-sandbox channel.
type AutoClawSettings struct {
	Enabled     bool   `json:"enabled"`
	UserAPIBase string `json:"userapi_base"` // default https://autoglm-acceleration-api.zhipuai.cn
	MaxInFlight int    `json:"max_in_flight"`

	// FirstByteWindow is the legacy per-channel override (BP-71). It is
	// honoured only when channel_first_byte_windows has no entry for
	// AutoClaw; new configurations should use the generic global setting
	// plus channel overrides (BP-72).
	FirstByteWindow Duration `json:"first_byte_window"`

	// RelayEnabled re-enables the sandbox relay as a fallback when the CN
	// managed gateway fails. Default false (2026-10-21): the relay is 5-38s
	// slower per call and cannot receive images at all, so a managed-gateway
	// failure should surface rather than be masked by a slow path.
	RelayEnabled bool `json:"relay_enabled"`

	// AppSecret signs the userapi requests (X-Auth-Sign). It is a constant
	// baked into the official client, NOT this project's credential, and it
	// is intentionally not shipped in source: set it in config.json from the
	// client version you use. Empty = AutoClaw userapi calls (send-code,
	// login, wallet/ledger, checkin) fail with an explicit config error; the
	// chat relay uses a separate token header and is unaffected.
	AppSecret string `json:"app_secret"`

	// OverseasBase overrides the overseas lane host (default
	// https://autoglm-api.autoglm.ai). The CN lane has had an override
	// (userapi_base) since the start; the overseas one did not, which made the
	// lane untestable end-to-end and left host migrations to a source edit.
	OverseasBase string `json:"overseas_base"`
}

// QoderSettings configures the Alibaba Qoder channel (PAT + COSY protocol).
type QoderSettings struct {
	Enabled     bool   `json:"enabled"`
	GatewayBase string `json:"gateway_base"`  // default https://gateway.qoder.com.cn
	OpenBase    string `json:"open_base"`     // default https://openapi.qoder.com.cn
	MaxInFlight int    `json:"max_in_flight"` // upstream allows 1 concurrent request per PAT
	// DataDir hosts the device-fingerprint salt (qoder-device-salt). Empty
	// falls back to the top-level data_dir. The salt must never change once
	// generated: it seeds every account's virtual device identity.
	DataDir string `json:"data_dir"`
	// DesktopProtocol switches the request headers from the CLI family
	// (cosy-clienttype 5 / 0.1.43) to the official desktop client's family
	// (cosy-clienttype 10 / 1.1.69 / user-agent undici / cosy-scene app /
	// cosy-business-product app), which is what unlocks the flagship models
	// (Sonus/Cantus/Fast) in /algo/api/v2/model/list (Zhengyuuuui/qoder2api
	// 2671a21). Default off: it changes the account's client fingerprint, so
	// flip it after an A/B probe, not on faith.
	DesktopProtocol bool `json:"desktop_protocol"`
}

// OpenAICompatSettings configures the generic fallback channel.
type OpenAICompatSettings struct {
	Enabled bool     `json:"enabled"`
	BaseURL string   `json:"base_url"`
	APIKey  string   `json:"api_key"`
	Models  []string `json:"models"` // canonical models this fallback serves
}

// OpenCodeZenSettings configures the OpenCode Zen anonymous free-model
// channel (BP-54). Enabled flips the whole feature: driver registration,
// directory sync and the panel page read this switch.
type OpenCodeZenSettings struct {
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"base_url"` // default https://opencode.ai/inference/v1 (tests override)
}

// LobsterSettings configures the NetEase Youdao LobsterAI (有道龙虾) channel
// (BP-98). It is an unofficial channel: the endpoints below belong to the
// desktop client's own backend, not to a published developer API, and access
// requires a user's own login (OAuth authorization code → JWT bearer).
//
// ServerBase hosts both surfaces: /api/* is the JSON envelope API
// ({code,message,data}) used for login, models and quota; /api/proxy/v1 is
// the OpenAI-shaped inference proxy.
type LobsterSettings struct {
	Enabled bool `json:"enabled"`
	// ServerBase is the API host; PortalBase is the web portal that serves
	// the login page. Split because the login redirect must point at the
	// portal while every call goes to the server host.
	ServerBase  string `json:"server_base"` // default https://lobsterai-server.youdao.com
	PortalBase  string `json:"portal_base"` // default https://lobsterai.youdao.com
	MaxInFlight int    `json:"max_in_flight"`

	// ClientVersion / ClientCapabilities are the desktop client's advertised
	// identity. They are optional (the upstream answers without them) but gate
	// feature switches such as thinking-level control, so they are sent by
	// default. Both are the vendor's own public constants, not credentials.
	ClientVersion      string `json:"client_version"`
	ClientCapabilities string `json:"client_capabilities"`
}

// ModelBinding binds one canonical model to one channel's native model id.
type ModelBinding struct {
	Canonical     string `json:"canonical"`      // e.g. "glm-5.3"
	Channel       string `json:"channel"`        // antigravity|workbuddy|trae|zcode|openaicompat
	UpstreamModel string `json:"upstream_model"` // channel native id
	Priority      int    `json:"priority"`       // lower first within equal cost
}

// Duration is a JSON-friendly time.Duration ("30m", "120s"; days as e.g.
// "336h").
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("config: invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// D returns the duration value.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Default returns the built-in defaults (network addresses are production
// values; tests override them).
func Default() *Config {
	return &Config{
		Listen:      "127.0.0.1:13140",
		Store:       "memory",
		DataDir:     "./data",
		LogKeepDays: 14,
		Channels: ChannelSettings{
			Antigravity: AntigravitySettings{
				Enabled:     true,
				BaseURL:     "https://cloudcode-pa.googleapis.com",
				DailyURL:    "https://daily-cloudcode-pa.googleapis.com",
				TokenURL:    "https://oauth2.googleapis.com/token",
				MaxInFlight: 3,
			},
			WorkBuddy: WorkBuddySettings{
				Enabled:     true,
				ChatBaseCN:  "https://www.workbuddy.cn",
				ChatBaseGL:  "https://www.workbuddy.ai",
				WebBaseCN:   "https://www.workbuddy.cn",
				MaxInFlight: 2,
				PromptMode:  "custom",
			},
			Trae: TraeSettings{
				Enabled:     true,
				AgentHost:   "https://trae-api-cn.mchost.guru",
				UgHost:      "https://api.trae.cn",
				MaxInFlight: 1,
			},
			ZCode: ZCodeSettings{
				Enabled:     true,
				BaseURL:     "https://zcode.z.ai",
				MaxInFlight: 2,
			},
			AutoClaw: AutoClawSettings{
				Enabled:     true,
				UserAPIBase: "https://autoglm-acceleration-api.zhipuai.cn",
				MaxInFlight: 2,
			},
			Qoder: QoderSettings{
				Enabled:     true,
				GatewayBase: "https://gateway.qoder.com.cn",
				OpenBase:    "https://openapi.qoder.com.cn",
				MaxInFlight: 1,
			},
			OpenCodeZen: OpenCodeZenSettings{
				Enabled: true,
				BaseURL: "https://opencode.ai/inference/v1",
			},
			Lobster: LobsterSettings{
				// Enabled by default so the panel offers the login flow; the
				// channel takes no traffic until a model binding is added, so
				// an enabled-but-unbound pool is inert rather than noisy.
				Enabled:            true,
				ServerBase:         "https://lobsterai-server.youdao.com",
				PortalBase:         "https://lobsterai.youdao.com",
				MaxInFlight:        2,
				ClientVersion:      "2026.9.23",
				ClientCapabilities: "kimi-k3-agentic-v1,thinking-level-control-v1",
			},
		},
		MaxQueueWait: Duration(120 * time.Second),
		FallbackWait: Duration(8 * time.Second),
		// BP-115: 90s → 30s. The window is spent per binding, so a five-pool
		// cascade could hold a client for 7.5 minutes before the first byte.
		// 30s bounded that at 2.5 min, but it also cancelled requests that
		// would have answered: a 33万-token prefill measured 31.9s and the
		// next one was cut at exactly 30.000s (BP-118, 2026-10-26). 60s covers
		// roughly a 1M-token prefill at the same 2k-token/s rate and bounds a
		// five-leg cascade at 5 minutes. AutoClaw keeps its own longer ceiling
		// via channels.autoclaw.first_byte_window.
		FirstByteWindow: Duration(60 * time.Second),
		// One account per pool at a time: the anti-risk-control default
		// (2026-10-20). Explicitly setting false restores multi-account
		// concurrency within a pool.
		SingleActivePerChannel:  true,
		TaskAccountGap:          Duration(800 * time.Millisecond),
		TaskAccountSpread:       Duration(5 * time.Minute),
		TaskAccountSpreadMinGap: Duration(30 * time.Second),
		RPCTimeout:              Duration(120 * time.Second),
		HeaderTimeout:           Duration(300 * time.Second),
		IdleTimeout:             Duration(300 * time.Second),
		SessionTTL:              Duration(30 * time.Minute),
		CostTTL:                 Duration(6 * time.Hour),
		LogRetention:            Duration(336 * time.Hour),
		Sentinels:               []string{"DANGEROUS_ACTIVITY_BLOCKED"},
	}
}

// Load reads config.json (if present), applies defaults for missing fields,
// and creates the data directory. A missing file is not an error: defaults
// are used (memory store, no auth — suitable for a first local run).
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := json.Unmarshal(b, cfg); err != nil {
				return nil, fmt.Errorf("config: parse %s: %w", path, err)
			}
		case os.IsNotExist(err):
			// defaults
		default:
			return nil, err
		}
	}
	applyDefaults(cfg)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("config: create data dir: %w", err)
	}
	if cfg.KeyFile == "" {
		cfg.KeyFile = filepath.Join(cfg.DataDir, "master.key")
	}
	return cfg, nil
}

// Save persists cfg to path atomically (write temp + rename), preserving the
// panel-editable fields the struct carries. Used by the settings page's
// restart-required block.
func Save(path string, cfg *Config) error {
	if path == "" {
		path = "config.json"
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("config: write: %w", err)
	}
	return os.Rename(tmp, path)
}

func applyDefaults(cfg *Config) {
	d := Default()
	if cfg.Listen == "" {
		cfg.Listen = d.Listen
	}
	if cfg.Store == "" {
		cfg.Store = d.Store
	}
	if cfg.DataDir == "" {
		cfg.DataDir = d.DataDir
	}
	if cfg.LogKeepDays <= 0 {
		cfg.LogKeepDays = d.LogKeepDays
	}
	ch, dch := &cfg.Channels, d.Channels
	if ch.Antigravity.BaseURL == "" {
		ch.Antigravity.BaseURL = dch.Antigravity.BaseURL
	}
	if ch.Antigravity.DailyURL == "" {
		ch.Antigravity.DailyURL = dch.Antigravity.DailyURL
	}
	if ch.Antigravity.TokenURL == "" {
		ch.Antigravity.TokenURL = dch.Antigravity.TokenURL
	}
	// GoogleOAuthClientID / GoogleOAuthClientSecret intentionally have no
	// built-in default: supply them via config.json.
	if ch.Antigravity.MaxInFlight <= 0 {
		ch.Antigravity.MaxInFlight = dch.Antigravity.MaxInFlight
	}
	if ch.WorkBuddy.ChatBaseCN == "" {
		ch.WorkBuddy.ChatBaseCN = dch.WorkBuddy.ChatBaseCN
	}
	if ch.WorkBuddy.ChatBaseGL == "" {
		ch.WorkBuddy.ChatBaseGL = dch.WorkBuddy.ChatBaseGL
	}
	if ch.WorkBuddy.WebBaseCN == "" {
		ch.WorkBuddy.WebBaseCN = dch.WorkBuddy.WebBaseCN
	}
	if ch.WorkBuddy.MaxInFlight <= 0 {
		ch.WorkBuddy.MaxInFlight = dch.WorkBuddy.MaxInFlight
	}
	if ch.WorkBuddy.PromptMode == "" {
		ch.WorkBuddy.PromptMode = dch.WorkBuddy.PromptMode
	}
	if ch.Trae.AgentHost == "" {
		ch.Trae.AgentHost = dch.Trae.AgentHost
	}
	if ch.Trae.UgHost == "" {
		ch.Trae.UgHost = dch.Trae.UgHost
	}
	if ch.Trae.MaxInFlight <= 0 {
		ch.Trae.MaxInFlight = 1 // hard limit: serial
	}
	if ch.ZCode.BaseURL == "" {
		ch.ZCode.BaseURL = dch.ZCode.BaseURL
	}
	if ch.ZCode.MaxInFlight <= 0 {
		ch.ZCode.MaxInFlight = dch.ZCode.MaxInFlight
	}
	if ch.AutoClaw.UserAPIBase == "" {
		ch.AutoClaw.UserAPIBase = dch.AutoClaw.UserAPIBase
	}
	if ch.AutoClaw.MaxInFlight <= 0 {
		ch.AutoClaw.MaxInFlight = dch.AutoClaw.MaxInFlight
	}
	if ch.Qoder.GatewayBase == "" {
		ch.Qoder.GatewayBase = dch.Qoder.GatewayBase
	}
	if ch.Qoder.OpenBase == "" {
		ch.Qoder.OpenBase = dch.Qoder.OpenBase
	}
	if ch.Qoder.MaxInFlight <= 0 {
		ch.Qoder.MaxInFlight = 1 // upstream: one in-flight request per PAT
	}
	if ch.Qoder.DataDir == "" {
		ch.Qoder.DataDir = cfg.DataDir // device salt lives beside the master key
	}
	if ch.Lobster.ServerBase == "" {
		ch.Lobster.ServerBase = dch.Lobster.ServerBase
	}
	if ch.Lobster.PortalBase == "" {
		ch.Lobster.PortalBase = dch.Lobster.PortalBase
	}
	if ch.Lobster.MaxInFlight <= 0 {
		ch.Lobster.MaxInFlight = dch.Lobster.MaxInFlight
	}
	// The client identity headers are optional upstream, but they gate the
	// thinking-level feature switches, so an empty value means "use the
	// client version this build was written against", not "send nothing".
	if ch.Lobster.ClientVersion == "" {
		ch.Lobster.ClientVersion = dch.Lobster.ClientVersion
	}
	if ch.Lobster.ClientCapabilities == "" {
		ch.Lobster.ClientCapabilities = dch.Lobster.ClientCapabilities
	}
	if cfg.MaxQueueWait <= 0 {
		cfg.MaxQueueWait = d.MaxQueueWait
	}
	if cfg.FallbackWait <= 0 {
		cfg.FallbackWait = d.FallbackWait
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = d.SessionTTL
	}
	if cfg.CostTTL <= 0 {
		cfg.CostTTL = d.CostTTL
	}
	if cfg.LogRetention <= 0 {
		cfg.LogRetention = d.LogRetention
	}
	if len(cfg.Sentinels) == 0 {
		cfg.Sentinels = d.Sentinels
	}
	// Normalize canonical model names.
	for i := range cfg.Models {
		cfg.Models[i].Canonical = NormalizeModel(cfg.Models[i].Canonical)
		cfg.Models[i].Channel = strings.ToLower(strings.TrimSpace(cfg.Models[i].Channel))
	}
}

// NormalizeModel lowercases and trims a canonical model id. Note: unlike the
// panel's destructive norm (strip all punctuation), we keep punctuation —
// version identity matters (deepseek-v4.1 vs deepseek-v4).
func NormalizeModel(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
