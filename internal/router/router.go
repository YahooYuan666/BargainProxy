// Package router resolves a client-facing canonical model name to an ordered
// list of channel bindings. Bindings are explicit and curated — never the
// product of fuzzy string normalization (version identity matters).
//
// The table is hot-reloadable (Replace, panel PUT /panel/api/routes) and
// supports the debug escape hatch "channel:<name>:<model>" which pins the
// request to one channel without cross-channel fallback.
package router

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"bargainproxy/internal/config"
	"bargainproxy/internal/model"
)

// Binding is one resolved routing target.
type Binding struct {
	Channel       string // antigravity|workbuddy|trae|zcode|openaicompat
	UpstreamModel string // native model id at the channel
	Priority      int    // lower = preferred within same cost class
}

// Router holds the canonical model catalog.
type Router struct {
	mu sync.RWMutex
	// byModel maps canonical model -> ordered bindings.
	byModel map[string][]Binding
	// known lists canonical models in a stable order for /v1/models.
	known []string
	// fallback: canonical models the openai-compat channel serves when its
	// binding is absent from config (configured via openaicompat.models).
	compatModels map[string]bool
	enabled      map[string]bool // channel -> enabled
}

var channelNames = map[string]bool{
	model.ChannelAntigravity:  true,
	model.ChannelWorkBuddy:    true,
	model.ChannelTrae:         true,
	model.ChannelZCode:        true,
	model.ChannelAutoClaw:     true,
	model.ChannelQoder:        true,
	model.ChannelOpenAICompat: true,
	model.ChannelOpenCodeZen:  true,
	model.ChannelLobster:      true,
}

// ValidChannel reports whether name is a known channel identifier.
func ValidChannel(name string) bool { return channelNames[name] }

// New builds a router from configuration.
func New(cfg *config.Config) *Router {
	r := &Router{
		byModel:      map[string][]Binding{},
		compatModels: map[string]bool{},
		enabled: map[string]bool{
			model.ChannelAntigravity:  cfg.Channels.Antigravity.Enabled,
			model.ChannelWorkBuddy:    cfg.Channels.WorkBuddy.Enabled,
			model.ChannelTrae:         cfg.Channels.Trae.Enabled,
			model.ChannelZCode:        cfg.Channels.ZCode.Enabled,
			model.ChannelAutoClaw:     cfg.Channels.AutoClaw.Enabled,
			model.ChannelQoder:        cfg.Channels.Qoder.Enabled,
			model.ChannelOpenAICompat: cfg.Channels.OpenAICompat.Enabled,
			model.ChannelOpenCodeZen:  cfg.Channels.OpenCodeZen.Enabled,
			model.ChannelLobster:      cfg.Channels.Lobster.Enabled,
		},
	}
	for _, m := range cfg.Channels.OpenAICompat.Models {
		r.compatModels[config.NormalizeModel(m)] = true
	}
	r.install(cfg.Models)
	return r
}

// Replace hot-swaps the route table (panel edit). Validation is strict: an
// invalid row rejects the whole submission and the current table stays live.
func (r *Router) Replace(models []config.ModelBinding) error {
	if len(models) == 0 {
		return fmt.Errorf("router: route table must not be empty")
	}
	for _, m := range models {
		name := config.NormalizeModel(m.Canonical)
		if name == "" {
			return fmt.Errorf("router: model name required")
		}
		if !ValidChannel(m.Channel) {
			return fmt.Errorf("router: unknown channel %q for model %q", m.Channel, name)
		}
		if strings.TrimSpace(m.UpstreamModel) == "" {
			return fmt.Errorf("router: upstream_model required for model %q", name)
		}
	}
	r.install(models)
	return nil
}

// install rebuilds the resolved maps and swaps them under the lock.
func (r *Router) install(models []config.ModelBinding) {
	byModel := map[string][]Binding{}
	seen := map[string]bool{}
	var known []string
	for _, m := range models {
		name := config.NormalizeModel(m.Canonical)
		if name == "" || m.Channel == "" {
			continue
		}
		byModel[name] = append(byModel[name], Binding{
			Channel:       m.Channel,
			UpstreamModel: m.UpstreamModel,
			Priority:      m.Priority,
		})
		if !seen[name] {
			known = append(known, name)
			seen[name] = true
		}
	}
	for name, bindings := range byModel {
		sort.SliceStable(bindings, func(i, j int) bool { return bindings[i].Priority < bindings[j].Priority })
		byModel[name] = bindings
	}
	for name := range r.compatModels {
		if !seen[name] {
			known = append(known, name)
			seen[name] = true
		}
	}
	sort.Strings(known)

	r.mu.Lock()
	r.byModel = byModel
	r.known = known
	r.mu.Unlock()
}

