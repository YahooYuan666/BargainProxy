package panel

import (
	"strings"
	"testing"
)

// BP-275: the account row's 额度/窗口 cell used to enter its credits branch
// only on `balance_only || total > 0` — the PLAN pool alone. A qoder account
// whose plan is 0/0 but whose add-on/org/dedicated packages hold the balance
// (driver maps all of them to Bonus/BonusTotal, qoder.go probeCreditsFromDoc)
// fell through to "—", while the KPI card right above it (BP-135 accounting:
// remain+bonus over total+bonus_total) showed 805/900. Same data, two
// verdicts. The entry condition must accept the bonus pool too.
func TestBP275RowCreditsBranchCoversBonusPool(t *testing.T) {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded panel: %v", err)
	}
	html := string(raw)
	const cond = "a.credits.balance_only || a.credits.total > 0 || a.credits.bonus > 0 || a.credits.bonus_total > 0"
	if !strings.Contains(html, cond) {
		t.Fatalf("embedded panel missing the bonus-pool entry condition (BP-275):\n  want: %s", cond)
	}
}
