// Qoder model catalogue: live /algo/api/v2/model/list with a 10-minute cache
// and the reference project's static fallback. Ported from agentpoints2
// internal/qoder (MIT reference); the live list is authoritative, the static
// map only covers a cold start with no usable account.
package qoder

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// ModelCaps is one model's capability snapshot.
type ModelCaps struct {
	IsReasoning     bool
	MaxOutputTokens int
	// MaxInputTokens (BP-163): the listing's input ceiling. The official
	// client puts it in model_config.max_input_tokens; without it the server
	// caps at its ~200K default even for models opened to 1M.
	MaxInputTokens int
	// Pricing (BP-98 Qoder-3, magpie-community/plugins packages/qoder 0.2.30
	// freeOf/rateOf): Rate is the credits multiple the client shows ("0.5×");
	// RateWas is the struck-through price while a discount runs (0 = none);
	// Free mirrors freeOf — a 0 price is only free when no pre-promotion
	// price existed (a promotion's 0 is a discount, not free), and is_free
	// counts only when the listing carries no price at all (Qoder lists
	// is_free:true on Qwen3.8-Max at 0.5× — an off-peak discount, not free).
	Rate    float64
	RateWas float64
	Free    bool
	// Efforts (BP-98 Qoder-9, magpie 0.2.30 modelInfo): the reasoning levels
	// thinking_config lists, low→max; DefaultEffort is the is_default one.
	Efforts       []string
	DefaultEffort string
}

// ModelCatalog maps display names to upstream keys.
type ModelCatalog struct {
	ModelMap     map[string]string
	VisionModels map[string]bool
	Caps         map[string]*ModelCaps
	DefaultName  string
}

const defaultMaxOutputTokens = 32000

// preferredDefaultKey is the model a request without a usable name falls back
// to. qmodel_latest (Qwen3.7-Max) served until 2026-10-26, when Zhengyuuuui/
// qoder2api 872d9b9 pulled the live catalogue and the whole Qwen3.7 family
// was gone; Qwen3.8-Flash is the surviving cheap workhorse.
const preferredDefaultKey = "qfmodel"

// DefaultModelMap is the static fallback catalogue. 2026-10-26 refresh
// (Zhengyuuuui/qoder2api 872d9b9, live pull): qmodel / qmodel_latest /
// q37fmodel / gm51model are removed upstream (requests 400 非法模型), and
// kmodel_latest upgraded in place to Kimi-K3.1-Pro (rate 1.4 → 1). The
// desktop-only flagships (smodel/cmodel/fast) are NOT listed here: they only
// exist when the desktop header family is on (channels.qoder.desktop_protocol),
// and the live catalogue carries them in that mode.
var DefaultModelMap = map[string]string{
	"Qwen3.8-Max":       "qmodel_38max",
	"Qwen3.8-Flash":     "qfmodel",
	"DeepSeek-V4-Pro":   "dmodel",
	"DeepSeek-Flash":    "dfmodel",
	"GLM-5.3":           "gmodel",
	"GLM-5.3-Flash":     "gfmodel",
	"Kimi-K3.1-Pro":     "kmodel_latest",
	"Kimi-K2.8-Preview": "kmodel",
	"MiniMax-M2.7":      "mmodel",
}

func defaultCatalog() *ModelCatalog {
	mm := map[string]string{}
	for k, v := range DefaultModelMap {
		mm[k] = v
	}
	return &ModelCatalog{ModelMap: mm, VisionModels: map[string]bool{}, Caps: map[string]*ModelCaps{},
		DefaultName: nameForKey(mm, preferredDefaultKey)}
}

func nameForKey(modelMap map[string]string, key string) string {
	for name, k := range modelMap {
		if k == key {
			return name
		}
	}
	keys := make([]string, 0, len(modelMap))
	for k := range modelMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, want := range []string{"flash", "lite", "plus"} {
		for _, k := range keys {
			if strings.Contains(strings.ToLower(k), want) {
				return k
			}
		}
	}
	if len(keys) > 0 {
		return keys[0]
	}
	return ""
}

func reasoningCapability(m map[string]interface{}) bool {
	if tc, ok := m["thinking_config"]; ok {
		if tcMap, isMap := tc.(map[string]interface{}); isMap {
			enabled, present := tcMap["enabled"]
			return present && enabled != nil
		}
		if tc != nil {
			return true
		}
	}
	if v, present := m["is_reasoning"]; present {
		return enableFlag(v)
	}
	return true
}

func enableFlag(v interface{}) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return !(x == "false" || x == "0" || x == "False" || x == "FALSE")
	case float64:
		return x != 0
	case nil:
		return true
	}
	return true
}

