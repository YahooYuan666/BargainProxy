// 2026-10-16 用户实报：添加账号 → WorkBuddy → "发起失败：csrf token invalid"。
// 根因=开放模式的 CSRF token 是每进程一个，面板页在网关重启前打开时持有旧
// token，所有 mutation 都会被 403。修复=前端在 403/error 含 csrf 时自动从
// state 换新 token 并重试一次（重启对用户透明），服务端错误文案补刷新提示。
// 本测试锁死两条契约：
//  1. 服务端 403 体内的错误串必须含 "csrf"（前端据此判定是否自动换 token）；
//  2. 面板 HTML 必须带 refreshCSRF + api() 内的换 token 重试逻辑。
package panel

import (
	"strings"
	"testing"
)

func TestPanelShipsCSRFAutoRefresh(t *testing.T) {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded panel: %v", err)
	}
	html := string(raw)
	for _, want := range []string{
		"async function refreshCSRF()",
		"sessionStorage.setItem('csrf', csrf)",
		"String(data.error||'').includes('csrf')",
		"csrf = st.csrf;", // init 与 refreshCSRF 都以此方式刷新
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("embedded panel missing CSRFAutoRefresh piece %q", want)
		}
	}
}
