// growth.go implements the WorkBuddy growth-center surface: task listing /
// accept / claim, streak (连登) redemption + lottery, cat travel and buddy
// adoption, chat-activity reports and the desktop/web client event chains
// used to light up the growth tasks.
//
// The event-chain shapes are ported from the MIT-licensed
// workbuddy2api-panel fork (see NOTICE); they were reverse-engineered from
// the official client's telemetry. Chain semantics: each task is lit by a
// recognizable client behaviour (a chat round, a template use, an expert
// summon with a server-issued request id, ...), reconstructed as the same
// /v2/report events with the matching client fingerprints.
package workbuddy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

const (
	// desktopUA is the CN package's User-Agent. The first segment is always the
	// camel-case product name (openai-node's buildUserAgent does that) and only
	// the second segment follows the distribution: CN keeps the camel-case
	// product name, the international build rewrites it to the lowercase slug
	// (official packet captures of both packages, 2026-10-22). Version segments
	// are per realm — the two distributions ship on different cadences, so
	// sending CN's number to the global edge does not match any real build.
	desktopUA           = "WorkBuddy/" + clientVersionCN + " WorkBuddy/" + clientVersionCN + " CLI/" + cliVersionCN
	desktopUAGlobal     = "WorkBuddy/" + clientVersionGlobal + " workbuddy-ai/" + clientVersionGlobal + " CLI/" + cliVersionGlobal
	// BP-158: the shipped builds as of 2026-10-28 (magpie 74dcf40a, diffed
	// against both update feeds: CN 5.9.3.42398134 with @genie/agent-cli
	// 2.165.0, international 5.8.4.41366630 with CLI 2.161.0). 5.9 is also
	// when the CN desktop started sending X-Agent-Runtime. The older 2.176.0
	// figure came from misakano 244d4e6 and predates these builds.
	clientVersionCN     = "5.9.3"
	clientVersionGlobal = "5.8.4"
	cliVersionCN        = "2.165.0"
	cliVersionGlobal    = "2.161.0"
	cliVersion          = cliVersionCN // legacy alias used by the growth/billing paths
	stainlessSDKVersion   = "6.25.0"
	stainlessNodeVersion  = "v22.21.1"
	reportPath            = "/v2/report"
	appearanceSetPath     = "/v2/user-asset/appearance/set"
	growthTasksPath       = "/v2/activity/growth/tasks"
	growthAcceptPath      = "/v2/activity/growth/tasks/accept"
	streakPath            = "/activity/growth/streak"
	streakRedeemPath      = "/activity/growth/redeem"
	lotterySummaryPath    = "/activity/growth/lottery/summary"
	lotteryDrawPath       = "/activity/growth/lottery/draw"
	buddyInfoPath         = "/activity/growth/buddy/info"
	buddyFirstPath        = "/activity/growth/buddy/first"
	buddyAgreementPath    = "/activity/growth/buddy/agreement"
	travelStatusPath      = "/activity/growth/buddy/travel/status"
	travelDepartPath      = "/activity/growth/buddy/travel/depart"
	travelClaimPath       = "/activity/growth/buddy/travel/claim"
	expertListPath        = "/portal/operation-platform/market/expert/list"
	defaultWebBaseCN      = "https://www.workbuddy.cn"
	buddyTaskIncomplete   = "first_buddy task not completed yet"
	desktopWebEventOrigin = "/C:/Program%20Files/WorkBuddy/resources/app.asar/renderer/index.html"
)

// workBuddyUA is the desktop client's User-Agent for a realm. The first
// segment stays the camel-case product name on both packages; only the second
// brand segment follows the distribution, and the version numbers differ
// because the two builds ship on different cadences.
func workBuddyUA(global bool) string {
	if global {
		return desktopUAGlobal
	}
	return desktopUA
}

// workBuddyClientVersion is the X-IDE-Version the client reports for a realm.
func workBuddyClientVersion(global bool) string {
	if global {
		return clientVersionGlobal
	}
	return clientVersionCN
}

// webBase resolves the web-domain base (configurable; defaults to the
// official CN web host).
func (d *Driver) webBase() string {
	if d.cfg.WebBaseCN != "" {
		return strings.TrimRight(d.cfg.WebBaseCN, "/")
	}
	return defaultWebBaseCN
}

// --- generic transport ---

// growthCall posts/gets a growth-domain JSON call (chatBase + Billing-style
// headers + {code,data} envelope unwrap). Edge-level 401s are retried once —
// see edgeJitter401.
func (d *Driver) growthCall(ctx context.Context, creds *Creds, method, path string, body any, extraHeaders map[string]string) (json.RawMessage, error) {
	data, err := d.growthCallOnce(ctx, creds, method, path, body, extraHeaders)
	if err == nil || !edgeJitter401(err) {
		return data, err
	}
	select {
	case <-ctx.Done():
		return nil, err
	case <-time.After(300 * time.Millisecond):
	}
	return d.growthCallOnce(ctx, creds, method, path, body, extraHeaders)
}

// edgeJitter401 reports whether err is the CDN/edge HTML 401 (APISIX
// "<title>401 Authorization Required</title>") that occasionally replaces a
// normal response. It is a transient gateway fault, not a dead credential:
// the same request usually succeeds on an immediate retry.
func edgeJitter401(err error) bool {
	ue, ok := uperr.As(err)
	if !ok || ue.Status != 401 {
		return false
	}
	msg := strings.ToLower(ue.Message)
	return strings.Contains(msg, "<html") || strings.Contains(msg, "authorization required")
}

