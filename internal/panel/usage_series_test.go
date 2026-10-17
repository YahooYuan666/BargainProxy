// BP-52: the chart series buckets logs by real time — hourly for short
// windows, daily otherwise; empty periods produce no bucket (the frontend's
// real time axis shows the gaps honestly).
package panel

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
)

func TestUsageSeriesHourlyAndDaily(t *testing.T) {
	base := time.Date(2026, 10, 16, 18, 30, 0, 0, time.Local)
	logs := []model.RequestLog{
		{Time: base, InputTokens: 100, OutputTokens: 10},
		{Time: base.Add(20 * time.Minute), InputTokens: 50, OutputTokens: 5},
		{Time: base.Add(3*time.Hour + 5*time.Minute), InputTokens: 7, OutputTokens: 1},
	}

	hourly := usageSeries(logs, 24)
	if len(hourly) != 2 {
		t.Fatalf("hourly buckets = %+v, want 2 (the empty hours stay gaps)", hourly)
	}
	if hourly[0].Scope != "hour" || hourly[0].T != "2026-10-16T18" {
		t.Fatalf("bucket 0 = %+v", hourly[0])
	}
	if hourly[0].Requests != 2 || hourly[0].PromptTokens != 150 ||
		hourly[0].CompletionTokens != 15 || hourly[0].TotalTokens != 165 {
		t.Fatalf("bucket 0 aggregation = %+v", hourly[0])
	}
	if hourly[1].T != "2026-10-16T21" || hourly[1].Requests != 1 || hourly[1].TotalTokens != 8 {
		t.Fatalf("bucket 1 = %+v", hourly[1])
	}

	daily := usageSeries(logs, 168)
	if len(daily) != 1 || daily[0].Scope != "day" || daily[0].T != "2026-10-16" {
		t.Fatalf("daily series = %+v", daily)
	}
	if daily[0].Requests != 3 || daily[0].TotalTokens != 173 {
		t.Fatalf("daily aggregation = %+v", daily[0])
	}

	// "全部时间" picks by the actual data span: a ≤3-day history stays hourly
	// (parity with the reference panel's default granularity).
	if s := usageSeries(logs, 0); len(s) != 2 || s[0].Scope != "hour" {
		t.Fatalf("all-time short-span series = %+v, want hourly buckets", s)
	}
	// A long history falls back to daily buckets.
	old := model.RequestLog{Time: base.Add(-10 * 24 * time.Hour), InputTokens: 1, OutputTokens: 1}
	if s := usageSeries([]model.RequestLog{logs[0], old}, 0); len(s) != 2 || s[0].Scope != "day" {
		t.Fatalf("all-time long-span series = %+v, want daily buckets", s)
	}
}
