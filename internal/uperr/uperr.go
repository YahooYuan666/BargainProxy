// Package uperr defines the normalized upstream error taxonomy. Drivers
// classify raw upstream failures into these classes; the pool maps classes to
// account state transitions; the router decides rotation vs surface.
package uperr

import (
	"errors"
	"fmt"
	"time"
)

// Error classes. The class drives scheduling behaviour — it is the single
// vocabulary every channel's error classifier must speak.
const (
	ClassSoftRate       = "soft_rate"       // rate limited; cooldown to ResetAt (or bounded backoff)
	ClassModelRate      = "model_rate"      // model-scoped cooldown; account stays usable for other models
	ClassHardCredit     = "hard_credit"     // balance/credits exhausted; cooldown until daily refresh
	ClassAuth           = "auth"            // credential dead or refresh needed
	ClassBadRequest     = "bad_request"     // request content invalid for this upstream; no account penalty
	ClassContentBlocked = "content_blocked" // content policy rejection; no account penalty
	ClassWAF            = "waf"             // edge/WAF block, likely IP-scoped; short cooldown
	// ClassEdgeBlock: a CDN/WAF edge refused the request before it reached
	// the vendor's API (Cloudflare's "error code: 1010" browser-integrity
	// block and its kin). It is request/edge-level, not account-level: the
	// same account answers fine once the edge lets it through, and charging
	// the account a strike turns one edge rule into a dead pool (sub2api
	// e0fa4b94f, 2026-10-21). The pool pauses the channel briefly instead.
	ClassEdgeBlock = "edge_block"
	ClassTransport = "transport" // network/5xx; short cooldown, retry then rotate
	// ClassTransportDead: a transport failure that will NOT heal by retrying —
	// a dead proxy credential, an unresolvable host, a refused connection. Such
	// a failure hits every account of the channel identically, so walking the
	// pool on an 80s backoff loop can never succeed; the pool parks the channel
	// for longer instead (BP-99; sub2api's persistent-transport split).
	ClassTransportDead = "transport_dead"
	// ClassUpstreamTimeout: the upstream accepted the connection but stalled —
	// header watchdog, idle watchdog, or an explicit deadline. Not the
	// account's fault: another account hits the same slow upstream, so the
	// gateway stops account rotation and applies zero penalty (ticket 27,
	// agentpoints 55d1e4d parity).
	ClassUpstreamTimeout = "upstream_timeout"
	// ClassCanceled: the attempt was aborted by context cancellation — client
	// disconnect, the fallback first-byte window elapsing, or shutdown. Zero
	// account penalty; the gateway binding loop decides whether to cascade
	// (nothing sent to the client yet) or stop (ticket 27/28).
	ClassCanceled       = "canceled"
	ClassQuotaExhausted = "quota_exhausted" // pool level: no account available for this model
	// ClassModelUnavailable: every routable account is model-cooled for the
	// requested model — the client should switch models rather than retry
	// (ticket 33, agentpoints 44d6e04). No account penalty; the binding loop
	// may still cascade to another channel.
	ClassModelUnavailable = "model_unavailable"
	ClassUpstreamBusy     = "upstream_busy" // upstream at capacity (queue full, e.g. CodeBuddy 6021): neither the account nor the payload is at fault
	// ClassNotFound: the upstream answered 404 — the path or model does not
	// exist there. This is NOT "this account is rate limited for this model":
	// classifying it as model_rate used to cool the account for up to
	// ModelRateHoldCap (2h). One routing break then silently removed healthy
	// accounts, and a channel-wide batch of 404s turned every account
	// unavailable ("switch models" — the wrong advice; the channel is broken).
	// Zero account penalty. The pool pauses the channel+model pair for a short
	// fixed hold so a request storm does not hammer a dead path; the binding
	// loop may still cascade to another channel (2026-10-19; misakano7545
	// parity: ErrNotFound = fixed 60s soft cooldown, no failure count).
	ClassNotFound = "not_found"
	// ClassAccountRestricted: upstream has restricted the ACCOUNT itself —
	// workbuddy answers 403 with `code=11140, msg="request illegal"` and
	// displayMsg "账号存在异常行为，已被限制使用". Refresh/retry cannot fix it;
	// the account must leave routing so the pool rotates to a healthy one.
	// Before this class existed the error fell into the generic internal
	// bucket, so a restricted account stayed selectable and sticky
	// conversations kept binding to it — the whole pool looked broken while
	// one account was still healthy (2026-10-20, field diagnosis).
	ClassAccountRestricted = "account_restricted"
	// ClassConcurrencyExceeded: every routable pool for the model is already
	// serving another request. Single-active-per-channel mode allows one
	// account interaction per pool at a time (multi-account concurrency in one
	// pool is exactly the pattern upstream risk control flags as unusual
	// activity); a new request walks to the next pool, and when none is free
	// this is the honest answer instead of queueing silently (2026-10-20,
	// user requirement: "并行数量超发").
	ClassConcurrencyExceeded = "concurrency_exceeded"
	ClassSentinel            = "sentinel" // HTTP 200 body is a known rejection sentinel
	ClassInternal            = "internal" // gateway bug or unclassified
)

