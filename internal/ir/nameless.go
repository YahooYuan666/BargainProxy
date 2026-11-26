// BP-350 (magpie 33b04b64, #7565): a tool call that names no function runs
// nowhere, so it is no call — drop it on both sides of the turn.
//
// # The bug it fixes
//
// GLM-5.3 through Command Code answered with a tool_use whose name was "".
// The client replayed it as a function_call carrying "name":"", and the
// upstream refused that turn — and EVERY later turn of the same conversation —
// with "function_call is missing name". The session was dead and could not
// heal itself: the bad call rode in the history forever.
//
// # Why here
//
// Every protocol writer renders tool calls independently (openai chat,
// anthropic messages + stream, responses, geminiv1), so a guard in one of them
// leaves four holes. The IR layer is the one place both the reply path and the
// replayed history pass through.
//
// # Terminal only
//
// The judgement runs at the END of a turn, never on a delta: a streamed call
// legitimately arrives with its name in a later fragment, and dropping it on
// the first (nameless) frame would delete real calls.
package ir

import (
	"encoding/json"
	"strings"
)

// NormalizeToolCalls drops calls that name no function and renumbers the
// survivors from 0 in their original order. It reports whether the slice
// changed.
//
// The renumbering matters as much as the dropping: clients key parallel tool
// blocks by index and reject a gap (anthropic 400 "tool_use ids must be
// unique"/"index out of order" class), so a batch of three where the middle
// one is dropped must leave indices 0,1 — not 0,2.
//
// ok=false means "nothing to do": either the slice is already clean, or every
// call was nameless. The caller keeps its own ordering; this function only
// removes and renumbers, it never reorders.
func NormalizeToolCalls(calls []ToolCall) ([]ToolCall, bool) {
	if len(calls) == 0 {
		return calls, false
	}
	nameless := 0
	for _, c := range calls {
		if isNamelessCall(c) {
			nameless++
		}
	}
	if nameless == 0 {
		return calls, false
	}
	kept := make([]ToolCall, 0, len(calls)-nameless)
	for _, c := range calls {
		if isNamelessCall(c) {
			continue
		}
		kept = append(kept, c)
	}
	return kept, true
}

// isNamelessCall reports whether a call names no function. An ID or
// arguments without a name is the exact shape that poisons a conversation:
// the client has something to echo back, so it echoes back an unusable call.
func isNamelessCall(c ToolCall) bool {
	return c.Name == ""
}

// NamesNoFunction reports whether a call carries no function name. Exported
// for the streaming writers, which see one fragment at a time and can only ask
// about the accumulated state.
func NamesNoFunction(name string) bool { return name == "" }

// SplitNameless partitions a finished reply's calls into the ones that name a
// function (returned, renumbered from 0 in original order) and counts what the
// reply carried. A writer renders `kept` and passes named/nameless to
// ReplyStopForNameless.
//
// The renumbering is the point: tool blocks are keyed by index downstream, and
// a batch of three with the middle one dropped must present 0,1 — a gap makes
// strict clients reject the whole round.
func SplitNameless(calls []ToolCall) (named bool, nameless bool, kept []ToolCall) {
	if len(calls) == 0 {
		return false, false, nil
	}
	kept = make([]ToolCall, 0, len(calls))
	for _, c := range calls {
		if isNamelessCall(c) {
			nameless = true
			continue
		}
		named = true
		kept = append(kept, c)
	}
	return named, nameless, kept
}

// WithoutNamelessCalls removes from a replayed history every call that names
// no function, together with the tool messages that answered it by id — the
// pair must go at once or what is left no longer pairs and the stricter
// upstreams (workbuddy/zcode DeepSeek family) reject the whole history.
//
// A message left with neither content nor calls is removed. The input slice is
// not modified; changed=false returns the SAME slice (a body with no such call
// goes out byte for byte).
func WithoutNamelessCalls(messages []Message) ([]Message, bool) {
	gone := map[string]bool{}
	found := false
	for _, m := range messages {
		for _, c := range m.ToolCalls {
			if isNamelessCall(c) {
				found = true
				if c.ID != "" {
					gone[c.ID] = true
				}
			}
		}
	}
	if !found {
		return messages, false
	}
	out := make([]Message, 0, len(messages))
	for _, m := range messages {
		if m.Role == RoleTool && m.ToolCallID != "" && gone[m.ToolCallID] {
			continue // the answer to a call that no longer exists
		}
		if len(m.ToolCalls) > 0 {
			if kept, dropped := NormalizeToolCalls(m.ToolCalls); dropped {
				m.ToolCalls = kept
				// An assistant turn that carried ONLY nameless calls keeps its
				// text (if any) and loses the call list — never the reverse.
				if len(kept) == 0 && emptyContent(m.Content) && strings.TrimSpace(m.Reasoning) == "" && len(m.Thinking) == 0 {
					continue
				}
			}
		}
		out = append(out, m)
	}
	return out, true
}

// ReplyStopForNameless adjusts a reply's stop reason after nameless calls were
// dropped: a turn that ended "for tools" with no call left to run leaves an
// agent waiting on nothing, so it reads as an ordinary end.
//
// named and nameless say what the ORIGINAL reply carried. Only the
// "all calls were nameless" case changes the reason; a reply that still has a
// real call keeps StopToolUse.
func ReplyStopForNameless(stop StopReason, named, nameless bool) StopReason {
	if stop == StopToolUse && nameless && !named {
		return StopEnd
	}
	return stop
}

// ArgsAsObject turns a call's argument JSON into the object every
// function-calling wire (Gemini functionCall.args, functionResponse.response)
// requires. BP-356 (CLIProxyAPI c975104f/f7137853): `json.Unmarshal` into a map
// silently yields nil for a non-object body — an array, a scalar, or a
// TRUNCATED fragment from a cut stream — and the caller then sent `{}`,
// throwing the whole argument set away with no trace. The reference wraps
// anything that is not an object as {"params": <raw value>} so the model still
// sees what the tool was asked for.
//
// Shapes:
//   - a JSON object → as-is;
//   - empty / "null" / whitespace → empty object;
//   - valid non-object JSON (array, string, number, bool) → {"params": value};
//   - invalid JSON (a truncation) → {"params": "<the raw text>"}.
//
// The raw text case is a string on purpose: an agent reading a half-written
// object is better served by seeing the fragment than by seeing nothing.
func ArgsAsObject(raw string) map[string]any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return map[string]any{}
	}
	var v any
	// BP-128 (new-api 4d962e4): a plain Unmarshal turns every number into
	// float64, so a 64-bit id like 9007199254740993 comes back …992. UseNumber
	// keeps the digits; json.Number marshals back as the original literal.
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return map[string]any{"params": trimmed}
	}
	if obj, ok := v.(map[string]any); ok {
		if obj == nil {
			return map[string]any{}
		}
		return obj
	}
	if v == nil {
		return map[string]any{}
	}
	return map[string]any{"params": v}
}
