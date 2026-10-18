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
	ClassTransport      = "transport"       // network/5xx; short cooldown, retry then rotate
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
	ClassSentinel         = "sentinel"      // HTTP 200 body is a known rejection sentinel
	ClassInternal         = "internal"      // gateway bug or unclassified
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
func Rotatable(class string) bool {
	switch class {
	case ClassBadRequest, ClassContentBlocked, ClassSentinel, ClassUpstreamBusy,
		ClassUpstreamTimeout, ClassCanceled:
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
