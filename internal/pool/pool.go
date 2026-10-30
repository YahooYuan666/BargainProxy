// Package pool implements the account pool: the single scheduler shared by
// all channels. Each channel keeps its own sub-pool (credentials, quota
// mechanics and concurrency limits are channel-specific), while the pooling
// mechanics — state machine, cooldowns, session stickiness, in-flight
// accounting — are uniform.
package pool

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// randFloat64 returns a uniform float in [0,1) from crypto/rand. Used for
// scheduling fair draws (not security-sensitive, but no reason for weak
// sources anywhere in the process).
func randFloat64() float64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0.5
	}
	return float64(binary.BigEndian.Uint64(b[:])>>11) / float64(uint64(1)<<53)
}

// randIntn returns a uniform int in [0,n). n <= 0 returns 0.
func randIntn(n int) int {
	if n <= 1 {
		return 0
	}
	return int(randFloat64() * float64(n))
}

// Shanghai is the billing/reset timezone for Chinese upstreams. China has no
// DST, so a fixed +08:00 zone is exact and avoids tzdata dependency.
var Shanghai = time.FixedZone("CST", 8*3600)

// ErrNoCapacity means every candidate account is cooling or disabled.
type ErrNoCapacity struct {
	Channel           string
	EarliestReset     time.Time
	HadCandidatesOnly bool // true when accounts exist but all in-flight full
}

func (e *ErrNoCapacity) Error() string {
	if e.HadCandidatesOnly {
		return fmt.Sprintf("pool: %s all accounts busy", e.Channel)
	}
	return fmt.Sprintf("pool: %s no available account (earliest reset %s)", e.Channel, e.EarliestReset.Format(time.RFC3339))
}

// ErrChannelBusy reports that the channel already has an account interacting
// and single-active mode forbids starting a second one. It is deliberately
// distinct from ErrNoCapacity: the pool is healthy and NOT waiting — the
// caller should route to the next binding immediately. When every binding
// answers this, the request fails as "parallel capacity exceeded" instead of
// queueing (2026-10-20).
type ErrChannelBusy struct {
	Channel string
	Account string // account currently interacting (label for the message)
}

func (e *ErrChannelBusy) Error() string {
	if e.Account != "" {
		return fmt.Sprintf("pool: %s is interacting as %s (one account per pool at a time)", e.Channel, e.Account)
	}
	return fmt.Sprintf("pool: %s is interacting (one account per pool at a time)", e.Channel)
}

// ErrModelGone reports that the upstream has explicitly said it does not
// serve this model on this channel, confirmed by several distinct accounts.
// The caller should move to the next binding: no account here can serve it,
// so retrying would only produce the same answer.
type ErrModelGone struct {
	Channel  string
	Model    string
	Accounts int
	Reason   string
}

func (e *ErrModelGone) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("pool: %s does not serve model %s (%s)", e.Channel, e.Model, e.Reason)
	}
	return fmt.Sprintf("pool: %s does not serve model %s", e.Channel, e.Model)
}

// Params tunes pool behaviour.
type Params struct {
	MaxQueueWait time.Duration // max wait for a slot when all busy (sticky queue)
	SessionTTL   time.Duration // sticky binding TTL
	CostTTL      time.Duration // cost observation validity
	// StickyGrace is how long after a rest ends an account is "sticky only":
	// it keeps the conversations already bound to it but takes no new ones
	// until it proves healthy. 0 disables the band.
	StickyGrace time.Duration
	// StickyFailThreshold marks an account sticky-only once it has failed this
	// many times in a row without resting yet. 0 disables the band.
	StickyFailThreshold int
	// GlobalMaxInFlight caps how many tasks one WorkBuddy international
	// (realm=global) account may run at once. That edge's risk control is
	// tighter, so a global account runs below the CN ceiling even when the
	// channel limit is higher. 0 disables the override. Default 2.
	GlobalMaxInFlight int
	// SingleActivePerChannel allows at most one account of a channel to be
	// interacting at any time (2026-10-20). A second request does NOT wait and
	// is not routed to a second account: it walks to the next binding. Using
	// several accounts of one pool simultaneously is the pattern upstream risk
	// control reads as unusual activity, and it is what got two accounts
	// restricted in the field. Default true.
	SingleActivePerChannel bool
	// GreedyFloor clamps the seconds-to-reset divisor in the greedy burn-rate
	// score (hot-settable; default 600s).
	GreedyFloor time.Duration
	// SoftCooldownBase is the soft-rate cooldown base before breaker doubling
	// (hot-settable; default 600s).
	SoftCooldownBase time.Duration
	// UpstreamBusyCooldown is the channel+model shared pause applied when the
	// upstream reports a capacity event (queue full, e.g. CodeBuddy 6021).
	// During the pause Acquire fails fast for that channel+model instead of
	// banging every account against the same full queue; the account state
	// machines are untouched. 0 disables the pause (still no rotation, still
	// no account penalty). Default 45s.
	UpstreamBusyCooldown time.Duration
	// NotFoundPause is the channel+model shared pause applied when the upstream	// answers 404 (the path or model does not exist there). It is NOT an
	// account state change: no cooldown, no failure count — the same request
	// would 404 on every account of that channel. The pause only stops a
	// request storm from hammering a dead route while the binding loop
	// cascades to another channel. 0 disables the pause (still no penalty).
	// Default 60s, matching the reference implementation's notFoundCooldown.
	NotFoundPause time.Duration
	// ModelRateHoldCap bounds a model-scoped rate-limit hold: the routing
	// barrier ends at min(upstream reset, now+cap), so a request arriving
	// after the cap probes upstream again instead of waiting out the full
	// upstream window (a 6004 can state hours). The upstream reset is kept in
	// ResetAt for the panel. Default 2h.
	ModelRateHoldCap time.Duration
	// EdgeBlockPause is the channel-wide pause applied when a CDN/WAF edge
	// refused a request (Cloudflare 1010 and kin). Edge blocks are
	// request-level: every account behind the same egress hits them, so the
	// pause goes on the channel and no account is charged (BP-94).
	// Default 60s; 0 disables the pause.
	EdgeBlockPause time.Duration
	Now            func() time.Time
}

// Pool is the account pool.
type Pool struct {
	mu          sync.Mutex
	params      Params
	accounts    map[string]*model.Account // by ID
	byChan      map[string][]string       // channel -> account IDs (stable order)
	sessions    map[string]sessionBinding // "channel|sessionKey" -> binding
	inflight    map[string]int
	waiters     map[string]chan struct{} // channel -> broadcast on release
	seq         uint64                   // monotonic pick sequence
	sinkSeq     uint64                   // monotonic sink order (BP-91)
	limits      map[string]int           // channel -> per-account in-flight limit
	onChange    func(*model.Account)     // serial state-only snapshots outside the pool lock
	pending     map[string]*model.Account
	persistWake chan struct{}
	persistDone chan struct{}
	// uses accumulates model usage per account since the last cost-snapshot
	// drain (the cost observer task classifies tiers from balance deltas).
	uses map[string][]string
	// lastExplore throttles tier-1 exploration per channel.
	lastExplore map[string]time.Time
	// globalEnabled gates workbuddy international (realm=global) accounts.
	// Default OFF: global accounts are invisible to scheduling until enabled.
	globalEnabled bool
	// busyUntil holds the channel|model -> deadline pause after an upstream
	// capacity event (queue full). It is channel-scoped shared state, fully
	// independent of per-account cooldowns; expired entries are skipped
	// lazily on read.
	busyUntil map[string]time.Time
	// deadModels records channel+model pairs the upstream explicitly said it
	// no longer serves, counted by DISTINCT accounts (see deadmodel.go).
	deadModels map[string]*deadModelVerdict
	// lastRestricted remembers the last upstream restriction per channel, for
	// the BP-146 escalation: two bans inside a window park the channel.
	lastRestricted map[string]time.Time
	closed         bool
}

type sessionBinding struct {
	accountID string
	lastUsed  time.Time

	// Cache evidence from the last answer (BP-95). A conversation is worth
	// keeping on its account only while what the vendor cached of it is worth
	// keeping — that is the whole reason to stay: the next turn reads the
	// prefix instead of paying for it again. Once the cache has gone cold, or
	// the vendor never reported caching anything, staying buys nothing and
	// costs the pool its load spreading.
	cacheKnown bool
	cacheRead  int // tokens the last answer read from the vendor's cache
	cacheWrite int // tokens it wrote to it
	answeredAt time.Time
}

// New builds a pool. limits maps channel -> per-account max in-flight.
func New(params Params, limits map[string]int, onChange func(*model.Account)) *Pool {
	if params.Now == nil {
		params.Now = time.Now
	}
	if params.MaxQueueWait <= 0 {
		params.MaxQueueWait = 120 * time.Second
	}
	if params.StickyGrace <= 0 {
		params.StickyGrace = 2 * time.Minute
	}
	if params.StickyFailThreshold <= 0 {
		params.StickyFailThreshold = 2
	}
	if params.GlobalMaxInFlight <= 0 {
		params.GlobalMaxInFlight = 2
	}
	if params.SessionTTL <= 0 {
		params.SessionTTL = 30 * time.Minute
	}
	if params.CostTTL <= 0 {
		params.CostTTL = 6 * time.Hour
	}
	if params.GreedyFloor <= 0 {
		params.GreedyFloor = 600 * time.Second
	}
	if params.SoftCooldownBase <= 0 {
		params.SoftCooldownBase = 600 * time.Second
	}
	if params.UpstreamBusyCooldown <= 0 {
		params.UpstreamBusyCooldown = 45 * time.Second
	}
	if params.NotFoundPause <= 0 {
		params.NotFoundPause = 60 * time.Second
	}
	if params.EdgeBlockPause <= 0 {
		params.EdgeBlockPause = 60 * time.Second
	}
	if params.ModelRateHoldCap <= 0 {
		params.ModelRateHoldCap = 2 * time.Hour
	}
	p := &Pool{
		params:         params,
		accounts:       map[string]*model.Account{},
		byChan:         map[string][]string{},
		sessions:       map[string]sessionBinding{},
		inflight:       map[string]int{},
		waiters:        map[string]chan struct{}{},
		limits:         limits,
		onChange:       onChange,
		uses:           map[string][]string{},
		lastExplore:    map[string]time.Time{},
		busyUntil:      map[string]time.Time{},
		deadModels:     map[string]*deadModelVerdict{},
		pending:        map[string]*model.Account{},
		lastRestricted: map[string]time.Time{},
		persistWake:    make(chan struct{}, 1),
		persistDone:    make(chan struct{}),
	}
	if onChange != nil {
		go p.persistLoop()
	}
	return p
}

