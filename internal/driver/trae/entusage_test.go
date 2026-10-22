package trae

import (
	"testing"
	"time"
)

// TRAE-5 red-first tests (BP-98): the entitlement aggregation must read the
// same shapes the upstream actually sends. Translated from
// magpie-community/plugins packages/trae 0.2.19 credits():
//   - credits_limit === -1 is the UNLIMITED pack — not an empty one. The old
//     `if l <= 0 { continue }` registered a full account as Remain=0/Total=0,
//     which pool floor-blocking then kept out of routing (the empty-account
//     mirage).
//   - each pack's end_time / expire_time feeds Expiring / EarliestExpiresAt
//     so the pool can burn soon-to-die credits first (its ×3 weight needs
//     the data; trae never supplied it).

// TestParseEntUsageUnlimitedPack: -1 = unlimited. The pack contributes its
// usage as consumed, the remainder reads as available, and the caller can
// tell it apart from a genuinely empty account.
func TestParseEntUsageUnlimitedPack(t *testing.T) {
	raw := []byte(`{"user_entitlement_pack_list":[
		{"entitlement_base_info":{"quota":{"credits_limit":-1}},"usage":{"credits_amount":42}}
	]}`)
	remain, limit, used, packs, _, unlimited, err := ParseEntUsageFull(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !unlimited {
		t.Fatal("credits_limit=-1 must read as unlimited")
	}
	if packs != 1 {
		t.Fatalf("packs = %d, want 1", packs)
	}
	if used != 42 {
		t.Fatalf("used = %d, want 42", used)
	}
	if remain <= 0 {
		t.Fatalf("an unlimited pack must read as available, remain=%d", remain)
	}
	_ = limit
}

// TestParseEntUsageExpiryFeedsExpiring: the earliest pack end lands in the
// returned time; a response without expiries returns zero time.
func TestParseEntUsageExpiryFeedsExpiring(t *testing.T) {
	// Two packs: ends 2027-01-01 and 2026-11-01 (earlier, must win). Upstream
	// sends seconds or milliseconds; this body uses milliseconds.
	raw := []byte(`{"user_entitlement_pack_list":[
		{"entitlement_base_info":{"quota":{"credits_limit":100},"end_time":1798761600000},"usage":{"credits_amount":10}},
		{"entitlement_base_info":{"quota":{"credits_limit":50},"end_time":1762252800000},"usage":{"credits_amount":0}}
	]}`)
	_, _, _, packs, until, unlimited, err := ParseEntUsageFull(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if unlimited {
		t.Fatal("positive limits are not unlimited")
	}
	if packs != 2 {
		t.Fatalf("packs = %d, want 2", packs)
	}
	want := time.Unix(1762252800, 0)
	if until.IsZero() || !until.Equal(want) {
		t.Fatalf("earliest expiry = %v, want %v", until, want)
	}
}

// TestParseEntUsageZeroLimitPackSkipped: limit=0 (no quota) still does not
// count as a pack — only -1 changed meaning.
func TestParseEntUsageZeroLimitPackSkipped(t *testing.T) {
	raw := []byte(`{"user_entitlement_pack_list":[
		{"entitlement_base_info":{"quota":{"credits_limit":0}},"usage":{"credits_amount":0}},
		{"entitlement_base_info":{"quota":{"credits_limit":100}},"usage":{"credits_amount":25}}
	]}`)
	remain, limit, used, packs, _, _, err := ParseEntUsageFull(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if packs != 1 || limit != 100 || used != 25 || remain != 75 {
		t.Fatalf("got remain=%d limit=%d used=%d packs=%d, want 75/100/25/1", remain, limit, used, packs)
	}
}
