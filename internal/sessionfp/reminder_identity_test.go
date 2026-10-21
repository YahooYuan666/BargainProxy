package sessionfp

import (
	"testing"

	"bargainproxy/internal/ir"
)

// reminder is the harness boilerplate ZCode/Claude-Code prepend to every
// conversation; hashing it verbatim gave unrelated conversations one identity
// (2026-10-20: two sessions shared a conversation id and the model's answers
// crossed over).
const reminder = "<system-reminder>\nThe following skills are available...\n</system-reminder>\n"

func conv(first string) []ir.Message {
	return []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: reminder}}},
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: first}}},
	}
}

func TestFromMessagesIgnoresSystemReminders(t *testing.T) {
	a := FromMessages(conv("这游戏怎么把语音调回英语"))
	b := FromMessages(conv("本项目的参考项目有哪些"))
	if a == "" || b == "" {
		t.Fatalf("both conversations need an identity: a=%q b=%q", a, b)
	}
	if a == b {
		t.Fatalf("two different conversations got the same fingerprint %q", a)
	}
}

func TestFromMessagesReminderOnlyHasNoIdentity(t *testing.T) {
	msgs := []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: reminder}}},
	}
	if got := FromMessages(msgs); got != "" {
		t.Fatalf("reminder-only conversation must have no identity, got %q", got)
	}
}
