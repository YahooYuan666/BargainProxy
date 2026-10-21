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
	"sort"
	"strings"
	"sync"
	"time"

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

// Params tunes pool behaviour.
type Params struct {
	MaxQueueWait time.Duration // max wait for a slot when all busy (sticky queue)
	SessionTTL   time.Duration // sticky binding TTL
	CostTTL      time.Duration // cost observation validity
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
	// NotFoundPause is the channel+model shared pause applied when the upstream
	// answers 404 (the path or model does not exist there). It is NOT an
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
	Now              func() time.Time
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
	closed    bool
}

type sessionBinding struct {
	accountID string
	lastUsed  time.Time
}

// New builds a pool. limits maps channel -> per-account max in-flight.
func New(params Params, limits map[string]int, onChange func(*model.Account)) *Pool {
	if params.Now == nil {
		params.Now = time.Now
	}
	if params.MaxQueueWait <= 0 {
		params.MaxQueueWait = 120 * time.Second
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
	if params.ModelRateHoldCap <= 0 {
		params.ModelRateHoldCap = 2 * time.Hour
	}
	p := &Pool{
		params:      params,
		accounts:    map[string]*model.Account{},
		byChan:      map[string][]string{},
		sessions:    map[string]sessionBinding{},
		inflight:    map[string]int{},
		waiters:     map[string]chan struct{}{},
		limits:      limits,
		onChange:    onChange,
		uses:        map[string][]string{},
		lastExplore: map[string]time.Time{},
		busyUntil:   map[string]time.Time{},
		pending:     map[string]*model.Account{},
		persistWake: make(chan struct{}, 1),
		persistDone: make(chan struct{}),
	}
	if onChange != nil {
		go p.persistLoop()
	}
	return p
}

// Upsert installs or replaces one account (used at boot and by the panel).
func (p *Pool) Upsert(acc *model.Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, existed := p.accounts[acc.ID]
	p.accounts[acc.ID] = acc
	if !existed {
		p.byChan[acc.Channel] = append(p.byChan[acc.Channel], acc.ID)
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

// usableForModel reports whether the account can serve the model at all
// right now (not disabled, realm-visible, not cooling). In-flight load is
// deliberately excluded: a healthy-but-full account is waited for by its
// bound sessions rather than swapped (no-drift sticky). Callers must hold
// the lock.
func (p *Pool) usableForModel(a *model.Account, modelName string, now time.Time) bool {
	if a.Disabled || a.Paused {
		return false
	}
	if a.Realm == "global" && !p.globalEnabled {
		return false
	}
	if now.Before(a.AccountCooldownUntil) {
		return false
	}
	if mc, ok := a.ModelCooldowns[modelName]; ok && now.Before(mc.Until) {
		return false
	}
	return true
}

// floorBlockedLocked reports whether the credit floor holds the account out
// for this model: an exhausted balance blocks only models the account has
// been observed to bill (tier 2) — free/unknown stay eligible, and an
// unknown balance (no observation) never blocks (panel/state.go parity; the
// sticky path unbinds on the same predicate instead of serving a drained
// account until upstream rejects it). Callers must hold the lock.
func (p *Pool) floorBlockedLocked(a *model.Account, modelName string, now time.Time) bool {
	return modelName != "" && !a.Credits.UpdatedAt.IsZero() && a.Credits.Remain <= 0 &&
		costTierLocked(a, modelName, now, p.params.CostTTL) == 2
}

// earliestReset returns the earliest moment any account could become usable.
// Callers must hold the lock.
func (p *Pool) earliestReset(channel, modelName string, now time.Time) time.Time {
	var earliest time.Time
	for _, id := range p.byChan[channel] {
		a := p.accounts[id]
		if a == nil || a.Disabled || a.Paused {
			continue
		}
		t := a.AccountCooldownUntil
		if mc, ok := a.ModelCooldowns[modelName]; ok && mc.Until.After(t) {
			t = mc.Until
		}
		if t.IsZero() || !t.After(now) {
			return now // somebody is usable now (was likely just released)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
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
		if p.usableForModel(a, modelName, now) {
			return true, time.Time{}
		}
		t := a.AccountCooldownUntil
		if mc, ok := a.ModelCooldowns[modelName]; ok && mc.Until.After(t) {
			t = mc.Until
		}
		if t.After(now) && (earliest.IsZero() || t.Before(earliest)) {
			earliest = t
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
			if a == nil || !p.usableForModel(a, modelName, now) || p.floorBlockedLocked(a, modelName, now) {
				delete(p.sessions, key) // broken for this model: fall through and reselect
			} else if p.inflight[a.ID] < p.limitForAccount(a) {
				b.lastUsed = now
				p.sessions[key] = b
				return p.leaseLocked(a, now), nil, false
			} else {
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
		if now.Before(a.AccountCooldownUntil) {
			continue
		}
		if mc, ok := a.ModelCooldowns[modelName]; ok && now.Before(mc.Until) {
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
		return nil, &ErrNoCapacity{Channel: channel, EarliestReset: p.earliestReset(channel, modelName, now)}, false
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

// ApplyResult folds an upstream result into the account state machine. Call
// after every upstream attempt. Success clears failure counters.
func (p *Pool) ApplyResult(a *model.Account, model string, upstreamErr *uperr.Error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.params.Now()
	if upstreamErr == nil {
		a.ConsecutiveFails = 0
		a.BreakerFails = 0
		a.UpdatedAt = now
		p.changedLocked(a)
		return
	}
	switch upstreamErr.Class {
	case uperr.ClassSoftRate:
		until := upstreamErr.ResetAt
		if until.IsZero() || !until.After(now) {
			until = now.Add(p.softRateBase(a.BreakerFails))
		}
		if upstreamErr.Model != "" {
			// Model-scoped soft rate: same hold semantics as model_rate (BP-49).
			h := p.setModelCooldown(a, upstreamErr.Model, upstreamErr.ResetAt, "429 rate limit", upstreamErr.Message, now)
			upstreamErr.HoldUntil = h
		} else {
			a.AccountCooldownUntil = later(a.AccountCooldownUntil, until)
		}
		a.BreakerFails++
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
		a.AccountCooldownUntil = later(a.AccountCooldownUntil, NextDay4AM(now))
		a.BreakerFails = 0
	case uperr.ClassAuth:
		// Grace self-heal: short stop instead of permanent disable; the
		// refresh task will either fix it or the panel surfaces it.
		a.AccountCooldownUntil = later(a.AccountCooldownUntil, now.Add(10*time.Minute))
	case uperr.ClassAccountRestricted:
		// The upstream has restricted this account (workbuddy 403 code=11140
		// "request illegal" / "账号存在异常行为，已被限制使用"). No refresh can
		// fix it, so the account leaves routing entirely: keeping it selectable
		// is what made a pool of 3 accounts look dead while 1 was healthy
		// (sticky conversations stayed bound to the banned ones). Disabled is
		// the honest terminal state — the panel shows it and a human decides
		// whether to re-login. Rotatable stays true, so this request retries
		// another account of the same channel immediately.
		a.Disabled = true
		a.Paused = false
		a.ConsecutiveFails = 0
		a.BreakerFails = 0
		if a.ModelCooldowns != nil {
			delete(a.ModelCooldowns, model)
		}
	case uperr.ClassWAF:
		a.AccountCooldownUntil = later(a.AccountCooldownUntil, now.Add(60*time.Second))
	case uperr.ClassTransport:
		a.ConsecutiveFails++
		a.AccountCooldownUntil = later(a.AccountCooldownUntil, now.Add(backoffTransport(a.ConsecutiveFails)))
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
		a.ConsecutiveFails++
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
	a.Credits = c
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
	if a.ModelCooldowns == nil {
		a.ModelCooldowns = map[string]model.ModelCooldown{}
	}
	until := resetAt
	if cap := p.params.ModelRateHoldCap; cap > 0 {
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

// backoffTransport is the transport-failure backoff: 5s,10s,20s… capped 80s.
func backoffTransport(fails int) time.Duration {
	d := 5 * time.Second
	for i := 1; i < fails && d < 80*time.Second; i++ {
		d *= 2
	}
	if d > 80*time.Second {
		d = 80 * time.Second
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

func (p *Pool) gcSessionsLocked(now time.Time) {
	ttl := p.params.SessionTTL
	for k, b := range p.sessions {
		if now.Sub(b.lastUsed) > ttl {
			delete(p.sessions, k)
		}
	}
}

// SessionBinding returns (accountID, ok) for tests and the panel.
func (p *Pool) SessionBinding(channel, sessionKey string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.sessions[channel+"|"+sessionKey]
	return b.accountID, ok
}

// pickLocked dispatches to the channel strategy. Callers must hold the lock.
func (p *Pool) pickLocked(channel, modelName string, candidates []*model.Account, now time.Time) *model.Account {
	switch channel {
	case model.ChannelAntigravity:
		return p.pickGreedyLocked(candidates, now)
	case model.ChannelWorkBuddy, model.ChannelTrae, model.ChannelZCode:
		return p.pickCreditLocked(modelName, candidates, now)
	default:
		return p.pickLRULocked(candidates)
	}
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
		if a.Credits.Remain > maxCredits {
			maxCredits = a.Credits.Remain
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
		if maxCredits > 0 && a.Credits.Remain > 0 {
			w += float64(a.Credits.Remain) / float64(maxCredits) * 10
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
