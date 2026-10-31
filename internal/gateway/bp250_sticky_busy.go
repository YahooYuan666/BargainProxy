package gateway

import (
	"fmt"
	"hash/fnv"

	"bargainproxy/internal/uperr"
)

// sessionBoundTo reports whether this conversation is already pinned to an
// account of channel (BP-250).
//
// A bound conversation must not be turned away by the pool-busy pre-check: the
// sticky rule lives inside pool.Acquire, so skipping here means the binding
// never gets the chance to serve it or to wait for it. An empty session key is
// never "bound" — it is a fresh conversation by definition.
func (g *Gateway) sessionBoundTo(channel, sessionKey string) (bool, string) {
	if g == nil || g.pool == nil || sessionKey == "" {
		return false, ""
	}
	id, ok := g.pool.SessionBinding(channel, sessionKey)
	return ok, id
}

// describeSession renders a session key for a log line without leaking the
// conversation's content: the key is a hash of the client's own fingerprint, so
// only a short prefix plus a stable hash is printed.
func describeSession(sessionKey string) string {
	if sessionKey == "" {
		return "a new conversation"
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(sessionKey))
	return fmt.Sprintf("session %08x", h.Sum32())
}

// skippedReason renders the pool error a pre-flight skip carried, for the log
// line (BP-251). Kept short: the log line already names channel and model.
func skippedReason(err error) string {
	if err == nil {
		return "unavailable"
	}
	if ue, ok := uperr.As(err); ok {
		return ue.Class + ": " + ue.Message
	}
	return err.Error()
}
