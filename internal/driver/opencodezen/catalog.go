// Directory: the Zen live model list ∩ models.dev free-and-not-deprecated
// metadata. Both sources drift (live 2026-10-16: 88 live entries, 12 free
// metadata entries, 40 deprecated of which 25+ were once-free *-free ids) —
// the intersection is the candidate set, and only a live probe can say
// "actually usable right now". Persistence goes through the panel's settings
// store under one JSON key so restarts keep the last good snapshot.
package opencodezen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"bargainproxy/internal/store"
)

// errNotModified marks a 304 from either source: "nothing new", which is NOT a
// failure — the last decoded data stays authoritative. Treating it as
// unavailable was a real bug: the candidate gate fell back to the name
// heuristic (flipping the candidate set) and the metadata age got blanked;
// the same mistake on the live list would have denied every free model as
// "not_in_live_list" and emptied the channel.
var errNotModified = errors.New("not modified (304)")

var metadataURL = "https://models.dev/api.json"

// Body caps — vars so tests can shrink them. Bodies are read whole and a short
// read is an explicit error: the old shared 4 MiB cap silently truncated
// models.dev's api.json once it grew past that (5.75 MB by 2026-10), so the
// JSON parse failed and every model degraded to "metadata_pending" with no
// visible error anywhere.
var (
	liveLimit     int64 = 2 << 20
	metadataLimit int64 = 16 << 20
)

const (
	zenModelsPath = "/models"
	dirSettingKey = "opencodezen_catalog"
	directoryTTL  = 30 * time.Minute
)

// readCapped reads at most limit bytes and turns a truncation into an explicit
// error instead of letting a partial body fail later as a parse error.
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("body exceeds the %d-byte cap", limit)
	}
	return raw, nil
}

// ModelEntry is one catalog row as the panel shows it.
type ModelEntry struct {
	ID            string   `json:"id"`
	Candidate     bool     `json:"candidate"`        // passed the free∩live∩not-deprecated gate
	Denied        string   `json:"denied,omitempty"` // deny reason when not a candidate
	Deprecated    bool     `json:"deprecated,omitempty"`
	InLiveList    bool     `json:"in_live_list"`
	ContextWindow int      `json:"context_window,omitempty"`
	MaxOutput     int      `json:"max_output_tokens,omitempty"`
	InputImage    bool     `json:"input_image,omitempty"`
	ReasoningVals []string `json:"reasoning_vals,omitempty"`
	// Probe state (filled by the probe task; empty = never probed).
	ProbeStatus string    `json:"probe_status,omitempty"` // ok | limited | fail
	ProbeError  string    `json:"probe_error,omitempty"`
	ProbeMs     int64     `json:"probe_ms,omitempty"`
	ProbedAt    time.Time `json:"probed_at,omitempty"`
}

// Snapshot is the full catalog state (persisted as one settings blob).
type Snapshot struct {
	UpdatedAt     time.Time    `json:"updated_at"`
	LiveCount     int          `json:"live_count"`
	MetadataAge   time.Time    `json:"metadata_age,omitempty"`
	LastSyncError string       `json:"last_sync_error,omitempty"`
	MetadataError string       `json:"metadata_error,omitempty"` // last metadata-only failure, kept visible
	Models        []ModelEntry `json:"models"`
}

// Catalog keeps the in-memory snapshot and the HTTP caches.
type Catalog struct {
	mu         sync.Mutex
	hc         *http.Client
	st         store.Store
	base       string
	snap       Snapshot
	etagMeta   string
	etagLive   string
	metaDetail map[string]metaDetail
	prices     map[string]metaPrice // last decoded models.dev gate data (survives 304 + restarts)
	liveList   []string             // last live model list (survives 304 + restarts)
}

// NewCatalog builds the catalog. st may be nil (tests): no persistence.
func NewCatalog(hc *http.Client, st store.Store, baseURL string) *Catalog {
	base := strings.TrimRight(baseURL, "/")
	// Same legacy mapping as Driver.base: stale configs name the pre-BP-127
	// /zen default the free lane no longer answers on.
	if base == "" || strings.HasSuffix(base, "opencode.ai/zen") || strings.HasSuffix(base, "opencode.ai/zen/v1") {
		base = zenUpstreamBase
	}
	c := &Catalog{hc: hc, st: st, base: base}
	c.loadPersisted()
	return c
}

// --- models.dev metadata decoding ---

