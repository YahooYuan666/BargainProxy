// Package opencodezen implements the OpenCode Zen anonymous free-model
// channel: OpenAI chat-completions wire upstream with a hard free-tier gate —
// requests must stream and must carry "bash"+"read" function tools, plus a
// full CLI disguise header set — or the upstream answers 403 FreeTierError.
//
// Behaviour constants and rules are clean-room ported from the MIT-licensed
// opencode2zcode project (rust/src/zen.rs, https://github.com/YahooYuan666/
// opencode2zcode, MIT © 2026; see NOTICE). The disguise material (client
// version, header names, session-id shape) is what the official OpenCode CLI
// itself sends; it is not a secret and lives in one place so an upstream
// tightening is a one-constant change.
package opencodezen

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Anonymous free-lane bearer: a fixed public placeholder, not a credential.
const AnonymousKey = "public"

// openCodeClientVersion is the CLI version the disguise mimics. Upstream
// tightens against old versions; bump here when it does (2026-10-18: the
// free lane started answering 400 FreeTierError to 1.18.31).
const openCodeClientVersion = "1.19.3"

// gateToolNames are the two function tools the free lane requires on every
// request (live 2026-10-16: a request without them answers 403
// FreeTierError). When the client did not bring them we inject shadow
// definitions and strip the model's calls to them from the response.
var gateToolNames = [2]string{"bash", "read"}

// acceptedEfforts are the reasoning_effort values the upstream actually
// accepts; anything else (off/none/unknown) must be omitted, not passed
// through — passing "none" is a 400.
var acceptedEfforts = map[string]bool{
	"minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true,
}

const gateDescription = "Reserved for the host runtime; do not call it."

// --- identity: canonical session/request/project ids ---

// invertedTimestampHex mimics the official Identifier.descending(): the
// 48-bit complement of (now_ms * 0x1000 + 1), 12 hex chars. With counter=1
// the low bits always invert to a "…ffe" tail, which the upstream validates
// (check added live 2026-10-13).
func invertedTimestampHex(nowMillis uint64) string {
	current := uint64(nowMillis)*0x1000 + 1
	inverted := ^current & 0xFFFFFFFFFFFF
	return fmt.Sprintf("%012x", inverted)
}

