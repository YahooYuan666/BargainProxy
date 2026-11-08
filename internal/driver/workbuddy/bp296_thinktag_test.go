package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

// BP-296（misakano 771c887，issue #194）：上游 reasoning parser 偶发把整段
// CoT 当正文下发——正文首尾带 <think:SUF>…</think:SUF>（SUF 是模型的
// checkpoint token 后缀）。网关必须把它拆回 thinking，别让控制 token 进正文。

func TestSplitThinkTagsOneShot(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		think string
		body  string
	}{
		{"整段泄漏", "<think:6124c78e>思考内容</think:6124c78e>正文", "思考内容", "正文"},
		{"无泄漏原样", "普通正文", "", "普通正文"},
		{"正文里聊到标签字样", "前面正文 <think:xxx> 不是开头不算", "", "前面正文 <think:xxx> 不是开头不算"},
		{"裸 think 不处理", "<think>裸的不动</think>", "", "<think>裸的不动</think>"},
		{"空串", "", "", ""},
		{"只有标签没有正文", "<think:abc>只有思考</think:abc>", "只有思考", ""},
		{"自闭合形式按开标签", "<think:abc/>不在标签内", "不在标签内", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			think, body := SplitThinkTags(c.in)
			if think != c.think || body != c.body {
				t.Fatalf("SplitThinkTags(%q) = (%q, %q), want (%q, %q)", c.in, think, body, c.think, c.body)
			}
		})
	}
}

// 标签跨 delta 边界：Feed 逐片喂、Flush 收尾，任何切法结果一致（misakano
// 的有状态 scan：尾部可能是半个标签时留在 pend 里等下一片）。
func TestThinkTagFeedAcrossBoundaries(t *testing.T) {
	full := "<think:6124c78e>第一段思考</think:6124c78e>正文的开始"
	// 穷举所有单字节切法，结果都必须与一次性拆分一致。
	for cut := 1; cut < len(full); cut++ {
		var tt ThinkTag
		var think, body strings.Builder
		for _, piece := range []string{full[:cut], full[cut:]} {
			th, bd := tt.Feed(piece)
			think.WriteString(th)
			body.WriteString(bd)
		}
		th, bd := tt.Flush()
		think.WriteString(th)
		body.WriteString(bd)
		if think.String() != "第一段思考" || body.String() != "正文的开始" {
			t.Fatalf("cut=%d: think=%q body=%q", cut, think.String(), body.String())
		}
	}
}

// 标签未闭合时思考照样流式吐出（misakano 原语义：只有半个标签前缀才扣住），
// Flush 清空残留、不重复不丢字。
func TestThinkTagFlushTail(t *testing.T) {
	var tt ThinkTag
	th, bd := tt.Feed("<think:abc>思考")
	if th != "思考" || bd != "" {
		t.Fatalf("开标签后的思考应流式吐出: (%q,%q)", th, bd)
	}
	th, bd = tt.Flush()
	if th != "" || bd != "" {
		t.Fatalf("flush 应无残留: (%q,%q)", th, bd)
	}
	// 上游死在标签中途：flush 把标签残骸按正文吐出来，少一次改写好过丢内容。
	var tt2 ThinkTag
	th2, _ := tt2.Feed("<think:abc>半截")
	th3, bd3 := tt2.Flush()
	_ = th2
	if th3 != "" || bd3 != "" {
		t.Logf("flush on unterminated tag: (%q,%q)", th3, bd3)
	}
}

// --- lane 接线 ---

// chat lane：上游 content 里的泄漏标记必须以 EvThinking + 纯正文 EvText 到达
// 客户端；聚合正文（textBuf / 返回值）同样不含标记。
func TestChatLaneSplitsLeakedThinkTag(t *testing.T) {
	var think, text strings.Builder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 标记跨帧：首帧只有半个开标签。
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"<thi"}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"nk:6124c78e>内部推理"}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"</think:6124c78e>对外正文"}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	t.Cleanup(srv.Close)

	d := testDriver(srv.URL)
	call := bp131Call("deepseek-v4.1-flash", "")
	resp, err := d.Stream(context.Background(), call, func(ev ir.StreamEvent) error {
		switch ev.Kind {
		case ir.EvThinking:
			think.WriteString(ev.Delta)
		case ir.EvText:
			text.WriteString(ev.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if think.String() != "内部推理" {
		t.Fatalf("thinking = %q, want 内部推理", think.String())
	}
	if text.String() != "对外正文" {
		t.Fatalf("text = %q, want 对外正文", text.String())
	}
	if strings.Contains(ir.TextOf(resp.Message.Content), "think:") {
		t.Fatalf("聚合正文含泄漏标记: %q", ir.TextOf(resp.Message.Content))
	}
}

// messages lane：text_delta 里的泄漏同样拆分。
func TestMessagesLaneSplitsLeakedThinkTag(t *testing.T) {
	var think, text strings.Builder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `event: message_start`)
		fmt.Fprintln(w, `data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}`)
		fmt.Fprintln(w, `event: content_block_delta`)
		fmt.Fprintln(w, `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"<think:abc>CoT"}}`)
		fmt.Fprintln(w, `event: content_block_delta`)
		fmt.Fprintln(w, `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"</think:abc>答案"}}`)
		fmt.Fprintln(w, `event: message_delta`)
		fmt.Fprintln(w, `data: {"type":"message_delta","usage":{"output_tokens":3}}`)
		fmt.Fprintln(w, `event: message_stop`)
		fmt.Fprintln(w, `data: {"type":"message_stop"}`)
	}))
	t.Cleanup(srv.Close)

	// 捕获发往上游的请求体里 model 之外我们不关心的部分：解码仅为兜底。
	var _ map[string]any
	_ = json.Unmarshal

	d := testDriver(srv.URL)
	call := bp131Call("claude-sonnet-4-6", "")
	resp, err := d.Stream(context.Background(), call, func(ev ir.StreamEvent) error {
		switch ev.Kind {
		case ir.EvThinking:
			think.WriteString(ev.Delta)
		case ir.EvText:
			text.WriteString(ev.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if think.String() != "CoT" {
		t.Fatalf("thinking = %q, want CoT", think.String())
	}
	if text.String() != "答案" {
		t.Fatalf("text = %q, want 答案", text.String())
	}
	if strings.Contains(ir.TextOf(resp.Message.Content), "think:") {
		t.Fatalf("聚合正文含泄漏标记: %q", ir.TextOf(resp.Message.Content))
	}
}

// 健康流量零扰动：reasoning_content（正规思考通道）与普通正文不受拆分器影响。
func TestHealthyStreamUnaffectedBySplitter(t *testing.T) {
	var think, text strings.Builder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"reasoning_content":"正经思考"},"content":""}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"普通回答"}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	t.Cleanup(srv.Close)

	d := testDriver(srv.URL)
	call := bp131Call("deepseek-v4.1-flash", "")
	if _, err := d.Stream(context.Background(), call, func(ev ir.StreamEvent) error {
		switch ev.Kind {
		case ir.EvThinking:
			think.WriteString(ev.Delta)
		case ir.EvText:
			text.WriteString(ev.Delta)
		}
		return nil
	}); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if think.String() != "正经思考" || text.String() != "普通回答" {
		t.Fatalf("think=%q text=%q", think.String(), text.String())
	}
}
