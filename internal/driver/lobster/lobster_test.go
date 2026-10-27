package lobster

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// testCreds builds a JWT-shaped token whose payload carries a yid, so the
// driver's identity path is exercised rather than bypassed.
func testCreds(t *testing.T) *Creds {
	t.Helper()
	// {"yid":"u-123","exp":4102444800} — payload only; the signature is never
	// verified by this driver.
	const payload = "eyJ5aWQiOiJ1LTEyMyIsImV4cCI6NDEwMjQ0NDgwMH0"
	return &Creds{AccessToken: "h." + payload + ".s", UID: "u-123"}
}

func accountWith(t *testing.T, c *Creds) *model.Account {
	t.Helper()
	raw, _ := json.Marshal(c)
	return &model.Account{ID: "lobster:u-123", Channel: model.ChannelLobster, CredsJSON: raw}
}

func newTestDriver(t *testing.T, serverURL string) *Driver {
	t.Helper()
	cfg := config.LobsterSettings{
		Enabled:            true,
		ServerBase:         serverURL,
		PortalBase:         serverURL,
		ClientVersion:      "2026.9.23",
		ClientCapabilities: "thinking-level-control-v1",
	}
	return New(cfg, nil)
}

func collect(t *testing.T) (driver.Emit, *[]ir.StreamEvent) {
	t.Helper()
	var evs []ir.StreamEvent
	return func(ev ir.StreamEvent) error {
		evs = append(evs, ev)
		return nil
	}, &evs
}

// TestStreamTreatsErrorEventAsFailure is the core trap: the upstream reports
// failures as `event: error` inside an HTTP 200 stream. If that frame were
// passed through, the pool would record a success — no penalty, no rotation,
// sticky pinned to a dead account.
func TestStreamTreatsErrorEventAsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: error\n")
		io.WriteString(w, `data: {"code":42900,"message":"请求过于频繁"}`+"\n\n")
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	emit, _ := collect(t)
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       accountWith(t, testCreds(t)),
		UpstreamModel: "deepseek-flash",
		Request:       &ir.ChatRequest{Model: "deepseek-v4.1-flash"},
	}, emit)
	if err == nil {
		t.Fatal("an error event inside a 200 stream must fail the turn")
	}
	ue, ok := uperr.As(err)
	if !ok {
		t.Fatalf("error is not *uperr.Error: %v", err)
	}
	if ue.Class != uperr.ClassSoftRate {
		t.Fatalf("class = %q, want %q (42900 must become a rate event)", ue.Class, uperr.ClassSoftRate)
	}
}

// TestStreamMidStreamErrorFails: an error after some text must still fail, not
// return a truncated "success".
func TestStreamMidStreamErrorFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"hello"}}]}`+"\n\n")
		io.WriteString(w, "event: error\n")
		io.WriteString(w, `data: {"code":40100,"message":"登录已过期，请重新登录"}`+"\n\n")
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	emit, _ := collect(t)
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       accountWith(t, testCreds(t)),
		UpstreamModel: "deepseek-flash",
		Request:       &ir.ChatRequest{Model: "m"},
	}, emit)
	if err == nil {
		t.Fatal("mid-stream error must fail the turn")
	}
	ue, _ := uperr.As(err)
	if ue == nil || ue.Class != uperr.ClassAuth {
		t.Fatalf("class = %v, want auth (40100)", ue)
	}
}

// TestStreamAssemblesTextThinkingAndUsage covers the happy path plus the
// OpenAI-compatible chunk shapes the upstream actually sends.
func TestStreamAssemblesTextThinkingAndUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `data: {"choices":[{"delta":{"reasoning_content":"想一下"}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"答案"}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":5}}}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	emit, evs := collect(t)
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       accountWith(t, testCreds(t)),
		UpstreamModel: "deepseek-flash",
		Request:       &ir.ChatRequest{Model: "deepseek-v4.1-flash"},
	}, emit)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got := ir.TextOf(resp.Message.Content); got != "答案" {
		t.Fatalf("text = %q, want 答案", got)
	}
	if resp.Thinking != "想一下" {
		t.Fatalf("thinking = %q", resp.Thinking)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if !resp.Usage.CacheKnown || resp.Usage.CachedInputTokens != 3 {
		t.Fatalf("cache accounting lost: %+v", resp.Usage)
	}
	if resp.Usage.ReasoningTokens != 5 {
		t.Fatalf("reasoning tokens = %d", resp.Usage.ReasoningTokens)
	}
	if resp.StopReason != ir.StopEnd {
		t.Fatalf("stop = %q", resp.StopReason)
	}
	if len(*evs) == 0 {
		t.Fatal("no events emitted")
	}
}

