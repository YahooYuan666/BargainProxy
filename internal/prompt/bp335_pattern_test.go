// BP-335: JS `\u` escapes in tool-schema patterns break RE2, and a schema the
// upstream cannot compile takes the WHOLE tools array down with 400 11129.
//
// Field basis: misakano abed8395 (2026-11-20) — REA 6.9.0 attaches
// `^[^\uD800-\uDFFF]*$` to every local-path field; hub 8a9cfe31 (2026-11-22)
// adds the rule that escaped backslashes must be preserved.
package prompt

import "testing"

// The headline shape: a JS lone-surrogate class must be translated and must
// compile as RE2 afterwards.
func TestRE2PatternTranslatesUnicodeEscapes(t *testing.T) {
	got, ok := re2Pattern(`^[^\uD800-\uDFFF]*$`)
	if !ok {
		t.Fatal("a JS unicode-escape pattern must be translatable")
	}
	if got != `^[^\x{D800}-\x{DFFF}]*$` {
		t.Fatalf("got %q, want ^[^\\x{D800}-\\x{DFFF}]*$", got)
	}
}

func TestRE2PatternTranslatesBracedForm(t *testing.T) {
	got, ok := re2Pattern(`\u{1F600}`)
	if !ok {
		t.Fatal("braced form must translate")
	}
	if got != `\x{1F600}` {
		t.Fatalf("got %q, want \\x{1F600}", got)
	}
}

// A doubled backslash is a LITERAL backslash followed by "u0041", not an
// escape. Translating it would change what the regex matches.
func TestRE2PatternPreservesEscapedBackslash(t *testing.T) {
	got, ok := re2Pattern(`\\u0041`)
	if !ok {
		t.Fatal("an escaped-backslash pattern must still be accepted")
	}
	if got != `\\u0041` {
		t.Fatalf("got %q, want \\\\u0041 unchanged (literal backslash + u0041)", got)
	}
}

// `\_` is not a legal RE2 escape either; it means a literal underscore.
func TestRE2PatternFixesIllegalUnderscoreEscape(t *testing.T) {
	got, ok := re2Pattern(`a\_b`)
	if !ok {
		t.Fatal("`\\_` must be repaired, not rejected")
	}
	if got != `a_b` {
		t.Fatalf("got %q, want a_b", got)
	}
}

// Ordinary patterns pay nothing and come back byte-identical.
func TestRE2PatternLeavesOrdinaryPatternsAlone(t *testing.T) {
	for _, p := range []string{`^\d+$`, `^[a-z]*$`, `^/home/.*$`, ``} {
		got, ok := re2Pattern(p)
		if !ok || got != p {
			t.Fatalf("pattern %q must pass through unchanged, got %q ok=%v", p, got, ok)
		}
	}
}

// A pattern that is broken for reasons beyond `\u` cannot be repaired; the
// caller must drop it rather than send a schema the upstream rejects whole.
func TestRE2PatternRejectsUnrepairable(t *testing.T) {
	if _, ok := re2Pattern(`(?<bad`); ok {
		t.Fatal("an uncompilable pattern must report ok=false so the caller drops it")
	}
}

// End-to-end through the schema walker: the key is repaired in place, and an
// unrepairable one is DELETED (never left to poison the request).
func TestCoerceRepairsAndDropsPattern(t *testing.T) {
	s := map[string]any{
		"type":    "object",
		"pattern": `^[^\uD800-\uDFFF]*$`,
	}
	if !CoerceSchemaKeywordTypes(s) {
		t.Fatal("a repaired pattern must report a change")
	}
	if s["pattern"] != `^[^\x{D800}-\x{DFFF}]*$` {
		t.Fatalf("pattern = %v, want the translated form", s["pattern"])
	}

	bad := map[string]any{"type": "object", "pattern": `(?<bad`}
	if !CoerceSchemaKeywordTypes(bad) {
		t.Fatal("dropping an unrepairable pattern must report a change")
	}
	if _, present := bad["pattern"]; present {
		t.Fatal("an unrepairable pattern must be DELETED, not forwarded")
	}
}