func positiveInt(v interface{}) (int, bool) {
	switch x := v.(type) {
	case float64:
		if n := int(x); float64(n) == x && n > 0 {
			return n, true
		}
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(x), "%d", &n); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// effortRank is the global ordering of reasoning levels (magpie 0.2.30
// EFFORT_RANK: none < minimal < low < medium < high < xhigh < max < ultra).
var effortRank = map[string]int{
	"none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6, "ultra": 7,
}

// parseEfforts reads the thinking_config effort table (magpie 0.2.30
// modelInfo): enabled.efforts names the levels, is_default marks the
// default. Sorted by rank so callers can reason about adjacency.
func parseEfforts(m map[string]interface{}) ([]string, string) {
	tc, ok := m["thinking_config"].(map[string]interface{})
	if !ok {
		return nil, ""
	}
	enabled, ok := tc["enabled"].(map[string]interface{})
	if !ok {
		return nil, ""
	}
	effortsRaw, ok := enabled["efforts"].(map[string]interface{})
	if !ok {
		return nil, ""
	}
	var efforts []string
	def := ""
	for name, e := range effortsRaw {
		efforts = append(efforts, name)
		if em, isMap := e.(map[string]interface{}); isMap {
			if isDef, ok := em["is_default"].(bool); ok && isDef {
				def = name
			}
		}
	}
	sort.Slice(efforts, func(i, j int) bool {
		ri, rj := effortRank[efforts[i]], effortRank[efforts[j]]
		if ri != rj {
			return ri < rj
		}
		return efforts[i] < efforts[j]
	})
	return efforts, def
}

// fitEffort snaps a requested effort to the nearest level the model names
// (magpie 0.2.30 fitEffort): exact wins; a tie prefers the higher level;
// ultra aliases max first. Unknown/empty levels pass through unchanged.
func fitEffort(want string, levels []string) string {
	if want == "ultra" {
		for _, l := range levels {
			if l == "max" {
				want = "max"
				break
			}
		}
	}
	if len(levels) == 0 {
		return want
	}
	for _, l := range levels {
		if l == want {
			return want
		}
	}
	at, ok := effortRank[want]
	if !ok {
		return want
	}
	best, dist := want, len(effortRank)
	for _, l := range levels {
		i, ok := effortRank[l]
		if !ok || l == "none" {
			continue
		}
		d := i - at
		if d < 0 {
			d = -d
		}
		if d < dist || (d == dist && i > at) {
			best, dist = l, d
		}
	}
	return best
}
func sceneEntries(raw map[string]interface{}) []interface{} {
	scene := func(name string) []interface{} {
		v, ok := raw[name].([]interface{})
		if !ok {
			return nil
		}
		return v
	}
	app, chat := scene("app"), scene("chat")
	out := append([]interface{}{}, app...)
	after := len(out) - 1
	for _, m := range chat {
		key := ""
		if mm, ok := m.(map[string]interface{}); ok {
			key, _ = mm["key"].(string)
		}
		found := -1
		for i, x := range out {
			if xm, ok := x.(map[string]interface{}); ok {
				if xk, _ := xm["key"].(string); xk == key && key != "" {
					found = i
					break
				}
			}
		}
		if found >= 0 {
			after = found
			continue
		}
		after++
		if after < len(out) {
			out = append(out[:after], append([]interface{}{m}, out[after:]...)...)
		} else {
			out = append(out, m)
		}
	}
	return out
}

// parsePricing fills Rate/RateWas/Free from a listing entry, translated from
// magpie-community/plugins packages/qoder 0.2.30 freeOf/rateOf (see
// ModelCaps for the semantics).
func parsePricing(m map[string]interface{}) (rate, rateWas float64, free bool) {
	num := func(v interface{}) float64 {
		if f, ok := v.(float64); ok {
			return f
		}
		return 0
	}
	price, hasPrice := m["price_factor"].(float64)
	if !hasPrice {
		price, hasPrice = m["priceFactor"].(float64)
	}
	if !hasPrice {
		// No price at all: only then does is_free speak.
		return 0, 0, enableFlag(m["is_free"])
	}
	promo, _ := m["promotion"].(map[string]interface{})
	if promo == nil {
		promo, _ = m["prommotion"].(map[string]interface{}) // upstream's typo, magpie parity
	}
	var before, discount float64
	active := false
	if promo != nil {
		active, _ = promo["active"].(bool)
		before, _ = promo["before_promotion_price_factor"].(float64)
		if before == 0 {
			before = num(promo["beforePromotionPriceFactor"])
		}
		discount = num(promo["discount_factor"])
		if discount == 0 {
			discount = num(promo["discountFactor"])
		}
	}
	rate = price
	rateWas = num(m["original_price_factor"])
	if rateWas == 0 {
		rateWas = num(m["originalPriceFactor"])
	}
	discounted := active && before > 0
	if discounted {
		rateWas = before
		if rate == 0 {
			rate = before * discount
		}
	}
	if rateWas <= rate {
		rateWas = 0
	}
	free = rate == 0 && !discounted
	return rate, rateWas, free
}

// sceneEntries merges the app and chat scenes (BP-98 Qoder-6, magpie 0.2.30
// sceneModels): app entries come first and win over chat twins, chat-only
// entries keep their relative place after them. The app scene carries models
// the CLI's chat scene omits (fast/ultimate tiers).

// extractCatalog parses the model/list response (app scene merged over chat).
func extractCatalog(raw map[string]interface{}) *ModelCatalog {
	if raw == nil {
		return nil
	}
	sceneList := sceneEntries(raw)
	if sceneList == nil {
		// No app scene: a bare chat-only body.
		if _, ok := raw["chat"]; !ok {
			return nil
		}
	}
	modelMap := map[string]string{}
	vision := map[string]bool{}
	caps := map[string]*ModelCaps{}
	for _, item := range sceneList {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if enableVal, has := m["enable"]; has && !enableFlag(enableVal) {
			continue
		}
		key, _ := m["key"].(string)
		name, _ := m["display_name"].(string)
		if key == "" || name == "" || key == "auto" || key == "default" {
			continue
		}
		// The app scene wins over chat twins: the first entry for a key in
		// the merged order (app first) is the one kept.
		if _, dup := caps[key]; dup {
			continue
		}
		modelMap[name] = key
		if isVL, ok := m["is_vl"].(bool); ok && isVL {
			vision[name] = true
		}
		maxOut, ok := positiveInt(m["max_output_tokens"])
		if !ok {
			maxOut = defaultMaxOutputTokens
		}
		maxIn, _ := positiveInt(m["max_input_tokens"])
		rate, rateWas, free := parsePricing(m)
		efforts, defEffort := parseEfforts(m)
		caps[key] = &ModelCaps{IsReasoning: reasoningCapability(m), MaxOutputTokens: maxOut,
			MaxInputTokens: maxIn,
			Rate: rate, RateWas: rateWas, Free: free, Efforts: efforts, DefaultEffort: defEffort}
	}
	if len(modelMap) == 0 {
		return nil
	}
	return &ModelCatalog{ModelMap: modelMap, VisionModels: vision, Caps: caps,
		DefaultName: nameForKey(modelMap, preferredDefaultKey)}
}

func (c *ModelCatalog) maxOutputTokens(key string) int {
	if caps, ok := c.Caps[key]; ok && caps.MaxOutputTokens > 0 {
		return caps.MaxOutputTokens
	}
	return defaultMaxOutputTokens
}

func (c *ModelCatalog) reasoningDefault(key string) bool {
	if caps, ok := c.Caps[key]; ok {
		return caps.IsReasoning
	}
	return true
}

func (c *ModelCatalog) keys() []string {
	keys := make([]string, 0, len(c.ModelMap))
	for k := range c.ModelMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// resolveModel maps a display name to (display, upstream key).
func (c *ModelCatalog) resolveModel(model string) (string, string, error) {
	if model != "" {
		if key := c.ModelMap[model]; key != "" {
			return model, key, nil
		}
		return "", "", fmt.Errorf("unsupported model '%s'; supported: %s", model, strings.Join(c.keys(), ", "))
	}
	name := c.DefaultName
	if name == "" {
		for k := range c.ModelMap {
			name = k
			break
		}
	}
	return name, c.ModelMap[name], nil
}

// getCatalog returns the cached catalogue, refreshing it from the upstream
// every 10 minutes. Any failure falls back to the static map.
func (d *Driver) getCatalog(ctx context.Context, rt *runtime) *ModelCatalog {
	d.catMu.Lock()
	if d.cat != nil && time.Since(d.catAt) < 10*time.Minute {
		cat := d.cat
		d.catMu.Unlock()
		return cat
	}
	d.catMu.Unlock()

	var fetched *ModelCatalog
	if rt != nil {
		if err := d.ensureFreshSession(ctx, rt); err == nil {
			rt.mu.Lock()
			sess := rt.sess
			rt.mu.Unlock()
			if sess != nil {
				if raw, err := d.fetchModelCatalog(ctx, sess); err == nil {
					fetched = extractCatalog(raw)
				} else {
					log.Printf("qoder model/list: %v (fallback)", err)
				}
			}
		} else {
			log.Printf("qoder model/list session: %v (fallback)", err)
		}
	}
	if fetched == nil {
		fetched = defaultCatalog()
	}
	d.catMu.Lock()
	d.cat = fetched
	d.catAt = time.Now()
	d.catMu.Unlock()
	return fetched
}