// TestStreamToolCallsAggregate: tool deltas arrive in fragments keyed by
// index; the assembled call must carry id/name/arguments.
func TestStreamToolCallsAggregate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"SH\"}"}}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	emit, _ := collect(t)
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       accountWith(t, testCreds(t)),
		UpstreamModel: "deepseek-flash",
		Request:       &ir.ChatRequest{Model: "m"},
	}, emit)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "get_weather" {
		t.Fatalf("tool call identity = %+v", tc)
	}
	if tc.Arguments != `{"city":"SH"}` {
		t.Fatalf("arguments = %q", tc.Arguments)
	}
	if resp.StopReason != ir.StopToolUse {
		t.Fatalf("stop = %q, want tool_use", resp.StopReason)
	}
}

// TestThinkingFieldOnlyForCapableModels is the second trap: lobsterai_options
// is a hard 400 for the 22 catalog models without a thinkingConfig. The driver
// must consult the catalog, not just the caller's effort.
func TestThinkingFieldOnlyForCapableModels(t *testing.T) {
	var sawBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/pricing-catalog":
			io.WriteString(w, `{"code":0,"data":{"textModels":[
				{"modelId":"deepseek-flash","thinkingConfig":{"options":[{"level":"off"},{"level":"high"}],"defaultLevel":"high"}},
				{"modelId":"qwen3.8-max","thinkingConfig":null}
			]}}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &sawBody)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	emit, _ := collect(t)

	// A capable model: the field must be present with the mapped gear.
	sawBody = nil
	effort := "max"
	if _, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       accountWith(t, testCreds(t)),
		UpstreamModel: "deepseek-flash",
		Request:       &ir.ChatRequest{Model: "m", RequestedEffort: effort},
	}, emit); err != nil {
		t.Fatalf("stream capable: %v", err)
	}
	opts, ok := sawBody[optionsField].(map[string]any)
	if !ok {
		t.Fatalf("capable model lost %s: %v", optionsField, sawBody)
	}
	thinking, _ := opts["thinking"].(map[string]any)
	if thinking["level"] != "max" {
		t.Fatalf("gear = %v, want max", thinking["level"])
	}

	// A non-capable model: the field must be absent even though an effort was
	// requested, because the upstream rejects it outright.
	sawBody = nil
	if _, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       accountWith(t, testCreds(t)),
		UpstreamModel: "qwen3.8-max",
		Request:       &ir.ChatRequest{Model: "m", RequestedEffort: "max"},
	}, emit); err != nil {
		t.Fatalf("stream non-capable: %v", err)
	}
	if _, present := sawBody[optionsField]; present {
		t.Fatalf("%s sent to a model without thinkingConfig: %v", optionsField, sawBody)
	}
}

// TestKimiK3Contract: fixed sampling is stripped, reasoning_effort is max, and
// historical assistant tool-call turns get an empty reasoning_content.
func TestKimiK3Contract(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/pricing-catalog" {
			io.WriteString(w, `{"code":0,"data":{"textModels":[{"modelId":"kimi-k3","thinkingConfig":null}]}}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	emit, _ := collect(t)
	temp := 0.7
	_, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       accountWith(t, testCreds(t)),
		UpstreamModel: "kimi-k3",
		Request: &ir.ChatRequest{
			Model:       "kimi-k3",
			Temperature: &temp,
			Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
				{Role: ir.RoleAssistant, ToolCalls: []ir.ToolCall{{ID: "c1", Name: "f", Arguments: "{}"}}},
				{Role: ir.RoleTool, ToolCallID: "c1", Content: []ir.ContentPart{{Kind: "text", Text: "done"}}},
			},
		},
	}, emit)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if _, present := body["temperature"]; present {
		t.Fatalf("K3 sampling must be stripped, got temperature=%v", body["temperature"])
	}
	if body["reasoning_effort"] != "max" {
		t.Fatalf("reasoning_effort = %v, want max", body["reasoning_effort"])
	}
	if _, present := body[optionsField]; present {
		t.Fatalf("K3 must not receive %s", optionsField)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d", len(msgs))
	}
	assistant, _ := msgs[1].(map[string]any)
	if _, present := assistant["reasoning_content"]; !present {
		t.Fatal("assistant tool-call turn must carry reasoning_content for K3")
	}
}

