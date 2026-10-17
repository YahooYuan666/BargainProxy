// BP-49: state blobs written before the ModelCooldown struct change carry bare
// timestamps ("model": "2026-10-16T22:16:56+08:00") — they must keep loading,
// and the new object form must round-trip with its reason/message.
package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestModelCooldownUnmarshalLegacyTimestamp(t *testing.T) {
	var m map[string]ModelCooldown
	raw := []byte(`{"deepseek-v4.1-flash":"2026-10-16T22:16:56+08:00"}`)
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("legacy state must keep loading: %v", err)
	}
	mc := m["deepseek-v4.1-flash"]
	want, _ := time.Parse(time.RFC3339, "2026-10-16T22:16:56+08:00")
	if !mc.Until.Equal(want) || !mc.ResetAt.IsZero() || mc.Reason != "" {
		t.Fatalf("legacy parse = %+v", mc)
	}

	round, err := json.Marshal(map[string]ModelCooldown{
		"m": {Until: want, ResetAt: want, Reason: "6004 model rate limit", Message: "上游原文"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]ModelCooldown
	if err := json.Unmarshal(round, &back); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if !back["m"].ResetAt.Equal(want) || back["m"].Reason != "6004 model rate limit" || back["m"].Message != "上游原文" {
		t.Fatalf("round trip = %+v", back["m"])
	}
}
