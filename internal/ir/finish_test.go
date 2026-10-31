package ir

import "testing"

// BP-222: the finish_reason word table — only the exact "error" word (any
// case) means a failed turn; near-misses and real ends stay ordinary.
func TestFinishReasonFailed(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"error", true},
		{"Error", true},
		{"ERROR", true},
		{"errors", false},
		{"err", false},
		{"stop", false},
		{"length", false},
		{"tool_calls", false},
		{"content_filter", false},
		{"", false},
	}
	for _, c := range cases {
		if got := FinishReasonFailed(c.in); got != c.want {
			t.Fatalf("FinishReasonFailed(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
