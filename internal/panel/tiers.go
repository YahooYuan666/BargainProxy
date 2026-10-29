// Model tier catalog for the panel's "模型与档位" page. Specs are sourced
// from the models.dev catalog (the same public dataset the provider-fetcher
// tool chains, values cross-checked 2026-10-15): context/output limits,
// input modalities (text/image/video/audio/pdf), supported reasoning effort
// gears, tool-call and structured-output support.
//
// 国模 (domestic, credit-based pools: workbuddy / trae / zcode / autoclaw /
// qoder / lobster): the billing
// column carries the upstream model-catalog credit multiplier ("x0.79" glm-5.3
// flagship … "x0.06" flash tier), display only — live cost tiering reads the
// cost observer.
//
// 倍率取证口径（research/2026-10-18-三渠道模型倍率核实.md）：
//   - Qoder：一手接口 model/list 的 price_factor（客户端就是读它渲染的），
//     错峰折扣由 promotion 块按 22:00-08:00 Asia/Shanghai 本地生效；
//   - Trae：官方文档 docs.trae.cn「内置模型限时折扣」两张表（免费/付费），
//     忙时 08:00-22:00 / 空闲 22:00-08:00；
//   - WorkBuddy：官网与文档站均为客户端渲染抓不到，只能以客户端截图为准。
//
// 三个渠道的 1 分 / 1 credits / 1 积分购买力不同，**跨渠道不可直接比大小**。
//
// 洋模 (Google Antigravity window-quota pools + Claude): billing carries the
// pattern. For Claude the models.dev list price plus its multiple vs
// claude-sonnet-4-6 (reference tier 1.00×) is shown; Antigravity draws the
// separate 3p/Claude ledger for these.
package panel

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/router"
)

// tierRow is one model row; billing holds the family-specific billing text
// (credit multiplier for domestic, billing pattern for foreign).
type tierRow struct {
	Source     string   `json:"source"` // 账号源 / provider family
	ID         string   `json:"id"`
	Billing    string   `json:"billing"`
	ContextLen int      `json:"context_length"`
	MaxOutput  int      `json:"max_output"`
	InputModes []string `json:"input_modes"`
	Reasoning  string   `json:"reasoning"` // supported effort gears
	ToolCall   bool     `json:"tool_call"`
	Structured bool     `json:"structured_output"`
	// Routed: this (channel, upstream model) pair appears in the final route
	// table. Rows outside it are retired; first-seen rows are new and always
	// shown so a refreshed catalogue stays visible until it is routed.
	Routed  bool `json:"routed"`
	Retired bool `json:"retired"`
	New     bool `json:"new"`
	// Observed is the real per-request deduction measured from request logs
	// (upstream-reported credit), last 7 days. It answers "how many points did
	// this actually cost" instead of only showing the catalogue multiplier.
	ObservedN      int     `json:"observed_n,omitempty"`
	ObservedAvg    float64 `json:"observed_avg,omitempty"`
	ObservedMin    float64 `json:"observed_min,omitempty"`
	ObservedMax    float64 `json:"observed_max,omitempty"`
	ObservedAvgIn  int64   `json:"observed_avg_in,omitempty"`
	ObservedAvgOut int64   `json:"observed_avg_out,omitempty"`
	ObservedUnit   string  `json:"observed_unit,omitempty"`
}

const keyTiersSeen = "tiers_seen_models"

func m(names ...string) []string { return names }

