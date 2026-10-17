package sessionfp

import (
	"testing"

	"bargainproxy/internal/ir"
)

func text(role ir.Role, s string) ir.Message {
	return ir.Message{Role: role, Content: []ir.ContentPart{{Kind: "text", Text: s}}}
}

// TestFromMessagesStability pins the level-3 contract: same system+first-user
// prefix yields the same fingerprint regardless of the appended tail;
// different prefixes differ; empty conversations never stick.
func TestFromMessagesStability(t *testing.T) {
	sys := text(ir.RoleSystem, "you are a helpful bot")
	first := text(ir.RoleUser, "hello world")
	table := []struct {
		name string
		a, b []ir.Message
		same bool
	}{
		{
			name: "same prefix different tail",
			a:    []ir.Message{sys, first},
			b: []ir.Message{sys, first,
				text(ir.RoleAssistant, "hi"), text(ir.RoleUser, "follow up")},
			same: true,
		},
		{
			name: "different system",
			a:    []ir.Message{text(ir.RoleSystem, "prompt A"), first},
			b:    []ir.Message{text(ir.RoleSystem, "prompt B"), first},
			same: false,
		},
		{
			name: "different first user",
			a:    []ir.Message{sys, text(ir.RoleUser, "question one")},
			b:    []ir.Message{sys, text(ir.RoleUser, "question two")},
			same: false,
		},
		{
			name: "system only versus user-only",
			a:    []ir.Message{sys},
			b:    []ir.Message{first},
			same: false,
		},
	}
	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			fa, fb := FromMessages(tc.a), FromMessages(tc.b)
			if fa == "" || fb == "" {
				t.Fatalf("fingerprints must be non-empty: %q / %q", fa, fb)
			}
			if (fa == fb) != tc.same {
				t.Fatalf("same=%v, got %q / %q", tc.same, fa, fb)
			}
			if got := FromMessages(tc.a); got != fa {
				t.Fatalf("fingerprint not deterministic: %q -> %q", fa, got)
			}
		})
	}

	if got := FromMessages(nil); got != "" {
		t.Fatalf("empty conversation must not stick, got %q", got)
	}
	if got := FromMessages([]ir.Message{text(ir.RoleAssistant, "orphan")}); got != "" {
		t.Fatalf("no system/user text must not stick, got %q", got)
	}
}

func TestCacheControl(t *testing.T) {
	if CacheControl("") != "" || CacheControl("   ") != "" {
		t.Fatal("blank marked block must not produce a fingerprint")
	}
	a := CacheControl("stable cached prefix")
	b := CacheControl("stable cached prefix")
	c := CacheControl("other cached prefix")
	if a == "" || a != b {
		t.Fatalf("identical marked blocks must share a fingerprint: %q / %q", a, b)
	}
	if a == c {
		t.Fatalf("different marked blocks must differ: %q / %q", a, c)
	}
}