// Upsert installs or replaces one account (used at boot and by the panel).
//
// BP-150: re-registering an account must not destroy what the pool has LEARNED
// about it. Onboarding paths build a fresh *model.Account from the upstream's
// answer — credentials only, no runtime state — and upsert it over the live
// one. Before this, that replaced the whole object: the credit high-water mark
// (BP-122), the cost tiers, the cooldowns and the failure counters all reset to
// zero, so an account that had once held 13000 points showed "12673/12673"
// after the next AutoClaw re-import — the historical peak the user watches for
// a top-up was simply gone.
//
// The merge rule is per field: the incoming object owns identity, credentials
// and the operator's switches; the existing object owns anything it OBSERVED
// (balances, windows, cost tiers, cooldown deadlines, failure counts). A field
// the caller explicitly set is never overwritten by the old one.
func (p *Pool) Upsert(acc *model.Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if prev, existed := p.accounts[acc.ID]; existed && prev != nil {
		mergeLearnedState(prev, acc)
		p.accounts[acc.ID] = acc
		return
	}
	p.accounts[acc.ID] = acc
	p.byChan[acc.Channel] = append(p.byChan[acc.Channel], acc.ID)
}

// mergeLearnedState copies the observations held by prev into next, for every
// field next left empty. Credentials and identity are never touched: those
// belong to the incoming registration.
func mergeLearnedState(prev, next *model.Account) {
	// Credit snapshot: take the whole block when the incoming one carries no
	// observation at all (a fresh registration), otherwise keep whichever is
	// newer — and always keep the higher peak, which is a high-water mark and
	// therefore monotonic by definition.
	if next.Credits.UpdatedAt.IsZero() && prev.Credits.UpdatedAt.IsZero() == false {
		next.Credits = prev.Credits
	} else if !next.Credits.UpdatedAt.IsZero() && !prev.Credits.UpdatedAt.IsZero() {
		peak, peakAt := prev.Credits.Peak, prev.Credits.PeakAt
		if next.Credits.Peak > peak {
			peak, peakAt = next.Credits.Peak, next.Credits.PeakAt
		}
		if next.Credits.UpdatedAt.Before(prev.Credits.UpdatedAt) {
			next.Credits = prev.Credits
		}
		next.Credits.Peak = peak
		next.Credits.PeakAt = peakAt
	}
	// Peak survives even when only one side knows it.
	if next.Credits.Peak == 0 && prev.Credits.Peak > 0 {
		next.Credits.Peak = prev.Credits.Peak
		next.Credits.PeakAt = prev.Credits.PeakAt
	}

	if len(next.Windows) == 0 && len(prev.Windows) > 0 {
		next.Windows = prev.Windows
	}
	if len(next.CostTiers) == 0 && len(prev.CostTiers) > 0 {
		next.CostTiers = prev.CostTiers
	}
	if len(next.ModelCooldowns) == 0 && len(prev.ModelCooldowns) > 0 {
		next.ModelCooldowns = prev.ModelCooldowns
	}
	// Cooldown deadlines only ever extend: a fresh registration must not
	// shorten a rest the pool decided on.
	if next.AccountCooldownUntil.IsZero() && !prev.AccountCooldownUntil.IsZero() {
		next.AccountCooldownUntil = prev.AccountCooldownUntil
		next.AccountCooldownReason = prev.AccountCooldownReason
		next.AccountCooldownBy = prev.AccountCooldownBy
	}
	if next.ConsecutiveFails == 0 {
		next.ConsecutiveFails = prev.ConsecutiveFails
	}
	if next.BreakerFails == 0 {
		next.BreakerFails = prev.BreakerFails
	}
	if next.LastUsedAt.IsZero() {
		next.LastUsedAt = prev.LastUsedAt
	}
	if next.RateLimitedAt.IsZero() {
		next.RateLimitedAt = prev.RateLimitedAt
	}
	// The operator's switches survive a silent re-registration: a fresh struct
	// carries Disabled/Paused == false, which would silently re-enable an
	// account someone turned off. A caller that MEANS to flip them sets the
	// field through the panel's toggle endpoint, not through a re-import.
	if prev.Disabled {
		next.Disabled = true
	}
	if prev.Paused {
		next.Paused = true
	}
	// UpdatedAt orders persistence writes: keep the later one so the merge is
	// not mistaken for an older state and skipped by SaveAccountState.
	if next.UpdatedAt.Before(prev.UpdatedAt) {
		next.UpdatedAt = prev.UpdatedAt
	}
}

// Remove drops an account and its bindings.
func (p *Pool) Remove(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	acc, ok := p.accounts[id]
	if !ok {
		return
	}
	delete(p.accounts, id)
	delete(p.pending, id)
	ids := p.byChan[acc.Channel]
	for i, v := range ids {
		if v == id {
			p.byChan[acc.Channel] = append(ids[:i], ids[i+1:]...)
			break
		}
	}
	for k, b := range p.sessions {
		if b.accountID == id {
			delete(p.sessions, k)
		}
	}
}

// Accounts returns a snapshot slice (pointers are live; callers must not
// mutate scheduling fields without taking Lock).
func (p *Pool) Accounts(channel string) []*model.Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accountsLocked(channel)
}

// HasAccounts reports whether the channel has at least one imported account.
// Routing uses this to skip empty bindings instead of holding each request
// for the fallback wait on a channel that could never serve it. O(1), lock
// held only for the map read.
func (p *Pool) HasAccounts(channel string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byChan[channel]) > 0
}

// AccountsLocked is Accounts for callers already holding the pool lock.
func (p *Pool) AccountsLocked(channel string) []*model.Account {
	return p.accountsLocked(channel)
}

func (p *Pool) accountsLocked(channel string) []*model.Account {
	ids := p.byChan[channel]
	out := make([]*model.Account, 0, len(ids))
	for _, id := range ids {
		if a, ok := p.accounts[id]; ok {
			out = append(out, a)
		}
	}
	return out
}

// Lock exposes the pool lock for panel snapshots that read account fields.
func (p *Pool) Lock()   { p.mu.Lock() }
func (p *Pool) Unlock() { p.mu.Unlock() }

func (p *Pool) limitFor(channel string) int {
	if n, ok := p.limits[channel]; ok && n > 0 {
		return n
	}
	return 2
}

// limitForAccount is limitFor with the per-realm override applied: WorkBuddy
// international accounts run at a lower ceiling than CN ones because that
// edge's risk control is tighter (misakano upstream keeps
// pool.max_in_flight_global=2 for exactly this reason, 2026-10-21).
func (p *Pool) limitForAccount(a *model.Account) int {
	limit := p.limitFor(a.Channel)
	if a.Realm == "global" && limit > p.params.GlobalMaxInFlight && p.params.GlobalMaxInFlight > 0 {
		return p.params.GlobalMaxInFlight
	}
	return limit
}

// floorBlockedLocked reports whether the credit floor holds the account out
// for this model: an exhausted balance blocks only models the account has
// been observed to bill (tier 2) — free/unknown stay eligible, and an
// unknown balance (no observation) never blocks (panel/state.go parity; the
// sticky path unbinds on the same predicate instead of serving a drained
// account until upstream rejects it). Callers must hold the lock.
func (p *Pool) floorBlockedLocked(a *model.Account, modelName string, now time.Time) bool {
	// Spendable, not Remain (BP-134): Qoder's plan pool reads 0 while the
	// add-on pack still holds credits; benching the account for observed-paid
	// models there threw away exactly the pool the daily check-in feeds.
	return modelName != "" && !a.Credits.UpdatedAt.IsZero() && a.Credits.Spendable() <= 0 &&
		costTierLocked(a, modelName, now, p.params.CostTTL) == 2
}

// earliestReset returns the earliest moment any account could take NEW traffic.
// Callers must hold the lock.
//
// It asks admissionAt rather than reading the cooldown fields directly (BP-215):
// a sticky-only account is not cooling but is still refused for a new
// conversation, and the old field-based answer said "now" for it — the client
// was told to retry at once, retried, and got the same refusal, so a two-minute
// sticky band read as a dead channel.
func (p *Pool) earliestReset(channel, modelName string, now time.Time) time.Time {
	var earliest time.Time
	for _, id := range p.byChan[channel] {
		a := p.accounts[id]
		if a == nil || a.Disabled || a.Paused {
			continue
		}
		adm, lift := p.admissionAt(a, modelName, now)
		if adm == AdmitNew {
			return now // somebody is usable now (was likely just released)
		}
		if !lift.After(now) {
			// Blocked with no self-lifting deadline (the realm switch, say):
			// it will not reset on its own, so it contributes no wait.
			continue
		}
		if earliest.IsZero() || lift.Before(earliest) {
			earliest = lift
		}
	}
	return earliest
}