// TestProbeMapsQuotaAndBalance: points become a balance-only Credits block and
// the free allowance becomes a separate window — never summed, because they
// are different currencies.
func TestProbeMapsQuotaAndBalance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/quota":
			io.WriteString(w, `{"code":0,"data":{"planName":"Pro","freeCreditsTotal":100,"freeCreditsUsed":25}}`)
		case "/api/user/profile-summary":
			io.WriteString(w, `{"code":0,"data":{"nickname":"tester","totalCreditsRemaining":4321}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	res, err := d.Probe(context.Background(), accountWith(t, testCreds(t)))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Credits == nil {
		t.Fatal("credits missing")
	}
	if !res.Credits.BalanceOnly || res.Credits.Remain != 4321 {
		t.Fatalf("credits = %+v, want balance-only 4321", *res.Credits)
	}
	w, ok := res.Windows["free_credits"]
	if !ok {
		t.Fatalf("free credit window missing: %+v", res.Windows)
	}
	if got := w.RemainingFraction; got < 0.74 || got > 0.76 {
		t.Fatalf("remaining fraction = %v, want 0.75", got)
	}
	if !strings.Contains(res.Note, "Pro") {
		t.Fatalf("note = %q", res.Note)
	}
}

// TestProbeAuthFailureIsClassified: 40100 on a 200 envelope must surface as an
// auth error so the pool marks the account, not as a generic failure.
func TestProbeAuthFailureIsClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":40100,"message":"登录已过期，请重新登录"}`)
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	_, err := d.Probe(context.Background(), accountWith(t, testCreds(t)))
	if err == nil {
		t.Fatal("probe must fail on 40100")
	}
	ue, _ := uperr.As(err)
	if ue == nil || ue.Class != uperr.ClassAuth {
		t.Fatalf("class = %v, want auth", ue)
	}
}

// TestModelsUsesPublicCatalog: the catalog needs no login, so bindings can be
// validated before an account exists.
func TestModelsUsesPublicCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/pricing-catalog" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"code":0,"data":{"textModels":[{"modelId":"deepseek-flash"},{"modelId":"glm-5.3"}]}}`)
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	got, err := d.Models(context.Background())
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(got) != 2 || got[0] != "deepseek-flash" || got[1] != "glm-5.3" {
		t.Fatalf("models = %v", got)
	}
}

// TestJWTAudienceAndExp guards the identity path: yid wins, sub is the
// fallback, and exp is read in seconds.
func TestJWTAudienceAndExp(t *testing.T) {
	c := testCreds(t)
	if got := JWTAudience(c.AccessToken); got != "u-123" {
		t.Fatalf("audience = %q, want u-123", got)
	}
	if got := JWTExp(c.AccessToken); got != 4102444800 {
		t.Fatalf("exp = %d", got)
	}
	if got := JWTAudience("not-a-jwt"); got != "" {
		t.Fatalf("garbage token audience = %q", got)
	}
}

