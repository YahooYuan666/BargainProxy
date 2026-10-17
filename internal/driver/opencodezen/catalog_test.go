package opencodezen

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCatalogSyncDecision(t *testing.T) {
	var mu sync.Mutex
	metaCalls := 0
	liveSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"live-1"`)
		fmt.Fprint(w, `{"data":[{"id":"glm-5.3-flash-free"},{"id":"mimo-v2.5-free"},{"id":"claude-opus-5"},{"id":"brand-new-free"}]}`)
	}))
	defer liveSrv.Close()
	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		metaCalls++
		mu.Unlock()
		w.Header().Set("ETag", `"meta-1"`)
		fmt.Fprint(w, `{"opencode":{"models":{
			"glm-5.3-flash-free": {"cost":{"input":0},"limit":{"context":200000,"output":32000},"modalities":{"input":["text","image"]}},
			"mimo-v2.5-free":     {"cost":{"input":0},"status":"deprecated"},
			"claude-opus-5":      {"cost":{"input":1.2}},
			"gone-free":          {"cost":{"input":0}}
		}}}`)
	}))
	defer metaSrv.Close()

	c := NewCatalog(liveSrv.Client(), nil, liveSrv.URL)
	// Point metadata at the test server via the package var.
	oldMeta := metadataURL
	metadataURL = metaSrv.URL
	t.Cleanup(func() { metadataURL = oldMeta })

	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	snap := c.Snapshot()
	byID := map[string]ModelEntry{}
	for _, m := range snap.Models {
		byID[m.ID] = m
	}
	if !byID["glm-5.3-flash-free"].Candidate {
		t.Fatalf("free∩live must be candidate: %+v", byID["glm-5.3-flash-free"])
	}
	if byID["glm-5.3-flash-free"].ContextWindow != 200000 || !byID["glm-5.3-flash-free"].InputImage {
		t.Fatalf("metadata detail lost: %+v", byID["glm-5.3-flash-free"])
	}
	if byID["mimo-v2.5-free"].Candidate || byID["mimo-v2.5-free"].Denied != "metadata_deprecated" {
		t.Fatalf("deprecated must be denied: %+v", byID["mimo-v2.5-free"])
	}
	if byID["claude-opus-5"].Candidate || byID["claude-opus-5"].Denied != "metadata_paid" {
		t.Fatalf("paid must be denied: %+v", byID["claude-opus-5"])
	}
	if byID["gone-free"].Candidate || byID["gone-free"].InLiveList {
		t.Fatalf("metadata-only id must be non-candidate, not in live list: %+v", byID["gone-free"])
	}
	if !byID["brand-new-free"].Candidate {
		// name-free fallback only when metadata is missing; metadata exists and
		// lacks the id → denied.
		if byID["brand-new-free"].Denied != "metadata_model_missing" {
			t.Fatalf("unexpected deny reason: %+v", byID["brand-new-free"])
		}
	}
	if len(c.Candidates()) != 1 || c.Candidates()[0] != "glm-5.3-flash-free" {
		t.Fatalf("candidates = %v", c.Candidates())
	}

	// Probe state recorded and carried forward across a re-sync.
	c.RecordProbe("glm-5.3-flash-free", true, 1234, "")
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("resync: %v", err)
	}
	var probed *ModelEntry
	snap2 := c.Snapshot()
	for i := range snap2.Models {
		if snap2.Models[i].ID == "glm-5.3-flash-free" {
			probed = &snap2.Models[i]
		}
	}
	if probed == nil || probed.ProbeStatus != "ok" || probed.ProbeMs != 1234 {
		t.Fatalf("probe state lost after resync: %+v", snap2.Models)
	}
}

func TestCatalogLastGoodOnOutage(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer dead.Close()
	c := NewCatalog(dead.Client(), nil, dead.URL)
	oldMeta := metadataURL
	metadataURL = dead.URL
	t.Cleanup(func() { metadataURL = oldMeta })

	// Total outage from a cold start: no snapshot, error reported.
	if err := c.Sync(context.Background()); err == nil {
		t.Fatal("cold outage must report an error")
	}
	// Seed a snapshot (as if previously synced), then an outage keeps it.
	c.mu.Lock()
	c.snap = Snapshot{UpdatedAt: time.Now(), Models: []ModelEntry{{ID: "glm-5.3-flash-free", Candidate: true}}}
	c.mu.Unlock()
	if err := c.Sync(context.Background()); err == nil {
		t.Fatal("outage should surface an error")
	}
	snap := c.Snapshot()
	if len(snap.Models) != 1 || !snap.Models[0].Candidate || snap.LastSyncError == "" {
		t.Fatalf("last-good snapshot must survive an outage: %+v", snap)
	}
}

func TestCatalogSurfacesMetadataFailure(t *testing.T) {
	// Regression: a metadata body past the cap used to be truncated silently —
	// every model degraded to "metadata_pending" and the four metadata columns
	// stayed empty with no error anywhere. A short read must now be loud, and
	// the snapshot must carry the reason even though the live list answered.
	liveSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"glm-5.3-flash-free"}]}`)
	}))
	defer liveSrv.Close()
	big := strings.Repeat("x", 4096)
	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"opencode":{"models":{"glm-5.3-flash-free":{"cost":{"input":0},"pad":%q}}}}`, big)
	}))
	defer metaSrv.Close()

	oldMeta, oldCap := metadataURL, metadataLimit
	metadataURL = metaSrv.URL
	metadataLimit = 1024
	t.Cleanup(func() { metadataURL, metadataLimit = oldMeta, oldCap })

	c := NewCatalog(liveSrv.Client(), nil, liveSrv.URL)
	if err := c.Sync(context.Background()); err == nil {
		t.Fatal("oversized metadata body must be reported as an error")
	}
	snap := c.Snapshot()
	if snap.MetadataError == "" || !strings.Contains(snap.MetadataError, "cap") {
		t.Fatalf("snapshot must carry the metadata failure: %+v", snap)
	}
	if snap.MetadataAge.IsZero() == false {
		t.Fatal("failed metadata fetch must not look fresh")
	}
	// The live list still answered, so the catalog keeps serving rows.
	if len(snap.Models) != 1 {
		t.Fatalf("live rows must survive a metadata-only failure: %+v", snap.Models)
	}
}

// Live guard for the body caps: models.dev silently outgrew the old 4 MiB
// read limit (5.75 MB by 2026-10), which degraded every model to
// "metadata_pending". Opt-in (BARGAINPROXY_LIVE_ZEN=1) so the suite stays
// offline; run it when the four metadata columns suddenly go empty.
func TestLiveModelsDevFitsTheCap(t *testing.T) {
	if os.Getenv("BARGAINPROXY_LIVE_ZEN") == "" {
		t.Skip("live network check; set BARGAINPROXY_LIVE_ZEN=1 to run")
	}
	c := NewCatalog(http.DefaultClient, nil, "https://opencode.ai/zen")
	meta, _, err := c.fetchMetadata(context.Background())
	if err != nil {
		t.Fatalf("metadata fetch: %v", err)
	}
	if len(meta) < 50 {
		t.Fatalf("metadata looks truncated: %d entries", len(meta))
	}
	for _, id := range []string{"beam-free", "muse-spark-1.3-contributor-free"} {
		d, ok := c.metaDetail[id]
		if !ok || d.Context == 0 {
			t.Fatalf("metaDetail missing for %s: %+v", id, d)
		}
		if len(d.Reasoning) == 0 {
			t.Fatalf("reasoning ladder missing for %s: %+v", id, d)
		}
	}
}

// The live models.dev schema ships [{"type":"effort","values":[…]}] — the
// older {"value"/"effort"} shapes are kept as tolerances, not as the contract.
func TestReasoningValuesShapes(t *testing.T) {
	live := json.RawMessage(`[{"type":"effort","values":["low","medium","high","xhigh","max"]},{"type":"other","values":["ignored"]}]`)
	if got := reasoningValues(live); strings.Join(got, ",") != "high,low,max,medium,xhigh" {
		t.Fatalf("live shape: %v", got)
	}
	legacy := json.RawMessage(`[{"value":"low"},{"effort":"high"},{"value":"none","disabled":false},{"value":"off"}]`)
	if got := reasoningValues(legacy); strings.Join(got, ",") != "high,low" {
		t.Fatalf("legacy shape: %v", got)
	}
	if got := reasoningValues(nil); got != nil {
		t.Fatalf("empty raw: %v", got)
	}
}

// A 304 means "unchanged", not "unavailable": both sources must keep being
// served from the last decoded data — the candidate gate (metadata) and the
// in-live flags (live list) — and the metadata age must survive. This is the
// bug that flipped the candidate set 11→12 and blanked the four metadata
// columns right after a restart, when the persisted ETag made the first
// fetch answer 304.
func TestNotModifiedKeepsLastKnownState(t *testing.T) {
	liveSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"l1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"l1"`)
		fmt.Fprint(w, `{"data":[{"id":"kept-free"},{"id":"gone-free"},{"id":"paid-model"}]}`)
	}))
	defer liveSrv.Close()
	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"m1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"m1"`)
		fmt.Fprint(w, `{"opencode":{"models":{
			"kept-free":   {"cost":{"input":0},"limit":{"context":111111,"output":2222},"modalities":{"input":["text","image"]},"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","high"]}]},
			"gone-free":   {"cost":{"input":1.5}}
		}}}`)
	}))
	defer metaSrv.Close()

	oldMeta := metadataURL
	metadataURL = metaSrv.URL
	t.Cleanup(func() { metadataURL = oldMeta })

	byID := func(s Snapshot, id string) ModelEntry {
		for _, m := range s.Models {
			if m.ID == id {
				return m
			}
		}
		t.Fatalf("model %s missing from snapshot", id)
		return ModelEntry{}
	}
	checkGate := func(step string, s Snapshot) {
		if !byID(s, "kept-free").Candidate || byID(s, "gone-free").Candidate {
			t.Fatalf("%s: candidate gate wrong: %+v", step, s.Models)
		}
		if !byID(s, "kept-free").InLiveList {
			t.Fatalf("%s: in-live flag lost: %+v", step, byID(s, "kept-free"))
		}
		if byID(s, "kept-free").ContextWindow != 111111 || byID(s, "kept-free").MaxOutput != 2222 {
			t.Fatalf("%s: metadata columns lost: %+v", step, byID(s, "kept-free"))
		}
		if len(byID(s, "kept-free").ReasoningVals) == 0 {
			t.Fatalf("%s: reasoning ladder lost: %+v", step, byID(s, "kept-free"))
		}
		if s.MetadataAge.IsZero() {
			t.Fatalf("%s: metadata age blanked", step)
		}
		if s.MetadataError != "" {
			t.Fatalf("%s: 304 reported as an error: %q", step, s.MetadataError)
		}
	}

	c := NewCatalog(liveSrv.Client(), nil, liveSrv.URL)
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	checkGate("first sync", c.Snapshot())

	// Second sync: both sources answer 304.
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	checkGate("304 sync", c.Snapshot())

	// Restart with the persisted state, then a 304-only sync: the decoded
	// metadata and live list must have been persisted, not just kept in RAM.
	st := newZenMemStore()
	c2 := NewCatalog(liveSrv.Client(), st, liveSrv.URL)
	if err := c2.Sync(context.Background()); err != nil {
		t.Fatalf("persisting sync: %v", err)
	}
	c3 := NewCatalog(liveSrv.Client(), st, liveSrv.URL)
	if err := c3.Sync(context.Background()); err != nil {
		t.Fatalf("post-restart sync: %v", err)
	}
	checkGate("post-restart 304 sync", c3.Snapshot())
}

