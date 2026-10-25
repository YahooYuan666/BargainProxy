// Task aggregation and stable session ids for the Qoder channel (BP-111).
//
// # WHY THE IDS MATTER
//
// The official Qoder client sends ONE (session_id, request_set_id,
// business.id, begin_at) tuple per user turn: every tool call and result
// round-trip of that turn REUSES the tuple, with business.stage moving from
// "start" to "processing". This driver used to mint a fresh session_id per
// HTTP request and a fresh request_set_id unless the queue protocol handed
// one back — so a turn with 50 tool round-trips showed up upstream as 50
// sessions and 50 tasks, instead of one task with 50 calls. magpie 0.2.31
// observed the aggregate being billed as separate tasks ("Billing daily
// count exceeded" arrives sooner the more task heads you create), and
// 0.2.26/0.2.33 added cache marking and de-branded business.name on the same
// body.
//
// # WHAT STAYS PER-REQUEST
//
// request_id and chat_record_id stay a fresh UUID per HTTP request: they name
// THIS wire call, and the queue protocol (BP-92) keys its resends on
// request_set_id, not on these.
package qoder

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

const (
	// taskIdle is how long a task may sit untouched before a new turn with
	// the same fingerprint starts a NEW task instead of rejoining. Matches
	// magpie's 2h idle eviction.
	taskIdle = 2 * time.Hour
	// taskMax bounds the table; eviction is LRU by last use.
	taskMax = 512
)

// taskState is one upstream "agent task": the identity a whole user turn's
// tool loop shares.
type taskState struct {
	id       string
	begin    int64 // ms, business.begin_at
	name     string
	answered bool // any call answered → stage "processing"
	lastUsed time.Time
}

// sessionTable carries the per-account aggregation state. One live per
// account (runtime), guarded as a whole: the cost of one mutex around a map
// lookup is nothing next to a multi-second upstream call.
type sessionTable struct {
	mu      sync.Mutex
	tasks   map[string]*taskState // fingerprint → task
	sessOf  map[string]string     // fingerprint → stable session_id
	lastGCC time.Time
}

func newSessionTable() *sessionTable {
	return &sessionTable{
		tasks:   map[string]*taskState{},
		sessOf:  map[string]string{},
		lastGCC: time.Now(),
	}
}

// turnFingerprint names the turn: the conversation's session seed plus the
// latest user text. Two wire calls belong to the same task iff both match —
// which is exactly "the same user turn still running its tool loop".
func turnFingerprint(sessionSeed, latestUserText string) string {
	h := sha256.Sum256([]byte(sessionSeed + "\x00" + latestUserText))
	return hex.EncodeToString(h[:])
}

// sessionIDFor returns the stable session_id for one conversation. The seed
// is the gateway's sticky session key when the caller supplied one (it is
// derived per conversation upstream of the driver), else a hash of the
// conversation's opening (system + first user message), so replays that lost
// the sticky key still collide onto the same session. The value is a UUID
// v4-shaped 36-char id built from the hash — Qoder only sees the shape, and
// nothing of the client's own identifiers crosses the wire verbatim.
func (s *sessionTable) sessionIDFor(seed string) string {
	if seed == "" {
		return newUUID()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.sessOf[seed]; ok {
		return id
	}
	h := sha256.Sum256([]byte("qoder-session\x00" + seed))
	id := formatUUIDv4Shape(h[:])
	s.sessOf[seed] = id
	return id
}

// taskFor returns the task a turn belongs to: created on the first wire call
// of the turn, reused on every call after. stage tells the caller what to put
// in business.stage.
func (s *sessionTable) taskFor(fp, name string, now time.Time) (t *taskState, stage string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Reuse first: a matching live task rejoins (stage processing) and no new
	// entry is created, so the cap below sees the steady-state size.
	if t, ok := s.tasks[fp]; ok && now.Sub(t.lastUsed) <= taskIdle {
		t.lastUsed = now
		t.answered = true
		return t, "processing"
	}
	t = &taskState{
		id:       newUUID(),
		begin:    now.UnixMilli(),
		name:     name,
		lastUsed: now,
	}
	s.tasks[fp] = t
	// Cap AFTER the insert, so the invariant "table never exceeds taskMax"
	// holds on the way out of every call (a pre-insert gc would let a burst
	// grow the table to taskMax+1 between gc ticks).
	s.gcLocked(now)
	return t, "start"
}

// gcLocked evicts idle tasks and caps the table. Caller holds s.mu.
//
// The idle sweep is throttled to once a minute, but the CAP is not: a burst
// of new turns within that minute must still be bounded (the eviction test
// inserts taskMax+50 at one timestamp). The overflow trim drops the oldest
// entries until the table fits.
func (s *sessionTable) gcLocked(now time.Time) {
	if now.Sub(s.lastGCC) >= time.Minute {
		s.lastGCC = now
		for k, t := range s.tasks {
			if now.Sub(t.lastUsed) > taskIdle {
				delete(s.tasks, k)
			}
		}
	}
	for len(s.tasks) > taskMax {
		var oldestK string
		var oldest time.Time
		first := true
		for k, t := range s.tasks {
			if first || t.lastUsed.Before(oldest) {
				oldestK, oldest, first = k, t.lastUsed, false
			}
		}
		delete(s.tasks, oldestK)
	}
}

// formatUUIDv4Shape renders 16 bytes as 8-4-4-4-12 with the v4 variant bits
// set, matching the id shapes the official client sends.
func formatUUIDv4Shape(b []byte) string {
	if len(b) < 16 {
		return ""
	}
	b = append([]byte(nil), b[:16]...)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	h := hex.EncodeToString(b)
	return strings.Join([]string{h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]}, "-")
}

// taskNameFor is the business record's name: the official client uses the
// prompt's first ten characters (qoder-agent-sdk AgentLifecycle). The
// previous wire value was a fixed "hi" — a fingerprint of its own.
func taskNameFor(latestUserPrompt string) string {
	r := []rune(strings.TrimSpace(latestUserPrompt))
	if len(r) > 10 {
		return string(r[:10])
	}
	return string(r)
}

// sessionSeedOf derives the conversation seed when the gateway did not supply
// a sticky session key: the system prompt plus the first user message, hashed
// by sessionIDFor. Same conversation → same seed → same session_id.
func sessionSeedOf(messages []map[string]interface{}) string {
	var sys, firstUser string
	for _, m := range messages {
		role, _ := m["role"].(string)
		text := normalizeMessageText(m)
		switch role {
		case "system", "developer":
			if sys == "" {
				sys = text
			}
		case "user":
			if firstUser == "" && strings.TrimSpace(text) != "" {
				firstUser = text
			}
		}
		if sys != "" && firstUser != "" {
			break
		}
	}
	return sys + "\x00" + firstUser
}

// markQoderCache stamps the LAST user message's last content block with
// cache_control ephemeral, the way the official client marks its cache point
// (magpie 0.2.26, and our own zcode driver does the same for Anthropic).
// Qoder's servers keep a prompt cached only where the client asked; without
// the mark the per-turn cache hit rate is whatever the upstream's implicit
// policy leaves. Best effort: messages without a contents array are left
// untouched.
func markQoderCache(msgs []qoderMessage) {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := &msgs[i]
		if m.Role != "user" || len(m.Contents) == 0 {
			continue
		}
		for j := len(m.Contents) - 1; j >= 0; j-- {
			p := &m.Contents[j]
			if p.Type == "" { // unknown block shape: skip
				continue
			}
			p.CacheControl = map[string]string{"type": "ephemeral"}
			return
		}
		return
	}
}
