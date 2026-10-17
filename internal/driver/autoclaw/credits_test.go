package autoclaw

import (
	"encoding/json"
	"testing"
)

func TestParseLedgerEntriesFlattensMetadata(t *testing.T) {
	raw := `[
	  {"occurred_at":"2026-10-17T17:27:00.85+08:00","amount":-6,"mutation_type":"model_usage",
	   "description":"AutoClaw Auto模型消耗积分",
	   "metadata":{"model":"glm-5.3-flash","actual_model":"zai_glm-5.3-flash","reason":"model_usage",
	     "charged_amount":6,"request_point_cap":50,"token_usage":"input_tokens:26093, output_tokens:58, cache_tokens:5376"}},
	  {"occurred_at":"2026-10-17T18:04:56+08:00","amount":200,"mutation_type":"credit","description":"每日签到"}
	]`
	var rows []map[string]any
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatal(err)
	}
	got := parseLedgerEntries(rows)
	if len(got) != 2 {
		t.Fatalf("entries = %d", len(got))
	}
	e := got[0]
	if e.Amount != "-6" || e.Charged != "6" || e.Cap != "50" {
		t.Fatalf("amount fields = %+v", e)
	}
	if e.Model != "glm-5.3-flash" || e.ActualModel != "zai_glm-5.3-flash" {
		t.Fatalf("model fields = %+v", e)
	}
	if e.Reason != "model_usage" || e.TokenUsage == "" || e.Description == "" {
		t.Fatalf("metadata = %+v", e)
	}
	if got[1].Model != "" || got[1].Amount != "200" || got[1].Reason != "credit" {
		t.Fatalf("recharge row = %+v", got[1])
	}
}

func TestToInt64AnyAcceptsJSONNumberAndString(t *testing.T) {
	if got := toInt64Any(json.Number("32227")); got != 32227 {
		t.Fatalf("json.Number = %d", got)
	}
	if got := toInt64Any("100"); got != 100 {
		t.Fatalf("string = %d", got)
	}
	if got := toInt64Any(float64(7)); got != 7 {
		t.Fatalf("float = %d", got)
	}
}
