package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

func mkAccount(id, channel string) *model.Account {
	return &model.Account{ID: id, Channel: channel}
}

func TestGreedyPicksHighestBurnRate(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now)}, map[string]int{model.ChannelAntigravity: 3}, nil)

	// Account A: 80% weekly remaining, resets in 24h -> greed = 0.8/86400
	a := mkAccount("a", model.ChannelAntigravity)
	a.Windows = map[string]model.Window{
		"weekly":    {RemainingFraction: 0.8, ResetAt: now.Add(24 * time.Hour), UpdatedAt: now},
		"five_hour": {RemainingFraction: 0.5, ResetAt: now.Add(2 * time.Hour), UpdatedAt: now},
	}
	// Account B: 50% weekly remaining, resets in 1h -> greed much higher.
	b := mkAccount("b", model.ChannelAntigravity)
	b.Windows = map[string]model.Window{
		"weekly":    {RemainingFraction: 0.5, ResetAt: now.Add(time.Hour), UpdatedAt: now},
		"five_hour": {RemainingFraction: 0.9, ResetAt: now.Add(time.Hour), UpdatedAt: now},
	}
	p.Upsert(a)
	p.Upsert(b)

	lease, err := p.Acquire(context.Background(), model.ChannelAntigravity, "gemini-x", "")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lease.Release()
	if lease.Account.ID != "b" {
		// b's rate = 0.5/3600 ≈ 1.39e-4 ; a's = 0.8/86400 ≈ 9.26e-6
		t.Fatalf("greedy should pick b (higher burn rate), got %s", lease.Account.ID)
	}
}

func TestGreedyPrefersFreshKnownWindows(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now)}, map[string]int{model.ChannelAntigravity: 3}, nil)

	known := mkAccount("known", model.ChannelAntigravity)
	known.Windows = map[string]model.Window{
		"weekly":    {RemainingFraction: 0.01, ResetAt: now.Add(24 * time.Hour), UpdatedAt: now},
		"five_hour": {RemainingFraction: 0.01, ResetAt: now.Add(time.Hour), UpdatedAt: now},
	}
	// Unknown windows but stale (over 20 min old): should rank below fresh-known
	// even though raw greed could be misleading. Also verify unknown >= stale.
	unknown := mkAccount("unknown", model.ChannelAntigravity)
	p.Upsert(known)
	p.Upsert(unknown)

	lease, err := p.Acquire(context.Background(), model.ChannelAntigravity, "gemini-x", "")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lease.Release()
	if lease.Account.ID != "known" {
		t.Fatalf("fresh known windows should outrank unknown, got %s", lease.Account.ID)
	}
}

func TestStickySessionKeepsAccount(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now), SessionTTL: time.Hour}, map[string]int{model.ChannelAntigravity: 1}, nil)
	a := mkAccount("a", model.ChannelAntigravity)
	b := mkAccount("b", model.ChannelAntigravity)
	p.Upsert(a)
	p.Upsert(b)

	l1, err := p.Acquire(context.Background(), model.ChannelAntigravity, "m", "sess-1")
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	first := l1.Account.ID
	l1.Release()

	l2, err := p.Acquire(context.Background(), model.ChannelAntigravity, "m", "sess-1")
	if err != nil {
		t.Fatalf("acquire2: %v", err)
	}
	defer l2.Release()
	if l2.Account.ID != first {
		t.Fatalf("sticky session drifted: %s -> %s", first, l2.Account.ID)
	}
}

func TestModelCooldownExemptsOtherModels(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now)}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	a := mkAccount("a", model.ChannelWorkBuddy)
	p.Upsert(a)

	// Simulate 6004-style model-scoped rate limit on glm-5.3.
	p.ApplyResult(a, "glm-5.3", &uperr.Error{Class: uperr.ClassModelRate, Model: "glm-5.3", ResetAt: now.Add(time.Hour)})

	if _, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", ""); err == nil {
		t.Fatal("expected no capacity for cooled model")
	}
	lease, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "deepseek-v4-flash", "")
	if err != nil {
		t.Fatalf("other model should be exempt from model cooldown: %v", err)
	}
	lease.Release()
}