// State persisted by an older build carries a validator but no decoded
// metadata: a 304 there has nothing to reuse, so the catalog must drop the
// validator and refetch instead of degrading to the name heuristic.
func TestNotModifiedWithoutCacheRefetches(t *testing.T) {
	liveSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"l1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"l1"`)
		fmt.Fprint(w, `{"data":[{"id":"kept-free"},{"id":"paid-model"}]}`)
	}))
	defer liveSrv.Close()
	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"m1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"m1"`)
		fmt.Fprint(w, `{"opencode":{"models":{"kept-free":{"cost":{"input":0},"limit":{"context":4242,"output":77}}}}}`)
	}))
	defer metaSrv.Close()

	oldMeta := metadataURL
	metadataURL = metaSrv.URL
	t.Cleanup(func() { metadataURL = oldMeta })

	c := NewCatalog(liveSrv.Client(), nil, liveSrv.URL)
	// As if loaded from an old persisted blob: validators present, no decoded data.
	c.etagMeta = `"m1"`
	c.etagLive = `"l1"`
	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	snap := c.Snapshot()
	if snap.MetadataAge.IsZero() {
		t.Fatal("304 without cached metadata must refetch, not degrade")
	}
	var kept ModelEntry
	for _, m := range snap.Models {
		if m.ID == "kept-free" {
			kept = m
		}
	}
	if !kept.Candidate || kept.ContextWindow != 4242 {
		t.Fatalf("refetched metadata not applied: %+v", kept)
	}
}

