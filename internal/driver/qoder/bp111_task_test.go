// BP-111: task aggregation and stable sessions for the Qoder channel.
//
// The official client sends ONE (session_id, request_set_id, business.id,
// begin_at) tuple per user turn and reuses it across that turn's whole tool
// loop, with business.stage moving start → processing. This driver used to
// mint a fresh session_id per request and a fresh request_set_id/business.id
// per request, so a 50-call tool loop looked like 50 tasks upstream — and the
// aggregate counts task heads ("Billing daily count exceeded" arrives sooner
// the more of them a session creates; magpie 0.2.31 live observation).
package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

// capture collects the wire bodies of every chat call.
type capture struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (c *capture) add(doc map[string]any) {
	c.mu.Lock()
	c.bodies = append(c.bodies, doc)
	c.mu.Unlock()
}

func (c *capture) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func (c *capture) at(i int) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bodies[i]
}

// newAggGateway spins a fake upstream answering every chat with one "ok"
// frame, capturing bodies. No queue detours: pure success path.
func newAggGateway(t *testing.T) (*Driver, *capture) {
	t.Helper()
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/algo/api/v3/user/jobToken":
			fmt.Fprint(w, jobTokenJSON)
		case "/algo/api/v2/model/list":
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":"fallback"}`)
		case "/algo/api/v2/service/pro/sse/agent_chat_generation":
			raw, _ := io.ReadAll(r.Body)
			cap.add(decodeGatewayBody(t, string(raw)))
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, `data: {"body":"{\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}","statusCodeValue":200}`)
			fmt.Fprintln(w, `data: [DONE]`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	d := New(config.QoderSettings{Enabled: true, GatewayBase: srv.URL, OpenBase: srv.URL, MaxInFlight: 1}, nil)
	return d, cap
}

func aggCall(ch, userText string) driver.ChatCall {
	return driver.ChatCall{
		Account: &model.Account{
			ID:        "qoder:agg",
			CredsJSON: []byte(`{"pat":"pt-test","machine_id":"m","machine_token":"mt","machine_type":"mtype"}`),
		},
		UpstreamModel: ch,
		Request: &ir.ChatRequest{
			Model:      ch,
			SessionKey: "conv-1",
			Messages:   []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: userText}}}},
			Stream:     true,
		},
	}
}

func runCall(t *testing.T, d *Driver, call driver.ChatCall) {
	t.Helper()
	if _, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}
}

// wireTuple reads the identity fields off a captured body.
func wireTuple(doc map[string]any) (session, setID, bizID, stage, name string) {
	session, _ = doc["session_id"].(string)
	setID, _ = doc["request_set_id"].(string)
	if biz, ok := doc["business"].(map[string]any); ok {
		bizID, _ = biz["id"].(string)
		stage, _ = biz["stage"].(string)
		name, _ = biz["name"].(string)
	}
	return
}

// TestSameTurnReusesTaskTuple: two wire calls of the SAME user turn (what a
// tool loop produces) must share session_id, request_set_id and business.id,
// with stage moving start → processing; a new turn starts a new task on the
// SAME session.
func TestSameTurnReusesTaskTuple(t *testing.T) {
	d, cap := newAggGateway(t)
	runCall(t, d, aggCall("GLM-5.3", "write a haiku"))
	runCall(t, d, aggCall("GLM-5.3", "write a haiku"))
	if cap.n() != 2 {
		t.Fatalf("captured %d bodies, want 2", cap.n())
	}
	s1, set1, biz1, stage1, name1 := wireTuple(cap.at(0))
	s2, set2, biz2, stage2, name2 := wireTuple(cap.at(1))
	if s1 == "" || s1 != s2 {
		t.Fatalf("session_id must be stable within a conversation: %q vs %q", s1, s2)
	}
	if set1 == "" || set1 != set2 {
		t.Fatalf("request_set_id must be the task identity across a turn's tool loop: %q vs %q", set1, set2)
	}
	if biz1 != set1 || biz2 != set2 {
		t.Fatalf("business.id must equal request_set_id (official client shape): %q vs %q", biz1, set1)
	}
	if stage1 != "start" || stage2 != "processing" {
		t.Fatalf("stage = %q then %q, want start then processing", stage1, stage2)
	}
	if name1 == "" || name1 != name2 {
		t.Fatalf("business.name must be stable within a turn: %q vs %q", name1, name2)
	}
	runCall(t, d, aggCall("GLM-5.3", "now a sonnet"))
	s3, set3, biz3, stage3, _ := wireTuple(cap.at(2))
	if s3 != s1 {
		t.Fatalf("session must hold across turns of one conversation: %q vs %q", s3, s1)
	}
	if set3 == set1 || biz3 != set3 {
		t.Fatalf("a new turn must start a new task: set1=%q set3=%q", set1, set3)
	}
	if stage3 != "start" {
		t.Fatalf("new task stage = %q, want start", stage3)
	}
}

