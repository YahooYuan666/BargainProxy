// Chat-sessions transport for the Trae channel.
//
// # WHY THIS EXISTS
//
// Trae exposes two completely different chat surfaces, and they do NOT share a
// context ceiling:
//
//	POST /api/agent/v3/llm_utils_chat   (the SOLO function ladder)
//	     max input 262144. Asking for more answers code 4026
//	     "your context length has exceeded the maximum limit". This is what the
//	     driver used exclusively until 2026-10-23.
//
//	POST /api/remote/v1/chat_sessions   (the desktop client's own chat surface)
//	     max input 1000000 per model catalogue (context_window_tokens.max),
//	     verified live: 300k / 600k / 900k prompt content all answered, one
//	     1.2M request was still running when the probe gave up.
//
// The model catalogue documents the split explicitly — every model that
// publishes max=1000000 carries max_mode=true, and the `dev` window (200000)
// is what llm_utils_chat serves:
//
//	GET /api/remote/v1/models
//	  deepseek-v4.1-flash  context_window_tokens {dev:200000, max:1000000}  max_mode:true
//	  glm-5.3-flash        context_window_tokens {dev:200000, max:1000000}  max_mode:true
//
// # THE TRADE-OFF, AND WHY THIS IS NOT A DROP-IN REPLACEMENT
//
// chat_sessions runs the vendor's own agent inside a sandbox. That agent has
// its own tools (RunCommand, file edits, a browser) and it IGNORES a caller's
// tool declarations entirely. Verified: a request declaring a `lookup_secret`
// function answered "I can't do that — there's no tool named `lookup_secret` in
// this environment." So a request that carries tools must keep using
// llm_utils_chat, or every MCP tool the client shipped would be dropped and
// the model would behave like a different agent.
//
// Therefore the channel is split by request shape:
//
//	tools present  → llm_utils_chat (262144 ceiling, full tool support)
//	tools absent   → chat_sessions   (1M ceiling, plain question/answer)
//
// Reference projects for comparison (2026-10-23): magpie's trae plugin uses
// llm_utils_chat only and never mentions 4026; apmengzi/autoclaw-switch's
// relay uses chat_sessions but flattens the whole conversation into one prompt
// (stateless, no tools, no multi-turn). Neither solves this.
package trae

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/httpx"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/uperr"
)

// chatSessionMaxInput is the prompt ceiling the session surface documents for
// max_mode models. It is the vendor's own number (context_window_tokens.max),
// not a measurement of the wall.
const chatSessionMaxInput = 1000000

// sessionPollInterval / sessionPollBudget bound the "no SSE available" path:
// when the event stream closes early (it does — the server closes it after the
// first burst) the answer is read back from the messages endpoint.
const (
	sessionPollInterval = 2 * time.Second
	sessionPollBudget   = 10 * time.Minute
)

// sessionCreateResp is the create-session envelope.
type sessionCreateResp struct {
	Code int    `json:"code"`
	Msg  string `json:"message"`
	Data struct {
		ChatSessionID string `json:"chat_session_id"`
		Status        int    `json:"status"`
		MessageID     string `json:"message_id"`
	} `json:"data"`
}

// sessionMessage is one entry of the messages list. Only the fields that carry
// an answer are modelled.
type sessionMessage struct {
	MessageID   string `json:"message_id"`
	Role        string `json:"role"`
	Status      string `json:"status"`
	MessageType string `json:"message_type"`
	Content     string `json:"content"`
}

// streamViaSessions runs one turn on the /api/remote/v1 chat-session surface.
// It is used only for requests that carry no tools (see the package comment).
func (d *Driver) streamViaSessions(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	creds, err := ParseCreds(call.Account.Credentials())
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	sid, err := d.createSession(ctx, creds, call)
	if err != nil {
		return nil, err
	}

	resp := &ir.ChatResponse{
		Model:     call.Request.Model,
		CreatedAt: time.Now().Unix(),
		Message:   ir.Message{Role: ir.RoleAssistant},
	}
	// The event stream is the fast path; if it closes without a terminal frame
	// the messages endpoint is authoritative. Both paths funnel into the same
	// extraction so the caller cannot tell which one answered.
	text, reasoning, stop, usage, err := d.awaitSessionAnswer(ctx, creds, sid)
	if err != nil {
		return nil, err
	}
	if text != "" {
		_ = emit(ir.StreamEvent{Kind: ir.EvText, Delta: text})
		resp.Message.Content = []ir.ContentPart{{Kind: "text", Text: text}}
	}
	if reasoning != "" {
		_ = emit(ir.StreamEvent{Kind: ir.EvThinking, Delta: reasoning})
		resp.Thinking = reasoning
	}
	resp.StopReason = ir.StopReason(stop)
	if usage != nil {
		resp.Usage = *usage
	}
	if text == "" && reasoning == "" {
		return nil, uperr.New(uperr.ClassInternal, 200, "trae sessions: the turn finished with no answer text")
	}
	return resp, nil
}

