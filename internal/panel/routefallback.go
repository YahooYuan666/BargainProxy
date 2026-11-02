package panel

import (
	"sort"
	"strings"

	"bargainproxy/internal/router"
)

// BP-270 (misakano 410398f1d6, PR #137): a limited model that has another
// route binding is not an outage — the gateway cascades the request to the
// sibling. The panel annotates the cooldown rows and the lock table with
// where the traffic goes, so "model locked" is not misread as "service down".
// The lookup mirrors the gateway's own cascade: same client model, bindings
// by priority, first sibling whose (channel, upstream) differs.

// routeKey identifies one (channel, model) cooldown/lock row. Struct key on
// purpose: a string join needs a separator, and an invisible separator in a
// map key is the exact trap lockKeySep documents in pool.
type routeKey struct {
	Channel string
	Model   string
}

// fallbackChannelLabels renders the sibling's channel for the annotation;
// same wording the panel's JS chanNames uses (BP-67's Chinese channel names).
var fallbackChannelLabels = map[string]string{
	"antigravity":  "Google（Antigravity）",
	"workbuddy":    "WorkBuddy",
	"trae":         "Trae CN",
	"zcode":        "ZCode",
	"autoclaw":     "AutoClaw",
	"qoder":        "Qoder",
	"lobster":      "LobsterAI（有道龙虾）",
	"openaicompat": "OpenAI 兼容兜底",
	"opencodezen":  "OpenCode 免费道",
}

// routeFallbackTargets maps every (channel, model) that appears in the route
// table to the annotation text for its first differing sibling binding, ""
// when the binding has no sibling (single-binding models route nowhere else).
// A cooldown keyed by M matches a client model X when a binding's upstream id
// is M or X itself equals M — cooldown keys are the id the account drove,
// which is the upstream id on the alias channels.
func routeFallbackTargets(rows []router.Row) map[routeKey]string {
	if len(rows) == 0 {
		return nil
	}
	type binding struct {
		channel, upstream string
		priority          int
	}
	byModel := map[string][]binding{}
	order := []string{}
	for _, r := range rows {
		if strings.TrimSpace(r.Model) == "" || strings.TrimSpace(r.Channel) == "" {
			continue
		}
		if _, seen := byModel[r.Model]; !seen {
			order = append(order, r.Model)
		}
		byModel[r.Model] = append(byModel[r.Model], binding{r.Channel, r.UpstreamModel, r.Priority})
	}
	out := map[routeKey]string{}
	for _, model := range order {
		bs := byModel[model]
		sort.SliceStable(bs, func(i, j int) bool { return bs[i].priority < bs[j].priority })
		for _, b := range bs {
			for _, sib := range bs {
				if sib.channel == b.channel && sib.upstream == b.upstream {
					continue
				}
				label := sib.upstream
				if sib.channel != b.channel {
					label = sib.upstream + " · " + fallbackChannelLabels[sib.channel]
				}
				out[routeKey{b.channel, b.upstream}] = label
				if model != b.upstream {
					out[routeKey{b.channel, model}] = label
				}
				break
			}
		}
	}
	return out
}
