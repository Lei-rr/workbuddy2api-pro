package panel

import (
	"bytes"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAppJSSyntax app.js / mobile.js 必须能通过 JS 解析器语法校验。
//
// 为什么需要：两者都是 go:embed 进二进制的静态资源，Go 编译器不检查其内容——
// 一次对象字面量键名未加引号（Model_chat_GLM5.2 被解析成属性访问 + 数字字面量）
// 就让整个面板白屏，而所有 Go 测试依然全绿。此测试把语法校验前移到 CI。
// 无 node 环境时跳过（不阻塞无 Node 的构建机）。
func TestAppJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	for _, name := range []string{"app.js", "mobile.js"} {
		path, err := filepath.Abs(name)
		if err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(node, "--check", path).CombinedOutput()
		if err != nil {
			t.Fatalf("%s syntax error:\n%s", name, out)
		}
	}
}

// TestIndexHTMLNoInlineScript index.html 不得含内联 <script> 块：
// 严格 CSP（script-src 'self'）会拦截内联脚本，页面将完全不可用。
// 外链形式 <script src="..."> 允许。
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	rest := body
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx:]
		end := strings.Index(rest, ">")
		if end < 0 {
			break
		}
		tag := rest[:end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("index.html contains inline <script> (blocked by CSP): %s", tag)
		}
		rest = rest[end:]
	}
}

// TestAppJSTopLevelSmoke app.js 顶层求值冒烟（v1.11.3/1.11.4 两连炸后补的运行时闸门）：
// node + DOM 桩执行 app.js（含按 hash 落到各视图的 go() 顶层调用），抓 TDZ/
// ReferenceError 类运行时错误——Go 侧 frontend_test 不执行 JS，语法层检查对此全盲。
// 无 node 的环境跳过（CI/精简机不受影响）；harness 与 app.js 同判（app.js 顶层
// start() 的 setInterval 会让 node 事件循环不退出，故成功路径显式 exit(0)）。
func TestAppJSTopLevelSmoke(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; JS smoke skipped")
	}
	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});
const sandbox = new Proxy({
  location: { hash: process.env.SMOKE_HASH || '#taskscenter' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert, getElementById: () => inert, addEventListener() {}, documentElement: inert, head: inert, body: inert, createElement: () => inert, cookie: '' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  // mobile.js 顶层直接 new MutationObserver(...)（移动端表格字段名补标签）：
  // 该文件不在本沙箱内执行，但保留此桩，将来把 mobile.js 纳入冒烟时可直接复用。
  MutationObserver: class { constructor() {} observe() {} disconnect() {} takeRecords() { return []; } },
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
try {
  vm.runInContext(src, sandbox, { filename: 'app.js' });
  console.log('SMOKE OK');
  process.exit(0);
} catch (e) {
  console.log('SMOKE FAIL:', (e && e.stack ? e.stack : e).toString().split('\n').slice(0, 5).join('\n'));
  process.exit(1);
}
`
	hf, err := os.CreateTemp(t.TempDir(), "smoke-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	hf.Close()
	for _, hash := range []string{"#taskscenter", "#accounts", "#usage", "#models", "#config", "#logs", "#packages"} {
		cmd := exec.Command(node, hf.Name(), "app.js")
		cmd.Dir = "." // 测试工作目录 = internal/panel
		cmd.Env = append(os.Environ(), "SMOKE_HASH="+hash)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("app.js 顶层求值 %s 崩溃: %v\n%s", hash, err, out)
		}
		if !bytes.Contains(out, []byte("SMOKE OK")) {
			t.Fatalf("app.js smoke %s 未通过:\n%s", hash, out)
		}
	}
}

// 积分扣除维度的格式必须稳定，且缺样本/缺匹配 Token 时不能伪造比例。
func TestAppJSCreditDimensionFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; credit formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
	const start = src.indexOf('function trimFixed');
const end = src.indexOf('function usStat');
if (start < 0 || end < 0) throw new Error('credit helpers not found');
const ctx = { Number, String, RegExp };
vm.createContext(ctx);
vm.runInContext(
  src.slice(start, end) +
  '\nthis.fmtCredit=fmtCredit; this.fmtCreditRatio=fmtCreditRatio; this.fmtModelRate=fmtModelRate;',
  ctx
);
process.stdout.write(JSON.stringify({
  credit: ctx.fmtCredit(1.25),
  zero: ctx.fmtCredit(0),
  hundred: ctx.fmtCredit(100),
  ratio: ctx.fmtCreditRatio(12.5, 2, 400),
  noSamples: ctx.fmtCreditRatio(12.5, 0, 400),
  noTokens: ctx.fmtCreditRatio(12.5, 2, 0),
  rate: ctx.fmtModelRate('0.5'),
  noRate: ctx.fmtModelRate(''),
}));`
	f, err := os.CreateTemp(t.TempDir(), "credit-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("credit formatting node test failed: %v\n%s", err, out)
	}
	const want = `{"credit":"1.25","zero":"0","hundred":"100","ratio":"12.5 / 1M","noSamples":"—","noTokens":"—","rate":"x0.5","noRate":"—"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("credit formatting=%s want %s", out, want)
	}
}

// 模型限流时间必须同时支持上游 reset_at、网关 until 和无重置时间三种形态。
func TestAppJSRateLimitMeta(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; rate limit formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function dur(');
const end = src.indexOf('function rateLimitRowsHtml');
if (start < 0 || end < 0) throw new Error('rate limit helpers not found');
const ctx = { Date, Number, String, Math };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.rateLimitMeta=rateLimitMeta;', ctx);
const now = new Date(2026, 8, 28, 14, 0, 0).getTime();
const reset = new Date(2026, 8, 28, 16, 0, 0).getTime();
const until = new Date(2026, 8, 28, 15, 0, 0).getTime();
const rate = ctx.rateLimitMeta({ model: 'glm-5.3', kind: 'rate_limit', reset_at: new Date(reset).toISOString(), until: new Date(until).toISOString() }, now);
const unavailable = ctx.rateLimitMeta({ model: 'missing', kind: 'model_unavailable', until: new Date(until).toISOString() }, now);
const unknown = ctx.rateLimitMeta({ model: 'glm-5.3', kind: 'rate_limit' }, now);
process.stdout.write(JSON.stringify({
  rate: rate.detail,
  unavailable: unavailable.detail,
  unknown: unknown.detail,
}));`
	f, err := os.CreateTemp(t.TempDir(), "rate-limit-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("rate-limit formatting node test failed: %v\n%s", err, out)
	}
	// issue #171 问题二：until < resetAt 时主文字双时刻明示、倒计时跟随 until。
	const want = `{"rate":"网关 2026-09-28 15:00 起重试（剩余 1时00分） · 上游称 2026-09-28 16:00 恢复","unavailable":"预计 1时00分 后重试","unknown":"预计解封时间未知"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("rate-limit formatting=%s want %s", out, want)
	}
}

// 请求记录行必须紧凑、可读，并带上调用来源（IP / UA）；来源缺失时以 — 兜底。
func TestAppJSRequestLogFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; request log formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
const reqStart = src.indexOf('function requestLogText');
const reqEnd = src.indexOf('function fmtBytes');
if ([escStart, escEnd, fmtStart, fmtEnd, reqStart, reqEnd].some(v => v < 0)) throw new Error('request log helpers not found');
const ctx = { Date, Number, String, Math, RegExp, isNaN };
vm.createContext(ctx);
vm.runInContext(
  src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(reqStart, reqEnd) +
  '\nthis.requestLogText=requestLogText;',
  ctx
);
const time = new Date(2026, 8, 28, 14, 5, 6).toISOString();
const good = { time, status: 200, outcome: 'success', model: 'glm-5.3', account: '账号(uid8)', duration_ms: 1250, total_tokens: 2300, credit_known: true, credit: 0.12, request_id: 'req-1', client_ip: '203.0.113.7', user_agent: 'python-requests/2.31.0' };
const noSource = { ...good, request_id: 'req-3', client_ip: '', user_agent: '' };
const cached = { ...good, request_id: 'req-2', cache_hit_tokens: 2257, cache_miss_tokens: 43 };
process.stdout.write(JSON.stringify({
  good: ctx.requestLogText(good),
  noSource: ctx.requestLogText(noSource),
  cached: ctx.requestLogText(cached),
}));`
	f, err := os.CreateTemp(t.TempDir(), "request-log-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("request log formatting node test failed: %v\n%s", err, out)
	}
	text := "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | req-1"
	noSource := "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | — | — | 1.25s | 2.3k tok | 0.12 credit | req-3"
	cached := "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | 命中 98.1% | req-2"
	want := `{"good":` + strconv.Quote(text) + `,"noSource":` + strconv.Quote(noSource) + `,"cached":` + strconv.Quote(cached) + `}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("request log formatting=%s want %s", out, want)
	}
}

// 积分历史区块：变动列 +N / −N（U+2212）、余额千分位、空列表提示。
func TestAppJSCreditHistoryFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; credit history formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function creditNum');
const end = src.indexOf('function renderCreditHistory');
if (start < 0 || end < 0) throw new Error('credit history helpers not found');
const ctx = { Date, Number, String, Math, RegExp, isNaN };
vm.createContext(ctx);
vm.runInContext(
  src.slice(start, end) +
  '\nthis.creditEntryText=creditEntryText;this.creditHistoryNote=creditHistoryNote;',
  ctx
);
const time = new Date(2026, 8, 28, 14, 5, 6).toISOString();
process.stdout.write(JSON.stringify({
  gain: ctx.creditEntryText({ time, uid: '89374120', account: '小明', delta: 100, before: 1300, after: 1400 }),
  loss: ctx.creditEntryText({ time, uid: '89374120', account: '', delta: -30, before: 1400, after: 1370 }),
  note: ctx.creditHistoryNote([{ delta: 1300 }, { delta: -60 }]),
  empty: ctx.creditHistoryNote([]),
}));`
	f, err := os.CreateTemp(t.TempDir(), "credit-history-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("credit history formatting node test failed: %v\n%s", err, out)
	}
	gain := "14:05:06 | 小明 | +100 | 1,400 | 余额 1,300 → 1,400"
	loss := "14:05:06 | 89374120 | −30 | 1,370 | 余额 1,400 → 1,370"
	want := `{"gain":` + strconv.Quote(gain) + `,"loss":` + strconv.Quote(loss) +
		`,"note":"2 条 · 净 +1,240","empty":"暂无积分变动记录"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("credit history formatting=%s want %s", out, want)
	}
}

// 请求记录筛选：IP / UA / 模型 / 账号 / 请求 ID 的包含匹配（空格分词 AND）+ 结果精确匹配。
func TestAppJSRequestMatch(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; request filter test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function reqMatch');
const end = src.indexOf('function reqOutcomeTag');
if (start < 0 || end < 0) throw new Error('reqMatch not found');
const ctx = {};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.reqMatch=reqMatch;', ctx);
const base = { outcome: 'success', client_ip: '203.0.113.7', user_agent: 'python-requests/2.31.0', model: 'cn:glm-5.3', account: '示例(uid8)', request_id: 'req-1' };
const other = { outcome: 'http_error', client_ip: '198.51.100.4', user_agent: 'Mozilla/5.0 Chrome/120', model: 'global:hy3', account: '甲(uid9)', request_id: 'req-2' };
const rows = [base, other];
const pick = f => rows.filter(e => ctx.reqMatch(e, f)).map(e => e.request_id);
process.stdout.write(JSON.stringify({
  all: pick({ q: '', outcome: '' }),
  byIP: pick({ q: '203.0.113', outcome: '' }),
  byUA: pick({ q: 'chrome/120', outcome: '' }),
  byModel: pick({ q: 'glm', outcome: '' }),
  multiKw: pick({ q: 'glm success', outcome: '' }),
  multiMiss: pick({ q: 'glm chrome', outcome: '' }),
  byOutcome: pick({ q: '', outcome: 'http_error' }),
  combined: pick({ q: '198.51', outcome: 'http_error' }),
}));`
	f, err := os.CreateTemp(t.TempDir(), "request-filter-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("request filter node test failed: %v\n%s", err, out)
	}
	// q 对 outcome 不参与匹配（outcome 有独立下拉），multiKw 里的 success 命中不了任何字段。
	const want = `{"all":["req-1","req-2"],"byIP":["req-1"],"byUA":["req-2"],"byModel":["req-1"],"multiKw":[],"multiMiss":[],"byOutcome":["req-2"],"combined":["req-2"]}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("request filter=%s want %s", out, want)
	}
}

// 模型按条件查询：域 / 能力 / 档位 / 价格 / 关键词，以及倍率、上下文、输出排序。
func TestAppJSModelFilter(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model filter test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function mdRateValue');
const end = src.indexOf('function mdRowHtml');
if (start < 0 || end < 0) throw new Error('model filter helpers not found');
const ctx = { Number, String, Array, Object, isFinite, parseFloat };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.mdMatch=mdMatch; this.mdSortList=mdSortList; this.mdRateValue=mdRateValue;', ctx);
const models = [
  { id: 'cn:glm-5.2', name: 'GLM-5.2', vendor: 'Zhipu', tags: ['视觉'], supports_tool_call: true, supports_images: true, supports_reasoning: true, can_disable_thinking: true, supported_efforts: ['high', 'xhigh'], default_effort: 'high', is_default: false, credits: '0.79', promo_factor: 0.5, promo_credits: '0.40', promo_label: '夜间折扣', context_length: 1000000, max_output_tokens: 131000 },
  { id: 'cn:hy3', name: 'Hy3', supports_tool_call: true, supports_images: true, supports_reasoning: true, can_disable_thinking: false, supported_efforts: ['low', 'high'], default_effort: 'high', is_default: false, credits: '0', promo_factor: 0, promo_credits: '0', promo_label: '限时免费', context_length: 192000, max_output_tokens: 64000 },
  { id: 'global:hy3', name: 'Hy3 Global', supports_tool_call: false, supports_images: false, supports_reasoning: false, supported_efforts: [], is_default: false, credits: '0.11', context_length: 1000000, max_output_tokens: 393000 },
  { id: 'cn:auto', name: 'Auto', supports_tool_call: true, supports_images: true, supports_reasoning: true, is_default: true, credits: null, context_length: 256000, max_output_tokens: 32000 },
];
const ids = list => list.map(m => m.id);
const filter = f => ids(ctx.mdSortList(models.filter(m => ctx.mdMatch(m, f)), f));
process.stdout.write(JSON.stringify({
  all: ids(models),
  realm: filter({ realm: 'cn' }),
  tool: filter({ cap: 'tool' }),
  vision: filter({ cap: 'vision' }),
  reasoning: filter({ cap: 'reasoning' }),
  isDefault: filter({ cap: 'default' }),
  effortOff: filter({ effort: 'off' }),
  effortLow: filter({ effort: 'low' }),
  free: filter({ promo: 'free' }),
  promo: filter({ promo: 'promo' }),
  discount: filter({ promo: 'discount' }),
  q: filter({ q: 'glm zhipu' }),
  qMiss: filter({ q: 'glm nosuch' }),
  sortRate: filter({ sort: 'rate' }),
  sortContext: filter({ sort: 'context' }),
  sortOutput: filter({ sort: 'output' }),
  sortName: filter({ sort: 'name' }),
  rateFree: ctx.mdRateValue(models[1]),
  rateMissing: ctx.mdRateValue(models[3]),
}));`
	f, err := os.CreateTemp(t.TempDir(), "model-filter-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("model filter node test failed: %v\n%s", err, out)
	}
	const want = `{"all":["cn:glm-5.2","cn:hy3","global:hy3","cn:auto"],` +
		`"realm":["cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"tool":["cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"vision":["cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"reasoning":["cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"isDefault":["cn:auto"],` +
		`"effortOff":["cn:glm-5.2"],` +
		`"effortLow":["cn:hy3"],` +
		`"free":["cn:hy3"],` +
		`"promo":["cn:glm-5.2","cn:hy3"],` +
		`"discount":["cn:glm-5.2"],` +
		`"q":["cn:glm-5.2"],` +
		`"qMiss":[],` +
		`"sortRate":["cn:hy3","global:hy3","cn:glm-5.2","cn:auto"],` +
		`"sortContext":["cn:glm-5.2","global:hy3","cn:auto","cn:hy3"],` +
		`"sortOutput":["global:hy3","cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"sortName":["cn:auto","cn:glm-5.2","cn:hy3","global:hy3"],` +
		`"rateFree":0,"rateMissing":null}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("model filter=%s\nwant %s", out, want)
	}
}

// Global 模型可用性标签：模型锁优先于最近请求；无本地证据时保持“待验证”，
// 不能把上游目录里出现模型误判为当前账号有权限。
func TestAppJSModelAvailability(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model availability test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function mdAvailabilityOf');
const end = src.indexOf('function mdRowHtml');
if (start < 0 || end < 0) throw new Error('model availability helpers not found');
const ctx = { esc: s => String(s) };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.mdAvailabilityOf=mdAvailabilityOf; this.mdAvailabilityTag=mdAvailabilityTag;', ctx);
const model = id => ({ id });
const state = (m, locks, recent) => ctx.mdAvailabilityOf(m, locks, recent).key;
const success = [{ model: 'global:ok', status: 200, outcome: 'success', time: '2026-10-18T12:00:00Z' }];
ctx.mdLocks = [{ realm: 'global', model: 'gpt-5.6-sol', state: 'locked', reason: '11158 model not allowed for account type' }];
ctx.mdRecent = [];
const deniedTag = ctx.mdAvailabilityTag(model('global:gpt-5.6-sol'));
process.stdout.write(JSON.stringify({
  cn: ctx.mdAvailabilityOf(model('cn:glm-5.3'), [], []),
  unknown: state(model('global:unknown'), [], []),
  denied: state(model('global:gpt-5.6-sol'), [{ realm: 'global', model: 'gpt-5.6-sol', state: 'locked', reason: '11158 model not allowed for account type' }], []),
  missing: state(model('global:auto-chat'), [{ realm: 'global', model: 'auto-chat', state: 'locked', reason: '11102 service info not found' }], []),
  partial: state(model('global:glm-5.3'), [{ realm: 'global', model: 'glm-5.3', state: 'partial', reason: '6004 model rate limit' }], []),
  starved: state(model('global:glm-5.3'), [{ realm: 'global', model: 'glm-5.3', state: 'starved' }], []),
  rate: state(model('global:glm-5.3'), [], [{ model: 'global:glm-5.3', status: 429 }]),
  upstream: state(model('global:glm-5.3'), [], [{ model: 'global:glm-5.3', status: 503 }]),
  ok: state(model('global:ok'), [], success),
  deniedTag: deniedTag.includes('无权限') && deniedTag.includes('tag bad'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "model-availability-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("model availability node test failed: %v\n%s", err, out)
	}
	const want = `{"cn":null,"unknown":"unknown","denied":"denied","missing":"missing","partial":"partial","starved":"starved","rate":"rate","upstream":"upstream","ok":"ok","deniedTag":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("model availability=%s want %s", out, want)
	}
}

// 用量时序图的柱体类名不得叫 bar：账号池的积分条是 .bar{height:3px}，而 SVG2 里
// height 是 rect 的 CSS 几何属性——同名类会把每根柱子压成 3px 高，图看起来"没数据"。
// 这个坑只能在浏览器里看出来，所以在这里钉住类名。
func TestAppJSUsageChartBarClass(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; usage chart test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function parsePointTime');
const end = src.indexOf('function fmtTokTip');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
if ([start, end, escStart, escEnd, fmtStart, fmtEnd].some(v => v < 0)) throw new Error('usage chart helpers not found');
const host = { innerHTML: '', textContent: '' };
const ctx = {
  Date, Number, String, Math, RegExp, isNaN, Set, Array, Object, Infinity,
  document: { getElementById: () => host },
  $: () => host,
};
vm.createContext(ctx);
vm.runInContext(src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(start, end) +
  '\nthis.renderUsageChart=renderUsageChart;', ctx);
const series = [
  { t: '2026-09-30T09', scope: 'hour', prompt_tokens: 35, completion_tokens: 16, total_tokens: 51, requests: 1 },
  { t: '2026-09-30T11', scope: 'hour', prompt_tokens: 978324, completion_tokens: 20621, total_tokens: 998945, requests: 39 },
  { t: '2026-09-30T13', scope: 'hour', prompt_tokens: 27400952, completion_tokens: 104913, total_tokens: 27505865, requests: 200 },
];
ctx.renderUsageChart(series);
const svg = host.innerHTML;
process.stdout.write(JSON.stringify({
  hasUsbar: svg.includes('class="usbar"'),
  hasBareBar: /class="bar"/.test(svg),
  hasGradient: svg.includes('usGradP') && svg.includes('usGradC'),
  barCount: (svg.match(/class="usbar"/g) || []).length,
  hasPeak: svg.includes('峰值'),
  hasAvg: svg.includes('均值'),
  emptyState: (function () { ctx.renderUsageChart([]); return host.innerHTML.includes('us-empty'); })(),
}));`
	f, err := os.CreateTemp(t.TempDir(), "usage-chart-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("usage chart node test failed: %v\n%s", err, out)
	}
	const want = `{"hasUsbar":true,"hasBareBar":false,"hasGradient":true,"barCount":6,"hasPeak":true,"hasAvg":true,"emptyState":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("usage chart=%s\nwant %s", out, want)
	}
}

// 时间范围控件：预设 → 查询参数的映射。要点：
//   - 「今天」必须发浏览器本地时区的 00:00（服务端时区未必一致），且不带 to；
//   - 滚动预设 rolling=true 发 hours（服务端整点对齐），rolling=false 折算成 from；
//   - 「全部历史」两者都不发；「自定义」发用户挑的 from/to。
func TestAppJSTimeRangeQuery(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; time range test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const TRANGE_PRESETS');
const end = src.indexOf('function rateLimitMeta');
if (start < 0 || end < 0 || end < start) throw new Error('trange helpers not found');
const host = { innerHTML: '' };
const ctx = {
  Date, Number, String, Math, Map, Array, Object, isNaN, URLSearchParams,
  document: { getElementById: () => host },
  $: () => host,
  esc: s => String(s == null ? '' : s),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.trangeState=trangeState; this.trangeQuery=trangeQuery; this.trangeLabel=trangeLabel; this.trangeMidnight=trangeMidnight;', ctx);
const q = (preset, rolling) => {
  ctx.trangeState('t').preset = preset;
  return ctx.trangeQuery('t', rolling).toString();
};
const secOf = d => String(Math.floor(d.getTime() / 1000));
const approx = (qs, wantSec) => {
  const m = /(?:^|&)from=(\d+)/.exec(qs);
  return m && Math.abs(Number(m[1]) - wantSec) < 120;
};
const now = Date.now();
const todayQ = q('today', true);
process.stdout.write(JSON.stringify({
  todayIsMidnight: todayQ === 'from=' + secOf(ctx.trangeMidnight()),
  todayNoTo: !/to=/.test(todayQ),
  rolling24: q('24', true),
  rolling72: q('72', true),
  rolling0: q('0', true),
  log0: q('0', false),
  log24From: approx(q('24', false), Math.floor((now - 24 * 3600e3) / 1000)),
  log24HasHours: /hours=/.test(q('24', false)),
  log7dFrom: approx(q('168', false), Math.floor((now - 168 * 3600e3) / 1000)),
  custom: (function () {
    const st = ctx.trangeState('t');
    st.preset = 'custom';
    st.from = new Date(2026, 8, 30, 9, 0, 0);
    st.to = new Date(2026, 8, 30, 18, 30, 0);
    return ctx.trangeQuery('t', true).toString();
  })(),
  labelCustom: ctx.trangeLabel('t'),
  labelToday: (function () { ctx.trangeState('t').preset = 'today'; return ctx.trangeLabel('t'); })(),
  // 用量页不带 preset 绑定时必须落到「今天」（TRANGE_DEFAULT），不是近 3 天。
  defaultPreset: ctx.trangeState('brand-new-host').preset,
}));`
	f, err := os.CreateTemp(t.TempDir(), "trange-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("time range node test failed: %v\n%s", err, out)
	}
	local := func(h, m int) string {
		return strconv.FormatInt(time.Date(2026, 9, 30, h, m, 0, 0, time.Local).Unix(), 10)
	}
	want := `{"todayIsMidnight":true,"todayNoTo":true,` +
		// rolling0 必须是 "hours=0"（显式全部历史）。此前期望值是空串——那恰好把
		// issue #121 的错误行为固化成了断言：空 query 会被后端的 72 小时缺省接管，
		// 于是「全部历史」显示成「近 3 天」。
		// log0 仍为空：请求记录端点没有缺省窗口，不传 from/to 就是全部历史。
		`"rolling24":"hours=24","rolling72":"hours=72","rolling0":"hours=0","log0":"",` +
		`"log24From":true,"log24HasHours":false,"log7dFrom":true,` +
		`"custom":"from=` + local(9, 0) + `&to=` + local(18, 30) + `",` +
		`"labelCustom":"9-30 09:00 → 9-30 18:30","labelToday":"今天","defaultPreset":"today"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("time range=%s\nwant %s", out, want)
	}
}

// 配置表单与 CFG_MAP 必须一一对应，且面板声称"可在线改"的热生效键必须真的
// 出现在表单里。
//
// 为什么需要：`logging.request_client_info` 曾经在表单里存在过，后来在某次改动中
// 被连带删掉，而 Go 侧的配置键、livecfg 热生效通路、README 的描述都还在——面板
// 少了一个开关而 Go 测试全绿，只有人肉点开配置页才会发现。这里把"表单字段 ↔
// CFG_MAP"与"关键热改键必须在表单里"两条都钉住。
func TestConfigFormMatchesCFGMap(t *testing.T) {
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(src)
	htmlBytes, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)

	// CFG_MAP 块（下面两条检查共用）。
	mapBlock := js[strings.Index(js, "const CFG_MAP = {"):]
	mapBlock = mapBlock[:strings.Index(mapBlock, "\n};")]
	// 不能按行首匹配：CFG_MAP 里多个键写在同一行（`a: [...], b: [...]`），只有行首
	// 那个带换行缩进。按「前面是行首或分隔符」判定才不漏。
	inMap := func(name string) bool {
		return regexp.MustCompile(`(?:^|[\s,{])` + regexp.QuoteMeta(name) + `:\s*\[`).MatchString(mapBlock)
	}

	// 1) 表单里的每个 name 都要有 CFG_MAP 条目（否则收集/回填都拿不到它）。
	form := html[strings.Index(html, `<form id="cfgForm">`):]
	form = form[:strings.Index(form, "</form>")]
	names := map[string]bool{}
	for _, m := range regexp.MustCompile(`name="([a-z_0-9]+)"`).FindAllStringSubmatch(form, -1) {
		names[m[1]] = true
	}
	if len(names) == 0 {
		t.Fatal("未从配置表单解析出任何 name 字段")
	}
	for n := range names {
		if !inMap(n) {
			t.Errorf("表单字段 %q 在 CFG_MAP 里没有条目（保存时会被静默丢弃）", n)
		}
	}

	// 2) CFG_MAP 里的每个键都要在表单里有控件（否则回填/保存是空转）。
	for _, m := range regexp.MustCompile(`(?:^|[\s,{])([a-z_0-9]+):\s*\[`).FindAllStringSubmatch(mapBlock, -1) {
		if !names[m[1]] {
			t.Errorf("CFG_MAP 键 %q 在配置表单里没有对应控件", m[1])
		}
	}

	// 3) 明确断言这一个键：后端有配置项、README 说面板可改，UI 不能少。
	if !strings.Contains(js, "request_client_info: ['logging', 'request_client_info']") {
		t.Error("CFG_MAP 缺 request_client_info 条目")
	}
	if !names["request_client_info"] {
		t.Error("配置表单缺「记录调用来源」开关（logging.request_client_info）")
	}
}

// 同到期时间按面额降序；其余未用完包与零/负余额包分别聚合。
func TestAppJSDetailGroups(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; detail groups test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const PK_DEFAULT_DETAIL_LIMIT');
const end = src.indexOf('function renderPackages');
if (start < 0 || end < 0) throw new Error('detail group functions not found');
const ctx = { Date, Math, Number, String, Map, Array, Object, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.pkDetailGroups = pkDetailGroups; this.pkDetailLimit = pkDetailLimit;', ctx);
const input = [
  { id: 'small-late', size: 100, remain: 1, expires_at: 400 },
  { id: 'zero-early-b', size: 200, remain: 0, expires_at: 200 },
  { id: 'small-early', size: 100, remain: 2, expires_at: 200 },
  { id: 'large-unknown', size: 300, remain: 3, end_time: '' },
  { id: 'zero-early-a', size: 200, remain: -1, expires_at: 200 },
  { id: 'small-unknown', size: 100, remain: 1, end_time: '' },
  { id: 'large-early', size: 300, remain: 4, expires_at: 200 },
  { id: 'zero-late', size: 300, remain: 0, expires_at: 300 },
];
const before = input.map(p => p.id).join(',');
const out = ctx.pkDetailGroups(input, 2);
process.stdout.write(JSON.stringify({
  visible: out.visible.map(p => p.id),
  rest: out.rest.map(p => p.id),
  used: out.used.map(p => p.id),
  restSize: out.restSize,
  restRemain: out.restRemain,
  usedSize: out.usedSize,
  defaultLimit: ctx.pkDetailLimit({}),
  configuredLimit: ctx.pkDetailLimit({ panel: { package_detail_limit: 7 } }),
  unchanged: input.map(p => p.id).join(',') === before,
}));`
	f, err := os.CreateTemp(t.TempDir(), "detail-groups-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("detail groups node test failed: %v\n%s", err, out)
	}
	const want = `{"visible":["large-early","small-early"],"rest":["small-late","large-unknown","small-unknown"],"used":["zero-early-b","zero-early-a","zero-late"],"restSize":500,"restRemain":5,"usedSize":700,"defaultLimit":5,"configuredLimit":7,"unchanged":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("detail groups=%s want %s", out, want)
	}
}

// 精确剩余天数聚合、账号内按总余额钳制、无到期批次不进入图表。
func TestAppJSExpirySummary(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry summary test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const PK_ACCOUNT_COLORS');
const end = src.indexOf('function renderExpiryDistribution');
if (start < 0 || end < 0) throw new Error('expiry summary functions not found');
const ctx = { Date, Math, Number, String, Map, Array, Object, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.summarizeCreditDays = summarizeCreditDays; this.pkAccountColorMap = pkAccountColorMap;', ctx);
const day = 86400000, now = 100000;
const out = ctx.summarizeCreditDays([
  { uid: 'a', remain: 100, packages: [
    { name: 'soon-a', remain: 30, expires_at: now + day },
    { name: 'later', remain: 70, expires_at: now + 7 * day },
  ] },
  { uid: 'b', remain: 55, packages: [
    { name: 'soon-b', remain: 20, expires_at: now + day },
    { name: 'unknown', remain: 5, end_time: '' },
  ] },
  { uid: 'err', error: 'offline' },
], now);
process.stdout.write(JSON.stringify({
  rows: out.rows.map(row => ({ days: row.days, credits: row.credits })),
  accountCount: out.accountCount,
  unavailable: out.unavailable,
  colorA: ctx.pkAccountColorMap([{ uid: 'b' }, { uid: 'a' }]).get('a'),
  colorB: ctx.pkAccountColorMap([{ uid: 'a' }, { uid: 'b' }]).get('b'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "expiry-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry summary node test failed: %v\n%s", err, out)
	}
	const want = `{"rows":[{"days":1,"credits":50},{"days":7,"credits":70}],"accountCount":3,"unavailable":1,"colorA":"#4f8cff","colorB":"#25b08b"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("expiry summary=%s want %s", out, want)
	}
}

// TestAppJSCollectConfigClearable 钉住 collectConfig 的空串语义。
//
// 覆盖型字段（user_agent / prompt_file）空串必须照发：漏发会让面板显示"已保存"
// 而 config.json 里的值没变（issue #102 附带发现 2）。
//
// 同时钉住反面：其余文本字段空串仍然不下发。这条同样重要——若哪天为了修上面那个
// 问题改成"所有空串都发"，表单里任何一个没填的框都会变成"请清空"，静默抹掉配置。
func TestAppJSCollectConfigClearable(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; collectConfig test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const CFG_MAP');
const end = src.indexOf('/* Go 时长字段即时校验');
if (start < 0 || end < 0 || end < start) throw new Error('collectConfig region not found');
const mk = v => ({ type: 'text', value: v });
const cfgForm = { elements: {
  listen: mk(''),
  api_key: mk('secret'),
  user_agent: mk(''),
  prompt_file: mk(''),
  checkin_hours: mk(''),
}};
const ctx = {
  Date, Number, String, Math, Map, Array, Object, isNaN, URLSearchParams, Set,
  document: { getElementById: id => (id === 'cfgForm' ? cfgForm : null) },
  $: id => (id === 'cfgForm' ? cfgForm : null),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.collectConfig = collectConfig;', ctx);
const out = ctx.collectConfig();
const has = (o, k) => Object.prototype.hasOwnProperty.call(o || {}, k);
process.stdout.write(JSON.stringify([
  has(out.upstream, 'user_agent'), (out.upstream || {}).user_agent,
  has(out.prompt, 'file'), (out.prompt || {}).file,
  has(out, 'listen'),
  has(out.schedule, 'checkin_hours'),
  out.api_key
]));`
	f, err := os.CreateTemp(t.TempDir(), "cfgc-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("collectConfig node test failed: %v\n%s", err, out)
	}
	// [user_agent 已发, 其值, prompt.file 已发, 其值, listen 未发, checkin_hours 未发, api_key]
	const want = `[true,"",true,"",false,false,"secret"]`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("collectConfig=%s want %s", strings.TrimSpace(string(out)), want)
	}
}

// TestAppJSExpirySortedByExpiry 到期提醒按「最近到期」升序，且不分域（issue #125）。
//
// 后端 /panel/api/packages 是按**余额降序**返回的，恰好把 CN 账号都排在前面、
// global 排在末尾，看上去像"按域分组"，实际只是余额顺序。本用例刻意按这个形态构造
// 输入（余额高的到期最晚、global 余额最低），若前端不重排就会保持该顺序而失败。
//
// 日期按「今天 +N 天」生成而不是写死：写死的话过了那天用例就会自己失效。
func TestAppJSExpirySortedByExpiry(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry sort test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function expBatches');
const end = src.indexOf('async function loadExpiry');
if (start < 0 || end < 0 || end < start) throw new Error('expiry region not found');
const sink = { innerHTML: '', textContent: '', hidden: true };
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  $: () => sink,
  Date, Math, Number, String, Map, Array, Object, isNaN,
  lastPackages: null, lastPackagesAt: 0,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderExpiry = renderExpiry;', ctx);
const iso = n => {
  const d = new Date(); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() + n);
  const p = x => String(x).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate());
};
const pack = (days, amount) => ({ end_time: iso(days) + ' 00:00:00', remain: amount });
// 模拟后端顺序：余额降序 → CN 高余额在前且到期最晚，global 最少且最快到期。
const d = { accounts: [
  { uid: 'a', nickname: 'cn-late',  realm: 'cn',     packages: [pack(16, 900)] },
  { uid: 'b', nickname: 'cn-mid',   realm: 'cn',     packages: [pack(12, 700)] },
  { uid: 'c', nickname: 'gl-soon',  realm: 'global', packages: [pack(2, 500)] },
  { uid: 'd', nickname: 'no-expiry', realm: 'cn',    packages: [pack(-3, 100)] },
  { uid: 'e', nickname: 'broken',   realm: 'cn',     error: 'offline' },
] };
ctx.renderExpiry(d);
const names = [...sink.innerHTML.matchAll(/<span class="exp-nm">([^<]*)<\/span>/g)].map(m => m[1]);
process.stdout.write(JSON.stringify(names));`
	f, err := os.CreateTemp(t.TempDir(), "expsort-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry sort node test failed: %v\n%s", err, out)
	}
	// 最快到期的排最前（跨域）；无 7 天内到期的与查询失败的排最后。
	const want = `["gl-soon","cn-mid","cn-late","no-expiry","broken"]`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("expiry order=%s\nwant %s", strings.TrimSpace(string(out)), want)
	}
}

// TestAppJSExpirySnapshotRenders 到期卡片用 overview 快照即可渲染（issue #137）。
//
// 此前的死锁：卡片在 index.html 上写死 hidden，而唯一解除隐藏的语句在
// renderExpiry() 末尾——该函数只有点「检查」才被调用。按钮本身就在隐藏卡片里，
// 用户看不见也点不到，于是"积分到期提醒"这个功能等于不存在。
//
// 修复引入 renderExpirySnapshot()：吃 /overview 响应里的 credits_earliest_expiry /
// credits_earliest_remaining（state.json 现成数据，零上游请求），并显式设
// expBox.hidden = false。本用例钉住两件事：
//  1. 只给 overview 形态的输入（没有 packages 字段）也能出内容；
//  2. 渲染后 expBox.hidden 必须为 false——把这句删掉（还原 bug）测试就必须 FAIL。
func TestAppJSExpirySnapshotRenders(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry snapshot test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function renderExpirySnapshot');
const end = src.indexOf('async function loadExpiry');
if (start < 0 || end < 0 || end < start) throw new Error('expiry snapshot region not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '', hidden: true });
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  $: id => (sinks[id] || mk(id)),
  // 卡片可见性由 expBoxShow() 按当前 tab 决定；切片环境没有 tab 状态，桩成
  // "渲染即显示"，与下方 visible 断言口径一致。
  expBoxShow: () => { (sinks['expBox'] || mk('expBox')).hidden = false; },
  // 到期列表分页状态（模块级 let，切片环境需要给初值）。
  expAll: [], expPage: 1, expPageSize: 100,
  Date, Math, Number, String, Map, Array, Object, isNaN,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderExpirySnapshot = renderExpirySnapshot;', ctx);
const day = n => {
  const d = new Date(); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() + n);
  return d.toISOString();
};
// overview 形态：没有 packages 数组，只有到期快照字段。
const d = { accounts: [
  { uid: 'aaaaaaaa-1', nickname: '秋',     credits: 4099,
    credits_earliest_expiry: day(2),  credits_earliest_remaining: 499 },
  { uid: 'bbbbbbbb-2', nickname: '秋千易', credits: 3243,
    credits_earliest_expiry: day(30), credits_earliest_remaining: 43 },
  { uid: 'cccccccc-3', nickname: '长期包', credits: 900,
    credits_earliest_expiry: null,    credits_earliest_remaining: 0 },
] };
ctx.renderExpirySnapshot(d);
const html = sinks['expList'] ? sinks['expList'].innerHTML : '';
const names = [...html.matchAll(/<span class="exp-nm">([^<]*)<\/span>/g)].map(m => m[1]);
// 2 天到期的应排最前（红），30 天其次，无到期信息的排最后。
const wantNames = JSON.stringify(['秋', '秋千易', '长期包']);
const gotNames = JSON.stringify(names);
process.stdout.write(JSON.stringify({
  visible: sinks['expBox'] ? sinks['expBox'].hidden === false : false,
  rows: names.length,
  order: gotNames === wantNames,
  got: gotNames,
  want: wantNames,
}));`
	f, err := os.CreateTemp(t.TempDir(), "expsnap-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry snapshot node test failed: %v\n%s", err, out)
	}
	const want = `{"visible":true,"rows":3,"order":true,"got":"[\"秋\",\"秋千易\",\"长期包\"]","want":"[\"秋\",\"秋千易\",\"长期包\"]"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("expiry snapshot=%s\nwant %s（visible 必须为 true，否则卡片仍隐藏）",
			strings.TrimSpace(string(out)), want)
	}
}

// TestAppJSExpirySnapshotMatchesLive 钉住「打开面板看到的」与「点检查后看到的」逐字节
// 一致（issue：首页快照与检查用了两套到期口径）。
//
// 做法：给同一逻辑数据的两条形态——packages（检查）与 credits_expiry_buckets（overview
// 快照）——分别渲染，比较 expList.innerHTML。相等意味着排序、日期、该批合计、日均
// 需耗、7 天内合计、随后批次全部一致，而不是只对齐了某一个字段。
func TestAppJSExpirySnapshotMatchesLive(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry parity test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function expBatches');
const end = src.indexOf('async function loadExpiry');
if (start < 0 || end < 0 || end < start) throw new Error('expiry region not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', children: [], textContent: '', hidden: true });
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  $: id => (sinks[id] || mk(id)),
  Date, Math, Number, String, Map, Array, Object, isNaN,
  lastPackages: null, lastPackagesAt: 0,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderExpiry = renderExpiry; this.renderExpirySnapshot = renderExpirySnapshot;', ctx);
const iso = n => {
  const d = new Date(); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() + n);
  const p = x => String(x).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate());
};
// 同一天多个包：检查侧是 3 + 100 + 80，快照侧已聚合成 180。
const live = { accounts: [
  { uid: 'a', nickname: '同日多批', packages: [
    { end_time: iso(0) + 'T09:00:00', remain: 3 },
    { end_time: iso(1) + 'T09:00:00', remain: 100 },
    { end_time: iso(1) + 'T12:00:00', remain: 80 },
    { end_time: iso(2) + 'T09:00:00', remain: 50 },
  ] },
  { uid: 'b', nickname: '远期', packages: [
    { end_time: iso(16) + 'T22:25:00', remain: 1600 },
    { end_time: iso(17) + 'T09:00:00', remain: 400 },
  ] },
  { uid: 'c', nickname: '无到期', packages: [] },
  { uid: 'd', nickname: '明天', packages: [{ end_time: iso(1) + 'T10:00:00', remain: 77 }] },
] };
const snap = { accounts: [
  { uid: 'b', nickname: '远期', credits_expiry_buckets: [
    { date: iso(16), remain: 1600 },
    { date: iso(17), remain: 400 },
  ] },
  { uid: 'a', nickname: '同日多批', credits_expiry_buckets: [
    { date: iso(2), remain: 50 },
    { date: iso(1), remain: 180 },
    { date: iso(0), remain: 3 },
  ] },
  { uid: 'c', nickname: '无到期', credits_expiry_buckets: [] },
  { uid: 'd', nickname: '明天', credits_expiry_buckets: [{ date: iso(1), remain: 77 }] },
] };
ctx.renderExpiry(live);
const liveHTML = sinks['expList'].innerHTML;
sinks['expList'].innerHTML = '';
ctx.renderExpirySnapshot(snap);
const snapHTML = sinks['expList'].innerHTML;
process.stdout.write(JSON.stringify({
  equal: liveHTML === snapHTML,
  today: liveHTML.includes('今天到期'),
  tomorrow: liveHTML.includes('明天到期'),
  weekSum: liveHTML.includes('7 天内合计'),
  len: liveHTML.length,
}));`
	f, err := os.CreateTemp(t.TempDir(), "expparity-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry parity node test failed: %v\n%s", err, out)
	}
	const want = `{"equal":true,"today":true,"tomorrow":true,"weekSum":true,"len":`
	got := strings.TrimSpace(string(out))
	if !strings.HasPrefix(got, want) {
		t.Fatalf("expiry parity=%s\nwant prefix %s（快照与检查必须逐字节一致）", got, want)
	}
}

// TestAppJSExpirySnapshotNoteCadence 到期卡片必须如实标注数据节奏与年龄。
// 界面本身 5 秒轮询一次 overview，但卡片数据只随余额后台刷新更新（默认每 5 分钟）；
// 不标注的话，5 秒级的界面刷新会让人误以为数据也是 5 秒级新鲜。
func TestAppJSExpirySnapshotNoteCadence(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry note test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
// 从 expBatches 切起：renderExpirySnapshot 的按日批次分支要调 renderExpiry，
// 只切它自己会 ReferenceError。
const start = src.indexOf('function expBatches');
const end = src.indexOf('async function loadExpiry');
if (start < 0 || end < 0 || end < start) throw new Error('expiry region not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', children: [], textContent: '', hidden: true });
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  $: id => (sinks[id] || mk(id)),
  Date, Math, Number, String, Map, Array, Object, isNaN,
  lastPackages: null, lastPackagesAt: 0,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderExpirySnapshot = renderExpirySnapshot;', ctx);
const refreshedAt = new Date(Date.now() - 3 * 60000).toISOString();
ctx.renderExpirySnapshot({ accounts: [
  { uid: 'a', nickname: 'n', credits_expiry_buckets: [{ date: '2099-01-01', remain: 1 }] },
], balance_refresh_sec: 300, balance_refreshed_at: refreshedAt });
const withCadence = sinks['expNote'].textContent;
ctx.renderExpirySnapshot({ accounts: [
  { uid: 'a', nickname: 'n', credits_expiry_buckets: [{ date: '2099-01-01', remain: 1 }] },
] });
const withoutCadence = sinks['expNote'].textContent;
process.stdout.write(JSON.stringify({
  cadence: withCadence.includes('每 5 分钟后台刷新') && withCadence.includes('3 分钟前更新'),
  noCadence: withoutCadence.includes('未开启后台刷新'),
  note: withCadence,
}));`
	f, err := os.CreateTemp(t.TempDir(), "expnote-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry note node test failed: %v\n%s", err, out)
	}
	const want = `{"cadence":true,"noCadence":true,"note":`
	if !strings.HasPrefix(strings.TrimSpace(string(out)), want) {
		t.Fatalf("expiry note=%s\nwant prefix %s", strings.TrimSpace(string(out)), want)
	}
}

// TestPanelIndexExpiryBoxNotHidden 到期卡片在 HTML 骨架里不得带 hidden 属性（issue #137）。
//
// 与上一个用例互补：app.js 侧有 renderExpirySnapshot 兜底，HTML 侧若仍写死 hidden，
// 用户在首次 overview 返回前（以及 JS 加载失败时）看到的依然是空白。这个断言就是
// 那个曾经卡住整个功能的属性——把它加回去测试必须 FAIL。
func TestPanelIndexExpiryBoxNotHidden(t *testing.T) {
	b, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	// 定位到期卡片那一行，只检查它自己，避免误伤其他合法的 hidden 元素。
	idx := strings.Index(string(b), `id="expBox"`)
	if idx < 0 {
		t.Fatal(`index.html missing id="expBox"`)
	}
	lineStart := strings.LastIndex(string(b[:idx]), "\n") + 1
	lineEnd := strings.Index(string(b[idx:]), "\n")
	if lineEnd < 0 {
		lineEnd = len(b) - idx
	}
	line := string(b[lineStart : idx+lineEnd])
	if strings.Contains(line, "hidden") {
		t.Fatalf(`expBox 元素不得带 hidden（issue #137 死锁根因），实际行：%s`, strings.TrimSpace(line))
	}
}

