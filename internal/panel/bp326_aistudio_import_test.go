package panel

import (
	"encoding/json"
	"testing"

	"bargainproxy/internal/model"
)

// TestBP326AistudioImport pins the aistudio import contract: the API key is
// the stable identity (hashed into the ID — re-import updates, never
// duplicates), the raw key never lands in the ID, and an empty key is
// rejected.
func TestBP326AistudioImport(t *testing.T) {
	acc, err := normalizeImport(model.ChannelAIStudio, json.RawMessage(`{"api_key":"AQ.test-key"}`), "号1")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if acc.Channel != model.ChannelAIStudio {
		t.Fatalf("channel = %s", acc.Channel)
	}
	if len(acc.ID) < 10 || acc.ID[:9] != "aistudio:" {
		t.Fatalf("id = %s (want aistudio:<hash>)", acc.ID)
	}
	if acc.ID == "aistudio:AQ.test-key" {
		t.Fatalf("raw key leaked into the account ID")
	}
	if acc.Nickname != "号1" {
		t.Fatalf("nickname = %q", acc.Nickname)
	}
	var creds map[string]string
	if err := json.Unmarshal(acc.CredsJSON, &creds); err != nil {
		t.Fatalf("creds: %v", err)
	}
	if creds["api_key"] != "AQ.test-key" {
		t.Fatalf("api_key round-trip = %q", creds["api_key"])
	}

	// Re-import the same key → same ID (upsert, not duplicate).
	again, err := normalizeImport(model.ChannelAIStudio, json.RawMessage(`{"api_key":"AQ.test-key"}`), "号1-改名")
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if again.ID != acc.ID {
		t.Fatalf("re-import re-keyed the account: %s vs %s", again.ID, acc.ID)
	}

	// Empty / whitespace key → rejected.
	for _, bad := range []string{`{"api_key":""}`, `{"api_key":"   "}`, `{}`, `[]`} {
		if _, err := normalizeImport(model.ChannelAIStudio, json.RawMessage(bad), ""); err == nil {
			t.Fatalf("bad creds accepted: %s", bad)
		}
	}
}