func TestReadCappedRejectsTruncation(t *testing.T) {
	if _, err := readCapped(strings.NewReader("abcdef"), 3); err == nil {
		t.Fatal("oversized body must error, not silently truncate")
	}
	raw, err := readCapped(strings.NewReader("abc"), 3)
	if err != nil || string(raw) != "abc" {
		t.Fatalf("exact-fit body must pass: %q %v", raw, err)
	}
}

func TestCatalogPersistRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"glm-5.3-flash-free"}]}`)
	}))
	defer srv.Close()
	meta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"opencode":{"models":{"glm-5.3-flash-free":{"cost":{"input":0}}}}}`)
	}))
	defer meta.Close()
	oldMeta := metadataURL
	metadataURL = meta.URL
	t.Cleanup(func() { metadataURL = oldMeta })

	st := newZenMemStore()
	c := NewCatalog(srv.Client(), st, srv.URL)
	if err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.RecordProbe("glm-5.3-flash-free", true, 42, "")

	// A fresh catalog (simulated restart) restores snapshot + probe + etags.
	c2 := NewCatalog(srv.Client(), st, srv.URL)
	snap := c2.Snapshot()
	if len(snap.Models) != 1 || snap.Models[0].ProbeStatus != "ok" || snap.Models[0].ProbeMs != 42 {
		t.Fatalf("persisted state lost: %+v", snap)
	}
}