// TestRefreshUpdatesCreds: a refresh must persist the new token and expiry.
func TestRefreshUpdatesCreds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/refresh" {
			http.NotFound(w, r)
			return
		}
		// New token expiring in the past, so the early-refresh guard lets it run.
		io.WriteString(w, `{"code":0,"data":{"accessToken":"h.eyJ5aWQiOiJ1LTEyMyIsImV4cCI6NDEwMjQ0NDgwMH0.s","refreshToken":"r2"}}`)
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	c := &Creds{AccessToken: "h.eyJ5aWQiOiJ1LTEyMyIsImV4cCI6MTAwMH0.s", RefreshToken: "r1"}
	acc := accountWith(t, c)
	if err := d.Refresh(context.Background(), acc); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.RefreshToken != "r2" {
		t.Fatalf("refresh token = %q", got.RefreshToken)
	}
	if got.ExpiresAt != 4102444800 {
		t.Fatalf("expiry = %d", got.ExpiresAt)
	}
	if !acc.CredsDirty {
		t.Fatal("refreshed creds must be marked dirty for persistence")
	}
}

// TestClassifyCode pins the vendor code mapping the whole error path depends on.
func TestClassifyCode(t *testing.T) {
	cases := []struct {
		code int
		msg  string
		want string
	}{
		{40100, "登录已过期", uperr.ClassAuth},
		{40300, "forbidden", uperr.ClassWAF},
		{42900, "too many", uperr.ClassSoftRate},
		{40301, "no entitlement", uperr.ClassHardCredit},
		{4000, "bad param", uperr.ClassBadRequest},
		{0, "请求过于频繁", uperr.ClassSoftRate},
	}
	for _, tc := range cases {
		got := classifyCode(tc.code, tc.msg)
		if got.Class != tc.want {
			t.Errorf("code %d (%q) → %q, want %q", tc.code, tc.msg, got.Class, tc.want)
		}
	}
}

// TestStreamUsageInTrailingChunk: qwen/kimi send the terminal usage in a
// SEPARATE chunk with an empty choices array, AFTER the finish_reason chunk.
// Breaking on finish_reason dropped it and every such turn reported zero
// tokens (2026-10-23 field finding: kimi-k3 and qwen3.8-max answered fine but
// usage was 0/0/0 through the gateway).
func TestStreamUsageInTrailingChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"东方"}}]}`+"\n\n")
		// finish_reason chunk WITHOUT usage
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		// terminal usage in a separate chunk with an empty choices array
		io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":86,"completion_tokens":392,"total_tokens":478,"completion_tokens_details":{"reasoning_tokens":231}}}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	emit, _ := collect(t)
	resp, err := d.Stream(context.Background(), driver.ChatCall{
		Account:       accountWith(t, testCreds(t)),
		UpstreamModel: "qwen3.8-max",
		Request:       &ir.ChatRequest{Model: "m"},
	}, emit)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if resp.Usage.InputTokens != 86 || resp.Usage.OutputTokens != 392 {
		t.Fatalf("usage = %+v, want 86/392 (trailing chunk must not be dropped)", resp.Usage)
	}
	if resp.Usage.ReasoningTokens != 231 {
		t.Fatalf("reasoning tokens = %d, want 231", resp.Usage.ReasoningTokens)
	}
}

// TestCheckinHappyPath walks the three-call activity flow (slot → context →
// check_in) and pins the wire shapes, including the clientVersion gate that
// silently hides the activity when wrong.
func TestCheckinHappyPath(t *testing.T) {
	var gotVersion, gotUA string
	var postBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/client-activities/slot"):
			gotVersion = r.URL.Query().Get("clientVersion")
			gotUA = r.Header.Get("User-Agent")
			if gotVersion != "2026.10.16" {
				// The upstream's real behaviour: an old version yields an empty
				// slot, which is indistinguishable from "no activity".
				io.WriteString(w, `{"code":0,"data":{"slotState":"empty"}}`)
				return
			}
			io.WriteString(w, `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"daily-check-in-evergreen-prod-20260814","configRevision":1,"activityType":"daily_check_in"}}}`)
		case strings.HasSuffix(r.URL.Path, "/context"):
			io.WriteString(w, `{"code":0,"data":{"authenticated":true,"state":{"claimedToday":false,"claimedDays":1,"totalDays":36500,"rewardCredits":100}}}`)
		case strings.HasSuffix(r.URL.Path, "/actions/check_in"):
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &postBody)
			io.WriteString(w, `{"code":0,"data":{"replayed":false,"result":{"creditsGranted":100,"claimedDays":2,"totalDays":36500,"periodKey":"2026-10-23"},"context":{"state":{"claimedDays":2,"totalDays":36500}}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	res, err := d.Checkin(context.Background(), accountWith(t, testCreds(t)))
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if res.AlreadyClaimed {
		t.Fatal("fresh day must not report already-claimed")
	}
	if res.CreditsGranted != 100 {
		t.Fatalf("credits = %d, want 100", res.CreditsGranted)
	}
	if res.ClaimedDays != 2 || res.TotalDays != 36500 {
		t.Fatalf("day counter = %d/%d", res.ClaimedDays, res.TotalDays)
	}
	if res.PeriodKey != "2026-10-23" {
		t.Fatalf("period = %q", res.PeriodKey)
	}
	// The clientVersion gate is what makes the activity appear at all. Assert
	// the literal, not the constant: comparing against checkinClientVersion
	// would move with the code and catch nothing.
	if gotVersion != "2026.10.16" {
		t.Fatalf("clientVersion = %q, want the 2026.10.16 gate", gotVersion)
	}
	if !strings.HasPrefix(gotUA, "LobsterAI/") {
		t.Fatalf("User-Agent = %q, must carry the client identity", gotUA)
	}
	if postBody["idempotencyKey"] == "" || postBody["idempotencyKey"] == nil {
		t.Fatal("idempotencyKey missing from the claim request")
	}
	if postBody["configRevision"] != float64(1) && postBody["configRevision"] != 1 {
		t.Fatalf("configRevision = %v", postBody["configRevision"])
	}
}

// TestCheckinAlreadyClaimed: a second run in the same day must be a no-op that
// reports AlreadyClaimed, not an error (the task runs twice a day).
func TestCheckinAlreadyClaimed(t *testing.T) {
	posted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/client-activities/slot"):
			io.WriteString(w, `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"act","configRevision":1}}}`)
		case strings.HasSuffix(r.URL.Path, "/context"):
			io.WriteString(w, `{"code":0,"data":{"authenticated":true,"state":{"claimedToday":true,"claimedDays":2,"totalDays":36500}}}`)
		case strings.HasSuffix(r.URL.Path, "/actions/check_in"):
			posted = true
			io.WriteString(w, `{"code":0,"data":{}}`)
		}
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	res, err := d.Checkin(context.Background(), accountWith(t, testCreds(t)))
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if !res.AlreadyClaimed {
		t.Fatal("must report already-claimed")
	}
	if posted {
		t.Fatal("must not POST a claim when today is already claimed")
	}
}

