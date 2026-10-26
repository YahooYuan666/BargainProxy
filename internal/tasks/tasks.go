// Package tasks implements the task center: scheduled and on-demand
// operations (checkin, streak butler, growth pipelines, token refresh,
// balance probing) with an idempotent run ledger in the store.
//
// Time discipline: all daily schedules use Asia/Shanghai wall clock; every
// run is recorded per (account, task, date) so restarts never repeat a
// completed daily task.
package tasks

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/autoclaw"
	"bargainproxy/internal/driver/lobster"
	"bargainproxy/internal/driver/qoder"
	"bargainproxy/internal/driver/trae"
	"bargainproxy/internal/driver/workbuddy"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/store"
	"bargainproxy/internal/uperr"
)

// Shanghai is the billing/reset timezone for Chinese upstreams.
var Shanghai = time.FixedZone("CST", 8*3600)

// Task type identifiers.
const (
	TaskCheckinWorkBuddy  = "checkin_workbuddy"
	TaskCheckinTrae       = "checkin_trae"
	TaskCheckinAutoClaw   = "checkin_autoclaw"
	TaskCheckinQoder      = "checkin_qoder"
	TaskCheckinLobster    = "checkin_lobster"
	TaskProbeAll          = "probe_all"
	TaskRefreshTokens     = "refresh_tokens"
	TaskGrowthWorkBuddy   = "growth_workbuddy"
	TaskActivityWorkBuddy = "activity_report_workbuddy"
	TaskTravelWorkBuddy   = "travel_workbuddy"
	TaskBlackcatWorkBuddy = "blackcat_workbuddy"
)

// TaskEnabledKey is the settings key for one task type's enable switch.
func TaskEnabledKey(taskType string) string { return "task_enabled:" + taskType }

// Settings keys for the panel-editable schedule (ticket 25: agentpoints
// 定时任务参数迁移; ticket 26 adds activity/travel hours).
const (
	keyLastFiredPrefix    = "task_last_fired_"
	keyCheckinHours       = "task_checkin_hours"
	keyGrowthHours        = "task_growth_hours"
	keyProbeMinutes       = "task_probe_minutes"
	keyGoogleProbeMinutes = "task_google_probe_minutes"
	keyRefreshMinutes     = "task_refresh_minutes"
	keyActivityHours      = "task_activity_hours"
	keyTravelHours        = "task_travel_hours"
	keyBlackcatHours      = "task_blackcat_hours"
)

// Scheduler runs tasks on demand and on a daily wall clock.
type Scheduler struct {
	gw           *gateway.Gateway
	st           store.Store
	pl           *pool.Pool
	log          *log.Logger
	logRetention time.Duration

	mu          sync.Mutex
	running     map[string]bool      // "task|account" in-flight guard
	taskEnabled map[string]bool      // task type -> timer trigger switch
	lastProbeAt map[string]time.Time // accountID -> last automatic probe (channel cadence)
	// accountGap is the pause between accounts inside one task run. Several
	// accounts hitting the task RPCs back-to-back is a risk-control signal
	// (the reference projects use ~0.8s between accounts and 1.05s between
	// report chunks); 0 disables (2026-10-20).
	accountGap time.Duration
	// accountSpread is the window over which automatic task runs spread their
	// accounts, and accountSpreadGap the least distance between two of them
	// (BP-96). Zero window keeps the evenly-spaced behaviour.
	accountSpread    time.Duration
	accountSpreadGap time.Duration

	// OpenCode Zen automation (BP-54/55; zen.go). Catalog sync only — probing
	// is a one-shot at boot plus panel-driven, never scheduled.
	zen            ZenRunner
	zenSyncMinutes int
	zenLastSync    time.Time

	// Panel-editable schedule (all guarded by mu).
	checkinHours   []int
	growthHours    []int
	activityHours  []int
	travelHours    []int
	blackcatHours  []int
	probeMin       int
	googleProbeMin int
	refreshMin     int

	// adoptSkipped: accountID -> day the buddy-adoption threshold was found
	// unmet (ticket 26: agentpoints adoptTried — don't re-bomb the upstream
	// with a doomed adopt every travel run).
	adoptSkipped map[string]string

	// includeDisabled: the keep-alive tasks (check-ins, activity report,
	// token refresh, balance probe) also run for DISABLED accounts — the
	// "no traffic" switch must not cut their only self-healing path (ticket
	// 32, agentpoints c9986f9). Routing stays unaffected. Travel and growth
	// keep skipping disabled accounts (agentpoints #110 boundary).
	includeDisabled bool

	// fired: task family -> last-fired slot id "YYYY-MM-DD HH" (ticket 31).
	// Persisted in settings so a restart inside the same slot does not re-run
	// a slot that already fired; a missed slot (sleep, delayed tick) still
	// runs late exactly once.
	fired     map[string]string
	stop      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
	loopWG    sync.WaitGroup
	ctx       context.Context
	cancel    context.CancelFunc
}

