// BP-335: a native model can CLOSE call A and reuse the same transport index
// for call B. The index is a fragment-association slot, not a logical identity
// (Trae2api-cn f6eeeba6, 2026-11-22). The old accumulator folded B's arguments
// onto A's completed ones.
package trae

import (
	"testing"

	"bargainproxy/internal/ir"
)

func TestMergeArgsNewCompleteCallOnSameIndex(t *testing.T) {
	// prev is a complete call; next is a DIFFERENT complete call.
	prev := `{"path":"a.go"}`
	next := `{"path":"b.go"}`
	got := mergeArgs(prev, next)
	if got != next {
		t.Fatalf("a different complete call must REPLACE, not concatenate: got %s", got)
	}
}

// A re-sent snapshot with different punctuation is still a re-send (existing
// behaviour must not regress).
func TestMergeArgsResentSnapshotStillReplaces(t *testing.T) {
	prev := `{"a": 1}`
	next := `{"a":1}`
	if got := mergeArgs(prev, next); got != next {
		t.Fatalf("re-send must replace: got %s", got)
	}
}

// A fragment continues the accumulation as before.
func TestMergeArgsFragmentStillConcatenates(t *testing.T) {
	got := mergeArgs(`{"path":"a`, `.go"}`)
	if got != `{"path":"a.go"}` {
		t.Fatalf("fragment must concatenate, got %s", got)
	}
}

// Identical complete payloads are the duplicate-snapshot case: keep the first.
func TestMergeArgsIdenticalSnapshotKeepsPrev(t *testing.T) {
	prev := `{"path":"a.go"}`
	if got := mergeArgs(prev, prev); got != prev {
		t.Fatalf("identical complete payload must keep prev, got %s", got)
	}
}

func TestFirstFreeToolIndex(t *testing.T) {
	acc := map[int]*ir.ToolCall{0: {}, 1: {}}
	if got := firstFreeToolIndex(acc); got != 2 {
		t.Fatalf("first free index = %d, want 2", got)
	}
	acc[3] = &ir.ToolCall{}
	if got := firstFreeToolIndex(acc); got != 2 {
		t.Fatalf("first free index = %d, want 2 (smallest unused)", got)
	}
	delete(acc, 2)
	if got := firstFreeToolIndex(acc); got != 2 {
		t.Fatalf("after freeing 2, first free = %d", got)
	}
}
