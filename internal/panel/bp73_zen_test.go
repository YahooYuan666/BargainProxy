package panel

import (
	"testing"

	"bargainproxy/internal/driver/opencodezen"
)

// BP-73: the exposed (routed) set is "candidates minus definitive failures".
// A transient 429/timeout keeps the route; only a real failure removes it, and
// an all-failed cycle falls back to the full candidate list instead of emptying
// the table.
func TestZenUsableModelsKeepsLimited(t *testing.T) {
	cat := opencodezen.NewCatalog(nil, nil, "")
	cat.SeedForTest([]opencodezen.ModelEntry{
		{ID: "ok-model", Candidate: true, ProbeStatus: opencodezen.ProbeOK},
		{ID: "limited-model", Candidate: true, ProbeStatus: opencodezen.ProbeLimited},
		{ID: "dead-model", Candidate: true, ProbeStatus: opencodezen.ProbeFail},
		{ID: "unprobed-model", Candidate: true},
		{ID: "paid-model", Candidate: false},
	})
	SetZen(cat, func(bool) {})
	p := &Panel{}
	got := map[string]bool{}
	for _, id := range p.zenUsableModels() {
		got[id] = true
	}
	if !got["ok-model"] || !got["limited-model"] || !got["unprobed-model"] {
		t.Fatalf("usable set dropped a live model: %v", got)
	}
	if got["dead-model"] {
		t.Fatalf("definitive failure must leave the route table: %v", got)
	}
	if got["paid-model"] {
		t.Fatalf("non-candidate must never be routed: %v", got)
	}
}

func TestZenUsableModelsAllFailedFallsBack(t *testing.T) {
	cat := opencodezen.NewCatalog(nil, nil, "")
	cat.SeedForTest([]opencodezen.ModelEntry{
		{ID: "a", Candidate: true, ProbeStatus: opencodezen.ProbeFail},
		{ID: "b", Candidate: true, ProbeStatus: opencodezen.ProbeFail},
	})
	SetZen(cat, func(bool) {})
	p := &Panel{}
	if got := p.zenUsableModels(); len(got) != 2 {
		t.Fatalf("all-failed cycle must keep the candidate list, got %v", got)
	}
}
