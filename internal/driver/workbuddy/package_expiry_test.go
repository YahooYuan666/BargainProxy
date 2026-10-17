// ticket 34: package expiry resolution — DeductionEndTime is the authoritative
// "use it or lose it" moment (the upstream deducts FEFO by it).
package workbuddy

import (
	"testing"
	"time"
)

// TestParsePackageEndPrefersDeductionEndTime DeductionEndTime（epoch 毫秒，
// 可抵扣窗口结束）优先于 ExpiredTime/PackageEndTime/CycleEndTime 三个字符串
// 字段——按周期边界判到期会把真实失效时刻算错（agentpoints acb3830）。
func TestParsePackageEndPrefersDeductionEndTime(t *testing.T) {
	deduction := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	p := billingPackage{
		DeductionEndTime: deduction.UnixMilli(),
		ExpiredTime:      "2026-12-31 00:00:00",
		PackageEndTime:   "2026-12-31 00:00:00",
		CycleEndTime:     "2026-12-31 00:00:00",
	}
	if got := parsePackageEnd(p); got.UnixMilli() != deduction.UnixMilli() {
		t.Fatalf("parsePackageEnd = %v, want DeductionEndTime %v", got, deduction)
	}

	// Fallbacks keep working when DeductionEndTime is absent: the first
	// present wall-clock field wins (ExpiredTime → PackageEndTime → CycleEndTime).
	want, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-12-30 08:00:00", time.FixedZone("CST", 8*3600))
	if got := parsePackageEnd(billingPackage{ExpiredTime: "2026-12-30 08:00:00", CycleEndTime: "2026-12-31 00:00:00"}); !got.Equal(want) {
		t.Fatalf("ExpiredTime fallback = %v, want %v", got, want)
	}
	if got := parsePackageEnd(billingPackage{PackageEndTime: "2026-12-30 08:00:00", CycleEndTime: "2026-12-31 00:00:00"}); !got.Equal(want) {
		t.Fatalf("PackageEndTime fallback = %v, want %v", got, want)
	}
	if got := parsePackageEnd(billingPackage{CycleEndTime: "2026-12-30 08:00:00"}); !got.Equal(want) {
		t.Fatalf("CycleEndTime fallback = %v, want %v", got, want)
	}
	if got := parsePackageEnd(billingPackage{}); !got.IsZero() {
		t.Fatalf("no fields = %v, want zero time", got)
	}
}
