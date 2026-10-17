// Real Gemini thought_signature capture/replay (experimental, BP-44).
//
// Gemini 3 thinking models stamp functionCall parts with a thought_signature
// and REQUIRE it back when the tool-call history is replayed. Clients on the
// OpenAI/Anthropic protocols cannot carry it, so the default path replays the
// documented placeholder (skip_thought_signature_validator, BP-42) — accepted
// by the upstream but the model cannot resume its original thinking chain
// ("may lead to degraded model performance" per Google).
//
// This cache closes that gap: signatures seen in upstream responses are kept
// in memory keyed by (upstream model, tool-call id) — the id is one WE
// generate and hand to the client, which echoes it back as the OpenAI
// tool_call_id / Anthropic tool_use_id. When the experimental
// thought_signature_replay toggle is on, replay attaches the real signature
// instead of the placeholder.
package antigravity

import (
	"sort"
	"sync"
	"time"
)

const (
	// thoughtSigTTL bounds how long a captured signature stays replayable.
	thoughtSigTTL = 2 * time.Hour
	// thoughtSigMax caps the cache; when full, expired entries are pruned
	// first, then the oldest quarter is dropped.
	thoughtSigMax = 4096
)

type thoughtSigKey struct {
	// account is part of the key on purpose: sub2api's field experience
	// (gemini_v1beta_handler.go, 2026-10) is that Gemini thought_signature
	// values are strongly bound to the upstream ACCOUNT — replaying one
	// captured on account A through account B gets rejected with 400. Our
	// sticky sessions normally pin the account, but rotation must degrade to
	// a cache miss (placeholder) instead of a doomed replay.
	account string
	model   string
	callID  string
}

type thoughtSigEntry struct {
	name string
	sig  string
	at   time.Time
}

type thoughtSigCache struct {
	mu      sync.Mutex
	entries map[thoughtSigKey]thoughtSigEntry
}

func newThoughtSigCache() *thoughtSigCache {
	return &thoughtSigCache{entries: map[thoughtSigKey]thoughtSigEntry{}}
}

// put stores a captured signature. Empty values are ignored (some turns carry
// no signatures at all — e.g. non-thinking models).
func (c *thoughtSigCache) put(account, model, callID, name, sig string) {
	if c == nil || sig == "" || callID == "" {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= thoughtSigMax {
		c.pruneLocked(now)
	}
	c.entries[thoughtSigKey{account: account, model: model, callID: callID}] = thoughtSigEntry{name: name, sig: sig, at: now}
}

// get resolves a replayable signature. A name mismatch counts as a miss: the
// client must not be able to pull another call's signature by reusing its id.
func (c *thoughtSigCache) get(account, model, callID, name string) (string, bool) {
	if c == nil || callID == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := thoughtSigKey{account: account, model: model, callID: callID}
	e, ok := c.entries[k]
	if !ok {
		return "", false
	}
	if time.Since(e.at) > thoughtSigTTL {
		delete(c.entries, k)
		return "", false
	}
	if e.name != name {
		return "", false
	}
	return e.sig, true
}

// size reports the entry count (tests/diagnostics).
func (c *thoughtSigCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// pruneLocked drops expired entries; if the map is still at/over the cap it
// removes the oldest entries down to 75% capacity.
func (c *thoughtSigCache) pruneLocked(now time.Time) {
	for k, e := range c.entries {
		if now.Sub(e.at) > thoughtSigTTL {
			delete(c.entries, k)
		}
	}
	if len(c.entries) < thoughtSigMax {
		return
	}
	type kv struct {
		k thoughtSigKey
		t time.Time
	}
	all := make([]kv, 0, len(c.entries))
	for k, e := range c.entries {
		all = append(all, kv{k, e.at})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
	drop := len(c.entries) - thoughtSigMax*3/4
	for i := 0; i < drop && i < len(all); i++ {
		delete(c.entries, all[i].k)
	}
}