// TestAppJSChartTooltipInsideBar 图表 tooltip 必须挂在每根柱子内部（issue #128）。
//
// <title> 在 SVG 里描述的是**父元素**。此前它被平铺在 <svg> 根下（<rect> 是自闭合的，
// 无法包含子节点），于是整张图共用一个 tooltip —— 浏览器取第一个 —— 悬停任何柱子都
// 显示同一份数据。这类问题不报错、不影响渲染，只靠肉眼看很容易漏。
//
// 断言结构：<g> 数量 == 数据点数，且每个 <g> 紧跟一个 <title>；同时确认没有
// 游离在 <g> 之外的 <title>。
func TestAppJSChartTooltipInsideBar(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; chart tooltip test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function parsePointTime');
const chartStart = src.indexOf('function renderUsageChart');
if (start < 0 || chartStart < 0) throw new Error('chart functions not found');
let end = src.indexOf('\nfunction ', chartStart + 10);
if (end < 0) end = src.length;
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '' });
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  $: id => (sinks[id] || mk(id)),
  Date, Math, Number, String, Map, Array, Object, isNaN, Infinity, isFinite, Set,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderUsageChart = renderUsageChart;', ctx);
const series = [];
for (let h = 0; h < 5; h++) {
  const d = new Date(); d.setHours(d.getHours() - (4 - h), 0, 0, 0);
  const iso = d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' +
              String(d.getDate()).padStart(2, '0') + 'T' + String(d.getHours()).padStart(2, '0');
  series.push({ t: iso, scope: 'hour', prompt_tokens: (h + 1) * 100,
                completion_tokens: (h + 1) * 10, total_tokens: (h + 1) * 110, requests: h + 1 });
}
ctx.renderUsageChart(series);
const svg = sinks['usChart'] ? sinks['usChart'].innerHTML : '';
const groups = svg.match(/<g><title>/g) || [];
const titles = svg.match(/<title>[^<]*<\/title>/g) || [];
// 游离的 <title>：前面不是 <g>（即仍平铺在根下）
const loose = (svg.match(/(?:<rect[^>]*\/>|<\/g>)<title>/g) || []).length;
process.stdout.write(JSON.stringify({
  points: series.length, groups: groups.length, titles: titles.length, loose: loose,
}));`
	f, err := os.CreateTemp(t.TempDir(), "charttip-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("chart tooltip node test failed: %v\n%s", err, out)
	}
	const want = `{"points":5,"groups":5,"titles":5,"loose":0}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("chart tooltip structure=%s\nwant %s（groups 应等于数据点数，loose 应为 0）",
			strings.TrimSpace(string(out)), want)
	}
}