func isDescendingTimePart(part string) bool {
	if len(part) != 12 {
		return false
	}
	for i := 0; i < 12; i++ {
		c := part[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return part[9] == 'f' && part[10] >= 'c' && part[10] <= 'f'
}

// isCanonicalSession reports whether value already has the official shape
// "ses_" + 12 hex + 14 base62 — such values pass through untouched.
func isCanonicalSession(value string) bool {
	rest, ok := strings.CutPrefix(value, "ses_")
	if !ok || len(rest) != 26 {
		return false
	}
	if !isDescendingTimePart(rest[:12]) {
		return false
	}
	for i := 12; i < 26; i++ {
		c := rest[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func base62Fixed(value uint64, width int) string {
	out := []byte(strings.Repeat("0", width))
	for i := width - 1; i >= 0; i-- {
		out[i] = base62Alphabet[value%62]
		value /= 62
	}
	return string(out)
}

func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// sessionMemo keeps the session id stable across the retries/multi-turns of
// one conversation despite the timestamp inside its shape: keyed by the
// conversation seed, capped, like the reference implementation.
var (
	sessionMemoMu sync.Mutex
	sessionMemo   = map[string]string{}
	sessionMemoQ  []string
)

const sessionMemoLimit = 1024

// canonicalSessionID returns the canonical session id for a conversation
// seed, minting (and memoizing) one when needed.
func canonicalSessionID(seed string) string {
	if isCanonicalSession(seed) {
		return seed
	}
	sessionMemoMu.Lock()
	defer sessionMemoMu.Unlock()
	if v, ok := sessionMemo[seed]; ok {
		return v
	}
	now := uint64(time.Now().UnixMilli())
	timePart := invertedTimestampHex(now)
	tail, _ := strconvParseUint64(sha256Hex("ses\x00" + seed)[12:32])
	session := "ses_" + timePart + base62Fixed(tail, 14)
	sessionMemo[seed] = session
	sessionMemoQ = append(sessionMemoQ, seed)
	if len(sessionMemoQ) > sessionMemoLimit {
		delete(sessionMemo, sessionMemoQ[0])
		sessionMemoQ = sessionMemoQ[1:]
	}
	return session
}

func strconvParseUint64(s string) (uint64, error) {
	var v uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		var d uint64
		switch {
		case c >= '0' && c <= '9':
			d = uint64(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint64(c-'a') + 10
		default:
			return v, fmt.Errorf("bad hex")
		}
		v = v*16 + d
	}
	return v, nil
}

// mintRequestID produces the official request-id shape:
// "msg_" + 12 ascending-hex timestamp + 14 base62 random.
func mintRequestID() string {
	current := uint64(time.Now().UnixMilli())*0x1000 + 1
	timePart := fmt.Sprintf("%012x", current&0xFFFFFFFFFFFF)
	var randBytes [8]byte
	_, _ = rand.Read(randBytes[:])
	randVal := uint64(randBytes[0])<<56 | uint64(randBytes[1])<<48 |
		uint64(randBytes[2])<<40 | uint64(randBytes[3])<<32 |
		uint64(randBytes[4])<<24 | uint64(randBytes[5])<<16 |
		uint64(randBytes[6])<<8 | uint64(randBytes[7])
	return "msg_" + timePart + base62Fixed(randVal, 14)
}

// projectID is a stable per-installation project marker (not a secret).
func projectID() string {
	return "prj_" + sha256Hex("prj\x00bargainproxy:default-project")[:24]
}

// requestIDs bundles the correlation ids derived from one request.
type requestIDs struct {
	session string
	request string
	project string
}

// conversationSeed is the first user message's JSON-encoded content — the
// same conversation always derives the same session id (quota/cache affinity
// upstream). Falls back to randomness when nothing usable is present.
func conversationSeed(body []byte) string {
	var doc struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return ""
	}
	pick := func(content json.RawMessage) string {
		if len(content) == 0 || string(content) == "null" {
			return ""
		}
		// Compact to keep the seed stable across insignificant whitespace.
		var v any
		if err := json.Unmarshal(content, &v); err != nil {
			return ""
		}
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
	for _, m := range doc.Messages {
		if m.Role != "user" {
			continue
		}
		if seed := pick(m.Content); seed != "" && seed != "null" {
			return seed
		}
	}
	// Responses wire: input may be a plain string.
	if len(doc.Input) > 0 {
		var sv string
		if err := json.Unmarshal(doc.Input, &sv); err == nil && sv != "" {
			b, err := json.Marshal(sv)
			if err == nil {
				return string(b)
			}
		}
	}
	return ""
}

// deriveRequestIDs builds the correlation ids for one upstream request.
func deriveRequestIDs(body []byte) requestIDs {
	seed := conversationSeed(body)
	if seed == "" {
		seed = "fallback_" + randomHex32()
	}
	return requestIDs{
		session: canonicalSessionID(seed),
		request: mintRequestID(),
		project: projectID(),
	}
}

func randomHex32() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// openCodeUserAgent mimics the official CLI UA string.
func openCodeUserAgent() string {
	return fmt.Sprintf("opencode/%s (windows amd64; node20)", openCodeClientVersion)
}

// disguiseHeaders returns the full header set every upstream request carries.
func disguiseHeaders(ids requestIDs) [][2]string {
	return [][2]string{
		{"user-agent", openCodeUserAgent()},
		{"x-opencode-client", "cli"},
		{"x-opencode-session", ids.session},
		{"x-session-affinity", ids.session},
		{"x-session-id", ids.session},
		{"x-opencode-request", ids.request},
		{"x-opencode-project", ids.project},
		{"authorization", "Bearer " + AnonymousKey},
	}
}

// --- free-lane request shaping ---

func gateToolChat(name string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": gateDescription,
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

func gateToolResponses(name string) map[string]any {
	return map[string]any{
		"type":        "function",
		"name":        name,
		"description": gateDescription,
		"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

// toolNameOf reads a tool's name from either wire shape.
func toolNameOf(tool map[string]any) string {
	if fn, ok := tool["function"].(map[string]any); ok {
		if s, ok := fn["name"].(string); ok {
			return s
		}
	}
	if s, ok := tool["name"].(string); ok {
		return s
	}
	return ""
}

// toolsOfAny normalizes body["tools"] to []any regardless of how the caller
// built it. A plain []any assertion used to fail for a concretely-typed
// []map[string]any slice and silently replaced every real tool with the gate
// stubs — the worst kind of bug, because the request still returned 200.
func toolsOfAny(v any) []any {
	switch ts := v.(type) {
	case []any:
		return ts
	case []map[string]any:
		out := make([]any, 0, len(ts))
		for _, t := range ts {
			out = append(out, t)
		}
		return out
	default:
		return nil
	}
}

// ensureGateTools injects the missing gate tools into an already-built wire
// body (map form). Chat lane with no tools at all also gets
// tool_choice:"none" so the model does not call the stubs; the Responses lane
// must never receive tool_choice:"none" (upstream 400) — an existing "none"
// is stripped instead. Returns the number of injected tools.
func ensureGateTools(body map[string]any, responsesLane bool) int {
	tools := toolsOfAny(body["tools"])
	wasEmpty := len(tools) == 0
	injected := 0
	for _, gate := range gateToolNames {
		found := false
		for _, t := range tools {
			if tm, ok := t.(map[string]any); ok && toolNameOf(tm) == gate {
				found = true
				break
			}
		}
		if !found {
			if responsesLane {
				tools = append(tools, gateToolResponses(gate))
			} else {
				tools = append(tools, gateToolChat(gate))
			}
			injected++
		}
	}
	if injected == 0 {
		return 0
	}
	body["tools"] = tools
	if wasEmpty && !responsesLane {
		body["tool_choice"] = "none"
	}
	if responsesLane {
		if s, ok := body["tool_choice"].(string); ok && s == "none" {
			delete(body, "tool_choice")
		}
	}
	return injected
}

// gateToolSet is the set of names we may have injected; calls to them are
// stripped from the response before it reaches the client.
var gateToolSet = map[string]bool{gateToolNames[0]: true, gateToolNames[1]: true}

// isGateToolName reports whether name is one of the free-lane gate tools.
func isGateToolName(name string) bool { return gateToolSet[name] }