// IsAuthDead reports whether err means the credential itself is unusable
// (as opposed to an edge/CDN hiccup that merely looks like a 401).
func IsAuthDead(err error) bool {
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassAuth {
		return false
	}
	return !edgeJitter401(err)
}

// growthCallOnce performs a single growth-domain attempt.
func (d *Driver) growthCallOnce(ctx context.Context, creds *Creds, method, path string, body any, extraHeaders map[string]string) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	base := d.chatBase(creds)
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("X-Domain", base)
	req.Header.Set("X-Product", "SaaS")
	if creds.UID != "" {
		req.Header.Set("X-User-Id", creds.UID)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := d.rpc.Do(req)
	if err != nil {
		return nil, uperr.New(uperr.ClassTransport, 0, "workbuddy growth: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		// Surface the buddy-task-incomplete marker to callers as bad_request
		// with the business message intact.
		if resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(string(data)), buddyTaskIncomplete) {
			return nil, uperr.New(uperr.ClassBadRequest, 400, "workbuddy: %s", truncate(data, 200))
		}
		return nil, classifyHTTP(resp.StatusCode, data, "")
	}
	// Envelope: {"code":0,"data":...}; some endpoints return bare objects.
	// Message text lives in `msg` (billing/growth envelope) — `message` is
	// only a fallback (ticket 30).
	var env struct {
		Code    int             `json:"code"`
		Msg     string          `json:"msg"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return data, nil
	}
	if env.Code != 0 {
		msg := env.Msg
		if msg == "" {
			msg = env.Message
		}
		return nil, classifyBusiness(env.Code, msg, "")
	}
	if len(env.Data) == 0 {
		return data, nil
	}
	return env.Data, nil
}

// deriveID produces a stable 36-hex device identifier from the uid (same
// account → same device, mirroring the reference implementation).
func deriveID(uid, salt string) string {
	sum := sha256.Sum256([]byte(salt + ":" + uid))
	return hex.EncodeToString(sum[:18])
}

func uuidToken() string {
	return newUUIDv4()
}

// --- desktop fingerprint reporting ---

// desktopEvent is one business event payload.
type desktopEvent map[string]any

func desktopFingerprint(creds *Creds) map[string]any {
	now := time.Now().UnixMilli()
	return map[string]any{
		"timezone":     "Asia/Shanghai",
		"reportDelay":  2000,
		"userId":       creds.UID,
		"username":     creds.Nickname,
		"userNickname": creds.Nickname,
		"product":      "SaaS",
		"releaseDate":  int64(1789036585355),
		"commit":       "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName":      "WorkBuddy",
		"ideType":      "WorkBuddy",
		"ideVersion":   "5.5.6",
		"machineId":    deriveID(creds.UID, "machine"),
		"sessionId":    deriveID(creds.UID, "session"),
		"extName":      "workbuddy-desktop",
		"extVersion":   "5.5.6",
		"os":           "win32",
		"arch":         "x64",
		"osVersion":    "10.0.26220",
		"cpuCores":     20,
		"memorySize":   24,
		"timestamp":    now,
		"presentAt":    now,
	}
}

// sendReport executes a report-style POST and classifies the response with the
// shared taxonomy (so a 401 arrives as ClassAuth and can be self-healed) and
// retries once on an edge-level HTML 401. The request is rebuilt per attempt
// because the body reader is consumed.
func (d *Driver) sendReport(ctx context.Context, label string, build func() (*http.Request, error)) error {
	attempt := func() error {
		req, err := build()
		if err != nil {
			return err
		}
		resp, err := d.rpc.Do(req)
		if err != nil {
			return uperr.New(uperr.ClassTransport, 0, "workbuddy %s: %v", label, err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			return classifyHTTP(resp.StatusCode, data, "")
		}
		return nil
	}
	err := attempt()
	if err == nil || !edgeJitter401(err) {
		return err
	}
	select {
	case <-ctx.Done():
		return err
	case <-time.After(300 * time.Millisecond):
	}
	return attempt()
}

// reportDesktopEvents posts a batch of desktop-fingerprint events.
func (d *Driver) reportDesktopEvents(ctx context.Context, creds *Creds, events []desktopEvent) error {
	if len(events) == 0 {
		return nil
	}
	fp := desktopFingerprint(creds)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := map[string]any{}
		for k, v := range fp {
			m[k] = v
		}
		for k, v := range ev {
			m[k] = v
		}
		arr = append(arr, m)
	}
	raw, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	base := d.chatBase(creds)
	return d.sendReport(ctx, "report", func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+reportPath, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
		req.Header.Set("User-Agent", desktopUA)
		req.Header.Set("X-Domain", base)
		req.Header.Set("X-Product", "SaaS")
		req.Header.Set("X-Request-ID", deriveID(creds.UID, "req")+fmt.Sprintf("%d", time.Now().UnixNano()%1e6))
		if creds.UID != "" {
			req.Header.Set("X-User-Id", creds.UID)
		}
		return req, nil
	})
}

// reportWebEvent posts a single browser-fingerprint event (Library_read etc).
func (d *Driver) reportWebEvent(ctx context.Context, creds *Creds, eventCode, pageURL, elementID, elementName string) error {
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
	ev := map[string]any{
		"eventCode": eventCode, "timestamp": time.Now().UnixMilli(), "reportDelay": 0,
		"pageURL": pageURL, "elementId": elementID, "elementName": elementName,
		"os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": ua,
		"machineId": deriveID(creds.UID, "webmachine"), "userId": creds.UID,
		"userNickname": creds.Nickname, "enterpriseId": creds.EnterpriseID,
	}
	raw, _ := json.Marshal([]map[string]any{ev})
	return d.sendReport(ctx, "web report", func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.webBase()+"/v2/report", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-client-platform", "web")
		req.Header.Set("Origin", d.webBase())
		req.Header.Set("Referer", pageURL)
		req.Header.Set("User-Agent", ua)
		if creds.UID != "" {
			req.Header.Set("X-User-Id", creds.UID)
		}
		return req, nil
	})
}

// DesktopChatSequence builds the six-event "successful desktop chat" chain
// that lights RichMeow_Chat (and joins template/playbook/canvas chains).
func DesktopChatSequence(conversationID, requestID, messageID, modelID, modelName string) []desktopEvent {
	mk := func(code string, extra map[string]any) desktopEvent {
		ev := desktopEvent{"eventCode": code}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []desktopEvent{
		mk("agent_task_created", map[string]any{
			"source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
			"requestModelId": modelID, "requestModelName": modelName,
			"has_repo": false, "repo_type": "none", "workspace_type": "empty",
			"has_connector": false, "connector_types": []any{},
			"has_mention": false, "mention_types": []any{},
			"has_template": false, "action": "", "template_name": "",
			"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
			"has_skill": false, "skill_names": []any{},
			"conversationId": conversationID, "messageId": messageID,
			"buddyId": "", "buddyName": "",
		}),
		mk("chat_message_send", map[string]any{
			"messageId": messageID + "-assistant", "historyCount": 0,
			"isContextTruncated": false, "currentStepCount": 1,
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		mk("chat_request_send", map[string]any{
			"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
			"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
			"maxSteps": 500, "temperature": 0, "maxRetries": 0,
			"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
			"codebaseId": "", "mentionContextCount": 0, "command": "",
			"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_response", map[string]any{
			"messageId": messageID + "-assistant", "responseModelId": modelID,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"firstTokenAt": time.Now().UnixMilli(), "traceId": requestID,
			"conversationId": conversationID,
			"rootRequestId":  requestID, "parentConversationId": conversationID,
			"agentName": "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_status", map[string]any{
			"messageId": messageID + "-assistant", "messageErrorCode": "0",
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		mk("chat_request_response", map[string]any{
			"mode": "craft", "toolCallCount": 0,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"rootRequestId": requestID, "parentConversationId": conversationID,
		}),
	}
}

// BuddyAppBuddyID is the penguin-teacher assistant (Buddy_App_QQ criterion).
const (
	BuddyAppBuddyID   = "cb_y5Dy46tPQGGWtueMxXbe"
	BuddyAppBuddyName = "企鹅教师助手"
)

// DesktopBuddyAppSequence lights Buddy_App and Buddy_App_QQ.
func DesktopBuddyAppSequence(buddyID, buddyName string) []desktopEvent {
	mk := func(code string, extra map[string]any) desktopEvent {
		ev := desktopEvent{"eventCode": code, "mode": "LOCAL", "buddyId": buddyID, "buddyName": buddyName}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []desktopEvent{
		mk("buddyapp_discover_click", nil),
		mk("buddyapp_show", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2}),
		mk("buddyapp_enter_click", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2, "isFirstPage": "1"}),
		mk("buddyapp_auth_confirm_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
		mk("buddyapp_bindaccount_skip_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
	}
}

// DesktopAutomationCreateEvent lights automation_1.
func DesktopAutomationCreateEvent(name string) desktopEvent {
	return desktopEvent{
		"eventCode": "automated_task_create_suc", "name": name,
		"source": "manually", "modelId": "fast-model", "modelIsThinking": true,
		"connectorCount": 0, "skills": "", "skillCount": 0,
		"scheduleType": "once", "mode": "LOCAL",
	}
}

// DesktopTemplateUseSequence lights template_5 (call five times with
// different template ids).
func DesktopTemplateUseSequence(conversationID, requestID, templateID, templateName string) []desktopEvent {
	events := DesktopChatSequence(conversationID, requestID, "msg-"+templateID, "fast-model", "fast-model")
	return append(events,
		desktopEvent{
			"eventCode": "agent_task_created_with_template", "mode": "working",
			"isCustomModel": false, "id": templateID, "name": templateName, "requestId": requestID,
		},
		desktopEvent{"eventCode": "template_used", "template_id": templateID, "task_mode": "working"},
	)
}

// DesktopPlaybookPromptSequence lights playbook_prompt.
func DesktopPlaybookPromptSequence(conversationID, requestID, caseID, caseName string) []desktopEvent {
	events := DesktopChatSequence(conversationID, requestID, "msg-pb", "fast-model", "fast-model")
	payload := map[string]any{"id": caseID, "name": caseName, "type": "document", "categoryId": "", "categoryName": ""}
	click := desktopEvent{
		"eventCode": "web_element_click", "pageName": "playbook_detail",
		"elementId": "playbook_ctaClick", "elementName": caseName, "source": "discover",
	}
	cta := desktopEvent{"eventCode": "playbook_cta_click", "source": "discover", "position": 0}
	for k, v := range payload {
		cta[k] = v
	}
	send := desktopEvent{"eventCode": "playbook_prompt_send", "conversationId": conversationID, "requestId": requestID}
	for k, v := range payload {
		send[k] = v
	}
	return append(events, click, cta, send)
}

// DesktopDesignCanvasSequence lights create_canvas.
func DesktopDesignCanvasSequence(conversationID, requestID string) []desktopEvent {
	events := DesktopChatSequence(conversationID, requestID, "msg-canvas", "fast-model", "fast-model")
	suffix := requestID
	if len(suffix) >= 8 {
		suffix = suffix[len(suffix)-8:]
	}
	return append(events,
		desktopEvent{
			"eventCode": "wbx_design_canvas_task_create", "conversationId": conversationID,
			"requestId": requestID, "source": "summon_keyword", "cost": 12000, "isSuccessful": true,
		},
		desktopEvent{
			"eventCode": "wbx_design_canvas_open", "conversationId": conversationID,
			"requestId": requestID, "id": "ardot-file-" + suffix,
			"source": "summon_keyword", "type": "page", "cost": 13000, "isSuccessful": true,
		},
	)
}

// MarketExpert is one platform expert.
type MarketExpert struct {
	ExpertID      string `json:"expert_id"`
	ExpertType    string `json:"expert_type"`
	DisplayNameZH string `json:"display_name_zh"`
	ProfessionZH  string `json:"profession_zh"`
	Version       string `json:"version"`
	Categories    []any  `json:"categories"`
}

// marketExpertList fetches real experts (fabricated ids do not count).
func (d *Driver) marketExpertList(ctx context.Context, creds *Creds, expertType string) ([]MarketExpert, error) {
	body := map[string]any{"page": 1, "page_size": 20, "sort_by": "reco_rank", "sort_order": "desc"}
	if expertType != "" {
		body["expert_type"] = expertType
	}
	data, err := d.growthCall(ctx, creds, http.MethodPost, expertListPath, body, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Experts []MarketExpert `json:"experts"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out.Experts, nil
}

// desktopChatWithExpert sends a real desktop-fingerprint chat request and
// returns the server-issued request id (server ids only count for the
// expert_actual_use JOIN). expertID is applied via the X-Expert-Id header.
func (d *Driver) desktopChatWithExpert(ctx context.Context, creds *Creds, expertID string) (conversationID, requestID string, err error) {
	return d.desktopChatOnce(ctx, creds, expertID)
}

func (d *Driver) desktopChatOnce(ctx context.Context, creds *Creds, expertID string) (string, string, error) {
	conversationID := randomUUIDV4()
	body := map[string]any{
		"model": "fast-model",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant. 当前处于中文环境，使用简体中文回答。"},
			map[string]any{"role": "user", "content": "1+1等于几？直接回答。"},
		},
		"agent":          "cli",
		"temperature":    1,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	raw, _ := json.Marshal(body)
	base := d.chatBase(creds)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v2/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	h := req.Header
	h.Set("Authorization", "Bearer "+creds.AccessToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("User-Agent", workBuddyUA(creds.RealmOrDefault() == "global"))
	h.Set("X-Domain", base)
	h.Set("X-Product", "SaaS")
	h.Set("X-User-Id", creds.UID)
	// A hand-built header set drifted from the chat path (it missed the
	// trace/B3/stainless/acp segment and pinned X-IDE-Version). Route it
	// through the same builder so the two paths cannot diverge again (BP-90).
	call := driver.ChatCall{
		Account:       nil,
		UpstreamModel: "fast-model",
		Request:       &ir.ChatRequest{SessionKey: conversationID},
	}
	d.applyClientFingerprint(req, creds, call)
	if expertID != "" {
		h.Set("X-Expert-Id", expertID)
	}
	resp, err := d.rpc.Do(req)
	if err != nil {
		return "", "", uperr.New(uperr.ClassTransport, 0, "workbuddy desktop chat: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", "", uperr.New(uperr.ClassInternal, resp.StatusCode, "workbuddy desktop chat: %s", truncate(b, 200))
	}
	// Scan the SSE stream for the server-issued request id (`"id":"..."`).
	buf := make([]byte, 0, 1<<20)
	tmp := make([]byte, 8192)
	searchFrom := 0
	for {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if i := bytes.Index(buf[searchFrom:], []byte(`"id":"`)); i >= 0 {
				abs := searchFrom + i
				rest := buf[abs+6:]
				if end := bytes.IndexByte(rest, '"'); end > 0 {
					id := string(rest[:end])
					if isServerRequestID(id) {
						return conversationID, id, nil
					}
					searchFrom = abs + 1
				}
			}
		}
		if rerr != nil || len(buf) > 1<<20 {
			break
		}
	}
	return "", "", uperr.New(uperr.ClassInternal, 0, "workbuddy desktop chat: server request id not found")
}

// isServerRequestID matches cmb-<32hex> or <32hex>.
func isServerRequestID(id string) bool {
	s := strings.TrimPrefix(id, "cmb-")
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// expertSummonSequence lights expert_summon click chains.
func expertSummonSequence(e MarketExpert) []desktopEvent {
	cat := "expert-all"
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok {
			cat = s
		}
	}
	ver := e.Version
	if ver == "" {
		ver = "1.0.0"
	}
	return []desktopEvent{
		{
			"eventCode": "web_element_click", "source": e.ExpertID, "type": cat, "version": ver,
			"elementId": "expert_summon_click", "elementName": "立即召唤",
			"pageURL": desktopWebEventOrigin,
		},
		{
			"eventCode": "expert_summon_click", "id": e.ExpertID, "name": e.DisplayNameZH,
			"expertTitle": e.ProfessionZH, "type": "expert-all", "position": 0,
			"expertType": e.ExpertType, "version": ver, "mode": "LOCAL",
		},
		{
			"eventCode": "expert_summoned", "id": e.ExpertID, "name": e.DisplayNameZH,
			"expertTitle": e.ProfessionZH, "type": "expert-all",
		},
	}
}

// expertActualUseEvent lights expert_5 / Expert_team_use_3 / Expert_lighthouse.
func expertActualUseEvent(e MarketExpert, conversationID, requestID string, local bool) desktopEvent {
	cat := "expert-all"
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok {
			cat = s
		}
	}
	ver := e.Version
	if ver == "" {
		ver = "1.0.0"
	}
	suffix := requestID
	if len(suffix) >= 8 {
		suffix = suffix[len(suffix)-8:]
	}
	ev := desktopEvent{
		"eventCode": "expert_actual_use",
		"id":        e.ExpertID, "name": e.DisplayNameZH, "expertTitle": e.ProfessionZH,
		"type": cat, "expertType": e.ExpertType, "source": "builtin", "version": ver,
		"cost": 9000, "characterCount": 14,
		"conversationId": conversationID, "requestId": requestID, "messageId": "msg-" + suffix,
		"requestModelId": "fast-model", "requestModelName": "fast-model",
	}
	if local {
		ev["mode"] = "LOCAL"
		ev["type"] = cat
		ev["cost"] = 0
	} else {
		ev["mode"] = "craft"
	}
	return ev
}

// --- growth API surface ---

// GrowthTask is one growth-center task row.
//
// Field shapes drift upstream (verified 2026-10-18): `progress` is an OBJECT
// {"current":N,"target":M} on the CN growth center, not a bare int — a plain
// int field failed the whole list with "cannot unmarshal object into Go
// struct field .tasks.progress of type int", so the night-owl task could never
// read its own progress. `status` is also gone from the payload, which is why
// completion is now derived from progress/accept_status (see GrowthTask.Done).
type GrowthTask struct {
	TaskCode     string `json:"task_code"`
	TaskName     string `json:"task_name"`
	Status       string `json:"status"`        // legacy; absent upstream since 2026-10
	AcceptStatus string `json:"accept_status"` // not_accepted / accepted / claimed
	Progress     int    `json:"-"`             // current progress, flattened from progress/target
	Target       int    `json:"-"`             // target progress, flattened from progress/target
}

// Done reports whether the task's progress target has been reached. Upstream
// dropped the `status` field, so "completed" must come from the numbers.
func (t GrowthTask) Done() bool {
	return t.Target > 0 && t.Progress >= t.Target
}

// Claimed reports whether the reward was already collected.
func (t GrowthTask) Claimed() bool {
	return strings.EqualFold(t.AcceptStatus, "claimed")
}

// parseGrowthTaskProgress flattens the two shapes upstream has used for a
// task's progress: the nested object {"current":N,"target":M} and the older
// flat current/target/progress numbers. Returns zeroes when neither is usable.
func parseGrowthTaskProgress(raw json.RawMessage, flatCurrent, flatTarget int) (progress, target int) {
	progress, target = flatCurrent, flatTarget
	if len(raw) == 0 || string(raw) == "null" {
		return progress, target
	}
	// Shape A: {"current":N,"target":M}
	var obj struct {
		Current *int `json:"current"`
		Target  *int `json:"target"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && (obj.Current != nil || obj.Target != nil) {
		if obj.Current != nil {
			progress = *obj.Current
		}
		if obj.Target != nil {
			target = *obj.Target
		}
		return progress, target
	}
	// Shape B: a bare number.
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		progress = n
	}
	return progress, target
}

// GrowthTasks lists growth tasks (parsed leniently across field variants).
func (d *Driver) GrowthTasks(ctx context.Context, acc *model.Account) ([]GrowthTask, error) {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	data, err := d.growthCall(ctx, creds, http.MethodGet, growthTasksPath, nil, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tasks []struct {
			TaskCode     string          `json:"task_code"`
			Code         string          `json:"code"`
			TaskName     string          `json:"task_name"`
			Title        string          `json:"title"`
			Name         string          `json:"name"`
			Status       string          `json:"status"`
			AcceptStatus string          `json:"accept_status"`
			Progress     json.RawMessage `json:"progress"`
			Current      int             `json:"current"`
			Target       int             `json:"target"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	out := make([]GrowthTask, 0, len(resp.Tasks))
	for _, t := range resp.Tasks {
		code := t.TaskCode
		if code == "" {
			code = t.Code
		}
		name := t.TaskName
		if name == "" {
			name = t.Title
		}
		if name == "" {
			name = t.Name
		}
		progress, target := parseGrowthTaskProgress(t.Progress, t.Current, t.Target)
		out = append(out, GrowthTask{
			TaskCode: code, TaskName: name, Status: t.Status,
			AcceptStatus: t.AcceptStatus, Progress: progress, Target: target,
		})
	}
	return out, nil
}

// GrowthAccept accepts task codes (not_accepted → accepted).
func (d *Driver) GrowthAccept(ctx context.Context, acc *model.Account, codes []string) error {
	if len(codes) == 0 {
		return nil
	}
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	_, err = d.growthCall(ctx, creds, http.MethodPost, growthAcceptPath, map[string]any{"task_codes": codes}, nil)
	return err
}

// GrowthClaim claims the reward of one completed+accepted task (web domain).
func (d *Driver) GrowthClaim(ctx context.Context, acc *model.Account, taskCode string) error {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	base := d.webBase()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/activity/growth/tasks/"+taskCode+"/claim", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("Referer", base+"/profile/growth-center")
	if creds.UID != "" {
		req.Header.Set("X-User-Id", creds.UID)
	}
	resp, err := d.rpc.Do(req)
	if err != nil {
		return uperr.New(uperr.ClassTransport, 0, "workbuddy claim: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return uperr.New(uperr.ClassInternal, resp.StatusCode, "workbuddy claim: %s", truncate(data, 200))
	}
	return nil
}

// StreakStatus returns the current consecutive-login days.
func (d *Driver) StreakStatus(ctx context.Context, acc *model.Account) (int, error) {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return 0, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	data, err := d.growthCall(ctx, creds, http.MethodGet, streakPath, nil, nil)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Streak struct {
			Days int `json:"days"`
		} `json:"streak"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, err
	}
	return resp.Streak.Days, nil
}

// RedeemTier redeems a streak tier ("7d" | "14d" | "28d").
func (d *Driver) RedeemTier(ctx context.Context, acc *model.Account, tier string) error {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	_, err = d.growthCall(ctx, creds, http.MethodPost, streakRedeemPath,
		map[string]any{"tier": tier, "client_token": uuidToken()}, nil)
	return err
}

// LotteryChances returns the remaining lottery draws.
func (d *Driver) LotteryChances(ctx context.Context, acc *model.Account) (int, error) {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return 0, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	data, err := d.growthCall(ctx, creds, http.MethodGet, lotterySummaryPath, nil, nil)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Chances int `json:"chances"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, err
	}
	return resp.Chances, nil
}

// LotteryDraw draws once.
func (d *Driver) LotteryDraw(ctx context.Context, acc *model.Account) error {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	_, err = d.growthCall(ctx, creds, http.MethodPost, lotteryDrawPath,
		map[string]any{"client_token": uuidToken()}, nil)
	return err
}

// HasBuddy reports whether the account has adopted a cat (agentpoints
// BuddyInfo: data.buddy null / missing / empty object → no buddy).
func (d *Driver) HasBuddy(ctx context.Context, acc *model.Account) (bool, error) {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return false, uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	data, err := d.growthCall(ctx, creds, http.MethodGet, buddyInfoPath, nil, nil)
	if err != nil {
		return false, err
	}
	var resp struct {
		Buddy json.RawMessage `json:"buddy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, err
	}
	trimmed := strings.TrimSpace(string(resp.Buddy))
	return trimmed != "" && trimmed != "null" && trimmed != "{}", nil
}

// BuddyAdopt adopts the first buddy: agreement + first. The first call may
// return 400 "first_buddy task not completed yet" (expected before the
// unlock); callers treat that as a skip.
func (d *Driver) BuddyAdopt(ctx context.Context, acc *model.Account) (adopted bool, note string, err error) {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return false, "", uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	if _, err := d.growthCall(ctx, creds, http.MethodPost, buddyAgreementPath, map[string]any{"agree": true}, nil); err != nil {
		return false, "", err
	}
	if _, err := d.growthCall(ctx, creds, http.MethodPost, buddyFirstPath, map[string]any{}, nil); err != nil {
		if ue, ok := uperr.As(err); ok && ue.Status == 400 && strings.Contains(strings.ToLower(ue.Message), buddyTaskIncomplete) {
			return false, "buddy adoption locked (first_buddy task not completed yet)", nil
		}
		return false, "", err
	}
	return true, "buddy adopted (+300 credits)", nil
}

// TravelButler runs the cat travel loop: claim arrived rewards, depart if
// idle and not yet departed today.
func (d *Driver) TravelButler(ctx context.Context, acc *model.Account) (note string, err error) {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return "", uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	data, err := d.growthCall(ctx, creds, http.MethodGet, travelStatusPath, nil, nil)
	if err != nil {
		return "", err
	}
	var st struct {
		State             string `json:"state"`
		DailyLimitReached bool   `json:"daily_limit_reached"`
		RecordID          int64  `json:"record_id"`
		RewardCredit      int64  `json:"reward_credit"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return "", err
	}
	claimed := int64(0)
	if st.State == "arrived" && st.RecordID != 0 {
		data, err := d.growthCall(ctx, creds, http.MethodPost, travelClaimPath, map[string]any{"record_id": st.RecordID}, nil)
		if err == nil {
			var resp struct {
				RewardCredit int64 `json:"reward_credit"`
			}
			if len(data) > 0 {
				_ = json.Unmarshal(data, &resp)
			}
			claimed = resp.RewardCredit
		} else {
			return "", err
		}
	}
	departed := false
	if st.State == "idle" && !st.DailyLimitReached {
		if err := func() error {
			_, err := d.growthCall(ctx, creds, http.MethodPost, travelDepartPath, map[string]any{"location_id": 1}, nil)
			return err
		}(); err == nil {
			departed = true
		} else {
			return "", err
		}
	}
	switch {
	case claimed > 0 && departed:
		return fmt.Sprintf("claimed %d credits + departed", claimed), nil
	case claimed > 0:
		return fmt.Sprintf("claimed %d credits", claimed), nil
	case departed:
		return "buddy departed on a trip", nil
	default:
		return "travel state: " + st.State, nil
	}
}

// ReportChatActivity posts one chat-activity report (lights chat_5 and the
// streak check).
func (d *Driver) ReportChatActivity(ctx context.Context, acc *model.Account, conversationID, requestID, modelID, modelName string) error {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	if requestID == "" {
		requestID = conversationID
	}
	if modelID == "" {
		modelID = "deepseek-v4-flash"
	}
	if modelName == "" {
		modelName = modelID
	}
	now := time.Now().UnixMilli()
	ev := map[string]any{
		"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
		"mode": "craft", "conversationId": conversationID, "requestId": requestID,
		"inputLength":    12,
		"requestModelId": modelID, "requestModelName": modelName,
		"isPlan": false, "isAutoExecuteTerminal": false, "isAutoModify": false,
		"codebaseEnable": false, "maxToken": 0, "maxSteps": 0, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
		"codebaseId": "", "mentionContextCount": 0, "command": "", "expertId": "",
		"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0, "fileUri": "",
		"presentAt": now, "traceId": "", "rootRequestId": requestID,
		"parentConversationId": conversationID, "agentName": "default", "agentType": "conversation",
		"userId": creds.UID,
	}
	raw, _ := json.Marshal([]map[string]any{ev})
	base := d.chatBase(creds)
	return d.sendReport(ctx, "activity report", func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+reportPath, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", desktopUA)
		req.Header.Set("X-Domain", base)
		req.Header.Set("X-Product", "SaaS")
		if creds.UID != "" {
			req.Header.Set("X-User-Id", creds.UID)
		}
		return req, nil
	})
}

// SetAppearanceTheme applies an appearance theme (light/dark/theme-tkmwj).
func (d *Driver) SetAppearanceTheme(ctx context.Context, acc *model.Account, resourceKey string) error {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return uperr.New(uperr.ClassAuth, 0, "%v", err)
	}
	_, err = d.growthCall(ctx, creds, http.MethodPost, appearanceSetPath,
		map[string]string{"kind": "theme", "resource_key": resourceKey}, nil)
	return err
}