// createSession submits the whole conversation as the session's first turn.
//
// The surface is stateful, but the gateway is not: every gateway request is
// self-contained, so the conversation is replayed in full rather than resumed.
// That is the same shape apmengzi/autoclaw-switch uses, minus its lossy
// flatten-to-one-string step — the messages keep their roles here.
func (d *Driver) createSession(ctx context.Context, creds *Creds, call driver.ChatCall) (string, error) {
	query, err := sessionQuery(call.Request)
	if err != nil {
		return "", uperr.New(uperr.ClassBadRequest, 400, "trae sessions: %v", err)
	}
	body := map[string]any{
		"env":          "local",
		"mode":         "work",
		"session_type": "assistant_chat",
		"initial_message": map[string]any{
			"content":                  []any{},
			"query":                    query,
			"model_name":               call.UpstreamModel,
			"agent_type":               "",
			"model_selection_strategy": "",
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", uperr.New(uperr.ClassInternal, 0, "trae sessions: %v", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		d.agentBase()+"/api/remote/v1/chat_sessions", strings.NewReader(string(raw)))
	if err != nil {
		return "", err
	}
	soloHeaders(hreq, creds, false)
	resp, err := d.http.Do(hreq)
	if err != nil {
		return "", uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "trae sessions: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", classifyHTTP(resp.StatusCode, rb, call.UpstreamModel)
	}
	var out sessionCreateResp
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", uperr.New(uperr.ClassInternal, 0, "trae sessions: decode: %v", err)
	}
	if out.Code != 0 {
		return "", classifyBusiness(out.Code, out.Msg)
	}
	if out.Data.ChatSessionID == "" {
		return "", uperr.New(uperr.ClassInternal, 0, "trae sessions: no chat_session_id in the reply")
	}
	return out.Data.ChatSessionID, nil
}

// awaitSessionAnswer polls the messages endpoint until the turn is terminal.
//
// The SSE endpoint is deliberately not used as the primary path: measured
// 2026-10-23, it delivers the metadata burst and then closes the connection,
// leaving the generation running server-side. Polling the messages list is
// what the answer actually appears in, and it also survives a dropped socket.
func (d *Driver) awaitSessionAnswer(ctx context.Context, creds *Creds, sid string) (text, reasoning, stop string, usage *ir.Usage, err error) {
	deadline := time.Now().Add(sessionPollBudget)
	var lastErr error
	for {
		if time.Now().After(deadline) {
			if lastErr != nil {
				return "", "", "", nil, lastErr
			}
			return "", "", "", nil, uperr.New(uperr.ClassUpstreamTimeout, 0,
				"trae sessions: the turn produced no answer within %s", sessionPollBudget)
		}
		select {
		case <-ctx.Done():
			return "", "", "", nil, uperr.New(uperr.ClassCanceled, 0, "trae sessions: %v", ctx.Err())
		case <-time.After(sessionPollInterval):
		}
		msgs, gerr := d.sessionMessages(ctx, creds, sid)
		if gerr != nil {
			// A transient poll failure is retried rather than surfaced: the turn
			// is still running upstream and the next poll usually succeeds.
			lastErr = gerr
			continue
		}
		t, r, st, done := extractSessionAnswer(msgs)
		if done {
			return t, r, st, nil, nil
		}
	}
}

// sessionMessages fetches the session's message list.
func (d *Driver) sessionMessages(ctx context.Context, creds *Creds, sid string) ([]sessionMessage, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		d.agentBase()+"/api/remote/v1/chat_sessions/"+sid+"/messages", nil)
	if err != nil {
		return nil, err
	}
	soloHeaders(hreq, creds, false)
	resp, err := d.http.Do(hreq)
	if err != nil {
		return nil, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "trae sessions: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, classifyHTTP(resp.StatusCode, rb, "")
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
		Data struct {
			Items []sessionMessage `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rb, &env); err != nil {
		return nil, uperr.New(uperr.ClassInternal, 0, "trae sessions: messages decode: %v", err)
	}
	if env.Code != 0 {
		return nil, classifyBusiness(env.Code, env.Msg)
	}
	return env.Data.Items, nil
}

// extractSessionAnswer pulls the assistant text out of the message list.
//
// The assistant's content is a JSON document of plan items. The answer text is
// carried by `thought` on each item and, when the agent chose to end the turn,
// by the `finish` tool call's params.summary — the same two places the
// reference relay reads. Items may be re-sent while streaming, so the LAST
// occurrence of an id wins.
func extractSessionAnswer(msgs []sessionMessage) (text, reasoning, stop string, done bool) {
	type item struct {
		ID        string `json:"id"`
		Thought   string `json:"thought"`
		Reasoning string `json:"reasoning_content"`
		ToolCall  struct {
			Name   string `json:"name"`
			Params struct {
				Summary string `json:"summary"`
			} `json:"params"`
		} `json:"tool_call_info"`
		AgentStatus struct {
			Status string `json:"status"`
		} `json:"agent_status"`
	}
	type doc struct {
		Messages []struct {
			PlanItem *item `json:"plan_item"`
		} `json:"messages"`
	}

	var thoughts, reasons []string
	var summary string
	var finished bool
	var pending bool
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		if m.Status == "in_progress" || m.Status == "pending" {
			pending = true
		}
		var d doc
		if json.Unmarshal([]byte(m.Content), &d) != nil {
			continue
		}
		seen := map[string]bool{}
		for _, w := range d.Messages {
			pi := w.PlanItem
			if pi == nil || seen[pi.ID] {
				continue
			}
			seen[pi.ID] = true
			if s := strings.TrimSpace(pi.Thought); s != "" {
				thoughts = append(thoughts, s)
			}
			if s := strings.TrimSpace(pi.Reasoning); s != "" {
				reasons = append(reasons, s)
			}
			if pi.ToolCall.Name == "finish" && pi.ToolCall.Params.Summary != "" {
				summary = pi.ToolCall.Params.Summary
				finished = true
			}
			if pi.AgentStatus.Status == "completed" {
				finished = true
			}
		}
	}
	text = strings.TrimSpace(strings.Join(thoughts, "\n"))
	if text == "" {
		text = strings.TrimSpace(summary)
	}
	reasoning = strings.TrimSpace(strings.Join(reasons, "\n"))
	return text, reasoning, "stop", finished && !pending && text != ""
}

// sessionQuery renders the conversation as the wire array the session surface
// accepts. Roles are kept; a system message is folded into the first user turn
// because the surface has no system slot of its own.
func sessionQuery(req *ir.ChatRequest) (string, error) {
	var parts []map[string]any
	var system []string
	for _, m := range req.Messages {
		text := ir.TextOf(m.Content)
		if text == "" && len(m.ToolCalls) > 0 {
			// Tool traffic never reaches this path (tools are filtered out
			// before dispatch), but be explicit rather than dropping silently.
			continue
		}
		switch m.Role {
		case ir.RoleSystem:
			system = append(system, text)
		case ir.RoleUser:
			if len(system) > 0 {
				text = strings.Join(system, "\n\n") + "\n\n" + text
				system = nil
			}
			parts = append(parts, map[string]any{"type": "text", "data": map[string]any{"content": text}})
		case ir.RoleAssistant:
			parts = append(parts, map[string]any{"type": "text", "data": map[string]any{"content": "[Assistant] " + text}})
		}
	}
	if len(system) > 0 {
		parts = append(parts, map[string]any{"type": "text", "data": map[string]any{"content": strings.Join(system, "\n\n")}})
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("empty conversation")
	}
	out, err := json.Marshal(parts)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// readSSE drains a stream just to keep the connection warm; currently unused by
// the poll path and kept as the seam for a future streaming reader.
func readSSE(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
	}
}

// estimateInputTokens is a coarse prompt-size estimate used only to decide
// whether a tool-carrying request can fit the ladder surface. It mirrors the
// gateway's own estimator (chars/3 for text, a flat allowance per image) so the
// two never disagree by much; exact accounting stays with the upstream usage.
func estimateInputTokens(req *ir.ChatRequest) int {
	if req == nil {
		return 0
	}
	chars, images := 0, 0
	for _, m := range req.Messages {
		for _, p := range m.Content {
			switch p.Kind {
			case "text":
				chars += len(p.Text)
			case "image":
				images++
			}
		}
		for _, th := range m.Thinking {
			chars += len(th.Text)
		}
		for _, tc := range m.ToolCalls {
			chars += len(tc.Arguments)
		}
	}
	return chars/3 + images*768
}