// metaPrice is the subset of models.dev's model metadata we surface.
type metaPrice struct {
	Input     *float64 `json:"input"`
	Deprecate bool     `json:"deprecated"`
}

func decodeModelsDev(data []byte) map[string]metaPrice {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}
	// The opencode section may live at root or under providers.
	section := doc["opencode"]
	if section == nil {
		var providers struct {
			Providers map[string]json.RawMessage `json:"providers"`
		}
		if json.Unmarshal(data, &providers) == nil {
			section = providers.Providers["opencode"]
		}
	}
	if section == nil {
		return nil
	}
	var provider struct {
		Models map[string]struct {
			Cost struct {
				Input *float64 `json:"input"`
			} `json:"cost"`
			Status string `json:"status"`
			Limit  struct {
				Context *int `json:"context"`
				Output  *int `json:"output"`
			} `json:"limit"`
			Modalities json.RawMessage `json:"modalities"`
			Reasoning  json.RawMessage `json:"reasoning_options"`
		} `json:"models"`
	}
	if err := json.Unmarshal(section, &provider); err != nil {
		return nil
	}
	out := map[string]metaPrice{}
	for id, m := range provider.Models {
		out[id] = metaPrice{Input: m.Cost.Input, Deprecate: m.Status == "deprecated"}
	}
	return out
}

// reasoningValues flattens models.dev's reasoning_options into a display list
// (drops off/none — the upstream rejects them on the free lane).
//
// Real shape as of 2026-10: [{"type":"effort","values":["low","medium",…]}].
// The older {"value"/"effort"} shapes stay tolerated so a schema drift on
// either side does not silently blank the column.
func reasoningValues(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var arr []struct {
		Value    string `json:"value"`
		Effort   string `json:"effort"`
		Disabled bool   `json:"disabled"`
	}
	var single struct {
		Value  string `json:"value"`
		Effort string `json:"effort"`
	}
	seen := map[string]bool{}
	add := func(v string) {
		if v == "" || v == "off" || v == "none" || seen[v] {
			return
		}
		seen[v] = true
	}
	// shape 1: [{"type":"effort","values":[…]}] — the live schema
	var typed []struct {
		Type   string   `json:"type"`
		Values []string `json:"values"`
	}
	if json.Unmarshal(raw, &typed) == nil && len(typed) > 0 {
		for _, o := range typed {
			if o.Type != "" && o.Type != "effort" {
				continue
			}
			for _, v := range o.Values {
				add(v)
			}
		}
		if len(seen) > 0 {
			return sortedKeys(seen)
		}
	}
	if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
		for _, o := range arr {
			if o.Disabled {
				continue
			}
			if o.Value != "" {
				add(o.Value)
			} else {
				add(o.Effort)
			}
		}
		return sortedKeys(seen)
	}
	if json.Unmarshal(raw, &single) == nil && (single.Value != "" || single.Effort != "") {
		if single.Value != "" {
			add(single.Value)
		} else {
			add(single.Effort)
		}
		return sortedKeys(seen)
	}
	return nil
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- sync ---