func domesticCatalog() []tierRow {
	return []tierRow{
		// WorkBuddy（腾讯 CodeBuddy 积分池）
		// 倍率来源：用户 2026-10-18 客户端截图（官网/文档站均客户端渲染，抓不到）。
		// kimi-k3 旧记录 0.35x 已被截图 1.62x 推翻；deepseek-v4-pro 取实测 0.51x。
		{Source: "workbuddy", ID: "glm-5.3", Billing: "x0.79（订阅优先）", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text"), Reasoning: "low/high/max", ToolCall: true, Structured: true},
		{Source: "workbuddy", ID: "glm-5.3-flash", Billing: "x0.06（订阅优先）", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text", "image", "video", "pdf"), Reasoning: "low/high/max", ToolCall: true, Structured: true},
		{Source: "workbuddy", ID: "glm-5.2", Billing: "x0.30（倍率待复核）", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text"), Reasoning: "high/max", ToolCall: true, Structured: true},
		{Source: "workbuddy", ID: "deepseek-v4-pro", Billing: "x0.51（夜间折扣）", ContextLen: 1000000, MaxOutput: 393216, InputModes: m("text"), Reasoning: "关闭 + high/max", ToolCall: true, Structured: true},
		{Source: "workbuddy", ID: "deepseek-v4-flash", Billing: "x0.11 · 已被 4.1 淘汰", ContextLen: 1000000, MaxOutput: 393216, InputModes: m("text", "image"), Reasoning: "关闭 + high", ToolCall: true, Structured: true},
		{Source: "workbuddy", ID: "deepseek-v4.1-flash", Billing: "x0.11（夜间折扣）", ContextLen: 1000000, MaxOutput: 393216, InputModes: m("text", "image"), Reasoning: "关闭 + low/high/max", ToolCall: true, Structured: true},
		{Source: "workbuddy", ID: "kimi-k3", Billing: "x1.62", ContextLen: 1048576, MaxOutput: 1048576, InputModes: m("text", "image", "video"), Reasoning: "low/high/max", ToolCall: true, Structured: true},
		// Kimi 3.1 (BP-138): CN spells it kimi-k3-1, global kimi-k3.1-pro — both
		// present in /v3/config (2026-10-27). 倍率未在配置里给出，按 K3 同档标注待实测。
		{Source: "workbuddy", ID: "kimi-k3-1", Billing: "倍率待实测（K3.1，CN 拼写）", ContextLen: 1048576, MaxOutput: 1048576, InputModes: m("text", "image"), Reasoning: "low/high/max", ToolCall: true, Structured: true},
		{Source: "workbuddy", ID: "kimi-k3.1-pro", Billing: "倍率待实测（K3.1，国际拼写）", ContextLen: 1048576, MaxOutput: 1048576, InputModes: m("text", "image"), Reasoning: "low/high/max", ToolCall: true, Structured: true},
		{Source: "workbuddy", ID: "kimi-k2.7", Billing: "x0.25（倍率待复核）", ContextLen: 262144, MaxOutput: 262144, InputModes: m("text", "image", "video"), Reasoning: "关闭（Moonshot 官方仅 toggle）", ToolCall: true, Structured: true},
		{Source: "workbuddy", ID: "kimi-k2.6", Billing: "x0.20（倍率待复核）", ContextLen: 262144, MaxOutput: 262144, InputModes: m("text", "image", "video"), Reasoning: "关闭（Moonshot 官方仅 toggle）", ToolCall: true, Structured: true},
		{Source: "workbuddy", ID: "minimax-m3", Billing: "x0.15（倍率待复核）", ContextLen: 1048576, MaxOutput: 512000, InputModes: m("text", "image", "video"), Reasoning: "关闭（MiniMax 官方仅 toggle）", ToolCall: true, Structured: true},
		// Trae CN（积分池）
		// 倍率来源：docs.trae.cn「内置模型限时折扣」官方表 + 2026-10-18 客户端截图；
		// 2026-10-29 起补 misakano7545/traework2api-panel df199b37/d26fa12b 的
		// /v1/models 实测价目（两次独立探针，284积分/M 口径）。-max 变体与基名
		// 同价（上游 credit_rate 相同）；剥 -max 归一到基名倍率时必须
		// 「基名在表内才剥」，否则 qwen3.8-max 会被误剥成不存在的 qwen3.8。
		// 忙时 08:00-22:00 / 空闲 22:00-08:00；客户端"标签+数字"口径不统一
		// （glm-5.3 显示折前、deepseek-v4.1-flash 显示折后），故部分写明两档。
		// glm-5.3 路由已下线（用户 2026-10-16 实测 Trae 只留 glm-5.3-flashx，
		// BP-38 删了 glm-5.3→trae:glm-5.2 第二腿），但官方折扣表仍列付费会员价，保留供对照。
		{Source: "trae", ID: "glm-5.3-flashx", Billing: "x0.31（输出 7.1；实测 284积分/M）", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text", "image", "video", "pdf"), Reasoning: "low/high/max", ToolCall: false, Structured: true},
		{Source: "trae", ID: "glm-5.3-flash", Billing: "x0.06", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text", "image", "video", "pdf"), Reasoning: "low/high/max", ToolCall: false, Structured: true},
		{Source: "trae", ID: "glm-5.3", Billing: "付费会员 x0.78 → 5 折 x0.39", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text"), Reasoning: "low/high/max", ToolCall: false, Structured: true},
		{Source: "trae", ID: "glm-5.1", Billing: "x0.83（上游 /v1/models 实测）", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text"), Reasoning: "未单列（走上游默认）", ToolCall: false, Structured: true},
		{Source: "trae", ID: "qwen3.8-max", Billing: "x1.50", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text", "image", "video"), Reasoning: "关闭 + low/medium/xhigh", ToolCall: false, Structured: true},
		{Source: "trae", ID: "qwen3.8-flash", Billing: "x0.08", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text", "image", "video"), Reasoning: "关闭 + low/medium/xhigh", ToolCall: false, Structured: true},
		{Source: "trae", ID: "qwen-3.5", Billing: "x0.26（上游 /v1/models 实测）", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text"), Reasoning: "未单列（走上游默认）", ToolCall: false, Structured: true},
		{Source: "trae", ID: "qwen-3.6-plus", Billing: "x0.26（上游 /v1/models 实测）", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text"), Reasoning: "未单列（走上游默认）", ToolCall: false, Structured: true},
		{Source: "trae", ID: "mimo-v2.6-flash", Billing: "x0.06（上游 /v1/models 实测）", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text"), Reasoning: "未单列（走上游默认）", ToolCall: false, Structured: true},
		{Source: "trae", ID: "mimo-v2.6-pro", Billing: "x0.15（上游 /v1/models 实测）", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text"), Reasoning: "未单列（走上游默认）", ToolCall: false, Structured: true},
		{Source: "trae", ID: "minimax-m2.7", Billing: "x0.27（上游 /v1/models 实测）", ContextLen: 1000000, MaxOutput: 512000, InputModes: m("text", "image", "video"), Reasoning: "未单列（走上游默认）", ToolCall: false, Structured: true},
		{Source: "trae", ID: "minimax-m3.1-flash", Billing: "x0.09（上游 /v1/models 实测）", ContextLen: 1000000, MaxOutput: 512000, InputModes: m("text"), Reasoning: "未单列（走上游默认）", ToolCall: false, Structured: true},
		{Source: "trae", ID: "doubao-seed-code", Billing: "x0.06（上游 /v1/models 实测）", ContextLen: 256000, MaxOutput: 131072, InputModes: m("text"), Reasoning: "未单列（走上游默认）", ToolCall: false, Structured: true},
		{Source: "trae", ID: "deepseek-v4-pro", Billing: "忙时 x0.72 / 空闲 x0.36（5 折）", ContextLen: 1000000, MaxOutput: 393216, InputModes: m("text"), Reasoning: "关闭 + high/max", ToolCall: true, Structured: true},
		{Source: "trae", ID: "deepseek-v4-flash", Billing: "忙时 x0.20 / 空闲 x0.10（5 折）", ContextLen: 1000000, MaxOutput: 393216, InputModes: m("text", "image"), Reasoning: "关闭 + high", ToolCall: true, Structured: true},
		{Source: "trae", ID: "deepseek-v4.1-flash", Billing: "免费忙时 x0.12 / 空闲 x0.06；付费会员全时段 x0.06", ContextLen: 1000000, MaxOutput: 393216, InputModes: m("text", "image"), Reasoning: "关闭 + low/high/max", ToolCall: false, Structured: true},
		{Source: "trae", ID: "doubao-seed-2.1-pro", Billing: "高级包 约30-50/次", ContextLen: 256000, MaxOutput: 128000, InputModes: m("text", "image", "video"), Reasoning: "官方 turbo 口径 关闭 + low/high（pro 未单列）", ToolCall: true, Structured: true},
		{Source: "trae", ID: "kimi-k3", Billing: "x1.83", ContextLen: 1048576, MaxOutput: 1048576, InputModes: m("text", "image", "video"), Reasoning: "low/high/max", ToolCall: false, Structured: true},
		{Source: "trae", ID: "kimi-k3.1-pro", Billing: "x1.2（上游 /v1/models 实测）", ContextLen: 1048576, MaxOutput: 1048576, InputModes: m("text", "image"), Reasoning: "low/high/max", ToolCall: false, Structured: true},
		// ZCode（Z.AI Coding Plan 计划内额度；提供 GLM 系模型，无 Claude）
		{Source: "zcode", ID: "glm-5.3", Billing: "计划内额度", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text"), Reasoning: "low/high/max", ToolCall: true, Structured: true},
	}
}

func foreignCatalog() []tierRow {
	return []tierRow{
		// Google Antigravity 反代实际提供（AIClient2API gemini-antigravity 清单核对
		// 2026-10-15）：一个 gemini-3.8-flash 模型族（四档） + 两个 Claude 模型。
		{Source: "antigravity · Gemini", ID: "gemini-3.8-flash-high", Billing: "窗口账本（high 档消耗快）；API 价 $0.75/$3.75", ContextLen: 1048576, MaxOutput: 65536, InputModes: m("text", "image", "video", "audio", "pdf"), Reasoning: "low/medium/high", ToolCall: true, Structured: true},
		{Source: "antigravity · Gemini", ID: "gemini-3.8-flash-medium", Billing: "窗口账本；API 价 $0.75/$3.75", ContextLen: 1048576, MaxOutput: 65536, InputModes: m("text", "image", "video", "audio", "pdf"), Reasoning: "low/medium/high", ToolCall: true, Structured: true},
		{Source: "antigravity · Gemini", ID: "gemini-3.8-flash-low", Billing: "窗口账本（low 最省）；API 价 $0.75/$3.75", ContextLen: 1048576, MaxOutput: 65536, InputModes: m("text", "image", "video", "audio", "pdf"), Reasoning: "low/medium/high", ToolCall: true, Structured: true},
		{Source: "antigravity · Gemini", ID: "gemini-3.8-flash", Billing: "窗口账本（默认档）；API 价 $0.75/$3.75", ContextLen: 1048576, MaxOutput: 65536, InputModes: m("text", "image", "video", "audio", "pdf"), Reasoning: "low/medium/high", ToolCall: true, Structured: true},
		// Claude 经 Antigravity 走 3p/Claude 独立账本；倍率以 Sonnet 4.6 = 1.00× 基准（价取自 models.dev）
		{Source: "antigravity · Claude 账本", ID: "gemini-claude-sonnet-4-6", Billing: "1.00×（基准）· $3/$15 · 3p 账本", ContextLen: 1000000, MaxOutput: 128000, InputModes: m("text", "image", "pdf"), Reasoning: "low/medium/high/max", ToolCall: true, Structured: true},
		{Source: "antigravity · Claude 账本", ID: "gemini-claude-opus-4-6-thinking", Billing: "1.67× Sonnet · $5/$25 · 3p 账本 · thinking 版", ContextLen: 1000000, MaxOutput: 128000, InputModes: m("text", "image", "pdf"), Reasoning: "low/medium/high/max", ToolCall: true, Structured: true},
		// 以下为反代同样支持、但当前主用之外的档位（便于按需启用）
		{Source: "antigravity · 备用档", ID: "gemini-3.7-flash(-low/medium/high)", Billing: "窗口账本；API 价 $0.75/$3.75", ContextLen: 1048576, MaxOutput: 65536, InputModes: m("text", "image", "video", "audio", "pdf"), Reasoning: "low/medium/high", ToolCall: true, Structured: true},
		{Source: "antigravity · 备用档", ID: "gemini-3.6-flash(-low/medium/high)", Billing: "窗口账本；API 价 $0.75/$3.75", ContextLen: 1048576, MaxOutput: 65536, InputModes: m("text", "image", "video", "audio", "pdf"), Reasoning: "low/medium/high", ToolCall: true, Structured: true},
		{Source: "antigravity · 备用档", ID: "gemini-3.5-flash-low/high", Billing: "窗口账本；API 价 $1.50/$9.00", ContextLen: 1048576, MaxOutput: 65536, InputModes: m("text", "image", "video", "audio", "pdf"), Reasoning: "minimal/low/medium/high", ToolCall: true, Structured: true},
		{Source: "antigravity · 备用档", ID: "gemini-3.1-pro-low/high", Billing: "窗口账本（pro 消耗显著更高）；API 价 $2.00/$12.00", ContextLen: 1048576, MaxOutput: 65536, InputModes: m("text", "image", "video", "audio", "pdf"), Reasoning: "low/medium/high", ToolCall: true, Structured: true},
		{Source: "antigravity · 备用档", ID: "gpt-oss-120b-medium", Billing: "窗口账本（开源权重模型）", ContextLen: 131072, MaxOutput: 32768, InputModes: m("text"), Reasoning: "medium", ToolCall: true, Structured: false},
	}
}

// tierChannel maps a display source to the routing channel.
func tierChannel(source string) string {
	if strings.HasPrefix(source, "antigravity") {
		return model.ChannelAntigravity
	}
	return source
}

// routedSpecs fills specs for route bindings whose upstream model id is not
// in the static catalog (AutoClaw / Qoder / Trae / ZCode extra legs). Values:
//   - AutoClaw: upstream autoclaw-model-config (GLM-5.3 maxTokens 307200,
//     GLM-5.3-Flash text+image 131072) and the DeepSeek equivalents;
//   - Qoder: same model identity as models.dev (display name → key), with the
//     channel's billing text;
//   - Trae/ZCode: glm-5.3-flash per models.dev (text+image+video+pdf).
var routedSpecs = map[string]tierRow{
	// Qoder 倍率取一手接口 model/list 的 price_factor（2026-10-18 核实；
	// 2026-10-26 872d9b9 复核：Kimi-K3.1-Pro 由 Kimi-K3 原位升级，倍率
	// 1.4 → 1.0）：GLM-5.3 0.8 / GLM-5.3-Flash 0.1 / Qwen3.8-Flash 0（免费）/
	// Qwen3.8-Max 0.5（错峰 22:00-08:00 打 4 折）/ DeepSeek-V4-Pro 0.5 /
	// DeepSeek-Flash 0.1 / Kimi-K3.1-Pro 1.0。
	"qoder|GLM-5.3":              {Billing: "x0.8", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text"), Reasoning: "low/high/max", ToolCall: true, Structured: true},
	"qoder|GLM-5.3-Flash":        {Billing: "x0.1", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text", "image", "video", "pdf"), Reasoning: "low/high/max", ToolCall: true, Structured: true},
	"qoder|Qwen3.8-Flash":        {Billing: "免费（折前 x0.1）", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text", "image", "video"), Reasoning: "关闭 + low/medium/xhigh", ToolCall: true, Structured: true},
	"qoder|Qwen3.8-Max":          {Billing: "x0.5（错峰 22:00-08:00 → x0.2）", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text", "image", "video", "pdf"), Reasoning: "关闭 + low/medium/xhigh", ToolCall: true, Structured: true},
	"qoder|DeepSeek-V4-Pro":      {Billing: "x0.5", ContextLen: 1000000, MaxOutput: 393216, InputModes: m("text"), Reasoning: "关闭 + high/max", ToolCall: true, Structured: true},
	"qoder|DeepSeek-Flash":       {Billing: "x0.1", ContextLen: 1000000, MaxOutput: 393216, InputModes: m("text", "image"), Reasoning: "关闭 + low/high/max", ToolCall: true, Structured: true},
	"qoder|Kimi-K3.1-Pro":        {Billing: "x1.0", ContextLen: 1048576, MaxOutput: 131072, InputModes: m("text", "image", "video"), Reasoning: "low/high/max", ToolCall: true, Structured: true},
	"autoclaw|zaicoding_glm-5.3": {Billing: "按次扣点数", ContextLen: 1000000, MaxOutput: 307200, InputModes: m("text"), Reasoning: "low/high/max", ToolCall: true, Structured: true},
	"autoclaw|glm-5.3-flash":     {Billing: "按次扣点数", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text", "image"), Reasoning: "low/high/max", ToolCall: true, Structured: true},
	// Lobster（有道龙虾，BP-136）：倍率取自公开价格目录
	// GET /api/models/pricing-catalog（无需登录，2026-10-27 实测 32 个模型）。
	// creditsPerYuan=100，inputCredits/outputCredits 即每百万 token 的 credits；
	// DeepSeek 两档有分时计价（高峰 09:00-12:00、14:00-18:00 工作日 = 空闲 ×2），
	// costMultiplier 即当前时段倍率。qwen3.8-max/flash 与 kimi-k3 的
	// thinkingConfig=null：按 BP-98 结论"null=不显示入口、不发档位参数、走上游
	// 默认"，kimi-k3 客户端固定 max。MaxOutput 沿用同族模型在 models.dev 的
	// 规格（目录不提供该字段）。
	"lobster|deepseek-flash":  {Billing: "x0.05 空闲 / x0.10 高峰 · ¥1/¥4 每百万", ContextLen: 1000000, MaxOutput: 393216, InputModes: m("text", "image"), Reasoning: "off/high/max（默认 high）", ToolCall: true, Structured: true},
	"lobster|deepseek-v4-pro": {Billing: "x0.26 空闲 / x0.52 高峰 · ¥4.5/¥13.5 每百万", ContextLen: 1000000, MaxOutput: 393216, InputModes: m("text"), Reasoning: "off/high/max（默认 high）", ToolCall: true, Structured: true},
	"lobster|glm-5.3-flashx":  {Billing: "x0.15 · ¥2/¥7 每百万", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text", "image"), Reasoning: "off/high/max（默认 max）", ToolCall: true, Structured: true},
	"lobster|glm-5.3-flash":   {Billing: "x0.06 · ¥0.8/¥2.8 每百万", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text", "image"), Reasoning: "off/high/max（默认 max）", ToolCall: true, Structured: true},
	"lobster|glm-5.3":         {Billing: "x1.08 · ¥8/¥28 每百万", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text"), Reasoning: "off/high/max（默认 max）", ToolCall: true, Structured: true},
	"lobster|qwen3.8-max":     {Billing: "x0.91 · ¥12/¥36 每百万", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text", "image"), Reasoning: "官方不开放档位（走上游默认）", ToolCall: true, Structured: true},
	"lobster|qwen3.8-flash":   {Billing: "x0.06 · ¥0.8/¥2.7 每百万", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text", "image"), Reasoning: "官方不开放档位（走上游默认）", ToolCall: true, Structured: true},
	"lobster|kimi-k3":         {Billing: "x1.35 · ¥20/¥100 每百万（思考固定 max）", ContextLen: 1048576, MaxOutput: 1048576, InputModes: m("text", "image"), Reasoning: "客户端固定 max", ToolCall: true, Structured: true},
	// K3.1 反而比 K3 便宜（0.88 vs 1.35，2026-10-27 价格目录实测）；highspeed 版更贵。
	"lobster|kimi-k3.1-pro":           {Billing: "x0.88 · K3.1 Pro（比 K3 便宜）", ContextLen: 1048576, MaxOutput: 1048576, InputModes: m("text", "image"), Reasoning: "客户端固定 max", ToolCall: true, Structured: true},
	"lobster|kimi-k3.1-pro-highspeed": {Billing: "x1.16 · K3.1 Pro 高速版", ContextLen: 1048576, MaxOutput: 1048576, InputModes: m("text", "image"), Reasoning: "客户端固定 max", ToolCall: true, Structured: true},
	// autoclaw 的 tdpsk_deepseek-* 两条已移除：CN 加速网关对本账号三个候选 id
	// 全部回 400「非法模型」，而同刻 glm-5.3-flash 可用（2026-10-23 实测）。
	// Trae 免费用户折扣表：glm-5.3-flash 未单列，取客户端截图 0.06x（无折扣标签）。
	"trae|glm-5.3-flash": {Billing: "x0.06", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text", "image", "video", "pdf"), Reasoning: "low/high/max", ToolCall: true, Structured: true},
	// Trae 的 4.1-flash 折扣表（docs.trae.cn「内置模型限时折扣」）：免费忙时 x0.12 /
	// 空闲 x0.06，付费会员全时段 x0.06。上游 config_name 必须是小写原样。
	"trae|deepseek-v4.1-flash": {Billing: "免费忙时 x0.12 / 空闲 x0.06；付费会员 x0.06", ContextLen: 1000000, MaxOutput: 393216, InputModes: m("text", "image"), Reasoning: "关闭 + low/high/max", ToolCall: true, Structured: true},
	"zcode|glm-5.3-flash":      {Billing: "计划内额度", ContextLen: 1000000, MaxOutput: 131072, InputModes: m("text", "image", "video", "pdf"), Reasoning: "low/high/max", ToolCall: true, Structured: true},
}

// mergeRouted appends route bindings that are not yet in the static catalog
// (new channels / models) so the page always reflects the final route table.
func mergeRouted(rows []tierRow, routes []router.Row, channels map[string]bool) []tierRow {
	have := map[string]bool{}
	for _, r := range rows {
		have[tierChannel(r.Source)+"|"+r.ID] = true
	}
	out := rows
	for _, rt := range routes {
		if !channels[rt.Channel] {
			continue
		}
		key := rt.Channel + "|" + rt.UpstreamModel
		if have[key] {
			continue
		}
		have[key] = true
		row, ok := routedSpecs[key]
		if !ok {
			row = tierRow{Billing: "未收录规格"}
		}
		row.Source = rt.Channel
		row.ID = rt.UpstreamModel
		out = append(out, row)
	}
	return out
}

// classifyTiers marks rows routed/retired/new. On the first run the seen set
// is seeded without flags (so today's catalogue is the baseline); afterwards
// any unseen key is new and stays visible even when retired rows are hidden.
func classifyTiers(rows []tierRow, active, seen map[string]bool, firstRun bool, added map[string]bool) []tierRow {
	for i := range rows {
		key := tierChannel(rows[i].Source) + "|" + rows[i].ID
		rows[i].Routed = active[key]
		rows[i].Retired = !rows[i].Routed
		if !seen[key] && !added[key] {
			added[key] = true
			if !firstRun {
				rows[i].New = true
			}
		}
	}
	return rows
}

// tierSourceRank fixes the account-source order the page presents. Domestic
// pools follow their operational priority; Antigravity splits into its Gemini
// family, the Claude ledger and the standby tiers.
func tierSourceRank(source string) int {
	switch source {
	case model.ChannelWorkBuddy:
		return 0
	case model.ChannelTrae:
		return 1
	case model.ChannelZCode:
		return 2
	case model.ChannelAutoClaw:
		return 3
	case model.ChannelQoder:
		return 4
	case model.ChannelLobster:
		return 5
	}
	switch {
	case strings.HasPrefix(source, "antigravity · Gemini"):
		return 10
	case strings.HasPrefix(source, "antigravity · Claude"):
		return 11
	case strings.HasPrefix(source, "antigravity · 备用"):
		return 12
	case strings.HasPrefix(source, "antigravity"):
		return 13
	}
	return 90
}

func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }

// naturalLess compares model ids with numeric runs as numbers, so
// glm-5.3 < glm-5.3-flash and kimi-k2.7 < kimi-k3.
func naturalLess(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if isASCIIDigit(a[i]) && isASCIIDigit(b[j]) {
			ai, bj := i, j
			for i < len(a) && isASCIIDigit(a[i]) {
				i++
			}
			for j < len(b) && isASCIIDigit(b[j]) {
				j++
			}
			an := strings.TrimLeft(a[ai:i], "0")
			bn := strings.TrimLeft(b[bj:j], "0")
			if len(an) != len(bn) {
				return len(an) < len(bn)
			}
			if an != bn {
				return an < bn
			}
			continue
		}
		if a[i] != b[j] {
			return a[i] < b[j]
		}
		i++
		j++
	}
	return len(a) < len(b)
}

// sortTierRows supports the two orders the user asked for: by account source
// (default) and by model name.
func sortTierRows(rows []tierRow, mode string) {
	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := tierSourceRank(rows[i].Source), tierSourceRank(rows[j].Source)
		if mode == "model" {
			ai, aj := strings.ToLower(rows[i].ID), strings.ToLower(rows[j].ID)
			if ai != aj {
				return naturalLess(rows[i].ID, rows[j].ID)
			}
			if ri != rj {
				return ri < rj
			}
			return rows[i].Source < rows[j].Source
		}
		if ri != rj {
			return ri < rj
		}
		if rows[i].Source != rows[j].Source {
			return rows[i].Source < rows[j].Source
		}
		return naturalLess(rows[i].ID, rows[j].ID)
	})
}

func creditUnit(channel string) string {
	switch channel {
	case model.ChannelWorkBuddy, model.ChannelTrae:
		return "分"
	case model.ChannelQoder:
		return "credits"
	case model.ChannelAutoClaw:
		return "点"
	case model.ChannelLobster:
		return "credits"
	}
	return ""
}

// attachObserved fills each row with the real per-request deduction measured
// from request logs (last 7 days). Route rows map upstream ids to the client
// model names they serve, so Qoder's "GLM-5.3" still matches logs written for
// "glm-5.3". Rows without upstream credit telemetry keep the field empty.
func attachObserved(rows []tierRow, routes []router.Row, logs []model.RequestLog, since time.Time) {
	clients := map[string]map[string]bool{}
	for _, rt := range routes {
		key := rt.Channel + "|" + rt.UpstreamModel
		if clients[key] == nil {
			clients[key] = map[string]bool{}
		}
		clients[key][strings.ToLower(rt.Model)] = true
	}
	for i := range rows {
		ch := tierChannel(rows[i].Source)
		key := ch + "|" + rows[i].ID
		names := clients[key]
		if len(names) == 0 {
			names = map[string]bool{strings.ToLower(rows[i].ID): true}
		}
		var n int
		var sum, lo, hi float64
		var inTok, outTok int64
		for _, l := range logs {
			if !l.CreditKnown || l.Status != 200 || l.Channel != ch || l.Time.Before(since) {
				continue
			}
			if !names[strings.ToLower(l.Model)] {
				continue
			}
			c := l.Credit
			if n == 0 || c < lo {
				lo = c
			}
			if n == 0 || c > hi {
				hi = c
			}
			sum += c
			inTok += int64(l.InputTokens)
			outTok += int64(l.OutputTokens)
			n++
		}
		if n == 0 {
			continue
		}
		rows[i].ObservedN = n
		rows[i].ObservedAvg = round2(sum / float64(n))
		rows[i].ObservedMin = round2(lo)
		rows[i].ObservedMax = round2(hi)
		rows[i].ObservedAvgIn = inTok / int64(n)
		rows[i].ObservedAvgOut = outTok / int64(n)
		rows[i].ObservedUnit = creditUnit(ch)
	}
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func (p *Panel) currentRouteRows() []router.Row {
	if p.store != nil {
		if v, ok, err := p.store.GetSetting(KeyRoutes); err == nil && ok && v != "" {
			var rows []router.Row
			if json.Unmarshal([]byte(v), &rows) == nil {
				return rows
			}
		}
	}
	if p.gw != nil {
		return p.gw.RouteRows()
	}
	return nil
}

func (p *Panel) tierSeen() (map[string]bool, bool) {
	if p.store == nil {
		return map[string]bool{}, true
	}
	v, ok, err := p.store.GetSetting(keyTiersSeen)
	if err != nil || !ok || v == "" {
		return map[string]bool{}, true
	}
	var m map[string]bool
	if json.Unmarshal([]byte(v), &m) != nil {
		return map[string]bool{}, true
	}
	return m, false
}

// tiersAdmittedChannels is the set of domestic channels whose routes may
// appear on the 模型与档位 page (BP-136). It exists as a named function so a
// test can pin it against the router's fleet: Lobster was routed and probed
// from BP-98 onward yet never appeared here, and nothing failed — the page
// just silently omitted a whole channel.
func tiersAdmittedChannels() map[string]bool {
	return map[string]bool{
		model.ChannelWorkBuddy: true,
		model.ChannelTrae:      true,
		model.ChannelZCode:     true,
		model.ChannelAutoClaw:  true,
		model.ChannelQoder:     true,
		model.ChannelLobster:   true,
	}
}

// handleTiers serves both catalogs for the panel page. Rows outside the final
// route table are retired (the page hides them by default, with a toggle);
// first-seen rows are new and always shown.
func (p *Panel) handleTiers(w http.ResponseWriter, r *http.Request) {
	routes := p.currentRouteRows()
	active := map[string]bool{}
	for _, rt := range routes {
		active[rt.Channel+"|"+rt.UpstreamModel] = true
	}
	domesticChannels := tiersAdmittedChannels()
	foreignChannels := map[string]bool{model.ChannelAntigravity: true}
	domestic := mergeRouted(domesticCatalog(), routes, domesticChannels)
	foreign := mergeRouted(foreignCatalog(), routes, foreignChannels)

	seen, firstRun := p.tierSeen()
	added := map[string]bool{}
	domestic = classifyTiers(domestic, active, seen, firstRun, added)
	foreign = classifyTiers(foreign, active, seen, firstRun, added)
	if len(added) > 0 && p.store != nil {
		for key := range added {
			seen[key] = true
		}
		if b, err := json.Marshal(seen); err == nil {
			_ = p.store.SetSetting(keyTiersSeen, string(b))
		}
	}
	// Real per-request deductions: the catalogue multiplier alone is too
	// abstract, so every routed row also carries the measured credit spend
	// from the request logs (upstream-reported, last 7 days).
	if p.store != nil {
		if logs, err := p.store.RecentRequestLogs(20000); err == nil {
			since := time.Now().Add(-7 * 24 * time.Hour)
			attachObserved(domestic, routes, logs, since)
			attachObserved(foreign, routes, logs, since)
		}
	}
	sortMode := r.URL.Query().Get("sort")
	if sortMode != "model" {
		sortMode = "source"
	}
	sortTierRows(domestic, sortMode)
	sortTierRows(foreign, sortMode)
	writeJSON(w, http.StatusOK, map[string]any{
		"domestic":  domestic,
		"foreign":   foreign,
		"sort":      sortMode,
		"unit_help": "实扣=近 7 天上游回传的每次请求扣分（WorkBuddy usage.credit / Qoder usage.credits）；AutoClaw 见账号页「积分详情」逐笔流水",
		"source":    "规格来源 models.dev（2026-10-16 复核；reasoning=关闭+官方 effort 档位，官方 provider 条目优先）",
	})
}
