package qoder

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// BP-304（plugins qoder 0.2.64 #880，magpie#5726）：每日积分用尽后 Qoder 把
// 账号 listing 里的模型下架，chat 请求回 `400 unknown or disabled model
// "key"`——真相是当日额度耗尽，次日 10:00 放份额自愈，不是坏请求。改判 429
// 必须三条件齐备：错误命中该形状 + 实时 listing 确认该 key enable=false +
// usage 确认无可花积分。key 不在册 / 积分有余 / usage 读不到 → 维持 400。

// TestBP304CatalogRecordsOffKeys listing 里 enable=false 的 key 必须进 Off 集
// （而不是被静默丢弃），enable=true 与缺席 key 不得进。
func TestBP304CatalogRecordsOffKeys(t *testing.T) {
	body := map[string]interface{}{
		"app": []interface{}{
			map[string]interface{}{"key": "dfmodel", "display_name": "DF", "enable": false},
			map[string]interface{}{"key": "qfmodel", "display_name": "QF", "enable": true},
		},
		"chat": []interface{}{
			map[string]interface{}{"key": "dfmodel", "display_name": "DF", "enable": false},
		},
	}
	cat := extractCatalog(body)
	if cat == nil {
		t.Fatal("catalog nil")
	}
	if !cat.Off["dfmodel"] {
		t.Fatal("dfmodel must be recorded as off")
	}
	if cat.Off["qfmodel"] {
		t.Fatal("enabled key must not be in Off")
	}
	if cat.Off["no-such-model"] {
		t.Fatal("unlisted key must not be in Off")
	}
	if _, served := cat.Caps["dfmodel"]; served {
		t.Fatal("disabled key must not be served as a capability")
	}
}

func bp304Err() *uperr.Error {
	e := uperr.New(uperr.ClassBadRequest, 400, `qoder: 400 unknown or disabled model "dfmodel"`)
	return e
}

func bp304Credits(remain int64) *model.Credits {
	return &model.Credits{Remain: remain, Total: 800}
}

// TestBP304RecastFires 三条件齐备 → ClassHardCredit 429、ResetAt=下一个 10:00、
// code=credits_off。
func TestBP304RecastFires(t *testing.T) {
	got := recastCreditsOff(bp304Err(), "dfmodel", true, bp304Credits(0))
	if got.Class != uperr.ClassHardCredit || got.Status != 429 {
		t.Fatalf("class=%s status=%d, want hard_credit 429", got.Class, got.Status)
	}
	if got.Code != "credits_off" {
		t.Fatalf("code=%q", got.Code)
	}
	want := time.Date(time.Now().Year(), time.Now().Month(), time.Now().Day(), 10, 0, 0, 0, time.Now().Location())
	if !want.After(time.Now()) {
		want = want.AddDate(0, 0, 1)
	}
	if got.ResetAt.IsZero() || got.ResetAt.Sub(want) > time.Minute || want.Sub(got.ResetAt) > time.Minute {
		t.Fatalf("ResetAt=%v, want next local 10:00 (%v)", got.ResetAt, want)
	}
	// Bonus/Sota 池有余也不算耗尽。
	if got2 := recastCreditsOff(bp304Err(), "dfmodel", true, &model.Credits{Remain: 0, Bonus: 5}); got2 == got || got2.Class == uperr.ClassHardCredit {
		t.Fatal("bonus credits left must keep the 400")
	}
	if got3 := recastCreditsOff(bp304Err(), "dfmodel", true, &model.Credits{Remain: 0, Sota: 5}); got3.Class == uperr.ClassHardCredit {
		t.Fatal("sota credits left must keep the 400")
	}
}

// TestBP304RecastGuards 负例矩阵：key 不在册 / 积分有余 / usage 读不到 /
// 错误形状不对 / 非 bad_request，全部维持原 400。
func TestBP304RecastGuards(t *testing.T) {
	cases := []struct {
		name string
		ue   *uperr.Error
		key  string
		off  bool
		cr   *model.Credits
	}{
		{"key the listing never served", bp304Err(), "dfmodel", false, bp304Credits(0)},
		{"credits left", bp304Err(), "dfmodel", true, bp304Credits(200)},
		{"usage unreadable", bp304Err(), "dfmodel", true, nil},
		{"wrong error shape (no key extracted)", bp304Err(), "", true, bp304Credits(0)},
	}
	for _, c := range cases {
		got := recastCreditsOff(c.ue, c.key, c.off, c.cr)
		if got != c.ue {
			t.Fatalf("%s: must keep the original error, got class=%s status=%d", c.name, got.Class, got.Status)
		}
	}
}

// TestBP304NextLocal10 边界：09:59 → 今天 10:00；10:00 整 → 明天 10:00。
func TestBP304NextLocal10(t *testing.T) {
	morning := time.Date(2026, 11, 9, 9, 59, 0, 0, time.Local)
	if got := nextLocal10(morning); got.Day() != 9 || got.Hour() != 10 {
		t.Fatalf("09:59 → %v, want today 10:00", got)
	}
	at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.Local)
	if got := nextLocal10(at); got.Day() != 10 || got.Hour() != 10 {
		t.Fatalf("10:00 → %v, want tomorrow 10:00", got)
	}
}

// TestBP304DisabledModelRegex 正则捕获 key（含引号内任意字符）。
func TestBP304DisabledModelRegex(t *testing.T) {
	m := disabledModelRe.FindStringSubmatch(`400 unknown or disabled model "dfmodel_x.1"`)
	if m == nil || m[1] != "dfmodel_x.1" {
		t.Fatalf("regex capture = %v", m)
	}
}

// TestBP304CatalogJSONRoundTrip 防 extractCatalog 的 Off 在 JSON 序列化链路
// （错误重判前走 fetchModelCatalog → extractCatalog）里丢字段。
func TestBP304CatalogJSONRoundTrip(t *testing.T) {
	raw := []byte(`{"app":[{"key":"dfmodel","display_name":"DF","enable":false},{"key":"qfmodel","display_name":"QF","enable":true}],"chat":[]}`)
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	cat := extractCatalog(body)
	if cat == nil || !cat.Off["dfmodel"] {
		t.Fatalf("Off lost through the JSON path: %+v", cat)
	}
	if !strings.Contains("dfmodel", "dfmodel") {
		t.Fatal("unreachable")
	}
}
