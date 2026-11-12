// BP-314: a quota percentage must carry the moment it was read, and say so
// when it is stale.
//
// Field 2026-11-12: the antigravity quota probe had been failing for hours
// (TLS handshake timeouts to both hosts, then EOF to oauth2). The panel kept
// rendering the numbers fetched at 13:19 — five_hour=100%, weekly=95.6% —
// with nothing on the card saying how old they were. Asked "is there quota
// left?", that screen answered "yes, 100%" while the model was refusing
// service. The reading was real; presenting it as current was the lie.
package panel_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"bargainproxy/internal/model"
)

func TestPanelWindowsCarryUpdatedAtAndStaleFlag(t *testing.T) {
	srv, _, gw, _ := buildPanelEnv(t, nil)
	acc := gw.Pool().Accounts(model.ChannelWorkBuddy)[0]

	fresh := time.Now().Add(-2 * time.Minute)
	stale := time.Now().Add(-6 * time.Hour)

	gw.Pool().Lock()
	acc.Windows = map[string]model.Window{
		"five_hour": {RemainingFraction: 1.0, ResetAt: time.Now().Add(2 * time.Hour), UpdatedAt: fresh},
		"weekly":    {RemainingFraction: 0.956, ResetAt: time.Now().Add(48 * time.Hour), UpdatedAt: stale},
	}
	gw.Pool().Unlock()

	type win struct {
		RemainingFraction float64 `json:"remaining_fraction"`
		ResetAt           string  `json:"reset_at"`
		UpdatedAt         string  `json:"updated_at"`
		Stale             bool    `json:"stale"`
	}
	readWindows := func() map[string]win {
		t.Helper()
		resp, err := http.Get(srv.URL + "/panel/api/state")
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		defer resp.Body.Close()
		var st struct {
			Accounts []struct {
				ID      string         `json:"id"`
				Windows map[string]win `json:"windows"`
			} `json:"accounts"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			t.Fatalf("decode state: %v", err)
		}
		for _, a := range st.Accounts {
			if a.ID == acc.ID {
				return a.Windows
			}
		}
		t.Fatalf("account %s missing from state", acc.ID)
		return nil
	}

	w := readWindows()
	fh, ok := w["five_hour"]
	if !ok {
		t.Fatal("five_hour window missing")
	}
	if fh.UpdatedAt == "" {
		t.Fatal("five_hour carries no updated_at — a percentage without a read time cannot be told apart from a stale one")
	}
	if fh.Stale {
		t.Fatalf("a 2-minute-old reading must not be marked stale (stale=%v)", fh.Stale)
	}

	wk, ok := w["weekly"]
	if !ok {
		t.Fatal("weekly window missing")
	}
	if wk.UpdatedAt == "" {
		t.Fatal("weekly carries no updated_at")
	}
	if !wk.Stale {
		t.Fatalf("a 6-hour-old reading must be marked stale (stale=%v) — this is what let a 13:19 figure be quoted as current", wk.Stale)
	}
	if wk.RemainingFraction != 0.956 {
		t.Fatalf("remaining_fraction = %v, want 0.956 unchanged by the new fields", wk.RemainingFraction)
	}
}
