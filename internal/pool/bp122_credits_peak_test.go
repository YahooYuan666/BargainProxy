package pool

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
)

// BP-122: the balance-only channels (AutoClaw points, Lobster credits) have no
// issued total, so every probe overwrites Total with the current balance and the
// panel can only ever say "now". The high-water mark is what makes a credit
// ARRIVAL visible — the user's request was "新增积分加入上限，便于用户发现有没有
// 新的积分计入": a daily check-in (+400 AutoClaw / +100 Lobster) has to be
// visible as the balance rising above the previous peak.
func TestRecordCreditsTracksPeak(t *testing.T) {
	now := sinkNow
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(now)}, nil, nil)
	a := mkAccount("ac-a", model.ChannelAutoClaw)
	p.Upsert(a)

	// First snapshot: the peak is the balance itself.
	p.RecordCredits(a, model.Credits{Remain: 10000, BalanceOnly: true, UpdatedAt: time.Unix(1000, 0)})
	if a.Credits.Peak != 10000 {
		t.Fatalf("peak = %d after the first snapshot, want 10000", a.Credits.Peak)
	}

	// Spending lowers the balance but must NOT lower the peak.
	p.RecordCredits(a, model.Credits{Remain: 9600, BalanceOnly: true, UpdatedAt: time.Unix(2000, 0)})
	if a.Credits.Peak != 10000 {
		t.Fatalf("peak = %d after spending, want the high-water mark 10000", a.Credits.Peak)
	}
	if a.Credits.Remain != 9600 {
		t.Fatalf("remain = %d, want 9600", a.Credits.Remain)
	}

	// A check-in (+400) raises the balance above the old peak: the peak moves.
	p.RecordCredits(a, model.Credits{Remain: 10000, BalanceOnly: true, UpdatedAt: time.Unix(3000, 0)})
	if a.Credits.Peak != 10000 {
		t.Fatalf("peak = %d after reaching the old peak, want 10000", a.Credits.Peak)
	}
	p.RecordCredits(a, model.Credits{Remain: 10400, BalanceOnly: true, UpdatedAt: time.Unix(4000, 0)})
	if a.Credits.Peak != 10400 {
		t.Fatalf("peak = %d after a +400 arrival, want 10400", a.Credits.Peak)
	}
	if a.Credits.PeakAt.Unix() != 4000 {
		t.Fatalf("peak_at = %v, want the arrival's timestamp (Unix 4000)", a.Credits.PeakAt)
	}
}

// TestRecordCreditsKeepsPeakAcrossUnreadProbes: a probe that returns a lower
// number must not erase the peak, or the "自峰值已用" figure would reset every
// time the balance dips.
func TestRecordCreditsKeepsPeakAcrossUnreadProbes(t *testing.T) {
	now := sinkNow
	p := New(Params{SingleActivePerChannel: false, Now: fixedNow(now)}, nil, nil)
	a := mkAccount("lb-a", model.ChannelLobster)
	p.Upsert(a)

	p.RecordCredits(a, model.Credits{Remain: 142, BalanceOnly: true, UpdatedAt: time.Unix(100, 0)})
	p.RecordCredits(a, model.Credits{Remain: 42, BalanceOnly: true, UpdatedAt: time.Unix(200, 0)})
	p.RecordCredits(a, model.Credits{Remain: 142, BalanceOnly: true, UpdatedAt: time.Unix(300, 0)})
	if a.Credits.Peak != 142 {
		t.Fatalf("peak = %d, want 142 (the highest ever seen)", a.Credits.Peak)
	}
	// A zero-balance probe (spent out) keeps the peak too: that is the case
	// where the operator most needs to see "it used to be 142".
	p.RecordCredits(a, model.Credits{Remain: 0, BalanceOnly: true, UpdatedAt: time.Unix(400, 0)})
	if a.Credits.Peak != 142 {
		t.Fatalf("peak = %d after spending out, want 142", a.Credits.Peak)
	}
}
