package panel

import (
	"testing"

	"bargainproxy/internal/model"
	"bargainproxy/internal/router"
)

// BP-136: the 模型与档位 page never showed the Lobster channel — BP-98 wired
// the driver, the routes and the account pool, but the tiers page's three
// hardcoded channel sets (admission, display order, credit unit) were never
// extended, so all eight lobster route rows were dropped at mergeRouted. The
// user asked "模型与档位页面为什么不显示 Lobster 账号,之前没评估么?" — the
// honest answer is the checklist missed a fourth panel surface.
func TestLobsterRoutesReachTheTiersPage(t *testing.T) {
	routes := []router.Row{
		{Model: "deepseek-v4.1-flash", Channel: model.ChannelLobster, UpstreamModel: "deepseek-flash"},
		{Model: "kimi-k3", Channel: model.ChannelLobster, UpstreamModel: "kimi-k3"},
	}
	got := mergeRouted(nil, routes, tiersAdmittedChannels())
	byKey := map[string]tierRow{}
	for _, r := range got {
		byKey[r.Source+"|"+r.ID] = r
	}
	row, ok := byKey["lobster|deepseek-flash"]
	if !ok {
		t.Fatalf("lobster route row missing: %+v", got)
	}
	if row.Billing == "未收录规格" || row.Billing == "" {
		t.Fatalf("lobster row must carry real specs, got %+v", row)
	}
	if row.ContextLen != 1000000 {
		t.Fatalf("lobster deepseek-flash context = %d, want 1000000 (pricing catalog)", row.ContextLen)
	}
	if k := byKey["lobster|kimi-k3"]; !ok || k.Billing == "" || k.ContextLen != 1048576 {
		t.Fatalf("lobster kimi-k3 row wrong: %+v", k)
	}
}

// TestTiersAdmittedChannelsMatchTheDomesticFleet pins the admission set: every
// domestic channel the router can carry must appear on the tiers page. A new
// channel added to the router without updating this set repeats the lobster
// miss; this test is the checklist for it.
func TestTiersAdmittedChannelsMatchTheDomesticFleet(t *testing.T) {
	want := []string{
		model.ChannelWorkBuddy, model.ChannelTrae, model.ChannelZCode,
		model.ChannelAutoClaw, model.ChannelQoder, model.ChannelLobster,
	}
	admitted := tiersAdmittedChannels()
	for _, ch := range want {
		if !admitted[ch] {
			t.Errorf("channel %q is routed domestically but the tiers page never shows it", ch)
		}
	}
}

// TestCreditUnitCoversAllAdmittedChannels: every CREDIT-POOL channel needs a
// unit for the 实扣 column. ZCode is deliberately absent — it bills inside a
// subscription plan ("计划内额度") with no per-request unit, and inventing one
// would be the kind of fake precision BP-67 removed.
func TestCreditUnitCoversAllAdmittedChannels(t *testing.T) {
	units := map[string]string{
		model.ChannelWorkBuddy: "分",
		model.ChannelTrae:      "分",
		model.ChannelAutoClaw:  "点",
		model.ChannelQoder:     "credits",
		model.ChannelLobster:   "credits",
	}
	for ch, want := range units {
		if got := creditUnit(ch); got != want {
			t.Errorf("creditUnit(%q) = %q, want %q", ch, got, want)
		}
	}
}
