// Package streamx holds the shared SSE line-stream hardening used by every
// upstream driver (BP-318): a line-level scanner that splits GLUED frames, and
// the tool-call fragment folder (BP-320).
//
// # Glued frames (BP-318, misakano afb5f224 #197)
//
// The upstream occasionally packs two chunks into one `data:` line —
// `data: {a} {b}`. Per the SSE spec the client parses the whole line as ONE
// JSON value and dies on the second object ("Unexpected non-whitespace
// character after JSON at position N"), killing the whole turn. Our drivers
// were worse: their per-line decode failed and silently DROPPED the frame —
// content vanished without a trace. The split here happens at the line level,
// so every driver's loop body stays byte-identical: the wrapper yields each
// JSON value as its own `data:` line.
//
// Splitting is deliberately conservative: a byte scan tracks string/escape
// state and {} [] nesting depth, cuts at depth-zero boundaries, and
// re-validates every part as JSON — anything that does not split cleanly
// passes through untouched (single element), exactly the pre-fix behavior.
package streamx

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"strings"
)

// maxSplitParts caps how many values one line may be cut into. Upstream glue
// is "one or two"; the cap only bounds pathological garbage — without it a
// few-hundred-KB bad line would fan out into thousands of writes.
const maxSplitParts = 64

// SplitScanner is a drop-in *bufio.Scanner wrapper that splits glued
// `data:` lines. Scan/Text/Err/Buffer mirror bufio.Scanner; the loop bodies
// in the drivers do not change.
type SplitScanner struct {
	s     *bufio.Scanner
	queue []string
	cur   string
}

// NewSplitScanner wraps r with the glue-splitting scanner.
func NewSplitScanner(r io.Reader) *SplitScanner {
	return &SplitScanner{s: bufio.NewScanner(r)}
}

// Buffer forwards to the underlying scanner (drivers raise the 8MB line cap).
func (ss *SplitScanner) Buffer(buf []byte, max int) { ss.s.Buffer(buf, max) }

// Err forwards the underlying scanner's error.
func (ss *SplitScanner) Err() error { return ss.s.Err() }

// Scan yields the next logical line: queued split parts first, then the
// underlying scanner. A glued data line expands into several queued lines.
func (ss *SplitScanner) Scan() bool {
	if len(ss.queue) > 0 {
		ss.cur = ss.queue[0]
		ss.queue = ss.queue[1:]
		return true
	}
	if !ss.s.Scan() {
		return false
	}
	line := ss.s.Text()
	if parts, ok := splitGluedDataLine(line); ok {
		// Field signal (misakano afb5f224): this is the only trace a glued
		// frame leaves behind — log it, then hand each value its own line.
		log.Printf("WARN: [streamx] data line packed %d JSON values (glued frame) — splitting, first %.120q", len(parts), truncate(parts[0], 120))
		ss.queue = make([]string, len(parts))
		for i, p := range parts {
			ss.queue[i] = "data:" + p
		}
		return ss.Scan()
	}
	ss.cur = line
	return true
}

// Text returns the current logical line.
func (ss *SplitScanner) Text() string { return ss.cur }

// splitGluedDataLine reports whether line is a `data:` line holding several
// JSON values, and returns the split payloads. ok=false (or a single-element
// result expressed as ok=false) keeps the line byte-identical.
func splitGluedDataLine(line string) ([]string, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, "data:") {
		return nil, false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return nil, false
	}
	parts := splitConcatenatedJSON(payload)
	if len(parts) < 2 {
		return nil, false
	}
	return parts, true
}

// looksLikeJSON reports whether the payload INTENDS to be JSON (first
// non-space byte is { or [) — used only to decide whether a decode failure
// deserves a WARN (plain-text heartbeats stay silent).
func LooksLikeJSON(payload string) bool {
	t := strings.TrimLeft(payload, " \t\r\n")
	if t == "" {
		return false
	}
	return t[0] == '{' || t[0] == '['
}

// splitConcatenatedJSON cuts a run of concatenated JSON values (`{a} {b}` /
// `{a}{b}`) into independent strings. Cut points: byte scan tracking string
// state (with \ escapes) and {} [] depth; a cut happens where the depth
// returns to zero. Every part is re-validated as JSON — better to return the
// original unsplit than to hand the client half an object.
func splitConcatenatedJSON(payload string) []string {
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" {
		return []string{payload}
	}
	var (
		parts   []string
		depth   int
		inStr   bool
		escaped bool
		start   = -1
	)
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if start < 0 {
			// Whitespace between values; anything else opens a new value.
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
				continue
			}
			if c != '{' && c != '[' && c != '"' && c != '-' && (c < '0' || c > '9') &&
				c != 't' && c != 'f' && c != 'n' {
				return []string{payload} // not a JSON value sequence — give up
			}
			start = i
		}
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
				if depth == 0 { // a bare string value: cut after the quote
					part, ok := validJSON(trimmed[start : i+1])
					if !ok {
						return []string{payload}
					}
					parts = append(parts, part)
					if len(parts) >= maxSplitParts {
						return []string{payload}
					}
					start = -1
				}
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth < 0 {
				return []string{payload}
			}
			if depth == 0 {
				part, ok := validJSON(trimmed[start : i+1])
				if !ok {
					return []string{payload}
				}
				parts = append(parts, part)
				if len(parts) >= maxSplitParts {
					return []string{payload}
				}
				start = -1
			}
		}
	}
	if start >= 0 {
		// Trailing bytes that never closed: not a clean value run.
		return []string{payload}
	}
	if len(parts) < 2 {
		return []string{payload}
	}
	return parts
}

// validJSON verifies s parses as JSON and returns the trimmed original.
func validJSON(s string) (string, bool) {
	t := strings.TrimSpace(s)
	if t == "" || json.Unmarshal([]byte(t), new(any)) != nil {
		return "", false
	}
	return t, true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