func TestHardCreditCoolsUntil4AMShanghai(t *testing.T) {
	// 2026-10-14 05:00 UTC = 13:00 Shanghai; a 402 now should unlock at
	// 04:00 Shanghai next day = 2026-10-15 04:00 +08.
	now := time.Date(2026, 10, 14, 5, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now)}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	a := mkAccount("a", model.ChannelWorkBuddy)
	p.Upsert(a)

	p.ApplyResult(a, "glm-5.3", &uperr.Error{Class: uperr.ClassHardCredit})
	until := a.AccountCooldownUntil.In(Shanghai)
	if until.Hour() != 4 || until.Day() != 15 {
		t.Fatalf("402 should cool to next 04:00 Shanghai, got %s", until)
	}
}

func TestContentErrorsDoNotPenalize(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now)}, map[string]int{model.ChannelWorkBuddy: 2}, nil)
	a := mkAccount("a", model.ChannelWorkBuddy)
	p.Upsert(a)

	p.ApplyResult(a, "glm-5.3", &uperr.Error{Class: uperr.ClassBadRequest})
	p.ApplyResult(a, "glm-5.3", &uperr.Error{Class: uperr.ClassContentBlocked})
	if now.Before(a.AccountCooldownUntil) {
		t.Fatal("content-level errors must not cool the account")
	}
	if len(a.ModelCooldowns) != 0 {
		t.Fatal("content-level errors must not create model cooldowns")
	}
}

// TestUpstreamBusyPausesChannelModelNotAccounts: a queue-full event must
// pause the channel+model pair (fast-fail Acquire) while leaving every
// account's cooldown/disable/credit state untouched — the field incident
// cooled a whole healthy pool in seconds because capacity errors were folded
// into per-account state. Other models on the same channel stay servable,
// and the pause expires so the channel recovers without manual action.
func TestUpstreamBusyPausesChannelModelNotAccounts(t *testing.T) {
	cur := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: func() time.Time { return cur }, UpstreamBusyCooldown: 45 * time.Second},
		map[string]int{model.ChannelWorkBuddy: 2}, nil)
	a := mkAccount("a", model.ChannelWorkBuddy)
	p.Upsert(a)

	p.ApplyResult(a, "deepseek-v4.1-flash", &uperr.Error{Class: uperr.ClassUpstreamBusy, Model: "deepseek-v4.1-flash"})

	// Zero account penalty.
	if !a.AccountCooldownUntil.IsZero() {
		t.Fatalf("queue-full must not cool the account: %s", a.AccountCooldownUntil)
	}
	if len(a.ModelCooldowns) != 0 {
		t.Fatalf("queue-full must not create model cooldowns: %v", a.ModelCooldowns)
	}
	if a.ConsecutiveFails != 0 {
		t.Fatalf("queue-full must not advance fail counters: %d", a.ConsecutiveFails)
	}

	// Same channel+model: fast fail with the pause deadline, no lease.
	if lease, _, _ := p.tryAcquireForTest(model.ChannelWorkBuddy, "deepseek-v4.1-flash", ""); lease != nil {
		t.Fatal("paused channel+model must not produce a lease")
	}
	if until, ok := p.ChannelBusyUntil(model.ChannelWorkBuddy, "deepseek-v4.1-flash", cur); !ok || !until.Equal(cur.Add(45*time.Second)) {
		t.Fatalf("busy until = (%s, %v)", until, ok)
	}

	// A different model on the same channel is unaffected.
	l, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "")
	if err != nil {
		t.Fatalf("other model must stay servable: %v", err)
	}
	l.Release()

	// After the pause expires the channel recovers and the entry is dropped.
	cur = cur.Add(46 * time.Second)
	if _, ok := p.ChannelBusyUntil(model.ChannelWorkBuddy, "deepseek-v4.1-flash", cur); ok {
		t.Fatal("pause must expire")
	}
	l2, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "deepseek-v4.1-flash", "")
	if err != nil {
		t.Fatalf("expired pause must release the channel: %v", err)
	}
	l2.Release()
}

