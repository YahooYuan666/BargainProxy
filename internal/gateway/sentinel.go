package gateway

import (
	"strings"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/uperr"
)

// sentinelMaxLen bounds the buffering window: replies longer than the
// longest sentinel are flushed immediately.
const sentinelMaxLen = 128

// sentinelGuard wraps an emitter and detects HTTP-200 plain-text rejection
// markers ("sentinels", e.g. DANGEROUS_ACTIVITY_BLOCKED): some upstreams
// answer content-policy refusals with a 200 + a fixed assistant text instead
// of an error status.
//
// Semantics (per the requirements review):
//   - the first text output is buffered up to sentinelMaxLen chars;
//   - if the message ends and the assembled text matches a sentinel exactly,
//     a content-rejection error is returned and NOTHING was emitted;
//   - any other output, or a longer reply, flushes the buffer and streams
//     normally (mid-stream matches are not treated specially).
//
// The guard returns a *uperr.Error{Class: sentinel} which callers must treat
// as non-rotatable (the account is fine; the payload was rejected).
func sentinelGuard(inner driver.Emit, sentinels []string) driver.Emit {
	if len(sentinels) == 0 || inner == nil {
		return inner
	}
	var (
		buf      strings.Builder
		flushed  bool
		rejected bool
	)
	flush := func() error {
		if flushed {
			return nil
		}
		flushed = true
		if buf.Len() == 0 {
			return nil
		}
		return inner(ir.StreamEvent{Kind: ir.EvText, Delta: buf.String()})
	}
	return func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EvAttemptReset {
			// BP-123: a fresh attempt is starting on the same client stream.
			// Text buffered for sentinel matching belongs to the FAILED
			// attempt and must be DROPPED, never delivered — the default
			// branch below flushes on any non-text event, which would emit
			// the dead attempt's opening words followed by the new attempt's
			// own. The guard is per-binding, so this path is reached on
			// account rotation inside one binding; cross-binding cascades
			// already get a fresh guard. A settled rejection stays settled:
			// the payload was refused by content policy, and the retry (which
			// only happens on another account/channel) must not un-refuse it —
			// the buffered text is still dropped so it cannot leak later.
			// The reset itself PASSES THROUGH: the ledger and the protocol
			// writers downstream need it to drop their own buffers too.
			buf.Reset()
			return inner(ev)
		}
		if rejected {
			return uperr.New(uperr.ClassSentinel, 200, "upstream rejected the request (content policy)")
		}
		switch ev.Kind {
		case ir.EvText:
			if flushed {
				return inner(ev)
			}
			buf.WriteString(ev.Delta)
			if buf.Len() > sentinelMaxLen {
				return flush()
			}
			return nil
		case ir.EvDone:
			if !flushed {
				text := strings.TrimSpace(buf.String())
				for _, s := range sentinels {
					if text == strings.TrimSpace(s) {
						rejected = true
						return uperr.New(uperr.ClassSentinel, 200, "upstream rejected the request (content policy)")
					}
				}
				if err := flush(); err != nil {
					return err
				}
			}
			return inner(ev)
		default:
			// Any non-text output proves this is a real reply: flush first.
			if !flushed {
				if err := flush(); err != nil {
					return err
				}
			}
			return inner(ev)
		}
	}
}
