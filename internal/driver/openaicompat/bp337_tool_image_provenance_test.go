// BP-337: a tool's returned images must be told AS a tool's, never as the
// user's — same fix as the workbuddy driver (see its
// bp337_tool_image_provenance_test.go for the full incident). A bare carrier
// line had the model re-litigate who sent the pictures on every request.
//
// The shape follows magpie's Chat path (internal/gateway/chat.go seeLater /
// showSeen): the tool message announces what follows, and the carrier names
// the tool call the images came from.
package openaicompat

import (
	"strings"
	"testing"
)

// bp337Messages returns the wire messages as maps.
func bp337Messages(body map[string]any) []map[string]any {
	raw, _ := body["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		if mm, ok := m.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

// bp337TextOf returns a message's text: string content, or its text parts
// joined.
func bp337TextOf(m map[string]any) string {
	switch c := m["content"].(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			pm, _ := p.(map[string]any)
			if pm["type"] == "text" {
				if s, ok := pm["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	case []map[string]any:
		var b strings.Builder
		for _, pm := range c {
			if pm["type"] == "text" {
				if s, ok := pm["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// TestBP337ToolMessageAnnouncesFollowingImages: the tool message must say an
// image follows, so the model reads the next message as that tool's output.
func TestBP337ToolMessageAnnouncesFollowingImages(t *testing.T) {
	body := bp308Build(t, false)
	found := false
	for _, m := range bp337Messages(body) {
		if m["role"] != "tool" {
			continue
		}
		found = true
		if !strings.Contains(bp337TextOf(m), "follow") {
			t.Errorf("tool message does not announce the images that follow it: %q", bp337TextOf(m))
		}
	}
	if !found {
		t.Fatal("no tool message in wire body")
	}
}

// TestBP337CarrierNamesTheToolCall: the carrying user message must say which
// tool result the images belong to.
func TestBP337CarrierNamesTheToolCall(t *testing.T) {
	body := bp308Build(t, false)
	carrier := false
	for _, m := range bp337Messages(body) {
		if m["role"] != "user" {
			continue
		}
		text := bp337TextOf(m)
		if strings.Contains(text, "From the result of") {
			carrier = true
			if !strings.Contains(text, "Read") {
				t.Errorf("carrier does not name the tool: %q", text)
			}
			if !strings.Contains(text, "call_x") {
				t.Errorf("carrier does not name the tool call id: %q", text)
			}
		}
	}
	if !carrier {
		t.Fatal("no user message attributing the images to a tool result")
	}
}

// TestBP337OldBareCarrierGone: the un-attributed carrier must be gone.
func TestBP337OldBareCarrierGone(t *testing.T) {
	for _, withTurn := range []bool{false, true} {
		body := bp308Build(t, withTurn)
		for _, m := range bp337Messages(body) {
			if strings.Contains(bp337TextOf(m), "（工具返回的图片如下）") {
				t.Errorf("withFollowingUserTurn=%v: bare carrier still on the wire: %q",
					withTurn, bp337TextOf(m))
			}
		}
	}
}