// GrowthAutomation bundles the per-account growth pipeline used by the task
// center. Every step is best-effort; the returned detail string records what
// happened for the task run ledger.
type growthStepResult struct {
	Step string
	OK   bool
	Note string
}

// RunGrowthPipeline lights the automatable growth tasks for one account.
// Ordered so early tasks unlock later ones (activity report unlocks
// first_buddy, adoption feeds the travel reward chain).
func (d *Driver) RunGrowthPipeline(ctx context.Context, acc *model.Account) ([]growthStepResult, error) {
	var results []growthStepResult
	var firstErr error
	record := func(step string, err error, note string) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
		results = append(results, growthStepResult{Step: step, OK: err == nil, Note: note})
	}

	// 1. Chat activity (lights chat_5 progress + growth streak).
	for i := 0; i < 5; i++ {
		cid := randomUUIDV4()
		err := d.ReportChatActivity(ctx, acc, cid, "", "", "")
		if err != nil {
			record("chat_5", err, "")
			break
		}
		if i == 4 {
			record("chat_5", nil, "5 activity reports sent")
		}
	}

	// 2. Real chat on glm-5.2 (Model_chat_GLM5.2).
	glmCid := randomUUIDV4()
	if err := d.ReportChatActivity(ctx, acc, glmCid, "", "glm-5.2", "glm-5.2"); err == nil {
		record("Model_chat_GLM5.2", nil, "glm-5.2 activity reported")
	} else {
		record("Model_chat_GLM5.2", err, "")
	}

	// 3. Buddy adoption (first_buddy; +300c when unlocked).
	if adopted, note, err := d.BuddyAdopt(ctx, acc); err == nil {
		record("first_buddy", nil, note)
		_ = adopted
	} else {
		record("first_buddy", err, "")
	}

	// 4. Desktop chat chain (RichMeow_Chat).
	conv := randomUUIDV4()
	reqID := uuidToken()
	if err := d.reportDesktopEvents(ctx, mustCreds(acc), DesktopChatSequence(conv, reqID, "msg-rich", "fast-model", "fast-model")); err == nil {
		record("RichMeow_Chat", nil, "desktop chat chain sent")
	} else {
		record("RichMeow_Chat", err, "")
	}

	// 5. Buddy app enter chain (Buddy_App + Buddy_App_QQ).
	if err := d.reportDesktopEvents(ctx, mustCreds(acc), DesktopBuddyAppSequence(BuddyAppBuddyID, BuddyAppBuddyName)); err == nil {
		record("Buddy_App", nil, "buddy app chain sent")
		results = append(results, growthStepResult{Step: "Buddy_App_QQ", OK: true, Note: "shared chain"})
	} else {
		record("Buddy_App", err, "")
	}

	// 6. Automation create event (automation_1).
	if err := d.reportDesktopEvents(ctx, mustCreds(acc), []desktopEvent{DesktopAutomationCreateEvent(randomUUIDV4())}); err == nil {
		record("automation_1", nil, "automation event sent")
	} else {
		record("automation_1", err, "")
	}

	// 7. Library read (web event, Library_read).
	if err := d.reportWebEvent(ctx, mustCreds(acc), "library_doc_intro_click", d.webBase()+"/library", "library-doc-intro", "资料库介绍"); err == nil {
		record("Library_read", nil, "library click sent")
	} else {
		record("Library_read", err, "")
	}

	// 8. Template use ×5 (template_5).
	templates := []struct{ id, name string }{
		{"tpl-web-app", "网页应用"}, {"tpl-doc-summary", "文档总结"}, {"tpl-code-review", "代码审查"},
		{"tpl-data-chart", "数据图表"}, {"tpl-api-debug", "接口调试"},
	}
	allTemplates := true
	for _, tpl := range templates {
		conv := randomUUIDV4()
		reqID := uuidToken()
		if err := d.reportDesktopEvents(ctx, mustCreds(acc), DesktopTemplateUseSequence(conv, reqID, tpl.id, tpl.name)); err != nil {
			allTemplates = false
		}
	}
	record("template_5", errFromBool(allTemplates), "5 template chains sent")

	// 9. Playbook prompt (playbook_prompt).
	pbConv := randomUUIDV4()
	pbReq := uuidToken()
	record("playbook_prompt", d.reportDesktopEvents(ctx, mustCreds(acc), DesktopPlaybookPromptSequence(pbConv, pbReq, "case-bp-1", "灵感案例")), "playbook chain sent")

	// 10. Design canvas (create_canvas).
	cvConv := randomUUIDV4()
	cvReq := uuidToken()
	record("create_canvas", d.reportDesktopEvents(ctx, mustCreds(acc), DesktopDesignCanvasSequence(cvConv, cvReq)), "canvas chain sent")

	// 11. Expert summon + actual use ×3 (expert_5 needs 5; team ×3).
	experts, err := d.marketExpertList(ctx, mustCreds(acc), "agent")
	if err != nil || len(experts) == 0 {
		record("expert_5", errFromBool(false), "expert market unavailable")
	} else {
		needed := 5
		if len(experts) < needed {
			needed = len(experts)
		}
		ok := 0
		for i := 0; i < needed; i++ {
			if d.runExpertUse(ctx, acc, experts[i], false) {
				ok++
			}
		}
		record("expert_5", errFromBool(ok >= 5), fmt.Sprintf("%d/5 expert uses completed", ok))
	}

	// 12. Expert team use ×3.
	teams, err := d.marketExpertList(ctx, mustCreds(acc), "team")
	if err == nil && len(teams) > 0 {
		needed := 3
		if len(teams) < needed {
			needed = len(teams)
		}
		ok := 0
		for i := 0; i < needed; i++ {
			if d.runExpertUse(ctx, acc, teams[i], false) {
				ok++
			}
		}
		if len(teams) < 3 && ok > 0 {
			// Fall back to agent experts to reach 3 uses.
			for i := 0; i < 3-ok && i < len(experts); i++ {
				if d.runExpertUse(ctx, acc, experts[i], false) {
					ok++
				}
			}
		}
		record("Expert_team_use_3", errFromBool(ok >= 3), fmt.Sprintf("%d/3 team uses completed", ok))
	}

	// 13. Expert lighthouse (single expert, LOCAL mode).
	if len(experts) > 0 {
		record("Expert_lighthouse", errFromBool(d.runExpertUse(ctx, acc, experts[0], true)), "lighthouse expert use")
	}

	// 14. Theme appearance (Hp_Appearance best effort).
	record("Hp_Appearance", d.SetAppearanceTheme(ctx, acc, "theme-tkmwj"), "theme applied")

	// 15. Travel butler.
	if note, err := d.TravelButler(ctx, acc); err == nil {
		record("buddy_travel", nil, note)
	} else {
		record("buddy_travel", err, "")
	}

	// 16. Accept + claim pass over the task list.
	tasks, err := d.GrowthTasks(ctx, acc)
	if err != nil {
		record("claim_pass", err, "")
		return results, err
	}
	var toAccept []string
	for _, t := range tasks {
		if strings.EqualFold(t.AcceptStatus, "not_accepted") {
			toAccept = append(toAccept, t.TaskCode)
		}
	}
	if len(toAccept) > 0 {
		if err := d.GrowthAccept(ctx, acc, toAccept); err != nil {
			record("accept_pass", err, "")
		} else {
			record("accept_pass", nil, fmt.Sprintf("accepted %d tasks", len(toAccept)))
		}
	}
	claimed := 0
	claimNote := ""
	var claimErr error
	for _, t := range tasks {
		// Upstream no longer sends `status`; "completed" now comes from the
		// progress numbers and "already collected" from accept_status=claimed.
		if !t.Done() || t.Claimed() {
			continue
		}
		if err := d.GrowthClaim(ctx, acc, t.TaskCode); err == nil {
			claimed++
		} else if claimNote == "" {
			claimNote = err.Error()
			claimErr = err
		}
	}
	note := fmt.Sprintf("claimed %d reward(s)", claimed)
	if claimNote != "" {
		note += "; first claim error: " + claimNote
	}
	record("claim_pass", claimErr, note)

	return results, firstErr
}

func (d *Driver) runExpertUse(ctx context.Context, acc *model.Account, e MarketExpert, local bool) bool {
	creds, err := ParseCreds(acc.Credentials())
	if err != nil {
		return false
	}
	if err := d.reportDesktopEvents(ctx, creds, expertSummonSequence(e)); err != nil {
		return false
	}
	convID, reqID, err := d.desktopChatWithExpert(ctx, creds, e.ExpertID)
	if err != nil {
		return false
	}
	return d.reportDesktopEvents(ctx, creds, []desktopEvent{expertActualUseEvent(e, convID, reqID, local)}) == nil
}

func mustCreds(acc *model.Account) *Creds {
	creds, _ := ParseCreds(acc.Credentials())
	if creds == nil {
		creds = &Creds{}
	}
	return creds
}

func errFromBool(ok bool) error {
	if ok {
		return nil
	}
	return uperr.New(uperr.ClassInternal, 0, "step incomplete")
}

func newUUIDv4() string {
	return uuidV4()
}
