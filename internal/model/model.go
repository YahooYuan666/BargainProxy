// Package model contains the persisted domain entities shared by the pool,
// the store and the drivers.
package model

import (
	"encoding/json"
	"time"
)

// ModelCooldown is one (account, model) rate-limit hold. Until is the routing
// barrier — the earliest moment the gateway will try this model again (capped
// at the pool's ModelRateHoldCap so a long upstream window still gets probed
// early). ResetAt is the unlock moment the upstream itself stated (zero = the
// upstream gave none); Reason/Message are the panel's short label and the
// upstream text (BP-49).
type ModelCooldown struct {
	Until   time.Time `json:"until"`
	ResetAt time.Time `json:"reset_at,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Message string    `json:"message,omitempty"`
}

// UnmarshalJSON also accepts the pre-BP-49 bare-timestamp form
// ("deepseek-v4.1-flash": "2026-10-16T22:16:56+08:00") so state blobs written
// by older builds keep loading.
func (c *ModelCooldown) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return err
		}
		*c = ModelCooldown{Until: t}
		return nil
	}
	var obj struct {
		Until   time.Time `json:"until"`
		ResetAt time.Time `json:"reset_at"`
		Reason  string    `json:"reason"`
		Message string    `json:"message"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	*c = ModelCooldown{Until: obj.Until, ResetAt: obj.ResetAt, Reason: obj.Reason, Message: obj.Message}
	return nil
}

// Channel identifiers. A channel is one upstream system with its own
// credentials, quota mechanics and request protocol.
const (
	ChannelAntigravity  = "antigravity"
	ChannelWorkBuddy    = "workbuddy"
	ChannelTrae         = "trae"
	ChannelZCode        = "zcode"
	ChannelAutoClaw     = "autoclaw"
	ChannelQoder        = "qoder"
	ChannelOpenAICompat = "openaicompat"
	// ChannelOpenCodeZen is the OpenCode Zen anonymous free-model channel
	// (OpenAI wire upstream; free-tier gated — BP-54).
	ChannelOpenCodeZen = "opencodezen"
)

