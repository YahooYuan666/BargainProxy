package qoder

// Qoder's queue protocol (BP-92, 2026-10-22).
//
// A 10605 answer is not a rate limit: the upstream queued the request and tells
// the client how long to wait. The official client does not simply sleep and
// resend — it asks a queue-status endpoint where the request stands, and only
// when the queue says it is no longer queued does it send the same request
// again, request_set_id and all. That identity is what lets Qoder recognise the
// resend as the same queued request instead of a new one at the back of the
// queue.
//
// Reference: magpie-community/plugins packages/qoder 0.2.26–0.2.28 (MIT,
// behaviour only), which reads qoder-agent-sdk's "[ModelQueue]" path.
//
// Everything here fails open: if the status endpoint answers 404, keeps
// failing, or returns a shape we do not recognize, the caller falls back to
// the previous behaviour (sleep retryAfterSeconds, resend). A queue protocol we
// cannot read must never make a request fail that used to work.

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"bargainproxy/internal/httpx"
	"bargainproxy/internal/uperr"
)

// queueStatusPath is where the queue's state is asked. The gateway base has no
// /algo prefix in the request path (sigPath strips it), but the upstream route
// itself is under /algo — the reference sends this exact path.
const queueStatusPath = "/algo/api/v2/service/ask/queue/status"

const (
	// queuePollShortest / queuePollLongest bound one wait between status asks,
	// as the reference bounds retryAfterSeconds (0.5s..30s).
	queuePollShortest = 500 * time.Millisecond
	queuePollLongest  = 30 * time.Second
	// queuePollUnsaid is the pause when a queue state names none.
	queuePollUnsaid = 30 * time.Second
	// queuePollAsk bounds one status request.
	queuePollAsk = 30 * time.Second
	// queuePollFailures is how many status asks may fail in a row before we
	// give up and fall back to the plain sleep retry.
	queuePollFailures = 3
	// queuePollBudget is the most one request spends in the queue poll before
	// falling back. The whole wait already has queueWaitBudget; this is the
	// slice the polling may consume.
	queuePollBudget = 20 * time.Second
)

// queueState is the queue's answer, found the way the reference finds it: the
// object itself, or one nested under data/result/message/body, as an object or
// a JSON string, that says isQueued or serviceAvailable.
type queueState struct {
	IsQueued          *bool
	ServiceAvailable  *bool
	ModelKey          string
	QueueType         string
	RetryAfterSeconds float64
	RetryAfterKnown   bool
}

// queued reports whether the request is still waiting. A state with neither
// flag is not a queue answer.
func (q queueState) queued() bool {
	if q.IsQueued != nil {
		return *q.IsQueued
	}
	if q.ServiceAvailable != nil {
		return !*q.ServiceAvailable
	}
	return false
}

// known reports whether the state said anything at all.
func (q queueState) known() bool { return q.IsQueued != nil || q.ServiceAvailable != nil }

// parseQueueState reads a queue-status body.
func parseQueueState(body []byte) (queueState, bool) {
	var root any
	if json.Unmarshal(body, &root) != nil {
		return queueState{}, false
	}
	// Depth-bounded walk: a JSON document cannot cycle, but a hostile or
	// broken body can nest arbitrarily deep, and the shape we want is shallow.
	type node struct {
		v any
		d int
	}
	todo := []node{{root, 0}}
	for len(todo) > 0 {
		cur := todo[0]
		todo = todo[1:]
		if cur.d > 4 {
			continue
		}
		obj, ok := cur.v.(map[string]any)
		if !ok {
			continue
		}
		if q, ok := queueStateOf(obj); ok {
			return q, true
		}
		for _, key := range []string{"data", "result", "message", "body"} {
			switch v := obj[key].(type) {
			case map[string]any:
				todo = append(todo, node{v, cur.d + 1})
			case string:
				var inner any
				if json.Unmarshal([]byte(v), &inner) == nil {
					todo = append(todo, node{inner, cur.d + 1})
				}
			}
		}
	}
	return queueState{}, false
}

// queueStateOf reads one object as a queue state, if it is one.
func queueStateOf(obj map[string]any) (queueState, bool) {
	flag := func(v any) *bool {
		switch t := v.(type) {
		case bool:
			return &t
		case string:
			if t == "true" {
				b := true
				return &b
			}
			if t == "false" {
				b := false
				return &b
			}
		}
		return nil
	}
	str := func(v any) string {
		if s, ok := v.(string); ok {
			return s
		}
		return ""
	}
	q := queueState{
		IsQueued:         flag(obj["isQueued"]),
		ServiceAvailable: flag(obj["serviceAvailable"]),
		ModelKey:         str(obj["modelKey"]),
		QueueType:        str(obj["queueType"]),
	}
	if !q.known() {
		return queueState{}, false
	}
	switch v := obj["retryAfterSeconds"].(type) {
	case float64:
		q.RetryAfterSeconds, q.RetryAfterKnown = v, true
	case string:
		if n, err := atof(v); err == nil {
			q.RetryAfterSeconds, q.RetryAfterKnown = n, true
		}
	}
	return q, true
}

