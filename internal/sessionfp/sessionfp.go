// Package sessionfp derives conversation fingerprints for sticky routing
// when a client provides no explicit session id. The ingress applies a
// three-level fallback: explicit ids (protocol-specific) → cache_control
// marked-block digest → system+first-user prefix digest. Fingerprints are
// independent of the model name, so switching models mid-conversation does
// not change the identity (the pool keeps one binding per channel).
package sessionfp

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"bargainproxy/internal/ir"
)

// shortHashBytes keeps fingerprints compact (32 hex chars) while staying
// collision-safe for a personal gateway's conversation counts.
const shortHashBytes = 16

func digest(kind, content string) string {
	sum := sha256.Sum256([]byte(content))
	return kind + hex.EncodeToString(sum[:shortHashBytes])
}

// CacheControl fingerprints a cache_control-marked content block by its
// text. Callers pass the first marked block found in document order: in
// prompt-caching practice the first breakpoint (system-prompt anchor) stays
// put as the conversation grows, while later breakpoints move with the
// tail — hashing the first one therefore yields a stable per-conversation
// key. Empty text yields no fingerprint.
func CacheControl(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return digest("cc:", text)
}

// Prefix fingerprints the stable conversation prefix: system text + first
// user message text. As a conversation grows, later turns append after the
// prefix, so the fingerprint is stable per conversation; distinct
// conversations differ in their opening messages. Empty when the request
// carries no usable text (no stickiness, never a shared key).
func Prefix(system, firstUser string) string {
	if system == "" && firstUser == "" {
		return ""
	}
	return digest("pfx:", system+"\x00"+firstUser)
}

// FromMessages derives the prefix fingerprint from parsed IR messages.
func FromMessages(msgs []ir.Message) string {
	var system, firstUser string
	for _, m := range msgs {
		switch m.Role {
		case ir.RoleSystem:
			system += textOf(m.Content)
		case ir.RoleUser:
			if firstUser == "" {
				firstUser = textOf(m.Content)
			}
		}
	}
	return Prefix(system, firstUser)
}

func textOf(parts []ir.ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Kind == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