// Acquire selects an account for (channel, model, sessionKey) and reserves one
// in-flight slot. It waits up to MaxQueueWait when candidates exist but are
// all busy (bounded queue); it fails fast with ErrNoCapacity when every
// candidate is cooling/disabled.
//
// NextAvailable is the pre-flight gate a router uses before committing to a
// binding: unlike Acquire it never waits. ok=false with a zero earliest means
// the block is structural (empty pool, every account disabled/paused, or the
// credit floor holds them out for this model); a non-zero earliest is the
// soonest known cooldown expiry. In-flight load is ignored on purpose — a busy
// pool is still worth queueing for, a cooling one is not.
func (p *Pool) NextAvailable(channel, modelName string) (bool, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.params.Now()
	// Channel+model pause (6021 queue-full, 404 dead route): the pre-flight
	// gate must see it too. Acquire already fails fast on it, but this
	// function is what the binding loop consults to decide whether to try the
	// binding at all — reporting "available" here made the gateway commit to
	// a paused route and only then discover the pause, which reads as a bogus
	// no-capacity failure instead of a short wait.
	if until, ok := p.channelBusyUntilLocked(channel, modelName, now); ok {
		return false, until
	}
	var earliest time.Time
	for _, id := range p.byChan[channel] {
		a := p.accounts[id]
		if a == nil || a.Disabled || a.Paused {
			continue
		}
		if a.Realm == "global" && !p.globalEnabled {
			continue
		}
		if p.floorBlockedLocked(a, modelName, now) {
			continue
		}
		// Ask admission, not the raw cooldown fields (BP-215): a sticky-only
		// account is not cooling, but it will refuse a new conversation, so
		// reporting it "available" made the binding loop commit to a route and
		// then discover the refusal — a bogus no-capacity failure instead of the
		// short wait that is actually happening.
		adm, lift := p.admissionAt(a, modelName, now)
		if adm == AdmitNew {
			return true, time.Time{}
		}
		if lift.After(now) && (earliest.IsZero() || lift.Before(earliest)) {
			earliest = lift
		}
	}
	return false, earliest
}

func (p *Pool) Acquire(ctx context.Context, channel, model, sessionKey string) (*Lease, error) {
	deadline := p.params.Now().Add(p.params.MaxQueueWait)
	for {
		lease, err, retryable := p.tryAcquire(channel, model, sessionKey)
		if lease != nil {
			return lease, nil
		}
		if !retryable {
			return nil, err
		}
		if p.params.Now().After(deadline) {
			return nil, &ErrNoCapacity{Channel: channel, HadCandidatesOnly: true}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		case <-p.notifyChan(channel):
		}
	}
}

// tryAcquire is one scheduling attempt. retryable=true means "all busy, worth
// waiting"; otherwise err carries the terminal reason.
// ChannelBusyUntil reports the active channel+model pause deadline, if any.
// Expired entries are dropped lazily on read. Callers may pass an empty
// modelName for the channel-wide (unscoped) pause.
func (p *Pool) ChannelBusyUntil(channel, modelName string, now time.Time) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.channelBusyUntilLocked(channel, modelName, now)
}

func (p *Pool) channelBusyUntilLocked(channel, modelName string, now time.Time) (time.Time, bool) {
	key := channel + "|" + modelName
	until, ok := p.busyUntil[key]
	if !ok {
		return time.Time{}, false
	}
	if !until.After(now) {
		delete(p.busyUntil, key) // lazy expiry
		return time.Time{}, false
	}
	return until, true
}

// markChannelBusyLocked records/extends the channel+model pause.
func (p *Pool) markChannelBusyLocked(channel, modelName string, until time.Time) {
	key := channel + "|" + modelName
	if cur, ok := p.busyUntil[key]; ok && cur.After(until) {
		return // keep the later deadline
	}
	p.busyUntil[key] = until
}

// poolHolderNickname returns the nickname of the account currently
// interacting on this channel (for the busy-refusal message). Callers must
// hold the lock; "-" when unknown.
func (p *Pool) poolHolderNickname(channel string) string {
	for _, id := range p.byChan[channel] {
		if a := p.accounts[id]; a != nil && p.inflight[id] > 0 {
			return a.Nickname
		}
	}
	return "-"
}

func (p *Pool) tryAcquire(channel, modelName, sessionKey string) (*Lease, error, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.params.Now()
	p.gcSessionsLocked(now)

	if len(p.byChan[channel]) == 0 {
		return nil, &ErrNoCapacity{Channel: channel}, false
	}

	// Dead model first: when the upstream has told two distinct accounts that
	// it does not serve this model, no account-level gate below is worth
	// consulting — the answer is the same for every account (2026-10-22).
	if dead, why, n := p.deadModelLocked(channel, modelName, now); dead {
		return nil, &ErrModelGone{Channel: channel, Model: modelName, Accounts: n, Reason: why}, false
	}

	// Single-active mode (2026-10-20): one account per pool may interact at a
	// time. A second request is NOT queued on this pool and NOT sent to another
	// account of the same pool — the caller routes to the next binding. This is
	// the guard against the multi-account-at-once pattern upstream risk control
	// treats as unusual activity.
	if a, busy := p.activeAccountLocked(channel); busy {
		return nil, &ErrChannelBusy{Channel: channel, Account: a.Nickname}, false
	}

	// Upstream capacity pause: while the channel+model pair is paused after a
	// queue-full event, fail fast instead of sending another request into the
	// same full queue (per-account state is untouched — this is a wait, not
	// a cooldown).
	if until, ok := p.channelBusyUntilLocked(channel, modelName, now); ok {
		log.Printf("pool: %s/%s paused until %s after an upstream capacity event; refusing without dispatch",
			channel, modelName, until.Format(time.RFC3339))
		return nil, &ErrNoCapacity{Channel: channel, EarliestReset: until, HadCandidatesOnly: true}, false
	}

	// 1. Sticky binding: a bound conversation keeps its account. The binding
	// resolves from the session itself, so a later higher greed score or a
	// weight change never moves it (AIClient2API 7a7a3b6 semantics). Unbind
	// only when the account is truly unusable for this model (disabled/
	// cooling/realm-invisible/credit-floor-blocked); a healthy-but-full
	// account is waited for, never silently swapped.
	if sessionKey != "" {
		key := channel + "|" + sessionKey
		if b, ok := p.sessions[key]; ok {
			a := p.accounts[b.accountID]
			// A bound conversation may continue while the account is
			// AdmitSticky (short grace after a rest); only AdmitNone
			// unbinds it. That keeps a mid-conversation account from being
			// swapped out just because its rest ended moments ago.
			switch {
			case a == nil,
				p.admissionLocked(a, modelName, now) == AdmitNone,
				p.floorBlockedLocked(a, modelName, now),
				// Staying is only worth it while the vendor's cache of this
				// conversation is still warm: that cache is the entire reason
				// to keep the conversation where it is (BP-95).
				!p.stickyWorthLocked(b, now):
				delete(p.sessions, key) // broken for this model: fall through and reselect
			case p.inflight[a.ID] < p.limitForAccount(a):
				b.lastUsed = now
				p.sessions[key] = b
				return p.leaseLocked(a, now), nil, false
			default:
				// Bound account healthy but full: hold the request for it
				// rather than drifting to another account (no-drift queue,
				// bounded by Acquire's deadline). The next tryAcquire round
				// re-checks: cooling/disabled unbinds and reselects.
				return nil, nil, true
			}
		}
	}

	// 2. Candidate selection by channel strategy. Single-active mode allows
	// the account already interacting to take MORE work (up to max_in_flight)
	// but forbids STARTING a second account — the "one account, 2-3 tasks"
	// principle (2026-10-21). A candidate with zero in-flight would be a
	// second account: exclude it while a holder exists.
	holderActive := false
	for _, id := range p.byChan[channel] {
		if p.inflight[id] > 0 && p.accounts[id] != nil {
			holderActive = true
			break
		}
	}
	var candidates []*model.Account
	for _, id := range p.byChan[channel] {
		a := p.accounts[id]
		if a == nil || a.Disabled || a.Paused {
			continue
		}
		if a.Realm == "global" && !p.globalEnabled {
			continue // workbuddy international switch is off (default)
		}
		// New conversations need full admission: an account that has just
		// left a rest, or that keeps failing, is refused here but still
		// serves the conversations already bound to it (AdmitSticky), which
		// never reach this loop.
		if p.admissionLocked(a, modelName, now) != AdmitNew {
			continue
		}
		if p.inflight[a.ID] >= p.limitForAccount(a) {
			continue // busy but healthy
		}
		if holderActive && p.params.SingleActivePerChannel && p.inflight[a.ID] == 0 {
			continue // idle account = a SECOND account on this pool: forbidden
		}
		candidates = append(candidates, a)
	}
	if len(candidates) == 0 {
		if holderActive && p.params.SingleActivePerChannel {
			// The holder exists but has no free slot (or is unusable for this
			// model). Refuse instead of queueing: the gateway walks to the
			// next pool, and only a full walk ends as 并行数量超发.
			return nil, &ErrChannelBusy{Channel: channel, Account: p.poolHolderNickname(channel)}, false
		}
		if p.hasHealthyButBusy(channel, modelName, now) {
			return nil, nil, true // bounded wait for a slot
		}
		until := p.earliestReset(channel, modelName, now)
		// Say WHY, not just "no account" (BP-215). The anonymous version of this
		// refusal is what made a healthy-but-sticky account look like a dead
		// channel: the line named no account and no reason, so the only way to
		// find the sticky band was to read the scheduler itself.
		log.Printf("pool: %s/%s has no account for a new conversation until %s (%s)",
			channel, modelName, until.Format(time.RFC3339), p.noCandidateReasonLocked(channel, modelName, now))
		return nil, &ErrNoCapacity{Channel: channel, EarliestReset: until}, false
	}

	picked := p.pickLocked(channel, modelName, candidates, now)
	if picked == nil {
		return nil, &ErrNoCapacity{Channel: channel, EarliestReset: p.earliestReset(channel, modelName, now)}, false
	}

	if sessionKey != "" {
		p.sessions[channel+"|"+sessionKey] = sessionBinding{accountID: picked.ID, lastUsed: now}
	}
	return p.leaseLocked(picked, now), nil, false
}