// TestSessionKeyAbsentStillCollapses: without a sticky key the seed comes from
// the conversation's opening (system + first user message), so replays of the
// same conversation still land on one session and one task per turn.
func TestSessionKeyAbsentStillCollapses(t *testing.T) {
	d, cap := newAggGateway(t)
	for i := 0; i < 2; i++ {
		call := aggCall("GLM-5.3", "list the files")
		call.Request.SessionKey = ""
		call.Request.Messages = append([]ir.Message{{
			Role:    ir.RoleSystem,
			Content: []ir.ContentPart{{Kind: "text", Text: "you are a helpful coder"}},
		}}, call.Request.Messages...)
		runCall(t, d, call)
	}
	if cap.n() != 2 {
		t.Fatalf("captured %d bodies", cap.n())
	}
	s1, set1, _, stage1, _ := wireTuple(cap.at(0))
	s2, set2, _, stage2, _ := wireTuple(cap.at(1))
	if s1 == "" || s1 != s2 {
		t.Fatalf("derived session must be stable: %q vs %q", s1, s2)
	}
	if set1 != set2 || stage1 == stage2 {
		t.Fatalf("same turn must reuse the task: set %q/%q stage %q/%q", set1, set2, stage1, stage2)
	}
}

// TestCacheControlOnLastUserBlock: the last block of the last user message
// carries cache_control ephemeral — the official client's cache point. Without
// it Qoder's per-turn cache hit rate is whatever its implicit policy leaves.
func TestCacheControlOnLastUserBlock(t *testing.T) {
	d, cap := newAggGateway(t)
	runCall(t, d, aggCall("GLM-5.3", "hi there"))
	if cap.n() != 1 {
		t.Fatalf("captured %d bodies", cap.n())
	}
	msgs, _ := cap.at(0)["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages on the wire")
	}
	marked := false
	for i := len(msgs) - 1; i >= 0; i-- {
		m, _ := msgs[i].(map[string]any)
		if m == nil || m["role"] != "user" {
			continue
		}
		contents, _ := m["contents"].([]any)
		if len(contents) == 0 {
			t.Fatalf("last user message has no contents array: %v", m)
		}
		block, _ := contents[len(contents)-1].(map[string]any)
		cc, _ := block["cache_control"].(map[string]any)
		if cc != nil && cc["type"] == "ephemeral" {
			marked = true
		}
		break
	}
	if !marked {
		t.Fatal("last block of the last user message must carry cache_control ephemeral")
	}
}

// TestTaskNameIsPromptHead: the business name follows the official client
// (first ten RUNES of the prompt). The old fixed "hi" was a fingerprint.
func TestTaskNameIsPromptHead(t *testing.T) {
	if got := taskNameFor("写一首关于秋天的诗"); got != "写一首关于秋天的诗" {
		t.Fatalf("short prompt name = %q", got)
	}
	if got := taskNameFor("abcdefghijklmnopqrstuvwxyz"); got != "abcdefghij" {
		t.Fatalf("long prompt name = %q, want the first ten RUNES", got)
	}
	if got := taskNameFor("   "); got != "" {
		t.Fatalf("blank prompt name = %q", got)
	}
}

// TestTaskTableEviction: the table must not grow unbounded across many turns.
func TestTaskTableEviction(t *testing.T) {
	s := newSessionTable()
	now := time.Now()
	for i := 0; i < taskMax+50; i++ {
		s.taskFor(fmt.Sprintf("fp-%d", i), "t", now)
	}
	s.mu.Lock()
	n := len(s.tasks)
	s.mu.Unlock()
	if n > taskMax {
		t.Fatalf("task table grew to %d, must be capped at %d", n, taskMax)
	}
}

// TestBodyKeepsCoreFields: no field of the old wire body lost by the rewrite.
func TestBodyKeepsCoreFields(t *testing.T) {
	d, cap := newAggGateway(t)
	runCall(t, d, aggCall("GLM-5.3", "hi"))
	doc := cap.at(0)
	for _, k := range []string{"request_id", "chat_record_id", "request_set_id", "session_id", "stream", "model_config", "messages", "business"} {
		if _, ok := doc[k]; !ok {
			t.Fatalf("wire body lost field %q", k)
		}
	}
	if doc["stream"] != true {
		t.Fatal("stream must be forced true")
	}
	b, _ := json.Marshal(doc["business"])
	if len(b) == 0 || string(b) == "{}" {
		t.Fatalf("business must be populated, got %s", b)
	}
}