// tryAcquireForTest is the test-facing form of tryAcquire (package-internal).
func (p *Pool) tryAcquireForTest(channel, modelName, sessionKey string) (*Lease, error, bool) {
	return p.tryAcquire(channel, modelName, sessionKey)
}

func TestCreditTierPrefersFreeAccounts(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now), CostTTL: time.Hour}, map[string]int{model.ChannelWorkBuddy: 4}, nil)
	free := mkAccount("free", model.ChannelWorkBuddy)
	paid := mkAccount("paid", model.ChannelWorkBuddy)
	p.Upsert(free)
	p.Upsert(paid)
	p.RecordCostTier(free, "glm-5.3", 0) // observed free
	p.RecordCostTier(paid, "glm-5.3", 2) // observed paid

	for i := 0; i < 10; i++ {
		lease, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		if lease.Account.ID != "free" {
			t.Fatalf("credit tier 0 must beat tier 2, got %s", lease.Account.ID)
		}
		lease.Release()
	}
}

func TestCreditFloorOnlyBlocksObservedPaid(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)

	newPool := func(acc *model.Account) *Pool {
		p := New(Params{Now: fixedNow(now), CostTTL: time.Hour}, map[string]int{model.ChannelWorkBuddy: 4}, nil)
		p.Upsert(acc)
		return p
	}
	get := func(p *Pool) (*Lease, error) {
		return p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "")
	}

	// 0-credit account + no cost observation → selectable (tier-1 must stay
	// reachable, otherwise the account can never re-learn its cost).
	empty := mkAccount("empty", model.ChannelWorkBuddy)
	empty.Credits = model.Credits{Remain: 0, Total: 100, UpdatedAt: now}
	l, err := get(newPool(empty))
	if err != nil {
		t.Fatalf("empty + unknown tier must be selectable: %v", err)
	}
	l.Release()

	// 0-credit account + observed FREE model → selectable (the floor exists
	// to keep balance for free models).
	free := mkAccount("free", model.ChannelWorkBuddy)
	free.Credits = model.Credits{Remain: 0, Total: 100, UpdatedAt: now}
	pf := newPool(free)
	pf.RecordCostTier(free, "glm-5.3", 0)
	l, err = get(pf)
	if err != nil {
		t.Fatalf("empty + observed-free must be selectable: %v", err)
	}
	l.Release()

	// 0-credit account + observed PAID model → blocked.
	paid := mkAccount("paid", model.ChannelWorkBuddy)
	paid.Credits = model.Credits{Remain: 0, Total: 100, UpdatedAt: now}
	pp := newPool(paid)
	pp.RecordCostTier(paid, "glm-5.3", 2)
	if _, err = get(pp); err == nil {
		t.Fatal("empty + observed-paid must be floor-blocked")
	}

	// Funded account + observed paid model → selectable.
	rich := mkAccount("rich", model.ChannelWorkBuddy)
	rich.Credits = model.Credits{Remain: 500, Total: 500, UpdatedAt: now}
	pr := newPool(rich)
	pr.RecordCostTier(rich, "glm-5.3", 2)
	l, err = get(pr)
	if err != nil {
		t.Fatalf("funded account must stay selectable for paid models: %v", err)
	}
	l.Release()
}

