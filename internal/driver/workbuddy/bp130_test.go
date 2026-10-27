package workbuddy

import (
	"encoding/json"
	"testing"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
)

// BP-130: the international 429 wording carries an explicit UTC offset
// (Coding2API 9ba8b80); the old scanner only knew the bare CN wall clock, so
// every intl reset fell back to the constant backoff and the account kept
// re-hitting the wall until the real reset.
func TestParseResetWallClockUTCOffsets(t *testing.T) {
	cn := parseResetWallClock("模型调用频率超限，将在 2026-10-14 15:30:00 重置")
	if cn.IsZero() || cn.UTC().Format("15:04") != "07:30" {
		t.Fatalf("CN bare clock = %v, want 15:30 +08:00", cn)
	}
	intl := parseResetWallClock("usage will reset at 2026-10-25 22:34:25 UTC+8")
	if intl.IsZero() || intl.UTC().Format("2006-01-02 15:04") != "2026-10-25 14:34" {
		t.Fatalf("UTC+8 = %v, want 2026-10-25 14:34 UTC", intl)
	}
	neg := parseResetWallClock("resets at 2026-10-25 04:34:25 UTC-5")
	if neg.IsZero() || neg.UTC().Format("2006-01-02 15:04") != "2026-10-25 09:34" {
		t.Fatalf("UTC-5 = %v, want 2026-10-25 09:34 UTC", neg)
	}
	half := parseResetWallClock("resets at 2026-10-25 04:34:25 UTC+5:30")
	if half.IsZero() || half.UTC().Format("15:04") != "23:04" {
		t.Fatalf("UTC+5:30 = %v, want 23:04 UTC", half)
	}
}

// BP-130: reasoning_effort=off/none must not ride as a gear (upstream
// downgrades it to the model default and DeepSeek keeps thinking); for
// DeepSeek the upstream's own off switch is thinking.type=disabled.
func wbCall(mdl, effort string) driver.ChatCall {
	return driver.ChatCall{
		Account:       &model.Account{ID: "wb-t", Channel: model.ChannelWorkBuddy},
		UpstreamModel: mdl,
		Request: &ir.ChatRequest{
			Model: mdl, RequestedEffort: effort,
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}
}

func testCreds() *Creds {
	raw, _ := json.Marshal(map[string]any{"access_token": "tok", "refresh_token": "r", "realm": "cn"})
	c, _ := ParseCreds(raw)
	return c
}

func bodyMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("body: %v", err)
	}
	return doc
}

func TestOffEffortDisablesThinking(t *testing.T) {
	d := testDriver("http://unused")
	call := wbCall("deepseek-v4.1-flash", "off")
	body, err := d.buildBody(call, testCreds(), false)
	if err != nil {
		t.Fatal(err)
	}
	doc := bodyMap(t, body)
	if _, has := doc["reasoning_effort"]; has {
		t.Fatalf("reasoning_effort must be absent for off, got %v", doc["reasoning_effort"])
	}
	th, ok := doc["thinking"].(map[string]any)
	if !ok || th["type"] != "disabled" {
		t.Fatalf("deepseek thinking = %v, want {type: disabled}", doc["thinking"])
	}
	// GLM has no observed disabled switch: drop the gear, add nothing.
	body2, err := d.buildBody(wbCall("glm-5.3", "none"), testCreds(), false)
	if err != nil {
		t.Fatal(err)
	}
	doc2 := bodyMap(t, body2)
	if _, has := doc2["reasoning_effort"]; has {
		t.Fatal("glm none must also drop reasoning_effort")
	}
	if _, has := doc2["thinking"]; has {
		t.Fatal("glm none must not fabricate a thinking block")
	}
	// A real gear still rides for global pins and normal values.
	body3, err := d.buildBody(wbCall("glm-5.3", "high"), testCreds(), false)
	if err != nil {
		t.Fatal(err)
	}
	if doc3 := bodyMap(t, body3); doc3["reasoning_effort"] != "high" {
		t.Fatalf("normal gear lost: %v", doc3["reasoning_effort"])
	}
	_ = time.Now
}
