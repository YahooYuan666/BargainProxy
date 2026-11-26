package antigravity

// Per-account model catalog + enum probe (BP-340), following CLIProxyAPI's
// antigravity model discovery (sdk/auth/antigravity_models.go,
// internal/runtime/executor/helps/antigravity_agent_envelope.go).
//
// WHY THIS EXISTS — measured 2026-11-25: after the BP-338 User-Agent fix the
// upstream still answered a bare 404 NOT_FOUND for every gemini-4-* id on a
// healthy account (claude-* answered 200 in the same second). The gate is not
// the UA alone: the request must carry the account's OWN model enum, which
// Google hands out per credential through
//
//	POST /v1internal:fetchAvailableModels   → {"models": {"<id>": {"model": "<enum>"}}}
//
// Measured response on a live Pro account (2026-11-25):
//
//	gemini-4-hydrogen-high → MODEL_PLACEHOLDER_M525
//	gemini-4-hydrogen-low  → MODEL_PLACEHOLDER_M527
//	gemini-3.1-flash-image → MODEL_PLACEHOLDER_M21
//
// and a chat request carrying that enum in request.labels.model_enum plus the
// agent-shaped requestId answered 200 with real text. Without it: 404.
//
// The envelope also needs the agent correlation ids CLIProxyAPI builds
// (cascade/trajectory UUIDs, request_id "<trajectory>-<n>", last_step_index,
// used_claude/used_non_gemini_model flags). Those are cosmetic-looking but the
// upstream validates the shape; a bare hex requestId is what it rejected.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"bargainproxy/internal/model"
)

// modelCatalogs caches one account's model→enum map.
type modelCatalogs struct {
	mu      sync.Mutex
	entries map[string]*modelCatalogEntry // account ID -> entry
}

type modelCatalogEntry struct {
	enums   map[string]string
	fetched time.Time
	err     error
}

const (
	modelCatalogTTL      = 6 * time.Hour
	modelCatalogPath     = "/v1internal:fetchAvailableModels"
	modelCatalogMaxBytes = 8 << 20
)

func (d *Driver) catalogs() *modelCatalogs {
	d.catalogOnce.Do(func() {
		d.catalog = &modelCatalogs{entries: map[string]*modelCatalogEntry{}}
	})
	return d.catalog
}

// resolveCatalogID maps a gateway model id onto the id the account's catalog
// actually lists. The catalog has NO bare "gemini-4-hydrogen": the band is
// published per thinking level (…-low/-medium/-high/-xhigh), so a request for
// the bare id must borrow the default level's entry — otherwise the enum lookup
// misses and the upstream answers 404 (measured 2026-11-25 18:47: the body had
// a correct agent requestId but an empty model_enum).
func resolveCatalogID(enums map[string]string, upstreamModel string) (string, bool) {
	if _, ok := enums[upstreamModel]; ok {
		return upstreamModel, true
	}
	for _, suffix := range []string{"-high", "-medium", "-low", "-xhigh"} {
		if _, ok := enums[upstreamModel+suffix]; ok {
			return upstreamModel + suffix, true
		}
	}
	return upstreamModel, false
}

// modelEnumFor returns the upstream enum string for one model id, fetching the
// account catalog on demand. An empty string means "unknown" — the caller then
// omits the label rather than sending a wrong one.
func (d *Driver) modelEnumFor(ctx context.Context, acc *model.Account, token, upstreamModel string) string {
	c := d.catalogs()
	c.mu.Lock()
	entry := c.entries[acc.ID]
	if entry != nil && time.Since(entry.fetched) < modelCatalogTTL && entry.err == nil {
		id, _ := resolveCatalogID(entry.enums, upstreamModel)
		enum := entry.enums[id]
		c.mu.Unlock()
		return enum
	}
	c.mu.Unlock()

	enums, err := d.fetchModelCatalog(ctx, acc, token)
	c.mu.Lock()
	c.entries[acc.ID] = &modelCatalogEntry{enums: enums, fetched: time.Now(), err: err}
	c.mu.Unlock()
	if err != nil {
		return ""
	}
	id, _ := resolveCatalogID(enums, upstreamModel)
	return enums[id]
}

