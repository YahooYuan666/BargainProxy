package panel

import (
	"encoding/json"
	"testing"
)

// TestParseWBAccountShapes pins both shapes the device-flow account endpoint
// returns. The flat shape is the CN edge; the accounts-list shape is the
// international edge (field capture 2026-10-22): the flat-only decoder read
// nothing there, so the panel polled forever.
//
// Identity fields are placeholders: this repository is public, and the shape
// is the contract, not the account it was captured from.
func TestParseWBAccountShapes(t *testing.T) {
	flat := json.RawMessage(`{"uid":"u-flat","nickname":"国内号","enterpriseId":"ent-1"}`)
	if got := parseWBAccount(flat); got.UID != "u-flat" || got.Nickname != "国内号" || got.EnterpriseID != "ent-1" {
		t.Fatalf("flat = %+v", got)
	}

	list := json.RawMessage(`{"accounts":[
		{"uid":"u-old","nickname":"旧号","lastLogin":false},
		{"uid":"00000000-0000-4000-8000-000000000000","nickname":"user@example.com","lastLogin":true,"accountType":""}
	]}`)
	got := parseWBAccount(list)
	if got.UID != "00000000-0000-4000-8000-000000000000" || got.Nickname != "user@example.com" {
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