// TestCheckinLoginRequiredIsAuth: code 51102 must surface as ClassAuth so the
// task refreshes the token instead of retrying a dead one forever.
func TestCheckinLoginRequiredIsAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":51102,"message":"请先登录"}`)
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	_, err := d.Checkin(context.Background(), accountWith(t, testCreds(t)))
	if err == nil {
		t.Fatal("51102 must fail the call")
	}
	ue, _ := uperr.As(err)
	if ue == nil || ue.Class != uperr.ClassAuth {
		t.Fatalf("class = %v, want auth", ue)
	}
}

// TestCheckinNoActivity: an empty slot means the account has no check-in
// programme offered — report NotFound rather than pretending success.
func TestCheckinNoActivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":0,"data":{"slotState":"empty"}}`)
	}))
	defer srv.Close()

	d := newTestDriver(t, srv.URL)
	_, err := d.Checkin(context.Background(), accountWith(t, testCreds(t)))
	if err == nil {
		t.Fatal("empty slot must not be reported as success")
	}
	ue, _ := uperr.As(err)
	if ue == nil || ue.Class != uperr.ClassNotFound {
		t.Fatalf("class = %v, want not_found", ue)
	}
}

// TestNewUUIDShape guards the idempotency key format (v4 UUID).
func TestNewUUIDShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		u := newUUID()
		if len(u) != 36 || strings.Count(u, "-") != 4 {
			t.Fatalf("bad uuid shape: %q", u)
		}
		if u[14] != '4' {
			t.Fatalf("not a v4 uuid: %q", u)
		}
		if seen[u] {
			t.Fatalf("duplicate uuid: %q", u)
		}
		seen[u] = true
	}
}
