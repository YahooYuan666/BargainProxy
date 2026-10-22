package panel

import (
	"encoding/json"
	"testing"
)

// TestParseWBAccountShapes pins both shapes the device-flow account endpoint
// returns. The flat shape is the CN edge; the accounts-list shape is the
// international edge (field capture 2026-10-22, uid c71ff2d7…): the flat-only
// decoder read nothing there, so the panel polled forever.
func TestParseWBAccountShapes(t *testing.T) {
	flat := json.RawMessage(`{"uid":"u-flat","nickname":"国内号","enterpriseId":"ent-1"}`)
	if got := parseWBAccount(flat); got.UID != "u-flat" || got.Nickname != "国内号" || got.EnterpriseID != "ent-1" {
		t.Fatalf("flat = %+v", got)
	}

	list := json.RawMessage(`{"accounts":[
		{"uid":"u-old","nickname":"旧号","lastLogin":false},
		{"uid":"c71ff2d7-9d95-41c6-a0b6-3351eeb56343","nickname":"y200901152022@gmail.com","lastLogin":true,"accountType":""}
	]}`)
	got := parseWBAccount(list)
	if got.UID != "c71ff2d7-9d95-41c6-a0b6-3351eeb56343" || got.Nickname != "y200901152022@gmail.com" {
		t.Fatalf("list must pick the lastLogin account, got %+v", got)
	}

	only := json.RawMessage(`{"accounts":[{"uid":"u-only","nickname":"唯一号"}]}`)
	if got := parseWBAccount(only); got.UID != "u-only" {
		t.Fatalf("single-entry list = %+v", got)
	}
	if got := parseWBAccount(json.RawMessage(`{}`)); got.UID != "" {
		t.Fatalf("empty = %+v", got)
	}
}