// 账号备注：键取 uid 前 8 位（与 renderUsageDim 账号维度、积分扣除表的 uid 前缀同源，
// 三个表格因此能直接 join）；有备注时备注当主标识、原昵称作浅色虚体跟随；备注来自本机
// 输入，必须转义后才能拼进 innerHTML。
func TestAppJSRemarkName(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; remark name test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const kStart = src.indexOf('function remarkKey(');
const kEnd = src.indexOf('function storeRemarks(');
// v2.3.0 #12：备注展示改为「服务端 meta 优先、localStorage 降级」——
// remarkOf 依赖 metaFor，这里注入恒 null 的服务端查询，验证本地降级路径不变。
const rStart = src.indexOf('function remarkOf(');
const rEnd = src.indexOf('function accTagsHtml(');
if (escStart < 0 || escEnd < 0) throw new Error('esc helpers not found');
if (kStart < 0 || kEnd < 0 || rStart < 0 || rEnd < 0) throw new Error('remark helpers not found');
const ctx = { String, RegExp, Number };
vm.createContext(ctx);
vm.runInContext(
  src.slice(escStart, escEnd) + src.slice(kStart, kEnd) + src.slice(rStart, rEnd) +
  '\nconst REM = {abcdef12: "JTOL", deadbeef: "<b>x</b>"};' +
  '\nfunction metaFor() { return null; }' +
  '\nfunction remarksAll() { return REM; }' +
  '\nthis.remarkKey=remarkKey; this.remarkName=remarkName;',
  ctx
);
process.stdout.write(JSON.stringify({
  key: ctx.remarkKey('ABCDEF1234567890'),
  short: ctx.remarkKey('abc'),
  empty: ctx.remarkKey(null),
  remarked: ctx.remarkName('abcdef12', '张三'),
  plain: ctx.remarkName('zzzzzzzz', '张三'),
  escaped: ctx.remarkName('deadbeef', 'n'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "remark-name-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("remark name node test failed: %v\n%s", err, out)
	}
	const want = `{"key":"ABCDEF12","short":"abc","empty":"","remarked":"JTOL<span class=\"rm-ghost\">张三</span>","plain":"张三","escaped":"&lt;b&gt;x&lt;/b&gt;<span class=\"rm-ghost\">n</span>"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("remark name=%s\nwant %s", strings.TrimSpace(string(out)), want)
	}
}

// 「积分到期提醒」折叠：默认展开（点「检查」就得能看到结果），用户收起一次后由
// localStorage 记住、重载时恢复；折叠只切列表容器的 hidden，不影响卡片与「检查」。
func TestAppJSExpiryFold(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry fold test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function expFolded(');
const endMarker = 'applyExpFold();';
const end = src.lastIndexOf(endMarker);
if (start < 0 || end < 0) throw new Error('expiry fold helpers not found');
const slice = src.slice(start, end + endMarker.length);
function boot(stored) {
  const ctx = {};
  vm.createContext(ctx);
  vm.runInContext(
    "const LS_EXP_FOLD='wb2api.expfold.v1';" +
    "const store=" + JSON.stringify(stored) + ";" +
    "const pad={hidden:false};" +
    "const btn={textContent:'',attrs:{},setAttribute:function(k,v){this.attrs[k]=v;}};" +
    "function $(id){return id==='expPad'?pad:(id==='btnExpFold'?btn:null);}" +
    "const localStorage={getItem:function(k){return (k in store)?store[k]:null;},setItem:function(k,v){store[k]=v;}};" +
    slice +
    "\nthis.state=function(){return {hidden:pad.hidden,text:btn.textContent,aria:btn.attrs['aria-expanded'],raw:store['wb2api.expfold.v1']||null};};" +
    "\nthis.click=function(){btn.onclick();};",
    ctx
  );
  return ctx;
}
const a = boot({});
const fresh = a.state();
a.click();
const folded = a.state();
a.click();
const unfolded = a.state();
const restored = boot({'wb2api.expfold.v1': '1'}).state();
process.stdout.write(JSON.stringify({fresh: fresh, folded: folded, unfolded: unfolded, restored: restored}));`
	f, err := os.CreateTemp(t.TempDir(), "expiry-fold-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry fold node test failed: %v\n%s", err, out)
	}
	const want = `{"fresh":{"hidden":false,"text":"收起","aria":"true","raw":null},"folded":{"hidden":true,"text":"展开","aria":"false","raw":"1"},"unfolded":{"hidden":false,"text":"收起","aria":"true","raw":"0"},"restored":{"hidden":true,"text":"展开","aria":"false","raw":"1"}}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("expiry fold=%s\nwant %s", strings.TrimSpace(string(out)), want)
	}
}

// 备注与折叠的接线齐备性：少一个 id 或漏一个 localStorage 键，表现为「按钮点了没反应」，
// 而 Go 编译、JS 语法检查、顶层冒烟都发现不了（脚本能正常跑，只是挂不上）。
func TestPanelRemarkAndFoldWiring(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`id="btnRemarks"`, `id="remarkVeil"`, `id="remarkBody"`, `id="btnRemarkSave"`, `id="btnRemarkClear"`, `id="expPad"`, `id="btnExpFold"`} {
		if !bytes.Contains(html, []byte(want)) {
			t.Fatalf("index.html 缺少 %s", want)
		}
	}
	// 折叠容器默认必须可见：列表一旦带上 hidden，点「检查」就看不到结果了。
	for _, line := range strings.Split(string(html), "\n") {
		if strings.Contains(line, `id="expPad"`) && strings.Contains(line, "hidden") {
			t.Fatalf("index.html 的过期列表容器默认带 hidden：%s", strings.TrimSpace(line))
		}
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"LS_REMARKS = 'wb2api.remarks.v1'",
		"LS_EXP_FOLD = 'wb2api.expfold.v1'",
		"function remarkKey(",
		"function remarkName(",
		"function applyExpFold(",
	} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js 缺少 %s", want)
		}
	}
}

// 用实际表单函数验证地区策略回填、独立提交以及清空账号顺序。
func TestRegionSchedulingFormRoundTrip(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	script := `const fs=require('fs'), vm=require('vm'), assert=require('assert');
 const src=fs.readFileSync(process.argv[2],'utf8');
 const html=fs.readFileSync('index.html','utf8');
 const elements={};
 for(const name of ['strategy_domestic','strategy_international','domestic_account_order','international_account_order']) {
   assert(html.includes('name="'+name+'"'), 'missing form field '+name);
   elements[name]={type:name.startsWith('strategy_')?'select-one':'text',value:''};
 }
 const cfg={pool:{strategy:{domestic:'round_robin',international:'sequential_exhaust'},account_order:{domestic:['c','a'],international:['y','x']}}};
 const nodes={cfgForm:{elements},cfgPath:{},cfgNote:{}};
 const ctx={cfgLoaded:null,api:async()=>({config:cfg}),$:id=>nodes[id],markDurationFields(){},toast(message){throw Error(message)}};
 vm.createContext(ctx);
 vm.runInContext(src.slice(src.indexOf('const CFG_MAP ='),src.indexOf('/* Go 时长字段即时校验')),ctx);
 (async()=>{
   await ctx.loadConfig();
   assert.equal(elements.strategy_domestic.value,'round_robin');
   assert.equal(elements.strategy_international.value,'sequential_exhaust');
   assert.equal(elements.domestic_account_order.value,'c, a');
   elements.strategy_domestic.value='weighted_random';
   elements.international_account_order.value='';
   const result=ctx.collectConfig();
   assert.equal(result.pool.strategy.domestic,'weighted_random');
   assert.equal(result.pool.strategy.international,'sequential_exhaust');
   assert.equal(JSON.stringify(result.pool.account_order.domestic),'["c","a"]');
   assert.equal(JSON.stringify(result.pool.account_order.international),'[]');
   delete cfg.pool.strategy; await ctx.loadConfig();
   assert.equal(elements.strategy_domestic.value,'weighted_random');
   assert.equal(elements.strategy_international.value,'weighted_random');
 })().catch(e=>{console.error(e);process.exitCode=1});`
	fp := filepath.Join(t.TempDir(), "region-form.cjs")
	if err = os.WriteFile(fp, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, fp, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("form round trip: %v\n%s", err, out)
	}
}

// TestMobileJSChartTooltipInsideBar 手机端自绘图表的 tooltip 同样要挂在柱子内部（issue #128）。
//
// mobile.js 在窄屏另画一张图（440×230），不走 app.js 的 renderUsageChart：上游修好
// 桌面实现不会覆盖它。断言与 TestAppJSChartTooltipInsideBar 同构，只取文件里
// 「接管 renderUsageChart」之后的部分，DOM/全局用桩喂进去。
func TestMobileJSChartTooltipInsideBar(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; mobile chart tooltip test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const deskUsageChart');
if (start < 0) throw new Error('mobile chart override not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '' });
const ctx = {
  MOBILE_MQ: { matches: true },
  renderUsageChart: () => {},
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  fmtTokTimeLabel: p => { const d = new Date(p.t.length === 13 ? p.t + ':00:00' : p.t + 'T00:00:00');
                          return p.scope === 'day' ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0')
                                                   : String(d.getHours()).padStart(2, '0') + ':00'; },
  parsePointTime: p => { const d = new Date(p.t.length === 13 ? p.t + ':00:00' : p.t + 'T00:00:00');
                         return isNaN(d.getTime()) ? null : d.getTime(); },
  $: id => (sinks[id] || mk(id)),
  Date, Math, Number, String, Map, Array, Object, isNaN, Infinity, isFinite, Set,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start) + '\nthis.renderUsageChart = renderUsageChart;', ctx);
const series = [];
for (let h = 0; h < 5; h++) {
  const d = new Date(); d.setHours(d.getHours() - (4 - h), 0, 0, 0);
  const iso = d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' +
              String(d.getDate()).padStart(2, '0') + 'T' + String(d.getHours()).padStart(2, '0');
  series.push({ t: iso, scope: 'hour', prompt_tokens: (h + 1) * 100,
                completion_tokens: (h + 1) * 10, total_tokens: (h + 1) * 110, requests: h + 1 });
}
ctx.renderUsageChart(series);
const svg = sinks['usChart'] ? sinks['usChart'].innerHTML : '';
const groups = svg.match(/<g><title>/g) || [];
const titles = svg.match(/<title>[^<]*<\/title>/g) || [];
const loose = (svg.match(/(?:<rect[^>]*\/>|<\/g>)<title>/g) || []).length;
process.stdout.write(JSON.stringify({
  points: series.length, groups: groups.length, titles: titles.length, loose: loose,
}));`
	f, err := os.CreateTemp(t.TempDir(), "mobilecharttip-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "mobile.js").CombinedOutput()
	if err != nil {
		t.Fatalf("mobile chart tooltip node test failed: %v\n%s", err, out)
	}
	const want = `{"points":5,"groups":5,"titles":5,"loose":0}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("mobile chart tooltip structure=%s\nwant %s（groups 应等于数据点数，loose 应为 0）",
			strings.TrimSpace(string(out)), want)
	}
}

// feNode 用 node 执行一段前端「切片」脚本：脚本以 app.js 为 process.argv[2]，工作目录
// 为 internal/panel。无 node 环境时跳过（与既有前端测试同口径）。新增用例较多，把
// 「写临时脚本 + 跑 node + 失败打印输出」收敛到一处，避免每处重复十行样板。
func feNode(t *testing.T, script string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; frontend slice test skipped")
	}
	fp := filepath.Join(t.TempDir(), "fe-slice.cjs")
	if err := os.WriteFile(fp, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, fp, "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("frontend slice node test failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestAppJSCompensationRemoved 「官方补偿」前端必须彻底删除（用户明确不再需要）。
//
// 为什么用源码级断言：补偿卡片是「打开页面逐号打上游」的触发点（start() 里首查 +
// refreshVisible 每轮重渲染），删掉的功能没有可执行入口，运行时测试抓不到「删得不干净」。
// 留一处 compBox / loadCompensation 就等于给这条逐号上游链路留了复活点，故按 token
// 逐文件钉死；配套的 Go 端测试文件也必须一并消失（否则它继续引用已删函数/接口）。
func TestAppJSCompensationRemoved(t *testing.T) {
	forbidden := []string{
		"compCache", "compInFlight", "compPending", "compErrored", "compTotalPending",
		"compDeadlineText", "renderCompensation", "renderCachedCompensation", "loadCompensation",
		"btnCompRefresh", "btnCompClaimAll", "compBox", "comp-row", "compNote", "compList",
		"官方补偿", "领取补偿", "compensation",
	}
	for _, name := range []string{"app.js", "index.html", "mobile.js", "mobile.css"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, tok := range forbidden {
			if strings.Contains(text, tok) {
				t.Errorf("%s 仍残留官方补偿实现 %q（该功能已删除，勿恢复）", name, tok)
			}
		}
	}
	if _, err := os.Stat("compensation_frontend_test.go"); !os.IsNotExist(err) {
		t.Errorf("compensation_frontend_test.go 应已删除（stat err=%v）", err)
	}
}

// TestAppJSPageSliceBounds 分页切片纯函数的边界：空列表、页码越界/非数字、pageSize=0
// （全部）、整除与非整除、小数入参。这些分支在浏览器里很难手工覆盖，而算错页码的表现
// 是「翻到空页」或「最后一页丢行」——不报错，只能肉眼看，故在此逐项钉死。
func TestAppJSPageSliceBounds(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function pageSlice');
const end = src.indexOf('function accDoTaskBtn');
if (start < 0 || end < 0 || end < start) throw new Error('pagination helpers not found');
const ctx = { Number, Math, String };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.pageSlice = pageSlice; this.hitCountLabel = hitCountLabel;', ctx);
const list = [];
for (let i = 0; i < 10; i++) list.push({ n: i });
const six = [];
for (let i = 0; i < 6; i++) six.push({ n: i });
const nums = pr => pr.rows.map(r => r.n);
const pick = (pr, keys) => { const o = { rows: nums(pr) }; for (const k of keys) o[k] = pr[k]; return o; };
process.stdout.write(JSON.stringify({
  first: pick(ctx.pageSlice(list, 3, 1), ['page', 'pages', 'total', 'size']),
  last: pick(ctx.pageSlice(list, 3, 4), ['page', 'pages', 'total', 'size']),
  beyond: pick(ctx.pageSlice(list, 3, 9), ['page', 'pages']),
  zero: pick(ctx.pageSlice(list, 3, 0), ['page']),
  negative: pick(ctx.pageSlice(list, 3, -2), ['page']),
  nonnum: pick(ctx.pageSlice(list, 3, 'x'), ['page']),
  all: pick(ctx.pageSlice(list, 0, 5), ['page', 'pages', 'size']),
  exact: pick(ctx.pageSlice(six, 3, 2), ['page', 'pages']),
  empty: pick(ctx.pageSlice([], 3, 2), ['page', 'pages', 'total']),
  nullList: pick(ctx.pageSlice(null, 3, 1), ['total']),
  fracSize: pick(ctx.pageSlice(list, 2.9, 1), ['size']),
  fracPage: pick(ctx.pageSlice(list, 3, 2.9), ['page']),
  hits: {
    a: ctx.hitCountLabel(3, 10),
    b: ctx.hitCountLabel(10, 10),
    c: ctx.hitCountLabel(0, 0),
    d: ctx.hitCountLabel(2, 5, '行'),
    e: ctx.hitCountLabel(0, 3),
  },
}));`
	want := `{"first":{"rows":[0,1,2],"page":1,"pages":4,"total":10,"size":3},` +
		`"last":{"rows":[9],"page":4,"pages":4,"total":10,"size":3},` +
		`"beyond":{"rows":[9],"page":4,"pages":4},` +
		`"zero":{"rows":[0,1,2],"page":1},` +
		`"negative":{"rows":[0,1,2],"page":1},` +
		`"nonnum":{"rows":[0,1,2],"page":1},` +
		`"all":{"rows":[0,1,2,3,4,5,6,7,8,9],"page":1,"pages":1,"size":0},` +
		`"exact":{"rows":[3,4,5],"page":2,"pages":2},` +
		`"empty":{"rows":[],"page":1,"pages":1,"total":0},` +
		`"nullList":{"rows":[],"total":0},` +
		`"fracSize":{"rows":[0,1],"size":2},` +
		`"fracPage":{"rows":[3,4,5],"page":2},` +
		`"hits":{"a":"命中 3 / 10 条","b":"10 条","c":"","d":"命中 2 / 5 行","e":"命中 0 / 3 条"}}`
	if got := feNode(t, script); got != want {
		t.Fatalf("pageSlice 边界=%s\nwant %s", got, want)
	}
}

// TestAppJSAccountFilterAndPager 账号池本地筛选（兼容路径）与冷却判定。
//
// v2.1 #1 后账号池主路径改为服务端筛选/分页，但 renderAccounts(array) 的本地
// 筛选分支仍被 node harness / 旧调用使用；这里钉住它的行为不回退。
// 同时钉住 v2.1 的倒计时改造：light 投影没有 cool_remaining_sec，冷却必须能由
// 绝对时间 until/breaker_until/degrade_until 本地算出来（否则 304 缓存下倒计时冻结）。
func TestAppJSAccountFilterAndPager(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function secUntil');
const end = src.indexOf('function renderAccounts');
if (start < 0 || end < 0 || end < start) throw new Error('account filter helpers not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '', disabled: false });
const ctx = {
  Date, Number, String, Array, Object, Math, isNaN,
  $: id => (sinks[id] || mk(id)),
  accAll: [], accFilters: { q: '', state: '', ver: '' }, accPage: 1,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.accMatch = accMatch; this.updateAccPager = updateAccPager;' +
  ' this.accCoolRemaining = accCoolRemaining; this.accCoolKind = accCoolKind;' +
  ' this.updateAccPagerMeta = updateAccPagerMeta;', ctx);
const now = Date.now();
const iso = ms => new Date(ms).toISOString();
const rows = [
  { uid: 'uid-ok', nickname: 'Alpha', realm: 'cn' },
  { uid: 'uid-cool', nickname: 'Beta', realm: 'global', cool_remaining_sec: 120 },
  { uid: 'uid-until', nickname: 'Zeta', realm: 'cn', until: iso(now + 90000) },
  { uid: 'uid-paused', nickname: 'Gamma', enterprise: true, paused: true },
  { uid: 'uid-dis', nickname: 'Delta', disabled: true },
  { uid: 'uid-ent', nickname: 'Eps', enterprise: true },
];
ctx.accAll = rows.slice();
const pick = (q, state, ver) => {
  ctx.accFilters = { q: q || '', state: state || '', ver: ver || '' };
  return rows.filter(r => ctx.accMatch(r)).map(r => r.uid);
};
const out = {
  all: pick('', '', ''),
  qNick: pick('alpha'),
  qUid: pick('UID-COOL'),
  stateOk: pick('', 'ok'),
  stateCool: pick('', 'cool'),
  statePaused: pick('', 'paused'),
  stateDisabled: pick('', 'disabled'),
  verEnterprise: pick('', '', 'enterprise'),
  verGlobal: pick('', '', 'global'),
  verCn: pick('', '', 'cn'),
  combo: pick('', 'ok', 'cn'),
};
ctx.accPage = 1;
ctx.updateAccPager(2, 3);
out.pager = [sinks.accCount.textContent, sinks.accPageInfo.textContent, sinks.accPrev.disabled, sinks.accNext.disabled];
ctx.accPage = 3;
ctx.updateAccPager(5, 3);
out.pagerLast = [sinks.accCount.textContent, sinks.accPageInfo.textContent, sinks.accPrev.disabled, sinks.accNext.disabled];
// 服务端分页计数：total/filtered 用响应值，不再数当前页。
ctx.updateAccPagerMeta({ total: 1146, filtered: 23, page: 2, pages: 12 });
out.serverPager = [sinks.accCount.textContent, sinks.accPageInfo.textContent, sinks.accPrev.disabled, sinks.accNext.disabled];
// 绝对时间倒计时（无 cool_remaining_sec）：服务端 now 作为基准。
out.coolUntil = {
  until: Math.round(ctx.accCoolRemaining(rows[2], now)),
  legacy: Math.round(ctx.accCoolRemaining(rows[1], now)),
  expired: Math.round(ctx.accCoolRemaining({ until: iso(now - 60000) }, now)),
  breaker: Math.round(ctx.accCoolRemaining({ breaker_until: iso(now + 30000) }, now)),
  degrade: Math.round(ctx.accCoolRemaining({ degrade_until: iso(now + 45000) }, now)),
  none: Math.round(ctx.accCoolRemaining({}, now)),
};
out.kinds = [
  ctx.accCoolKind({ breaker_until: iso(now + 30000) }, now),
  ctx.accCoolKind({ degrade_until: iso(now + 45000) }, now),
  ctx.accCoolKind({ until: iso(now + 10000), cool_kind: 'hard_credit' }, now),
  ctx.accCoolKind({ until: iso(now + 10000) }, now),
];
process.stdout.write(JSON.stringify(out));`
	want := `{"all":["uid-ok","uid-cool","uid-until","uid-paused","uid-dis","uid-ent"],` +
		`"qNick":["uid-ok"],"qUid":["uid-cool"],` +
		`"stateOk":["uid-ok","uid-ent"],"stateCool":["uid-cool","uid-until"],` +
		`"statePaused":["uid-paused"],"stateDisabled":["uid-dis"],` +
		`"verEnterprise":["uid-paused","uid-ent"],"verGlobal":["uid-cool"],` +
		`"verCn":["uid-ok","uid-until","uid-dis"],"combo":["uid-ok"],` +
		`"pager":["共 6 个 · 筛出 2","1 / 3",true,false],` +
		`"pagerLast":["共 6 个 · 筛出 5","3 / 3",false,true],` +
		`"serverPager":["命中 23 / 共 1146 个","2 / 12",false,false],` +
		`"coolUntil":{"until":90,"legacy":120,"expired":0,"breaker":30,"degrade":45,"none":0},` +
		`"kinds":["熔断","连败降权","积分冷却","限流冷却"]}`
	if got := feNode(t, script); got != want {
		t.Fatalf("账号池筛选/分页/倒计时=%s\nwant %s", got, want)
	}
}

// TestAppJSLogFilter 运行日志关键词筛选：只对 text 做大小写不敏感包含匹配，频道过滤
// 与关键词取交集；空关键词/脏数据不得抛异常。频道 chips 与 5s 轮询继续沿用原有协议。
func TestAppJSLogFilter(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function logMatch');
const end = src.indexOf('$(\'logChips\')');
if (start < 0 || end < 0 || end < start) throw new Error('log filter helpers not found');
const ctx = {};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.filterLogs = filterLogs;', ctx);
const entries = [
  { ch: 'task', text: '签到完成 uid=abc' },
  { ch: 'task', text: '任务执行失败：HTTP 502' },
  { ch: 'chat', text: 'chat ok 200' },
  { ch: 'sys', text: '启动 warn 冷却' },
];
const n = (ch, q) => ctx.filterLogs(entries, ch, q).length;
process.stdout.write(JSON.stringify({
  all: n('all', ''),
  task: n('task', ''),
  taskFail: n('task', '失败'),
  caseInsensitive: n('all', 'CHAT'),
  textOnly: n('all', '签到'),
  miss: n('all', '不存在'),
  chAndKw: n('chat', '失败'),
  emptyEntries: ctx.filterLogs(null, 'all', '').length,
  dirty: ctx.filterLogs([null, { ch: 'task' }], 'all', '').length,
}));`
	want := `{"all":4,"task":2,"taskFail":1,"caseInsensitive":1,"textOnly":1,"miss":0,"chAndKw":0,"emptyEntries":0,"dirty":1}`
	if got := feNode(t, script); got != want {
		t.Fatalf("log filter=%s\nwant %s", got, want)
	}
}

// TestAppJSRequestFilterAndPage 请求记录：筛选结果计数 + 客户端分页（只渲染当前页，
// 页码按命中数算而不是按拉取总数算——否则「命中 1 条」还会显示 10 页空页）。
func TestAppJSRequestFilterAndPage(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const pageStart = src.indexOf('function pageSlice');
const pageEnd = src.indexOf('function accDoTaskBtn');
const start = src.indexOf('function reqMatch');
const end = src.indexOf('function reqOutcomeTag');
if ([pageStart, pageEnd, start, end].some(v => v < 0) || pageEnd < pageStart || end < start)
  throw new Error('request filter/page helpers not found');
const ctx = { Number, Math, String, Array, Object };
vm.createContext(ctx);
vm.runInContext(src.slice(pageStart, pageEnd) + src.slice(start, end) +
  '\nthis.reqMatch = reqMatch; this.filterReqEntries = filterReqEntries; this.pageSlice = pageSlice;', ctx);
const base = { status: 200, outcome: 'success' };
const rows = [
  Object.assign({}, base, { request_id: 'req-1', client_ip: '203.0.113.7', user_agent: 'python-requests/2.31.0', model: 'cn:glm-5.2', account: '甲' }),
  { status: 502, outcome: 'http_error', request_id: 'req-2', client_ip: '198.51.100.4', user_agent: 'Mozilla/5.0', model: 'global:hy3', account: '乙' },
  { status: 200, outcome: 'stream_error', request_id: 'req-3', client_ip: '203.0.113.9', user_agent: 'curl/8', model: 'cn:glm-5.2', account: '甲' },
  { status: 200, outcome: 'interrupted', request_id: 'req-4', client_ip: '198.51.100.8', user_agent: 'python-requests/2.31.0', model: 'cn:hy3', account: '丙' },
  Object.assign({}, base, { request_id: 'req-5', client_ip: '203.0.113.10', user_agent: 'python-requests/2.31.0', model: 'cn:glm-5.2', account: '乙' }),
];
const f = (q, outcome) => ({ q: q || '', outcome: outcome || '' });
const ids = flt => rows.filter(e => ctx.reqMatch(e, flt)).map(e => e.request_id);
const matched = ctx.filterReqEntries(rows, f('', ''));
const pr = ctx.pageSlice(matched, 2, 3);
const http = ctx.filterReqEntries(rows, f('', 'http_error'));
process.stdout.write(JSON.stringify({
  all: ids(f('', '')),
  qGlm: ids(f('glm')),
  qIp: ids(f('198.51')),
  outcome: ids(f('', 'http_error')),
  combined: ids(f('203.0.113', 'success')),
  page3: { rows: pr.rows.map(e => e.request_id), page: pr.page, pages: pr.pages, total: pr.total },
  httpPages: ctx.pageSlice(http, 1, 1).pages,
}));`
	want := `{"all":["req-1","req-2","req-3","req-4","req-5"],` +
		`"qGlm":["req-1","req-3","req-5"],"qIp":["req-2","req-4"],` +
		`"outcome":["req-2"],"combined":["req-1","req-5"],` +
		`"page3":{"rows":["req-5"],"page":3,"pages":3,"total":5},"httpPages":1}`
	if got := feNode(t, script); got != want {
		t.Fatalf("请求记录筛选/分页=%s\nwant %s", got, want)
	}
}

// TestAppJSCreditFilterAndPagination 积分历史：账号筛选 + 分页，header 的「N 条 · 净 X」
// 跟随筛选（不是当前页），页码按筛选后结果算且越界自动钳位。
func TestAppJSCreditFilterAndPagination(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const pageStart = src.indexOf('function pageSlice');
const pageEnd = src.indexOf('function accDoTaskBtn');
const start = src.indexOf('function creditNum');
const end = src.indexOf('function creditLimitValue');
if ([escStart, escEnd, pageStart, pageEnd, start, end].some(v => v < 0) ||
    escEnd < escStart || pageEnd < pageStart || end < start) throw new Error('credit filter helpers not found');
const els = {};
const $ = id => (els[id] || (els[id] = { innerHTML: '', textContent: '', disabled: false }));
const ctx = { Date, Number, String, Math, RegExp, isNaN, $ };
vm.createContext(ctx);
vm.runInContext(src.slice(escStart, escEnd) + src.slice(pageStart, pageEnd) + src.slice(start, end) +
  '\nthis.esc = esc; this.renderCreditHistory = renderCreditHistory;' +
  'this.setCredit = (f, p, s) => { creditFilter = f; creditPage = p; creditPageSize = s; };' +
  'this.creditPageNo = () => creditPage;', ctx);
const t = new Date(2026, 8, 28, 14, 5, 6).toISOString();
const entries = [
  { time: t, uid: 'u1', account: '甲', delta: 100, before: 0, after: 100 },
  { time: t, uid: 'u2', account: '乙', delta: -30, before: 100, after: 70 },
  { time: t, uid: 'u1', account: '甲', delta: 5, before: 100, after: 105 },
  { time: t, uid: 'u2', account: '乙', delta: 20, before: 70, after: 90 },
  { time: t, uid: 'u1', account: '甲', delta: -30, before: 105, after: 75 },
];
const out = {};
const rows = () => (els.creditBody.innerHTML.match(/<tr>/g) || []).length;
ctx.setCredit({ uid: 'u1' }, 2, 2);
ctx.renderCreditHistory({ entries: entries });
out.u1Page2 = {
  rows: rows(), note: els.creditNote.textContent, count: els.creditCount.textContent,
  info: els.creditPageInfo.textContent, prev: els.creditPrev.disabled,
  next: els.creditNext.disabled, page: ctx.creditPageNo(),
};
ctx.setCredit({ uid: '' }, 1, 100);
ctx.renderCreditHistory({ entries: entries });
out.all = { rows: rows(), note: els.creditNote.textContent, count: els.creditCount.textContent, info: els.creditPageInfo.textContent };
ctx.setCredit({ uid: 'nope' }, 1, 100);
ctx.renderCreditHistory({ entries: entries });
out.none = {
  rows: rows(), note: els.creditNote.textContent, count: els.creditCount.textContent,
  noMatch: els.creditBody.innerHTML.indexOf('没有匹配的积分变动记录') >= 0,
};
ctx.setCredit({ uid: 'u1' }, 99, 2);
ctx.renderCreditHistory({ entries: entries });
out.clamp = { info: els.creditPageInfo.textContent, page: ctx.creditPageNo(), rows: rows() };
ctx.renderCreditHistory({ entries: null });
out.nullEntries = { note: els.creditNote.textContent, noMatch: els.creditBody.innerHTML.indexOf('没有匹配') >= 0 };
process.stdout.write(JSON.stringify(out));`
	want := `{"u1Page2":{"rows":1,"note":"3 条 · 净 +75","count":"命中 3 / 5 条","info":"2 / 2","prev":false,"next":true,"page":2},` +
		`"all":{"rows":5,"note":"5 条 · 净 +65","count":"5 条","info":"1 / 1"},` +
		`"none":{"rows":1,"note":"暂无积分变动记录","count":"命中 0 / 5 条","noMatch":true},` +
		`"clamp":{"info":"2 / 2","page":2,"rows":1},` +
		`"nullEntries":{"note":"暂无积分变动记录","noMatch":false}}`
	if got := feNode(t, script); got != want {
		t.Fatalf("积分历史筛选/分页=%s\nwant %s", got, want)
	}
}

// TestAppJSAccountDotaskButton 账号行「做任务」按钮：普通账号渲染 data-a="dotask"，
// 企业版不渲染（与签到/任务按钮同口径），uid 必须转义；同时钉住前端调用的接口路径。
func TestAppJSAccountDotaskButton(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const start = src.indexOf('const dotaskBusy = new Set();');
const end = src.indexOf('// 账号是否处于冷却');
if ([escStart, escEnd, start, end].some(v => v < 0) || escEnd < escStart || end < start)
  throw new Error('dotask helper not found');
const ctx = { String };
vm.createContext(ctx);
vm.runInContext(src.slice(escStart, escEnd) + src.slice(start, end) +
  '\nthis.accDoTaskBtn = accDoTaskBtn; this.markBusy = u => dotaskBusy.add(u); this.clearBusy = u => dotaskBusy.delete(u);', ctx);
const normal = ctx.accDoTaskBtn({ uid: 'uid-1' });
const bad = ctx.accDoTaskBtn({ uid: 'a"<b>' });
// 进行中（5s 轮询重渲染后）必须仍是 disabled + 「做任务中…」，且不影响其他账号。
ctx.markBusy('uid-1');
const busy = ctx.accDoTaskBtn({ uid: 'uid-1' });
const other = ctx.accDoTaskBtn({ uid: 'uid-3' });
ctx.clearBusy('uid-1');
process.stdout.write(JSON.stringify({
  normalHasBtn: normal.indexOf('data-a="dotask"') >= 0,
  normalHasUid: normal.indexOf('data-u="uid-1"') >= 0,
  normalHasLabel: normal.indexOf('做任务') >= 0,
  normalEnabled: normal.indexOf('disabled') < 0,
  busyDisabled: busy.indexOf('disabled') >= 0,
  busyLabel: busy.indexOf('做任务中…') >= 0,
  busyUidKept: busy.indexOf('data-u="uid-1"') >= 0,
  otherEnabled: other.indexOf('disabled') < 0 && other.indexOf('做任务中…') < 0,
  enterpriseEmpty: ctx.accDoTaskBtn({ uid: 'uid-2', enterprise: true }) === '',
  nilEmpty: ctx.accDoTaskBtn(null) === '',
  xssEscaped: bad.indexOf('<b>') < 0 && bad.indexOf('&lt;b&gt;') >= 0 && bad.indexOf('&quot;') >= 0,
}));`
	want := `{"normalHasBtn":true,"normalHasUid":true,"normalHasLabel":true,"normalEnabled":true,` +
		`"busyDisabled":true,"busyLabel":true,"busyUidKept":true,"otherEnabled":true,` +
		`"enterpriseEmpty":true,"nilEmpty":true,"xssEscaped":true}`
	if got := feNode(t, script); got != want {
		t.Fatalf("做任务按钮渲染=%s\nwant %s", got, want)
	}
	// 接口路径与「执行中」文案在源码里（行为测试已覆盖渲染；路径改名会让按钮静默 404）。
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/tasks/run", "做任务中…", "runAccountDotask", "a === 'dotask'"} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js 缺少「做任务」实现片段 %q", want)
		}
	}
}

// TestAppJSQueueFilter 任务中心队列：按状态 / 账号关键词筛选（空格分词 AND），
// 分组在分页后按账号重聚合。1146 账号时全部渲染会卡死，筛+页是可用性底线。
func TestAppJSQueueFilter(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function queueFlatten');
const end = src.indexOf('let queueQTimer');
if (start < 0 || end < 0 || start > end) throw new Error('queue filter helpers not found');
const ctx = { String, Number, Array, Object, Map, Math, GROWTH_TITLES: { chat_5: '聊天五次' } };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.filterQueueRows = filterQueueRows; this.queueRegroup = queueRegroup;', ctx);
const groups = [
  { uid: 'u1', nick: '甲', rows: [
    { kind: 'growth', code: 'chat_5', status: 'done' },
    { kind: 'growth', code: 'skill_1', status: 'error' },
  ] },
  { uid: 'u2', nick: '乙', rows: [{ kind: 'school', code: 'school_season', status: 'running' }] },
  { uid: 'u3', nick: '丙', rows: [{ kind: 'growth', code: 'black_cat', status: 'skipped' }] },
];
const f = (q, state) => ({ q: q || '', state: state || '' });
const n = flt => ctx.filterQueueRows(groups, flt).length;
const rg = ctx.queueRegroup(ctx.filterQueueRows(groups, f('', '')));
process.stdout.write(JSON.stringify({
  all: n(f('', '')),
  stateError: n(f('', 'error')),
  stateRunning: n(f('', 'running')),
  qNick: n(f('乙')),
  qUid: n(f('U1')),
  qCode: n(f('chat_5')),
  qTitle: n(f('聊天五次')),
  qSchool: n(f('开学')),
  multi: n(f('甲 skill')),
  multiMiss: n(f('甲 running')),
  regroup: rg.map(g => [g.uid, g.rows.length]),
}));`
	want := `{"all":4,"stateError":1,"stateRunning":1,"qNick":1,"qUid":2,"qCode":1,"qTitle":1,"qSchool":1,"multi":1,"multiMiss":0,"regroup":[["u1",2],["u2",1],["u3",1]]}`
	if got := feNode(t, script); got != want {
		t.Fatalf("任务队列筛选/分组=%s\nwant %s", got, want)
	}
}

// TestPanelPaginationWiring 分页/筛选控件必须「index.html 里有、app.js 里也绑定了」：
// 少一边的表现是「控件在那儿但点了没反应」或「JS 抓到 null」，Go 编译、JS 语法检查、
// 顶层冒烟都不会报错。断言 id 字符串双侧存在，改 id 时两边必须同步。
func TestPanelPaginationWiring(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{
		// 模型锁池
		"mlQ", "mlState", "mlRealm", "mlPageSize", "mlCount", "mlPrev", "mlPageInfo", "mlNext",
		// 任务中心队列
		"tcQ", "tcState", "tcPageSize", "qcCount", "tcPrev", "tcPageInfo", "tcNext",
		// 运行日志
		"logQ", "logPageSize", "logCount", "logPrev", "logPageInfo", "logNext",
		// 请求记录
		"reqPageSize", "reqPrev", "reqPageInfo", "reqNext",
		// 积分历史
		"creditAccount", "creditPageSize", "creditCount", "creditPrev", "creditPageInfo", "creditNext",
	}
	for _, id := range ids {
		if !bytes.Contains(html, []byte(`id="`+id+`"`)) {
			t.Errorf("index.html 缺少控件 id=%q", id)
		}
		if !bytes.Contains(js, []byte(`'`+id+`'`)) {
			t.Errorf("app.js 未绑定控件 id=%q（元素存在但事件挂不上）", id)
		}
	}
}

// TestAppJSRenderersSmoke 用惰性 DOM 桩执行整份 app.js 后，直接调用本次新增/改写的
// 渲染函数（账号池 / 模型锁池 / 运行日志 / 请求记录 / 任务队列 / 积分历史）各一次（含
// 空态与有数据态）：语法检查与顶层求值冒烟都不会进这些分支，而渲染里的
// ReferenceError / TypeError（漏声明、调错辅助函数）只在真正渲染时出现——表现是
// 页面局部空白、控制台报错，Go 侧却全绿。
func TestAppJSRenderersSmoke(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});
const sandbox = new Proxy({
  location: { hash: '#accounts' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert, getElementById: () => inert, addEventListener() {}, documentElement: inert, head: inert, body: inert, createElement: () => inert, cookie: '' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  MutationObserver: class { constructor() {} observe() {} disconnect() {} takeRecords() { return []; } },
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
const hook = '\nthis.__r = {' +
  'accounts: renderAccounts, ml: renderModelLocks, log: renderLogBox, req: renderRequestTable,' +
  'queue: renderQueue, credit: renderCreditHistory,' +
  'setLog: v => { logAll = v; }, setReq: v => { reqEntries = v; },' +
  '};';
vm.runInContext(src + hook, sandbox, { filename: 'app.js' });
const r = sandbox.__r;
const now = new Date().toISOString();
try {
  // 空态
  r.accounts([]);
  r.ml([]);
  r.setLog([]); r.log(false);
  r.setReq([]); r.req();
  r.queue([], null, '没有待办任务', '全部完成');
  r.credit({ entries: [] });
  r.credit(null);   // 积分历史不可用分支
  // 有数据态（覆盖行渲染、状态标、翻页控件写入）
  r.accounts([
    { uid: 'u-1', nickname: '甲', realm: 'cn', credits: 100, credits_total: 200, checkin_done: true,
      success_count: 3, err_total: 1, in_flight: 0, last_success: now,
      token_usage: { request_count: 5, total_tokens: 1000, last_latency_ms: 500, last_tokens_per_second: 20 },
      rate_limited_models: [] },
    { uid: 'u-2', nickname: '乙', enterprise: true, credits: -1, credits_total: -1, paused: true },
  ]);
  r.accounts(Array.from({ length: 130 }, (_, i) => ({ uid: 'u-' + i, nickname: 'n' + i, credits: i })));
  r.ml([
    { model: 'cn:glm-5.2', realm: 'cn', state: 'partial', servable: 3, total: 5, locked: 2,
      unlock_at: now, fully_unlock_at: now, reason: '限流' },
    { model: 'global:hy3', realm: 'global', state: 'locked', servable: 0, total: 5 },
  ]);
  r.setLog([{ ch: 'task', ts: now, text: '签到完成' }, { ch: 'sys', ts: now, text: 'warn 冷却' }]);
  r.log(true);
  r.setReq([
    { time: now, status: 200, outcome: 'success', model: 'cn:glm-5.2', account: '甲',
      client_ip: '1.2.3.4', user_agent: 'curl', duration_ms: 120, total_tokens: 50,
      credit_known: true, credit: 0.5, request_id: 'req-1' },
    { time: now, status: 502, outcome: 'http_error', model: 'cn:hy3', account: '乙',
      duration_ms: 9, request_id: 'req-2' },
  ]);
  r.req();
  r.queue([
    { uid: 'u-1', nick: '甲', rows: [{ kind: 'growth', code: 'chat_5', status: 'done' },
      { kind: 'growth', code: 'skill_1', status: 'error' }] },
  ], { running: true, items: [{ status: 'done' }, { status: 'error' }] });
  r.credit({ entries: [
    { time: now, uid: 'u-1', account: '甲', delta: 50, before: 50, after: 100 },
    { time: now, uid: 'u-2', account: '乙', delta: -10, before: 100, after: 90 },
  ] });
} catch (e) {
  console.log('RENDER FAIL:', (e && e.stack ? e.stack : e).toString().split('\n').slice(0, 6).join('\n'));
  process.exit(1);
}
console.log('RENDER SMOKE OK');
process.exit(0);`
	if got := feNode(t, script); got != "RENDER SMOKE OK" {
		t.Fatalf("渲染函数冒烟失败：%s", got)
	}
}

// TestAppJSAccountDotaskFlow 账号行「做任务」（需求 4）的交互契约：
//   - 取消 confirm ⇒ 不发请求、不动按钮；
//   - 确认 ⇒ POST /accounts/{uid}/tasks/run，按钮「做任务中…」禁用，完成后按
//     summary 弹「成功 X / 跳过 Y / 失败 Z」并刷新 overview；
//   - 200 + ok:false（企业版/无凭证等门控拒绝）⇒ 弹后端 message，不能当成成功；
//   - 接口抛错（409/504 等）⇒ 错误 toast，按钮恢复原状。
//
// 这些分支只在浏览器里发生，Go 侧与纯函数测试都看不到；字段名（ok/summary.done 等）
// 与后端 taskRunSummary 契约绑定，改名/漏读会让汇总数字静默变 0。
func TestAppJSAccountDotaskFlow(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('async function runAccountDotask');
const stop = src.indexOf('$(\'accBody\').addEventListener');
if (start < 0 || stop < 0 || stop < start) throw new Error('runAccountDotask region not found');
const toasts = [];
let apiCalls = [], overviewCalls = 0, confirmAnswer = false, apiImpl = null;
const ctx = {
  confirm: () => confirmAnswer,
  api: async (path, opts) => { apiCalls.push([path, opts && opts.method]); return apiImpl(path, opts); },
  toast: (msg, cls) => toasts.push([msg, cls || '']),
  // v2.1 #1：操作后的等价刷新入口（summary + 当前页），不再是全量 loadOverview。
  refreshAccountPool: () => { overviewCalls++; },
  dotaskBusy: new Set(),   // 进行中标记：轮询重渲染后按钮仍 disabled，完成/失败必须清空
  encodeURIComponent, Number, String, Object, Array,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, stop) + '\nthis.runAccountDotask = runAccountDotask;', ctx);
const btn = () => ({ disabled: false, textContent: '做任务' });
const out = {};
(async () => {
  // ① 取消确认
  let b = btn();
  confirmAnswer = false; apiCalls = []; toasts.length = 0;
  await ctx.runAccountDotask(b, 'u-1');
  out.cancel = { calls: apiCalls.length, disabled: b.disabled, label: b.textContent, toasts: toasts.length };
  // ② 正常完成：1 done / 1 skipped / 1 error
  b = btn(); confirmAnswer = true; toasts.length = 0; overviewCalls = 0;
  apiImpl = async () => {
    out.busyDuring = ctx.dotaskBusy.has('u-1');   // 执行中：轮询重渲染应继续渲染 disabled
    return ({ ok: true, uid: 'u-1',
      results: [{ status: 'done' }, { status: 'skipped' }, { status: 'error' }],
      summary: { total: 3, done: 1, skipped: 1, error: 1, claimed: 0 } });
  };
  await ctx.runAccountDotask(b, 'u-1');
  out.ok = { call: apiCalls.length ? apiCalls[0][0] : '', method: apiCalls.length ? apiCalls[0][1] : '',
    toast: toasts.length ? toasts[0][0] : '', cls: toasts.length ? toasts[0][1] : '',
    label: b.textContent, disabled: b.disabled, overview: overviewCalls };
  out.busyAfterOk = ctx.dotaskBusy.size;   // 完成后必须清空，否则按钮永久 disabled
  // ③ 门控拒绝 200 + ok:false
  b = btn(); toasts.length = 0; overviewCalls = 0;
  apiImpl = async () => ({ ok: false, message: '该账号为企业版，无个人成长任务体系', results: [], summary: {} });
  await ctx.runAccountDotask(b, 'u-2');
  out.reject = { toast: toasts.length ? toasts[0][0] : '', cls: toasts.length ? toasts[0][1] : '',
    label: b.textContent, overview: overviewCalls };
  // ④ 接口抛错（409 等）
  b = btn(); toasts.length = 0;
  apiImpl = async () => { throw new Error('HTTP 409'); };
  await ctx.runAccountDotask(b, 'u-3');
  out.fail = { toast: toasts.length ? toasts[0][0] : '', cls: toasts.length ? toasts[0][1] : '', label: b.textContent };
  // ⑤ 没有可执行任务
  b = btn(); toasts.length = 0;
  apiImpl = async () => ({ ok: true, results: [], summary: { total: 0, done: 0, skipped: 0, error: 0 } });
  await ctx.runAccountDotask(b, 'u-4');
  out.empty = { toast: toasts.length ? toasts[0][0] : '', label: b.textContent };
  out.busyEnd = ctx.dotaskBusy.size;   // 各分支（含抛错/门控拒绝）都不允许泄漏 busy 标记
  process.stdout.write(JSON.stringify(out));
})().catch(e => { console.error(e); process.exit(1); });`
	want := `{"cancel":{"calls":0,"disabled":false,"label":"做任务","toasts":0},` +
		`"busyDuring":true,` +
		`"ok":{"call":"accounts/u-1/tasks/run","method":"POST",` +
		`"toast":"做任务完成：成功 1 项，跳过 1 项，失败 1 项","cls":"err",` +
		`"label":"做任务","disabled":false,"overview":1},` +
		`"busyAfterOk":0,` +
		`"reject":{"toast":"该账号为企业版，无个人成长任务体系","cls":"err","label":"做任务","overview":1},` +
		`"fail":{"toast":"做任务失败：HTTP 409","cls":"err","label":"做任务"},` +
		`"empty":{"toast":"没有可执行的任务（可能已全部完成）","label":"做任务"},` +
		`"busyEnd":0}`
	if got := feNode(t, script); got != want {
		t.Fatalf("做任务交互=%s\nwant %s", got, want)
	}
}

// TestAppJSNextPollDelay v2.0.1 #2 轮询治理的纯函数（间隔解析/clamp、失败退避、
// ±20% 抖动、可见性状态机）。这些函数只吃数值、不碰 DOM，是整条调度链唯一能
// 逐项钉死的地方：15s 写成 15 或上限写错只会表现为「轮询过密/过疏」，肉眼极难发现。
//
// 规则（与 app.js 注释一致）：
//   - poll_interval_sec：缺省/null/空串/非数字 → 5；越界 clamp 2-60（四舍五入）。
//   - 失败退避：失败 1 次仍按基础间隔，之后 2x/6x/12x，上限 60s；
//     默认基础 5s 时即 5s → 10s → 30s → 60s。
//   - nextPollDelay：退避间隔 ±20% 抖动（rand 0=下限、0.5=原值、1=上限），
//     最终毫秒数封顶 60000（抖动不得突破退避上限）。
//   - visibility：hidden↔visible 的转换 → pause/resume，同向重复事件 noop。
func TestAppJSNextPollDelay(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function pollIntervalSec');
const stop = src.indexOf('function stopPollTimer');
if (start < 0 || stop < 0 || stop < start) throw new Error('poll pure functions not found');
const ctx = { Number, Math, String, JSON };
vm.createContext(ctx);
vm.runInContext(src.slice(start, stop) +
  '\nthis.pollIntervalSec = pollIntervalSec; this.pollBackoffSec = pollBackoffSec;' +
  ' this.nextPollDelay = nextPollDelay; this.pollVisibilityAction = pollVisibilityAction;', ctx);
const out = {
  parse: [undefined, null, '', 'abc', NaN, 0, 1, 2, 5, 60, 61, 1000, '7', 5.4].map(v => ctx.pollIntervalSec(v)),
  backoff: [[5,0],[5,1],[5,2],[5,3],[5,4],[5,9],[10,1],[10,2],[10,3],[10,4],[2,3],[2,4],[2,9],[60,0],[60,4],[1000,0],[1000,4],[5,-1],[5,'2']].map(([b,f]) => ctx.pollBackoffSec(b,f)),
  delay: [[5,0,0.5],[5,0,0],[5,0,1],[5,2,0.5],[5,3,0.5],[5,4,0.5],[5,4,1],[60,4,0],[60,4,1],[5,0,-5],[5,0,99],[5,0,NaN]].map(([b,f,r]) => ctx.nextPollDelay(b,f,r)),
  vis: [[false,false],[true,false],[true,true],[false,true]].map(([h,w]) => ctx.pollVisibilityAction(h,w)),
};
process.stdout.write(JSON.stringify(out));`
	want := `{"parse":[5,5,5,5,5,2,2,2,5,60,60,60,7,5],` +
		`"backoff":[5,5,10,30,60,60,10,20,60,60,12,24,24,60,60,60,60,5,10],` +
		`"delay":[5000,4000,6000,10000,30000,60000,60000,48000,60000,4000,6000,5000],` +
		`"vis":["noop","pause","noop","resume"]}`
	if got := feNode(t, script); got != want {
		t.Fatalf("轮询纯函数=%s\nwant %s", got, want)
	}
}

// TestAppJSPollingStateMachine v2.0.1 #2 的可见性/退避/401 状态机（真实函数执行、
// 定时器用桩替身）。纯函数测试看不出「隐藏时到底停没停表」这类接线错误：
// 少一处 scheduleNextPoll 判断就会在后台标签页继续打请求，而 Go 测试与语法检查全绿。
//
// 断言链路：
//  1. start() 首查成功后按默认 5s（±20%）排期，且注册了 visibilitychange；
//  2. 隐藏 → 清表、不新排期；隐藏期间 pollTick 直接返回（0 请求）；
//  3. 恢复可见 → 立即刷一次并重新排期；
//  4. 连续失败 4 次 → 5s/10s/30s/60s 四档退避且始终 ≤60s；成功一次回到基础间隔；
//  5. 401 → 停表、不自动重试、密钥框只自动弹一次；手动保存密钥后 start() 恢复；
//  6. refreshVisible 的视图分派（accounts/logs/taskscenter/其它）语义不变。
func TestAppJSPollingStateMachine(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('let refTimer = null;');
const stop = src.indexOf('\nstart();', start);
if (start < 0 || stop < 0 || stop < start) throw new Error('polling section not found');
const timers = [];
const cleared = [];
let timerSeq = 0;
const handlers = {};
const doc = { visibilityState: 'visible', addEventListener: (ev, fn) => { handlers[ev] = fn; } };
let loadCalls = 0, loadResult = true, logCalls = 0, reattachCalls = 0, openKeyCalls = 0;
const ctx = {
  document: doc,
  view: 'accounts',
  // v2.1 #1：accounts 视图的刷新入口是 refreshAccountsView（lite + 当前页），
  // start() 还会后台补一次完整 overview（ensureFullOverview）。
  refreshAccountsView: async () => { loadCalls++; return loadResult; },
  ensureFullOverview: async () => true,
  loadMeta: async () => true,          // v2.3.0 #12：start() 会后台加载服务端备注
  maybeMigrateRemarks: () => {},
  loadLogs: async () => { logCalls++; return true; },
  reattachQueueView: () => { reattachCalls++; },
  openKey: () => { openKeyCalls++; },
  api: async () => ({}),
  setTimeout: (fn, ms) => { timers.push(ms); return ++timerSeq; },
  clearTimeout: id => { cleared.push(id); },
  Math, Number, String, Promise, JSON,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, stop) + '\nthis.__p = {\n' +
  '  start, pollTick, onAuthRequired, handleVisibilityChange, refreshVisible,\n' +
  '  state: () => ({ base: pollBaseSec, failures: refFailures, authStop: pollAuthStopped,\n' +
  '                  prompted: keyPrompted, hidden: pageHidden, armed: pollArmed, timer: refTimer }),\n' +
  '};', ctx);
const P = ctx.__p;
const inRange = (v, lo, hi) => v >= lo && v <= hi;
const lastDelay = () => timers.length ? timers[timers.length - 1] : -1;
const flush = async () => { for (let i = 0; i < 8; i++) await Promise.resolve(); };
(async () => {
  const out = {};
  loadCalls = 0; loadResult = true;
  P.start();
  await flush();
  let s = P.state();
  out.start = { armed: s.armed, timer: s.timer !== null, loads: loadCalls,
                listener: typeof handlers.visibilitychange === 'function',
                delayOk: inRange(lastDelay(), 4000, 6000) };
  doc.visibilityState = 'hidden';
  const beforeTimers = timers.length, beforeClears = cleared.length;
  await P.handleVisibilityChange();
  s = P.state();
  out.hide = { hidden: s.hidden, timerGone: s.timer === null, clearedOne: cleared.length > beforeClears,
               noNewTimer: timers.length === beforeTimers };
  const beforeLoads = loadCalls;
  await P.pollTick();
  out.hiddenTick = { loads: loadCalls - beforeLoads, timerGone: P.state().timer === null };
  doc.visibilityState = 'visible';
  const loadsB = loadCalls, timersB = timers.length;
  await P.handleVisibilityChange();
  s = P.state();
  out.resume = { loads: loadCalls - loadsB, visible: !s.hidden, timer: s.timer !== null,
                 newTimer: timers.length === timersB + 1, delayOk: inRange(lastDelay(), 4000, 6000) };
  loadResult = false;
  const fails = [];
  for (let i = 0; i < 4; i++) { await P.pollTick(); fails.push(lastDelay()); }
  out.backoff = [
    inRange(fails[0], 4000, 6000),
    inRange(fails[1], 8000, 12000),
    inRange(fails[2], 24000, 36000),
    inRange(fails[3], 48000, 60000),
  ];
  out.backoffCapped = fails.every(d => d <= 60000);
  loadResult = true;
  await P.pollTick();
  out.recovered = { delayOk: inRange(lastDelay(), 4000, 6000), failures: P.state().failures };
  const before401Timers = timers.length;
  P.onAuthRequired();
  P.onAuthRequired();
  s = P.state();
  out.auth401 = { authStop: s.authStop, prompted: s.prompted, openKey: openKeyCalls,
                  timerGone: s.timer === null, noRetryTimer: timers.length === before401Timers };
  await P.pollTick();
  out.auth401.noRequest = P.state().timer === null;
  loadResult = true;
  P.start();
  await flush();
  s = P.state();
  out.manualResume = { authStop: s.authStop, prompted: s.prompted, timer: s.timer !== null,
                       delayOk: inRange(lastDelay(), 4000, 6000) };
  ctx.view = 'accounts'; loadCalls = 0;
  const accRet = await P.refreshVisible();
  ctx.view = 'logs'; logCalls = 0;
  const logsRet = await P.refreshVisible();
  ctx.view = 'taskscenter'; reattachCalls = 0;
  const tcRet = await P.refreshVisible();
  ctx.view = 'usage'; const usRet = await P.refreshVisible();
  out.dispatch = { accounts: accRet === true && loadCalls === 1, logs: logsRet === true && logCalls === 1,
                   taskscenter: tcRet === true && reattachCalls === 1, other: usRet === true };
  process.stdout.write(JSON.stringify(out));
})().catch(e => { console.error(e); process.exit(1); });`
	want := `{"start":{"armed":true,"timer":true,"loads":1,"listener":true,"delayOk":true},` +
		`"hide":{"hidden":true,"timerGone":true,"clearedOne":true,"noNewTimer":true},` +
		`"hiddenTick":{"loads":0,"timerGone":true},` +
		`"resume":{"loads":1,"visible":true,"timer":true,"newTimer":true,"delayOk":true},` +
		`"backoff":[true,true,true,true],"backoffCapped":true,` +
		`"recovered":{"delayOk":true,"failures":0},` +
		`"auth401":{"authStop":true,"prompted":true,"openKey":1,"timerGone":true,"noRetryTimer":true,"noRequest":true},` +
		`"manualResume":{"authStop":false,"prompted":false,"timer":true,"delayOk":true},` +
		`"dispatch":{"accounts":true,"logs":true,"taskscenter":true,"other":true}}`
	if got := feNode(t, script); got != want {
		t.Fatalf("轮询状态机=%s\nwant %s", got, want)
	}
}

// TestAppJSConfigNewFields v2.0.1 #4/#10/#11 新增 7 个配置项的表单接线：
//   - CFG_MAP 的 JSON 路径必须与后端配置键完全一致（panel.api_key /
//     panel.poll_interval_sec / security.allow_anonymous / schedule.catch_up_minutes /
//     schedule.balance_refresh_concurrency / schedule.balance_refresh_startup_delay /
//     schedule.balance_refresh_startup_force）；
//   - collectConfig 类型：number → Number、checkbox → bool、duration → string；
//   - 面板密钥空串必须照发（= 回落网关 key），数字/duration 空串不下发；
//   - loadConfig 回填数字/复选框/密钥；
//   - duration 校验覆盖新字段，且裸 "0" 合法（Go ParseDuration 同口径）。
func TestAppJSConfigNewFields(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const CFG_MAP');
const stop = src.indexOf('/* ── 添加账号');
if (start < 0 || stop < 0 || stop < start) throw new Error('config section not found');
const elements = {};
const mkEl = (type, value) => ({ type, value, checked: false, classList: { toggle() {} }, title: '' });
for (const n of ['panel_api_key','poll_interval_sec','allow_anonymous','catch_up_minutes',
                 'balance_refresh_concurrency','balance_refresh_startup_delay','balance_refresh_startup_force']) {
  elements[n] = mkEl(n === 'allow_anonymous' || n === 'balance_refresh_startup_force' ? 'checkbox' : 'text', '');
}
elements.poll_interval_sec.type = 'number';
elements.catch_up_minutes.type = 'number';
elements.balance_refresh_concurrency.type = 'number';
elements.panel_api_key.type = 'password';
const cfgForm = { elements, addEventListener() {}, onsubmit: null };
const nodes = { cfgForm, cfgPath: { textContent: '' }, cfgNote: { textContent: '' },
                btnEye: {}, btnCfgReload: {}, btnCfgSave: { disabled: false, textContent: '' },
                cfgKey: mkEl('password', ''), cfgPanelKey: mkEl('password', '') };
const toasts = [];
let cfgResp = { path: '/tmp/config.json', config: {} };
const ctx = {
  $: id => nodes[id],
  api: async () => cfgResp,
  toast: (msg, cls) => toasts.push([msg, cls || '']),
  Date, Number, String, Math, Map, Array, Object, isNaN, URLSearchParams, Set,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, stop) + '\nthis.cfgMap = CFG_MAP;' +
  ' this.collectConfig = collectConfig; this.loadConfig = loadConfig;' +
  ' this.durationBad = durationBad; this.durationFields = DURATION_FIELDS;' +
  ' this.clearable = CLEARABLE_CFG;', ctx);
const out = {};
const has = (o, k) => Object.prototype.hasOwnProperty.call(o || {}, k);
const wantMap = {
  panel_api_key: ['panel', 'api_key'],
  poll_interval_sec: ['panel', 'poll_interval_sec'],
  allow_anonymous: ['security', 'allow_anonymous'],
  catch_up_minutes: ['schedule', 'catch_up_minutes'],
  balance_refresh_concurrency: ['schedule', 'balance_refresh_concurrency'],
  balance_refresh_startup_delay: ['schedule', 'balance_refresh_startup_delay'],
  balance_refresh_startup_force: ['schedule', 'balance_refresh_startup_force'],
};
out.map = Object.keys(wantMap).map(k => JSON.stringify(ctx.cfgMap[k]) === JSON.stringify(wantMap[k]));
elements.panel_api_key.value = 'panel-secret';
elements.poll_interval_sec.value = '7';
elements.allow_anonymous.checked = true;
elements.catch_up_minutes.value = '45';
elements.balance_refresh_concurrency.value = '6';
elements.balance_refresh_startup_delay.value = '1m30s';
elements.balance_refresh_startup_force.checked = true;
const cfg = ctx.collectConfig();
out.collect = {
  panelKey: (cfg.panel || {}).api_key,
  interval: (cfg.panel || {}).poll_interval_sec,
  intervalIsNumber: typeof (cfg.panel || {}).poll_interval_sec === 'number',
  anonymous: (cfg.security || {}).allow_anonymous,
  anonymousIsBool: typeof (cfg.security || {}).allow_anonymous === 'boolean',
  catchUp: (cfg.schedule || {}).catch_up_minutes,
  catchUpIsNumber: typeof (cfg.schedule || {}).catch_up_minutes === 'number',
  conc: (cfg.schedule || {}).balance_refresh_concurrency,
  delay: (cfg.schedule || {}).balance_refresh_startup_delay,
  force: (cfg.schedule || {}).balance_refresh_startup_force,
  forceIsBool: typeof (cfg.schedule || {}).balance_refresh_startup_force === 'boolean',
};
elements.panel_api_key.value = '';
elements.poll_interval_sec.value = '';
elements.balance_refresh_startup_delay.value = '';
const cfg2 = ctx.collectConfig();
out.cleared = {
  clearable: ctx.clearable.has('panel_api_key'),
  panelKeySentEmpty: has(cfg2.panel, 'api_key') && cfg2.panel.api_key === '',
  intervalSkipped: !has(cfg2.panel, 'poll_interval_sec'),
  delaySkipped: !has(cfg2.schedule, 'balance_refresh_startup_delay'),
  checkboxStillSent: has(cfg2.schedule, 'balance_refresh_startup_force') && cfg2.schedule.balance_refresh_startup_force === true,
};
cfgResp = { path: '/etc/config.json', config: {
  panel: { api_key: 'pk-secret', poll_interval_sec: 11 },
  security: { allow_anonymous: true },
  schedule: { catch_up_minutes: 60, balance_refresh_concurrency: 4,
              balance_refresh_startup_delay: '45s', balance_refresh_startup_force: true },
} };
(async () => {
  await ctx.loadConfig();
  out.load = {
    path: nodes.cfgPath.textContent,
    panelKey: elements.panel_api_key.value,
    interval: String(elements.poll_interval_sec.value),
    anonymous: elements.allow_anonymous.checked,
    catchUp: String(elements.catch_up_minutes.value),
    conc: String(elements.balance_refresh_concurrency.value),
    delay: elements.balance_refresh_startup_delay.value,
    force: elements.balance_refresh_startup_force.checked,
    noteEmpty: nodes.cfgNote.textContent === '',
  };
  const bad = v => { elements.balance_refresh_startup_delay.value = v; return ctx.durationBad('balance_refresh_startup_delay'); };
  out.duration = {
    inFields: ctx.durationFields.indexOf('balance_refresh_startup_delay') >= 0,
    ok30s: !bad('30s'), ok0: !bad('0'), okMix: !bad('1h30m'),
    badNoUnit: bad('30'), badNegative: bad('-5s'), badText: bad('abc'), emptyOk: !bad(''),
  };
  process.stdout.write(JSON.stringify(out));
})().catch(e => { console.error(e); process.exit(1); });`
	want := `{"map":[true,true,true,true,true,true,true],` +
		`"collect":{"panelKey":"panel-secret","interval":7,"intervalIsNumber":true,"anonymous":true,` +
		`"anonymousIsBool":true,"catchUp":45,"catchUpIsNumber":true,"conc":6,"delay":"1m30s",` +
		`"force":true,"forceIsBool":true},` +
		`"cleared":{"clearable":true,"panelKeySentEmpty":true,"intervalSkipped":true,` +
		`"delaySkipped":true,"checkboxStillSent":true},` +
		`"load":{"path":"/etc/config.json","panelKey":"pk-secret","interval":"11","anonymous":true,` +
		`"catchUp":"60","conc":"4","delay":"45s","force":true,"noteEmpty":true},` +
		`"duration":{"inFields":true,"ok30s":true,"ok0":true,"okMix":true,"badNoUnit":true,` +
		`"badNegative":true,"badText":true,"emptyOk":true}}`
	if got := feNode(t, script); got != want {
		t.Fatalf("配置新字段=%s\nwant %s", got, want)
	}
}

// TestAppJSBalanceRefreshAsync v2.0.1 #10：顶部「刷新」按钮对异步 balance_all 的交互：
//   - 点击 → 按钮「已提交…」+ 禁用 → POST balance_all；
//   - 成功 → toast「已开始刷新余额（后台执行，完成后自动刷新）」，2.5s 后补拉一次
//     overview（让首批余额先出现），按钮恢复原文案；
//   - 409（同类任务正在执行）→ api() 抛错，catch 原样展示后端 error 文案；
//   - 不再同步等待、也不再谎报「余额已从上游刷新」。
func TestAppJSBalanceRefreshAsync(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf("$('btnRefresh').onclick");
const stop = src.indexOf('/* ── 轮询治理');
if (start < 0 || stop < 0 || stop < start) throw new Error('btnRefresh region not found');
const btn = { disabled: false, textContent: '刷新' };
const apiCalls = [];
const toasts = [];
const timers = [];
let overviewCalls = 0, logCalls = 0, apiErr = null;
const during = {};
const ctx = {
  $: id => (id === 'btnRefresh' ? btn : null),
  api: async (path, opts) => {
    apiCalls.push([path, opts && opts.method]);
    during.text = btn.textContent;
    during.disabled = btn.disabled;
    if (apiErr) throw new Error(apiErr);
    return { ok: true, started: true };
  },
  toast: (m, c) => toasts.push([m, c || '']),
  // v2.1 #1：定时补拉走 refreshAccountPool（summary + 当前页），不再全量 overview。
  refreshAccountPool: () => { overviewCalls++; },
  loadJobs: () => {},   // v2.3.0 #10：余额刷新后立即刷顶部 Job 进度条
  loadLogs: () => { logCalls++; },
  view: 'accounts',
  setTimeout: (fn, ms) => { timers.push({ fn, ms }); return timers.length; },
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, stop), ctx);
(async () => {
  const out = {};
  await btn.onclick();
  out.ok = { calls: apiCalls.splice(0), during: { text: during.text, disabled: during.disabled },
             toast: toasts.splice(0), timers: timers.map(t => t.ms),
             label: btn.textContent, disabled: btn.disabled };
  timers.splice(0).forEach(t => t.fn());
  out.ok.overviewAfterTimer = overviewCalls;
  overviewCalls = 0; timers.length = 0; apiErr = '正在执行余额刷新任务，请稍后重试';
  await btn.onclick();
  out.conflict = { calls: apiCalls.splice(0), toast: toasts.splice(0),
                   timers: timers.length, label: btn.textContent, disabled: btn.disabled,
                   overview: overviewCalls };
  apiErr = null; logCalls = 0; ctx.view = 'logs';
  await btn.onclick();
  for (const t of timers.splice(0)) t.fn();
  out.logsView = { logCalls };
  process.stdout.write(JSON.stringify(out));
})().catch(e => { console.error(e); process.exit(1); });`
	want := `{"ok":{"calls":[["balance_all","POST"]],"during":{"text":"已提交…","disabled":true},` +
		`"toast":[["已开始刷新余额（后台执行，job 进度见顶部）","ok"]],"timers":[2500],` +
		`"label":"刷新","disabled":false,"overviewAfterTimer":1},` +
		`"conflict":{"calls":[["balance_all","POST"]],` +
		`"toast":[["刷新失败：正在执行余额刷新任务，请稍后重试","err"]],"timers":0,` +
		`"label":"刷新","disabled":false,"overview":0},` +
		`"logsView":{"logCalls":1}}`
	if got := feNode(t, script); got != want {
		t.Fatalf("顶部刷新交互=%s\nwant %s", got, want)
	}
}

// TestPanelConfigNewControls 配置页新增控件的「双侧接线」：index.html 里有控件
// （name/id/type + 中文提示），app.js 的 CFG_MAP 有对应路径，且轮询/密钥门的
// 关键接线存在。只改一边时表单看起来正常，但值会被静默丢弃或永远不生效。
func TestPanelConfigNewControls(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	h, j := string(html), string(js)
	form := h
	if i := strings.Index(h, `<form id="cfgForm">`); i >= 0 {
		form = h[i:]
		if k := strings.Index(form, "</form>"); k >= 0 {
			form = form[:k]
		}
	} else {
		t.Fatal("index.html 缺少配置表单 #cfgForm")
	}
	controls := []struct{ name, id, typ string }{
		{"panel_api_key", "cfgPanelKey", "password"},
		{"poll_interval_sec", "cfgPollInterval", "number"},
		{"allow_anonymous", "cfgAllowAnonymous", "checkbox"},
		{"catch_up_minutes", "cfgCatchUp", "number"},
		{"balance_refresh_concurrency", "cfgBalConc", "number"},
		{"balance_refresh_startup_delay", "cfgBalDelay", ""},
		{"balance_refresh_startup_force", "cfgBalForce", "checkbox"},
	}
	// 逐个控件按 <input ...> 标签整体校验：name 决定 collectConfig 取值，
	// id 决定 app.js 里 $('cfgXxx') 能否取到，type 决定 number/bool/string 转换。
	for _, c := range controls {
		tagRe := regexp.MustCompile(`<input[^>]*\bname="` + regexp.QuoteMeta(c.name) + `"[^>]*>`)
		tag := tagRe.FindString(form)
		if tag == "" {
			t.Errorf("配置表单缺少控件 name=%q（CFG_MAP 有键但表单没有 → 无法回填/保存）", c.name)
			continue
		}
		if !strings.Contains(tag, `id="`+c.id+`"`) {
			t.Errorf("控件 %q 的标签缺少 id=%q：%s", c.name, c.id, tag)
		}
		if c.typ != "" && !strings.Contains(tag, `type="`+c.typ+`"`) {
			t.Errorf("控件 %q 类型应为 %q：%s", c.name, c.typ, tag)
		}
	}
	// CFG_MAP 的 JSON 路径必须与后端键完全一致（写错层级 = 保存后不生效）。
	for _, want := range []string{
		"panel_api_key: ['panel', 'api_key']",
		"poll_interval_sec: ['panel', 'poll_interval_sec']",
		"allow_anonymous: ['security', 'allow_anonymous']",
		"catch_up_minutes: ['schedule', 'catch_up_minutes']",
		"balance_refresh_concurrency: ['schedule', 'balance_refresh_concurrency']",
		"balance_refresh_startup_delay: ['schedule', 'balance_refresh_startup_delay']",
		"balance_refresh_startup_force: ['schedule', 'balance_refresh_startup_force']",
	} {
		if !strings.Contains(j, want) {
			t.Errorf("app.js CFG_MAP 缺少映射 %q", want)
		}
	}
	// 提示文案要点：回落语义、2-60 clamp、非回环空 key 拒绝启动、补跑/并发/首刷。
	for _, want := range []string{"留空 = 回落网关 API 密钥", "2–60 秒", "拒绝启动", "补跑窗口", "余额刷新并发", "启动首刷"} {
		if !strings.Contains(h, want) {
			t.Errorf("index.html 新配置提示缺少 %q", want)
		}
	}
	// 登录框与 401 文案区分「网关密钥 / 面板密钥」。
	for _, want := range []string{"面板密钥", "panel.api_key", "网关"} {
		if !strings.Contains(h, want) {
			t.Errorf("index.html 密钥门文案缺少 %q（需区分网关密钥与面板密钥）", want)
		}
	}
	if !strings.Contains(j, "面板密钥可能已变更（panel.api_key）") {
		t.Error("app.js 401 提示未指向「面板密钥可能已变更（panel.api_key）」")
	}
	if !strings.Contains(j, "const LS_KEY = 'wb2api.key'") {
		t.Error("localStorage 键必须沿用 wb2api.key（兼容旧版本已存凭据）")
	}
}

// TestPanelPollingWiring 轮询治理的源码接线：防「纯函数都对、但主链路没用上」。
//   - overview 成功时用响应里的 poll_interval_sec 更新基础间隔；
//   - 自调度 setTimeout 走 nextPollDelay（带抖动/退避），不再是无条件 setInterval；
//   - visibilitychange 已注册；401 走 onAuthRequired（不直接 openKey 抢焦点）；
//   - 页面隐藏/401/退避的判断都在 scheduleNextPoll 里。
func TestPanelPollingWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	j := string(js)
	for _, want := range []string{
		"pollBaseSec = pollIntervalSec(d.poll_interval_sec)",
		"refTimer = setTimeout(pollTick, nextPollDelay(pollBaseSec, refFailures, Math.random()))",
		"document.addEventListener('visibilitychange', handleVisibilityChange)",
		"if (!pollArmed || pageHidden || pollAuthStopped) return;",
		"if (r.status === 401) { onAuthRequired(); throw new Error('密钥无效或未填写'); }",
		"function onAuthRequired() {",
		"keyPrompted = true;",
		"function pollVisibilityAction(hiddenNow, wasHidden)",
	} {
		if !strings.Contains(j, want) {
			t.Errorf("app.js 轮询接线缺少 %q", want)
		}
	}
	// 旧实现必须彻底移除：无条件 5s setInterval / 401 直接 openKey / 旧的刷新成功文案。
	// （注释里允许保留「旧实现是 setInterval…」的说明，故只钉代码形态。）
	for _, forbidden := range []string{
		"refTimer = setInterval(",
		"余额已从上游刷新",
		"if (r.status === 401) { openKey()",
	} {
		if strings.Contains(j, forbidden) {
			t.Errorf("app.js 仍残留旧轮询实现 %q", forbidden)
		}
	}
}

// TestAppJSPollIntervalFromOverview 运行时验证「间隔可配」的完整链路：整份 app.js 在
// 惰性 DOM 沙箱里求值，fetch 返回带 poll_interval_sec 的 overview，再逐次改响应值调用
// loadOverview，断言 pollBaseSec 被正确解析/clamp。纯函数测试只证明解析器本身对，
// 这条证明 overview → 调度状态真的接上了（漏接时前端永远 5s，Go 侧全绿）。
func TestAppJSPollIntervalFromOverview(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});
let overviewResp = { version: '2.0.1-pro', uptime_sec: 1, total: 0, healthy: 0, cooling: 0,
  disabled: 0, paused: 0, sticky_sessions: 0, in_flight_full: 0, accounts: [], model_locks: [],
  balance_refresh_sec: 0, poll_interval_sec: 11 };
const sandbox = new Proxy({
  location: { hash: '#accounts' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert, getElementById: () => inert,
              addEventListener() {}, documentElement: inert, head: inert, body: inert,
              createElement: () => inert, cookie: '' },
  fetch: () => Promise.resolve({ status: 200, ok: true, json: () => Promise.resolve(overviewResp) }),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  MutationObserver: class { constructor() {} observe() {} disconnect() {} takeRecords() { return []; } },
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set,
  RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent,
  decodeURIComponent, URL, URLSearchParams, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
vm.runInContext(src + '\nthis.__r = { loadOverview, getBase: () => pollBaseSec };', sandbox, { filename: 'app.js' });
(async () => {
  // start() 的首次 overview 是异步的：轮询等待它把 11 写进 pollBaseSec（最多 2s），
  // 固定 sleep 在慢机器/并行测试下会偶发假失败。
  for (let i = 0; i < 200 && sandbox.__r.getBase() === 5; i++) {
    await new Promise(r => setTimeout(r, 10));
  }
  const out = {};
  out.fromStart = sandbox.__r.getBase();
  const setAndLoad = async v => {
    overviewResp = Object.assign({}, overviewResp, { poll_interval_sec: v });
    await sandbox.__r.loadOverview(true);
    return sandbox.__r.getBase();
  };
  out.updated = await setAndLoad(3);
  out.clampedHigh = await setAndLoad(100);
  out.clampedLow = await setAndLoad(1);
  out.fallback = await setAndLoad('oops');
  out.nullFallback = await setAndLoad(null);
  process.stdout.write(JSON.stringify(out));
  process.exit(0);
})().catch(e => { console.error(e); process.exit(1); });`
	want := `{"fromStart":11,"updated":3,"clampedHigh":60,"clampedLow":2,"fallback":5,"nullFallback":5}`
	if got := feNode(t, script); got != want {
		t.Fatalf("overview 轮询间隔=%s\nwant %s", got, want)
	}
}

// TestAppJSCfgSaveCredentialSync 配置保存后的本机凭据（localStorage: wb2api.key）同步：
//   - 面板密钥未变（含留空回落网关 key）→ 沿用当前生效凭据（网关 key 是热生效的）；
//   - 面板密钥变了（设置新值或清空回落）→ 必须保持旧凭据：panel.api_key 是重启项，
//     提前切换会让重启前所有请求 401；
//   - 保存失败 → 不动凭据、不恢复轮询；保存成功 → 清 401 停表状态并重新排期。
//
// 这条链路只在浏览器里发生（重启前后各一次），漏掉任何一个分支都会让面板「保存后
// 立刻踢自己下线」，且 Go 侧完全无感。
func TestAppJSCfgSaveCredentialSync(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf("$('cfgForm').onsubmit");
const stop = src.indexOf('/* ── 添加账号');
if (start < 0 || stop < 0 || stop < start) throw new Error('cfg save handler not found');
const sets = [], toasts = [], apiCalls = [];
let apiErr = null, schedCalls = 0;
const cfgKey = { value: '' }, cfgPanelKey = { value: '' };
const saveBtn = { disabled: false, textContent: '保存配置' };
const nodes = { cfgForm: { elements: {}, onsubmit: null }, btnCfgSave: saveBtn, cfgKey, cfgPanelKey };
const ctx = {
  $: id => nodes[id],
  LS_KEY: 'wb2api.key',
  localStorage: { setItem: (k, v) => sets.push([k, v]) },
  dig: (o, p) => p.reduce((x, k) => (x == null ? undefined : x[k]), o),
  cfgLoaded: null,
  collectConfig: () => ({ marker: 1 }),
  api: async (path, opts) => {
    apiCalls.push([path, opts && opts.method]);
    if (apiErr) throw new Error(apiErr);
    return { restart_required: ['panel.api_key'] };
  },
  toast: (m, c) => toasts.push([m, c || '']),
  markDurationFields: () => {},
  durationBad: () => false,
  DURATION_FIELDS: [],
  pollAuthStopped: true, keyPrompted: true, refFailures: 3, refTimer: null, refInFlight: false,
  scheduleNextPoll: () => { schedCalls++; },
  loadConfig: () => {}, refreshAccountPool: () => {},
  JSON,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, stop), ctx);
const onSubmit = nodes.cfgForm.onsubmit;
const run = async (prevPanel, panelVal, gatewayVal, err) => {
  sets.length = 0; toasts.length = 0; apiCalls.length = 0; schedCalls = 0;
  apiErr = err || null;
  ctx.cfgLoaded = { panel: { api_key: prevPanel } };
  ctx.pollAuthStopped = true; ctx.keyPrompted = true; ctx.refFailures = 3;
  cfgPanelKey.value = panelVal;
  cfgKey.value = gatewayVal;
  await onSubmit({ preventDefault() {} });
  return {
    sets: sets.slice(), toast: toasts.length ? toasts[toasts.length - 1] : null,
    calls: apiCalls.slice(),
    state: { authStop: ctx.pollAuthStopped, prompted: ctx.keyPrompted,
             failures: ctx.refFailures, sched: schedCalls, label: saveBtn.textContent, disabled: saveBtn.disabled },
  };
};
(async () => {
  const out = {};
  out.fallbackChanged = await run('', '', 'new-gw', null);
  out.fallbackEmpty = await run('', '', '', null);
  out.panelUnchanged = await run('pk', 'pk', 'new-gw', null);
  out.panelChanged = await run('old-pk', 'new-pk', '', null);
  out.panelCleared = await run('old-pk', '', 'gw', null);
  out.saveFail = await run('', '', 'new-gw', 'HTTP 400');
  process.stdout.write(JSON.stringify(out));
  process.exit(0);
})().catch(e => { console.error(e); process.exit(1); });`
	okState := `"toast":["配置已保存，其中 1 项需重启进程生效","ok"],"calls":[["config","POST"]],` +
		`"state":{"authStop":false,"prompted":false,"failures":0,"sched":1,` +
		`"label":"保存配置","disabled":false}}`
	want := `{"fallbackChanged":{"sets":[["wb2api.key","new-gw"]],` + okState +
		`,"fallbackEmpty":{"sets":[],` + okState +
		`,"panelUnchanged":{"sets":[["wb2api.key","pk"]],` + okState +
		`,"panelChanged":{"sets":[],` + okState +
		`,"panelCleared":{"sets":[],` + okState +
		`,"saveFail":{"sets":[],"toast":["保存失败：HTTP 400","err"],"calls":[["config","POST"]],` +
		`"state":{"authStop":true,"prompted":true,"failures":3,"sched":0,` +
		`"label":"保存配置","disabled":false}}}`
	if got := feNode(t, script); got != want {
		t.Fatalf("配置保存凭据同步=%s\nwant %s", got, want)
	}
}

// TestAppJSAccountsQueryParams v2.1 #1 的服务端筛选/分页参数映射。
//
// 账号池的筛选条件必须翻译成后端约定的 query（改名/改值都会让筛选静默失效：
// 界面还是那个界面，但返回的是未筛选的数据）。这里逐项钉住：
//
//	state: ok→healthy、cool→cooling、paused、disabled
//	ver:   enterprise→enterprise=1、global→realm=global&enterprise=0、
//	       cn→realm=cn&enterprise=0（旧 UI 三分法互斥：国内/海外都排除企业号，
//	       而服务端 realm 与 enterprise 是两个正交维度）
//	page_size=0（全部）原样下发；页码取整、非法值回落 1。
func TestAppJSAccountsQueryParams(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function accStateParam');
const end = src.indexOf('function secUntil');
if (start < 0 || end < 0 || end < start) throw new Error('accounts query helpers not found');
const ctx = { URLSearchParams };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.accStateParam = accStateParam; this.accQueryString = accQueryString;', ctx);
const out = {
  state: ['ok', 'cool', 'paused', 'disabled', '', 'bogus'].map(v => ctx.accStateParam(v)),
  query: [
    ctx.accQueryString({ q: '', state: '', ver: '' }, 1, 100),
    ctx.accQueryString({ q: '', state: 'ok', ver: '' }, 2, 0),
    ctx.accQueryString({ q: '', state: 'cool', ver: 'enterprise' }, 1, 50),
    ctx.accQueryString({ q: 'nick&x', state: '', ver: 'global' }, 1, 100),
    ctx.accQueryString({ q: '', state: '', ver: 'cn' }, 1, 200),
    ctx.accQueryString({ q: '', state: '', ver: '' }, 0, -5),
    // v2.3.1 标签筛选：tag 只编码一次，可与 q/state/ver 组合
    ctx.accQueryString({ q: '', state: '', ver: '', tag: '采购A' }, 1, 100),
    ctx.accQueryString({ q: 'x', state: 'ok', ver: 'global', tag: 'a b&c' }, 3, 50),
    ctx.accQueryString({ q: '', state: '', ver: 'enterprise', tag: '待补签' }, 1, 100),
  ],
};
// 服务端收到的 tag 必须与用户所选完全一致（防止 URLSearchParams 之前又手工 encodeURIComponent）
out.tagRoundTrip = new URLSearchParams(out.query[6]).get('tag');
out.tagRoundTrip2 = new URLSearchParams(out.query[7]).get('tag');
process.stdout.write(JSON.stringify(out));`
	want := `{"state":["healthy","cooling","paused","disabled","",""],` +
		`"query":["page=1&page_size=100","page=2&page_size=0&state=healthy",` +
		`"page=1&page_size=50&state=cooling&enterprise=1",` +
		`"page=1&page_size=100&q=nick%26x&realm=global&enterprise=0",` +
		`"page=1&page_size=200&realm=cn&enterprise=0","page=1&page_size=0",` +
		`"page=1&page_size=100&tag=%E9%87%87%E8%B4%ADA",` +
		`"page=3&page_size=50&state=healthy&q=x&tag=a+b%26c&realm=global&enterprise=0",` +
		`"page=1&page_size=100&tag=%E5%BE%85%E8%A1%A5%E7%AD%BE&enterprise=1"],` +
		`"tagRoundTrip":"采购A","tagRoundTrip2":"a b&c"}`
	if got := feNode(t, script); got != want {
		t.Fatalf("账号池服务端参数=%s\nwant %s", got, want)
	}
}

// TestAppJSApiCached304 apiCached 的 ETag/304 缓存与服务端分页查询。
//
// 304 没有 body（parse 会得到空对象），必须回落到按查询串缓存的上一份 rows；缓存
// 键隔离（不同筛选/页码不能串数据）、无 ETag 不缓存、401 走统一停表逻辑、LRU 有界。
// 这些分支只在真实 HTTP 条件下出现，浏览器里很难手工复现。
func TestAppJSApiCached304(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function accStateParam');
const end = src.indexOf('function secUntil');
if (start < 0 || end < 0 || end < start) throw new Error('apiCached helpers not found');
const calls = [];
let resp = null;
const ctx = {
  URLSearchParams,
  LS_KEY: 'wb2api.key',
  localStorage: { getItem: () => 'sk-test' },
  keyPrompted: true,
  authCalls: 0,
  onAuthRequired: () => { ctx.authCalls++; },
  fetch: async (url, opts) => {
    calls.push({ url: String(url), headers: (opts && opts.headers) || {} });
    const r = resp();
    return {
      status: r.status,
      ok: r.status >= 200 && r.status < 300,
      headers: { get: k => (k === 'ETag' ? (r.etag || null) : null) },
      json: async () => r.body,
    };
  },
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.apiCached = apiCached; this.etagCachePut = etagCachePut;', ctx);
(async () => {
  const out = {};
  const cache = {};
  resp = () => ({ status: 200, etag: 'W/"acct-1"', body: { rows: [{ uid: 'a' }], total: 1 } });
  const r1 = await ctx.apiCached('accounts?page=1&page_size=100', cache);
  resp = () => ({ status: 304 });
  const r2 = await ctx.apiCached('accounts?page=1&page_size=100', cache);
  out.cache304 = {
    firstNotCached: r1.cached === false,
    secondCached: r2.cached === true,
    sameRows: JSON.stringify(r2.data) === JSON.stringify(r1.data),
    conditional: calls[1].headers['If-None-Match'] || '',
    noConditionalFirst: calls[0].headers['If-None-Match'] === undefined,
    authHeader: calls[0].headers['Authorization'] || '',
  };
  resp = () => ({ status: 200, etag: 'W/"acct-2"', body: { rows: [] } });
  await ctx.apiCached('accounts?page=2&page_size=100', cache);
  out.keyIsolation = calls[2].headers['If-None-Match'] === undefined;
  const cache2 = {};
  resp = () => ({ status: 200, body: { rows: [] } });
  await ctx.apiCached('accounts?page=3&page_size=10', cache2);
  await ctx.apiCached('accounts?page=3&page_size=10', cache2);
  out.noEtag = calls[4].headers['If-None-Match'] === undefined;
  resp = () => ({ status: 401 });
  let authErr = '';
  try { await ctx.apiCached('accounts?page=1&page_size=100', {}); } catch (e) { authErr = e.message; }
  out.unauthorized = { calls: ctx.authCalls, msg: authErr };
  resp = () => ({ status: 500, body: { error: 'boom' } });
  let srvErr = '';
  try { await ctx.apiCached('accounts?page=1&page_size=100', {}); } catch (e) { srvErr = e.message; }
  out.serverError = srvErr;
  const cache3 = {};
  for (let i = 0; i < 40; i++) ctx.etagCachePut(cache3, 'k' + i, { etag: 'e' + i, data: {} });
  out.lru = { kept: Object.keys(cache3).filter(k => k !== '__keys').length,
              newest: !!cache3.k39, oldestGone: !cache3.k0 };
  process.stdout.write(JSON.stringify(out));
})().catch(e => { console.error(e); process.exit(1); });`
	want := `{"cache304":{"firstNotCached":true,"secondCached":true,"sameRows":true,` +
		`"conditional":"W/\"acct-1\"","noConditionalFirst":true,"authHeader":"Bearer sk-test"},` +
		`"keyIsolation":true,"noEtag":true,"unauthorized":{"calls":1,"msg":"密钥无效或未填写"},` +
		`"serverError":"boom","lru":{"kept":32,"newest":true,"oldestGone":true}}`
	if got := feNode(t, script); got != want {
		t.Fatalf("apiCached/304=%s\nwant %s", got, want)
	}
}

// TestAppJSAccountsServerPaging v2.1 #1 的端到端数据流（整份 app.js 在惰性 DOM
// 沙箱里求值，fetch 按 URL 路由返回 lite/full/accounts 三种响应）：
//  1. 首屏 = lite overview + 当前页 accounts（query 带 page/page_size）+ 后台一次性
//     完整 overview（积分合计/到期快照/备注需要全量；不是每 5s 全量）；
//  2. 完整 overview 的 60s 新鲜窗口：窗口内 ensureFullOverview 不再请求，作废后重拉；
//  3. 列表 tab 轮询不拉全量；「积分到期」tab 激活才按窗口补全量（懒加载）；
//  4. renderAccounts(rows, meta) 直接渲染服务端 rows（不再本地 filter），计数/页码
//     用服务端 total/filtered/page/pages；冷却倒计时由 until 本地算（light 无
//     cool_remaining_sec）；无 meta 时退回旧的数组调用行为。
func TestAppJSAccountsServerPaging(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});
const now = Math.floor(Date.now() / 1000);
const lite = { version: '2.1.0-pro', uptime_sec: 60, redis_mode: 'memory', sticky_sessions: 0,
  total: 2, healthy: 1, cooling: 1, disabled: 0, paused: 0, in_flight_full: 0,
  model_locks: [], balance_refresh_sec: 300, poll_interval_sec: 5,
  credits_sum: 15, credits_total_sum: 30 };
const full = Object.assign({}, lite, { accounts: [
  { uid: 'a', nickname: 'A', credits: 10, credits_total: 20 },
  { uid: 'b', nickname: 'B', credits: 5, credits_total: 10, disabled: true },
] });
const page = { ok: true, now: now, total: 2, filtered: 2, page: 1, page_size: 100, pages: 1,
  rows: [{ uid: 'a', nickname: 'A', credits: 10, credits_total: 20, token_usage: {} },
         { uid: 'b', nickname: 'B', credits: 5, credits_total: 10, disabled: true, token_usage: {} }] };
const calls = [];
const sinks = {};
const sinkIds = ['accBody', 'accCount', 'accPageInfo', 'accPrev', 'accNext', 'sTotal', 'sHealthy',
  'sCooling', 'sDisabled', 'sPaused', 'sCredits', 'sSticky', 'navSub', 'navVer', 'navRedis',
  'navState', 'navPulse', 'accNote', 'subMeta', 'expBox', 'expList', 'expNote', 'expPad', 'cfgForm',
  'accTruncHint', 'btnRescan', 'accTagFilter'];
for (const id of sinkIds) sinks[id] = { innerHTML: '', textContent: '', hidden: false, disabled: false,
  className: '', value: '', dataset: {}, children: [], addEventListener() {}, style: {} };
// 标签下拉的 change 监听要能触发（其余控件保持 inert）。
sinks.accTagFilter.addEventListener = function (ev, fn) { this.onchange = fn; };
const fetchStub = url => {
  const u = String(url);
  calls.push(u);
  let body = {};
  if (u.indexOf('overview?lite=1') >= 0) body = lite;
  else if (u.indexOf('/panel/api/overview') >= 0) body = full;
  else if (u.indexOf('/panel/api/accounts/rescan') >= 0) body = { scanned: 2, added: 1, removed: 0 };
  else if (u.indexOf('/panel/api/accounts?') >= 0) body = page;
  return Promise.resolve({ status: 200, ok: true, headers: { get: () => null }, json: async () => body });
};
const sandbox = new Proxy({
  location: { hash: '#accounts' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert,
              getElementById: id => (sinks[id] || inert),
              addEventListener() {}, documentElement: inert, head: inert, body: inert,
              createElement: () => inert, cookie: '' },
  fetch: fetchStub,
  toast: () => {},
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  MutationObserver: class { constructor() {} observe() {} disconnect() {} takeRecords() { return []; } },
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set,
  RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent,
  decodeURIComponent, URL, URLSearchParams, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
vm.runInContext(src + '\nthis.__r = {' +
  ' loadSummary, loadOverview, ensureFullOverview, invalidateFullOverview, refreshAccountsView,' +
  ' switchAccTab, renderAccounts,' +
  ' setFilters: f => { accFilters = f; },' +
  ' setTab: t => { accTab = t; },' +
  ' state: () => ({ overviewAccounts: ((overviewData && overviewData.accounts) || []).length,' +
  '                 overviewAt: overviewAt, credits: creditsSum, base: pollBaseSec,' +
  '                 rows: accAll.length, meta: accPageMeta,' +
  '                 tpl: document.getElementById(\'accBody\').innerHTML }),' +
  '};', sandbox, { filename: 'app.js' });
const R = sandbox.__r;
const wait = async (fn, ms) => {
  const t0 = Date.now();
  while (Date.now() - t0 < (ms || 3000)) {
    if (fn()) return true;
    await new Promise(r => setTimeout(r, 10));
  }
  return false;
};
const fullCount = () => calls.filter(u => u.indexOf('/panel/api/overview') >= 0 && u.indexOf('lite=1') < 0).length;
const liteCount = () => calls.filter(u => u.indexOf('overview?lite=1') >= 0).length;
const accCount = () => calls.filter(u => u.indexOf('/panel/api/accounts?') >= 0).length;
(async () => {
  // 首屏三路请求都回来后（rows/credits 已就位）再断言，避免异步竞态。
  await wait(() => R.state().overviewAccounts > 0 && R.state().rows >= 2 && R.state().credits && R.state().tpl.length > 0);
  const out = {};
  out.initial = { lite: liteCount(), full: fullCount(), accounts: accCount(),
                  pageUrl: calls.find(u => u.indexOf('/panel/api/accounts?') >= 0) || '',
                  credits: R.state().credits, templateRows: R.state().tpl.split('<tr').length - 1 };
  const f0 = fullCount();
  await R.ensureFullOverview(true);
  out.freshTtlReused = fullCount() === f0;
  R.invalidateFullOverview();
  await R.ensureFullOverview(true);
  out.staleRefetched = fullCount() === f0 + 1;
  const f1 = fullCount(), a1 = accCount();
  await R.loadSummary(true);
  out.summaryOnly = fullCount() === f1 && accCount() === a1 && R.state().overviewAccounts === 2;
  // 积分合计：lite overview 的 credits_sum / credits_total_sum 优先（5s 轮询即可刷新
  // 统计条，不必等全量 overview）；缺这两个字段时保持上一次的值（旧后端/mock 兼容）。
  lite.credits_sum = 99; lite.credits_total_sum = 200;
  await R.loadSummary(true);
  out.creditsFromLite = { credits: R.state().credits, text: sinks.sCredits.textContent };
  delete lite.credits_sum; delete lite.credits_total_sum;
  await R.loadSummary(true);
  out.creditsFallbackKeep = { credits: R.state().credits, text: sinks.sCredits.textContent };
  R.setTab('list');
  const f2 = fullCount();
  await R.refreshAccountsView(true);
  out.listTabNoFull = fullCount() === f2;
  R.setTab('expiry');
  R.invalidateFullOverview();
  const f3 = fullCount();
  await R.refreshAccountsView(true);
  out.expiryTabFull = fullCount() === f3 + 1;
  // 服务端分页渲染：本地筛选条件故意设成「全不匹配」，rows 仍应原样渲染（证明不再本地 filter）；
  // 冷却时间来自 until（light 投影无 cool_remaining_sec）。
  const nowMs = Date.now();
  const rows = [
    { uid: 'u1', nickname: '甲', credits: 5, until: new Date(nowMs + 90000).toISOString() },
    { uid: 'u2', nickname: '乙', credits: 3, disabled: true, disabled_reason: '凭证失效' },
  ];
  R.setFilters({ q: '不匹配', state: 'ok', ver: 'enterprise' });
  R.renderAccounts(rows, { total: 3, filtered: 2, page: 1, pages: 2, now: Math.floor(nowMs / 1000) });
  const html = sinks.accBody.innerHTML;
  out.serverRender = {
    rendersServerRows: html.indexOf('甲') >= 0 && html.indexOf('乙') >= 0,
    coolTag: html.indexOf('限流冷却') >= 0 && html.indexOf('class="cool"') >= 0,
    disabledTag: html.indexOf('已禁用') >= 0,
    count: sinks.accCount.textContent, info: sinks.accPageInfo.textContent,
    prev: sinks.accPrev.disabled, next: sinks.accNext.disabled,
    rows: R.state().rows,
  };
  // 无 meta = 兼容旧调用：退回本地筛选/分页（渲染器冒烟与切片 harness 依赖）。
  R.setFilters({ q: '', state: '', ver: '' });
  R.renderAccounts(rows);
  out.legacyRender = { count: sinks.accCount.textContent, info: sinks.accPageInfo.textContent,
                       next: sinks.accNext.disabled, rows: R.state().rows, meta: R.state().meta };
  // 服务端空页要区分「筛选无命中」与「池为空」。
  R.renderAccounts([], { total: 5, filtered: 0, page: 1, pages: 1, now: now });
  out.emptyFiltered = { html: sinks.accBody.innerHTML, count: sinks.accCount.textContent };
  R.renderAccounts([], { total: 0, filtered: 0, page: 1, pages: 1, now: now });
  out.emptyPool = { html: sinks.accBody.innerHTML };
  // v2.3.1：带标签筛选且池里有账号 → 明确提示「无账号使用该标签」。
  R.setFilters({ q: '', state: '', ver: '', tag: 'vip' });
  R.renderAccounts([], { total: 5, filtered: 0, page: 1, pages: 1, now: now });
  out.emptyTag = { html: sinks.accBody.innerHTML, count: sinks.accCount.textContent };
  // 标签下拉 change：回第 1 页并带 tag= 重新请求（URLSearchParams 只编码一次）。
  const aTag = accCount();
  sinks.accTagFilter.value = '采购A';
  sinks.accTagFilter.onchange();
  await wait(() => accCount() > aTag);
  const tagUrl = calls.filter(u => u.indexOf('/panel/api/accounts?') >= 0).pop() || '';
  out.tagChange = { requests: accCount() - aTag,
                    hasTag: tagUrl.indexOf('tag=%E9%87%87%E8%B4%ADA') >= 0,
                    page1: tagUrl.indexOf('page=1') >= 0 };
  R.setFilters({ q: '', state: '', ver: '', tag: '' });
  sinks.accTagFilter.value = '';
  // P2-1：账号集合变更（重新扫描）后，除当前页外还后台补拉一次完整 overview，
  // 让积分历史下拉 / 积分条基准 / 到期快照立刻跟上（不再等切到期 tab 或整页刷新）。
  const f4 = fullCount(), l4 = liteCount(), a4 = accCount();
  await sinks.btnRescan.onclick();
  await wait(() => fullCount() === f4 + 1 && accCount() > a4);
  out.mutationRefetch = {
    fullRefetched: fullCount() === f4 + 1,
    liteRefreshed: liteCount() > l4,
    pageRefreshed: accCount() > a4,
    overviewAccounts: R.state().overviewAccounts,
  };
  // P2-2：page_size=0（全部）被服务端 2000 上限截断时给出提示；未截断/普通分页不显示。
  R.setFilters({ q: '', state: '', ver: '' });
  R.renderAccounts(rows, { total: 2500, filtered: 2500, page: 1, pages: 1, page_size: 0, now: now });
  out.truncHint = { hidden: sinks.accTruncHint.hidden, text: sinks.accTruncHint.textContent,
                    count: sinks.accCount.textContent };
  // 带筛选时 total 是池总数：命中 2 个 < 上限，不构成截断（用 filtered 判断）。
  R.renderAccounts(rows, { total: 2500, filtered: 2, page: 1, pages: 1, page_size: 0, now: now });
  out.truncFiltered = { hidden: sinks.accTruncHint.hidden, text: sinks.accTruncHint.textContent };
  R.renderAccounts(rows, { total: 2, filtered: 2, page: 1, pages: 1, page_size: 0, now: now });
  out.truncNone = { hidden: sinks.accTruncHint.hidden, text: sinks.accTruncHint.textContent,
                    count: sinks.accCount.textContent };
  R.renderAccounts(rows, { total: 2500, filtered: 2500, page: 1, pages: 2, page_size: 100, now: now });
  out.truncPaged = { hidden: sinks.accTruncHint.hidden, count: sinks.accCount.textContent };
  process.stdout.write(JSON.stringify(out));
  process.exit(0);
})().catch(e => { console.error(e); process.exit(1); });`
	want := `{"initial":{"lite":2,"full":1,"accounts":1,` +
		`"pageUrl":"/panel/api/accounts?page=1&page_size=100",` +
		`"credits":{"rem":15,"tot":30},"templateRows":2},` +
		`"freshTtlReused":true,"staleRefetched":true,"summaryOnly":true,` +
		`"creditsFromLite":{"credits":{"rem":99,"tot":200},"text":"99 / 200"},` +
		`"creditsFallbackKeep":{"credits":{"rem":99,"tot":200},"text":"99 / 200"},` +
		`"listTabNoFull":true,"expiryTabFull":true,` +
		`"serverRender":{"rendersServerRows":true,"coolTag":true,"disabledTag":true,` +
		`"count":"命中 2 / 共 3 个","info":"1 / 2","prev":true,"next":false,"rows":2},` +
		`"legacyRender":{"count":"共 2 个","info":"1 / 1","next":true,"rows":2,"meta":null},` +
		`"emptyFiltered":{"html":"<tr><td colspan=\"9\"><div class=\"empty\">没有符合筛选条件的账号</div></td></tr>",` +
		`"count":"命中 0 / 共 5 个"},` +
		`"emptyPool":{"html":"<tr><td colspan=\"9\"><div class=\"empty\">账号池是空的</div></td></tr>"},` +
		`"emptyTag":{"html":"<tr><td colspan=\"9\"><div class=\"empty\">无账号使用该标签「vip」</div></td></tr>",` +
		`"count":"命中 0 / 共 5 个"},` +
		`"tagChange":{"requests":1,"hasTag":true,"page1":true},` +
		`"mutationRefetch":{"fullRefetched":true,"liteRefreshed":true,"pageRefreshed":true,"overviewAccounts":2},` +
		`"truncHint":{"hidden":false,"text":"仅显示前 2 个（服务端上限 2000，建议用筛选或分页）","count":"共 2500 个"},` +
		`"truncFiltered":{"hidden":true,"text":""},` +
		`"truncNone":{"hidden":true,"text":"","count":"共 2 个"},` +
		`"truncPaged":{"hidden":true,"count":"共 2500 个"}}`
	if got := feNode(t, script); got != want {
		t.Fatalf("账号池服务端分页/懒加载=%s\nwant %s", got, want)
	}
}

// TestPanelAccountsServerPagingWiring 服务端分页/懒加载的源码接线（防「函数写了但
// 主链路没接上」）：账号视图轮询必须走 refreshAccountsView、全量 overview 只能从
// ensureFullOverview/loadOverview 进、列表 tab 不拉全量、旧的全量渲染断言已移除。
func TestPanelAccountsServerPagingWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	j := string(js)
	for _, want := range []string{
		"function accStateParam(",
		"function accQueryString(",
		"async function apiCached(",
		"async function loadSummary(",
		"async function loadOverview(",
		"async function ensureFullOverview(",
		"async function loadAccountPage(",
		"async function refreshAccountsView(",
		"async function refreshAccountPool(",
		"apiCached('accounts?' + qs, ACCOUNTS_CACHE)",
		"if (view === 'accounts') return refreshAccountsView(true)",
		"if (accTab === 'expiry' && view === 'accounts')",
		"const ACCOUNTS_CACHE = {}",
		"let accPageMeta = null;",
		// 版本筛选保持旧 UI 三分法（国内/海外排除企业号）；服务端 realm/enterprise 正交。
		"p.set('realm', 'global'); p.set('enterprise', '0')",
		"p.set('realm', 'cn'); p.set('enterprise', '0')",
		// 积分合计优先用后端下发的 credits_sum / credits_total_sum（lite 也带）。
		"d.credits_sum != null || d.credits_total_sum != null",
		// 304 倒计时：200 校准 skew，渲染用 Date.now()-skew。
		"accClockSkew = Date.now() - Number(d.now) * 1000",
		"skew: accClockSkew",
		"const nowMs = server ? accNowMs(meta) : Date.now();",
	} {
		if !strings.Contains(j, want) {
			t.Errorf("app.js 服务端分页接线缺少 %q", want)
		}
	}
	// lite overview：loadSummary 与鉴权探测各一处；完整 overview 只允许一处（loadOverview）。
	if n := strings.Count(j, "'overview?lite=1'"); n < 2 {
		t.Errorf("app.js 只有 %d 处 lite overview（loadSummary/checkAuthGate 应各一处）", n)
	}
	if n := strings.Count(j, "api('overview')"); n != 1 {
		t.Errorf("app.js 有 %d 处完整 overview 请求（应只在 loadOverview 内 1 处）", n)
	}
	// 旧实现必须移除：账号视图直接拉全量、全量 overview 直接渲染账号表。
	for _, forbidden := range []string{
		"if (view === 'accounts') return loadOverview(true)",
		"renderAccounts(d.accounts || [])",
	} {
		if strings.Contains(j, forbidden) {
			t.Errorf("app.js 仍残留旧的全量账号路径 %q", forbidden)
		}
	}
	// 筛选项/分页控件保持原 id（mobile.css 与既有 harness 依赖），全部选项注明服务端上限。
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	for _, want := range []string{`id="accQ"`, `id="accState"`, `id="accVer"`, `id="accPageSize"`,
		`id="accCount"`, `id="accPrev"`, `id="accNext"`, `全部（上限 2000）`} {
		if !strings.Contains(h, want) {
			t.Errorf("index.html 账号池控件缺少 %q", want)
		}
	}
}

// TestAppJSAccountsCountdownAcross304 P1：304 命中时冷却倒计时不能冻结。
//
// 空闲期账号页 ETag 稳定 → 每轮都是 304，复用缓存的 rows/meta。若直接用响应里的
// now*1000 当「当前时间」，倒计时会停走。实现：200 时校准 skew = Date.now()-now*1000，
// 渲染用 Date.now()-skew（304 时 meta 不刷新，但 Date.now() 继续前进）。
// 本用例用可控时钟模拟「200 一次 → 连续两次 304」，断言倒计时文本按秒减少。
func TestAppJSAccountsCountdownAcross304(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});
// 可控时钟：FakeDate 的 now()/无参构造都读 clock.now，用来模拟 5s 间隔的轮询。
const clock = { now: 1761200000000 };
class FakeDate extends Date {
  constructor(...args) { if (args.length === 0) super(clock.now); else super(...args); }
  static now() { return clock.now; }
  static parse(...a) { return Date.parse(...a); }
  static UTC(...a) { return Date.UTC(...a); }
}
const lite = { version: '2.1.0-pro', uptime_sec: 60, redis_mode: 'memory', sticky_sessions: 0,
  total: 1, healthy: 1, cooling: 0, disabled: 0, paused: 0, in_flight_full: 0,
  model_locks: [], balance_refresh_sec: 300, poll_interval_sec: 5, credits_sum: 1, credits_total_sum: 2 };
const full = Object.assign({}, lite, { accounts: [{ uid: 'u1', nickname: 'X', credits: 1, credits_total: 2 }] });
let accountsCalls = 0;
const calls = [];
const sinks = {};
const sinkIds = ['accBody', 'accCount', 'accPageInfo', 'accPrev', 'accNext', 'sTotal', 'sHealthy',
  'sCooling', 'sDisabled', 'sPaused', 'sCredits', 'sSticky', 'navSub', 'navVer', 'navRedis',
  'navState', 'navPulse', 'accNote', 'subMeta', 'expBox', 'expList', 'expNote', 'expPad', 'cfgForm'];
for (const id of sinkIds) sinks[id] = { innerHTML: '', textContent: '', hidden: false, disabled: false,
  className: '', value: '', dataset: {}, children: [], addEventListener() {}, style: {} };
const resp = (status, body, etag) => Promise.resolve({
  status, ok: status >= 200 && status < 300,
  headers: { get: k => (k === 'ETag' ? etag : null) },
  json: async () => body,
});
const fetchStub = (url, opts) => {
  const u = String(url);
  calls.push({ url: u, headers: (opts && opts.headers) || {} });
  if (u.indexOf('overview?lite=1') >= 0) return resp(200, lite, null);
  if (u.indexOf('/panel/api/overview') >= 0) return resp(200, full, null);
  if (u.indexOf('/panel/api/accounts?') >= 0) {
    accountsCalls++;
    if (accountsCalls === 1) {
      const row = { uid: 'u1', nickname: 'X', credits: 1, credits_total: 2,
        until: new Date(clock.now + 95000).toISOString(), token_usage: {} };
      return resp(200, { ok: true, now: Math.floor(clock.now / 1000), total: 1, filtered: 1,
        page: 1, page_size: 100, pages: 1, rows: [row] }, 'W/"acct-cd"');
    }
    return resp(304, {}, null);
  }
  return resp(200, {}, null);
};
const sandbox = new Proxy({
  location: { hash: '#accounts' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert,
              getElementById: id => (sinks[id] || inert),
              addEventListener() {}, documentElement: inert, head: inert, body: inert,
              createElement: () => inert, cookie: '' },
  fetch: fetchStub,
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  MutationObserver: class { constructor() {} observe() {} disconnect() {} takeRecords() { return []; } },
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date: FakeDate, Number, String, Boolean, Object, Array, Promise, Map, Set,
  RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent,
  decodeURIComponent, URL, URLSearchParams, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
vm.runInContext(src + '\nthis.__r = { loadAccountPage,' +
  ' tpl: () => document.getElementById(\'accBody\').innerHTML };', sandbox, { filename: 'app.js' });
const R = sandbox.__r;
const wait = async (fn, ms) => {
  const t0 = Date.now();
  while (Date.now() - t0 < (ms || 3000)) { if (fn()) return true; await new Promise(r => setTimeout(r, 10)); }
  return false;
};
const parseSec = html => {
  const m = html.match(/(\d+)分(\d+)秒/);
  if (m) return Number(m[1]) * 60 + Number(m[2]);
  const s = html.match(/(\d+)秒/);
  return s ? Number(s[1]) : -1;
};
(async () => {
  // 首屏 200 渲染出冷却倒计时（until = 现在 + 95s）
  await wait(() => sinks.accBody.innerHTML.indexOf('分') >= 0 && sinks.accBody.innerHTML.indexOf('限流冷却') >= 0);
  const out = {};
  out.first = parseSec(sinks.accBody.innerHTML);
  const callIdx = calls.length;
  clock.now += 5000;                       // 模拟 5s 后的下一次轮询
  await R.loadAccountPage(true);           // 304：复用缓存 rows/meta
  out.second = parseSec(sinks.accBody.innerHTML);
  clock.now += 5000;
  await R.loadAccountPage(true);           // 再来一次 304
  out.third = parseSec(sinks.accBody.innerHTML);
  const accCalls = calls.slice(callIdx).filter(c => c.url.indexOf('/panel/api/accounts?') >= 0);
  out.conditional = accCalls.map(c => c.headers['If-None-Match'] || '');
  out.accountsCalls = accountsCalls;
  out.decreased5 = out.second === out.first - 5;
  out.decreased10 = out.third === out.first - 10;
  process.stdout.write(JSON.stringify(out));
  process.exit(0);
})().catch(e => { console.error(e); process.exit(1); });`
	want := `{"first":95,"second":90,"third":85,` +
		`"conditional":["W/\"acct-cd\"","W/\"acct-cd\""],` +
		`"accountsCalls":3,"decreased5":true,"decreased10":true}`
	if got := feNode(t, script); got != want {
		t.Fatalf("304 倒计时=%s\nwant %s", got, want)
	}
}

// TestAppJSAuditAndTaskRuns v2.2.0 #5/#9 只读 UI 的渲染与加载路径（node 切片）。
//
// 覆盖：时间格式化容错（空/脏/秒/毫秒）、字段别名兜底（ts/time、ip/client_ip、
// action/act、runs 字段）、ok 缺失显示「—」、动作/类型/触发中文标签、表格行与列数、
// 空态提示、点击加载的接口路径与失败行内错误态（+toast、按钮恢复可重试）。
func TestAppJSAuditAndTaskRuns(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const AUDIT_LIMIT = 200;');
const end = src.indexOf("if ($('btnAuditLoad'))");
if (start < 0 || end < 0 || end < start) throw new Error('audit section not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '', hidden: false, className: '', disabled: false });
const ctx = {
  esc: s => String(s == null ? '' : s),
  $: id => (sinks[id] || mk(id)),
  Date, isNaN, isFinite, Number, String, Array, Object, Math, JSON,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.__r = {' +
  ' normalizeAuditEntries, renderAudit, auditActionText, auditUidText, fmtRunTime,' +
  ' normalizeTaskRuns, renderTaskRuns, loadAudit, loadTaskRuns,' +
  '};', ctx);
const R = ctx.__r;
(async () => {
  const out = {};
  out.fmt = {
    empty: R.fmtRunTime(''),
    missing: R.fmtRunTime(null),
    bogus: R.fmtRunTime('not-a-time'),
    zero: R.fmtRunTime(0),
    sameSecMs: R.fmtRunTime(1700000000) === R.fmtRunTime(1700000000000),
    shaped: /^\d\d-\d\d \d\d:\d\d:\d\d$/.test(R.fmtRunTime(1700000000)),
  };
  const auditNorm = R.normalizeAuditEntries({ entries: [
    { ts: 1700000000, ip: '1.2.3.4', action: 'account_remove', uid: '1234567890abcdefXYZ', ok: false, detail: '删凭证失败' },
    { time: '2026-01-02T03:04:05Z', client_ip: '5.6.7.8', act: 'checkin_all', success: true, message: 'ok' },
    {},
  ] });
  out.auditNorm = {
    count: auditNorm.length,
    failOk: auditNorm[0].ok === false,
    uidShort: R.auditUidText(auditNorm[0].uid),
    aliasIp: auditNorm[1].ip,
    aliasOk: auditNorm[1].ok === true,
    aliasAction: R.auditActionText(auditNorm[1].action),
    missingOkNull: auditNorm[2].ok === null,
    missingActionText: R.auditActionText(auditNorm[2].action),
    missingIp: auditNorm[2].ip,
    unknownActionText: R.auditActionText('brand_new_action'),
    notArrayCount: R.normalizeAuditEntries({ entries: 'oops' }).length,
  };
  R.renderAudit(auditNorm);
  const ah = sinks.auditBody.innerHTML;
  out.auditRender = {
    stateHidden: sinks.auditState.hidden,
    tableShown: sinks.auditTable.hidden === false,
    note: sinks.auditNote.textContent,
    rows: (ah.match(/<tr>/g) || []).length,
    cells: (ah.match(/<td/g) || []).length,
    hasRemoved: ah.indexOf('移除账号') >= 0,
    hasAllCheckin: ah.indexOf('全部签到') >= 0,
    hasFail: ah.indexOf('失败') >= 0,
    hasOk: ah.indexOf('成功') >= 0,
    hasUnknown: ah.indexOf('<span class="tag mute">—</span>') >= 0,
    hasDetail: ah.indexOf('删凭证失败') >= 0,
  };
  R.renderAudit([]);
  out.auditEmpty = {
    stateHidden: sinks.auditState.hidden, stateText: sinks.auditState.textContent,
    tableHidden: sinks.auditTable.hidden, note: sinks.auditNote.textContent,
    body: sinks.auditBody.innerHTML,
  };
  const runsNorm = R.normalizeTaskRuns({ runs: [
    { id: 'r1', kind: 'checkin', trigger: 'manual', started_at: 1700000000,
      finished_at: 1700000060, total: 3, done: 2, fail: 1, skip: 0, error: '' },
    { task: 'balance', started: 1700000100, count: 5, success: 5, skipped: 0 },
  ] });
  out.runsNorm = {
    count: runsNorm.length,
    kindAlias: runsNorm[1].kind,
    triggerMissing: runsNorm[1].trigger,
    startedAlias: runsNorm[1].started_at,
    failDefault: runsNorm[1].fail,
    finishedMissing: runsNorm[1].finished_at,
    notArrayCount: R.normalizeTaskRuns({ runs: null }).length,
  };
  R.renderTaskRuns(runsNorm);
  const rh = sinks.runsBody.innerHTML;
  out.runsRender = {
    stateHidden: sinks.runsState.hidden,
    tableShown: sinks.runsTable.hidden === false,
    note: sinks.runsNote.textContent,
    rows: (rh.match(/<tr>/g) || []).length,
    cells: (rh.match(/<td/g) || []).length,
    hasCheckin: rh.indexOf('签到') >= 0,
    hasBalance: rh.indexOf('刷新余额') >= 0,
    hasManual: rh.indexOf('手动') >= 0,
    hasCounts: rh.indexOf('>3<') >= 0 && rh.indexOf('>2<') >= 0 && rh.indexOf('>1<') >= 0 && rh.indexOf('>5<') >= 0,
    hasErrorDash: rh.indexOf('>—</td>') >= 0,
  };
  R.renderTaskRuns([]);
  out.runsEmpty = {
    stateHidden: sinks.runsState.hidden, stateText: sinks.runsState.textContent,
    tableHidden: sinks.runsTable.hidden, note: sinks.runsNote.textContent, body: sinks.runsBody.innerHTML,
  };
  let apiPath = '', apiErr = null;
  const toasts = [];
  ctx.api = async p => { apiPath = p; if (apiErr) throw new Error('HTTP 500'); return { entries: [{ ts: 0, action: 'login', ok: true }] }; };
  ctx.toast = (m, c) => toasts.push([m, c || '']);
  sinks.btnAuditLoad = { textContent: '加载', disabled: false };
  await R.loadAudit();
  out.loadAuditOk = { path: apiPath, rows: (sinks.auditBody.innerHTML.match(/<tr>/g) || []).length,
                      stateHidden: sinks.auditState.hidden, btn: sinks.btnAuditLoad.textContent };
  apiErr = 'HTTP 500';
  await R.loadAudit();
  out.loadAuditErr = { stateHidden: sinks.auditState.hidden, stateClass: sinks.auditState.className,
                       toast: toasts.length ? toasts[toasts.length - 1][0] : '',
                       btn: sinks.btnAuditLoad.textContent, btnDisabled: sinks.btnAuditLoad.disabled };
  let runPath = '';
  ctx.api = async p => { runPath = p; return { runs: [] }; };
  sinks.btnRunsLoad = { textContent: '加载', disabled: false };
  await R.loadTaskRuns();
  out.loadRuns = { path: runPath, stateHidden: sinks.runsState.hidden,
                   stateText: sinks.runsState.textContent, btn: sinks.btnRunsLoad.textContent };
  process.stdout.write(JSON.stringify(out));
  process.exit(0);
})().catch(e => { console.error(e); process.exit(1); });`
	want := `{"fmt":{"empty":"—","missing":"—","bogus":"—","zero":"—","sameSecMs":true,"shaped":true},` +
		`"auditNorm":{"count":3,"failOk":true,"uidShort":"1234567890abcdef…","aliasIp":"5.6.7.8",` +
		`"aliasOk":true,"aliasAction":"全部签到","missingOkNull":true,"missingActionText":"—",` +
		`"missingIp":"","unknownActionText":"brand_new_action","notArrayCount":0},` +
		`"auditRender":{"stateHidden":true,"tableShown":true,"note":"最近 3 条","rows":3,"cells":18,` +
		`"hasRemoved":true,"hasAllCheckin":true,"hasFail":true,"hasOk":true,"hasUnknown":true,"hasDetail":true},` +
		`"auditEmpty":{"stateHidden":false,"stateText":"暂无审计记录（面板写操作成功后会自动留痕）",` +
		`"tableHidden":true,"note":"—","body":""},` +
		`"runsNorm":{"count":2,"kindAlias":"balance","triggerMissing":"","startedAlias":1700000100,` +
		`"failDefault":0,"finishedMissing":"","notArrayCount":0},` +
		`"runsRender":{"stateHidden":true,"tableShown":true,"note":"最近 2 条","rows":2,"cells":18,` +
		`"hasCheckin":true,"hasBalance":true,"hasManual":true,"hasCounts":true,"hasErrorDash":true},` +
		`"runsEmpty":{"stateHidden":false,"stateText":"暂无任务执行记录（批量任务/单号任务结束后自动记录）",` +
		`"tableHidden":true,"note":"—","body":""},` +
		`"loadAuditOk":{"path":"audit?limit=200","rows":1,"stateHidden":true,"btn":"加载"},` +
		`"loadAuditErr":{"stateHidden":false,"stateClass":"state err",` +
		`"toast":"读取操作审计失败：HTTP 500","btn":"加载","btnDisabled":false},` +
		`"loadRuns":{"path":"task_runs?limit=100","stateHidden":false,` +
		`"stateText":"暂无任务执行记录（批量任务/单号任务结束后自动记录）","btn":"加载"}}`
	if got := feNode(t, script); got != want {
		t.Fatalf("审计/任务历史渲染=%s\nwant %s", got, want)
	}
}

// TestPanelAuditTaskRunsWiring 审计/任务历史 + 两个 P2 修复的源码接线：
// 控件 id 双侧存在、接口路径/limit 正确、只在点击时拉取（无轮询）、
// 账号集合变更分支统一走 refreshAccountPoolAfterMutation（会补拉全量 overview）、
// page_size=0 截断提示元素已接入。
func TestPanelAuditTaskRunsWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	j := string(js)
	for _, want := range []string{
		"const AUDIT_LIMIT = 200;",
		"const TASK_RUNS_LIMIT = 100;",
		"api('audit?limit=' + AUDIT_LIMIT)",
		"api('task_runs?limit=' + TASK_RUNS_LIMIT)",
		"function normalizeAuditEntries(",
		"function normalizeTaskRuns(",
		"function renderAudit(",
		"function renderTaskRuns(",
		"$('btnAuditLoad').onclick = loadAudit;",
		"$('btnRunsLoad').onclick = loadTaskRuns;",
		// P2-1：账号集合变更统一收尾（当前页 + 静默补拉全量 overview）。
		"function refreshFullOverviewInBackground(",
		"function refreshAccountPoolAfterMutation(",
		"ensureFullOverview(true, true)",
		// P2-2：page_size=0 被 2000 上限截断时的提示。
		"const ACC_MAX_PAGE_SIZE = 2000;",
		"$('accTruncHint').hidden = !truncated;",
		"建议用筛选或分页",
	} {
		if !strings.Contains(j, want) {
			t.Errorf("app.js 缺少 %q", want)
		}
	}
	// 只读区块只在点击时拉取：函数定义各 1 处、无 setInterval 轮询。
	if n := strings.Count(j, "loadAudit()"); n != 1 {
		t.Errorf("app.js 出现 %d 处 loadAudit()（应只有函数定义，仅点击触发）", n)
	}
	if n := strings.Count(j, "loadTaskRuns()"); n != 1 {
		t.Errorf("app.js 出现 %d 处 loadTaskRuns()（应只有函数定义，仅点击触发）", n)
	}
	for _, forbidden := range []string{"setInterval(loadAudit", "setInterval(loadTaskRuns"} {
		if strings.Contains(j, forbidden) {
			t.Errorf("审计/任务历史不应轮询：%q", forbidden)
		}
	}
	// 账号集合变更的 4 个分支（删号/重扫/登录成功/导入成功）+ 定义 = ≥5 处引用。
	if n := strings.Count(j, "refreshAccountPoolAfterMutation("); n < 5 {
		t.Errorf("refreshAccountPoolAfterMutation 引用 %d 处（定义 + 删号/重扫/登录/导入 至少 5 处）", n)
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	for _, want := range []string{
		`id="accTruncHint"`,
		`id="auditBox"`, `id="btnAuditLoad"`, `id="auditTable"`, `id="auditBody"`,
		`id="auditState"`, `id="auditNote"`,
		`id="runsBox"`, `id="btnRunsLoad"`, `id="runsTable"`, `id="runsBody"`,
		`id="runsState"`, `id="runsNote"`,
		"操作审计", "任务执行历史", "来源 IP", "触发", "跳过",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("index.html 缺少 %q", want)
		}
	}
}

// TestAppJSJobsProgressAndCancel v2.3.0 #10 的 Job 进度条/取消/批量按钮交互（node 切片）。
//
// 覆盖：jobs 归一化（字段别名/缺失）、只展示 running（pending/done 不显示）、
// 进度百分比/失败/跳过/触发来源文案、取消确认（拒绝不发请求、确认发 POST、失败 toast）、
// 后端未提供 jobs 接口（404）时停止空打（兜底旧行为）、批量按钮成功 toast 指向顶部进度条、
// 409 原样展示后端 error 文案。
func TestAppJSJobsProgressAndCancel(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const JOBS_LIMIT = 20;');
const end = src.indexOf("\nif ($('btnJobCancel')) $('btnJobCancel').onclick = cancelJob;");
if (start < 0 || end < 0 || end < start) throw new Error('jobs section not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '', hidden: false, disabled: false, className: '',
  style: {}, value: '', dataset: {}, children: [], classList: { add() {}, remove() {}, toggle() {} } });
const toasts = [];
const apiCalls = [];
let apiImpl = async () => ({});
let confirmAnswer = false;
const ctx = {
  fmtRunTime: v => (v ? 'T:' + v : '—'),
  $: id => (sinks[id] || mk(id)),
  api: async (p, o) => { apiCalls.push([p, o && o.method]); return apiImpl(p, o); },
  toast: (m, c) => toasts.push([m, c || '']),
  confirm: () => confirmAnswer,
  Date, Number, String, Object, Array, Math, JSON,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.__job = {' +
  ' normalizeJob, normalizeJobs, pickActiveJob, jobKindText, jobStateText, jobTriggerText,' +
  ' renderJobBar, loadJobs, cancelJob, runBatchTask,' +
  ' state: () => ({ active: jobsActive, supported: jobsSupported, loading: jobsLoading }),' +
  ' reset: () => { jobsActive = null; jobsSupported = true; jobsLoading = false; },' +
  '};', ctx);
const J = ctx.__job;
(async () => {
  const out = {};
  const jobs = [
    { id: 'j0', kind: 'travel', trigger: 'schedule', state: 'done', total: 3, done: 3, fail: 0, skip: 0 },
    // 后端实际契约：started_at 是 RFC3339 字符串；done 含失败不含跳过 → 已处理 = done+skip。
    { id: 'j1', kind: 'checkin', trigger: 'manual', state: 'running', total: 10, done: 3, fail: 1, skip: 2,
      started_at: '2026-10-24T04:16:40.123+08:00' },
    { id: 'j2', kind: 'balance', trigger: 'manual', state: 'pending', total: 5, done: 0 },
  ];
  const norm = J.normalizeJobs({ jobs: jobs });
  out.norm = { count: norm.length, id: norm[1].id, kind: norm[1].kind, done: norm[1].done,
               fail: norm[1].fail, skip: norm[1].skip, trigger: norm[1].trigger,
               startedIsString: typeof norm[1].started_at === 'string' };
  out.pick = J.pickActiveJob(norm).id;
  out.pickNone = J.pickActiveJob([{ id: 'x', state: 'done' }, { id: 'y', state: 'pending' }]) === null;
  // 未知/中断状态按兜底标签展示（interrupted = 进程重启时上次未完成的 Job）
  out.labels = [J.jobKindText('checkin'), J.jobStateText('running'),
                J.jobStateText('interrupted'), J.jobTriggerText('manual')];
  J.renderJobBar(norm);
  out.render = { hidden: sinks.jobBar.hidden, kind: sinks.jobKind.textContent,
                 prog: sinks.jobProgText.textContent, meta: sinks.jobMeta.textContent,
                 width: sinks.jobBarFill.style.width, cancelDisabled: sinks.btnJobCancel.disabled };
  J.renderJobBar([{ id: 'x', state: 'done', kind: 'checkin' }]);
  out.hideWhenNoRunning = sinks.jobBar.hidden;
  J.reset();
  apiImpl = async () => ({ ok: true, jobs: jobs });
  await J.loadJobs(true);
  out.load = { path: apiCalls[apiCalls.length - 1][0], active: J.state().active && J.state().active.id };
  // 后端未就绪（404）：隐藏进度条并停止空打
  J.reset(); apiCalls.length = 0;
  apiImpl = async () => { throw new Error('HTTP 404'); };
  await J.loadJobs(true);
  const afterFirst404 = apiCalls.length;
  await J.loadJobs(true);
  out.unsupported = { supported: J.state().supported, first404Calls: afterFirst404,
                      retriedCalls: apiCalls.length, hidden: sinks.jobBar.hidden };
  J.reset();
  apiImpl = async () => ({ ok: true, jobs: [jobs[1]] });
  await J.loadJobs(true);
  J.renderJobBar([jobs[1]]);   // 取消场景逐项重置为「有 running job」状态（防异步 loadJobs 清空）
  confirmAnswer = false; apiCalls.length = 0; toasts.length = 0;
  await J.cancelJob();
  out.cancelNo = { calls: apiCalls.length, toasts: toasts.length };
  confirmAnswer = true; apiImpl = async () => ({ ok: true });
  J.renderJobBar([jobs[1]]);
  apiCalls.length = 0; toasts.length = 0;
  await J.cancelJob();
  out.cancelYes = { calls: apiCalls.slice(), toast: toasts[0] && toasts[0][0], cls: toasts[0] && toasts[0][1] };
  apiImpl = async () => { throw new Error('HTTP 409'); };
  J.renderJobBar([jobs[1]]);
  apiCalls.length = 0; toasts.length = 0;
  await J.cancelJob();
  out.cancelFail = { toast: toasts[0] && toasts[0][0], cls: toasts[0] && toasts[0][1] };
  // canceled:false = 请求到达时任务已结束（后端契约：canceled 布尔）
  apiImpl = async () => ({ ok: true, canceled: false });
  J.renderJobBar([jobs[1]]);
  toasts.length = 0;
  await J.cancelJob();
  out.cancelAlready = { toast: toasts[0] && toasts[0][0], cls: toasts[0] && toasts[0][1] };
  apiCalls.length = 0; toasts.length = 0;
  apiImpl = async () => ({ ok: true, started: true, job_id: 'j9' });
  const btn = { disabled: false };
  await J.runBatchTask('checkin_all', '全部签到', btn);
  out.batchOk = { call: apiCalls[0] && apiCalls[0][0], method: apiCalls[0] && apiCalls[0][1],
                  toast: toasts[0] && toasts[0][0], cls: toasts[0] && toasts[0][1], btn: btn.disabled };
  apiCalls.length = 0; toasts.length = 0;
  apiImpl = async () => { throw new Error('签到任务正在执行中，请稍后再试'); };
  await J.runBatchTask('checkin_all', '全部签到', btn);
  out.batch409 = { toast: toasts[0] && toasts[0][0], cls: toasts[0] && toasts[0][1] };
  process.stdout.write(JSON.stringify(out));
  process.exit(0);
})().catch(e => { console.error(e); process.exit(1); });`
	want := `{"norm":{"count":3,"id":"j1","kind":"checkin","done":3,"fail":1,"skip":2,"trigger":"manual",` +
		`"startedIsString":true},` +
		`"pick":"j1","pickNone":true,"labels":["全部签到","执行中","已中断","手动"],` +
		`"render":{"hidden":false,"kind":"全部签到（执行中）","prog":"5 / 10 · 失败 1 · 跳过 2",` +
		`"meta":"手动 · 开始 T:2026-10-24T04:16:40.123+08:00","width":"50%","cancelDisabled":false},` +
		`"hideWhenNoRunning":true,` +
		`"load":{"path":"jobs?limit=20","active":"j1"},` +
		`"unsupported":{"supported":false,"first404Calls":1,"retriedCalls":1,"hidden":true},` +
		`"cancelNo":{"calls":0,"toasts":0},` +
		`"cancelYes":{"calls":[["jobs/j1/cancel","POST"],["jobs?limit=20",null]],` +
		`"toast":"已请求取消：当前账号跑完后停止后续","cls":"ok"},` +
		`"cancelFail":{"toast":"取消失败：HTTP 409","cls":"err"},` +
		`"cancelAlready":{"toast":"任务已结束，无需取消","cls":"ok"},` +
		`"batchOk":{"call":"checkin_all","method":"POST",` +
		`"toast":"全部签到已开始（job 进度见顶部）","cls":"ok","btn":false},` +
		`"batch409":{"toast":"全部签到失败：签到任务正在执行中，请稍后再试","cls":"err"}}`
	if got := feNode(t, script); got != want {
		t.Fatalf("Job 进度/取消=%s\nwant %s", got, want)
	}
}

// TestAppJSMetaTagsAndMigration v2.3.0 #12 的标签解析/展示优先级/服务端加载/一次性迁移。
//
// 覆盖：tags 分隔（逗号/全角逗号/空格）、去重、≤10 个、每个 ≤32 字；备注展示
// 「服务端优先、localStorage 降级（uid8）」；账号行标签最多 3 个 + 「+N」；
// GET /meta 成功/失败（失败回落本地）；服务端 meta 为空且本地有旧备注时按完整 uid
// 批量 PUT 迁移一次并清理已迁移本地键；服务端已有 meta 不自动导入；账号列表未就位
// 时等待（不置迁移标记、不误写）。
func TestAppJSMetaTagsAndMigration(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('let metaAll = {};');
const end = src.indexOf("\nif ($('btnRemarks')) $('btnRemarks').onclick = openRemarks;");
if (start < 0 || end < 0 || end < start) throw new Error('meta section not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '', hidden: false, disabled: false, className: '',
  style: {}, value: '', dataset: {}, children: [], classList: { add() {}, remove() {}, toggle() {} } });
const toasts = [];
const apiCalls = [];
let apiImpl = async () => ({ ok: true, meta: {} });
let pool = [];
const state = { view: 'accounts', renders: 0 };
const ctx = {
  LS_REMARKS: 'wb2api.remarks.v1',
  esc: s => String(s == null ? '' : s),
  $: id => (sinks[id] || mk(id)),
  localStorage: {
    store: {},
    getItem(k) { return this.store[k] == null ? null : this.store[k]; },
    setItem(k, v) { this.store[k] = String(v); },
  },
  poolAccountList: () => pool,
  api: async (p, o) => { apiCalls.push([p, o && o.method, o && o.body]); return apiImpl(p, o); },
  toast: (m, c) => toasts.push([m, c || '']),
  get view() { return state.view; },
  renderAccounts: () => { state.renders++; },
  renderUsage: () => {},
  loadAccTags: () => {},   // v2.3.1：loadMeta 成功后会刷新标签下拉
  usageData: null, accAll: [], accPageMeta: null,
  Date, Number, String, Object, Array, Math, JSON, Promise,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.__meta = {' +
  ' parseTags, normalizeMeta, loadMeta, maybeMigrateRemarks, remarkOf, remarkName, accTagsHtml,' +
  ' remarkFormRows, renderRemarkBody, saveRemarkForm, openRemarks, remarksAll, storeRemarks, metaFor,' +
  ' state: () => ({ all: metaAll, loaded: metaLoaded, tried: metaMigrationTried }),' +
  ' reset: () => { metaAll = {}; metaLoaded = false; metaMigrationTried = false; },' +
  ' setMeta: m => { metaAll = m; }, setLoaded: v => { metaLoaded = v; },' +
  '};', ctx);
const M = ctx.__meta;
const flush = async () => { for (let i = 0; i < 8; i++) await Promise.resolve(); };
(async () => {
  const out = {};
  out.parse = {
    basic: M.parseTags('a, b，c  a'),
    empty: M.parseTags(''),
    arr: M.parseTags(['x', 'x', ' y ']),
    cap: M.parseTags(Array.from({ length: 15 }, (_, i) => 't' + i)).length,
    long: M.parseTags('x'.repeat(40))[0].length,
  };
  const norm = M.normalizeMeta({ meta: { u1: { note: 'n1', tags: ['a', ' b '], updated_at: 5 }, u2: {} } });
  out.normalize = { uids: Object.keys(norm).sort(), note: norm.u1.note, tags: norm.u1.tags,
                    at: norm.u1.updated_at, empty: norm.u2.note };
  M.storeRemarks({ abcd1234: '本机备注' });
  out.display = { local: M.remarkOf('abcd1234XYZ'), name: M.remarkName('abcd1234XYZ', '昵称') };
  M.setMeta({ abcd1234XYZ: { note: '服务端备注', tags: [], updated_at: 1 } });
  out.displayServer = { note: M.remarkOf('abcd1234XYZ'), name: M.remarkName('abcd1234XYZ', '昵称') };
  M.setMeta({ u1: { note: 's', tags: ['t1', 't2'], updated_at: 1 },
              u2: { note: '', tags: ['a', 'b', 'c', 'd', 'e'], updated_at: 2 } });
  const t2 = M.accTagsHtml('u1');
  const t5 = M.accTagsHtml('u2');
  out.tags = { none: M.accTagsHtml('nope'),
               twoCount: (t2.match(/class="tag mute"/g) || []).length,
               fiveCount: (t5.match(/class="tag mute"/g) || []).length,
               hasPlus: t5.indexOf('+2') >= 0, two: t2 };
  M.reset(); pool = [{ uid: 'u1', nickname: 'A' }];
  apiImpl = async () => ({ ok: true, meta: { u1: { note: '服务端A', tags: ['vip'], updated_at: 9 } } });
  apiCalls.length = 0;
  out.loadOk = { ret: await M.loadMeta(true), path: apiCalls[0] && apiCalls[0][0],
                 note: M.remarkOf('u1'), localNote: ctx.localStorage.store['wb2api.remarks.v1'] };
  M.reset();
  apiImpl = async () => { throw new Error('HTTP 404'); };
  out.loadFail = { ret: await M.loadMeta(true), localFallback: M.remarkOf('abcd1234XYZ') };
  // 迁移：服务端空 + 本地旧备注（uid8）→ 解析完整 uid 后 PUT 一次，清已迁移本地键
  M.reset(); M.setLoaded(true); pool = [{ uid: 'abcd1234XYZ', nickname: 'A' }];
  M.storeRemarks({ abcd1234: '旧备注', orphankey: '孤立' });
  apiCalls.length = 0; toasts.length = 0;
  apiImpl = async () => ({ ok: true });
  M.maybeMigrateRemarks();
  await flush();
  out.migrate = { calls: apiCalls.map(c => [c[0], c[1]]),
                  body: apiCalls[0] && JSON.parse(apiCalls[0][2]),
                  metaNote: M.metaFor('abcd1234XYZ') && M.metaFor('abcd1234XYZ').note,
                  local: M.remarksAll(), toast: toasts[0] && toasts[0][0] };
  // 服务端已有 meta：不自动导入（避免覆盖），本地保留
  M.reset(); M.setLoaded(true); pool = [{ uid: 'abcd1234XYZ', nickname: 'A' }];
  M.setMeta({ u9: { note: 'x', tags: [], updated_at: 1 } });
  M.storeRemarks({ abcd1234: '旧备注' });
  apiCalls.length = 0;
  M.maybeMigrateRemarks();
  await flush();
  out.migrateSkip = { calls: apiCalls.length, local: M.remarksAll() };
  // 账号列表未就位：不尝试、不置 tried（loadAccountPage 成功后会再调）
  M.reset(); M.setLoaded(true); pool = [];
  apiCalls.length = 0;
  M.maybeMigrateRemarks();
  out.migrateWait = { calls: apiCalls.length, tried: M.state().tried };
  process.stdout.write(JSON.stringify(out));
  process.exit(0);
})().catch(e => { console.error(e); process.exit(1); });`
	want := `{"parse":{"basic":["a","b","c"],"empty":[],"arr":["x","y"],"cap":10,"long":32},` +
		`"normalize":{"uids":["u1","u2"],"note":"n1","tags":["a","b"],"at":5,"empty":""},` +
		`"display":{"local":"本机备注","name":"本机备注<span class=\"rm-ghost\">昵称</span>"},` +
		`"displayServer":{"note":"服务端备注","name":"服务端备注<span class=\"rm-ghost\">昵称</span>"},` +
		`"tags":{"none":"","twoCount":2,"fiveCount":4,"hasPlus":true,` +
		`"two":"<span class=\"acc-tags\"><span class=\"tag mute\" title=\"t1\">t1</span>` +
		`<span class=\"tag mute\" title=\"t2\">t2</span></span>"},` +
		`"loadOk":{"ret":true,"path":"meta","note":"服务端A",` +
		`"localNote":"{\"abcd1234\":\"本机备注\",\"u1\":\"服务端A\"}"},` +
		`"loadFail":{"ret":false,"localFallback":"本机备注"},` +
		`"migrate":{"calls":[["accounts/abcd1234XYZ/meta","PUT"]],"body":{"note":"旧备注","tags":[]},` +
		`"metaNote":"旧备注","local":{"orphankey":"孤立"},` +
		`"toast":"已迁移本机备注到服务端（1 条）"},` +
		`"migrateSkip":{"calls":0,"local":{"abcd1234":"旧备注"}},` +
		`"migrateWait":{"calls":0,"tried":false}}`
	if got := feNode(t, script); got != want {
		t.Fatalf("备注标签/迁移=%s\nwant %s", got, want)
	}
}

// TestAppJSMetaSaveAndRender v2.3.0 #12 的服务端保存/删除/降级与备注弹窗渲染。
//
// 覆盖：改动 → PUT（note+tags）、清空且服务端有记录 → DELETE、未改动跳过、
// 无完整 uid 的旧本机键只写本地不误写服务端；写失败 toast 数量+本地保留+meta 不变；
// 弹窗渲染服务端备注/tags 输入值 + 本机孤儿旧键（data-legacy）；打开时未加载过 meta
// 会先 GET /meta 再渲染。
func TestAppJSMetaSaveAndRender(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('let metaAll = {};');
const end = src.indexOf("\nif ($('btnRemarks')) $('btnRemarks').onclick = openRemarks;");
if (start < 0 || end < 0 || end < start) throw new Error('meta section not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '', hidden: false, disabled: false, className: '',
  style: {}, value: '', dataset: {}, children: [], classList: { on: 0, add() { this.on++; }, remove() { this.on--; }, toggle() {} } });
const toasts = [];
const apiCalls = [];
let apiImpl = async () => ({ ok: true });
let pool = [];
const state = { view: 'accounts', renders: 0, tagsLoads: 0, poolRefreshes: 0 };
const ctx = {
  LS_REMARKS: 'wb2api.remarks.v1',
  esc: s => String(s == null ? '' : s),
  $: id => (sinks[id] || mk(id)),
  localStorage: {
    store: {},
    getItem(k) { return this.store[k] == null ? null : this.store[k]; },
    setItem(k, v) { this.store[k] = String(v); },
  },
  poolAccountList: () => pool,
  api: async (p, o) => { apiCalls.push([p, o && o.method, o && o.body]); return apiImpl(p, o); },
  toast: (m, c) => toasts.push([m, c || '']),
  get view() { return state.view; },
  renderAccounts: () => { state.renders++; },
  renderUsage: () => { state.usageRenders++; },
  // v2.3.1：保存备注/标签后要刷新标签下拉与当前页（这里只计数，行为在标签用例里测）
  loadAccTags: () => { state.tagsLoads++; },
  refreshAccountPool: () => { state.poolRefreshes++; },
  usageData: null, accAll: [], accPageMeta: null,
  Date, Number, String, Object, Array, Math, JSON, Promise,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.__meta = {' +
  ' parseTags, normalizeMeta, loadMeta, maybeMigrateRemarks, remarkOf, remarkName, accTagsHtml,' +
  ' remarkFormRows, renderRemarkBody, saveRemarkForm, openRemarks, remarksAll, storeRemarks, metaFor,' +
  ' state: () => ({ all: metaAll, loaded: metaLoaded, tried: metaMigrationTried }),' +
  ' reset: () => { metaAll = {}; metaLoaded = false; metaMigrationTried = false; },' +
  ' setMeta: m => { metaAll = m; }, setLoaded: v => { metaLoaded = v; },' +
  '};', ctx);
const M = ctx.__meta;
const row = (uid, note, tags, legacy) => ({
  getAttribute: k => (k === 'data-uid' ? uid : (k === 'data-legacy' ? (legacy ? '1' : null) : null)),
  querySelector: sel => (sel === '.rm-note' ? { value: note } : (sel === '.rm-tags' ? { value: tags } : null)),
});
(async () => {
  const out = {};
  M.reset(); M.setLoaded(true);
  M.setMeta({ u1: { note: 'old', tags: ['x'], updated_at: 1 }, u2: { note: '', tags: [], updated_at: 2 },
              u4: { note: 'old4', tags: [], updated_at: 3 } });
  pool = [{ uid: 'u1', nickname: 'A' }, { uid: 'u2', nickname: 'B' }, { uid: 'u4', nickname: 'D' }];
  M.storeRemarks({ legacy12: '旧本机' });
  const rows = [
    row('u1', ' new1 ', 'a, b', false),
    row('u2', '', '', false),
    row('u4', '', '', false),
    row('u3', '新账号备注', 'vip', false),
    row('legacy12', '本机改', '', true),
  ];
  sinks.remarkBody = { innerHTML: '', querySelectorAll: () => rows };
  apiImpl = async () => ({ ok: true });
  apiCalls.length = 0; toasts.length = 0; state.renders = 0;
  state.tagsLoads = 0; state.poolRefreshes = 0;
  await M.saveRemarkForm();
  out.save = {
    calls: apiCalls.map(c => [c[0], c[1], c[2] ? JSON.parse(c[2]) : null]),
    u1: { note: M.metaFor('u1').note, tags: M.metaFor('u1').tags },
    u3: { note: M.metaFor('u3').note, tags: M.metaFor('u3').tags },
    u4: M.metaFor('u4'),
    local: M.remarksAll(), toast: toasts[0] && toasts[0][0], cls: toasts[0] && toasts[0][1],
    btn: sinks.btnRemarkSave.disabled, renders: state.renders,
    tagsLoads: state.tagsLoads, poolRefreshes: state.poolRefreshes,   // v2.3.1 保存后刷新标签下拉+列表
  };
  M.reset(); M.setLoaded(true); M.setMeta({ u1: { note: 'old', tags: [], updated_at: 1 } });
  pool = [{ uid: 'u1', nickname: 'A' }];
  M.storeRemarks({});
  sinks.remarkBody = { innerHTML: '', querySelectorAll: () => [row('u1', 'changed', '', false)] };
  apiImpl = async () => { throw new Error('HTTP 500'); };
  apiCalls.length = 0; toasts.length = 0;
  await M.saveRemarkForm();
  out.saveFail = { calls: apiCalls.length, u1: M.metaFor('u1').note, local: M.remarksAll(),
                   toast: toasts[0] && toasts[0][0], cls: toasts[0] && toasts[0][1] };
  M.reset(); M.setLoaded(true); M.setMeta({ u1: { note: 'same', tags: ['t'], updated_at: 1 } });
  sinks.remarkBody = { innerHTML: '', querySelectorAll: () => [row('u1', 'same', 't', false)] };
  apiCalls.length = 0; toasts.length = 0;
  await M.saveRemarkForm();
  out.saveNoop = { calls: apiCalls.length, toast: toasts[0] && toasts[0][0] };
  // 弹窗渲染：服务端备注/标签 + 本机孤儿旧键（legacy）
  M.reset(); M.setLoaded(true);
  M.setMeta({ u1: { note: '服务端备注', tags: ['t1', 't2'], updated_at: 1 } });
  pool = [{ uid: 'u1', nickname: '甲' }];
  M.storeRemarks({ orphan01: '本机孤立' });
  sinks.remarkBody = { innerHTML: '' };
  M.renderRemarkBody();
  const html = sinks.remarkBody.innerHTML;
  out.renderRows = {
    rows: (html.match(/class="rm-row"/g) || []).length,
    hasNote: html.indexOf('服务端备注') >= 0,
    hasTags: html.indexOf('value="t1, t2"') >= 0,
    hasLegacy: html.indexOf('data-legacy="1"') >= 0,
    hasLegacyNote: html.indexOf('本机孤立') >= 0,
  };
  out.formRows = M.remarkFormRows().map(r => ({ uid: r.uid, legacy: r.legacy }));
  // openRemarks：未加载 meta 时先拉服务端再渲染
  M.reset(); pool = [{ uid: 'u1', nickname: '甲' }];
  apiImpl = async () => ({ ok: true, meta: { u1: { note: '打开时拉取', tags: [], updated_at: 1 } } });
  apiCalls.length = 0;
  sinks.remarkVeil = mk('remarkVeil');
  sinks.remarkBody = { innerHTML: '' };
  await M.openRemarks();
  out.open = { path: apiCalls[0] && apiCalls[0][0],
               hasNote: sinks.remarkBody.innerHTML.indexOf('打开时拉取') >= 0,
               veilOn: sinks.remarkVeil.classList.on };
  process.stdout.write(JSON.stringify(out));
  process.exit(0);
})().catch(e => { console.error(e); process.exit(1); });`
	want := `{"save":{"calls":[["accounts/u1/meta","PUT",{"note":"new1","tags":["a","b"]}],` +
		`["accounts/u4/meta","DELETE",null],["accounts/u3/meta","PUT",{"note":"新账号备注","tags":["vip"]}]],` +
		`"u1":{"note":"new1","tags":["a","b"]},"u3":{"note":"新账号备注","tags":["vip"]},"u4":null,` +
		`"local":{"u1":"new1","u3":"新账号备注","legacy12":"本机改"},` +
		`"toast":"备注/标签已保存到服务端（3 条）","cls":"ok","btn":false,"renders":1,` +
		`"tagsLoads":1,"poolRefreshes":1},` +
		`"saveFail":{"calls":1,"u1":"old","local":{"u1":"changed"},` +
		`"toast":"已保存 0 条，失败 1 条（本机缓存已保留，可重试）","cls":"err"},` +
		`"saveNoop":{"calls":0,"toast":"没有需要保存的改动"},` +
		`"renderRows":{"rows":2,"hasNote":true,"hasTags":true,"hasLegacy":true,"hasLegacyNote":true},` +
		`"formRows":[{"uid":"u1","legacy":false},{"uid":"orphan01","legacy":true}],` +
		`"open":{"path":"meta","hasNote":true,"veilOn":1}}`
	if got := feNode(t, script); got != want {
		t.Fatalf("备注保存/渲染=%s\nwant %s", got, want)
	}
}

// TestPanelJobMetaWiring v2.3.0 #10/#12/#20 的前端接线（源码级）：
// Job 进度条/取消/批量按钮、备注标签服务端读写与迁移、移动端样式、旧「仅本机」实现移除。
func TestPanelJobMetaWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	j := string(js)
	for _, want := range []string{
		// #10 Job API 契约（路径/limit/取消）
		"const JOBS_LIMIT = 20;",
		"api('jobs?limit=' + JOBS_LIMIT)",
		"api('jobs/' + encodeURIComponent(j.id) + '/cancel'",
		"function renderJobBar(",
		"async function loadJobs(",
		"async function cancelJob(",
		"async function runBatchTask(",
		"已开始（job 进度见顶部）",
		"loadJobs(true);   // 进度条跟随轮询刷新",
		// 后端契约：state 含 interrupted；进度 = done+skip（done 含失败不含跳过）
		"interrupted: '已中断'",
		"const processed = j.done + j.skip;",
		"canceled === false",
		// 批量按钮统一走 runBatchTask（4 个全池按钮）
		"$('btnCheckinAll').onclick = () => runBatchTask('checkin_all', '全部签到'",
		"$('btnTravelAll').onclick = () => runBatchTask('travel_all', '旅行巡检'",
		"$('btnActivityAll').onclick = () => runBatchTask('activity_all', '活跃上报'",
		"$('btnKeepaliveAll').onclick = () => runBatchTask('keepalive_all', '全部保活'",
		// #12 meta 服务端读写 + 迁移 + 标签
		"api('meta')",
		"accounts/' + encodeURIComponent(ch.uid) + '/meta'",
		"function maybeMigrateRemarks(",
		"function parseTags(",
		"function accTagsHtml(",
		"accTagsHtml(s.uid)",
		"LS_REMARKS = 'wb2api.remarks.v1'",
		"已迁移本机备注到服务端",
		"备注/标签已保存到服务端",
		// 账号集合变更时清本地 meta 缓存（服务端 forget 的镜像）
		"delete metaAll[u]",
	} {
		if !strings.Contains(j, want) {
			t.Errorf("app.js 缺少 %q", want)
		}
	}
	// 旧「只写本机」实现必须移除（避免备注看起来保存了其实没上服务端）。
	for _, forbidden := range []string{
		"备注已保存（仅本机浏览器）",
		"function saveRemarkForm() {\n  const map = {};",
	} {
		if strings.Contains(j, forbidden) {
			t.Errorf("app.js 仍残留旧的纯本机备注实现 %q", forbidden)
		}
	}
	if n := strings.Count(j, "runBatchTask("); n < 5 {
		t.Errorf("runBatchTask 引用 %d 处（定义 + 4 个批量按钮至少 5 处）", n)
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	for _, want := range []string{
		`id="jobBar"`, `id="jobKind"`, `id="jobBarFill"`, `id="jobProgText"`,
		`id="jobMeta"`, `id="btnJobCancel"`,
		"账号备注 / 标签", "服务端", "data/account-meta.json",
		"标签用逗号或空格分隔",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("index.html 缺少 %q", want)
		}
	}
	css, err := os.ReadFile("mobile.css")
	if err != nil {
		t.Fatal(err)
	}
	c := string(css)
	for _, want := range []string{
		"v2.3.0", "#jobBar", ".acc-tags", "#remarkVeil .rm-row", "@media (max-width: 760px)",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("mobile.css 缺少 %q", want)
		}
	}
}

// TestAppJSAccTagFilter v2.3.1 标签筛选：/meta/tags 归一化、50 上限、选中项保留、
// 失败静默降级（metaAll 聚合 / 只剩「全部标签」）。
func TestAppJSAccTagFilter(t *testing.T) {
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const ACC_TAG_LIMIT = 50;');
const end = src.indexOf('/* etagCachePut');
if (start < 0 || end < 0 || end < start) throw new Error('tag filter section not found');
const sinks = {};
const sel = { innerHTML: '', value: '' };
sinks.accTagFilter = sel;
const apiCalls = [];
let apiImpl = async () => ({ ok: true, tags: [] });
const ctx = {
  esc: s => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;')
    .replace(/>/g, '&gt;').replace(/"/g, '&quot;'),
  $: id => sinks[id] || null,
  api: async (p, o) => { apiCalls.push([p, o && o.method]); return apiImpl(p, o); },
  accFilters: { tag: '' },
  metaAll: {},
  Array, Map, String, Number,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.__tag = {' +
  ' normalizeAccTags, accTagOptionsFromMeta, renderAccTagOptions, loadAccTags,' +
  ' state: () => ({ opts: accTagOptions }),' +
  '};', ctx);
const T = ctx.__tag;
(async () => {
  const out = {};
  out.normArray = T.normalizeAccTags({ ok: true, tags: [{ tag: 'b', count: 2 }, { tag: 'a', count: 5 },
    { tag: 'b', count: 1 }, { tag: '', count: 9 }, { tag: 'c' }] });
  out.normObj = T.normalizeAccTags({ tags: { x: 2, y: 1 } });
  const many = Array.from({ length: 60 }, (_, i) => ({ tag: 't' + String(i).padStart(2, '0'), count: 1 }));
  const capped = T.normalizeAccTags({ tags: many });
  out.cap = { len: capped.length, first: capped[0].tag, last: capped[capped.length - 1].tag };
  ctx.metaAll = { u1: { tags: ['m1', 'm2'] }, u2: { tags: ['m1'] }, u3: { note: 'x', tags: [] } };
  out.fromMeta = T.accTagOptionsFromMeta();
  // 选中项保留；选中项已不在列表时补一条，避免下拉与筛选条件不一致
  ctx.accFilters.tag = 'vip';
  T.renderAccTagOptions([{ tag: 'a', count: 3 }, { tag: 'vip', count: 1 }]);
  out.render1 = { options: (sel.innerHTML.match(/<option/g) || []).length, value: sel.value,
                  hasVip: sel.innerHTML.indexOf('vip（1）') >= 0,
                  hasAll: sel.innerHTML.indexOf('全部标签') >= 0 };
  T.renderAccTagOptions([{ tag: 'a', count: 3 }]);
  out.render2 = { options: (sel.innerHTML.match(/<option/g) || []).length, value: sel.value,
                  hasVip: sel.innerHTML.indexOf('vip') >= 0 };
  ctx.accFilters.tag = '';
  T.renderAccTagOptions([]);
  out.renderEmpty = { options: (sel.innerHTML.match(/<option/g) || []).length, value: sel.value };
  apiCalls.length = 0;
  apiImpl = async () => ({ ok: true, tags: [{ tag: 'x', count: 2 }] });
  await T.loadAccTags(true);
  out.loadOk = { path: apiCalls[0] && apiCalls[0][0], opts: T.state().opts,
                 html: sel.innerHTML.indexOf('x（2）') >= 0 };
  apiImpl = async () => { throw new Error('HTTP 404'); };
  await T.loadAccTags(true);
  out.loadFailFallback = { opts: T.state().opts, html: sel.innerHTML.indexOf('m1（2）') >= 0 };
  ctx.metaAll = {};
  await T.loadAccTags(true);
  out.loadFailEmpty = { options: (sel.innerHTML.match(/<option/g) || []).length, value: sel.value };
  process.stdout.write(JSON.stringify(out));
})().catch(e => { console.error(e); process.exit(1); });`
	want := `{"normArray":[{"tag":"a","count":5},{"tag":"b","count":3},{"tag":"c","count":0}],` +
		`"normObj":[{"tag":"x","count":2},{"tag":"y","count":1}],` +
		`"cap":{"len":50,"first":"t00","last":"t49"},` +
		`"fromMeta":[{"tag":"m1","count":2},{"tag":"m2","count":1}],` +
		`"render1":{"options":3,"value":"vip","hasVip":true,"hasAll":true},` +
		`"render2":{"options":3,"value":"vip","hasVip":true},` +
		`"renderEmpty":{"options":1,"value":""},` +
		`"loadOk":{"path":"meta/tags","opts":[{"tag":"x","count":2}],"html":true},` +
		`"loadFailFallback":{"opts":[{"tag":"m1","count":2},{"tag":"m2","count":1}],"html":true},` +
		`"loadFailEmpty":{"options":1,"value":""}}`
	if got := feNode(t, script); got != want {
		t.Fatalf("标签筛选=%s\nwant %s", got, want)
	}
}

// TestPanelTagFilterWiring v2.3.1 标签筛选的源码/HTML/CSS 接线。
func TestPanelTagFilterWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	j := string(js)
	for _, want := range []string{
		"function normalizeAccTags(",
		"function accTagOptionsFromMeta(",
		"function renderAccTagOptions(",
		"async function loadAccTags(",
		"api('meta/tags')",
		"p.set('tag', filters.tag)",
		"无账号使用该标签「",
		"$('accTagFilter')",
		"const ACC_TAG_LIMIT = 50;",
		"loadAccTags(true);   // v2.3.1：备注/标签加载后同步标签筛选下拉（静默）",
	} {
		if !strings.Contains(j, want) {
			t.Errorf("app.js 标签筛选接线缺少 %q", want)
		}
	}
	// 标签下拉刷新点：loadMeta / refreshAccountPoolAfterMutation / saveRemarkForm
	if n := strings.Count(j, "loadAccTags(true);"); n < 3 {
		t.Errorf("loadAccTags 静默刷新只有 %d 处（loadMeta/账号集合变更/备注保存后至少 3 处）", n)
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`id="accTagFilter"`, `全部标签`, `按标签筛选`} {
		if !strings.Contains(string(html), want) {
			t.Errorf("index.html 标签筛选控件缺少 %q", want)
		}
	}
	css, err := os.ReadFile("mobile.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), "#accTagFilter") {
		t.Error("mobile.css 缺少 320px 标签筛选下拉限宽规则")
	}
}

// TestPanelFrontendCheckScript v2.3.1 #18：前端工程化检查脚本必须存在且 exit 0。
// 检查项：app.js/mobile.js 解析、DOM id 交叉契约、API 路径白名单、体积预算。
func TestPanelFrontendCheckScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; frontend check script test skipped")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "panel_frontend_check.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("scripts/panel_frontend_check.mjs 不存在：%v", err)
	}
	out, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("前端工程化检查失败（exit != 0）：%v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	if !strings.HasPrefix(got, "panel frontend check: OK") {
		t.Fatalf("前端工程化检查输出异常：%s", got)
	}
	for _, want := range []string{"app.js=", "mobile.js=", "index.html="} {
		if !strings.Contains(got, want) {
			t.Fatalf("前端工程化检查结果缺少指标 %q：%s", want, got)
		}
	}
}

// TestPanelFrontendCheckScriptFailsClosed 检查脚本必须「失败即非 0」：
// 语法错误 / DOM id 契约缺失 / 体积超预算任意一项都必须 exit 1 并打印 FAIL。
func TestPanelFrontendCheckScriptFailsClosed(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; frontend check script test skipped")
	}
	repoScript, err := filepath.Abs(filepath.Join("..", "..", "scripts", "panel_frontend_check.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(repoScript)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	panelDir := filepath.Join(root, "internal", "panel")
	if err := os.MkdirAll(panelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "panel_frontend_check.mjs"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	// app.js：合法语法 + 引用不存在的 id + 超过 350KB 预算；mobile.js：解析错误；
	// index.html：只含一个未被引用的 id（双向契约都要报错）。
	appJs := "function boot(){ $('missingId'); }\nvar pad = '" + strings.Repeat("a", 360*1024) + "';\n"
	if err := os.WriteFile(filepath.Join(panelDir, "app.js"), []byte(appJs), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(panelDir, "mobile.js"), []byte("function broken( {"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(panelDir, "index.html"), []byte("<div id=\"present\"></div>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, filepath.Join(root, "scripts", "panel_frontend_check.mjs")).CombinedOutput()
	if err == nil {
		t.Fatalf("错误输入下检查脚本应 exit 1，实际通过：%s", out)
	}
	got := string(out)
	for _, want := range []string{"FAIL:", "解析失败", "不存在的 id", "体积超预算", "panel frontend check: FAILED"} {
		if !strings.Contains(got, want) {
			t.Errorf("失败输出缺少 %q：%s", want, got)
		}
	}
}
