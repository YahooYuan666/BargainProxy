// BP-88 UI contract: the update notice must stay a notice. These tests read
// the embedded page and lock the parts that would silently turn it into
// something else (an auto-download, a blocking boot path, or a banner that
// fires for builds it cannot judge).
package panel

import (
	"strings"
	"testing"
)

func panelHTML(t *testing.T) string {
	t.Helper()
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded panel: %v", err)
	}
	return string(raw)
}

func TestUpdateBannerStartsHidden(t *testing.T) {
	html := panelHTML(t)
	if !strings.Contains(html, `id="updBar" hidden`) {
		t.Fatal("提醒条必须默认隐藏，只在确有更新时由 JS 打开")
	}
}

func TestUpdateBannerNeverPromisesAutoInstall(t *testing.T) {
	html := panelHTML(t)
	// The whole feature is a notice. Any of these phrases would promise
	// something the project does not do (it publishes source only).
	for _, forbidden := range []string{"立即更新", "自动更新到", "下载新版", "一键升级"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("面板不得出现 %q —— 本项目只发源码，没有可下载的升级包", forbidden)
		}
	}
	if !strings.Contains(html, "只提醒、不自动升级") {
		t.Fatal("关于卡片必须说清只提示不代劳")
	}
}

func TestUpdateBannerIsNotShownForUnjudgeableStates(t *testing.T) {
	html := panelHTML(t)
	// The banner gates on the server-computed notice flag, so a local build
	// sitting exactly on the remote head (nothing to look at) stays silent.
	if !strings.Contains(html, "verState.notice === true") {
		t.Fatal("提醒条必须由服务端的 notice 决定是否显示")
	}
	if strings.Contains(html, "st === 'local_build' && verState.remote") {
		t.Fatal("本地构建只要拿到 remote 就弹条会误报（上游=基准提交时并无更新）")
	}
}

func TestVersionLoadingIsNotOnTheBootCriticalPath(t *testing.T) {
	html := panelHTML(t)
	// A blocked GitHub must never stop the panel from booting: the version call
	// has no await in init() and swallows its own errors.
	if !strings.Contains(html, "loadVersion();") {
		t.Fatal("refresh 必须触发 loadVersion")
	}
	if strings.Contains(html, "await loadVersion();") {
		t.Fatal("loadVersion 不得被 await 进 refresh 的关键路径")
	}
	if !strings.Contains(html, "更新提醒失败绝不打断面板其它功能") {
		t.Fatal("错误必须被吞掉，不能冒泡成 toast 打断刷新")
	}
}

func TestVersionPanelWordingExplainsTheDirtyBuildCase(t *testing.T) {
	html := panelHTML(t)
	if !strings.Contains(html, "无法判断") {
		t.Fatal("必须向用户说明：本机带未提交改动的构建无法判断是否落后")
	}
	if !strings.Contains(html, "不会谎报有新版本") {
		t.Fatal("必须明确承诺不谎报")
	}
}