// TestCreditWeightPrefersHigherBalance: the credits ratio term (×10) must
// make a 900-credit account dominate a 50-credit one when everything else
// is equal. The clock advances between draws so the anti-collision window
// never silently reroutes the draw to the LRU fallback (which would flatten
// the distribution).
func TestCreditWeightPrefersHigherBalance(t *testing.T) {
	cur := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: func() time.Time { return cur }}, map[string]int{model.ChannelWorkBuddy: 8}, nil)
	rich := mkAccount("rich", model.ChannelWorkBuddy)
	rich.Credits = model.Credits{Remain: 900, Total: 900, UpdatedAt: cur}
	poor := mkAccount("poor", model.ChannelWorkBuddy)
	poor.Credits = model.Credits{Remain: 50, Total: 900, UpdatedAt: cur}
	p.Upsert(rich)
	p.Upsert(poor)

	hits := map[string]int{}
	for i := 0; i < 60; i++ {
		lease, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		hits[lease.Account.ID]++
		lease.Release()
		cur = cur.Add(200 * time.Millisecond) // step past the anti-collision gap
	}
	// Weight ratio ≈ 7:1 → rich must win the clear majority.
	if hits["rich"] <= hits["poor"]*2 {
		t.Fatalf("balance-weighted draw must favor the richer account: %+v", hits)
	}
}

// TestCreditLRUFallbackOnAllInAntiCollisionWindow: when every shortlisted
// account was picked inside the 100ms window, the fallback must pick the
// oldest pick by monotonic sequence — never wall-clock (Windows time.Now()
// ~0.5ms precision makes burst comparisons tie).
func TestCreditLRUFallbackOnAllInAntiCollisionWindow(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now)}, map[string]int{model.ChannelWorkBuddy: 8}, nil)
	a := mkAccount("a", model.ChannelWorkBuddy)
	b := mkAccount("b", model.ChannelWorkBuddy)
	p.Upsert(a)
	p.Upsert(b)

	// First two picks land both accounts inside the anti-collision window
	// (fixed clock: the window never expires).
	l1, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "")
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	first := l1.Account.ID
	l1.Release()
	l2, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "")
	if err != nil {
		t.Fatalf("acquire2: %v", err)
	}
	if l2.Account.ID == first {
		t.Fatalf("anti-collision must spread the second pick: %s twice", first)
	}
	l2.Release()

	l3, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "")
	if err != nil {
		t.Fatalf("acquire3: %v", err)
	}
	defer l3.Release()
	if l3.Account.ID != first {
		t.Fatalf("LRU fallback must return the oldest pick %s, got %s", first, l3.Account.ID)
	}
}

// TestCreditEqualWeightShuffleCoversAll: with >5 equal-weight candidates the
// pre-truncation Fisher-Yates shuffle must give every account a Top-5 entry
// over repeated draws — no fixed-order starvation of later ids (the
// historical 惊群 root cause).
//
// The clock MUST advance between draws: with a frozen clock every account
// picked once sits in the 100ms anti-collision window from draw 6 on, so the
// weighted draw is bypassed for the LastPickSeq LRU fallback, which rotates
// through the full candidate set and covers every id with the shuffle code
// never executed (a false green — deleting the shuffle kept this test
// passing). Candidates come from the deterministic byChan insertion order,
// so without the shuffle the Top-5 is always the first five ids and the
// later ones starve — making this test discriminating.
func TestCreditEqualWeightShuffleCoversAll(t *testing.T) {
	cur := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: func() time.Time { return cur }}, map[string]int{model.ChannelWorkBuddy: 4}, nil)
	ids := []string{"n1", "n2", "n3", "n4", "n5", "n6", "n7", "n8"}
	for _, id := range ids {
		p.Upsert(mkAccount(id, model.ChannelWorkBuddy))
	}

	hits := map[string]int{}
	for i := 0; i < 80*len(ids); i++ {
		lease, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "")
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		hits[lease.Account.ID]++
		lease.Release()
		cur = cur.Add(200 * time.Millisecond) // step past the anti-collision gap
	}
	for _, id := range ids {
		if hits[id] == 0 {
			t.Fatalf("equal-weight account %s was starved out of Top-5: %+v", id, hits)
		}
	}
}

