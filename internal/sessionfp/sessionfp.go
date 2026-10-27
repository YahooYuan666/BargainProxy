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

// NativeHeaders are where agents already name their own conversation. The
// list is ordered most-specific first, matching what the reference gateways
// read (magpie's sessionHeaders, 2026-10-21). Reading these first matters:
// a client that names its session knows the truth, while a first-message
// digest can collide when two chats open with the same boilerplate (the
// 2026-10-20 cross-talk bug, where ZCode's identical <system-reminder>
// prefix made two sessions share one key).
var NativeHeaders = []string{
	"X-BargainProxy-Session",
	"X-Opencode-Session",
	"X-Session-Affinity",
	"X-Session-Id",
	"Session-Id",
	"X-Claude-Code-Session-Id",
	"X-Conversation-Id",
	"X-Deepseek-Harness-Session-Id",
}

// SubagentHeaders name the SUBAGENT a request belongs to, when the client
// sends one. Claude Code sends its session id with every request of the
// session — its subagents' too — and only the agent id distinguishes a
// subagent's turn (yetone/magpie 3b717d5, 2026-10-25).
//
// Why it has to split the key: stickiness is one binding per
// (channel, session). A subagent that runs on the lead's model wrote its own
// answer over the lead's binding, so the lead's next turn was judged by the
// subagent's cache — a small one reads as "no cache", a 5-minute write reads
// as "cold" — and the conversation moved to another account, where the whole
// history is re-written at the cache-write price. Upstream measured two Pro
// accounts' 5 hours spent in ten minutes (~318k tokens a request).
var SubagentHeaders = []string{
	"X-Claude-Code-Agent-Id",
}

// FromHeader returns the conversation id the client named, "" when it named
// none. Values are hashed so a raw client id never reaches logs or upstream
// headers. A blank or absurdly long value is ignored rather than trusted.
//
// BP-143: when the request also names a subagent, the key gets a second
// segment. The lead keeps the plain session key it always had (so bindings
// written before this change still resolve), while each subagent gets its
// own — a subagent's answer can no longer move the lead off its account.
func FromHeader(get func(string) string) string {
	if get == nil {
		return ""
	}
	for _, h := range NativeHeaders {
		v := strings.TrimSpace(get(h))
		if v == "" || len(v) > 256 {
			continue
		}
		key := digest("sid:", v)
		if sub := subagentOf(get); sub != "" {
			key += "#sub:" + sub
		}
		return key
	}
	return ""
}

// subagentOf returns the hashed subagent id the request carries, "" for a
// lead conversation (or a client that does not name subagents at all).
func subagentOf(get func(string) string) string {
	for _, h := range SubagentHeaders {
		v := strings.TrimSpace(get(h))
		if v == "" || len(v) > 256 {
			continue
		}
		return strings.TrimPrefix(digest("sub:", v), "sub:")
	}
	return ""
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
			if firstUser != "" {
				continue
			}
			// Claude-Code-style clients prepend the same <system-reminder>
			// skill/context blocks to EVERY conversation. Hashing the first
			// user message verbatim therefore hands different conversations the
			// same fingerprint (2026-10-20 field bug: two ZCode sessions got one
			// conversation id, and the AutoClaw relay pooled both into its
			// "main" sandbox session, so answers and images leaked across
			// conversations). Use the first SUBSTANTIVE user text instead.
			if t := stripReminders(textOf(m.Content)); strings.TrimSpace(t) != "" {
				firstUser = t
			}
		}
	}
	if firstUser == "" {
		// No substantive user text (a degenerate request or a harness-only
		// prelude). A non-empty system prompt still identifies it; with
		// neither, report "no identity" rather than inventing a shared key.
		if strings.TrimSpace(system) == "" {
			return ""
		}
		return Prefix(system, "")
	}
	return Prefix(system, firstUser)
}

// stripReminders removes <system-reminder>...</system-reminder> blocks, which
// are harness boilerplate rather than conversation content.
func stripReminders(s string) string {
	for {
		i := strings.Index(s, "<system-reminder>")
		if i < 0 {
			break
		}
		j := strings.Index(s[i:], "</system-reminder>")
		if j < 0 {
			s = s[:i]
			break
		}
		s = s[:i] + s[i+j+len("</system-reminder>"):]
	}
	return strings.TrimSpace(s)
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