// New builds the scheduler.
func New(gw *gateway.Gateway, st store.Store, pl *pool.Pool, logger *log.Logger) *Scheduler {
	if logger == nil {
		logger = log.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Scheduler{ctx: ctx, cancel: cancel,
		gw: gw, st: st, pl: pl, log: logger,
		logRetention: 14 * 24 * time.Hour,
		running:      map[string]bool{},
		taskEnabled:  map[string]bool{},
		lastProbeAt:  map[string]time.Time{},
		accountGap:   800 * time.Millisecond,
		// Spread automatic runs over 5 minutes by default: enough that several
		// accounts of one pool never share a recognizable burst, small enough
		// that a 9:00 task is still a 9:00 task (BP-96).
		accountSpread:    5 * time.Minute,
		accountSpreadGap: 30 * time.Second,
		checkinHours:     []int{9, 21},
		growthHours:      []int{9},
		// agentpoints activity_hours default 10, travel_hours default 9,21.
		activityHours: []int{10},
		travelHours:   []int{9, 21},
		blackcatHours: []int{23},
		adoptSkipped:  map[string]string{},
		fired:         map[string]string{},
		probeMin:      5, googleProbeMin: 15, refreshMin: 60,
		zenSyncMinutes: 60, // OpenCode Zen sync cadence (panel-editable)
		stop:           make(chan struct{}),
	}
	for _, t := range s.TaskTypes() {
		s.taskEnabled[t] = true // default: everything on
	}
	return s
}

// SetLogRetention adjusts the request-log retention window.
func (s *Scheduler) SetLogRetention(d time.Duration) {
	if d > 0 {
		s.logRetention = d
	}
}

// SetAccountGap sets the pause between accounts inside one task run
// (config task_account_gap). 0 disables the pause.
func (s *Scheduler) SetAccountGap(d time.Duration) {
	s.mu.Lock()
	s.accountGap = d
	s.mu.Unlock()
}

// SetAccountSpread sets the per-account spread window and its minimum gap
// (config task_account_spread_minutes / ..._min_gap_seconds). 0 disables it,
// keeping the old evenly-spaced behaviour (BP-96).
func (s *Scheduler) SetAccountSpread(window, minGap time.Duration) {
	s.mu.Lock()
	s.accountSpread, s.accountSpreadGap = window, minGap
	s.mu.Unlock()
}

// spreadOffsets is the per-account wait for one automatic run (BP-96).
//
// Why not just an even gap: several accounts of one pool doing the same thing
// within a few seconds of each other is the most recognizable time shape there
// is (a field report logged ten CN accounts checking in inside ten seconds of
// 9:00). A fixed gap only turns "all at once" into "at a fixed beat", which is
// just as recognizable. Each slot here is derived from (task, account, date),
// so it is fixed for the day — a restart does not reshuffle it — and the slots
// are then pushed apart so no two accounts land closer than minGap.
//
// ids empty (a panel click) or a single account means no spreading at all.
func (s *Scheduler) spreadOffsets(taskType string, ids []string) []time.Duration {
	s.mu.Lock()
	window, minGap := s.accountSpread, s.accountSpreadGap
	s.mu.Unlock()
	if window <= 0 || len(ids) <= 1 {
		return nil
	}
	if minGap <= 0 {
		minGap = time.Second
	}
	// Slots live in the first (window - total*minGap) so pushing a collision
	// up can never run past the window.
	usable := window - time.Duration(len(ids))*minGap
	if usable <= 0 {
		// Too many accounts for the window: even spacing is all that fits.
		out := make([]time.Duration, len(ids))
		for i := range out {
			out[i] = time.Duration(i) * minGap
		}
		return out
	}
	today := time.Now().In(Shanghai).Format("2006-01-02")
	type slot struct {
		at time.Duration
		id string
	}
	slots := make([]slot, len(ids))
	for i, id := range ids {
		slots[i] = slot{at: time.Duration(fnv64a(taskType+"|"+id+"|"+today) % uint64(usable)), id: id}
	}
	sort.Slice(slots, func(i, j int) bool {
		if slots[i].at != slots[j].at {
			return slots[i].at < slots[j].at
		}
		return slots[i].id < slots[j].id
	})
	for i := 1; i < len(slots); i++ {
		if gap := slots[i].at - slots[i-1].at; gap < minGap {
			slots[i].at = slots[i-1].at + minGap
		}
	}
	out := make([]time.Duration, len(ids))
	for _, sl := range slots {
		for i, id := range ids {
			if id == sl.id {
				out[i] = sl.at
				break
			}
		}
	}
	return out
}

// accountIDsOf maps accounts to their ids, for the spread hash.
func accountIDsOf(accounts []*model.Account) []string {
	ids := make([]string, len(accounts))
	for i, a := range accounts {
		ids[i] = a.ID
	}
	return ids
}

// fnv64a is a small stable hash (FNV-1a, 64-bit). Not cryptographic: it only
// has to be the same value for the same input on every run.
func fnv64a(s string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// accountPause sleeps the configured inter-account gap. Call between accounts
// of the same run, never after the last one.
func (s *Scheduler) accountPause(ctx context.Context) {
	s.mu.Lock()
	d := s.accountGap
	s.mu.Unlock()
	if d <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// accountWait sleeps this account's share of the spread window, then the plain
// inter-account gap. offset comes from spreadOffsets; 0 means "no wait".
func (s *Scheduler) accountWait(ctx context.Context, offset time.Duration) {
	if offset > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(offset):
		}
	}
	s.accountPause(ctx)
}

// TaskTypes lists the runnable task types (panel contract).
func (s *Scheduler) TaskTypes() []string {
	return []string{
		TaskCheckinWorkBuddy, TaskGrowthWorkBuddy, TaskCheckinTrae,
		TaskCheckinAutoClaw, TaskCheckinQoder, TaskCheckinLobster,
		TaskActivityWorkBuddy, TaskTravelWorkBuddy, TaskBlackcatWorkBuddy,
		TaskProbeAll, TaskRefreshTokens,
	}
}

// TaskEnabled reports whether the timer trigger for a task type is on.
// Manual runs are never gated (explicit intent).
func (s *Scheduler) TaskEnabled(taskType string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	enabled, ok := s.taskEnabled[taskType]
	return !ok || enabled
}

// TaskEnabledStates snapshots the switches (panel contract).
func (s *Scheduler) TaskEnabledStates() map[string]bool {
	out := map[string]bool{}
	for _, t := range s.TaskTypes() {
		out[t] = s.TaskEnabled(t)
	}
	return out
}

// SetTaskEnabled flips one task type's timer switch and persists it.
func (s *Scheduler) SetTaskEnabled(taskType string, enabled bool) error {
	known := false
	for _, t := range s.TaskTypes() {
		if t == taskType {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("tasks: unknown task type %q", taskType)
	}
	val := "false"
	if enabled {
		val = "true"
	}
	if s.st != nil {
		if err := s.st.SetSetting(TaskEnabledKey(taskType), val); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.taskEnabled[taskType] = enabled
	s.mu.Unlock()
	return nil
}

// LoadSettings restores the per-task switches and the editable schedule from
// the settings table (defaults when absent or unparsable).
func (s *Scheduler) LoadSettings() {
	if s.st == nil {
		return
	}
	for _, t := range s.TaskTypes() {
		v, ok, err := s.st.GetSetting(TaskEnabledKey(t))
		if err != nil || !ok {
			continue
		}
		s.mu.Lock()
		s.taskEnabled[t] = v != "false"
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok, err := s.st.GetSetting(keyCheckinHours); err == nil && ok {
		if hours, err := parseHours(v); err == nil {
			s.checkinHours = hours
		}
	}
	if v, ok, err := s.st.GetSetting(keyGrowthHours); err == nil && ok {
		if hours, err := parseHours(v); err == nil {
			s.growthHours = hours
		}
	}
	if v, ok, err := s.st.GetSetting(keyProbeMinutes); err == nil && ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 1440 {
			s.probeMin = n
		}
	}
	if v, ok, err := s.st.GetSetting(keyGoogleProbeMinutes); err == nil && ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 1440 {
			s.googleProbeMin = n
		}
	}
	if v, ok, err := s.st.GetSetting(keyRefreshMinutes); err == nil && ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 1440 {
			s.refreshMin = n
		}
	}
	if v, ok, err := s.st.GetSetting(keyActivityHours); err == nil && ok {
		if hours, err := parseHours(v); err == nil {
			s.activityHours = hours
		}
	}
	if v, ok, err := s.st.GetSetting(keyTravelHours); err == nil && ok {
		if hours, err := parseHours(v); err == nil {
			s.travelHours = hours
		}
	}
	if v, ok, err := s.st.GetSetting(keyBlackcatHours); err == nil && ok {
		if hours, err := parseHours(v); err == nil {
			s.blackcatHours = hours
		}
	}
	if v, ok, err := s.st.GetSetting(KeyIncludeDisabledInTasks); err == nil && ok {
		s.includeDisabled = v != "false"
	}
	// Last-fired slots (ticket 31): a restart inside the same slot must not
	// re-run it; a slot missed while asleep still runs late exactly once.
	for _, fam := range taskFamilies {
		if v, ok, err := s.st.GetSetting(keyLastFiredPrefix + fam); err == nil && ok {
			s.fired[fam] = v
		}
	}
}

// TaskScheduleParams snapshots the editable schedule (panel contract).
func (s *Scheduler) TaskScheduleParams() model.TaskParams {
	s.mu.Lock()
	defer s.mu.Unlock()
	return model.TaskParams{
		CheckinHours:       append([]int(nil), s.checkinHours...),
		GrowthHours:        append([]int(nil), s.growthHours...),
		ActivityHours:      append([]int(nil), s.activityHours...),
		TravelHours:        append([]int(nil), s.travelHours...),
		BlackcatHours:      append([]int(nil), s.blackcatHours...),
		ProbeMinutes:       s.probeMin,
		GoogleProbeMinutes: s.googleProbeMin,
		RefreshMinutes:     s.refreshMin,
	}
}

// SetTaskScheduleParams validates, persists and hot-applies the schedule.
func (s *Scheduler) SetTaskScheduleParams(p model.TaskParams) error {
	if len(p.CheckinHours) == 0 {
		return fmt.Errorf("tasks: checkin_hours must not be empty")
	}
	if len(p.GrowthHours) == 0 {
		return fmt.Errorf("tasks: growth_hours must not be empty")
	}
	if len(p.ActivityHours) == 0 {
		return fmt.Errorf("tasks: activity_hours must not be empty")
	}
	if len(p.TravelHours) == 0 {
		return fmt.Errorf("tasks: travel_hours must not be empty")
	}
	if len(p.BlackcatHours) == 0 {
		return fmt.Errorf("tasks: blackcat_hours must not be empty")
	}
	for _, h := range append(append(append(append(append([]int{}, p.CheckinHours...), p.GrowthHours...), p.ActivityHours...), p.TravelHours...), p.BlackcatHours...) {
		if h < 0 || h > 23 {
			return fmt.Errorf("tasks: hour %d out of range 0-23", h)
		}
	}
	for _, m := range []struct {
		v int
		k string
	}{{p.ProbeMinutes, "probe_minutes"}, {p.GoogleProbeMinutes, "google_probe_minutes"}, {p.RefreshMinutes, "refresh_minutes"}} {
		if m.v < 1 || m.v > 1440 {
			return fmt.Errorf("tasks: %s must be 1-1440 minutes", m.k)
		}
	}
	if s.st != nil {
		if err := s.st.SetSetting(keyCheckinHours, formatHours(p.CheckinHours)); err != nil {
			return err
		}
		if err := s.st.SetSetting(keyGrowthHours, formatHours(p.GrowthHours)); err != nil {
			return err
		}
		if err := s.st.SetSetting(keyActivityHours, formatHours(p.ActivityHours)); err != nil {
			return err
		}
		if err := s.st.SetSetting(keyTravelHours, formatHours(p.TravelHours)); err != nil {
			return err
		}
		if err := s.st.SetSetting(keyBlackcatHours, formatHours(p.BlackcatHours)); err != nil {
			return err
		}
		for _, kv := range []struct {
			k string
			v int
		}{{keyProbeMinutes, p.ProbeMinutes}, {keyGoogleProbeMinutes, p.GoogleProbeMinutes}, {keyRefreshMinutes, p.RefreshMinutes}} {
			if err := s.st.SetSetting(kv.k, strconv.Itoa(kv.v)); err != nil {
				return err
			}
		}
	}
	s.mu.Lock()
	s.checkinHours = normalizeHours(p.CheckinHours)
	s.growthHours = normalizeHours(p.GrowthHours)
	s.activityHours = normalizeHours(p.ActivityHours)
	s.travelHours = normalizeHours(p.TravelHours)
	s.blackcatHours = normalizeHours(p.BlackcatHours)
	s.probeMin, s.googleProbeMin, s.refreshMin = p.ProbeMinutes, p.GoogleProbeMinutes, p.RefreshMinutes
	s.mu.Unlock()
	return nil
}

// normalizeHours sorts and deduplicates an hour list (parseHours without the
// parsing).
func normalizeHours(hours []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(hours))
	for _, h := range hours {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	sort.Ints(out)
	return out
}

// parseHours parses a comma/space separated hour list ("9,21") into a sorted,
// deduplicated 0-23 slice.
func parseHours(s string) ([]int, error) {
	seen := map[int]bool{}
	var out []int
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' || r == '，' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("tasks: bad hour %q", part)
		}
		if n < 0 || n > 23 {
			return nil, fmt.Errorf("tasks: hour %d out of range 0-23", n)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("tasks: empty hour list")
	}
	sort.Ints(out)
	return out, nil
}