// TestNeverUsedRanksWithFullyIdleAccounts: a fresh import must carry the
// full idle score (upstream pick.go parity), so it competes evenly with an
// account idle beyond the 30-minute cap instead of ranking below one used
// seconds ago. Before the fix a never-used account scored 0 idle, so the
// first lucky pick (idle > 0 the moment it was used) outranked every
// still-unused peer and herded all traffic onto the early picks — the exact
// mechanism that once made the shuffle-coverage test a false green.
func TestNeverUsedRanksWithFullyIdleAccounts(t *testing.T) {
	cur := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: func() time.Time { return cur }}, map[string]int{model.ChannelWorkBuddy: 4}, nil)
	fresh := mkAccount("fresh", model.ChannelWorkBuddy)
	veteran := mkAccount("veteran", model.ChannelWorkBuddy)
	veteran.LastUsedAt = cur.Add(-45 * time.Minute) // idle bonus already capped at +1.0
	p.Upsert(fresh)
	p.Upsert(veteran)

	hits := map[string]int{}
	for i := 0; i < 60; i++ {
		lease, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		hits[lease.Account.ID]++
		lease.Release()
		cur = cur.Add(200 * time.Millisecond) // step past the anti-collision gap
	}
	// Post-fix the two weights tie at the first draw and then alternate
	// (whichever was just picked drops to ~1.0 while the other sits at 2.0),
	// so fresh lands near 30/60. Pre-fix it starves near 0.
	if hits["fresh"] < 15 {
		t.Fatalf("never-used account must compete with a fully idle one: %+v", hits)
	}
}

// TestExpiringCreditsGetPriority: expiring-credit pressure (×3) must favor
// the expiring account in the weighted draw. The clock advances between
// draws so the 100ms anti-collision window never distorts the distribution.
func TestExpiringCreditsGetPriority(t *testing.T) {
	cur := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: func() time.Time { return cur }}, map[string]int{model.ChannelWorkBuddy: 4}, nil)
	expiring := mkAccount("expiring", model.ChannelWorkBuddy)
	expiring.Credits = model.Credits{Remain: 100, Total: 100, Expiring: 80, UpdatedAt: cur}
	normal := mkAccount("normal", model.ChannelWorkBuddy)
	normal.Credits = model.Credits{Remain: 100, Total: 100, UpdatedAt: cur}
	p.Upsert(expiring)
	p.Upsert(normal)

	hits := map[string]int{}
	for i := 0; i < 60; i++ {
		lease, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		hits[lease.Account.ID]++
		lease.Release()
		cur = cur.Add(200 * time.Millisecond) // step past the anti-collision gap
	}
	if hits["expiring"] <= hits["normal"] {
		t.Fatalf("expiring-credits account should be favored: %+v", hits)
	}
}

func TestAllBusyWaitsThenSucceeds(t *testing.T) {
	now := time.Now()
	p := New(Params{Now: time.Now, MaxQueueWait: 2 * time.Second}, map[string]int{model.ChannelTrae: 1}, nil)
	a := mkAccount("a", model.ChannelTrae)
	a.Credits = model.Credits{Remain: 100, UpdatedAt: now}
	p.Upsert(a)

	l1, err := p.Acquire(context.Background(), model.ChannelTrae, "m", "")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	done := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		l1.Release()
		close(done)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	l2, err := p.Acquire(ctx, model.ChannelTrae, "m", "")
	if err != nil {
		t.Fatalf("second acquire should wait for the slot: %v", err)
	}
	l2.Release()
	<-done
}

func TestAllCoolingFailsFast(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now), MaxQueueWait: time.Second}, map[string]int{model.ChannelTrae: 1}, nil)
	a := mkAccount("a", model.ChannelTrae)
	p.Upsert(a)
	p.ApplyResult(a, "m", &uperr.Error{Class: uperr.ClassModelRate, Model: "m", ResetAt: now.Add(time.Hour)})

	start := time.Now()
	_, err := p.Acquire(context.Background(), model.ChannelTrae, "m", "")
	if err == nil {
		t.Fatal("expected failure")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("cooling pool should fail fast, took %s", elapsed)
	}
}

// --- BP-09 sticky no-drift acceptance ---