// Sync refreshes both sources and rebuilds the snapshot. Either source may
// fail independently (the persisted last-good state fills the gap); a total
// failure keeps the previous snapshot and records the error.
func (c *Catalog) Sync(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var firstErr error
	live, etag, err := c.fetchLive(ctx, c.etagLive)
	if errors.Is(err, errNotModified) && c.liveList == nil {
		// 304 with nothing cached to reuse (state persisted by an older build):
		// refetch unconditionally once instead of degrading.
		live, etag, err = c.fetchLive(ctx, "")
	}
	switch {
	case errors.Is(err, errNotModified):
		live = c.liveList // unchanged: keep the last known live list
	case err != nil:
		firstErr = err
	default:
		c.liveList = live
		c.etagLive = etag
	}
	meta, metagEtag, err2 := c.fetchMetadata(ctx)
	if errors.Is(err2, errNotModified) && c.prices == nil {
		// 304 but nothing decoded to reuse (state persisted by an older build):
		// drop the stored validator and refetch unconditionally once.
		c.etagMeta = ""
		meta, metagEtag, err2 = c.fetchMetadata(ctx)
	}
	metaErr := ""
	switch {
	case errors.Is(err2, errNotModified):
		meta = c.prices // unchanged: the last decoded gate data stays authoritative
	case err2 != nil:
		metaErr = err2.Error()
		if firstErr == nil {
			firstErr = err2
		}
	default:
		c.prices = meta
		c.etagMeta = metagEtag
	}

	if live == nil && meta == nil {
		if firstErr != nil {
			c.snap.LastSyncError = firstErr.Error()
		}
		return firstErr
	}

	prev := map[string]ModelEntry{}
	for _, m := range c.snap.Models {
		prev[m.ID] = m
	}
	snap := Snapshot{UpdatedAt: time.Now(), Models: []ModelEntry{}}
	if live != nil {
		snap.LiveCount = len(live)
	}
	// Metadata freshness: a 304 keeps the previous age, and so does a real
	// failure (staleness is then visible through MetadataError instead).
	snap.MetadataAge = c.snap.MetadataAge
	if metaErr != "" {
		snap.MetadataError = metaErr
	} else if meta != nil && !errors.Is(err2, errNotModified) {
		snap.MetadataAge = time.Now()
	}
	seen := map[string]bool{}
	addModel := func(id string, inLive bool) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		entry := ModelEntry{ID: id, InLiveList: inLive}
		if m, ok := prev[id]; ok {
			// Carry probe state and context forward.
			entry.ProbeStatus, entry.ProbeError, entry.ProbeMs, entry.ProbedAt =
				m.ProbeStatus, m.ProbeError, m.ProbeMs, m.ProbedAt
			entry.ContextWindow, entry.MaxOutput, entry.InputImage, entry.ReasoningVals =
				m.ContextWindow, m.MaxOutput, m.InputImage, m.ReasoningVals
		}
		// Decision (opencode2zcode catalog.rs parity): candidate = live ∩
		// metadata-free-and-not-deprecated. Name-free fallback only when the
		// whole metadata set is unavailable.
		if meta == nil {
			if strings.Contains(id, "free") {
				entry.Candidate = true
				entry.Denied = ""
			} else {
				entry.Candidate = false
				entry.Denied = "metadata_pending"
			}
		} else if mp, ok := meta[id]; ok {
			if mp.Deprecate {
				entry.Deprecated = true
				entry.Candidate = false
				entry.Denied = "metadata_deprecated"
			} else if mp.Input == nil || *mp.Input == 0 {
				// Free per metadata, but absent from the live list = likely
				// delisted upstream; not a candidate (live∩free discipline).
				if !inLive {
					entry.Candidate = false
					entry.Denied = "not_in_live_list"
				} else {
					entry.Candidate = true
				}
			} else {
				entry.Candidate = false
				entry.Denied = "metadata_paid"
			}
		} else {
			entry.Candidate = false
			entry.Denied = "metadata_model_missing"
		}
		// Fresh metadata display fields override the carried values.
		if cm, ok := c.metaDetail[id]; ok {
			entry.ContextWindow, entry.MaxOutput, entry.InputImage, entry.ReasoningVals =
				cm.Context, cm.MaxOut, cm.Image, cm.Reasoning
		}
		snap.Models = append(snap.Models, entry)
	}
	for _, id := range live {
		addModel(id, true)
	}
	for id := range meta {
		addModel(id, false)
	}
	sort.Slice(snap.Models, func(i, j int) bool { return snap.Models[i].ID < snap.Models[j].ID })
	c.snap = snap
	c.persist()
	return firstErr
}

// Upstream lanes a model can be served on. wireUnsupported marks models whose
// declared SDK no lane of ours can speak (e.g. jev's systemone).
const (
	wireChat        = "chat"
	wireResponses   = "responses"
	wireUnsupported = "unsupported"
)

// decodeWire maps models.dev's declared SDK onto our upstream lane
// (opencode2zcode catalog.rs parity): @ai-sdk/openai → responses,
// openai-compatible → chat, anything else (anthropic, systemone, …) → none.
func decodeWire(npm string) string {
	switch {
	case npm == "":
		return ""
	case npm == "@ai-sdk/openai" || strings.HasSuffix(npm, "/openai"):
		return wireResponses
	case npm == "openai-compatible" || npm == "@ai-sdk/openai-compatible":
		return wireChat
	default:
		return wireUnsupported
	}
}

// metaDetail carries the decoded context/modality/reasoning fields per id.
// Fields are exported so the map survives JSON persistence — with unexported
// fields every persisted entry round-tripped as an empty object, which kept
// the four columns blank even when the metadata itself was fresh.
type metaDetail struct {
	Context   int      `json:"context,omitempty"`
	MaxOut    int      `json:"max_out,omitempty"`
	Image     bool     `json:"image,omitempty"`
	Reasoning []string `json:"reasoning,omitempty"`
	Wire      string   `json:"wire,omitempty"` // chat | responses | unsupported
}