func formatHours(hours []int) string {
	sorted := append([]int(nil), hours...)
	sort.Ints(sorted)
	parts := make([]string, len(sorted))
	for i, h := range sorted {
		parts[i] = strconv.Itoa(h)
	}
	return strings.Join(parts, ",")
}

func (s *Scheduler) hourIn(hours []int, h int) bool {
	for _, x := range hours {
		if x == h {
			return true
		}
	}
	return false
}

// checkinFanOutTasks is the single list of channels whose daily check-in the
// timer fires.
//
// It is a function returning a fresh slice rather than four inline calls
// because of BP-121: Lobster's driver has had a complete Checkin (the
// 每日积分礼, +100 credits/day) since BP-98, its task type was registered in
// TaskTypes(), and its per-account handler existed in RunTask — yet nothing
// ever called it from the timer, so task_runs holds ZERO rows for
// checkin_lobster and the balance could only fall. Adding a channel meant
// remembering an invisible fourth edit; the list makes it visible, and a test
// pins that every registered checkin task is scheduled.
func checkinFanOutTasks() []string {
	return []string{
		TaskCheckinWorkBuddy,
		TaskCheckinTrae,
		TaskCheckinAutoClaw,
		TaskCheckinQoder,
		TaskCheckinLobster,
	}
}

// Start launches the background loop: daily checkins at the configured hours
// (default 09:05/21:05 Asia/Shanghai), probes every probe_minutes, token
// refresh every refresh_minutes — all panel-editable and hot-applied.
func (s *Scheduler) Start() {
	s.startOnce.Do(func() { s.loopWG.Add(1); go func() { defer s.loopWG.Done(); s.loop() }() })
}

// Stop terminates the loop.
func (s *Scheduler) Stop() { s.stopOnce.Do(func() { s.cancel(); close(s.stop) }); s.loopWG.Wait() }