// TestStickyNotMovedByGreedChange pins the AIClient2API 7a7a3b6 semantics:
// once a session is bound, a later higher greed score on another account
// must not move it.
func TestStickyNotMovedByGreedChange(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now), SessionTTL: time.Hour}, map[string]int{model.ChannelAntigravity: 1}, nil)

	// Both fresh-known; b's burn rate is higher, so the first pick is b.
	a := mkAccount("a", model.ChannelAntigravity)
	a.Windows = map[string]model.Window{
		"weekly":    {RemainingFraction: 0.1, ResetAt: now.Add(24 * time.Hour), UpdatedAt: now},
		"five_hour": {RemainingFraction: 0.5, ResetAt: now.Add(time.Hour), UpdatedAt: now},
	}
	b := mkAccount("b", model.ChannelAntigravity)
	b.Windows = map[string]model.Window{
		"weekly":    {RemainingFraction: 0.9, ResetAt: now.Add(time.Hour), UpdatedAt: now},
		"five_hour": {RemainingFraction: 0.9, ResetAt: now.Add(time.Hour), UpdatedAt: now},
	}
	p.Upsert(a)
	p.Upsert(b)

	l1, err := p.Acquire(context.Background(), model.ChannelAntigravity, "m", "sess-1")
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	if l1.Account.ID != "b" {
		t.Fatalf("first pick should be b (higher burn rate), got %s", l1.Account.ID)
	}
	l1.Release()

	// Flip the scores: a is now far greedier than b.
	a.Windows["weekly"] = model.Window{RemainingFraction: 1.0, ResetAt: now.Add(time.Hour), UpdatedAt: now}
	b.Windows["weekly"] = model.Window{RemainingFraction: 0.05, ResetAt: now.Add(24 * time.Hour), UpdatedAt: now}

	l2, err := p.Acquire(context.Background(), model.ChannelAntigravity, "m", "sess-1")
	if err != nil {
		t.Fatalf("acquire2: %v", err)
	}
	defer l2.Release()
	if l2.Account.ID != "b" {
		t.Fatalf("bound session moved to %s after greed change (drift)", l2.Account.ID)
	}
}

// TestStickyFullWaitsNotDrift: while the bound account is full, a second
// request for the same session must queue for it instead of taking the idle
// sibling (AIClient2API enqueue semantics).
func TestStickyFullWaitsNotDrift(t *testing.T) {
	p := New(Params{Now: time.Now, SessionTTL: time.Hour, MaxQueueWait: 3 * time.Second},
		map[string]int{model.ChannelAntigravity: 1}, nil)
	a := mkAccount("a", model.ChannelAntigravity)
	b := mkAccount("b", model.ChannelAntigravity)
	p.Upsert(a)
	p.Upsert(b)

	// Deterministic bind: both unknown-window accounts tie, ID order picks a.
	l1, err := p.Acquire(context.Background(), model.ChannelAntigravity, "m", "sess-1")
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	if l1.Account.ID != "a" {
		t.Fatalf("expected deterministic bind to a, got %s", l1.Account.ID)
	}

	type result struct {
		id  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		l2, err := p.Acquire(ctx, model.ChannelAntigravity, "m", "sess-1")
		if err != nil {
			done <- result{err: err}
			return
		}
		done <- result{id: l2.Account.ID}
		l2.Release()
	}()

	// Give the waiter time to attempt: it must NOT take b while a is full.
	time.Sleep(120 * time.Millisecond)
	if got := p.InFlight("b"); got != 0 {
		t.Fatalf("waiter drifted to the idle sibling while bound account full")
	}
	l1.Release()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("waiter failed: %v", r.err)
		}
		if r.id != "a" {
			t.Fatalf("waiter landed on %s, want the bound account a", r.id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never acquired after release")
	}
}

