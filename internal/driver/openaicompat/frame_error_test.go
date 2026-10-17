// ticket 36: mid-stream error frames on a 200 stream must classify, not pass
// as success.
package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

func frameStream(t *testing.T, frames ...string) (*Driver, *model.Account) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprintln(w, f)
		}
	}))
	t.Cleanup(up.Close)
	d := New(config.OpenAICompatSettings{Enabled: true, BaseURL: up.URL + "/v1", APIKey: "k"}, nil)
	creds, _ := json.Marshal(map[string]string{"api_key": "k"})
	return d, &model.Account{ID: "compat-1", Channel: model.ChannelOpenAICompat, CredsJSON: creds}
}

func streamErr(t *testing.T, d *Driver, acc *model.Account) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := d.Stream(ctx, driver.ChatCall{
		Account: acc, UpstreamModel: "glm-5.3",
		Request: &ir.ChatRequest{Model: "glm-5.3", Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}, func(ev ir.StreamEvent) error { return nil })
	return err
}

// TestErrorFrameClassified 200 流中一帧 error（rate limit）→ SoftRate 错误，
// 不再被当成功（此前只查 HTTP 状态，帧内错误全被忽略）。
func TestErrorFrameClassified(t *testing.T) {
	d, acc := frameStream(t,
		`data: {"choices":[{"delta":{"content":"par"}}]}`,
		`data: {"error":{"message":"Rate limit reached for requests","type":"rate_limit_exceeded"}}`,
		`data: [DONE]`,
	)
	err := streamErr(t, d, acc)
	if err == nil {
		t.Fatal("error frame must not pass as success")
	}
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassSoftRate {
		t.Fatalf("class = %v (%v), want soft_rate", ue, err)
	}
}

// TestInsufficientQuotaFrame 配额类 error 帧 → HardCredit。
func TestInsufficientQuotaFrame(t *testing.T) {
	d, acc := frameStream(t,
		`data: {"error":{"message":"You exceeded your current quota","type":"insufficient_quota"}}`,
	)
	err := streamErr(t, d, acc)
	ue, ok := uperr.As(err)
	if err == nil || !ok || ue.Class != uperr.ClassHardCredit {
		t.Fatalf("class = %v (%v), want hard_credit", ue, err)
	}
}

// TestNormalStreamUnaffected 正常流不受新检测影响。
func TestNormalStreamUnaffected(t *testing.T) {
	d, acc := frameStream(t,
		`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	)
	if err := streamErr(t, d, acc); err != nil {
		t.Fatalf("normal stream: %v", err)
	}
}