func (s *Scheduler) loop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	lastProbe := time.Time{}
	lastRefresh := time.Time{}
	lastPurge := time.Time{}
	for {
		select {
		case <-s.stop:
			return
		case now := <-ticker.C:
			local := now.In(Shanghai)
			// Daily checkins at the panel-editable hours (+ a 5-minute guard
			// window past the hour to avoid the top-of-hour upstream rush).
			s.mu.Lock()
			checkinHours := append([]int(nil), s.checkinHours...)
			growthHours := append([]int(nil), s.growthHours...)
			activityHours := append([]int(nil), s.activityHours...)
			travelHours := append([]int(nil), s.travelHours...)
			blackcatHours := append([]int(nil), s.blackcatHours...)
			probeEvery := time.Duration(s.probeMin) * time.Minute
			refreshEvery := time.Duration(s.refreshMin) * time.Minute
			s.mu.Unlock()
			// Daily slots fire once each, whenever a tick observes them due —
			// a machine that slept through the slot or a tick delayed past the
			// exact minute still runs the latest missed slot late instead of
			// skipping the day (ticket 31; agentpoints 0e52af2 parity). The
			// (account, task, date) ledger keeps late runs idempotent.
			// Check-in at the panel-editable hours (+ a 5-minute guard window
			// past the hour to avoid the top-of-hour upstream rush).
			if id, _, ok := dueSlot(local, checkinHours, 5, s.lastFiredSlot("checkin")); ok {
				s.markFired("checkin", id)
				for _, taskType := range checkinFanOutTasks() {
					s.runScheduled(taskType, now)
				}
			}
			// Growth pipeline at the panel-editable hours (15-minute offset).
			if id, _, ok := dueSlot(local, growthHours, 15, s.lastFiredSlot("growth")); ok {
				s.markFired("growth", id)
				s.runScheduled(TaskGrowthWorkBuddy, now)
			}
			// Activity report at the panel-editable hours (25-minute offset —
			// agentpoints activity_hours default 10).
			if id, _, ok := dueSlot(local, activityHours, 25, s.lastFiredSlot("activity")); ok {
				s.markFired("activity", id)
				s.runScheduled(TaskActivityWorkBuddy, now)
			}
			// Cat-travel patrol at the panel-editable hours (35-minute offset —
			// agentpoints travel_hours default 9,21; one state-machine step per
			// run: adopt / depart / claim).
			if id, _, ok := dueSlot(local, travelHours, 35, s.lastFiredSlot("travel")); ok {
				s.markFired("travel", id)
				s.runScheduled(TaskTravelWorkBuddy, now)
			}
			// Night-owl (black_cat) at blackcat_hours (45-minute offset — the
			// default 23:45 sits inside the 23:00-08:00 scoring window). The
			// task itself re-checks the window and skips outside it.
			if id, _, ok := dueSlot(local, blackcatHours, 45, s.lastFiredSlot("blackcat")); ok {
				s.markFired("blackcat", id)
				s.runScheduled(TaskBlackcatWorkBuddy, now)
			}
			// Balance probing cadence follows both references: credit-pool
			// accounts every probe_minutes (agentpoints balance_refresh_minutes,
			// default 5), Google/antigravity every google_probe_minutes
			// (AIClient2API CRON_NEAR_MINUTES, default 15) — probeAll enforces
			// the per-channel split.
			if now.Sub(lastProbe) >= probeEvery {
				lastProbe = now
				s.runScheduled(TaskProbeAll, now)
			}
			if now.Sub(lastRefresh) >= refreshEvery {
				lastRefresh = now
				s.runScheduled(TaskRefreshTokens, now)
			}
			// OpenCode Zen catalog sync + probe (BP-54/55), cadence from the
			// panel settings.
			s.zenTick(now)
			// Retention: purge request logs AND task-ledger rows older than
			// the window, hourly (ticket 08 requires both to be cleaned).
			if now.Sub(lastPurge) >= time.Hour {
				lastPurge = now
				if n, err := s.st.PurgeRequestLogs(now.Add(-s.logRetention)); err == nil && n > 0 {
					s.log.Printf("tasks: purged %d expired request-log rows", n)
				}
				if n, err := s.st.PurgeTaskRuns(now.Add(-s.logRetention)); err == nil && n > 0 {
					s.log.Printf("tasks: purged %d expired task-ledger rows", n)
				}
			}
		}
	}
}

// taskFamilies are the daily slot families tracked for catch-up firing.
var taskFamilies = []string{"checkin", "growth", "activity", "travel", "blackcat"}

// slotID formats a (date, hour) slot identity: each configured hour fires at
// most once per day, and a late wake-up runs only the latest missed slot —
// the daily tasks are idempotent per (account, task, date) anyway.
func slotID(t time.Time, hour int) string {
	return fmt.Sprintf("%s %02d", t.Format("2006-01-02"), hour)
}

// dueSlot returns the latest configured hour whose fire time (h:minute local)
// has passed and which has not fired yet (ticket 31). Pure function: the
// ticker calls it every minute, so sleeping through a slot or a delayed tick
// runs the slot late instead of skipping it. ok=false when nothing is due.
func dueSlot(now time.Time, hours []int, minute int, lastFired string) (id string, hour int, ok bool) {
	best := -1
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, minute, 0, 0, now.Location())
		if !now.Before(t) && h > best {
			best = h
		}
	}
	if best < 0 {
		return "", 0, false
	}
	id = slotID(now, best)
	if id == lastFired {
		return "", 0, false
	}
	return id, best, true
}

// lastFiredSlot returns the last-fired slot id for a task family.
func (s *Scheduler) lastFiredSlot(family string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fired[family]
}

// markFired records the slot (in memory and in settings) before the run, so
// a slow or crashing run does not re-trigger on the next tick.
func (s *Scheduler) markFired(family, id string) {
	s.mu.Lock()
	s.fired[family] = id
	s.mu.Unlock()
	if s.st != nil {
		_ = s.st.SetSetting(keyLastFiredPrefix+family, id)
	}
}

// KeyIncludeDisabledInTasks is the settings key for the keep-alive-over-
// disabled-accounts switch (panel settings; ticket 32).
const KeyIncludeDisabledInTasks = "include_disabled_in_tasks"

// disabledKeepAliveEligible reports whether a daily task type runs for
// DISABLED accounts when the switch is on: check-ins, activity report (the
// keep-alive families). Travel and growth stay excluded — agentpoints #110
// drew the identical boundary.
func (s *Scheduler) disabledKeepAliveEligible(taskType string) bool {
	if !s.includeDisabledNow() {
		return false
	}
	switch taskType {
	case TaskCheckinWorkBuddy, TaskCheckinTrae, TaskCheckinAutoClaw, TaskCheckinQoder, TaskActivityWorkBuddy:
		return true
	default:
		return false
	}
}

func (s *Scheduler) includeDisabledNow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.includeDisabled
}

// SetIncludeDisabledInTasks hot-updates the switch (panel settings).
func (s *Scheduler) SetIncludeDisabledInTasks(v bool) {
	s.mu.Lock()
	s.includeDisabled = v
	s.mu.Unlock()
}

// runScheduled executes a timer-driven task unless its switch is off (manual
// panel runs are never gated). Returns true when the gate let it through.
func (s *Scheduler) runScheduled(taskType string, now time.Time) bool {
	if !s.TaskEnabled(taskType) {
		return false
	}
	s.runDaily(taskType, now)
	return true
}

// runDaily executes a daily task once, guarded by the run ledger.
func (s *Scheduler) runDaily(taskType string, now time.Time) {
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Minute)
	defer cancel()
	detail, err := s.RunTask(ctx, taskType, "")
	if err != nil {
		s.log.Printf("task %s: %v (%s)", taskType, err, detail)
		return
	}
	s.log.Printf("task %s: %s", taskType, detail)
}

