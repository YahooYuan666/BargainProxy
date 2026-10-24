// Package wbregister registers the BargainProxy gateway models into the
// WorkBuddy desktop app's custom-model file (%USERPROFILE%\.workbuddy\
// models.json, OpenAI chat/completions protocol — the only protocol
// WorkBuddy custom models speak, and the gateway's best-tested wire).
//
// One implementation serves both entry points (BP-105): the panel's
// 「登记进 WorkBuddy」 button and the scripts/wbmodels CLI. Specs (context
// length, max output, input modes, reasoning efforts) come from the panel's
// tiers API aggregated across every routed channel. Entries the user added
// themselves are never overwritten; entries generated here (vendor
// "BargainProxy") are refreshed or pruned.
package wbregister

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const VendorMark = "BargainProxy"

// DefaultPath is WorkBuddy's user-level custom-model file.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".workbuddy", "models.json"), nil
}

type tierRow struct {
	ID         string   `json:"id"`
	ContextLen int      `json:"context_length"`
	MaxOutput  int      `json:"max_output"`
	InputModes []string `json:"input_modes"`
	Reasoning  string   `json:"reasoning"`
	ToolCall   bool     `json:"tool_call"`
	Routed     bool     `json:"routed"`
}

// TierRow is the exported input shape for FromCatalog (in-process callers
// such as the panel endpoint convert their rows to this).
type TierRow struct {
	ID         string   `json:"id"`
	ContextLen int      `json:"context_length"`
	MaxOutput  int      `json:"max_output"`
	InputModes []string `json:"input_modes"`
	Reasoning  string   `json:"reasoning"`
	ToolCall   bool     `json:"tool_call"`
	Routed     bool     `json:"routed"`
}

// EntryID identifies one /v1/models entry (the whitelist-curated list).
type EntryID struct {
	ID string `json:"id"`
}

type modelsResp struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// Model is one entry in WorkBuddy's models.json (field names per the app's
// own saveLocalCustomModel shape).
type Model struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	Vendor            string    `json:"vendor"`
	URL               string    `json:"url"`
	APIKey            string    `json:"apiKey"`
	SupportsToolCall  bool      `json:"supportsToolCall"`
	SupportsImages    bool      `json:"supportsImages"`
	SupportsReasoning bool      `json:"supportsReasoning"`
	UseCustomProtocol bool      `json:"useCustomProtocol"`
	Reasoning         Reasoning `json:"reasoning"`
	MaxInputTokens    int       `json:"maxInputTokens"`
	MaxOutputTokens   int       `json:"maxOutputTokens"`
	Tags              []string  `json:"tags,omitempty"`
	Disabled          bool      `json:"disabled,omitempty"`
}

type Reasoning struct {
	SupportedEfforts   []string `json:"supportedEfforts,omitempty"`
	CanDisableThinking bool     `json:"canDisableThinking,omitempty"`
}

// Build fetches /v1/models (the whitelist-curated list) and /panel/api/tiers
// (specs aggregated over routed bindings) and produces one entry per model.
func Build(panelURL, apiKey string, timeout time.Duration) ([]Model, error) {
	client := &http.Client{Timeout: timeout}
	var list modelsResp
	if err := getJSON(client, strings.TrimRight(panelURL, "/")+"/v1/models", &list); err != nil {
		return nil, fmt.Errorf("GET /v1/models: %w", err)
	}
	var tiers struct {
		Domestic []tierRow `json:"domestic"`
		Foreign  []tierRow `json:"foreign"`
	}
	if err := getJSON(client, strings.TrimRight(panelURL, "/")+"/panel/api/tiers?sort=source", &tiers); err != nil {
		return nil, fmt.Errorf("GET /panel/api/tiers: %w", err)
	}
	return FromCatalog(toEntries(list.Data), toTierRows(tiers.Domestic), toTierRows(tiers.Foreign), panelURL, apiKey)
}

func toTierRows(rows []tierRow) []TierRow {
	out := make([]TierRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, TierRow(r))
	}
	return out
}

func toEntries(list []struct {
	ID string `json:"id"`
}) []EntryID {
	out := make([]EntryID, 0, len(list))
	for _, e := range list {
		out = append(out, EntryID{ID: e.ID})
	}
	return out
}

