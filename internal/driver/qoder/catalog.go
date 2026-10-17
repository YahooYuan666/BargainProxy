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
}

// ModelCatalog maps display names to upstream keys.
type ModelCatalog struct {
	ModelMap     map[string]string
	VisionModels map[string]bool
	Caps         map[string]*ModelCaps
	DefaultName  string
}

const defaultMaxOutputTokens = 32000
const preferredDefaultKey = "qmodel_latest"

// DefaultModelMap is the static fallback catalogue (catalog-v7).
var DefaultModelMap = map[string]string{
	"Qwen3.8-Max":       "qmodel_38max",
	"Qwen3.8-Flash":     "qfmodel",
	"Qwen3.7-Max":       "qmodel_latest",
	"Qwen3.7-Plus":      "qmodel",
	"Qwen3.7-Flash":     "q37fmodel",
	"DeepSeek-V4-Pro":   "dmodel",
	"DeepSeek-Flash":    "dfmodel",
	"GLM-5.3":           "gmodel",
	"GLM-5.3-Flash":     "gfmodel",
	"GLM-5.2":           "gm51model",
	"Kimi-K3":           "kmodel_latest",
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

// extractCatalog parses the model/list response (chat scene).
func extractCatalog(raw map[string]interface{}) *ModelCatalog {
	if raw == nil {
		return nil
	}
	sceneRaw, ok := raw["chat"]
	if !ok {
		return nil
	}
	sceneList, ok := sceneRaw.([]interface{})
	if !ok {
		return nil
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
		if key == "" || name == "" || key == "auto" {
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
		caps[key] = &ModelCaps{IsReasoning: reasoningCapability(m), MaxOutputTokens: maxOut}
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