// RunTask runs one task type, optionally scoped to one account. Returns a
// human-readable detail for the panel.
func (s *Scheduler) RunTask(ctx context.Context, taskType, accountID string) (string, error) {
	switch taskType {
	case TaskCheckinWorkBuddy:
		return s.runPerAccount(ctx, taskType, accountID, model.ChannelWorkBuddy, s.workBuddyWithAuthRetry(s.checkinWorkBuddy))
	case TaskGrowthWorkBuddy:
		return s.runPerAccount(ctx, taskType, accountID, model.ChannelWorkBuddy, s.workBuddyWithAuthRetry(s.growthWorkBuddy))
	case TaskActivityWorkBuddy:
		return s.runPerAccount(ctx, taskType, accountID, model.ChannelWorkBuddy, s.workBuddyWithAuthRetry(s.activityWorkBuddy))
	case TaskTravelWorkBuddy:
		return s.runPerAccount(ctx, taskType, accountID, model.ChannelWorkBuddy, s.workBuddyWithAuthRetry(s.travelWorkBuddy))
	case TaskBlackcatWorkBuddy:
		// A daytime manual run must not write a daily "ok" ledger row: the
		// (account, task, date) ledger would then skip the real 23:45 run.
		if !inNightWindow(time.Now()) {
			return "skipped (outside the 23:00-08:00 black_cat window)", nil
		}
		return s.runPerAccount(ctx, taskType, accountID, model.ChannelWorkBuddy, s.workBuddyWithAuthRetry(s.blackcatWorkBuddy))
	case TaskCheckinTrae:
		return s.runPerAccount(ctx, taskType, accountID, model.ChannelTrae, s.checkinTrae)
	case TaskCheckinAutoClaw:
		return s.runPerAccount(ctx, taskType, accountID, model.ChannelAutoClaw, s.checkinAutoClaw)
	case TaskCheckinQoder:
		return s.runPerAccount(ctx, taskType, accountID, model.ChannelQoder, s.checkinQoder)
	case TaskCheckinLobster:
		return s.runPerAccount(ctx, taskType, accountID, model.ChannelLobster, s.checkinLobster)
	case TaskProbeAll:
		return s.probeAll(ctx, accountID)
	case TaskRefreshTokens:
		return s.refreshTokens(ctx)
	default:
		return "", fmt.Errorf("tasks: unknown task type %q", taskType)
	}
}

// runPerAccount is the shared loop: iterate accounts of a channel (or one
// id), guard against concurrent runs, skip already-done daily tasks, record
// the ledger row.
func (s *Scheduler) runPerAccount(
	ctx context.Context, taskType, accountID, channel string,
	fn func(ctx context.Context, acc *model.Account) (string, error),
) (string, error) {
	accounts := s.accountsOf(channel)
	if accountID != "" {
		var filtered []*model.Account
		for _, a := range accounts {
			if a.ID == accountID {
				filtered = append(filtered, a)
			}
		}
		if len(filtered) == 0 {
			return "", fmt.Errorf("tasks: account %q not found on channel %s", accountID, channel)
		}
		accounts = filtered
	}
	if len(accounts) == 0 {
		return "no accounts on channel " + channel, nil
	}

	today := time.Now().In(Shanghai).Format("2006-01-02")
	var notes []string
	var firstErr error
	// Automatic runs spread their accounts over the spread window; an
	// account-targeted run (a panel click) never waits (BP-96).
	var offsets []time.Duration
	if accountID == "" {
		ids := make([]string, len(accounts))
		for i, a := range accounts {
			ids[i] = a.ID
		}
		offsets = s.spreadOffsets(taskType, ids)
	}
	for i, acc := range accounts {
		if offsets != nil {
			s.accountWait(ctx, offsets[i])
		}
		if acc.Disabled && !s.disabledKeepAliveEligible(taskType) {
			continue
		}
		// Idempotency: skip if this daily task already succeeded today
		// (account-scoped runs bypass the ledger so the panel can re-run).
		if accountID == "" {
			if run, err := s.st.TaskRunFor(acc.ID, taskType, today); err == nil && run.Status == "ok" && (taskType != TaskTravelWorkBuddy || strings.Contains(run.Detail, "adoption locked") || strings.Contains(run.Detail, "adopt skipped")) {
				continue
			}
		}
		key := taskType + "|" + acc.ID
		s.mu.Lock()
		if s.running[key] {
			s.mu.Unlock()
			continue
		}
		s.running[key] = true
		s.mu.Unlock()

		started := time.Now()
		detail, err := fn(ctx, acc)
		fin := time.Now()
		status := "ok"
		if err != nil {
			status = "failed"
			if firstErr == nil {
				firstErr = err
			}
			if detail == "" {
				detail = err.Error()
			}
		}
		_ = s.st.UpsertTaskRun(&model.TaskRun{
			AccountID: acc.ID, TaskType: taskType, RunDate: today,
			Status: status, Detail: detail, Credits: taskRunCredits(detail),
			StartedAt: started, FinishedAt: fin,
		})
		if status == "ok" {
			notes = append(notes, fmt.Sprintf("%s: %s", label(acc), detail))
		} else {
			notes = append(notes, fmt.Sprintf("%s: FAILED %s", label(acc), detail))
		}
		s.mu.Lock()
		delete(s.running, key)
		s.mu.Unlock()
	}
	if len(notes) == 0 {
		return "nothing to do (all skipped or already done)", nil
	}
	detail := strings.Join(notes, "; ")
	// Errors are informative, not fatal: accounts are independent. Report the
	// first failure so the panel can surface it while keeping the detail.
	return detail, firstErr
}

func (s *Scheduler) accountsOf(channel string) []*model.Account {
	accts := s.pl.Accounts(channel)
	sort.SliceStable(accts, func(i, j int) bool { return accts[i].ID < accts[j].ID })
	return accts
}

// taskRunCredits lifts the credit reward a task's detail states so it lands
// in TaskRun.Credits (BP-98 Qoder-7a): the check-in tasks phrase their
// reward as "+100 credits …" and the ledger column used to stay zero, so
// 签到收益 could not be tracked from task_runs alone. No stated reward → 0.
func taskRunCredits(detail string) int64 {
	const marker = "+"
	if !strings.HasPrefix(detail, marker) {
		return 0
	}
	end := strings.Index(detail, " credits")
	if end <= 1 {
		return 0
	}
	var n int64
	if _, err := fmt.Sscanf(detail[1:end], "%d", &n); err != nil {
		return 0
	}
	return n
}

func label(a *model.Account) string {
	if a.Nickname != "" {
		return a.Nickname
	}
	if a.UID != "" {
		return a.UID
	}
	return a.ID
}

// --- task implementations ---

// checkinWorkBuddy: official checkin + streak butler (redeem unlocked tiers
// and draw the lottery) — the panel's 连登管家 loop.
func (s *Scheduler) checkinWorkBuddy(ctx context.Context, acc *model.Account) (string, error) {
	if acc.Realm == "global" {
		return "skipped (global realm has no checkin)", nil
	}
	drv, ok := s.gw.Driver(model.ChannelWorkBuddy).(*workbuddy.Driver)
	if !ok {
		return "", fmt.Errorf("workbuddy driver not installed")
	}
	var parts []string
	if err := drv.Checkin(ctx, acc); err != nil {
		// Duplicate daily checkin is an idempotent success upstream-side
		// (code 10001/14001 "已签到"); recording it as FAILED used to poison
		// every repeat slot of the day (ticket 30).
		if workbuddy.IsAlreadyCheckin(err) {
			parts = append(parts, "already checked in today")
		} else {
			return "checkin failed: " + err.Error(), err
		}
	} else {
		parts = append(parts, "checkin ok")
	}
	// Streak butler: attempt redemption of every tier; locked tiers 403 and
	// are skipped silently (expected).
	redeemed := 0
	for _, tier := range []string{"7d", "14d", "28d"} {
		if err := drv.RedeemTier(ctx, acc, tier); err == nil {
			redeemed++
		}
	}
	if redeemed > 0 {
		parts = append(parts, fmt.Sprintf("redeemed %d streak tier(s)", redeemed))
	}
	// Lottery: draw all chances.
	if chances, err := drv.LotteryChances(ctx, acc); err == nil && chances > 0 {
		drawn := 0
		for i := 0; i < chances && i < 20; i++ {
			if err := drv.LotteryDraw(ctx, acc); err != nil {
				break
			}
			drawn++
		}
		parts = append(parts, fmt.Sprintf("lottery draws: %d/%d", drawn, chances))
	}
	// Refresh balance after the activity.
	if prober, ok := s.gw.Driver(model.ChannelWorkBuddy).(driver.Prober); ok {
		if res, err := prober.Probe(ctx, acc); err == nil {
			s.gw.ApplyProbe(acc, res)
			_ = s.st.SaveAccount(acc)
		}
	}
	return strings.Join(parts, ", "), nil
}