// Wire reports which upstream lane serves a model: "chat", "responses", or ""
// when no lane of ours can serve it. The metadata's declared SDK wins; the
// name heuristic is the fallback (reference parity: muse-spark* → responses,
// jev* → unsupported, everything else → chat).
func (c *Catalog) Wire(id string) string {
	c.mu.Lock()
	d, ok := c.metaDetail[id]
	c.mu.Unlock()
	if ok && d.Wire != "" {
		if d.Wire == wireUnsupported {
			return ""
		}
		return d.Wire
	}
	lower := strings.ToLower(id)
	if lower == "" || strings.HasPrefix(lower, "jev") {
		return ""
	}
	if strings.HasPrefix(lower, "muse-spark") {
		return wireResponses
	}
	return wireChat
}

// fetchLive pulls the Zen live list with ETag caching. Returns nil models on
// 304/failure (caller keeps the previous state).
func (c *Catalog) fetchLive(ctx context.Context, etag string) ([]string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+zenModelsPath, nil)
	if err != nil {
		return nil, "", err
	}
	for _, h := range disguiseHeaders(deriveRequestIDs([]byte(`{"messages":[]}`)), "chat") {
		req.Header.Set(h[0], h[1])
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, etag, errNotModified
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", fmt.Errorf("zen /v1/models HTTP %d: %s", resp.StatusCode, truncate(body, 200))
	}
	raw, err := readCapped(resp.Body, liveLimit)
	if err != nil {
		return nil, "", err
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, "", fmt.Errorf("zen models body: %w", err)
	}
	out := make([]string, 0, len(doc.Data))
	for _, m := range doc.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, resp.Header.Get("ETag"), nil
}

// fetchMetadata pulls models.dev with ETag caching. Returns nil on 304 or
// failure; on success it also fills c.metaDetail.
func (c *Catalog) fetchMetadata(ctx context.Context) (map[string]metaPrice, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return nil, "", err
	}
	if c.etagMeta != "" {
		req.Header.Set("If-None-Match", c.etagMeta)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, c.etagMeta, errNotModified
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("models.dev HTTP %d", resp.StatusCode)
	}
	raw, err := readCapped(resp.Body, metadataLimit)
	if err != nil {
		return nil, "", err
	}
	prices := decodeModelsDev(raw)
	if len(prices) == 0 {
		return nil, "", fmt.Errorf("models.dev contains no opencode metadata")
	}
	// Second pass for the display fields (kept separate to keep decodeModelsDev
	// trivially testable).
	var section struct {
		NPM    string `json:"npm"` // provider-level SDK default
		Models map[string]struct {
			Limit struct {
				Context *int `json:"context"`
				Output  *int `json:"output"`
			} `json:"limit"`
			Modalities struct {
				Input []string `json:"input"`
			} `json:"modalities"`
			ReasoningEnabled bool            `json:"reasoning"`
			Reasoning        json.RawMessage `json:"reasoning_options"`
			Provider         struct {
				NPM string `json:"npm"`
			} `json:"provider"`
		} `json:"models"`
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) == nil {
		if sec, ok := doc["opencode"]; ok && json.Unmarshal(sec, &section) == nil {
			detail := map[string]metaDetail{}
			for id, m := range section.Models {
				d := metaDetail{}
				if m.ReasoningEnabled {
					// Options only count when the model declares reasoning at
					// all (reference parity).
					d.Reasoning = reasoningValues(m.Reasoning)
				}
				if m.Limit.Context != nil {
					d.Context = *m.Limit.Context
				}
				if m.Limit.Output != nil {
					d.MaxOut = *m.Limit.Output
				}
				for _, mod := range m.Modalities.Input {
					if mod == "image" {
						d.Image = true
					}
				}
				npm := m.Provider.NPM
				if npm == "" {
					npm = section.NPM
				}
				d.Wire = decodeWire(npm)
				detail[id] = d
			}
			c.metaDetail = detail
		}
	}
	return prices, resp.Header.Get("ETag"), nil
}

// Snapshot returns a copy of the current catalog state.
func (c *Catalog) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.snap
	out.Models = append([]ModelEntry(nil), c.snap.Models...)
	return out
}

// Candidates lists the candidate (possibly still probe-pending) model ids.
func (c *Catalog) Candidates() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, m := range c.snap.Models {
		if m.Candidate {
			out = append(out, m.ID)
		}
	}
	return out
}

