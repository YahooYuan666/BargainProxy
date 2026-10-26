package pool

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
)

// BP-134: the credit floor guards on SPENDABLE (plan remainder + add-on
// packs), not the plan remainder alone. Field 2026-10-27: a Qoder account
// with 套餐内 300/300 used (remain 0) and 资源包 382/400 remaining was being
// held out of routing for every observed-paid model, while the client's own
// usage page showed 382 credits available.
func TestFloorRespectsAddonPack(t *testing.T) {
	now := sinkNow
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(now)}, nil, nil)
	a := mkAccount("qd-a", model.ChannelQoder)
	p.Upsert(a)

	// The model is OBSERVED to bill (tier 2) — the only case the floor
	// applies to at all.
	p.NoteModelUse(a, "GLM-5.3-Flash")
	p.RecordCredits(a, model.Credits{Remain: 300, Total: 300, UpdatedAt: time.Unix(1000, 0)})
	p.mu.Lock()
	p.params.Now = fixedNow(now.Add(time.Minute))
	p.mu.Unlock()
	// RecordCredits takes the pool lock itself — it must never be called
	// while the test holds it (first draft deadlocked exactly there).
	p.RecordCredits(a, model.Credits{Remain: 0, Total: 300, Bonus: 382, BonusTotal: 400,
		UpdatedAt: time.Unix(2000, 0)})
	p.mu.Lock()
	blockedWithAddon := p.floorBlockedLocked(a, "GLM-5.3-Flash", now.Add(time.Minute))
	p.mu.Unlock()
	if blockedWithAddon {
		t.Fatal("plan exhausted but the add-on pack holds 382 credits — the account must stay routable")
	}

	// Same shape WITHOUT the pack: the floor does its job.
	a2 := mkAccount("qd-b", model.ChannelQoder)
	p.Upsert(a2)
	p.NoteModelUse(a2, "GLM-5.3-Flash")
	p.RecordCredits(a2, model.Credits{Remain: 300, Total: 300, UpdatedAt: time.Unix(1000, 0)})
	p.mu.Lock()
	p.params.Now = fixedNow(now.Add(time.Minute))
	p.mu.Unlock()
	p.RecordCredits(a2, model.Credits{Remain: 0, Total: 300, UpdatedAt: time.Unix(2000, 0)})
	p.mu.Lock()
	blockedDrained := p.floorBlockedLocked(a2, "GLM-5.3-Flash", now.Add(time.Minute))
	p.mu.Unlock()
	if !blockedDrained {
		t.Fatal("plan exhausted with no add-on pack must be floored for observed-paid models")
	}
}

// TestFloorBalanceOnlyUnaffected: balance-only channels keep the old
// semantics (the balance IS the spendable amount).
func TestFloorBalanceOnlyUnaffected(t *testing.T) {
	now := sinkNow
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(now)}, nil, nil)
	a := mkAccount("ac-a", model.ChannelAutoClaw)
	p.Upsert(a)
	p.NoteModelUse(a, "zai_glm-5.3-flash")
	// Two snapshots so the cost observer sees the drop (tier 2 = observed
	// paid) — the floor only applies to observed-paid models.
	p.RecordCredits(a, model.Credits{Remain: 10400, Total: 10400, BalanceOnly: true, UpdatedAt: time.Unix(1000, 0)})
	p.RecordCredits(a, model.Credits{Remain: 0, Total: 0, BalanceOnly: true, UpdatedAt: time.Unix(2000, 0)})
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.floorBlockedLocked(a, "zai_glm-5.3-flash", now) {
		t.Fatal("a drained balance-only account must stay floored")
	}
}
