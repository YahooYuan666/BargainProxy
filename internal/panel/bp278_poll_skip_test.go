package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAppJSPollSkipsUnchangedRender 轮询载荷没变时不重建账号表。
//
// 面板每 5 秒静默轮询 state，此前无条件 renderAccounts：renderAccounts 先清空
// tbody 再全量重建，行内焦点与悬停每 5 秒抖一次（BP-278，misakano dd01a52
// 同病同修：payload 签名跳过）。断言打在两处可观测契约上：同载荷连轮两次只
// 渲染一次，载荷变了必须再渲染。
func TestAppJSPollSkipsUnchangedRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; poll skip test skipped")
	}
	script := `const fs = require('fs'), vm = require('vm'), assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const s = src.indexOf('let silentTimer = null');
const e = src.indexOf('function describeAccountDelta', s);
assert.ok(s >= 0 && e > s, 'silentRefresh slice anchors not found: s=' + s + ' e=' + e);
assert.ok(src.slice(s, e).includes('lastPollSig'), 'silentRefresh must carry the poll signature skip (BP-278)');
const dom = new Proxy(function () {}, { get: (t, k) => (k === Symbol.toPrimitive ? () => '' : dom), set: () => true, apply: () => dom });
const el = { classList: { contains: () => false }, hidden: false };
const store = { payload: null };
const calls = { accounts: 0, locks: 0, kpis: 0 };
const ctx = {
  document: { getElementById: () => el, hidden: false }, window: dom, localStorage: dom, location: dom,
  JSON, String, Number, Math, Date, Object, Array, Set, RegExp, console, encodeURIComponent,
  api: async () => store.payload,
  allAccounts: [], allLocks: [],
  renderKpis: () => { calls.kpis++; },
  renderModelLocks: () => { calls.locks++; },
  renderAccounts: () => { calls.accounts++; },
  toast: () => {}, describeAccountDelta: () => '',
};
vm.createContext(ctx);
vm.runInContext(src.slice(s, e), ctx);

(async () => {
  const base = { accounts: [{ uid: 'u1', credits: 5 }], model_locks: [] };
  store.payload = base;
  await ctx.silentRefresh();
  await ctx.silentRefresh();
  assert.strictEqual(calls.accounts, 1, '同载荷重复轮询不该重建账号表，实际 ' + calls.accounts + ' 次');
  store.payload = { accounts: [{ uid: 'u1', credits: 6 }], model_locks: [] };
  await ctx.silentRefresh();
  assert.strictEqual(calls.accounts, 2, '载荷变化后必须重渲染');
  console.log(JSON.stringify(calls));
})().catch(err => { console.error(err); process.exit(1); });`

	f, err := os.CreateTemp(t.TempDir(), "pollskip-*.cjs")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatalf("write script: %v", err)
	}
	f.Close()

	out, err := exec.Command(node, f.Name(), "web/index.html").CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"accounts":2`) {
		t.Fatalf("unexpected output: %s", out)
	}
}
