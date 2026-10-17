// BP-49: the panel state carries every unexpired (account, model) rate-limit
// hold as a detail row — model, the gateway's retry barrier (until), the
// upstream reset, the short reason label and the upstream text. Expired
// entries are omitted entirely (the card used to render an empty ghost row).
package panel_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"bargainproxy/internal/model"
)

func TestPanelStateCarriesModelCooldownDetails(t *testing.T) {
	srv, _, gw, _ := buildPanelEnv(t, nil)
	acc := gw.Pool().Accounts(model.ChannelWorkBuddy)[0]

	until := time.Now().Add(2 * time.Hour)
	resetAt := time.Now().Add(6 * time.Hour)
	gw.Pool().Lock()
	acc.ModelCooldowns = map[string]model.ModelCooldown{
		"glm-5.3": {
			Until:   until,
			ResetAt: resetAt,
			Reason:  "6004 model rate limit",
			Message: "workbuddy: 您的使用量已超出频率限制",
		},
		"stale-model": {Until: time.Now().Add(-time.Minute)},
	}
	gw.Pool().Unlock()

	type coolRow struct {
		Model   string `json:"model"`
		Until   string `json:"until"`
		ResetAt string `json:"reset_at"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	readCool := func() []coolRow {
		t.Helper()
		resp, err := http.Get(srv.URL + "/panel/api/state")
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		defer resp.Body.Close()
		var st struct {
			Accounts []struct {
				ID        string    `json:"id"`
				ModelCool []coolRow `json:"model_cooldowns"`
			} `json:"accounts"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			t.Fatalf("decode state: %v", err)
		}
		for _, a := range st.Accounts {
			if a.ID == acc.ID {
				return a.ModelCool
			}
		}
		t.Fatalf("account %s missing from state", acc.ID)
		return nil
	}

	rows := readCool()
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want exactly the unexpired hold", rows)
	}
	r := rows[0]
	if r.Model != "glm-5.3" || r.Until != until.Format(time.RFC3339) ||
		r.ResetAt != resetAt.Format(time.RFC3339) ||
		r.Reason != "6004 model rate limit" || r.Message == "" {
		t.Fatalf("row = %+v", r)
	}

	// All entries expired → the field is omitted entirely.
	gw.Pool().Lock()
	acc.ModelCooldowns = map[string]model.ModelCooldown{"stale-model": {Until: time.Now().Add(-time.Minute)}}
	gw.Pool().Unlock()
	if rows := readCool(); rows != nil {
		t.Fatalf("expired-only holds must be omitted, got %+v", rows)
	}
}
