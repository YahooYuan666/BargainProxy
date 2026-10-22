package qoder

import (
	"strings"
	"testing"
)

// Qoder-8 red-first tests (BP-98): models sometimes write tool calls as
// TEXT — <tool_call>{json}</tool_call> or the XML <function=name>
// <parameter=k>v</parameter> form. Those must come out as tool calls, not
// as prose the client cannot act on (magpie-community/plugins packages/qoder
// 0.2.30 Splitter/parseCall, ~600 lines there; we take the two forms and
// the code-fence guard).

func TestParseToolCallBlockJSONForm(t *testing.T) {
	calls, rest := parseToolCallBlocks("before<tool_call>{\"name\":\"Read\",\"arguments\":{\"path\":\"x.go\"}}</tool_call>after")
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Function.Name != "Read" {
		t.Fatalf("name = %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"path":"x.go"}` {
		t.Fatalf("arguments = %q", calls[0].Function.Arguments)
	}
	if rest != "beforeafter" {
		t.Fatalf("surrounding text lost: %q", rest)
	}
}

func TestParseToolCallBlockXMLForm(t *testing.T) {
	calls, _ := parseToolCallBlocks(`<tool_call><function=Read><parameter=path>x.go</parameter><parameter=limit>10</parameter></function></tool_call>`)
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Function.Name != "Read" {
		t.Fatalf("name = %q", calls[0].Function.Name)
	}
	if !strings.Contains(calls[0].Function.Arguments, `"path":"x.go"`) || !strings.Contains(calls[0].Function.Arguments, `"limit":10`) {
		t.Fatalf("arguments = %q", calls[0].Function.Arguments)
	}
}

func TestParseToolCallBlocksMultiple(t *testing.T) {
	calls, _ := parseToolCallBlocks(`<tool_call>{"name":"A","arguments":{}}</tool_call>mid<tool_call>{"name":"B","arguments":{}}</tool_call>`)
	if len(calls) != 2 || calls[0].Function.Name != "A" || calls[1].Function.Name != "B" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestParseToolCallBlocksProseUntouched(t *testing.T) {
	// A tool_call inside a code fence is prose ABOUT one: stays text.
	text := "```json\n<tool_call>{\"name\":\"A\"}</tool_call>\n```"
	calls, rest := parseToolCallBlocks(text)
	if len(calls) != 0 {
		t.Fatalf("code-fenced block must stay text, got %+v", calls)
	}
	if rest != text {
		t.Fatalf("text mangled: %q", rest)
	}
}

func TestParseToolCallBlocksTruncatedStaysText(t *testing.T) {
	// A stream cut off mid-block: the JSON is unreadable, the block stays
	// text (magpie flush() keeps a whole-JSON block, drops a broken one as
	// text rather than emitting the answer's failure).
	calls, rest := parseToolCallBlocks("<tool_call>{\"name\":\"A\",\"arg")
	if len(calls) != 0 {
		t.Fatalf("truncated block must stay text, got %+v", calls)
	}
	if !strings.Contains(rest, "<tool_call>") {
		t.Fatalf("truncated block dropped: %q", rest)
	}
}