// workBuddyWithAuthRetry wraps a per-account WorkBuddy task with one forced
// token refresh + retry when the first attempt died from a dead credential.
//
// WorkBuddy access tokens live 60 days, so a real 401 is almost always a
// server-side revocation (signed in elsewhere, session killed) rather than
// expiry — the scheduled refresher skips it precisely because the local clock
// says the token is still valid. Without this, one revocation turns every
// workbuddy task into a permanently failing row until a manual re-login.
func (s *Scheduler) workBuddyWithAuthRetry(fn func(context.Context, *model.Account) (string, error)) func(context.Context, *model.Account) (string, error) {
	return func(ctx context.Context, acc *model.Account) (string, error) {
		note, err := fn(ctx, acc)
		if err == nil || !workbuddy.IsAuthDead(err) {
			return note, err
		}
		drv, ok := s.gw.Driver(model.ChannelWorkBuddy).(*workbuddy.Driver)
		if !ok {
			return note, err
		}
		if rerr := drv.RefreshNow(ctx, acc); rerr != nil {
			return note, fmt.Errorf("%w (token refresh also failed: %v)", err, rerr)
		}
		if acc.CredentialsDirty() {
			raw := acc.Credentials()
			if err := s.st.SaveAccount(acc); err != nil {
				return note, err
			}
			acc.CredentialsPersisted(raw)
		}
		s.log.Printf("task: %s 401 recovered by token refresh, retrying", label(acc))
		return fn(ctx, acc)
	}
}

// growthWorkBuddy runs the full growth pipeline and returns a step summary.
func (s *Scheduler) growthWorkBuddy(ctx context.Context, acc *model.Account) (string, error) {
	if acc.Realm == "global" {
		return "skipped (global realm has no growth center)", nil
	}
	drv, ok := s.gw.Driver(model.ChannelWorkBuddy).(*workbuddy.Driver)
	if !ok {
		return "", fmt.Errorf("workbuddy driver not installed")
	}
	results, pipelineErr := drv.RunGrowthPipeline(ctx, acc)
	if pipelineErr != nil && len(results) == 0 {
		return pipelineErr.Error(), pipelineErr
	}
	var okSteps, failed []string
	for _, r := range results {
		if r.OK {
			okSteps = append(okSteps, r.Step)
		} else {
			failed = append(failed, r.Step)
		}
	}
	detail := fmt.Sprintf("succeeded: %s", strings.Join(okSteps, ","))
	if len(failed) > 0 {
		detail += fmt.Sprintf("; failed/incomplete: %s", strings.Join(failed, ","))
	}
	// Refresh balance after the pipeline.
	if prober, isProber := s.gw.Driver(model.ChannelWorkBuddy).(driver.Prober); isProber {
		if res, err := prober.Probe(ctx, acc); err == nil {
			s.gw.ApplyProbe(acc, res)
			_ = s.st.SaveAccount(acc)
		}
	}
	if pipelineErr != nil {
		return detail, pipelineErr
	}
	if len(failed) > 0 {
		return detail, fmt.Errorf("growth steps failed or incomplete: %s", strings.Join(failed, ","))
	}
	return detail, nil
}

func (s *Scheduler) checkinTrae(ctx context.Context, acc *model.Account) (string, error) {
	drv, ok := s.gw.Driver(model.ChannelTrae).(*trae.Driver)
	if !ok {
		return "", fmt.Errorf("trae driver not installed")
	}
	checkedIn, credits, enable, err := drv.Checkin(ctx, acc)
	if err != nil {
		return "trae checkin failed: " + err.Error(), err
	}
	// Refresh balance, then release the cooldown the balance just disproved
	// (BP-98 TRAE-8): a 1005 plan-entitlement rest lifts as soon as the
	// check-in/probe shows credits came back — without this the account
	// waits out the full 12h while the quota sits at zero-balance-refilled.
	// A disabled account stays disabled (only cooldowns lift).
	if prober, ok := s.gw.Driver(model.ChannelTrae).(driver.Prober); ok {
		if res, err := prober.Probe(ctx, acc); err == nil {
			s.gw.ApplyProbe(acc, res)
			if res.Credits != nil {
				s.gw.Pool().ReleaseOnCredits(acc, res.Credits.Remain)
			}
			_ = s.st.SaveAccount(acc)
		}
	}
	switch {
	case checkedIn:
		return "already checked in today", nil
	case enable:
		return fmt.Sprintf("checkin ok (credits hint %d)", credits), nil
	default:
		// enable=false: the check-in program is off/unavailable for this
		// account — say so instead of pretending it was already checked in
		// (ticket 37).
		return "trae checkin not available (enable=false)", nil
	}
}

// checkinLobster claims the 每日积分礼 (+100 credits, one per day). The
// upstream reports an already-claimed day as state, not as an error, so a
// second scheduled run in the same day is a no-op rather than a failure.
func (s *Scheduler) checkinLobster(ctx context.Context, acc *model.Account) (string, error) {
	drv, ok := s.gw.Driver(model.ChannelLobster).(*lobster.Driver)
	if !ok {
		return "", fmt.Errorf("lobster driver not installed")
	}
	res, err := drv.Checkin(ctx, acc)
	if err != nil {
		// A dead access token must not fail the whole day's task: refresh once
		// and retry, same discipline as the other channels.
		if ue, ok := uperr.As(err); ok && (ue.Status == 401 || ue.Class == uperr.ClassAuth) {
			if rerr := drv.Refresh(ctx, acc); rerr == nil {
				_ = s.st.SaveAccount(acc)
				res, err = drv.Checkin(ctx, acc)
			}
		}
		if err != nil {
			return "lobster checkin failed: " + err.Error(), err
		}
	}
	// Refresh the balance so the panel shows the new credits immediately.
	if prober, ok := s.gw.Driver(model.ChannelLobster).(driver.Prober); ok {
		if pr, perr := prober.Probe(ctx, acc); perr == nil {
			s.gw.ApplyProbe(acc, pr)
			_ = s.st.SaveAccount(acc)
		}
	}
	if res.AlreadyClaimed {
		return fmt.Sprintf("already checked in today (day %d/%d)", res.ClaimedDays, res.TotalDays), nil
	}
	return fmt.Sprintf("checkin ok: +%d credits (day %d/%d, period %s)",
		res.CreditsGranted, res.ClaimedDays, res.TotalDays, res.PeriodKey), nil
}