// TestStickyUnbindsWhenCooling: a bound account that goes cooling (or is
// disabled) unbinds, and the session reselects + rebinds.
func TestStickyUnbindsWhenCooling(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now), SessionTTL: time.Hour}, map[string]int{model.ChannelAntigravity: 1}, nil)
	a := mkAccount("a", model.ChannelAntigravity)
	a.Windows = map[string]model.Window{
		"weekly":    {RemainingFraction: 0.9, ResetAt: now.Add(time.Hour), UpdatedAt: now},
		"five_hour": {RemainingFraction: 0.9, ResetAt: now.Add(time.Hour), UpdatedAt: now},
	}
	b := mkAccount("b", model.ChannelAntigravity)
	p.Upsert(a)
	p.Upsert(b)

	l1, err := p.Acquire(context.Background(), model.ChannelAntigravity, "m", "sess-1")
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	if l1.Account.ID != "a" {
		t.Fatalf("expected bind to a, got %s", l1.Account.ID)
	}
	l1.Release()

	// a cools down at the model level: the binding must break.
	p.ApplyResult(a, "m", &uperr.Error{Class: uperr.ClassModelRate, Model: "m", ResetAt: now.Add(time.Hour)})

	l2, err := p.Acquire(context.Background(), model.ChannelAntigravity, "m", "sess-1")
	if err != nil {
		t.Fatalf("acquire2: %v", err)
	}
	defer l2.Release()
	if l2.Account.ID != "b" {
		t.Fatalf("cooled bound account must unbind and reselect, got %s", l2.Account.ID)
	}
	if id, ok := p.SessionBinding(model.ChannelAntigravity, "sess-1"); !ok || id != "b" {
		t.Fatalf("binding not updated after reselect: %q %v", id, ok)
	}
}

// TestStickyUnbindsWhenFloorBlocked: the sticky path applies the same credit
// floor as fresh selection (upstream state.go parity) — a bound account
// drained to zero unbinds for a model observed PAID (tier 2), but stays bound
// for an unknown (no observation) or observed-free (tier 0) model. Without
// the sticky floor check a drained account keeps serving a paid model until
// upstream rejects every request.
func TestStickyUnbindsWhenFloorBlocked(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now), CostTTL: time.Hour}, map[string]int{model.ChannelWorkBuddy: 4}, nil)
	p.Upsert(mkAccount("a", model.ChannelWorkBuddy))
	p.Upsert(mkAccount("b", model.ChannelWorkBuddy))

	l1, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "sess-1")
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	bound := l1.Account.ID
	other := map[string]string{"a": "b", "b": "a"}[bound]
	l1.Release()

	acc := p.Accounts(model.ChannelWorkBuddy)
	var boundAcc *model.Account
	for _, x := range acc {
		if x.ID == bound {
			boundAcc = x
		}
	}
	p.RecordCredits(boundAcc, model.Credits{Remain: 0, Total: 100, UpdatedAt: now})

	// No cost observation (tier 1): floor does not block, binding survives.
	l2, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "sess-1")
	if err != nil {
		t.Fatalf("acquire2: %v", err)
	}
	if l2.Account.ID != bound {
		t.Fatalf("drained + unknown tier must stay sticky on %s, got %s", bound, l2.Account.ID)
	}
	l2.Release()

	// Observed FREE (tier 0): floor reserves balance for free models, binding survives.
	p.RecordCostTier(boundAcc, "glm-5.3", 0)
	l3, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "sess-1")
	if err != nil {
		t.Fatalf("acquire3: %v", err)
	}
	if l3.Account.ID != bound {
		t.Fatalf("drained + observed-free must stay sticky on %s, got %s", bound, l3.Account.ID)
	}
	l3.Release()

	// Observed PAID (tier 2): sticky unbinds and reselects the funded account.
	p.RecordCostTier(boundAcc, "glm-5.3", 2)
	l4, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "sess-1")
	if err != nil {
		t.Fatalf("acquire4: %v", err)
	}
	defer l4.Release()
	if l4.Account.ID != other {
		t.Fatalf("drained + observed-paid must unbind to %s, got %s", other, l4.Account.ID)
	}
	if id, ok := p.SessionBinding(model.ChannelWorkBuddy, "sess-1"); !ok || id != other {
		t.Fatalf("binding not moved to %s after floor unbind: %q %v", other, id, ok)
	}
}

