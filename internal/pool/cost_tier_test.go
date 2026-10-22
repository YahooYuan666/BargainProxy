package pool

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
)

// TestRecordCreditsObservesCostTier pins BP-100: for channels that do not
// report a per-request charge, the balance delta is the only truth. Before this
// nothing wrote CostTiers outside tests, so the floor guard never fired.
func TestRecordCreditsObservesCostTier(t *testing.T) {
	now := sinkNow
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(now)}, nil, nil)
	a := mkAccount("wb-a", model.ChannelAutoClaw)
	p.Upsert(a)

	// A first snapshot: nothing to compare against, no observation.
	p.RecordCredits(a, model.Credits{Remain: 1000})
	p.NoteModelUse(a, "zai_glm-5.3-flash")

	// Balance fell: the model was billed.
	p.mu.Lock()
	p.params.Now = fixedNow(now.Add(time.Minute))
	p.mu.Unlock()
	p.RecordCredits(a, model.Credits{Remain: 979})
	obs, ok := a.CostTiers["zai_glm-5.3-flash"]
	if !ok || obs.Tier != 2 {
		t.Fatalf("tier = %+v ok=%v, want tier 2 after a balance drop", obs, ok)
	}

	// Balance held: a different model was free.
	p.NoteModelUse(a, "zai_auto")
	p.mu.Lock()
	p.params.Now = fixedNow(now.Add(2 * time.Minute))
	p.mu.Unlock()
	p.RecordCredits(a, model.Credits{Remain: 979})
	if obs := a.CostTiers["zai_auto"]; obs.Tier != 0 {
		t.Fatalf("tier = %d, want 0 (balance held)", obs.Tier)
	}

	// A top-up says nothing: it must not mark anything free or paid.
	p.NoteModelUse(a, "zai_glm-5.3-flash")
	p.mu.Lock()
	p.params.Now = fixedNow(now.Add(3 * time.Minute))
	p.mu.Unlock()
	before := a.CostTiers["zai_glm-5.3-flash"]
	p.RecordCredits(a, model.Credits{Remain: 1979})
	if after := a.CostTiers["zai_glm-5.3-flash"]; after != before {
		t.Fatalf("a top-up changed the observation: %+v -> %+v", before, after)
	}
}
