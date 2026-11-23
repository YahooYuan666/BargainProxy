// BP-335: agent-only models must START their function walk on their own group.
// solo_agent_remote-only models (Doubao-Seed-Code, qwen3.8-flash) answer 4001
// under solo_work_lite, and the error text looks like a payload bug — one
// doomed upstream round trip per request before the ladder reached the right
// rung (trae2api-hub 32961736, 2026-11-20).
package trae

import "testing"

func TestAgentOnlyModelsStartOnTheirGroup(t *testing.T) {
	for _, m := range []string{"Doubao-Seed-Code", "qwen3.8-flash", "QWEN3.8-FLASH"} {
		fns := preferredFunctionsFor(m)
		if fns[0] != "solo_agent_remote" {
			t.Fatalf("model %s must start on solo_agent_remote, got %s", m, fns[0])
		}
		if len(fns) != len(functionsTrae) {
			t.Fatalf("the ladder must keep every rung, got %d want %d", len(fns), len(functionsTrae))
		}
	}
}

// Every other model's walk is byte-identical to the static ladder.
func TestOtherModelsKeepDefaultOrder(t *testing.T) {
	fns := preferredFunctionsFor("deepseek-v4.1-flash")
	if len(fns) != len(functionsTrae) {
		t.Fatal("the ladder must not change for other models")
	}
	for i := range fns {
		if fns[i] != functionsTrae[i] {
			t.Fatalf("run %d = %s, want %s", i, fns[i], functionsTrae[i])
		}
	}
}