// Error is the normalized upstream failure.
type Error struct {
	Class    string
	Status   int
	Message  string
	ResetAt  time.Time // precise upstream reset when known
	Model    string    // set when the error is scoped to a single model
	Channel  string    // binding that produced the failure (final one for pool errors)
	Sentinel string    // matched sentinel text for ClassSentinel
	// Code is the upstream business code when the body carried one (e.g.
	// workbuddy "6004"); the panel shows it as the rate-limit label (BP-49).
	Code          string
	UpstreamType  string
	UpstreamCode  any
	UpstreamParam string
	// HoldUntil is the gateway's own routing hold end, annotated by
	// pool.ApplyResult when the failure was folded into a cooldown — it can be
	// earlier than ResetAt (the hold is capped so a later request re-probes).
	HoldUntil time.Time
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("upstream error class=%s status=%d: %s", e.Class, e.Status, e.Message)
}

// As extracts an *Error from an error chain.
func As(err error) (*Error, bool) {
	var ue *Error
	if errors.As(err, &ue) {
		return ue, true
	}
	return nil, false
}

// New builds an *Error.
func New(class string, status int, format string, args ...any) *Error {
	return &Error{Class: class, Status: status, Message: fmt.Sprintf(format, args...)}
}

// Rotatable reports whether the gateway should try another account/binding
// after this error. Content-level failures are not rotatable: the same
// request would be rejected everywhere, and burning accounts on it is waste.
// Upstream capacity events (queue full) are not rotatable either: every
// account hits the same full queue, and rotating through them at request
// pace is exactly the "miskill the whole pool in seconds" failure mode.
// Upstream timeouts and cancellations are not rotatable across accounts for
// the same reason (same slow upstream / the attempt was aborted by us); the
// binding loop still cascades to a *different channel* when nothing has been
// sent to the client yet (tickets 27/28).
// 404 is not rotatable either: a missing path/model is a property of the
// binding, so the next account of the SAME channel answers 404 too. Rotating
// would walk the whole pool and penalize nothing while spending every
// account's request budget on a dead route; the binding loop still cascades to
// another channel, which is where a working route may live.
func Rotatable(class string) bool {
	switch class {
	case ClassBadRequest, ClassContentBlocked, ClassSentinel, ClassUpstreamBusy,
		ClassUpstreamTimeout, ClassCanceled, ClassNotFound, ClassEdgeBlock,
		ClassTransportDead:
		return false
	default:
		return true
	}
}

// CooldownScope returns "model" when the failure should cool down only the
// affected model on the account, "account" otherwise.
func CooldownScope(class string, hasModel bool) string {
	if hasModel && (class == ClassModelRate || class == ClassSoftRate) {
		return "model"
	}
	return "account"
}
