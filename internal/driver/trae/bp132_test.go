package trae

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
)

// BP-132: the client's gear must reach Trae as its own levels
// light/high/extra_high (magpie trae 0.2.20) — before, reasoning_effort was
// never sent and every model ran at its default.
func TestTraeEffortMapping(t *testing.T) {
	cases := map[string]string{
		"minimal": "light", "low": "light", "medium": "high", "high": "high",
		"xhigh": "extra_high", "max": "extra_high", "off": "", "none": "", "": "",
	}
	for in, want := range cases {
		if got := traeEffort(in); got != want {
			t.Fatalf("traeEffort(%q) = %q, want %q", in, got, want)
		}
	}
	call := driver.ChatCall{
		UpstreamModel: "deepseek-v4.1-flash",
		Request: &ir.ChatRequest{
			Model: "deepseek-v4.1-flash", RequestedEffort: "xhigh",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}
	raw, err := buildSOLOBody(call, "solo_work_lite")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["reasoning_effort"] != "extra_high" {
		t.Fatalf("reasoning_effort = %v, want extra_high", doc["reasoning_effort"])
	}
	call.Request.RequestedEffort = "off"
	raw2, _ := buildSOLOBody(call, "solo_work_lite")
	var doc2 map[string]any
	_ = json.Unmarshal(raw2, &doc2)
	if _, has := doc2["reasoning_effort"]; has {
		t.Fatal("off must send no reasoning_effort")
	}
}

// The entitlement answer also carries the account's concurrency allowance and
// pay identity (Trae2api-cn bb7b659).
func TestParseTraeEntitlementMeta(t *testing.T) {
	raw := []byte(`{"user_entitlement_pack_list":[
		{"entitlement_base_info":{"quota":{"credits_limit":100,"solo_agent_parallel_limit":2,"user_pay_identity":"free"}}},
		{"entitlement_base_info":{"quota":{"credits_limit":-1,"solo_agent_parallel_limit":5,"user_pay_identity":"paid"}}}]}`)
	pl, pay := ParseTraeEntitlementMeta(raw)
	if pl != 5 {
		t.Fatalf("parallel limit = %d, want 5 (max across packs)", pl)
	}
	if pay != "free" {
		t.Fatalf("pay identity = %q, want the first pack's", pay)
	}
	if pl2, pay2 := ParseTraeEntitlementMeta([]byte(`{}`)); pl2 != 0 || pay2 != "" {
		t.Fatalf("empty answer = %d/%q, want zeros", pl2, pay2)
	}
}