// ParseChannelPrefix recognizes the debug escape hatch
// "channel:<name>:<model>". has=true means the input carried the prefix;
// channel is "" when the prefix was malformed or names an unknown channel —
// callers must treat that as an unresolvable model (404), never fall back to
// normal resolution.
func ParseChannelPrefix(s string) (bare string, channel string, has bool) {
	const p = "channel:"
	if !strings.HasPrefix(s, p) {
		return s, "", false
	}
	rest := s[len(p):]
	i := strings.Index(rest, ":")
	if i <= 0 || i == len(rest)-1 {
		return "", "", true
	}
	ch := rest[:i]
	if !ValidChannel(ch) {
		return "", "", true
	}
	return rest[i+1:], ch, true
}

// Resolve returns the ordered candidate bindings for a canonical model.
// Channels that are disabled or unknown are filtered out. An empty result
// means the model cannot be served.
func (r *Router) Resolve(canonical string) []Binding {
	name, forced, has := ParseChannelPrefix(canonical)
	if has && forced == "" {
		return nil // malformed / unknown-channel escape hatch: 404
	}
	r.mu.RLock()
	base := r.byModel[name]
	compat := r.enabled[model.ChannelOpenAICompat] && r.compatModels[name]
	enabled := r.enabled
	r.mu.RUnlock()

	out := make([]Binding, 0, 4)
	for _, b := range base {
		if enabled[b.Channel] {
			out = append(out, b)
		}
	}
	// openai-compat fallback for models it declares.
	if compat {
		already := false
		for _, b := range out {
			if b.Channel == model.ChannelOpenAICompat {
				already = true
				break
			}
		}
		if !already {
			out = append(out, Binding{Channel: model.ChannelOpenAICompat, UpstreamModel: name, Priority: 100})
		}
	}
	if has {
		// Escape hatch: only the forced channel, no cross-channel fallback.
		var filtered []Binding
		for _, b := range out {
			if b.Channel == forced {
				filtered = append(filtered, b)
			}
		}
		return filtered
	}
	return out
}

// KnownModels lists canonical models that have at least one enabled binding.
func (r *Router) KnownModels() []string {
	r.mu.RLock()
	known := append([]string(nil), r.known...)
	r.mu.RUnlock()
	out := make([]string, 0, len(known))
	for _, name := range known {
		if len(r.Resolve(name)) > 0 {
			out = append(out, name)
		}
	}
	return out
}

// ChannelFor reports whether a channel name is valid and enabled.
func (r *Router) ChannelFor(ch string) error {
	r.mu.RLock()
	enabled, known := r.enabled[ch], channelNames[ch]
	r.mu.RUnlock()
	if !known {
		return fmt.Errorf("router: unknown channel %q", ch)
	}
	if !enabled {
		return fmt.Errorf("router: channel %q disabled", ch)
	}
	return nil
}

// Describe renders the model catalog as plain rows (panel/debug).
func (r *Router) Describe() []string {
	var out []string
	for _, name := range r.KnownModels() {
		var parts []string
		for _, b := range r.Resolve(name) {
			parts = append(parts, fmt.Sprintf("%s:%s", b.Channel, b.UpstreamModel))
		}
		out = append(out, name+" -> "+strings.Join(parts, ", "))
	}
	return out
}

// Row is one editable route-table entry (panel GET /routes).
type Row struct {
	Model         string `json:"model"`
	Channel       string `json:"channel"`
	UpstreamModel string `json:"upstream_model"`
	Priority      int    `json:"priority"`
}

// Rows returns the configured table (not the resolved view): one row per
// binding, models in stable order.
func (r *Router) Rows() []Row {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Row, 0, 8)
	for _, name := range r.known {
		for _, b := range r.byModel[name] {
			out = append(out, Row{Model: name, Channel: b.Channel, UpstreamModel: b.UpstreamModel, Priority: b.Priority})
		}
	}
	return out
}
