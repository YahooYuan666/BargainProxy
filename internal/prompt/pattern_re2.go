package prompt

import (
	"regexp"
	"strings"
)

// BP-335: JS-authored regex escapes in tool-schema `pattern` values.
//
// The upstream compiles a schema's `pattern` with RE2. RE2 has no `\uXXXX`
// escape (it spells code points `\x{XXXX}`), and JS's lone surrogates are not
// legal code points anyway. REA 6.9.0 attaches `^[^\uD800-\uDFFF]*$` to every
// local-path field, so an MCP tool set that came from a JS client fails to
// compile upstream and the WHOLE tools array is rejected with 400 11129 —
// every tool the client declared becomes unusable, not just the one.
//
// Two shapes arrive here:
//   - single-escaped (`\ud800` in the JSON source): the JSON round trip already
//     turned it into U+FFFD, which compiles — nothing to do;
//   - double-escaped (`\\uD800`, i.e. Go sees the literal `\uD800`): forwarded
//     verbatim today and rejected. Translated here.
//
// Escaped backslashes are preserved: `\\u0041` means "a literal backslash then
// u0041", not an escape, and must not be translated (hub 8a9cfe31's rule).

var (
	// reuBrace matches `\u{XXXX}` / `\u{XXXXXXXX}` (1-6 hex digits).
	reuBrace = regexp.MustCompile(`\\u\{([0-9a-fA-F]{1,6})\}`)
	// reuFour matches `\uXXXX`.
	reuFour = regexp.MustCompile(`\\u([0-9a-fA-F]{4})`)
	// reEscapedBackslash matches a doubled backslash — a literal `\` in the
	// regex, not the start of an escape.
	reEscapedBackslash = regexp.MustCompile(`\\\\`)
)

// re2Pattern translates JS `\u` escapes into RE2's `\x{…}` and verifies the
// result compiles. ok=false means the pattern cannot be made RE2-legal and the
// caller should DROP it (one local regex is worth far less than the tool set).
//
// DELIBERATE DEVIATION from the upstream reference (misakano abed8395 skips
// compile validation when the pattern contains no `\u`, calling it "zero
// overhead"). We always compile: a pattern can be illegal for reasons that
// have nothing to do with `\u` (a stray `(?<`, an unbalanced bracket, a
// lookbehind RE2 never supported), and ANY uncompilable pattern takes the
// whole tools array down with 400 11129. Compiling a short regex costs
// microseconds against a request that spends seconds upstream — the trade is
// obviously worth it, and it is the difference between "one constraint lost"
// and "every tool the client declared is unusable".
func re2Pattern(p string) (string, bool) {
	out := p
	if strings.Contains(p, `\u`) || strings.Contains(p, `\_`) {
		// Protect escaped backslashes: swap them for a placeholder no regex
		// escape can collide with, translate, then restore.
		const placeholder = "\x00BP-ESC-BACKSLASH\x00"
		out = reEscapedBackslash.ReplaceAllString(out, placeholder)
		// `\_` is not a legal RE2 escape either; it means a literal underscore.
		out = strings.ReplaceAll(out, `\_`, `_`)
		out = reuBrace.ReplaceAllString(out, `\x{$1}`)
		out = reuFour.ReplaceAllString(out, `\x{$1}`)
		out = strings.ReplaceAll(out, placeholder, `\\`)
	}
	if _, err := regexp.Compile(out); err != nil {
		return "", false
	}
	return out, true
}
