// ticket 31: scheduler slot catch-up — due-slot纯函数与最后已跑持久化。
package tasks

import (
	"testing"
	"time"

	"bargainproxy/internal/store"
)

func localAt(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, Shanghai)
}

// TestDueSlot 钉住「到点即跑、已跑不重复、睡过补跑、跨日重置」口径：
// 机器睡过时点或 tick 被推迟到 :06 时，最近一个已过且未跑的时点必须补跑，
// 而不是整日漏掉（ticket 31；agentpoints 0e52af2 的墙钟重判思想）。
func TestDueSlot(t *testing.T) {
	cases := []struct {
		name      string
		now       time.Time
		hours     []int
		minute    int
		lastFired string
		wantID    string
		wantOK    bool
	}{
		{"未到点不触发", localAt(2026, 10, 16, 9, 4), []int{9, 21}, 5, "", "", false},
		{"到点即触发", localAt(2026, 10, 16, 9, 5), []int{9, 21}, 5, "", "2026-10-16 09", true},
		{"tick 推迟到 :06 也能触发", localAt(2026, 10, 16, 9, 6), []int{9, 21}, 5, "", "2026-10-16 09", true},
		{"已跑不重复", localAt(2026, 10, 16, 9, 30), []int{9, 21}, 5, "2026-10-16 09", "", false},
		{"睡过时点迟到补跑（取最近已过时点）", localAt(2026, 10, 16, 14, 40), []int{9, 21}, 5, "", "2026-10-16 09", true},
		{"第二个时点覆盖第一个", localAt(2026, 10, 16, 21, 36), []int{9, 21}, 5, "2026-10-16 09", "2026-10-16 21", true},
		{"两个时点都已跑则静默", localAt(2026, 10, 16, 22, 0), []int{9, 21}, 5, "2026-10-16 21", "", false},
		{"跨日重置", localAt(2026, 10, 17, 9, 6), []int{9, 21}, 5, "2026-10-16 21", "2026-10-17 09", true},
		{"空时点列表不触发", localAt(2026, 10, 16, 12, 0), nil, 5, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, _, ok := dueSlot(c.now, c.hours, c.minute, c.lastFired)
			if ok != c.wantOK || id != c.wantID {
				t.Fatalf("dueSlot = (%q, %v), want (%q, %v)", id, ok, c.wantID, c.wantOK)
			}
		})
	}
}

// TestMarkFiredPersists 最后已跑必须落 settings（跨重启不重复补跑）。
func TestMarkFiredPersists(t *testing.T) {
	st := store.NewMemory()
	s := &Scheduler{st: st, fired: map[string]string{}}
	s.markFired("checkin", "2026-10-16 14")

	if got := s.lastFiredSlot("checkin"); got != "2026-10-16 14" {
		t.Fatalf("in-memory slot = %q", got)
	}
	v, ok, err := st.GetSetting(keyLastFiredPrefix + "checkin")
	if err != nil || !ok || v != "2026-10-16 14" {
		t.Fatalf("persisted = (%q, %v, %v), want 2026-10-16 14", v, ok, err)
	}
}
