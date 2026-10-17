package ir

// MergeAdjacentToolCalls normalizes the "back-to-back assistant tool_calls"
// shape some OpenAI-compatible agent clients produce when replaying parallel
// tool calls: several consecutive assistant messages each carrying part of
// the same parallel batch (or a text-only assistant right after a call-only
// one). Upstream DeepSeek-family models reject that shape with 400
// code=11148 (tool_call_sequence_broken) and kill the whole conversation —
// rotating accounts cannot help (ticket 35, agentpoints 0d0e509).
//
// Two forms are merged, both strictly bounded so semantics never change:
//
//	Form 1 (forward): this message is an assistant with tool_calls and EMPTY
//	  content, the previous kept message is an assistant with tool_calls →
//	  its calls are appended to the previous one.
//	Form 2 (reverse): this message is a text-only assistant (no tool_calls),
//	  the previous kept message is an assistant with tool_calls and EMPTY
//	  content → the text is folded into the previous one.
//
// Anything else (non-adjacent, non-empty content, tool/system/user in
// between) is left as-is: guessing would change meaning. Returns the
// (possibly shorter) message list and whether anything changed; when nothing
// changed the original slice is returned unchanged.
func MergeAdjacentToolCalls(messages []Message) ([]Message, bool) {
	if len(messages) < 2 {
		return messages, false
	}
	out := make([]Message, 0, len(messages))
	changed := false
	for _, m := range messages {
		if m.Role == RoleAssistant && len(out) > 0 {
			prev := &out[len(out)-1]
			if prev.Role == RoleAssistant && len(prev.ToolCalls) > 0 {
				// Form 1: call-only fragment joins the previous batch.
				if len(m.ToolCalls) > 0 && emptyContent(m.Content) {
					prev.ToolCalls = append(prev.ToolCalls, m.ToolCalls...)
					changed = true
					continue
				}
				// Form 2: plain text folds into the call-only assistant so
				// "the assistant declaring tool_calls is immediately followed
				// by its own results" holds in both client orderings.
				if len(m.ToolCalls) == 0 && emptyContent(prev.Content) && hasText(m.Content) {
					prev.Content = append(prev.Content, m.Content...)
					changed = true
					continue
				}
			}
		}
		out = append(out, m)
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// RepairToolPairing makes the assistant-tool_calls ↔ tool-result pairing
// internally consistent before a replayed history goes upstream. Strict
// DeepSeek-family upstreams reject any mismatch with code=11148 ("tool calls
// and tool results do not match, please start a new conversation and retry"),
// and the failure is per-request: every resend of the same history fails. The
// bad shapes come from real clients — a turn interrupted by a model switch, a
// pruned tool call whose result stayed behind, results replayed out of order.
//
// Repairs, all local to one call/results group:
//   - a tool message with no immediately-preceding matching call is dropped;
//   - duplicate results for one call id are dropped (first wins);
//   - a call with no result is removed from its assistant turn;
//   - results are reordered to the call order;
//   - an assistant turn left with no content and no calls is dropped.
//
// Valid histories pass through unchanged (changed=false, same slice).
func RepairToolPairing(messages []Message) ([]Message, bool) {
	out := make([]Message, 0, len(messages))
	changed := false
	for i := 0; i < len(messages); {
		m := messages[i]
		if m.Role != RoleAssistant || len(m.ToolCalls) == 0 {
			if m.Role == RoleTool {
				// Only a call group may own tool results; this one has no call
				// group in front of it.
				changed = true
				i++
				continue
			}
			out = append(out, m)
			i++
			continue
		}
		// The contiguous run of tool results that answers this turn.
		j := i + 1
		for j < len(messages) && messages[j].Role == RoleTool {
			j++
		}
		results := messages[i+1 : j]
		byID := map[string]Message{}
		for _, r := range results {
			if _, dup := byID[r.ToolCallID]; dup {
				changed = true
				continue
			}
			byID[r.ToolCallID] = r
		}
		var keptCalls []ToolCall
		var keptResults []Message
		seen := map[string]bool{}
		for _, c := range m.ToolCalls {
			if c.ID == "" || seen[c.ID] {
				changed = true
				continue
			}
			r, ok := byID[c.ID]
			if !ok {
				changed = true // orphan call: no result was ever produced
				continue
			}
			seen[c.ID] = true
			keptCalls = append(keptCalls, c)
			keptResults = append(keptResults, r)
		}
		if len(keptResults) != len(byID) || len(keptCalls) != len(m.ToolCalls) {
			changed = true
		}
		if len(keptCalls) == 0 {
			// Nothing survived: keep the assistant's text (if any), drop the rest.
			if !emptyContent(m.Content) {
				m.ToolCalls = nil
				out = append(out, m)
			}
			i = j
			continue
		}
		m.ToolCalls = keptCalls
		out = append(out, m)
		out = append(out, keptResults...)
		if len(results) == len(keptResults) {
			for k := range results {
				if results[k].ToolCallID != keptResults[k].ToolCallID {
					changed = true // reordered
					break
				}
			}
		}
		i = j
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// emptyContent reports whether a message carries no content at all (missing,
// or only empty text parts).
func emptyContent(parts []ContentPart) bool {
	for _, p := range parts {
		if p.Kind == "text" && p.Text != "" {
			return false
		}
		if p.Kind != "text" {
			return false // images and other parts count as content
		}
	}
	return true
}

// hasText reports whether any part is non-empty text.
func hasText(parts []ContentPart) bool {
	for _, p := range parts {
		if p.Kind == "text" && p.Text != "" {
			return true
		}
	}
	return false
}