// hasHealthyButBusy reports whether some account would be usable except for
// its in-flight limit. Callers must hold the lock.
func (p *Pool) hasHealthyButBusy(channel, modelName string, now time.Time) bool {
	for _, id := range p.byChan[channel] {
		a := p.accounts[id]
		if a == nil || a.Disabled || a.Paused {
			continue
		}
		if now.Before(a.AccountCooldownUntil) {
			continue
		}
		if mc, ok := a.ModelCooldowns[modelName]; ok && now.Before(mc.Until) {
			continue
		}
		if p.inflight[a.ID] >= p.limitForAccount(a) {
			return true
		}
	}
	return false
}

// noCandidateReasonLocked explains in one line why no account was offered for
// a new conversation. It exists because the refusal used to be anonymous — the
// gateway printed "no available account" per request while every account was
// healthy and idle, and the sticky-only band that was actually holding them out
// could only be found by reading this file (BP-215). Callers must hold the lock.
func (p *Pool) noCandidateReasonLocked(channel, modelName string, now time.Time) string {
	var parts []string
	for _, id := range p.byChan[channel] {
		a := p.accounts[id]
		if a == nil {
			continue
		}
		switch {
		case a.Disabled:
			parts = append(parts, a.Nickname+": disabled")
			continue
		case a.Paused:
			parts = append(parts, a.Nickname+": paused")
			continue
		case a.Realm == "global" && !p.globalEnabled:
			parts = append(parts, a.Nickname+": international realm switched off")
			continue
		}
		adm, lift := p.admissionAt(a, modelName, now)
		switch adm {
		case AdmitNone:
			parts = append(parts, fmt.Sprintf("%s: cooling until %s", a.Nickname, lift.Format(time.RFC3339)))
		case AdmitSticky:
			parts = append(parts, fmt.Sprintf("%s: sticky-only %.0fs more (breaker %d, no new conversations)",
				a.Nickname, lift.Sub(now).Seconds(), a.BreakerFails))
		default:
			parts = append(parts, fmt.Sprintf("%s: full (%d/%d in flight)",
				a.Nickname, p.inflight[a.ID], p.limitForAccount(a)))
		}
	}
	if len(parts) == 0 {
		return "no accounts"
	}
	return strings.Join(parts, ", ")
}

// leaseLocked reserves an in-flight slot and builds the lease. Callers must
// hold the lock.
func (p *Pool) leaseLocked(a *model.Account, now time.Time) *Lease {
	p.inflight[a.ID]++
	p.seq++
	a.LastPickSeq = p.seq
	a.LastUsedAt = now
	p.changedLocked(a)
	return &Lease{Account: a, pool: p}
}

// Release returns the slot. Safe to call once per lease; further calls no-op.
func (p *Pool) Release(l *Lease) {
	if l == nil || l.released {
		return
	}
	l.released = true
	p.mu.Lock()
	if p.inflight[l.Account.ID] > 0 {
		p.inflight[l.Account.ID]--
	}
	ch := l.Account.Channel
	p.mu.Unlock()
	p.broadcast(ch)
}

// InFlight reports the current in-flight count of an account (panel/debug).
func (p *Pool) InFlight(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflight[id]
}

// InFlightLocked is InFlight for callers already holding the pool lock.
func (p *Pool) InFlightLocked(id string) int {
	return p.inflight[id]
}

// activeAccountLocked reports whether single-active mode currently forbids
// starting work on this channel. The rule (2026-10-20 user principle, clarified
// 2026-10-21) is: ONE account may interact per pool at a time, and THAT
// ACCOUNT may run 2-3 tasks concurrently (max_in_flight still bounds it) —
// a SECOND ACCOUNT is what is forbidden, because multi-account concurrency in
// one pool is the pattern upstream risk control reads as unusual activity.
// Callers must hold the lock.
func (p *Pool) activeAccountLocked(channel string) (*model.Account, bool) {
	if !p.params.SingleActivePerChannel {
		return nil, false
	}
	// The account that holds the pool: anyone already interacting.
	var holder *model.Account
	for _, id := range p.byChan[channel] {
		if p.inflight[id] > 0 {
			if a := p.accounts[id]; a != nil {
				holder = a
				break
			}
		}
	}
	if holder == nil {
		return nil, false
	}
	// The pool is open while the holder has spare in-flight slots: the next
	// request (and its sticky session) may run ON THE SAME ACCOUNT. Only a
	// request that cannot be served by the holder is refused, so it walks to
	// the next pool instead of starting a second account here.
	if p.inflight[holder.ID] < p.limitForAccount(holder) {
		return nil, false
	}
	return holder, true
}

// ChannelBusy reports whether single-active mode currently forbids another
// request on this channel, and names the account that holds it. The gateway
// consults it before committing to a binding so a busy pool is skipped
// immediately instead of turning into a no-capacity wait (2026-10-20).
func (p *Pool) ChannelBusy(channel string) (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a, ok := p.activeAccountLocked(channel); ok {
		return true, a.Nickname
	}
	return false, ""
}

// SetGlobalEnabled toggles workbuddy international accounts. When off
// (default), realm=global accounts are invisible to scheduling; sticky
// sessions already bound to a global account are dropped immediately rather
// than lingering until their SessionTTL GC, so the next request rebinds
// within the visible (cn) pool.
func (p *Pool) SetGlobalEnabled(enabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.globalEnabled == enabled {
		return
	}
	p.globalEnabled = enabled
	if !enabled {
		for k, b := range p.sessions {
			if a := p.accounts[b.accountID]; a != nil && a.Realm == "global" {
				delete(p.sessions, k)
			}
		}
	}
}

// GlobalEnabled reports the switch state.
func (p *Pool) GlobalEnabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.globalEnabled
}

// SetGreedyFloor hot-updates the greedy burn-rate floor (new picks only).
func (p *Pool) SetGreedyFloor(d time.Duration) {
	if d <= 0 {
		return
	}
	p.mu.Lock()
	p.params.GreedyFloor = d
	p.mu.Unlock()
}

// SetSoftCooldownBase hot-updates the soft-rate cooldown base (applies to new
// cooldowns only; existing deadlines are untouched).
func (p *Pool) SetSoftCooldownBase(d time.Duration) {
	if d <= 0 {
		return
	}
	p.mu.Lock()
	p.params.SoftCooldownBase = d
	p.mu.Unlock()
}

// SetLimit hot-updates a channel's per-account in-flight limit. Queued
// waiters are re-evaluated immediately; in-flight requests are unaffected.
func (p *Pool) SetLimit(channel string, n int) {
	if channel == "" || n <= 0 {
		return
	}
	p.mu.Lock()
	p.limits[channel] = n
	p.mu.Unlock()
	p.broadcast(channel)
}

// SetWaitParams hot-updates queue/session timing knobs (panel settings).
// MaxQueueWait applies to new Acquire calls; SessionTTL to the next GC pass;
// CostTTL to the next tier lookup. Zero values keep the current setting.
func (p *Pool) SetWaitParams(maxQueueWait, sessionTTL, costTTL time.Duration) {
	p.mu.Lock()
	if maxQueueWait > 0 {
		p.params.MaxQueueWait = maxQueueWait
	}
	if sessionTTL > 0 {
		p.params.SessionTTL = sessionTTL
	}
	if costTTL > 0 {
		p.params.CostTTL = costTTL
	}
	p.mu.Unlock()
}

// ReleaseOnCredits ends a plan-entitlement cooldown when a check-in has
// demonstrably refilled the balance (BP-98 TRAE-8, agentpoints
// scheduler.ReenableIfCredits parity): check in → re-read credits →
// credits > 0 releases the rest, instead of letting a 1005-cooled account
// wait out its full 12h after the quota came back at midnight. A disabled
// account stays disabled — restriction is upstream's verdict, a cooldown is
// just our wait; a zero balance releases nothing.
func (p *Pool) ReleaseOnCredits(a *model.Account, remain int64) {
	if remain <= 0 || a == nil || a.Disabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.params.Now()
	if !now.Before(a.AccountCooldownUntil) {
		return // not cooling
	}
	a.AccountCooldownUntil = time.Time{}
	a.AccountCooldownReason = ""
	a.AccountCooldownBy = ""
	p.changedLocked(a)
}

// ApplyResult folds an upstream result into the account state machine. Call
// restAccount records an account-level rest together with WHY and WHAT set the
// deadline. Keeping the two beside the timestamp is what lets the panel say
// "rate limit, back at 21:05" instead of just "cooling down" (2026-10-22).
func (p *Pool) restAccount(a *model.Account, until time.Time, reason, by string, now time.Time) {
	if until.After(a.AccountCooldownUntil) {
		a.AccountCooldownUntil = until
		a.AccountCooldownReason = reason
		a.AccountCooldownBy = by
	}
}

