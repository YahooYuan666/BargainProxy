package panel

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/store"
)

func TestHandleTiersClassifiesRoutes(t *testing.T) {
	st := store.NewMemory()
	routes := `[{"model":"glm-5.3","channel":"workbuddy","upstream_model":"glm-5.3","priority":0},` +
		`{"model":"qwen3.8-max","channel":"qoder","upstream_model":"Qwen3.8-Max","priority":0}]`
	if err := st.SetSetting(KeyRoutes, routes); err != nil {
		t.Fatalf("routes: %v", err)
	}
	p := New(config.Default(), nil, st, nil)
	call := func() ([]tierRow, []tierRow) {
		rr := httptest.NewRecorder()
		p.handleTiers(rr, httptest.NewRequest("GET", "/panel/api/tiers", nil))
		if rr.Code != 200 {
			t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
		}
		var body struct {
			Domestic []tierRow `json:"domestic"`
			Foreign  []tierRow `json:"foreign"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body.Domestic, body.Foreign
	}
	find := func(rows []tierRow, source, id string) *tierRow {
		for i := range rows {
			if rows[i].Source == source && rows[i].ID == id {
				return &rows[i]
			}
		}
		return nil
	}

	dom, _ := call()
	if r := find(dom, "workbuddy", "glm-5.3"); r == nil || !r.Routed || r.Retired || r.New {
		t.Fatalf("workbuddy/glm-5.3 = %+v", r)
	}
	if r := find(dom, "workbuddy", "glm-5.2"); r == nil || r.Routed || !r.Retired || r.New {
		t.Fatalf("workbuddy/glm-5.2 = %+v", r)
	}
	if r := find(dom, "qoder", "Qwen3.8-Max"); r == nil || !r.Routed || r.ContextLen != 1000000 || r.Billing == "未收录规格" || r.New {
		t.Fatalf("dynamic qoder row = %+v", r)
	}
	if v, ok, _ := st.GetSetting("tiers_seen_models"); !ok || v == "" {
		t.Fatal("seen baseline must be persisted on the first run")
	}

	// A binding added after the baseline is new (always shown) and routed.
	routes2 := `[{"model":"glm-5.3","channel":"workbuddy","upstream_model":"glm-5.3","priority":0},` +
		`{"model":"qwen3.8-max","channel":"qoder","upstream_model":"Qwen3.8-Max","priority":0},` +
		`{"model":"glm-5.3","channel":"qoder","upstream_model":"GLM-5.3","priority":3}]`
	if err := st.SetSetting(KeyRoutes, routes2); err != nil {
		t.Fatalf("routes2: %v", err)
	}
	dom, _ = call()
	if r := find(dom, "qoder", "GLM-5.3"); r == nil || !r.Routed || !r.New {
		t.Fatalf("new route row = %+v", r)
	}
}