// Probe verdicts. "limited" exists because a probe failure often says nothing
// about the model: the anonymous free lane answers 429 FreeUsageLimitError
// whenever its shared quota is spent, and timeouts/cancellations are equally
// untranslatable. Recording those as "fail" used to delete a working model
// from the gateway routes and the ZCode picker until the next probe.
const (
	ProbeOK      = "ok"
	ProbeLimited = "limited" // transient upstream condition; last verdict kept
	ProbeFail    = "fail"    // definitive: this model cannot serve requests
)

// SeedForTest installs a snapshot without going through Sync. Test-only.
func (c *Catalog) SeedForTest(models []ModelEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snap = Snapshot{UpdatedAt: time.Now(), Models: models}
}

// RecordProbe folds one probe outcome back into the snapshot and persists.
func (c *Catalog) RecordProbe(id string, ok bool, ms int64, errMsg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.snap.Models {
		if c.snap.Models[i].ID != id {
			continue
		}
		if ok {
			c.snap.Models[i].ProbeStatus = ProbeOK
			c.snap.Models[i].ProbeError = ""
		} else {
			c.snap.Models[i].ProbeStatus = ProbeFail
			c.snap.Models[i].ProbeError = errMsg
		}
		c.snap.Models[i].ProbeMs = ms
		c.snap.Models[i].ProbedAt = time.Now()
		break
	}
	c.persist()
}

// RecordProbeTransient records a probe that failed for a reason outside the
// model: free-lane quota (429), timeout, cancellation, transport blip, or the
// network-dependent free-tier gate (403). Only the diagnostic is refreshed —
// the verdict stays usable, so a temporary upstream condition cannot pull the
// route.
//
// A model whose last probe failed definitively (404 delisted, protocol
// unsupported) stays failed. The one exception is a stored gate-403 failure:
// that verdict was itself a transient condition misclassified before this
// split existed, so a later transient probe clears it instead of leaving nine
// healthy models reading "failed" forever.
func (c *Catalog) RecordProbeTransient(id string, ms int64, errMsg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.snap.Models {
		if c.snap.Models[i].ID != id {
			continue
		}
		if c.snap.Models[i].ProbeStatus == ProbeFail &&
			!strings.Contains(c.snap.Models[i].ProbeError, "free-tier gate") {
			// Keep the definitive reason on screen; only record that we tried
			// again. A 429 must not overwrite "404 model endpoint gone".
			c.snap.Models[i].ProbeMs = ms
			c.snap.Models[i].ProbedAt = time.Now()
			break
		}
		c.snap.Models[i].ProbeStatus = ProbeLimited
		c.snap.Models[i].ProbeError = errMsg
		c.snap.Models[i].ProbeMs = ms
		c.snap.Models[i].ProbedAt = time.Now()
		break
	}
	c.persist()
}

// --- persistence (one settings blob; best-effort like creds) ---

type persistedCatalog struct {
	Snapshot
	MetaETag   string                `json:"meta_etag,omitempty"`
	LiveETag   string                `json:"live_etag,omitempty"`
	MetaDetail map[string]metaDetail `json:"meta_detail,omitempty"`
	MetaPrices map[string]metaPrice  `json:"meta_prices,omitempty"`
	LiveList   []string              `json:"live_list,omitempty"`
}

func (c *Catalog) persist() {
	if c.st == nil {
		return
	}
	blob := persistedCatalog{
		Snapshot:   c.snap,
		MetaETag:   c.etagMeta,
		LiveETag:   c.etagLive,
		MetaDetail: c.metaDetail,
		MetaPrices: c.prices,
		LiveList:   c.liveList,
	}
	b, err := json.Marshal(blob)
	if err != nil {
		return
	}
	_ = c.st.SetSetting(dirSettingKey, string(b))
}

func (c *Catalog) loadPersisted() {
	if c.st == nil {
		return
	}
	v, ok, _ := c.st.GetSetting(dirSettingKey)
	if !ok || v == "" {
		return
	}
	var blob persistedCatalog
	if json.Unmarshal([]byte(v), &blob) != nil {
		return
	}
	c.snap = blob.Snapshot
	c.etagMeta = blob.MetaETag
	c.etagLive = blob.LiveETag
	if blob.MetaDetail != nil {
		c.metaDetail = blob.MetaDetail
	}
	if blob.MetaPrices != nil {
		c.prices = blob.MetaPrices
	}
	if blob.LiveList != nil {
		c.liveList = blob.LiveList
	}
}