func (p *Pool) ApplyResult(a *model.Account, model string, upstreamErr *uperr.Error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.params.Now()
	if upstreamErr == nil {
		a.ConsecutiveFails = 0
		a.BreakerFails = 0
		// It answered, so it is not a risk-control candidate any more: take it
		// out of the sunk set and let the rotation reach it again (BP-91).
		a.RateLimitedAt = time.Time{}
		if !now.Before(a.AccountCooldownUntil) {
			// The rest expired and the account is serving again: drop the
			// stale explanation so the panel does not show "rate limit" on a
			// healthy account.
			a.AccountCooldownReason = ""
			a.AccountCooldownBy = ""
		}
		a.UpdatedAt = now
		p.changedLocked(a)
		return
	}
	switch upstreamErr.Class {
	case uperr.ClassSoftRate:
		// BP-194 (magpie 87c68c9): a failure reported for an account that is
		// ALREADY resting must not raise its backoff ladders again — one
		// request walking its retries through the same account pushed seven
		// Codex accounts to the 10-minute cap in a single turn upstream. The
		// rest itself is still set below wherever the branch does so.
		alreadyResting := now.Before(a.AccountCooldownUntil)
		until := upstreamErr.ResetAt
		if until.IsZero() || !until.After(now) {
			until = now.Add(p.softRateBase(a.BreakerFails))
		}
		if upstreamErr.SharedQuota {
			// BP-213: a SHARED per-egress allowance (opencodezen's anonymous
			// free lane) has run out. The account is healthy and the wall is
			// not its own, so resting it until the stated reset turns one 429
			// into a channel-wide outage that outlives the failure (field
			// 2026-10-30: a single 429 rested the channel to 00:00 UTC and
			// every later request answered "cooling down" without reaching
			// upstream at all). Fall back to the ordinary short backoff and
			// let the next request re-probe: if the allowance really is gone
			// the upstream says so again in one round trip.
			// BP-215: without the breaker, and with the plain base backoff. The
			// ladder exists to make OUR cooling-in periods gentle towards an
			// account that keeps failing; a shared allowance has nothing to do
			// with this account's history, and letting a stale breaker stretch
			// the wait turns the base window into 40 minutes for a wall the
			// account cannot influence.
			until = now.Add(p.softRateBase(0))
			p.restAccount(a, until, "shared quota", "fixed", now)
			upstreamErr.HoldUntil = until
			p.sinkLocked(a, now)
			// BP-215: do NOT raise the breaker. This allowance belongs to the
			// egress, not to the account, so the account is not the party that
			// misbehaved; charging it here is what pushed opencodezen's one
			// account to breaker_fails=2, at which point admissionLocked
			// refused it new conversations forever (the counter clears only on
			// a success it could no longer reach).
			break
		}
		if upstreamErr.Model != "" {
			// Model-scoped soft rate: same hold semantics as model_rate (BP-49).
			h := p.setModelCooldownHard(a, upstreamErr.Model, upstreamErr.ResetAt, upstreamErr.ResetHard, "429 rate limit", upstreamErr.Message, now)
			upstreamErr.HoldUntil = h
		} else {
			by := "fixed"
			if !upstreamErr.ResetAt.IsZero() {
				by = "upstream"
			}
			p.restAccount(a, until, "rate limit", by, now)
		}
		// A rate limit with quota left means "busy", not "broken": keep the
		// account selectable but put it behind the ones that have not been
		// limited since (BP-91).
		p.sinkLocked(a, now)
		if !alreadyResting {
			a.BreakerFails++
		}
	case uperr.ClassModelRate:
		m := upstreamErr.Model
		if m == "" {
			m = model
		}
		reason := "model rate limit"
		if upstreamErr.Code != "" {
			reason = upstreamErr.Code + " model rate limit"
		}
		until := p.setModelCooldown(a, m, upstreamErr.ResetAt, reason, upstreamErr.Message, now)
		// Annotate the client-facing error with the gateway's own hold end so
		// 429 renderers and Retry-After can point at the next attempt.
		upstreamErr.HoldUntil = until
	case uperr.ClassHardCredit:
		// A channel whose allowances are counted per model (AutoClaw: one
		// route's free allowance running out says nothing about the account's
		// other routes — field-verified, one model answers 200 while another
		// answers 810000 at the same moment) can say so by setting Model. Then
		// only that model rests; the rest of the account keeps serving.
		//
		// Everything else keeps the old account-wide behaviour: a plain balance
		// is spent for every model at once.
		//
		// BP-116: a quota wall carrying a business CODE but naming no model now
		// rests the model of THIS request instead of the whole account. Field
		// evidence 2026-10-25: an AutoClaw account showed "0 points, cooling to
		// 04:00 next morning" whose only stated reason was a per-model 810000
		// wall that had expired three days earlier (zaicoding_glm-5.3, until
		// 10-23 14:40). The account-wide fallback had turned one route's
		// exhausted free allowance into "the whole account is broke until 4am".
		// An account-wide balance exhaustion arrives as a plain 402 with NO
		// business code, and still takes the account path below.
		//
		// BP-152: a driver that sets an explicit ResetAt on a hard-credit
		// wall owns the wall's scope and end (qoder 110/112 are account-level
		// usage counters, qoder2api-hub #108) — the request's model must NOT
		// be filled in, and the account rest must run to that ResetAt, not to
		// a blanket 04:00.
		if upstreamErr.Model == "" && upstreamErr.Code != "" && upstreamErr.ResetAt.IsZero() && model != "" {
			upstreamErr.Model = model
		}
		if upstreamErr.Model != "" {
			reason := "out of credit"
			if upstreamErr.Code != "" {
				reason = upstreamErr.Code + " model allowance used up"
			}
			until := p.setModelCooldown(a, upstreamErr.Model, time.Time{}, reason, upstreamErr.Message, now)
			upstreamErr.HoldUntil = until
			a.BreakerFails = 0
			break
		}
		until := NextDay4AM(now)
		if !upstreamErr.ResetAt.IsZero() && upstreamErr.ResetAt.After(now) {
			until = upstreamErr.ResetAt
		}
		p.restAccount(a, until, "out of credit", "upstream", now)
		a.BreakerFails = 0
	case uperr.ClassAuth:
		// Grace self-heal: short stop instead of permanent disable; the
		// refresh task will either fix it or the panel surfaces it.
		p.restAccount(a, now.Add(10*time.Minute), "auth failed", "fixed", now)
	case uperr.ClassAccountRestricted:
		// The upstream has restricted this account (workbuddy 403 code=11140
		// "request illegal" / "账号存在异常行为，已被限制使用"). No refresh can
		// fix it NOW, so the account leaves routing: keeping it selectable is
		// what made a pool of 3 accounts look dead while 1 was healthy
		// (sticky conversations stayed bound to the banned ones).
		//
		// BP-146: this account leaving is NOT permission to walk the rest of
		// the pool. The ban is per account (or per egress IP when several
		// accounts share one), so `uperr.Rotatable` refuses to try another
		// account of the same channel inside this request — Coding2API
		// measured one request leaving a risk record on three accounts, the
		// third banned three minutes later. A second restriction on this
		// channel inside RestrictedChannelWindow still pauses the whole
		// channel briefly: two bans in minutes means the pattern, not the
		// account, is what upstream is reacting to.
		//
		// BP-205 (user-observed 2026-10-29): the restriction is NOT terminal —
		// field history shows the account comes back in about a week. So the
		// account rests for 7 days instead of being disabled outright: the
		// panel shows the cooldown with its end time, the account rejoins
		// routing on its own, and a fresh hit while resting simply pushes the
		// wall out again (restAccount keeps the later of the two).
		p.restAccount(a, now.Add(7*24*time.Hour), "restricted by upstream", "fixed", now)
		a.Paused = false
		a.ConsecutiveFails = 0
		a.BreakerFails = 0
		if a.ModelCooldowns != nil {
			delete(a.ModelCooldowns, model)
		}
		p.noteRestrictedLocked(a.Channel, now, model)
	case uperr.ClassEdgeBlock:
		// A CDN/WAF edge refused the request before it reached the vendor:
		// request-level, not the account's. Pausing the channel briefly stops
		// a retry storm from re-hitting the same edge rule, and — unlike the
		// ClassWAF branch — no account is charged a strike, because every
		// account behind this egress hits it identically (BP-94).
		if d := p.params.EdgeBlockPause; d > 0 {
			p.markChannelBusyLocked(a.Channel, model, now.Add(d))
		}
		return
	case uperr.ClassWAF:
		p.restAccount(a, now.Add(60*time.Second), "edge blocked", "fixed", now)
	case uperr.ClassTransport:
		// A dropped connection (unexpected EOF / connection reset) is the
		// egress path or the vendor's own edge closing the socket — not a
		// property of the account. Field evidence 2026-10-23 on AutoClaw's CN
		// managed gateway: the same 100k-token request answered 200 in 2-4s on
		// 16 consecutive tries, yet 2 of the first 8 died with `unexpected
		// EOF` after ~30s. Charging the account a strike there produced a 5s
		// rest, and the expiry of that rest put the account into the 2-minute
		// sticky-only band (StickyGrace) — so ONE transient EOF cost TWO
		// MINUTES of "no available account on channel autoclaw" for every new
		// conversation, which is what made a healthy pool look dead.
		//
		// No account penalty: ClassTransport stays rotatable, so the attempt
		// loop immediately re-enters Acquire and retries the same (still
		// healthy) account, which is exactly the right answer for a dropped
		// socket. A genuinely unfixable egress failure is ClassTransportDead,
		// handled separately below.
		return
	case uperr.ClassTransportDead:
		// A transport failure that retrying cannot fix (dead proxy credential,
		// unresolvable host, refused connection). Every account of the channel
		// fails identically, so charging them one at a time just walks the pool
		// through an 80s backoff loop that can never succeed. Park the CHANNEL
		// briefly and leave the accounts untouched (BP-99).
		if d := p.params.EdgeBlockPause; d > 0 {
			p.markChannelBusyLocked(a.Channel, model, now.Add(d))
		}
		return
	case uperr.ClassBadRequest, uperr.ClassContentBlocked, uperr.ClassSentinel:
		// No account penalty: the account is fine, the payload was not.
		return
	case uperr.ClassNotFound:
		// The path/model does not exist upstream. Not an account problem, so
		// zero account penalty (previously this class did not exist and a 404
		// became model_rate: the account lost that model for up to 2h). Pause
		// the channel+model pair briefly so a storm of retries does not walk
		// every account into the same dead route; the binding loop still
		// cascades to another channel.
		// Positive evidence: the upstream named this model as absent. Two
		// distinct accounts agreeing promotes it to "the model is gone here",
		// which stops the pool from walking every remaining account into the
		// same dead end (2026-10-22).
		p.noteMissingModelLocked(a.Channel, model, a.ID, upstreamErr.Message, now)
		if d := p.params.NotFoundPause; d > 0 {
			p.markChannelBusyLocked(a.Channel, model, now.Add(d))
		}
		return
	case uperr.ClassUpstreamTimeout, uperr.ClassCanceled:
		// Timeout / cancellation is never the account's fault (every account
		// hits the same slow upstream; cancellations come from the client or
		// our own fallback window). Zero penalty — feeding ConsecutiveFails
		// here is how a healthy pool got cooled by its own slow requests
		// (ticket 27).
		return
	case uperr.ClassUpstreamBusy:
		// Upstream capacity event (queue full): the account is healthy, the
		// request is fine — penalizing accounts here is exactly the field
		// failure that cooled a whole pool in seconds. Instead, pause the
		// channel+model pair briefly so follow-up requests fail fast until
		// the upstream queue drains. Account state untouched.
		if d := p.params.UpstreamBusyCooldown; d > 0 {
			p.markChannelBusyLocked(a.Channel, upstreamErr.Model, now.Add(d))
		}
		return
	default:
		// BP-194: the same no-double-count guard the SoftRate branch has —
		// retries of one request against a resting account must not climb the
		// ladder the rest exists to enforce.
		if !now.Before(a.AccountCooldownUntil) {
			a.ConsecutiveFails++
		}
	}
	a.UpdatedAt = now
	p.changedLocked(a)
}

