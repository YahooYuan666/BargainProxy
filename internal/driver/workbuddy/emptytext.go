// Empty-text-part repair (BP-299): the upstream rejects any message whose
// text part is empty (400, code 11151 "a message has empty content"), and it
// SNIFFS string content that trims to a `[`-prefixed JSON object array into a
// parts array first — so tool results carrying scraped form/button JSON die
// on shapes the client never meant as content blocks.
//
// Reference: misakano f987bac (empty_text_part.go, 2026-11-08, field matrix
// on cn pool): [{"type":"text","text":""}|null|missing] → 400; "   " (no
// trim), unknown types, non-array objects → 200. The repair therefore pads
// ONLY the exact empty shapes with a single space and never deletes a part
// (deleting shifts indices other references may rely on); a string is
// re-serialized only when the sniff fires AND a part actually changed — a
// plain request's bytes stay untouched.
package workbuddy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// emptyTextFill is the placeholder for an empty text part: one space. The
// upstream only checks zero length ("   " passes), and the part had nothing
// for the model to read anyway.
const emptyTextFill = " "

// toolImageCarrierFmt introduces the images a tool returned, once they have
// been moved onto a user turn (BP-308, BP-337). It NAMES the tool result the
// images belong to: without that, the carrying message reads as a fresh user
// turn and the model re-litigates who sent the pictures on every request
// (2026-11-23 deepseek-v4.1-flash: eight turns of "the user is showing them
// back to me", the same four screenshots restated each turn).
//
// The shape follows magpie's Chat path (internal/gateway/chat.go seeLater and
// showSeen): the tool message announces what follows, and the carrier names
// its source. A tool result is the harness speaking, not the user, so the
// label is English and takes the tool name and the tool call id.
const toolImageCarrierFmt = "[From the result of %s (tool call %s):]"

// toolImageFollows announces, at the END of the tool message, that its
// result's images follow as the next message. Written into the tool text so
// the model meets the pictures already knowing whose they are. Singular and
// plural read as English, not as a count glued to a fixed noun.
func toolImageFollows(n int) string {
	if n == 1 {
		return "[The tool returned an image; it follows in the next message.]"
	}
	return fmt.Sprintf("[The tool returned %d images; they follow in the next message.]", n)
}

// toolImageLabel is the carrier line naming the tool result the parked images
// came from. An unnamed result keeps a generic word rather than an empty slot:
// the id is what ties the pictures to the call, and the name is what the model
// reads.
func toolImageLabel(toolName, callID string) string {
	name := strings.TrimSpace(toolName)
	if name == "" {
		name = "tool"
	}
	return fmt.Sprintf(toolImageCarrierFmt, name, callID)
}

// padSniffedString repairs string content the upstream would sniff into a
// parts array. Returns s unchanged when the sniff does not fire or no part
// needs padding — the zero-impact guarantee for ordinary requests.
func padSniffedString(s string) string {
	parts := sniffParts(s)
	if parts == nil {
		return s
	}
	if !fillEmptyTextParts(parts) {
		return s
	}
	if nb, err := json.Marshal(parts); err == nil {
		return string(nb)
	}
	return s
}

// sniffParts mirrors the upstream's sniff: a string that trims to a
// `[`-prefixed prefix and parses into a NON-EMPTY array of OBJECTS will be
// read as parts. Anything else (plain text, numbers, `[`-strings that fail to
// parse) stays a string.
func sniffParts(content string) []any {
	stripped := strings.TrimLeft(content, " \t\r\n")
	if !strings.HasPrefix(stripped, "[") {
		return nil
	}
	var parts []any
	if err := json.Unmarshal([]byte(stripped), &parts); err != nil {
		return nil
	}
	if len(parts) == 0 {
		return nil
	}
	for _, p := range parts {
		if _, ok := p.(map[string]any); !ok {
			return nil
		}
	}
	return parts
}

// fillEmptyTextParts pads every empty text part in place; reports whether
// anything changed. Only the exact empty shapes measured upstream are
// touched: text missing, null, or "".
func fillEmptyTextParts(parts []any) bool {
	changed := false
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok || partType(part) != "text" {
			continue
		}
		if !isEmptyText(part) {
			continue
		}
		part["text"] = emptyTextFill
		changed = true
	}
	return changed
}

// partType reads the part's type. The upstream validates ONLY the exact
// "text" type ("textarea" and friends pass) — normalizing here would pull
// unvalidated shapes into validation.
func partType(part map[string]any) string {
	t, _ := part["type"].(string)
	return t
}

// isEmptyText reports whether the upstream judges this text part empty:
// missing, null, or the empty string. Zero length only — no trim ("   "
// passes and padding it would change a message the upstream already accepts).
func isEmptyText(part map[string]any) bool {
	v, has := part["text"]
	if !has || v == nil {
		return true
	}
	s, ok := v.(string)
	if !ok {
		return false
	}
	return s == ""
}
