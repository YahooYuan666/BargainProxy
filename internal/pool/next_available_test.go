// BP-64: pre-flight availability gate — a router must be able to ask "can this
// channel serve THIS model right now?" without waiting; a cooling pool is
// skipped, a busy pool is still worth queueing for.
package pool

import (
	"testing"
	"time"

	"bargainproxy/internal/model"
)

func TestNextAvailableGatesOnCooldowns(t *testing.T) {
	now := time.Now()
	p := New(Params{Now: time.Now}, map[string]int{"workbuddy": 1}, func(*model.Account) {})
	acc := &model.Account{ID: "workbuddy:acc", Channel: "workbuddy"}
	p.Upsert(acc)

	if ok, _ := p.NextAvailable("workbuddy", "glm-5.3"); !ok {
		t.Fatal("fresh account must be available")
	}

	// Account-level cooldown: unavailable, earliest is the cooldown expiry.
	until := now.Add(2 * time.Hour)
	acc.AccountCooldownUntil = until
	ok, earliest := p.NextAvailable("workbuddy", "glm-5.3")
	if ok {
		t.Fatal("cooling account must not be available")
	}
	if earliest.Before(until.Add(-time.Second)) {
		t.Fatalf("earliest = %s, want ~%s", earliest, until)
	}

	// Model-level cooldown: only that model is blocked; another model is fine.
	acc.AccountCooldownUntil = time.Time{}
	modelUntil := now.Add(30 * time.Minute)
	acc.ModelCooldowns = map[string]model.ModelCooldown{
		"glm-5.3": {Until: modelUntil},
	}
	if ok, _ := p.NextAvailable("workbuddy", "glm-5.3"); ok {
		t.Fatal("model-cooled pair must not be available")
	}
	if ok, _ := p.NextAvailable("workbuddy", "kimi-k3"); !ok {
		t.Fatal("other models must stay available")
	}

	// Fully disabled/paused pool: structural block, no known recovery time.
	acc.ModelCooldowns = nil
	acc.Disabled = true
	if ok, earliest := p.NextAvailable("workbuddy", "glm-5.3"); ok || !earliest.IsZero() {
		t.Fatalf("disabled account: ok=%v earliest=%s, want structural block", ok, earliest)
	}

	// Empty channel: structural block.
	if ok, earliest := p.NextAvailable("trae", "glm-5.3"); ok || !earliest.IsZero() {
		t.Fatalf("empty channel: ok=%v earliest=%s, want structural block", ok, earliest)
	}
}