// RecordWindows updates quota windows from an upstream usage report.
func (p *Pool) RecordWindows(a *model.Account, windows map[string]model.Window) {
	if len(windows) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if a.Windows == nil {
		a.Windows = map[string]model.Window{}
	}
	for k, w := range windows {
		a.Windows[k] = w
	}
	a.UpdatedAt = p.params.Now()
	p.changedLocked(a)
}

// NoteModelUse records that an account served the given model. The cost
// observer drains these to classify billing tiers from balance deltas.
func (p *Pool) NoteModelUse(a *model.Account, modelName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a == nil || modelName == "" {
		return
	}
	last := p.uses[a.ID]
	if len(last) > 0 && last[len(last)-1] == modelName {
		return // collapse consecutive duplicates
	}
	if len(last) >= 16 {
		last = last[1:]
	}
	p.uses[a.ID] = append(last, modelName)
}

// DrainUses returns and clears the model-use log for an account.
func (p *Pool) DrainUses(id string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.uses[id]
	delete(p.uses, id)
	return out
}

// RecordCredits updates the credit snapshot (balance + expiring pressure).
func (p *Pool) RecordCredits(a *model.Account, c model.Credits) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = p.params.Now()
	}
	// Observe the billing tier from the balance delta. This is the only place
	// the truth is available for channels that do not report a per-request
	// charge (AutoClaw among them): a lower balance than the last snapshot means
	// the models used since then were paid for; an unchanged one means they were
	// free.
	//
	// Before this existed nothing ever wrote CostTiers outside the tests, so
	// costTierLocked always answered "unknown" and the floor guard —
	// "do not pick an account whose balance is spent" — could never fire for any
	// credit channel (BP-100).
	if !a.Credits.UpdatedAt.IsZero() && !c.UpdatedAt.Before(a.Credits.UpdatedAt) {
		prev, next := a.Credits.Remain, c.Remain
		tier := -1
		switch {
		case next < prev:
			tier = 2 // something was billed
		case next > prev:
			tier = -1 // a top-up, not a spend: says nothing about the models
		default:
			tier = 0 // the balance held: nothing was billed
		}
		if tier >= 0 {
			now := p.params.Now()
			for _, m := range p.uses[a.ID] {
				if m == "" {
					continue
				}
				if a.CostTiers == nil {
					a.CostTiers = map[string]model.CostObservation{}
				}
				a.CostTiers[m] = model.CostObservation{Tier: tier, ObservedAt: now}
			}
		}
	}
	// High-water mark (BP-122): every probe of a balance-only channel
	// overwrites Total with the current balance, so the panel can only ever
	// show "now". Keeping the peak is what makes a credit ARRIVAL visible —
	// a daily check-in (+400 on AutoClaw, +100 on Lobster/Qoder) raises the
	// balance above the previous peak exactly once, and the panel can then say
	// "峰值 N，M 分钟前" instead of leaving the operator to guess whether the
	// scheduled task actually paid out.
	//
	// The previous peak has to be read BEFORE the snapshot is replaced: the
	// incoming Credits comes straight from a driver probe and carries no
	// history.
	prevPeak, prevPeakAt := a.Credits.Peak, a.Credits.PeakAt
	a.Credits = c
	if c.Remain > prevPeak || prevPeak == 0 {
		a.Credits.Peak = c.Remain
		a.Credits.PeakAt = c.UpdatedAt
	} else {
		a.Credits.Peak = prevPeak
		a.Credits.PeakAt = prevPeakAt
	}
	a.UpdatedAt = p.params.Now()
	p.changedLocked(a)
}

// RecordCostTier records an observed billing tier for (account, model):
// used by the credit strategy. tier semantics: 0 free, 1 unknown, 2 paid.
func (p *Pool) RecordCostTier(a *model.Account, modelName string, tier int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a.CostTiers == nil {
		a.CostTiers = map[string]model.CostObservation{}
	}
	a.CostTiers[modelName] = model.CostObservation{Tier: tier, ObservedAt: p.params.Now()}
	p.changedLocked(a)
}

// CostTier returns the effective tier for (account, model) with TTL expiry.
// Unknown/expired observations read as tier 1.
func (p *Pool) CostTier(a *model.Account, modelName string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return costTierLocked(a, modelName, p.params.Now(), p.params.CostTTL)
}

// ModelCostTier reports the most favorable fresh observation for a model
// across all accounts: 0 (observed free) or 2 (observed paid). ok=false when
// no fresh observation exists — callers omit pricing metadata entirely rather
// than guess.
func (p *Pool) ModelCostTier(modelName string) (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.params.Now()
	best, found := 0, false
	for _, a := range p.accounts {
		if _, observed := a.CostTiers[modelName]; !observed {
			continue
		}
		t := costTierLocked(a, modelName, now, p.params.CostTTL)
		if t == 1 {
			continue // stale or unclassifiable observation
		}
		if !found || t < best {
			best, found = t, true
		}
	}
	return best, found
}

func costTierLocked(a *model.Account, modelName string, now time.Time, ttl time.Duration) int {
	obs, ok := a.CostTiers[modelName]
	if !ok {
		return 1
	}
	if ttl > 0 && now.Sub(obs.ObservedAt) > ttl {
		return 1
	}
	switch obs.Tier {
	case 0, 2:
		return obs.Tier
	default:
		return 1
	}
}