// TestSetGlobalEnabledDropsGlobalStickyBindings: turning the international
// switch off immediately drops sticky bindings that point at realm=global
// accounts (previously they lingered until the SessionTTL GC), while cn
// bindings survive; re-enabling later must not have removed cn bindings.
// The cn account joins after the global session binds, so both bindings are
// deterministic.
func TestSetGlobalEnabledDropsGlobalStickyBindings(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	p := New(Params{Now: fixedNow(now), SessionTTL: time.Hour}, map[string]int{model.ChannelWorkBuddy: 4}, nil)
	g := mkAccount("g", model.ChannelWorkBuddy)
	g.Realm = "global"
	p.SetGlobalEnabled(true)
	p.Upsert(g)

	l1, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "sess-g")
	if err != nil {
		t.Fatalf("acquire global: %v", err)
	}
	if l1.Account.ID != "g" {
		l1.Release()
		t.Fatalf("global session must bind g, got %s", l1.Account.ID)
	}
	l1.Release()

	// cn account appears while sess-g is already bound to g.
	cn := mkAccount("cn", model.ChannelWorkBuddy)
	p.Upsert(cn)
	l2, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.2", "sess-cn")
	if err != nil {
		t.Fatalf("acquire cn: %v", err)
	}
	cnBound := l2.Account.ID
	if cnBound != "cn" {
		l2.Release()
		t.Fatalf("second session must reach the cn account, got %s", cnBound)
	}
	l2.Release()

	p.SetGlobalEnabled(false)

	if _, ok := p.SessionBinding(model.ChannelWorkBuddy, "sess-g"); ok {
		t.Fatal("global sticky binding must be dropped when the switch turns off")
	}
	if id, ok := p.SessionBinding(model.ChannelWorkBuddy, "sess-cn"); !ok || id != cnBound {
		t.Fatalf("cn sticky binding must survive the switch: %q %v", id, ok)
	}
	// The dropped global session no longer resolves: it reselects from the
	// now cn-only pool instead of erroring or serving the hidden account.
	l3, err := p.Acquire(context.Background(), model.ChannelWorkBuddy, "glm-5.3", "sess-g")
	if err != nil {
		t.Fatalf("acquire after switch-off: %v", err)
	}
	defer l3.Release()
	if l3.Account.ID != cnBound {
		t.Fatalf("post-switch-off reselect must land on a cn account, got %s", l3.Account.ID)
	}
}

// TestStickyBusyTimeoutReturnsBusy: when the bound account stays full past
// MaxQueueWait, the acquire reports "busy" and never switches accounts.
func TestStickyBusyTimeoutReturnsBusy(t *testing.T) {
	p := New(Params{Now: time.Now, SessionTTL: time.Hour, MaxQueueWait: 150 * time.Millisecond},
		map[string]int{model.ChannelAntigravity: 1}, nil)
	a := mkAccount("a", model.ChannelAntigravity)
	b := mkAccount("b", model.ChannelAntigravity)
	p.Upsert(a)
	p.Upsert(b)

	l1, err := p.Acquire(context.Background(), model.ChannelAntigravity, "m", "sess-1")
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	defer l1.Release()

	start := time.Now()
	_, err = p.Acquire(context.Background(), model.ChannelAntigravity, "m", "sess-1")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected busy error while bound account full past the queue window")
	}
	var nc *ErrNoCapacity
	if !errors.As(err, &nc) || !nc.HadCandidatesOnly {
		t.Fatalf("want ErrNoCapacity{HadCandidatesOnly}, got %v", err)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("returned before the queue window elapsed: %s", elapsed)
	}
	if got := p.InFlight("b"); got != 0 {
		t.Fatal("busy timeout must not silently switch to another account")
	}
	if id, ok := p.SessionBinding(model.ChannelAntigravity, "sess-1"); !ok || id != "a" {
		t.Fatalf("binding must survive a busy timeout: %q %v", id, ok)
	}
}