func (s *Scheduler) checkinAutoClaw(ctx context.Context, acc *model.Account) (string, error) {
	drv, ok := s.gw.Driver(model.ChannelAutoClaw).(*autoclaw.Driver)
	if !ok {
		return "", fmt.Errorf("autoclaw driver not installed")
	}
	checkedIn, credits, enable, err := drv.Checkin(ctx, acc)
	if err != nil {
		// agentpoints recoverToken parity: a dead access token is refreshed
		// once and the checkin retried (401 must not fail the whole day's task).
		if ue, ok := uperr.As(err); ok && (ue.Status == 401 || ue.Class == uperr.ClassAuth) {
			if rerr := drv.Refresh(ctx, acc); rerr == nil {
				_ = s.st.SaveAccount(acc)
				checkedIn, credits, enable, err = drv.Checkin(ctx, acc)
			}
		}
	}
	if err != nil {
		return "autoclaw checkin failed: " + err.Error(), err
	}
	if prober, ok := s.gw.Driver(model.ChannelAutoClaw).(driver.Prober); ok {
		if res, perr := prober.Probe(ctx, acc); perr == nil {
			s.gw.ApplyProbe(acc, res)
			_ = s.st.SaveAccount(acc)
		}
	}
	switch {
	case checkedIn:
		return "already checked in today", nil
	case enable:
		return fmt.Sprintf("checkin ok (reward %d points)", credits), nil
	default:
		return "autoclaw checkin not available", nil
	}
}

func (s *Scheduler) checkinQoder(ctx context.Context, acc *model.Account) (string, error) {
	drv, ok := s.gw.Driver(model.ChannelQoder).(*qoder.Driver)
	if !ok {
		return "", fmt.Errorf("qoder driver not installed")
	}
	checkedIn, credits, enable, err := drv.Checkin(ctx, acc)
	if err != nil {
		return "qoder checkin failed: " + err.Error(), err
	}
	if prober, ok := s.gw.Driver(model.ChannelQoder).(driver.Prober); ok {
		if res, perr := prober.Probe(ctx, acc); perr == nil {
			s.gw.ApplyProbe(acc, res)
			_ = s.st.SaveAccount(acc)
		}
	}
	switch {
	case checkedIn:
		return "already checked in today", nil
	case enable:
		return fmt.Sprintf("checkin ok (+%d credits)", credits), nil
	default:
		return "qoder checkin not available", nil
	}
}

// activityWorkBuddy sends one chat-activity report (ticket 26, agentpoints
// activity_hours): lights the growth streak and unlocks the first_buddy
// adoption threshold. A streak read-back follows the report (agentpoints
// checkActivityStreak): upstream 200s are silently dropped for accounts with
// a missing userId, so the read-back is the only truth oracle.
func (s *Scheduler) activityWorkBuddy(ctx context.Context, acc *model.Account) (string, error) {
	// Global accounts DO report (BP-117). This gate used to skip them under the
	// belief that the international build has no activity system; the reference
	// project tested it and corrected exactly that assumption (their code says
	// so beside the same skip): /v2/report answers code=0 on workbuddy.ai and
	// lights the streak up. Skipping it means an international account can
	// never light its streak, and the report costs one small request per day.
	//
	// Enterprise accounts still skip: /v2/report returns 200 there too, but its
	// only effect — lighting the growth streak and unlocking first_buddy — is
	// 403 for enterprise accounts ("growth system is only available for
	// personal users"), so the report is zero-value and only costs a request.
	// EnterpriseID lives in the credential blob, not on the account row.
	if cr, err := workbuddy.ParseCreds(acc.CredsJSON); err == nil && cr.EnterpriseID != "" {
		return "skipped (enterprise account has no personal growth system)", nil
	}
	drv, ok := s.gw.Driver(model.ChannelWorkBuddy).(*workbuddy.Driver)
	if !ok {
		return "", fmt.Errorf("workbuddy driver not installed")
	}
	cid := fmt.Sprintf("bp-activity-%d", time.Now().UnixMilli())
	if err := drv.ReportChatActivity(ctx, acc, cid, "", "", ""); err != nil {
		return "activity report failed: " + err.Error(), err
	}
	note := "activity reported"
	if days, err := drv.StreakStatus(ctx, acc); err != nil {
		note += " (streak read-back failed)"
	} else if days == 0 {
		note += " (streak days=0 — possible silent drop)"
		s.log.Printf("activity %s: report OK but streak.days=0 (silent drop?)", label(acc))
	} else {
		note += fmt.Sprintf(", streak %dd", days)
	}
	return note, nil
}

// travelWorkBuddy runs one cat-travel patrol step (ticket 26, agentpoints
// travel_hours): no cat → adopt chain (preflight activity report → agreement
// → first); idle → depart; arrived → claim; traveling → skip. The adoption
// threshold failure is remembered for the day so the patrol doesn't re-bomb a
// doomed adopt every run.
func (s *Scheduler) travelWorkBuddy(ctx context.Context, acc *model.Account) (string, error) {
	if acc.Realm == "global" {
		return "skipped (global realm has no growth center)", nil
	}
	drv, ok := s.gw.Driver(model.ChannelWorkBuddy).(*workbuddy.Driver)
	if !ok {
		return "", fmt.Errorf("workbuddy driver not installed")
	}
	today := time.Now().In(Shanghai).Format("2006-01-02")
	s.mu.Lock()
	skipped := s.adoptSkipped[acc.ID] == today
	s.mu.Unlock()
	if skipped {
		return "adopt skipped earlier today (conversation threshold)", nil
	}
	hasBuddy, err := drv.HasBuddy(ctx, acc)
	if err != nil {
		return "buddy info failed: " + err.Error(), err
	}
	if !hasBuddy {
		today := time.Now().In(Shanghai).Format("2006-01-02")
		s.mu.Lock()
		skippedToday := s.adoptSkipped[acc.ID] == today
		s.mu.Unlock()
		if skippedToday {
			return "adopt skipped earlier today (conversation threshold)", nil
		}
		// Adoption unlock rides on the same activity report (agentpoints
		// travelAdopt preflight); travel runs are hours apart so a fresh
		// report here is within the one-per-day risk posture.
		if err := drv.ReportChatActivity(ctx, acc, fmt.Sprintf("bp-adopt-%d", time.Now().UnixMilli()), "", "", ""); err != nil {
			s.log.Printf("travel %s: adopt preflight report: %v", label(acc), err)
		} else {
			time.Sleep(1050 * time.Millisecond) // agentpoints adoptReportGap: let upstream settle the event
		}
		adopted, note, err := drv.BuddyAdopt(ctx, acc)
		if err != nil {
			return "adopt failed: " + err.Error(), err
		}
		if !adopted {
			s.mu.Lock()
			s.adoptSkipped[acc.ID] = today
			s.mu.Unlock()
			return "adopt: " + note, nil
		}
		return "adopt: " + note, nil
	}
	note, err := drv.TravelButler(ctx, acc)
	if err != nil {
		return "travel failed: " + err.Error(), err
	}
	return note, nil
}

