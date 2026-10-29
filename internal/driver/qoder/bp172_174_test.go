package qoder

import (
	"testing"
	"time"
)

// BP-172: an earlier assistant turn's thinking rides the wire as
// reasoning_content, exactly where Qoder's own converter puts it
// (plugins 0.2.42, ae48bf0).
func TestBP172ReasoningContentRidesOnAssistant(t *testing.T) {
	msg := map[string]interface{}{
		"role":              "assistant",
		"content":           "",
		"reasoning_content": "I should read the file first.",
		"tool_calls": []interface{}{
			map[string]interface{}{
				"id":   "call_1",
				"type": "function",
				"function": map[string]interface{}{
					"name":      "read_file",
					"arguments": "{}",
				},
			},
		},
	}
	out := convertIncomingMessage(msg, true, true)
	if out == nil {
		t.Fatal("message dropped")
	}
	if out.ReasoningContent != "I should read the file first." {
		t.Fatalf("reasoning = %q", out.ReasoningContent)
	}
	// The `reasoning` spelling is the fallback.
	delete(msg, "reasoning_content")
	msg["reasoning"] = "from reasoning field"
	out2 := convertIncomingMessage(msg, true, true)
	if out2 == nil || out2.ReasoningContent != "from reasoning field" {
		t.Fatalf("reasoning fallback = %+v", out2)
	}
	// User turns never carry it.
	um := convertIncomingMessage(map[string]interface{}{"role": "user", "content": "hi", "reasoning_content": "leak"}, true, true)
	if um == nil || um.ReasoningContent != "" {
		t.Fatalf("user turn must not carry reasoning: %+v", um)
	}
}

// BP-174: a CLAIMED campaign that STARTED before today's 10:00 UTC+8 refresh
// is yesterday's share — the day must stay claimable (retryable), or the
// pre-10:00 slot settles it and today's credits are lost.
func TestBP174YesterdayCampaignDoesNotSettle(t *testing.T) {
	// 16:00 UTC+8 — past today's 10:00 refresh, so today's window is live.
	loc := time.FixedZone("UTC+8", 8*3600)
	now := time.Date(2026, 10, 29, 16, 0, 0, 0, loc)
	todayRefresh := qoderDailyRefreshUnix(now)
	yesterdayStart := todayRefresh - 26*3600 // yesterday's 08:00 UTC+8 window
	todayStart := todayRefresh               // today's 10:00 window

	end := todayRefresh + 24*3600 - 60 // ends 09:59 next day

	// Yesterday's window, already claimed: must NOT settle the day.
	claimedYesterday := []interface{}{
		map[string]interface{}{
			"actionType":  "CLAIM_BENEFIT",
			"id":          "camp-1",
			"claimStatus": "CLAIMED",
			"startAt":     float64(yesterdayStart),
			"endAt":       float64(end),
		},
	}
	if got := campaignsSettle(claimedYesterday, now); got {
		t.Fatal("yesterday's claimed share must not settle today's check-in")
	}

	// Today's window, already claimed: settled.
	claimedToday := []interface{}{
		map[string]interface{}{
			"actionType":  "CLAIM_BENEFIT",
			"id":          "camp-2",
			"claimStatus": "CLAIMED",
			"startAt":     float64(todayStart),
			"endAt":       float64(end),
		},
	}
	if !campaignsSettle(claimedToday, now) {
		t.Fatal("today's claimed share must settle the check-in")
	}
}

// campaignsSettle mirrors the loop in checkinCampaigns: true when a claimed
// campaign settles the day.
func campaignsSettle(list []interface{}, now time.Time) bool {
	nowUnix := now.Unix()
	var claimable map[string]any
	benefitSeen := false
	for _, it := range list {
		c, ok := it.(map[string]any)
		if !ok || toString(c["actionType"]) != "CLAIM_BENEFIT" {
			continue
		}
		benefitSeen = true
		start := toInt64(c["startAt"])
		end := toInt64(c["endAt"])
		active := (start == 0 || start <= nowUnix) && (end == 0 || end > nowUnix)
		switch toString(c["claimStatus"]) {
		case "CLAIMABLE":
			if active {
				claimable = c
			}
		case "CLAIMED":
			if active {
				if start > 0 && start < qoderDailyRefreshUnix(now) {
					break
				}
				return true
			}
		}
	}
	_ = claimable
	_ = benefitSeen
	return false
}

// The refresh boundary is 10:00 UTC+8 today, regardless of host zone.
func TestBP174DailyRefreshBoundary(t *testing.T) {
	nyc := time.FixedZone("UTC-5", -5*3600)
	now := time.Date(2026, 10, 29, 3, 0, 0, 0, nyc) // 16:00 UTC+8 same day
	want := time.Date(2026, 10, 29, 10, 0, 0, 0, time.FixedZone("UTC+8", 8*3600)).Unix()
	if got := qoderDailyRefreshUnix(now); got != want {
		t.Fatalf("boundary = %d, want %d", got, want)
	}
}
