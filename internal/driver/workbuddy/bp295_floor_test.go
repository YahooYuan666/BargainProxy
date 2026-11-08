package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"bargainproxy/internal/ir"
)

// BP-295（misakano 2b9b773/34a3ae6）：本上游把思考与正文记在同一份 max_tokens
// 里——effort=high + max_tokens=2048 时思考吃掉 2048、正文断半句。客户端显式
// 给的预算低于档位下限时抬到下限；抬的是上限不是目标长度（模型写完自然停）。

func chatLaneBody(t *testing.T, upstreamModel, gear string, maxTokens int, thinking *ir.ThinkingConfig) map[string]any {
	t.Helper()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	t.Cleanup(srv.Close)

	d := testDriver(srv.URL)
	call := bp131Call(upstreamModel, gear)
	call.Request.MaxTokens = maxTokens
	call.Request.Thinking = thinking
	if _, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}
	return body
}

func TestChatLaneRaisesBudgetToEffortFloor(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		gear      string
		maxTokens int
		thinking  *ir.ThinkingConfig
		want      int
	}{
		{"deepseek 无档默认 high（BP-177）", "deepseek-v4.1-flash", "", 2048, nil, 8192},
		{"deepseek max 档", "deepseek-v4.1-flash", "max", 4096, nil, 16384},
		{"deepseek xhigh 档", "deepseek-v4.1-flash", "xhigh", 2048, nil, 12288},
		{"glm high 档", "glm-5.3", "high", 2048, nil, 8192},
		{"已达标不动", "deepseek-v4.1-flash", "high", 8192, nil, 8192},
		{"超出下限不动", "deepseek-v4.1-flash", "high", 65536, nil, 65536},
		{"探活级不动", "deepseek-v4.1-flash", "max", 1, nil, 1},
		{"off 档（思考关）不动", "deepseek-v4.1-flash", "off", 2048, nil, 2048},
		{"客户端显式关思考不动", "deepseek-v4.1-flash", "high", 2048, &ir.ThinkingConfig{Enabled: false}, 2048},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := chatLaneBody(t, c.model, c.gear, c.maxTokens, c.thinking)
			got, _ := body["max_tokens"].(float64)
			if int(got) != c.want {
				t.Fatalf("max_tokens = %v, want %d", body["max_tokens"], c.want)
			}
		})
	}
}

// GLM 不带档位时不发 reasoning_effort（也就没有思考要贴钱），预算一个字节不动。
func TestChatLaneGLMWithoutGearKeepsBudget(t *testing.T) {
	body := chatLaneBody(t, "glm-5.3", "", 2048, nil)
	if got, _ := body["max_tokens"].(float64); int(got) != 2048 {
		t.Fatalf("max_tokens = %v, want 2048", body["max_tokens"])
	}
	if _, has := body["reasoning_effort"]; has {
		t.Fatalf("无档位不应发 reasoning_effort: %v", body["reasoning_effort"])
	}
}

// /v1/messages lane：effort=max + 显式小额 → 抬；注入的兜底预算（无显式额度）
// 同样受下限约束——这条 lane 的 max_tokens 是必填字段，注入 8192 也装不下 max 档。
func TestMessagesLaneRaisesBudgetToEffortFloor(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		gear      string
		maxTokens int
		want      int
	}{
		{"max 档显式 4096", "claude-sonnet-4-6", "max", 4096, 16384},
		{"high 档显式 2048", "claude-sonnet-4-6", "high", 2048, 8192},
		// 目录外的 claude 名走 8192 兜底注入——兜底预算同样受下限约束。
		{"max 档注入兜底 8192", "claude-haiku-4-5", "max", 0, 16384},
		{"无档位不动", "claude-sonnet-4-6", "", 4096, 4096},
		{"已达标不动", "claude-sonnet-4-6", "max", 32768, 32768},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body = map[string]any{}
				_ = json.NewDecoder(r.Body).Decode(&body)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintln(w, `event: message_start`)
				fmt.Fprintln(w, `data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}`)
				fmt.Fprintln(w, `event: content_block_delta`)
				fmt.Fprintln(w, `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`)
				fmt.Fprintln(w, `event: message_delta`)
				fmt.Fprintln(w, `data: {"type":"message_delta","usage":{"output_tokens":3}}`)
				fmt.Fprintln(w, `event: message_stop`)
				fmt.Fprintln(w, `data: {"type":"message_stop"}`)
			}))
			t.Cleanup(srv.Close)

			d := testDriver(srv.URL)
			call := bp131Call(c.model, c.gear)
			call.Request.MaxTokens = c.maxTokens
			if _, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil }); err != nil {
				t.Fatalf("stream: %v", err)
			}
			got, _ := body["max_tokens"].(float64)
			if int(got) != c.want {
				t.Fatalf("max_tokens = %v, want %d", body["max_tokens"], c.want)
			}
		})
	}
}
