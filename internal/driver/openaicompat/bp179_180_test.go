package openaicompat

import (
	"bytes"
	"encoding/json"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

func testAccount() *model.Account {
	creds := []byte(`{"api_key":"k"}`)
	return &model.Account{ID: "compat-1", Channel: model.ChannelOpenAICompat, CredsJSON: creds}
}

func contains(raw []byte, needle string) bool {
	return bytes.Contains(raw, []byte(needle))
}

var _ = json.Marshal

// BP-179: a content list made only of text parts goes out as the texts joined
// by a newline — strict backends 422 a text-part array.
func TestBP179AllTextPartsJoinToString(t *testing.T) {
	call := driver.ChatCall{
		Account:       testAccount(),
		UpstreamModel: "m",
		Request: &ir.ChatRequest{
			Model: "m",
			Messages: []ir.Message{{
				Role: ir.RoleUser,
				Content: []ir.ContentPart{
					{Kind: "text", Text: "first"},
					{Kind: "text", Text: "second"},
				},
			}},
		},
	}
	raw, err := buildBody(call)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(raw, `"content":"first\nsecond"`) {
		t.Fatalf("content not joined: %s", raw)
	}
}

// BP-180: the bare "[DONE]" line (no "data:" prefix) terminates the stream.
func TestBP180BareDoneTerminates(t *testing.T) {
	usage := bp161Stream(t,
		`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	)
	if usage.OutputTokens != 0 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}
