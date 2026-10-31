package uperr

import "testing"

// BP-244: the overflow flag decides whether the client is told "compact and
// retry" or handed a generic 400 it will answer by resending the same body.
// The wording families are the ones the domestic vendors actually use.
func TestMarkOverflowDetectsTheWordFamilies(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"prompt is too long: 312390 > 262144 maximum", true},
		{"context_length_exceeded", true},
		{"maximum context length is 128000 tokens", true},
		{"内容太长", true},
		{"输入超过最大长度", true},
		{"rate limit exceeded, please wait", false},
		{"内容太长，已达到速率限制", false}, // pace wins: a momentary cap, not an overflow
		{"invalid parameter", false},
	} {
		e := New(ClassBadRequest, 400, "test")
		MarkOverflow(e, tc.text)
		if e.ContextOverflow != tc.want {
			t.Errorf("MarkOverflow(%q) = %v, want %v", tc.text, e.ContextOverflow, tc.want)
		}
	}
}

// MarkOverflow must be safe on nil and must return the same error so callers
// can wrap inline.
func TestMarkOverflowNilSafe(t *testing.T) {
	if got := MarkOverflow(nil, "prompt is too long"); got != nil {
		t.Fatalf("MarkOverflow(nil) = %v, want nil", got)
	}
	e := New(ClassBadRequest, 400, "x")
	if got := MarkOverflow(e, "prompt is too long"); got != e {
		t.Fatal("MarkOverflow must return the same error for inline use")
	}
}
