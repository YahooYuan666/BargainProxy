package workbuddy

// Outbound identity shapes for the WorkBuddy chat edge (BP-90, 2026-10-22).
//
// Why this file exists: the ids this channel sends are themselves a
// fingerprint. A real client's values have a *shape* — the conversation id
// is a dashed UUID v4, the message/request ids are 32 hex with a
// millisecond timestamp in the leading 48 bits, and the trace/span ids are
// unrelated random values. A gateway that sends 32 hex for a conversation
// id, the same string as both a per-message and a per-session id, and a
// trace id equal to the conversation id, is distinguishable from the real
// client without any header comparison at all.
//
// Evidence: misakano7545/workbuddy2api-panel issue #51 (2026-10-22), which
// compared both official packages on the wire (CN 5.8.3, global 5.8.4):
//   - X-Conversation-ID is always `xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx`;
//   - X-Request-ID / X-Conversation-Message-ID carry a ms timestamp in their
//     first 12 hex and are strictly increasing within one conversation;
//   - X-Trace-ID / X-B3-SpanId are pure random and never equal either.
// No code was copied: the derivations here are re-implemented against our
// own header builder.

import (
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// stableUUIDV4 derives a deterministic UUID-v4-shaped value (36 chars, dashed)
// from (uid, conversation, purpose).
//
// purpose is a domain separator: one (account, conversation) sometimes needs
// several distinct-but-stable identities (the conversation id, the ACP
// connection id, the prompt cache key). An empty purpose keeps the historic
// input, so cache slots derived by earlier builds stay put.
//
// The version/variant nibbles are set per RFC 4122; leaving them random would
// put impossible values in the `4xxx`/`[89ab]xxx` positions, which is another
// distinguishable trait rather than a disguise.
func stableUUIDV4(uid, conversation, purpose string) string {
	in := uid + "|" + conversation
	if purpose != "" {
		in += "|" + purpose
	}
	sum := sha256.Sum256([]byte(in))
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// randomUUIDV4 returns a fresh UUID-v4-shaped value.
func randomUUIDV4() string {
	var u [16]byte
	if _, err := crand.Read(u[:]); err != nil {
		return stableUUIDV4(fmt.Sprintf("%d", time.Now().UnixNano()), "fallback", "")
	}
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// msgIDSeq keeps message ids strictly increasing even inside one millisecond.
//
// The real client's ids are ordered by their millisecond prefix within a
// conversation. Two requests in the same millisecond would otherwise tie,
// and a burst of tied ids is itself a shape the official client never
// produces; bumping the clock by a millisecond keeps the order and costs
// nothing (no client reads this timestamp back).
var msgIDSeq struct {
	mu   sync.Mutex
	last int64
}

// newMessageID returns a 32-hex id whose leading 12 hex are milliseconds
// since the epoch, strictly increasing across calls.
func newMessageID() string {
	var b [10]byte
	_, _ = crand.Read(b[:])
	msgIDSeq.mu.Lock()
	ms := time.Now().UnixMilli()
	if ms <= msgIDSeq.last {
		ms = msgIDSeq.last + 1
	}
	msgIDSeq.last = ms
	msgIDSeq.mu.Unlock()
	return fmt.Sprintf("%012x%s", ms, hex.EncodeToString(b[:]))
}

// newTraceID returns a pure-random 32-hex trace id: no timestamp prefix.
// The official client keeps trace and request ids unrelated, so adding a
// time prefix here would create a correlation the real client never has.
func newTraceID() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		return newMessageID()
	}
	return hex.EncodeToString(b[:])
}

// newSpanID returns a pure-random 16-hex span id.
func newSpanID() string {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		return newMessageID()[:16]
	}
	return hex.EncodeToString(b[:])
}

// validB3TraceID reports whether s may be sent as a B3 trace id (16 or 32 hex).
func validB3TraceID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}