// queuePause is how long to wait before asking again: the queue's
// retryAfterSeconds held to [shortest, longest], or the unsaid pause.
func queuePause(q queueState) time.Duration {
	if !q.RetryAfterKnown {
		return queuePollUnsaid
	}
	ms := time.Duration(q.RetryAfterSeconds * float64(time.Second))
	if ms < queuePollShortest {
		return queuePollShortest
	}
	if ms > queuePollLongest {
		return queuePollLongest
	}
	return ms
}

// awaitQueue asks the queue where the request stands until it says the request
// may be sent, the budget runs out, or the answers stop making sense. It
// reports whether the caller should resend now.
//
// A false answer is not an error: the caller then falls back to the plain
// sleep retry it used before this existed.
func (d *Driver) awaitQueue(ctx context.Context, sess *sessionContext, q queueState, requestSetID string, deadline time.Time) bool {
	if requestSetID == "" || sess == nil {
		return false
	}
	failures := 0
	for {
		if !time.Now().Before(deadline) {
			return false
		}
		state, err := d.queueStatus(ctx, sess, requestSetID, q)
		if err != nil {
			// 404 means there is no queue status here at all; anything else is
			// tolerated a few times before giving up on polling.
			if ue, ok := uperr.As(err); ok && ue.Status == http.StatusNotFound {
				return false
			}
			failures++
			if failures >= queuePollFailures {
				return false
			}
		} else if state.known() && !state.queued() {
			// No longer queued: the reference waits this state's own
			// retryAfterSeconds, if it named one, before resending.
			if state.RetryAfterKnown && state.RetryAfterSeconds > 0 {
				pause := queuePause(state)
				if time.Now().Add(pause).After(deadline) {
					return false
				}
				if !sleepCtx(ctx, pause) {
					return false
				}
			}
			return true
		}
		pause := queuePause(state)
		if time.Now().Add(pause).After(deadline) {
			return false
		}
		if !sleepCtx(ctx, pause) {
			return false
		}
	}
}

// queueStatus asks the queue where the request stands.
func (d *Driver) queueStatus(ctx context.Context, sess *sessionContext, requestSetID string, q queueState) (queueState, error) {
	params := url.Values{"requestSetId": {requestSetID}}
	if q.ModelKey != "" {
		params.Set("modelKey", q.ModelKey)
	}
	if q.QueueType != "" {
		params.Set("queueType", q.QueueType)
	}
	full := d.gatewayBase() + queueStatusPath + "?" + params.Encode()
	dateUnix := time.Now().Unix()
	bearer := sess.buildBearer(dateUnix, "", sigPath(full))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return queueState{}, err
	}
	req.Header = bearerHeaders(sess, dateUnix, bearer, "application/json")
	req.Header.Del("content-type")
	callCtx, cancel := context.WithTimeout(ctx, queuePollAsk)
	defer cancel()
	resp, err := d.rpc.Do(req.WithContext(callCtx))
	if err != nil {
		return queueState{}, uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "qoder: queue status: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return queueState{}, classifyQoder(resp.StatusCode, string(raw))
	}
	state, ok := parseQueueState(raw)
	if !ok {
		return queueState{}, uperr.New(uperr.ClassInternal, 0, "qoder: queue status unreadable: %s", clip(string(raw), 120))
	}
	return state, nil
}

// sleepCtx waits d, reporting false when the context ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// atof parses a decimal from a string field.
func atof(s string) (float64, error) {
	var f float64
	dec := json.NewDecoder(strings.NewReader(s))
	if err := dec.Decode(&f); err != nil {
		return 0, err
	}
	return f, nil
}

// queueRequestSetID extracts the request_set_id from the 10605 body so the
// resend can carry the same one. "" when the body does not name it.
func queueRequestSetID(body string) string {
	obj := extractJSONObject(body)
	if obj == "" {
		return ""
	}
	var env struct {
		RequestSetID string `json:"request_set_id"`
		RequestSetId string `json:"requestSetId"`
		Queue        struct {
			RequestSetID string `json:"request_set_id"`
			RequestSetId string `json:"requestSetId"`
		} `json:"queue"`
	}
	if json.Unmarshal([]byte(obj), &env) != nil {
		return ""
	}
	for _, v := range []string{env.RequestSetID, env.RequestSetId, env.Queue.RequestSetID, env.Queue.RequestSetId} {
		if v != "" {
			return v
		}
	}
	return ""
}

// logQueueFallback records why the poll gave up, so a field report can tell a
// queue-protocol change from a slow queue.
func logQueueFallback(reason string) {
	log.Printf("qoder: queue status poll gave up (%s); falling back to a plain sleep retry", reason)
}

// minDuration returns the shorter of two durations.
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