// Window is a quota window snapshot (antigravity 5h / weekly windows).
type Window struct {
	RemainingFraction float64   `json:"remaining_fraction"` // 0..1
	ResetAt           time.Time `json:"reset_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// Credits is a credit balance snapshot (workbuddy / trae credit pools).
type Credits struct {
	Remain            int64     `json:"remain"`
	Total             int64     `json:"total"`
	BalanceOnly       bool      `json:"balance_only,omitempty"`        // no finite quota: Total equals the current balance (AutoClaw points)
	Expiring          int64     `json:"expiring,omitempty"`            // within the expiring soon window
	Bonus             int64     `json:"bonus,omitempty"`               // extra add-on / org credits on top of the plan
	EarliestExpiresAt time.Time `json:"earliest_expires_at,omitempty"` // earliest future expiry among positive packs
	EarliestRemaining int64     `json:"earliest_remaining,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// CostObservation records the observed billing tier for one (account, model)
// pair: 0 = observed free, 1 = unknown, 2 = observed paid.
type CostObservation struct {
	Tier       int       `json:"tier"`
	ObservedAt time.Time `json:"observed_at"`
}

// Account is one upstream credential plus its runtime scheduling state.
//
// Persistence contract: the store saves everything except the volatile
// in-flight counters (those are pooled state, reset to zero on boot so a
// crash cannot leave an account permanently "busy").
type Account struct {
	ID        string `json:"id"` // stable id, e.g. "antigravity:uid" or uuid
	Channel   string `json:"channel"`
	UID       string `json:"uid,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	Realm     string `json:"realm,omitempty"`      // workbuddy: cn | global
	ProjectID string `json:"project_id,omitempty"` // antigravity upstream project

	// CredsJSON is the raw credential blob. In memory it is plaintext; the
	// store persists it through the crypto envelope.
	CredsJSON []byte `json:"-"`

	Disabled bool `json:"disabled,omitempty"`
	// Paused exits the routing pool WITHOUT stopping keep-alive tasks: the
	// account still checks in, reports activity, refreshes tokens and probes
	// balances — it just does not take chat traffic. Distinct from Disabled
	// (terminal: tasks stop too). Persisted in the store state blob.
	Paused bool `json:"paused,omitempty"`

	// CredsDirty marks credentials refreshed by a driver that need persisting.
	CredsDirty bool `json:"-"`

	// --- runtime scheduling state ---
	AccountCooldownUntil time.Time                  `json:"account_cooldown_until,omitempty"`
	ModelCooldowns       map[string]ModelCooldown   `json:"model_cooldowns,omitempty"`
	Windows              map[string]Window          `json:"windows,omitempty"` // keys: "five_hour","weekly"
	Credits              Credits                    `json:"credits,omitempty"`
	CostTiers            map[string]CostObservation `json:"cost_tiers,omitempty"`
	ConsecutiveFails     int                        `json:"consecutive_fails,omitempty"`
	BreakerFails         int                        `json:"breaker_fails,omitempty"`
	LastUsedAt           time.Time                  `json:"last_used_at,omitempty"`
	UpdatedAt            time.Time                  `json:"updated_at,omitempty"`

	// LastPickSeq is a monotonic sequence for the 0.1s anti-collision gap.
	LastPickSeq uint64 `json:"-"`
}

// RequestLog is one request audit row. Metadata only — never prompts or
// completions (privacy red line from the requirements review).
type RequestLog struct {
	ID           int64     `json:"id,omitempty"`
	Time         time.Time `json:"time"`
	RequestID    string    `json:"request_id"`
	Protocol     string    `json:"protocol"`
	Model        string    `json:"model"`
	Channel      string    `json:"channel"`
	AccountID    string    `json:"account_id,omitempty"`
	AccountLabel string    `json:"account_label,omitempty"`
	Status       int       `json:"status"`
	ErrorClass   string    `json:"error_class,omitempty"`
	TTFBMs       int       `json:"ttfb_ms"`
	DurationMs   int       `json:"duration_ms"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	Stream       bool      `json:"stream"`
	ClientIP     string    `json:"client_ip,omitempty"`
	UserAgent    string    `json:"user_agent,omitempty"`
	Degraded     string    `json:"degraded,omitempty"` // channel that actually served after fallback
	// Attempts counts binding/account attempts (1 = no cascade) and
	// AttemptChannels is the binding trail ("workbuddy→trae") — BP-38: the
	// cascade cost becomes visible in the panel instead of guesswork.
	Attempts        int    `json:"attempts"`
	AttemptChannels string `json:"attempt_channels,omitempty"`
	// Credit is the upstream-reported credit consumption for this request
	// (workbuddy usage.credit). 0 with CreditKnown=false means "no data"
	// (quota-based channels report none) — BP-40.
	Credit      float64 `json:"credit,omitempty"`
	CreditKnown bool    `json:"credit_known,omitempty"`
	// CachedTokens is the upstream-reported prefix-cache hit size for this
	// request (workbuddy usage.prompt_cache_hit_tokens / gemini
	// cachedContentTokenCount; 0 = nothing cached). CacheKnown marks whether
	// the upstream reported cache accounting at all — a reported 0-hit request
	// still counts in the hit-rate denominator (BP-53).
	CachedTokens int  `json:"cached_tokens,omitempty"`
	CacheKnown   bool `json:"cache_known,omitempty"`
}

// TaskRun is one scheduled-task execution record (checkin, growth tasks,
// keepalive). Unique per (account, task, run_date) for idempotency.
type TaskRun struct {
	ID         int64     `json:"id,omitempty"`
	AccountID  string    `json:"account_id"`
	TaskType   string    `json:"task_type"`
	RunDate    string    `json:"run_date"` // YYYY-MM-DD in Asia/Shanghai
	Status     string    `json:"status"`   // ok | failed | skipped
	Detail     string    `json:"detail,omitempty"`
	Credits    int64     `json:"credits,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}