// fetchModelCatalog POSTs fetchAvailableModels for one credential.
func (d *Driver) fetchModelCatalog(ctx context.Context, acc *model.Account, token string) (map[string]string, error) {
	project := acc.ProjectID
	if project == "" {
		project = "aicode-consumers"
	}
	body, _ := json.Marshal(map[string]string{"project": project})

	hosts := []string{d.cfg.DailyURL, d.cfg.BaseURL}
	var lastErr error
	for _, host := range hosts {
		if host == "" {
			continue
		}
		reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
			strings.TrimRight(host, "/")+modelCatalogPath, bytes.NewReader(body))
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("User-Agent", uaForModel("gemini-4-hydrogen")) // hub family
		resp, err := d.http.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, modelCatalogMaxBytes))
		resp.Body.Close()
		cancel()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("catalog: status %d", resp.StatusCode)
			continue
		}
		var doc struct {
			Models map[string]struct {
				Model string `json:"model"`
			} `json:"models"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			lastErr = fmt.Errorf("catalog: %w", err)
			continue
		}
		out := make(map[string]string, len(doc.Models))
		for id, m := range doc.Models {
			if strings.TrimSpace(m.Model) != "" {
				out[id] = m.Model
			}
		}
		if len(out) == 0 {
			lastErr = fmt.Errorf("catalog: empty model map")
			continue
		}
		return out, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("catalog: no host configured")
	}
	return nil, lastErr
}

// agentIDs are the correlation identifiers the agent envelope carries.
type agentIDs struct {
	cascade    string
	trajectory string
	step       int
}

// newAgentIDs mints a fresh cascade/trajectory pair (UUIDv4-shaped, which is
// what the upstream accepted in the live probe).
func newAgentIDs(step int) agentIDs {
	return agentIDs{cascade: randUUIDv4(), trajectory: randUUIDv4(), step: step}
}

// requestID renders the agent requestId shape "<kind>/<cascade>/<stamp>/<trajectory>/<step>".
func (a agentIDs) requestID() string {
	return fmt.Sprintf("agent/%s/%d/%s/%d", a.cascade, time.Now().UnixMilli(), a.trajectory, a.step)
}

// labels renders the request.labels block. modelEnum may be empty, in which
// case the label is omitted (never send a guess).
func (a agentIDs) labels(model, modelEnum string) map[string]string {
	lower := strings.ToLower(model)
	out := map[string]string{
		"cascade_id":                a.cascade,
		"root_cascade_id":           a.cascade,
		"trajectory_id":             a.trajectory,
		"request_id":                fmt.Sprintf("%s-%d", a.trajectory, a.step),
		"last_step_index":           fmt.Sprintf("%d", a.step),
		"used_claude":               fmt.Sprintf("%t", strings.Contains(lower, "claude")),
		"used_claude_conservative":  "false",
		"used_non_gemini_model":     fmt.Sprintf("%t", !strings.Contains(lower, "gemini")),
	}
	if modelEnum != "" {
		out["model_enum"] = modelEnum
	}
	return out
}

// randUUIDv4 returns a random RFC-4122 v4 UUID string.
func randUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}


// requestEnvelope carries the per-request correlation ids and labels that the
// agent protocol validates (BP-340), plus the catalog-resolved upstream model
// id: the Gemini 4 band publishes ONLY leveled ids (…-high/-medium/-low/-xhigh),
// so a bare id in the body is a 404 no matter what the labels say (measured
// 2026-11-25 18:52: bare+enum 404, "-high"+enum 200).
type requestEnvelope struct {
	RequestID   string
	Labels      map[string]string
	UpstreamID  string // non-empty = send this instead of the routed model id
}

// envelopeFor builds the envelope for one chat turn: fresh cascade/trajectory
// ids plus the account's model enum when the catalog probe knows it.
//
// The catalog probe only runs for the Gemini 4 band — that band is the only one
// that gates on the enum (measured 2026-11-25), and probing for every 3.x or
// claude turn would add an upstream round trip to requests that never needed it
// (and would show up as an extra request in every chat-path test).
func (d *Driver) envelopeFor(ctx context.Context, acc *model.Account, token, upstreamModel string) *requestEnvelope {
	ids := newAgentIDs(1)
	env := &requestEnvelope{RequestID: ids.requestID()}
	if strings.HasPrefix(strings.ToLower(upstreamModelName(upstreamModel)), "gemini-4-") {
		enum, id := d.modelEnumAndID(ctx, acc, token, upstreamModel)
		env.UpstreamID = id
		env.Labels = ids.labels(upstreamModel, enum)
		return env
	}
	env.Labels = ids.labels(upstreamModel, "")
	return env
}

// modelEnumAndID returns the enum AND the catalog id to send upstream for one
// routed model. Both are needed: the upstream validates the id (404 otherwise)
// and uses the enum for the credential-scoped entitlement.
func (d *Driver) modelEnumAndID(ctx context.Context, acc *model.Account, token, upstreamModel string) (enum, id string) {
	enums := d.catalogEnums(ctx, acc, token)
	resolved, ok := resolveCatalogID(enums, upstreamModel)
	if !ok {
		return "", ""
	}
	return enums[resolved], resolved
}

// catalogEnums returns the account catalog (cached, best effort).
func (d *Driver) catalogEnums(ctx context.Context, acc *model.Account, token string) map[string]string {
	c := d.catalogs()
	c.mu.Lock()
	entry := c.entries[acc.ID]
	if entry != nil && time.Since(entry.fetched) < modelCatalogTTL && entry.err == nil {
		enums := entry.enums
		c.mu.Unlock()
		return enums
	}
	c.mu.Unlock()
	enums, err := d.fetchModelCatalog(ctx, acc, token)
	c.mu.Lock()
	c.entries[acc.ID] = &modelCatalogEntry{enums: enums, fetched: time.Now(), err: err}
	c.mu.Unlock()
	return enums
}
