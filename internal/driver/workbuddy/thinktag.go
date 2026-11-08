// thinktag.go — split thinking control tokens that leaked into the answer's
// content (BP-296, misakano workbuddy2api-panel 771c887 for their issue #194,
// MIT; re-implemented here against the IR contract with their states kept).
//
// The upstream's reasoning parser occasionally emits the whole chain of
// thought as CONTENT, wrapped in <think:SUF>…</think:SUF> (SUF is a model
// checkpoint token suffix, not a request id; measured on their prod: 4 of 31
// assistant turns, usage reasoning_tokens=0). Anthropic-protocol clients only
// read content_block.type=thinking — an inline tag arrives as plain text.
package workbuddy

import "strings"

const (
	thinkOpen  = "<think:"
	thinkClose = "</think:"
)

// ThinkTag lifts thinking tags out of leaked content (BP-296).
//
// Only activates when the content STARTS with <think: — a `<think:` mentioned
// mid-prose is left alone. Bare <think> (no colon) is another parser's shape
// and is not handled until one shows up.
type ThinkTag struct {
	decided bool   // decided whether this content carries inline thinking
	on      bool   // lifting enabled
	inTag   bool   // the cursor is inside a tag
	pend    string // a tail that may be half a tag, waiting for the next piece
}

// Feed consumes one content delta and returns this piece's thinking and body
// (each in original order, concatenated into their own channel).
func (t *ThinkTag) Feed(s string) (think, body string) {
	if s == "" {
		return "", ""
	}
	t.pend += s
	if !t.decided {
		// Not enough bytes to decide yet: hold the piece rather than emit
		// "<thi" as body text.
		if len(t.pend) < len(thinkOpen) && strings.HasPrefix(thinkOpen, t.pend) {
			return "", ""
		}
		t.decided = true
		if t.on = strings.HasPrefix(t.pend, thinkOpen); !t.on {
			out := t.pend
			t.pend = ""
			return "", out
		}
	}
	if !t.on {
		out := t.pend
		t.pend = ""
		return "", out
	}
	return t.scan(false)
}

// Flush drains the tail. MUST be called when the stream ends (or once for a
// non-stream body), or the last few characters are lost.
func (t *ThinkTag) Flush() (think, body string) {
	if t.pend == "" {
		return "", ""
	}
	if !t.decided {
		t.decided = true
		if t.on = strings.HasPrefix(t.pend, thinkOpen); !t.on {
			out := t.pend
			t.pend = ""
			return "", out
		}
	}
	if !t.on {
		out := t.pend
		t.pend = ""
		return "", out
	}
	return t.scan(true)
}

// scan consumes t.pend. With flush=false a tail that may be the first half of
// a tag stays in pend for the next piece.
func (t *ThinkTag) scan(flush bool) (think, body string) {
	var tb, bb strings.Builder
	emit := func(s string) {
		if s == "" {
			return
		}
		if t.inTag {
			tb.WriteString(s)
		} else {
			bb.WriteString(s)
		}
	}
	for {
		p := t.pend
		idx, closing := -1, false
		for _, m := range []struct {
			tag     string
			closing bool
		}{{thinkOpen, false}, {thinkClose, true}} {
			if k := strings.Index(p, m.tag); k >= 0 && (idx < 0 || k < idx) {
				idx, closing = k, m.closing
			}
		}
		if idx < 0 {
			keep := 0
			if !flush {
				// The tail may be the first half of the next tag (at most
				// len(</think:)-1 bytes).
				for n := min(len(p), len(thinkClose)-1); n > 0; n-- {
					tail := p[len(p)-n:]
					if strings.HasPrefix(thinkClose, tail) || strings.HasPrefix(thinkOpen, tail) {
						keep = n
						break
					}
				}
			}
			emit(p[:len(p)-keep])
			t.pend = p[len(p)-keep:]
			return tb.String(), bb.String()
		}
		emit(p[:idx])
		rest := p[idx:]
		gt := strings.IndexByte(rest, '>')
		if gt < 0 {
			// The tag has not closed yet: hold it for the next piece. On
			// flush the upstream is done — emit it as body rather than drop
			// content.
			if flush {
				emit(rest)
				t.pend = ""
			} else {
				t.pend = rest
			}
			return tb.String(), bb.String()
		}
		t.pend = rest[gt+1:]
		t.inTag = !closing
	}
}

// SplitThinkTags is the one-shot form (a complete content string in, the two
// channels out).
func SplitThinkTags(s string) (think, body string) {
	var t ThinkTag
	a, b := t.Feed(s)
	c, d := t.Flush()
	return a + c, b + d
}
