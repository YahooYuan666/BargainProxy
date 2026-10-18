package geminiv1_test

import (
	"bargainproxy/internal/ir"
	"bargainproxy/internal/protocol/geminiv1"
	"bytes"
	"strings"
	"testing"
)

func TestBP76ParallelSameNameResultsSurviveRepair(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		body := `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{"n":1}}},{"functionCall":{"name":"f","args":{"n":2}}}]},{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"n":1}}},{"functionResponse":{"name":"f","response":{"n":2}}},{"text":"continue"}]}]}`
		if explicit {
			body = strings.ReplaceAll(body, `"name":"f","args":{"n":1}`, `"id":"a","name":"f","args":{"n":1}`)
			body = strings.ReplaceAll(body, `"name":"f","args":{"n":2}`, `"id":"b","name":"f","args":{"n":2}`)
			body = strings.ReplaceAll(body, `"name":"f","response":{"n":1}`, `"id":"a","name":"f","response":{"n":1}`)
			body = strings.ReplaceAll(body, `"name":"f","response":{"n":2}`, `"id":"b","name":"f","response":{"n":2}`)
		}
		req, err := geminiv1.ParseRequest([]byte(body), "")
		if err != nil {
			t.Fatal(err)
		}
		messages, changed := ir.RepairToolPairing(req.Messages)
		if changed || len(messages) != 4 || len(messages[0].ToolCalls) != 2 || messages[1].ToolCallID == messages[2].ToolCallID {
			t.Fatalf("same-name parallel results lost explicit=%v: %+v", explicit, messages)
		}
		if ir.TextOf(messages[3].Content) != "continue" {
			t.Fatal("mixed text lost")
		}
	}
}

func TestBP76InterleavedToolArgsAndFailure(t *testing.T) {
	var out bytes.Buffer
	s := geminiv1.NewStreamWriter(&out, nil, "test", false)
	for _, ev := range []ir.StreamEvent{{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{ID: "a", Name: "f"}}, {Kind: ir.EvToolStart, ToolIndex: 1, ToolCall: &ir.ToolCall{ID: "b", Name: "f"}}, {Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{"city":`}, {Kind: ir.EvToolArgs, ToolIndex: 1, Delta: `{"city":"Rome"}`}, {Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `"Paris"}`}, {Kind: ir.EvDone, Stop: ir.StopToolUse}} {
		if err := s.Send(ev); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Count(out.String(), `"functionCall"`) != 2 || !strings.Contains(out.String(), `"city":"Paris"`) || !strings.Contains(out.String(), `"city":"Rome"`) {
		t.Fatal(out.String())
	}
	out.Reset()
	s = geminiv1.NewStreamWriter(&out, nil, "test", false)
	s.Send(ir.StreamEvent{Kind: ir.EvToolStart, ToolIndex: 0, ToolCall: &ir.ToolCall{Name: "f"}})
	s.Send(ir.StreamEvent{Kind: ir.EvToolArgs, ToolIndex: 0, Delta: `{"city":`})
	if err := s.FinishWithError("interrupted"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), `"functionCall"`) || !strings.Contains(out.String(), `"error"`) {
		t.Fatal(out.String())
	}
}
