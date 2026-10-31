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

// BP-222: finish_reason "error" on a 200 stream is the vendor's word for a
// failed turn (magpie 86fef947 #2471). Through this generic driver it used to
// land in the default arm of mapFinish as StopEnd — a half answer reported as
// a success. It must fail the attempt as ClassTransport.
func TestBP222FinishReasonErrorFailsTheTurn(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"par"}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"error"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer up.Close()

	d := New(config.OpenAICompatSettings{Enabled: true, BaseURL: up.URL + "/v1", APIKey: "k"}, nil)
	creds, _ := json.Marshal(map[string]string{"api_key": "k"})
	acc := &model.Account{ID: "compat-1", Channel: model.ChannelOpenAICompat, CredsJSON: creds}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := d.Stream(ctx, driver.ChatCall{
		Account: acc, UpstreamModel: "deepseek-v4.1-flash",
		Request: &ir.ChatRequest{Model: "deepseek-v4.1-flash", Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}, func(ev ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatalf("finish_reason \"error\" must fail the attempt, got a success with stop=%q", resp.StopReason)
	}
	ue, ok := err.(*uperr.Error)
	if !ok {
		t.Fatalf("want *uperr.Error, got %T: %v", err, err)
	}
	if ue.Class != uperr.ClassTransport {
		t.Fatalf("class = %s, want ClassTransport", ue.Class)
	}
}
