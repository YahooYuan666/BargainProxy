package gateway

import "strings"

// BP-226: glm-5.3-flash* replicas stitch a lone carriage return into
// streaming tool-call arguments (magpie dd279e93 #4256, production case):
// the raw byte lands inside a JSON string, and every strict downstream JSON
// parser rejects it — the client sees a SyntaxError on arguments it never
// malformed. A lone \r that IS followed by \n stays: CRLF between JSON tokens
// is legal whitespace and models pretty-print arguments with it. Applied at
// the gateway's single event sink and on the final aggregated response, so
// every chat-shaped driver is covered without touching each one (the
// same-form-siblings discipline; the AutoClaw/WorkBuddy glm routes are the
// live exposure).

// crTidy scrubs a STREAM of tool-argument deltas. A trailing \r is held back
// until the next delta shows whether it pairs with a \n; a \r at the very
// end of the stream (or before the next tool starts) is lone and dropped.
type crTidy struct{ pending bool }

// write returns the delta to forward: lone \r inside the delta is dropped, a
// trailing \r is held back until the next delta decides its fate.
func (c *crTidy) write(delta string) string {
	if c.pending {
		c.pending = false
		if strings.HasPrefix(delta, "\n") {
			// The held \r pairs with this delta's leading \n — CRLF stays.
			delta = "\r" + delta
		}
	}
	if !strings.Contains(delta, "\r") {
		return delta
	}
	var b strings.Builder
	b.Grow(len(delta))
	for i := 0; i < len(delta); i++ {
		if delta[i] != '\r' {
			b.WriteByte(delta[i])
			continue
		}
		if i+1 < len(delta) {
			if delta[i+1] == '\n' {
				b.WriteByte('\r')
				b.WriteByte('\n')
				i++
			}
			// else: lone \r inside the delta — dropped.
			continue
		}
		// Trailing \r: hold for the next delta.
		c.pending = true
	}
	return b.String()
}

// flush settles a held \r at a boundary (end of stream, new tool start,
// attempt reset): alone, it is dropped.
func (c *crTidy) flush() {
	c.pending = false
}

// dropLoneCR scrubs a WHOLE tool-argument string: removes every \r not
// followed by \n. Escaped \r sequences (the two bytes `\` + `r`) are text
// and untouched.
func dropLoneCR(s string) string {
	if !strings.Contains(s, "\r") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' && (i+1 >= len(s) || s[i+1] != '\n') {
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
