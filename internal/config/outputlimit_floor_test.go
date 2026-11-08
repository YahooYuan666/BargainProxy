package config

import "testing"

// BP-295（misakano 2b9b773/34a3ae6）：workbuddy 上游把思考与正文记在同一份
// max_tokens 里——effort=high + max_tokens=2048 时思考吃掉 2048、正文断半句。
// 各档位的输出预算下限：低于它时思考必然先吃掉一半以上。
func TestMinOutputBudgetByEffort(t *testing.T) {
	cases := []struct {
		gear string
		want int
	}{
		{"minimal", 4096},
		{"low", 4096},
		{"medium", 6144},
		{"high", 8192},
		{"xhigh", 12288},
		{"max", 16384},
	}
	for _, c := range cases {
		got, ok := MinOutputBudget(c.gear, "m", "m")
		if !ok || got != c.want {
			t.Errorf("MinOutputBudget(%q) = %d,%v want %d,true", c.gear, got, ok, c.want)
		}
	}
	if _, ok := MinOutputBudget("", "m", "m"); ok {
		t.Error("unknown gear must report ok=false")
	}
	if _, ok := MinOutputBudget("nonsense", "m", "m"); ok {
		t.Error("unknown gear must report ok=false")
	}
}

// 目录声明的输出上限比下限小时以目录为准——抬到模型物理上写不了的值没有意义。
func TestMinOutputBudgetRespectsCatalogCap(t *testing.T) {
	got, ok := MinOutputBudget("max", "small-model", "small-model")
	if !ok {
		t.Fatal("known gear must report ok=true")
	}
	// small-model 不在目录里 → 不封顶，全档下限原样。
	if got != 16384 {
		t.Fatalf("uncataloged model: floor = %d, want 16384", got)
	}
	if got, _ := MinOutputBudget("max", "doubao-seed-2-1-pro", ""); got != 16384 {
		t.Fatalf("catalog cap above the floor must not raise the floor: %d", got)
	}
}

// 抬升矩阵：客户端显式给的预算低于档位下限 → 抬；探活级 / 已达标 / 档位未知 → 不动。
func TestRaiseMinOutputBudget(t *testing.T) {
	cases := []struct {
		name      string
		maxTokens int
		gear      string
		want      int
		changed   bool
	}{
		{"high+2048", 2048, "high", 8192, true},
		{"max+1024", 1024, "max", 16384, true},
		{"low+512", 512, "low", 4096, true},
		{"xhigh+2048", 2048, "xhigh", 12288, true},
		{"大写档位", 2048, "HIGH", 8192, true},
		{"已达标不动", 8192, "high", 8192, false},
		{"超出下限不动", 65536, "high", 65536, false},
		{"探活 max_tokens=1", 1, "max", 1, false},
		{"探活 max_tokens=15", 15, "max", 15, false},
		{"探活边界 16", 16, "max", 16384, true},
		{"档位未知", 512, "", 512, false},
	}
	for _, c := range cases {
		got, changed := RaiseMinOutputBudget(c.maxTokens, c.gear, "m", "m")
		if got != c.want || changed != c.changed {
			t.Errorf("%s: RaiseMinOutputBudget(%d,%q) = %d,%v want %d,%v",
				c.name, c.maxTokens, c.gear, got, changed, c.want, c.changed)
		}
	}
}