// FromCatalog is the in-memory core behind Build: given the /v1/models
// entries and the tiers rows (both already decoded), produce one entry per
// model. The panel calls this directly with its own gateway's data — no
// self-HTTP round trip — while the CLI goes through Build.
func FromCatalog(entries []EntryID, domestic, foreign []TierRow, panelURL, apiKey string) ([]Model, error) {
	// Union the routed rows per canonical model; specs are about capability,
	// take the max/union across channels.
	type agg struct {
		ctx, out int
		modes    map[string]bool
		tools    bool
		efforts  map[string]bool
		disable  bool
		known    bool
	}
	byModel := map[string]*agg{}
	absorb := func(rows []TierRow) {
		for _, r := range rows {
			if !r.Routed {
				continue
			}
			a := byModel[r.ID]
			if a == nil {
				a = &agg{modes: map[string]bool{}, efforts: map[string]bool{}}
				byModel[r.ID] = a
			}
			a.known = true
			if r.ContextLen > a.ctx {
				a.ctx = r.ContextLen
			}
			if r.MaxOutput > a.out {
				a.out = r.MaxOutput
			}
			for _, m := range r.InputModes {
				a.modes[m] = true
			}
			if r.ToolCall {
				a.tools = true
			}
			efforts, disable := ParseReasoning(r.Reasoning)
			for _, e := range efforts {
				a.efforts[e] = true
			}
			if disable {
				a.disable = true
			}
		}
	}
	absorb(domestic)
	absorb(foreign)

	base := strings.TrimRight(panelURL, "/")
	var out []Model
	for _, mi := range entries {
		a := byModel[mi.ID]
		if a == nil || !a.known {
			continue // not in the routed catalog; skip rather than guess specs
		}
		efforts := make([]string, 0, len(a.efforts))
		for e := range a.efforts {
			efforts = append(efforts, e)
		}
		sort.Strings(efforts)
		out = append(out, Model{
			ID:                mi.ID,
			Name:              mi.ID,
			Vendor:            VendorMark,
			URL:               base + "/v1",
			APIKey:            apiKey,
			SupportsToolCall:  a.tools,
			SupportsImages:    a.modes["image"],
			SupportsReasoning: len(efforts) > 0 || a.disable,
			UseCustomProtocol: false, // OpenAI chat/completions
			Reasoning: Reasoning{
				SupportedEfforts:   efforts,
				CanDisableThinking: a.disable,
			},
			MaxInputTokens:  a.ctx,
			MaxOutputTokens: a.out,
			Tags:            []string{"custom"},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ParseReasoning reads the tiers page's reasoning text ("关闭 + low/high/max",
// "high/max", "关闭（… 仅 toggle）") into effort lists plus a disable flag.
// Unknown shapes yield no efforts and no disable flag — nothing invented.
func ParseReasoning(s string) (efforts []string, canDisable bool) {
	known := map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return !('a' <= r && r <= 'z') }) {
		if known[part] {
			efforts = append(efforts, part)
		}
	}
	if strings.Contains(s, "关闭") {
		canDisable = true
	}
	return efforts, canDisable
}

// Merge replaces/refreshes generated entries by id, keeps everything the
// user added, and optionally prunes BargainProxy-marked entries that are no
// longer routed. An existing entry with the same id under a different vendor
// is the user's own configuration — it wins and the generated one is skipped.
func Merge(existing, generated []Model, prune bool) (merged []Model, added, updated, kept, removed, skipped int) {
	gen := map[string]Model{}
	for _, m := range generated {
		gen[m.ID] = m
	}
	seen := map[string]bool{}
	for _, e := range existing {
		g, ok := gen[e.ID]
		if ok && e.Vendor == VendorMark {
			merged = append(merged, g)
			updated++
			seen[e.ID] = true
			continue
		}
		if ok {
			merged = append(merged, e) // user's own entry with this id wins
			kept++
			skipped++
			seen[e.ID] = true
			continue
		}
		if prune && e.Vendor == VendorMark {
			removed++
			continue
		}
		merged = append(merged, e)
		kept++
	}
	for _, g := range generated {
		if !seen[g.ID] {
			merged = append(merged, g)
			added++
		}
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	return merged, added, updated, kept, removed, skipped
}

// ReadModelsJSON accepts both top-level shapes WorkBuddy writes: a bare
// array or {"models": [...]}. The shape is returned so the write keeps it.
func ReadModelsJSON(path string) ([]Model, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "array", nil
		}
		return nil, "", err
	}
	var arr []Model
	if json.Unmarshal(b, &arr) == nil {
		return arr, "array", nil
	}
	var obj struct {
		Models []Model `json:"models"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, "", fmt.Errorf("%s: not a recognizable models.json (%v)", path, err)
	}
	return obj.Models, "object", nil
}

// WriteModelsJSON writes the merged list, keeping the file's original shape,
// after making a timestamped backup. A fresh machine may not have the
// directory yet (WorkBuddy installed but never launched); it is created.
func WriteModelsJSON(path, shape string, models []Model) (backup string, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	var payload []byte
	if shape == "object" {
		b, err := json.Marshal(map[string]any{"models": models})
		if err != nil {
			return "", err
		}
		payload = b
	} else {
		b, err := json.MarshalIndent(models, "", "  ")
		if err != nil {
			return "", err
		}
		payload = b
	}
	payload = append(payload, '\n')

	if _, err := os.Stat(path); err == nil {
		backup = fmt.Sprintf("%s.bak-%d", path, time.Now().Unix())
		if err := copyFile(path, backup); err != nil {
			return "", fmt.Errorf("backup: %w", err)
		}
	}
	tmp := path + ".tmp-wbmodels"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return backup, err
	}
	return backup, os.Rename(tmp, path)
}

// Apply is the one-call entry both entry points use: fetch from the gateway,
// merge with the existing file, write. It returns a human-readable summary.
func Apply(panelURL, apiKey, path string, prune bool, timeout time.Duration) (string, error) {
	generated, err := Build(panelURL, apiKey, timeout)
	if err != nil {
		return "", err
	}
	return applyGenerated(generated, path, prune)
}

func applyGenerated(generated []Model, path string, prune bool) (string, error) {
	if len(generated) == 0 {
		return "", fmt.Errorf("网关当前没有可登记的模型（路由表为空或白名单过滤光了）")
	}
	existing, shape, err := ReadModelsJSON(path)
	if err != nil {
		return "", err
	}
	merged, added, updated, kept, removed, skipped := Merge(existing, generated, prune)
	if _, err := WriteModelsJSON(path, shape, merged); err != nil {
		return "", err
	}
	return fmt.Sprintf("网关模型 %d 个：新增 %d、刷新 %d、保留你自己的 %d 个（其中 %d 个同名由你的配置优先）、清理失效 %d；文件 %s",
		len(generated), added, updated, kept, skipped, removed, path), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func getJSON(c *http.Client, url string, v any) error {
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