// ModelBlocked reports whether EVERY routable account of the channel is
// model-cooled for the given model — the client's remedy is switching
// models, not retrying (ticket 33, agentpoints 44d6e04). Returns the
// earliest unlock moment and the blocked account count. Routable excludes
// disabled/paused accounts and (when the switch is off) the global realm.
func (p *Pool) ModelBlocked(channel, modelName string) (blocked bool, until time.Time, count int) {
	if modelName == "" {
		return false, time.Time{}, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.params.Now()
	routable := 0
	for _, id := range p.byChan[channel] {
		a := p.accounts[id]
		if a == nil || a.Disabled || a.Paused {
			continue
		}
		if a.Realm == "global" && !p.globalEnabled {
			continue
		}
		routable++
		if mc, ok := a.ModelCooldowns[modelName]; ok && now.Before(mc.Until) {
			count++
			if until.IsZero() || mc.Until.Before(until) {
				until = mc.Until
			}
			continue
		}
		// Someone can serve this model — not a model-level block.
		return false, time.Time{}, 0
	}
	if count == 0 || routable == 0 {
		return false, time.Time{}, 0
	}
	return true, until, count
}

// ModelLockRow is one (channel, model) lock-out profile for the panel's
// model-lock view: still servable vs total routable accounts, how many are
// locked, and when they unlock (earliest / all).
type ModelLockRow struct {
	Channel       string    `json:"channel"`
	Model         string    `json:"model"`
	Total         int       `json:"total"`
	Locked        int       `json:"locked"`
	UnlockAt      time.Time `json:"unlock_at"`
	FullyUnlockAt time.Time `json:"fully_unlock_at"`
}

// lockKeySep separates channel and model in the ModelLockRows aggregation key.
// It is an escape sequence on purpose: a literal control character here is
// invisible in diffs and has silently broken the key before.
const lockKeySep = "\x1f"

// ModelLockRows aggregates every unexpired model-level cooldown into
// (channel, model) rows, ordered locked-first for the panel. Same routable
// scope as ModelBlocked (disabled/paused and, when off, global excluded).
func (p *Pool) ModelLockRows() []ModelLockRow {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.params.Now()
	routable := map[string]int{}
	type agg struct {
		locked  int
		unlock  time.Time
		fullyAt time.Time
	}
	rows := map[string]*agg{}
	for channel, ids := range p.byChan {
		for _, id := range ids {
			a := p.accounts[id]
			if a == nil || a.Disabled || a.Paused {
				continue
			}
			if a.Realm == "global" && !p.globalEnabled {
				continue
			}
			routable[channel]++
			for model, mc := range a.ModelCooldowns {
				if !now.Before(mc.Until) {
					continue
				}
				key := channel + lockKeySep + model
				ag := rows[key]
				if ag == nil {
					ag = &agg{}
					rows[key] = ag
				}
				ag.locked++
				if ag.unlock.IsZero() || mc.Until.Before(ag.unlock) {
					ag.unlock = mc.Until
				}
				if ag.fullyAt.IsZero() || mc.Until.After(ag.fullyAt) {
					ag.fullyAt = mc.Until
				}
			}
		}
	}
	if len(rows) == 0 {
		return nil
	}
	out := make([]ModelLockRow, 0, len(rows))
	for key, ag := range rows {
		sep := strings.IndexByte(key, '')
		if sep < 0 {
			continue
		}
		ch, model := key[:sep], key[sep+1:]
		out = append(out, ModelLockRow{
			Channel: ch, Model: model, Total: routable[ch], Locked: ag.locked,
			UnlockAt: ag.unlock, FullyUnlockAt: ag.fullyAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		li := out[i].Locked >= out[i].Total
		lj := out[j].Locked >= out[j].Total
		if li != lj {
			return li // fully-locked rows first
		}
		if out[i].Locked != out[j].Locked {
			return out[i].Locked > out[j].Locked
		}
		if out[i].Channel != out[j].Channel {
			return out[i].Channel < out[j].Channel
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// SetDisabled toggles the hard disable flag. Disabling is the stronger
// terminal state and clears Paused (no conflicting "paused" label on a
// disabled account — ticket 32, agentpoints fd835df parity).
func (p *Pool) SetDisabled(id string, disabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.accounts[id]; a != nil {
		a.Disabled = disabled
		if disabled {
			a.Paused = false
		}
		a.UpdatedAt = p.params.Now()
		p.changedLocked(a)
	}
}

// SetPaused toggles the paused flag (ticket 32): the account leaves routing
// (chat never picks it — selection, fallback and busy probes all skip it)
// but keep-alive tasks keep running: it still checks in, reports activity,
// refreshes tokens and probes balances. Returns false for unknown ids so
// callers can 404.
func (p *Pool) SetPaused(id string, paused bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.accounts[id]
	if a == nil {
		return false
	}
	a.Paused = paused
	a.UpdatedAt = p.params.Now()
	p.changedLocked(a)
	return true
}

// --- helpers ---

// setModelCooldown folds one model-scoped rate-limit event into the account.
// The routing barrier ends at min(upstream reset, now+ModelRateHoldCap): the
// cap lets a later request probe for an early recovery, while ResetAt keeps
// the upstream's own unlock moment for the panel. A zero/expired reset keeps
// the historical bounded 60s hold. Returns the effective barrier. Callers
// must hold the lock.
func (p *Pool) setModelCooldown(a *model.Account, m string, resetAt time.Time, reason, message string, now time.Time) time.Time {
	return p.setModelCooldownHard(a, m, resetAt, false, reason, message, now)
}

func (p *Pool) setModelCooldownHard(a *model.Account, m string, resetAt time.Time, resetHard bool, reason, message string, now time.Time) time.Time {
	if a.ModelCooldowns == nil {
		a.ModelCooldowns = map[string]model.ModelCooldown{}
	}
	until := resetAt
	// The 2h cap bounds OUR invented/guessed holds. An upstream-stated hard
	// window (ResetHard) is the quota bucket's own boundary and is honored
	// as-is (BP-135); premature unblocking there just re-hits the same 429.
	if cap := p.params.ModelRateHoldCap; cap > 0 && !resetHard {
		if c := now.Add(cap); until.IsZero() || c.Before(until) {
			until = c
		}
	}
	if until.IsZero() || !until.After(now) {
		until = now.Add(60 * time.Second)
	}
	if prev, ok := a.ModelCooldowns[m]; ok {
		if prev.Until.After(until) {
			until = prev.Until // an active hold is never shortened
		}
		if resetAt.IsZero() {
			resetAt = prev.ResetAt
		}
	}
	a.ModelCooldowns[m] = model.ModelCooldown{Until: until, ResetAt: resetAt, Reason: reason, Message: message}
	// Opportunistic cleanup of expired entries. Must use the pool's injected
	// clock (not wall time): with a test clock far from wall time, a wall
	// comparison would delete fresh cooldowns immediately.
	for k, v := range a.ModelCooldowns {
		if !v.Until.After(now) {
			delete(a.ModelCooldowns, k)
		}
	}
	return until
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// softRateBase is the 429 backoff base: the configured soft-cooldown base
// (default 600s) doubling per consecutive breaker failure, capped at 1h
// (panel parity). Callers must hold the lock.
func (p *Pool) softRateBase(breakerFails int) time.Duration {
	d := p.params.SoftCooldownBase
	for i := 0; i < breakerFails && d < time.Hour; i++ {
		d *= 2
	}
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

// NextDay4AM returns the next 04:00 in Asia/Shanghai strictly after now —
// the workbuddy credit reset moment.
func NextDay4AM(now time.Time) time.Time {
	local := now.In(Shanghai)
	y, m, d := local.Date()
	t := time.Date(y, m, d, 4, 0, 0, 0, Shanghai)
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t
}

func (p *Pool) notifyChan(channel string) <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch, ok := p.waiters[channel]
	if !ok {
		ch = make(chan struct{})
		p.waiters[channel] = ch
	}
	return ch
}

func (p *Pool) broadcast(channel string) {
	p.mu.Lock()
	ch := p.waiters[channel]
	p.waiters[channel] = make(chan struct{})
	p.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// gcSessionsLocked drops expired bindings.
func (p *Pool) gcSessionsLocked(now time.Time) {
	ttl := p.params.SessionTTL
	for k, b := range p.sessions {
		if now.Sub(b.lastUsed) > ttl {
			delete(p.sessions, k)
		}
	}
}

// cacheWorthTokens is how many tokens read from the vendor's cache (or written
// to it) make a conversation worth keeping where it is across turns. Below it,
// the prefix is too small for the cache to be paying for the stickiness.
const cacheWorthTokens = 1024

// cacheColdAfter is how long a vendor keeps a prompt cached without it being
// read: five minutes at Anthropic and at OpenAI, the shortest there is.
const cacheColdAfter = 5 * time.Minute

// stickyWorthLocked reports whether keeping this conversation on its account
// still buys anything (BP-95).
//
// The point of stickiness is the vendor's prefix cache: the next turn reads the
// conversation instead of paying for it again. Once that cache has gone cold,
// or the vendor never reported caching anything, staying costs the pool its
// load spreading and buys nothing.
//
// No evidence yet (the binding was made but no answer came back, or the
// upstream reports no cache accounting at all) counts as worth keeping: the
// old behaviour is the safe default, and a channel that never reports cache
// figures must not lose stickiness entirely.
func (p *Pool) stickyWorthLocked(b sessionBinding, now time.Time) bool {
	if !b.cacheKnown || b.answeredAt.IsZero() {
		return true
	}
	if b.cacheRead+b.cacheWrite < cacheWorthTokens {
		return false
	}
	return now.Sub(b.answeredAt) <= cacheColdAfter
}

// NoteSessionCache records what the last answer said about the vendor's prefix
// cache, so the next Acquire can tell whether staying on this account still
// buys a cache hit (BP-95). Channels that report no cache accounting pass
// known=false, which keeps stickiness exactly as it was.
func (p *Pool) NoteSessionCache(channel, sessionKey string, u ir.Usage) {
	if sessionKey == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := channel + "|" + sessionKey
	b, ok := p.sessions[key]
	if !ok {
		return
	}
	b.cacheKnown = u.CacheKnown
	b.cacheRead = u.CachedInputTokens
	// What this answer wrote to the vendor's cache. A turn that read nothing
	// but is the FIRST answer of the binding built the cache: what the vendor
	// now holds is the prompt that turn sent, so count it as written — that is
	// what the next turn stands to read (the reasoning magpie records as
	// "written counts as read"). A later turn that reads nothing is a miss:
	// the cache is not being reused, so staying buys nothing.
	b.cacheWrite = 0
	if b.answeredAt.IsZero() && u.CachedInputTokens == 0 {
		b.cacheWrite = u.InputTokens
	}
	b.answeredAt = p.params.Now()
	p.sessions[key] = b
}

// SessionBinding returns (accountID, ok) for tests and the panel.
func (p *Pool) SessionBinding(channel, sessionKey string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.sessions[channel+"|"+sessionKey]
	return b.accountID, ok
}

// pickLocked dispatches to the channel strategy. Callers must hold the lock.
//
// Before the strategy runs, accounts that were rate limited while still
// having quota sink behind those that were not (BP-91). Without this, an
// account whose rest just ended takes the very next request back — the load
// returns to the same account every time, which is exactly the pattern a
// vendor's risk control reads as a bot (magpie's Sink; same field finding as
// BP-79). Sinking is not a penalty: the account stays selectable, it just
// goes last, and the first answer clears the flag.
func (p *Pool) pickLocked(channel, modelName string, candidates []*model.Account, now time.Time) *model.Account {
	if len(candidates) > 1 {
		if ready := p.notSunkLocked(candidates, now); len(ready) > 0 && len(ready) < len(candidates) {
			candidates = ready
		}
	}
	switch channel {
	case model.ChannelAntigravity:
		return p.pickGreedyLocked(candidates, now)
	case model.ChannelWorkBuddy, model.ChannelTrae, model.ChannelZCode,
		// AutoClaw is a credit pool like the three above: it has a points
		// balance, expiring packs and a per-route allowance. Serving it by
		// least-recently-used ignored both the balance and the expiry pressure,
		// so an account about to run dry could be picked ahead of a full one
		// (BP-100).
		model.ChannelAutoClaw:
		return p.pickCreditLocked(modelName, candidates, now)
	default:
		return p.pickLRULocked(candidates)
	}
}

// sinkTTL bounds how long a rate-limit sinks an account. The flag is cleared
// by any successful answer, so the TTL only matters for an account that never
// gets picked again — without it, a pool where every account sank once would
// keep the ordering forever.
const sinkTTL = 30 * time.Minute

// notSunkLocked filters out accounts that sank after a rate limit, when at
// least one candidate has not. An empty result means every candidate sank;
// the caller then tries them all rather than refusing.
func (p *Pool) notSunkLocked(candidates []*model.Account, now time.Time) []*model.Account {
	out := make([]*model.Account, 0, len(candidates))
	for _, a := range candidates {
		if a.RateLimitedAt.IsZero() || now.Sub(a.RateLimitedAt) > sinkTTL {
			out = append(out, a)
		}
	}
	return out
}

// sinkLocked records that the upstream rate limited this account. Callers must
// hold the lock. Re-sinking moves it to the back of the sunk order.
func (p *Pool) sinkLocked(a *model.Account, now time.Time) {
	p.sinkSeq++
	a.RateLimitedAt = now
	a.SinkSeq = p.sinkSeq
}

// pickGreedyLocked ranks window-quota accounts by burn-rate greed:
// freshness -> weekly remaining/seconds-to-reset desc -> 5h remaining desc.
func (p *Pool) pickGreedyLocked(candidates []*model.Account, now time.Time) *model.Account {
	floor := p.params.GreedyFloor
	sort.SliceStable(candidates, func(i, j int) bool {
		return greedyLess(candidates[i], candidates[j], now, floor)
	})
	// 0.1s anti-collision: skip accounts picked within the gap when a fresh
	// alternative exists.
	for _, a := range candidates {
		if len(candidates) > 1 && sameInstantPick(a, now) {
			continue
		}
		return a
	}
	return candidates[0]
}

const antiCollisionGap = 100 * time.Millisecond

func sameInstantPick(a *model.Account, now time.Time) bool {
	return !a.LastUsedAt.IsZero() && now.Sub(a.LastUsedAt) < antiCollisionGap
}

// greedyLess orders greedy candidates. Freshness: accounts with known, recent
// windows rank above unknown; then weekly burn rate; then 5h remaining.
func greedyLess(a, b *model.Account, now time.Time, floor time.Duration) bool {
	fa, fb := freshnessRank(a, now), freshnessRank(b, now)
	if fa != fb {
		return fa < fb
	}
	ga, gb := weeklyGreedFloor(a, now, floor), weeklyGreedFloor(b, now, floor)
	if ga != gb {
		return ga > gb
	}
	ha, hb := windowFraction(a, "five_hour"), windowFraction(b, "five_hour")
	if ha != hb {
		return ha > hb
	}
	return a.ID < b.ID
}

const staleAfter = 20 * time.Minute

func freshnessRank(a *model.Account, now time.Time) int {
	w, ok := a.Windows["weekly"]
	if !ok || now.Sub(w.UpdatedAt) > staleAfter {
		return 1
	}
	if _, ok := a.Windows["five_hour"]; !ok {
		return 1
	}
	return 0
}

func windowFraction(a *model.Account, key string) float64 {
	w, ok := a.Windows[key]
	if !ok {
		return -1
	}
	return w.RemainingFraction
}

// WeeklyGreed is the burn-rate score with the default floor: remainingFraction
// / max(seconds to reset, 600). Higher = more quota about to expire unused.
func WeeklyGreed(a *model.Account, now time.Time) float64 {
	return weeklyGreedFloor(a, now, 600*time.Second)
}

// weeklyGreedFloor is WeeklyGreed with a hot-settable floor clamp.
func weeklyGreedFloor(a *model.Account, now time.Time, floor time.Duration) float64 {
	w, ok := a.Windows["weekly"]
	if !ok {
		return 0
	}
	f := floor.Seconds()
	if f <= 0 {
		f = 600
	}
	secs := w.ResetAt.Sub(now).Seconds()
	if secs < f {
		secs = f
	}
	return w.RemainingFraction / secs
}

// pickCreditLocked implements the credit-pool strategy, aligned with the
// upstream panel (1.13.0 pick.go): a floor guard that only blocks
// observed-paid models for an exhausted account, cost tier filtering (0 free
// > 1 unknown > 2 paid) with conditional ride-along exploration, weights of
// credits ratio + idle + expiring pressure, an equal-weight Fisher-Yates
// shuffle before Top-5 truncation, an anti-collision-aware weighted draw,
// and an LRU fallback by monotonic pick sequence when every shortlisted
// account is inside the anti-collision window.
func (p *Pool) pickCreditLocked(modelName string, candidates []*model.Account, now time.Time) *model.Account {
	// Floor guard: an exhausted balance blocks only models this account has
	// been observed to bill (tier 2). Free (tier 0) and unknown (tier 1)
	// models stay eligible — blocking tier 1 would deadlock an account that
	// can never re-learn its cost after observation expiry or a restart.
	// Unknown balance (no observation) never blocks. Panel parity.
	var floored []*model.Account
	for _, a := range candidates {
		if p.floorBlockedLocked(a, modelName, now) {
			continue
		}
		floored = append(floored, a)
	}
	if len(floored) == 0 {
		return nil
	}

	// Cost tier filter: keep only the best tier present, with periodic
	// exploration of the unknown tier so new free accounts get discovered
	// (panel parity: conditional ride-along exploration).
	byTier := map[int][]*model.Account{}
	best := 3
	for _, a := range floored {
		t := costTierLocked(a, modelName, now, p.params.CostTTL)
		byTier[t] = append(byTier[t], a)
		if t < best {
			best = t
		}
	}
	group := byTier[best]
	if len(group) == 0 {
		group = floored
	}
	if best == 0 && len(byTier[1]) > 0 {
		// All known accounts are observed-free but unknowns exist: sprinkle
		// exploration requests through the unknown tier to learn its cost,
		// throttled per channel so exploration stays a small fraction.
		chanKey := ""
		if len(group) > 0 {
			chanKey = group[0].Channel
		}
		last := p.lastExplore[chanKey]
		if (last.IsZero() || now.Sub(last) > 30*time.Minute) && randFloat64() < 0.10 {
			group = append(append([]*model.Account{}, group...), byTier[1]...)
			p.lastExplore[chanKey] = now
		}
	}

	// maxCredits anchors the credits term across the full tier-prefiltered
	// set (upstream parity: truncation ranking and the draw share one basis).
	var maxCredits int64
	for _, a := range floored {
		if s := a.Credits.Spendable(); s > maxCredits { // BP-134: both pools
			maxCredits = s
		}
	}

	// Weight: credits ratio (×10) + idle compensation + expiring pressure.
	type scored struct {
		a *model.Account
		w float64
	}
	items := make([]scored, 0, len(group))
	for _, a := range group {
		w := 1.0
		if spend := a.Credits.Spendable(); maxCredits > 0 && spend > 0 { // BP-134: both pools
			w += float64(spend) / float64(maxCredits) * 10
		}
		if a.LastUsedAt.IsZero() {
			// Never used: full idle score (upstream pick.go parity). Scoring
			// fresh imports at 0 lets the first account lucky enough to be
			// picked outrank every still-unused peer (idle > 0 the moment it
			// was used) and herd all traffic onto the early picks.
			w += 1.0
		} else if idle := now.Sub(a.LastUsedAt).Minutes(); idle > 0 {
			w += minFloat(idle, 30) / 30 // up to +1.0 for 30+ min idle
		}
		if a.Credits.Expiring > 0 {
			w *= 3 // burn expiring credits first (panel parity)
		}
		items = append(items, scored{a: a, w: w})
	}

	// Equal-weight shuffle before truncation: with more than five candidates
	// and tied weights, a real Fisher-Yates pass keeps Top-5 entry uniformly
	// random for tied accounts (the stable sort below preserves the shuffled
	// order within ties). A coin-flip comparator would be non-transitive.
	sort.SliceStable(items, func(i, j int) bool { return items[i].w > items[j].w })
	if len(items) > 5 {
		tie := false
		for i := 1; i < len(items); i++ {
			if items[i].w == items[i-1].w {
				tie = true
				break
			}
		}
		if tie {
			for i := len(items) - 1; i > 0; i-- {
				j := randIntn(i + 1)
				items[i], items[j] = items[j], items[i]
			}
			sort.SliceStable(items, func(i, j int) bool { return items[i].w > items[j].w })
		}
	}
	full := items // pre-truncation set for the LRU fallback below
	if len(items) > 5 {
		items = items[:5]
	}

	// Weighted random draw, skipping accounts inside the anti-collision gap.
	var total float64
	for _, it := range items {
		if len(items) > 1 && sameInstantPick(it.a, now) {
			continue
		}
		total += it.w
	}
	if total > 0 {
		r := randFloat64() * total
		for _, it := range items {
			if len(items) > 1 && sameInstantPick(it.a, now) {
				continue
			}
			r -= it.w
			if r <= 0 {
				return it.a
			}
		}
	}
	// Every shortlisted account sits inside the anti-collision window: fall
	// back to the least recently picked candidate across the full
	// tier-filtered set by monotonic pick sequence (LastPickSeq), never by
	// wall-clock comparison — Windows time.Now() ~0.5ms precision makes
	// burst comparisons tie, the upstream-documented single-account herding
	// root cause.
	oldest := full[0]
	for _, it := range full[1:] {
		if it.a.LastPickSeq < oldest.a.LastPickSeq {
			oldest = it
		}
	}
	return oldest.a
}

func (p *Pool) pickLRULocked(candidates []*model.Account) *model.Account {
	if len(candidates) == 0 {
		return nil
	}
	best := candidates[0]
	for _, a := range candidates[1:] {
		if a.LastUsedAt.Before(best.LastUsedAt) {
			best = a
		}
	}
	return best
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
