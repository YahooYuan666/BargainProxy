// 2026-10-16 用户实报：强制刷新后"密码框又回来了"。
// 浏览器实测根因：带 #hash 的重载在脚本顶层同步执行 switchView → 撞上尚未
// 初始化的 let allLocks（TDZ）→ ReferenceError 中断整个脚本 → init 从未运行
// → 标记中预置可见的登录框永远挂着。本测试锁死三条防线：
//  1. 登录框默认隐藏（只有服务端真实 401 才允许 showLogin 打开）；
//  2. 初始视图只在 init（数据状态就绪）后应用，顶层不再直接调用 switchView；
//  3. 引导看门狗兜底：脚本中断时给明确提示，绝不显示密码框。
package panel

import (
	"strings"
	"testing"
)

func panelScripts(t *testing.T, html string) []string {
	t.Helper()
	var blocks []string
	rest := html
	for {
		i := strings.Index(rest, "<script>")
		if i < 0 {
			break
		}
		j := strings.Index(rest[i:], "</script>")
		if j < 0 {
			t.Fatalf("unbalanced <script> block")
		}
		blocks = append(blocks, rest[i+len("<script>"):i+j])
		rest = rest[i+j+len("</script>"):]
	}
	if len(blocks) == 0 {
		t.Fatal("no script blocks in panel html")
	}
	return blocks
}

func TestPanelLoginVeilHiddenByDefault(t *testing.T) {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded panel: %v", err)
	}
	html := string(raw)
	if strings.Contains(html, `class="veil on" id="loginVeil"`) {
		t.Fatal("登录框在标记里预置可见（class=veil on）——脚本一旦中断它就会冒充密码提示")
	}
	if !strings.Contains(html, `<div class="veil" id="loginVeil">`) {
		t.Fatal("登录框标记缺失或形态意外")
	}
	// .veil.on 的 display:flex 会压过 [hidden] 的 UA 样式；预置 on+hidden 的组合
	// 正是"页面一加载就可见"的机制，双重机制必须不再同时出现在登录框上。
	if strings.Contains(html, `<div class="veil on" id="loginVeil" hidden>`) {
		t.Fatal("登录框仍带 hidden+on 双重机制")
	}
}

func TestPanelInitialViewAppliesOnlyAfterInit(t *testing.T) {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded panel: %v", err)
	}
	html := string(raw)
	// 旧 bug 行：顶层同步派发初始视图（TDZ 中断脚本的引信）。
	if strings.Contains(html, "if (location.hash) switchView(location.hash);") {
		t.Fatal("顶层仍在同步派发初始视图（allLocks TDZ 事故的引信）")
	}
	if !strings.Contains(html, "function applyInitialView()") || !strings.Contains(html, "applyInitialView();") {
		t.Fatal("初始视图未改为 init 后应用（applyInitialView 定义/调用缺失）")
	}

	// 结构护栏：任何 brace-depth 0 的顶层语句引用 switchView 时，必须是
	// 回调注册（addEventListener / 箭头函数），不得直接执行。
	for _, block := range panelScripts(t, html) {
		depth := 0
		for _, line := range strings.Split(block, "\n") {
			stmt := strings.TrimSpace(line)
			atTop := depth == 0
			if atTop && strings.Contains(stmt, "switchView(") &&
				!strings.HasPrefix(stmt, "function ") && !strings.HasPrefix(stmt, "async function ") &&
				!strings.Contains(stmt, "=>") && !strings.Contains(stmt, "addEventListener") {
				t.Fatalf("顶层语句直接调用 switchView（TDZ 风险）：%s", stmt)
			}
			depth += strings.Count(line, "{") - strings.Count(line, "}")
		}
	}
}

func TestPanelBootWatchdogAndBootFlag(t *testing.T) {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded panel: %v", err)
	}
	html := string(raw)
	scripts := strings.Join(panelScripts(t, html), "\n")
	if !strings.Contains(scripts, "window.__panelBooted = true") {
		t.Fatal("init 未置位 __panelBooted（看门狗失去判定依据）")
	}
	if !strings.Contains(html, "__panelBooted") || !strings.Contains(html, "bootVeil") {
		t.Fatal("引导看门狗脚本缺失")
	}
	// 看门狗必须晚于主脚本、且有 3 秒窗口。
	if !strings.Contains(html, "}, 3000);") {
		t.Fatal("看门狗窗口未按 3 秒设置")
	}
}
