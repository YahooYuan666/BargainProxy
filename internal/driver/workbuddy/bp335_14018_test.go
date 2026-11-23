package workbuddy

import "testing"

func TestBP335_14018DataLayer(t *testing.T) {
	raw := []byte(`{"error":{"data":{"code":14018,"msg":"Credits exhausted. Please visit the link below to purchase add-on packs and get more credits: https://www.codebuddy.ai/price"}}}`)
	code, msg, ok := detectBusinessError(raw)
	if !ok || code != 14018 {
		t.Fatalf("code=%d ok=%v, want 14018/true", code, ok)
	}
	if msg == "" {
		t.Fatal("message must be extracted")
	}
	e := classifyBusiness(code, msg, "deepseek-v4.1-flash")
	if e.Class != "hard_credit" {
		t.Fatalf("class=%s, want hard_credit (14018 is a billing wall, not a rate limit)", e.Class)
	}
}

func TestBP335_TopLevelDataLayer(t *testing.T) {
	raw := []byte(`{"data":{"code":6004,"msg":"您的使用量已超出频率限制"}}`)
	code, _, ok := detectBusinessError(raw)
	if !ok || code != 6004 {
		t.Fatalf("code=%d ok=%v, want 6004/true", code, ok)
	}
}
