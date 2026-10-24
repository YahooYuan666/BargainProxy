// WorkBuddy registration endpoint (BP-105): one panel button that writes the
// gateway's routed models into the local WorkBuddy desktop app's custom
// models.json. Same wbregister logic as scripts/wbmodels; the catalog comes
// straight from the same in-process data the 模型与档位 page serves, so the
// button and the page can never disagree.
package panel

import (
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"bargainproxy/internal/model"
	"bargainproxy/internal/wbregister"
)

type wbRegisterRequest struct {
	// Target overrides the default file (tests / non-standard installs).
	Target string `json:"target,omitempty"`
	// Prune drops stale BargainProxy-marked entries no longer routed
	// (default true).
	Prune *bool `json:"prune,omitempty"`
	// DryRun reports what would happen without writing.
	DryRun bool `json:"dry_run,omitempty"`
}

type wbRegisterResult struct {
	Generated int    `json:"generated"`
	Added     int    `json:"added"`
	Updated   int    `json:"updated"`
	Kept      int    `json:"kept"`
	Skipped   int    `json:"skipped_user"`
	Removed   int    `json:"removed"`
	Total     int    `json:"total"`
	Target    string `json:"target"`
	Wrote     bool   `json:"wrote"`
	Summary   string `json:"summary"`
}

// handleWorkBuddyRegister implements POST /panel/api/workbuddy/register.
// WorkBuddy must be on this machine (the file path is local) — the button is
// a local-machine convenience, matching how the gateway and WorkBuddy both
// live on the same desktop.
func (p *Panel) handleWorkBuddyRegister(w http.ResponseWriter, r *http.Request) {
	var req wbRegisterRequest
	if !decode(w, r, &req) {
		return
	}
	prune := true
	if req.Prune != nil {
		prune = *req.Prune
	}
	target := req.Target
	if target == "" {
		var err error
		target, err = wbregister.DefaultPath()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "无法定位用户目录：" + err.Error()})
			return
		}
	}
	target = filepath.Clean(target)

	// In-process catalog: same domestic/foreign rows + route merge + route
	// classification the tiers handler serves, so button and page agree by
	// construction (Routed is what wbregister filters on).
	routes := p.currentRouteRows()
	active := map[string]bool{}
	for _, rt := range routes {
		active[rt.Channel+"|"+rt.UpstreamModel] = true
	}
	domestic := mergeRouted(domesticCatalog(), routes, map[string]bool{
		model.ChannelWorkBuddy: true, model.ChannelTrae: true, model.ChannelZCode: true,
		model.ChannelAutoClaw: true, model.ChannelQoder: true,
	})
	foreign := mergeRouted(foreignCatalog(), routes, map[string]bool{model.ChannelAntigravity: true})
	// seen/firstRun only affect the "new" badge on the page; for registration
	// a plain classify is correct (no store mutation on a read path).
	seen := map[string]bool{}
	freshlySeen := map[string]bool{}
	domestic = classifyTiers(domestic, active, seen, true, freshlySeen)
	foreign = classifyTiers(foreign, active, seen, true, freshlySeen)

	// wbregister consumes the JSON shape of tierRow; converting through
	// marshal keeps the two definitions in lockstep.
	wbDomestic, wbForeign := convertTiers(domestic), convertTiers(foreign)
	baseURL := p.selfBaseURL()
	var entries []wbregister.EntryID
	for _, mi := range p.gw.ListModels() {
		entries = append(entries, wbregister.EntryID{ID: mi.Model})
	}

	generated, err := wbregister.FromCatalog(entries, wbDomestic, wbForeign, baseURL, p.cfg.APIKey)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "构建模型清单失败：" + err.Error()})
		return
	}
	existing, shape, err := wbregister.ReadModelsJSON(target)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "读取 WorkBuddy 配置失败：" + err.Error() + "（" + target + "）"})
		return
	}
	merged, added, updated, kept, removed, skipped := wbregister.Merge(existing, generated, prune)

	if req.DryRun {
		writeJSON(w, http.StatusOK, wbRegisterResult{
			Generated: len(generated), Added: added, Updated: updated,
			Kept: kept, Skipped: skipped, Removed: removed, Total: len(merged),
			Target: target, Wrote: false,
			Summary: wbSummary("预览（未写文件）", len(generated), added, updated, kept, skipped, removed, len(merged), ""),
		})
		return
	}

	if _, err := wbregister.WriteModelsJSON(target, shape, merged); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "写入 WorkBuddy 配置失败：" + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, wbRegisterResult{
		Generated: len(generated), Added: added, Updated: updated,
		Kept: kept, Skipped: skipped, Removed: removed, Total: len(merged),
		Target: target, Wrote: true,
		Summary: wbSummary("完成", len(generated), added, updated, kept, skipped, removed, len(merged), target),
	})
}

func convertTiers(rows []tierRow) []wbregister.TierRow {
	out := make([]wbregister.TierRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, wbregister.TierRow{
			ID: r.ID, ContextLen: r.ContextLen, MaxOutput: r.MaxOutput,
			InputModes: r.InputModes, Reasoning: r.Reasoning,
			ToolCall: r.ToolCall, Routed: r.Routed,
		})
	}
	return out
}

// selfBaseURL is the URL WorkBuddy entries point at: this gateway's OpenAI
// endpoint on the configured listen address. A wildcard listen (":13140")
// is rewritten to loopback — the entries are for this same machine.
func (p *Panel) selfBaseURL() string {
	addr := p.cfg.Listen
	if addr == "" {
		addr = "127.0.0.1:13140"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	return "http://" + addr + "/v1"
}

func wbSummary(prefix string, gen, added, updated, kept, skipped, removed, total int, target string) string {
	s := prefix + "：网关模型 " + strconv.Itoa(gen) + " 个 — 新增 " + strconv.Itoa(added) +
		"、刷新 " + strconv.Itoa(updated) + "、保留你自己的 " + strconv.Itoa(kept) +
		"（其中同名 " + strconv.Itoa(skipped) + " 个以你的配置优先）、清理失效 " + strconv.Itoa(removed) +
		"；完成后共 " + strconv.Itoa(total) + " 条"
	if target != "" {
		s += "（" + target + "）"
	}
	return s
}