// blackcatWorkBuddy runs the night-owl (black_cat) task: a few real glm-5.2
// chats plus one activity report each, only inside the 23:00-08:00 scoring
// window (agentpoints RunNightChats parity). Outside the window it skips — the
// scheduled slot defaults to 23:45, so the window guard also protects manual
// daytime runs from pointless upstream traffic.
func (s *Scheduler) blackcatWorkBuddy(ctx context.Context, acc *model.Account) (string, error) {
	if acc.Realm == "global" {
		return "skipped (global realm has no growth center)", nil
	}
	if acc.Paused {
		return "skipped (paused account)", nil
	}
	if !inNightWindow(time.Now()) {
		return "skipped (outside the 23:00-08:00 black_cat window)", nil
	}
	drv, ok := s.gw.Driver(model.ChannelWorkBuddy).(*workbuddy.Driver)
	if !ok {
		return "", fmt.Errorf("workbuddy driver not installed")
	}
	tasks, err := drv.GrowthTasks(ctx, acc)
	if err != nil {
		return "growth tasks failed: " + err.Error(), err
	}
	need := blackcatRemaining(tasks)
	if need <= 0 {
		return "black_cat already complete today", nil
	}
	// Safety cap: black_cat targets 3; a bogus upstream target must not turn
	// into an unbounded run of real model calls.
	capped := false
	if need > 10 {
		need, capped = 10, true
	}
	done := 0
	for i := 0; i < need; i++ {
		cid := fmt.Sprintf("bp-night-%d-%d", time.Now().UnixMilli(), i)
		req := &ir.ChatRequest{
			Model:  "glm-5.2",
			Stream: true,
			Messages: []ir.Message{{
				Role:    ir.RoleUser,
				Content: []ir.ContentPart{{Kind: "text", Text: "1+1等于几？直接回答。"}},
			}},
		}
		if _, err := drv.Stream(ctx, driver.ChatCall{Account: acc, UpstreamModel: "glm-5.2", Request: req}, func(ir.StreamEvent) error { return nil }); err != nil {
			return fmt.Sprintf("night chat %d/%d failed: %v", done+1, need, err), err
		}
		if err := drv.ReportChatActivity(ctx, acc, cid, "", "glm-5.2", "GLM-5.2"); err != nil {
			return fmt.Sprintf("night chat %d/%d report failed: %v", done+1, need, err), err
		}
		done++
		select {
		case <-ctx.Done():
			return fmt.Sprintf("night chats %d/%d (context done)", done, need), ctx.Err()
		case <-time.After(4 * time.Second):
		}
	}
	note := fmt.Sprintf("%d night chat(s) done", done)
	if capped {
		note += " (capped at 10)"
	}
	return note, nil
}

// inNightWindow reports whether t (Asia/Shanghai) is inside the black_cat
// scoring window: 23:00 through 07:59.
func inNightWindow(t time.Time) bool {
	h := t.In(Shanghai).Hour()
	return h >= 23 || h < 8
}

// blackcatRemaining returns how many more night chats black_cat needs; 0 when
// the task is absent, already complete, or has no usable target.
func blackcatRemaining(tasks []workbuddy.GrowthTask) int {
	for _, t := range tasks {
		if t.TaskCode != "black_cat" {
			continue
		}
		if t.Target <= 0 || t.Progress >= t.Target {
			return 0
		}
		return t.Target - t.Progress
	}
	return 0
}

// probeAll refreshes balances/windows for every probeable account.
func (s *Scheduler) probeAll(ctx context.Context, accountID string) (string, error) {
	channels := []string{
		model.ChannelAntigravity, model.ChannelWorkBuddy, model.ChannelTrae,
		model.ChannelZCode, model.ChannelAutoClaw, model.ChannelQoder,
		model.ChannelOpenAICompat, model.ChannelLobster,
	}
	probed, failed, skipped := 0, 0, 0
	var firstErr error
	s.mu.Lock()
	googleEvery := time.Duration(s.googleProbeMin) * time.Minute
	s.mu.Unlock()
	for _, ch := range channels {
		prober, ok := s.gw.Driver(ch).(driver.Prober)
		if !ok {
			continue
		}
		probeAccounts := s.accountsOf(ch)
		var probeOffsets []time.Duration
		if accountID == "" {
			ids := make([]string, len(probeAccounts))
			for i, a := range probeAccounts {
				ids[i] = a.ID
			}
			probeOffsets = s.spreadOffsets(TaskProbeAll, ids)
		}
		for i, acc := range probeAccounts {
			// Probes are upstream traffic too: spread them on automatic runs.
			if probeOffsets != nil {
				s.accountWait(ctx, probeOffsets[i])
			}
			if (acc.Disabled && !s.includeDisabledNow()) || (accountID != "" && acc.ID != accountID) {
				continue
			}
			// Channel cadence for automatic runs: Google every
			// google_probe_minutes (AIClient2API CRON_NEAR_MINUTES), credit
			// pools every loop tick (probe_minutes, agentpoints
			// balance_refresh_minutes). Explicit account-targeted runs
			// (manual按钮) always probe.
			if accountID == "" && ch == model.ChannelAntigravity {
				s.mu.Lock()
				last := s.lastProbeAt[acc.ID]
				s.mu.Unlock()
				if !last.IsZero() && time.Since(last) < googleEvery {
					skipped++
					continue
				}
			}
			res, err := prober.Probe(ctx, acc)
			if accountID == "" {
				s.mu.Lock()
				s.lastProbeAt[acc.ID] = time.Now()
				s.mu.Unlock()
			}
			if err != nil {
				failed++
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			s.gw.ApplyProbe(acc, res)
			_ = s.st.SaveAccount(acc)
			probed++
		}
	}
	msg := fmt.Sprintf("probed %d accounts (%d failed", probed, failed)
	if skipped > 0 {
		msg += fmt.Sprintf(", %d within google %dmin cadence", skipped, int(googleEvery.Minutes()))
	}
	return msg + ")", firstErr
}

// refreshTokens refreshes credentials nearing expiry via driver.Refresher.
func (s *Scheduler) refreshTokens(ctx context.Context) (string, error) {
	channels := []string{
		model.ChannelAntigravity, model.ChannelWorkBuddy, model.ChannelTrae, model.ChannelZCode,
		model.ChannelAutoClaw, model.ChannelQoder, model.ChannelLobster,
	}
	refreshed, failed := 0, 0
	var firstErr error
	for _, ch := range channels {
		refresher, ok := s.gw.Driver(ch).(driver.Refresher)
		if !ok {
			continue
		}
		refreshAccounts := s.accountsOf(ch)
		refreshOffsets := s.spreadOffsets(TaskRefreshTokens, accountIDsOf(refreshAccounts))
		for i, acc := range refreshAccounts {
			// Token refresh is a per-account RPC; spread it the same way.
			if refreshOffsets != nil {
				s.accountWait(ctx, refreshOffsets[i])
			}
			if acc.Disabled && !s.includeDisabledNow() {
				continue
			}
			if err := refresher.Refresh(ctx, acc); err != nil {
				failed++
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if acc.CredentialsDirty() {
				raw := acc.Credentials()
				if err := s.st.SaveAccount(acc); err != nil {
					failed++
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				acc.CredentialsPersisted(raw)
				refreshed++
			}
		}
	}
	return fmt.Sprintf("refreshed %d credentials (%d failed)", refreshed, failed), firstErr
}
