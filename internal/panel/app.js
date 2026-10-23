'use strict';
/* ── 状态 ─────────────────────────────────────────────────────────── */
const LS_KEY = 'wb2api.key', LS_THEME = 'wb2api.theme';
/* 账号备注与「积分到期提醒」折叠的本机键（同上：模块级声明放顶部，避开 TDZ）。 */
const LS_REMARKS = 'wb2api.remarks.v1', LS_EXP_FOLD = 'wb2api.expfold.v1';
let remarksCache = null;
let theme = localStorage.getItem(LS_THEME) || 'auto';   // auto | light | dark
let view = 'accounts';
let overviewData = null, cfgLoaded = null;
let logPin = true, loginState = null, loginTimer = null;
/* 轮询状态（refTimer / refFailures / pollBaseSec / pageHidden …）集中在文件后段
   「轮询治理」区声明：只有该区的函数与 api()/loadOverview() 在**调用期**访问它们，
   放后段不会踩 TDZ；同时便于 node harness 按注释锚点切片测试。 */
/* 视图级筛选状态（模块级声明放在文件顶部，避免顶层 go() 早于声明执行时踩 TDZ）。 */
let mdFilter = { q: '', realm: '', cap: '', effort: '', promo: '', sort: 'default' };
let mdAll = [], mdProbes = {}, mdProbeOf = () => undefined;
let mdLocks = [], mdRecent = [];
let reqFilter = { q: '', outcome: '' };
let reqEntries = [];
let reqPage = 1, reqPageSize = 100;           // 客户端分页（0 = 全部）
let usDim = 'account', usCreditDim = 'account', usSort = 'total';
let usageData = null;
let reqRangeState = null; // 请求记录的时间范围（用量页的见 trangeState）

const $ = id => document.getElementById(id);

/* ── 主题 ─────────────────────────────────────────────────────────── */
/* 两态翻转（浅/深），首次访问跟随系统偏好；点击总是切换可见外观，符合直觉。 */
function effTheme() {
  return theme === 'auto' ? (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark') : theme;
}
function applyTheme() {
  const eff = effTheme();
  document.documentElement.dataset.theme = eff;
  $('icoTheme').innerHTML = eff === 'light'
    ? '<circle cx="8" cy="8" r="3"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M12.8 3.2l-1.4 1.4M4.6 11.4l-1.4 1.4"/>'
    : '<path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z"/>';
  $('btnTheme').title = eff === 'light' ? '切换到深色' : '切换到浅色';
}
addEventListener('change', applyTheme);
$('btnTheme').onclick = () => {
  theme = effTheme() === 'light' ? 'dark' : 'light';
  localStorage.setItem(LS_THEME, theme);
  applyTheme();
};
applyTheme();

/* ── 请求 ─────────────────────────────────────────────────────────── */
async function api(path, opts = {}) {
  const h = Object.assign({}, opts.headers || {});
  const k = localStorage.getItem(LS_KEY);
  if (k) h['Authorization'] = 'Bearer ' + k;
  if (opts.body) h['Content-Type'] = 'application/json';
  const r = await fetch('/panel/api/' + path, Object.assign({}, opts, { headers: h }));
  // 401 = 面板密钥（panel.api_key，未单配时回落网关 api_key）无效。统一交给
  // onAuthRequired：停掉自动轮询（否则后台标签页每 5s 刷一次 401、反复抢焦点弹
  // 密钥框），并保证一次失败周期里密钥框只自动弹一次。
  if (r.status === 401) { onAuthRequired(); throw new Error('密钥无效或未填写'); }
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
  keyPrompted = false;   // 鉴权恢复正常：之后再失效（如换了 panel.api_key）可再自动提示一次
  return d;
}
function toast(msg, cls) {
  const el = document.createElement('div');
  el.className = 'tst ' + (cls || '');
  el.textContent = msg;
  $('toasts').appendChild(el);
  setTimeout(() => el.remove(), 3600);
}
// esc 文本/属性双安全转义。不能只用 div.innerHTML（它转义 <>& 但不转义引号），
// 否则字符串拼进 HTML 属性（如 title="uid: ..."）时引号可闭合属性并注入事件处理器。
// 显式替换 5 个字符：& < > " '（& 必须最先，避免二次转义）。
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
function ago(iso) {
  if (!iso || iso.startsWith('0001-')) return '—';
  const s = (Date.now() - new Date(iso)) / 1000;
  if (s < 0) return '刚刚';
  if (s < 60) return Math.floor(s) + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}
function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  return h ? h + '时' + String(m).padStart(2, '0') + '分' : m ? m + '分' + String(s).padStart(2, '0') + '秒' : s + '秒';
}
function parseAPITime(value) {
  const text = String(value || '');
  if (!text || text.startsWith('0001-')) return 0;
  const ms = Date.parse(text);
  return Number.isFinite(ms) ? ms : 0;
}
function fmtLocalDateTime(ms) {
  const d = new Date(ms);
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
    p(d.getHours()) + ':' + p(d.getMinutes());
}

/* ── 时间范围控件（用量 / 请求记录共用）────────────────────────────────
   预设项：今天 / 近 24 小时 / 近 3 天 / 近 7 天 / 近 30 天 / 全部历史 / 自定义。

   为什么区间一律由前端算好再发：
     - 「今天」必须是**浏览器本地时区**的 00:00 起。服务端时区未必与浏览器一致
       （容器常挂 TZ=Asia/Shanghai，而浏览器可能在任何时区），让服务端算"今天"
       会在跨时区时切错日子。
     - 「自定义」本来就是用户挑的具体时刻，没有任何服务端推导空间。

   滚动预设（近 N 小时/天）则保留 hours 参数：服务端按整点对齐的滚动窗口与旧
   行为逐位一致，前端自己减 N 小时会多算/少算一个边界桶。 */
const TRANGE_PRESETS = [
  ['today', '今天'],
  ['24', '近 24 小时'],
  ['72', '近 3 天'],
  ['168', '近 7 天'],
  ['720', '近 30 天'],
  ['0', '全部历史'],
  ['custom', '自定义…'],
];
const TRANGE_DEFAULT = 'today';
const trangeStates = new Map(); // hostId → { preset, from: Date|null, to: Date|null }

// dtLocalValue / dtLocalParse 与 <input type=datetime-local> 的取值格式互转
// （YYYY-MM-DDTHH:mm，本地时区；ES 里"带时间的日期串"按本地解析，正是我们要的）。
function dtLocalValue(d) {
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' +
    p(d.getHours()) + ':' + p(d.getMinutes());
}
function dtLocalParse(s) {
  if (!s) return null;
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d;
}

// trangeMidnight 今天 00:00（本地时区）。
function trangeMidnight() {
  const d = new Date();
  d.setHours(0, 0, 0, 0);
  return d;
}

function trangeState(id) {
  if (!trangeStates.has(id)) {
    // 「自定义」的初始值给一段有意义的默认：今天 00:00 → 现在。
    trangeStates.set(id, { preset: TRANGE_DEFAULT, from: trangeMidnight(), to: new Date() });
  }
  return trangeStates.get(id);
}

// trangeRender 画出控件骨架（幂等：重复调用会保留当前状态）。
function trangeRender(id) {
  const host = $(id);
  if (!host) return;
  const st = trangeState(id);
  const custom = st.preset === 'custom';
  host.innerHTML =
    '<select class="tr-preset" aria-label="时间范围">' +
    TRANGE_PRESETS.map(([v, label]) =>
      '<option value="' + v + '"' + (v === st.preset ? ' selected' : '') + '>' + esc(label) + '</option>').join('') +
    '</select>' +
    '<span class="tr-custom"' + (custom ? '' : ' hidden') + '>' +
    '<input type="datetime-local" class="tr-from" value="' + esc(st.from ? dtLocalValue(st.from) : '') + '" aria-label="起始时间">' +
    '<span class="tr-sep">→</span>' +
    '<input type="datetime-local" class="tr-to" value="' + esc(st.to ? dtLocalValue(st.to) : '') + '" aria-label="结束时间">' +
    '</span>';
  const preset = host.querySelector('.tr-preset');
  if (preset) preset.onchange = () => {
    st.preset = preset.value;
    // 从别的预设切到自定义时，把区间重置为"今天 00:00 → 现在"，
    // 免得用户上次留下的半年区间被无声沿用。
    if (st.preset === 'custom' && (!st.from || !st.to)) { st.from = trangeMidnight(); st.to = new Date(); }
    trangeRender(id);
    trangeEmit(id);
  };
  const fromEl = host.querySelector('.tr-from');
  const toEl = host.querySelector('.tr-to');
  const readCustom = () => {
    st.from = dtLocalParse(fromEl.value);
    st.to = dtLocalParse(toEl.value);
    // 起止颠倒就地标红（不静默纠正：用户可能正输到一半）。
    const bad = st.from && st.to && st.from > st.to;
    fromEl.classList.toggle('tr-bad', !!bad);
    toEl.classList.toggle('tr-bad', !!bad);
    if (bad) return;
    trangeEmit(id);
  };
  if (fromEl) fromEl.onchange = readCustom;
  if (toEl) toEl.onchange = readCustom;
}

const trangeHandlers = new Map();
// trangeBind 渲染控件并登记变化回调。**不**在绑定时触发回调：各视图的首次加载
// 由 go() 统一驱动，这里再触发一次会让打开页面时打两遍接口。
function trangeBind(id, onChange, preset) {
  trangeHandlers.set(id, onChange);
  if (preset) trangeState(id).preset = preset;
  trangeRender(id);
}
function trangeEmit(id) {
  const fn = trangeHandlers.get(id);
  if (fn) fn();
}

// trangeQuery 把当前选择翻译成查询参数。
//   rolling=true  → 滚动预设发 hours（服务端整点对齐），今天/自定义发 from/to
//   rolling=false → 一律发 from/to（归档是线性日志，前端算区间更直观）
// 「全部历史」两者都不发。
function trangeQuery(id, rolling) {
  const st = trangeState(id);
  const q = new URLSearchParams();
  const sec = d => Math.floor(d.getTime() / 1000);
  if (st.preset === 'custom') {
    if (st.from) q.set('from', sec(st.from));
    if (st.to) q.set('to', sec(st.to));
    return q;
  }
  if (st.preset === 'today') {
    q.set('from', sec(trangeMidnight()));
    return q;
  }
  // 全部历史：滚动端点（用量）必须**显式**传 hours=0。
  //
  // 后端对「什么都不给」的缺省是 72 小时（见 panel.go 的说明：
  // 「都不给：等同于 hours=72（保持旧调用方行为）」），所以这里返回空 query 会被
  // 当成「近 3 天」—— 正是 issue #121 报的现象：选了「全部历史」，数字却和
  // 「近 3 天」一模一样。
  //
  // 非滚动端点（请求记录）没有缺省窗口：不传 from/to 即"不限起点"，保持空 query。
  if (st.preset === '0') {
    if (rolling) q.set('hours', '0');
    return q;
  }
  if (rolling) { q.set('hours', st.preset); return q; }
  q.set('from', sec(new Date(Date.now() - Number(st.preset) * 3600 * 1000)));
  return q;
}

// trangeLabel 人读口径，用于「用量总览」右上角这类需要回显区间的位置。
function trangeLabel(id) {
  const st = trangeState(id);
  const found = TRANGE_PRESETS.find(p => p[0] === st.preset);
  if (st.preset !== 'custom') return found ? found[1] : '';
  if (!st.from && !st.to) return '自定义';
  const f = d => d ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0') + ' ' +
    String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0') : '…';
  return f(st.from) + ' → ' + f(st.to);
}
function rateLimitMeta(row, now) {
  const model = String(row && row.model || '未知模型');
  const kind = String(row && row.kind || 'rate_limit');
  const resetAt = parseAPITime(row && row.reset_at);
  const until = parseAPITime(row && row.until);
  const deadline = resetAt || until;
  const remaining = deadline > now ? Math.round((deadline - now) / 1000) : 0;
  if (kind === 'model_unavailable') {
    return {
      model,
      kind,
      detail: remaining ? '预计 ' + dur(remaining) + ' 后重试' : '等待重新探测',
      title: model + '\n模型当前不可用' + (deadline ? '\n最早重试：' + fmtLocalDateTime(deadline) : ''),
    };
  }
  const title = [model, resetAt ? '上游重置：' + fmtLocalDateTime(resetAt) : '上游重置：时间未知'];
  if (until && resetAt && until < resetAt) {
    // 双时钟（issue #171 问题二）：黄框出现/消失由 until 决定（网关为防超长
    // 限流拖死全池，把重试封顶在 now+softRateMax），主文字却按 resetAt 显示，
    // 导致「写的解封时间」与「实际掉框时刻」不一致。此时主文字同时明示两个
    // 时刻与口径，倒计时跟随 until（与实际掉框一致，也与「模型与档位」页
    // 锁池表的 until 口径对齐）。
    const retryIn = until > now ? Math.round((until - now) / 1000) : 0;
    detail = '网关 ' + fmtLocalDateTime(until) + ' 起重试' + (retryIn ? '（剩余 ' + dur(retryIn) + '）' : '') +
      ' · 上游称 ' + fmtLocalDateTime(resetAt) + ' 恢复';
    title.push('网关最早重试：' + fmtLocalDateTime(until));
  } else {
    detail = resetAt
      ? '预计 ' + fmtLocalDateTime(resetAt) + ' 解封' + (remaining ? '（剩余 ' + dur(remaining) + '）' : '')
      : (until ? '预计 ' + fmtLocalDateTime(until) + ' 恢复（剩余 ' + dur(remaining) + '）' : '预计解封时间未知');
  }
  return { model, kind, detail, title: title.join('\n') };
}
function rateLimitRowsHtml(rows, now) {
  const list = Array.isArray(rows) ? rows.filter(row => row && row.model) : [];
  if (!list.length) return '';
  return '<div class="rate-limits">' + list.map(row => {
    const m = rateLimitMeta(row, now);
    return '<div class="rate-limit ' + (m.kind === 'model_unavailable' ? 'model-unavailable' : '') +
      '" title="' + esc(m.title) + '"><b>' + esc(m.model) + '</b><span>' + esc(m.detail) + '</span></div>';
  }).join('') + '</div>';
}

function formatTokenCount(tokens) {
  if (tokens == null || tokens === '') return '—';
  const n = Number(tokens);
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1000) return String(Math.round(n));
  const units = [['k', 1e3], ['m', 1e6], ['b', 1e9]];
  let unit = units[0];
  for (const candidate of units) {
    if (n >= candidate[1]) unit = candidate;
  }
  let value = n / unit[1];
  let rounded = Number(value.toFixed(1));
  // 999999 → 1m，而不是 1000k；四舍五入后自动升级单位。
  const next = units[units.indexOf(unit) + 1];
  if (next && rounded >= 1000) {
    unit = next;
    value = n / unit[1];
    rounded = Number(value.toFixed(1));
  }
  return rounded + unit[0];
}

function formatLatency(ms) {
  if (ms == null || ms === '') return '—';
  const n = Number(ms);
  if (!Number.isFinite(n) || n <= 0) return '—';
  return n < 1000 ? Math.round(n) + 'ms' : (n / 1000).toFixed(1).replace(/\.0$/, '') + 's';
}
function formatRate(rate) {
  if (rate == null || rate === '') return '—';
  const n = Number(rate);
  if (!Number.isFinite(n) || n < 0) return '—';
  return n.toFixed(1) + 'tok/s';
}

/* ── 密钥门 ───────────────────────────────────────────────────────── */
/* 面板鉴权用的是「面板密钥」panel.api_key；未单配时后端回落到网关 api_key。
   localStorage 键（wb2api.key）沿用不改：老浏览器里存的就是当时生效的那把密钥
   （旧版本是网关 key，分离后可能是面板 key），继续用它发 Authorization 即可。

   openKey 只在 401 时由 onAuthRequired 调用；本机已存过密钥说明「原本能进、后来
   401」——多半是服务端换了 panel.api_key（升级后分离密钥的典型场景），提示文案
   要明确指向它，而不是笼统的「密钥不正确」。 */
function openKey() {
  const had = !!localStorage.getItem(LS_KEY);
  const err = $('keyErr');
  if (err) {
    err.textContent = had
      ? '面板密钥可能已变更（panel.api_key），请重新输入。'
      : '密钥不正确，请重试。';
    err.hidden = !had;
  }
  $('keyVeil').classList.add('on');
  setTimeout(() => $('keyInput').focus(), 60);
}
$('btnKey').onclick = async () => {
  const v = $('keyInput').value.trim();
  if (!v) return;
  localStorage.setItem(LS_KEY, v);
  try {
    await api('overview?lite=1');   // 只验鉴权：用 lite（1-2 KB）即可，不拉全量账号
    $('keyErr').hidden = true;
    $('keyVeil').classList.remove('on');
    start();   // 手动保存密钥成功 → 重置 401 停表状态并重启轮询
  } catch (e) { $('keyErr').hidden = false; }
};
$('keyInput').addEventListener('keydown', e => { if (e.key === 'Enter') $('btnKey').click(); });

/* ── 路由 ─────────────────────────────────────────────────────────── */
/* 积分包明细共享缓存：「积分构成」视图与首页「积分到期提醒」卡片共用同一份
   /panel/api/packages 数据（逐账号实时查上游，能省一次是一次）。声明在路由区
   是因为 go() 的初始调用就会触发首页卡片的渲染，必须先于它就位。 */
let lastPackages = null;
let lastPackagesAt = 0;
let expFetching = false;                       // 到期卡片在途标记（防重复打上游）
const EXP_FRESH_MS = 2 * 60 * 1000;            // 缓存新鲜窗口：2 分钟内复用

// 整池「积分构成」入口已下线（整池逐号查上游的批量特征有风控风险），
// 视图代码保留在下方，仅不再可从导航/直链进入。
const TITLES = { accounts: '账号池', usage: '用量', taskscenter: '任务中心', models: '模型与档位', config: '配置', logs: '运行日志' };
function go(v) {
  view = v;
  document.querySelectorAll('.view').forEach(s => s.hidden = s.id !== 'view-' + v);
  document.querySelectorAll('.nav a').forEach(a => a.classList.toggle('on', a.dataset.view === v));
  $('ttl').textContent = TITLES[v];
  if (v === 'models' && !$('mdBody').children.length) loadModels();
  if (v === 'config') loadConfig();
  if (v === 'logs') loadLogs();
  if (v === 'usage') loadUsage();
  if (v === 'packages') loadPackages();
  // 到期提醒卡片：默认可见，进页面即用 overview 快照渲染（零上游请求，随首页统计
  // 每轮刷新）。不再「打开账号池就打上游」——逐账号实时查在账号多时会卡死面板。
  // 完整逐包明细由卡片上的「检查」按钮显式触发（loadPackages/loadExpiry）。
  if (v === 'taskscenter') reattachQueueView();
}
document.querySelectorAll('.nav a').forEach(a => a.onclick = e => { e.preventDefault(); go(a.dataset.view); history.replaceState(null, '', '#' + a.dataset.view); });
/* 首次进入延到本轮脚本求值之后再 go()。
   原因：go() 会同步触发视图的数据加载（loadUsage/loadLogs/loadPackages…），而这些
   函数读到的模块级 let/const（usageRateWarmAt、PK_* 等）在文件后半段才初始化——
   直接深链 #usage / #packages 打开页面时会踩 TDZ（"Cannot access 'x' before
   initialization"），表现为该页永远显示"读取失败"，而点导航进去一切正常。
   延迟 0ms 让整份脚本先求值完，是修这一类问题最省事也最不容易再犯的办法。 */
setTimeout(() => {
  const hash = (location.hash || '#accounts').slice(1);
  go(hash in TITLES ? hash : 'accounts');
}, 0);

/* ── 账号池 ───────────────────────────────────────────────────────── */
/* ── 账号池二级 tab（账号 / 积分到期）───────────────────────────────────
   账号 tab：账号表 + 模型锁池；积分到期 tab：到期卡片（/overview 快照，零上游请求）。 */
let accTab = 'list';                          // list | expiry

function switchAccTab(tab) {
  accTab = (tab === 'expiry' || tab === 'models') ? tab : 'list';
  document.querySelectorAll('#accTabs .subtab').forEach(b => b.classList.toggle('on', b.dataset.tab === accTab));
  expBoxShow();
  // v2.1 #1：切到「积分到期」时按需拉完整 overview（60s 新鲜窗口内复用缓存）。
  // 已有快照先渲染，避免等请求的空白期；拉回后再渲染一次。
  if (accTab === 'expiry') {
    if (overviewData) renderExpirySnapshot(overviewData);
    ensureFullOverview(true).then(ok => {
      if (ok && accTab === 'expiry' && overviewData) renderExpirySnapshot(overviewData);
    });
  }
}

// 三个面板容器（账号池 / 积分到期 / 模型锁池）的可见性统一由当前 tab 决定。
// hidden 不写死在 HTML（issue #137 的死锁根因）：只要这份脚本能执行，可见性就由这里
// 接管；脚本坏掉时三个容器都裸露（可发现）而不是整块消失。
// 注意必须同时管三个：只初始化到期卡片会让模型锁池在首屏裸露在账号池下面。
function expBoxShow() {
  if ($('expBox')) $('expBox').hidden = accTab !== 'expiry';
  if ($('accBox')) $('accBox').hidden = accTab !== 'list';
  if ($('mlBox')) $('mlBox').hidden = accTab !== 'models';
}
// 求值即接管初始可见性。
expBoxShow();

/* ── 账号池筛选 + 服务端分页（v2.1 #1）────────────────────────────────
   accFilters/accPage/accPageSize 是「请求参数」而非本地过滤条件：任何变化都重新
   请求 /panel/api/accounts（服务端 filter+分页），前端只持有当前页 rows。
   兼容：renderAccounts(array) 仍走本地筛选+分页（node harness/旧调用）。 */
let expAll = [];                              // 到期快照（按最近到期升序，含无到期账号）
let expPage = 1;
let expPageSize = 100;                        // 0 = 全部
let accAll = [];
let accFilters = { q: '', state: '', ver: '' };
let accPageSize = 100;                        // 0 = 全部
let accPage = 1;

/* ── 分页纯函数（账号池 / 日志 / 请求记录 / 积分历史 / 模型锁池 / 任务队列共用）──
   pageSlice 只做「钳页码 + 切片」，不读写任何模块状态；updatePagerUI 把结果写回
   一组分页控件。抽成纯函数是为了 node harness 能直接切片测试边界（空列表、页码
   越界、pageSize=0 表示全部）——这些分支在浏览器里很难手工复现。 */
function pageSlice(list, pageSize, page) {
  const rows = list || [];
  const size = Number(pageSize) > 0 ? Math.floor(Number(pageSize)) : 0;   // 0 = 全部
  const total = rows.length;
  const pages = size > 0 ? Math.max(1, Math.ceil(total / size)) : 1;
  let p = Math.floor(Number(page)) || 1;
  if (p < 1) p = 1;
  if (p > pages) p = pages;
  const start = size > 0 ? (p - 1) * size : 0;
  return { rows: size > 0 ? rows.slice(start, start + size) : rows.slice(), page: p, pages: pages, total: total, size: size };
}

/* updatePagerUI 把 pageSlice 结果写回一组控件：ids = {count, info, prev, next} 是元素 id。
   label 只在传入时更新计数文案（各列表口径不同，如「命中 3 / 100 条」）。 */
function updatePagerUI(ids, pr, label) {
  ids = ids || {};
  const count = ids.count ? $(ids.count) : null;
  if (count && label != null) count.textContent = label;
  const info = ids.info ? $(ids.info) : null;
  if (info) info.textContent = pr.page + ' / ' + pr.pages;
  const prev = ids.prev ? $(ids.prev) : null;
  if (prev) prev.disabled = pr.page <= 1;
  const next = ids.next ? $(ids.next) : null;
  if (next) next.disabled = pr.page >= pr.pages;
}

/* hitCountLabel 列表命中计数文案：未筛选时只报总数，筛选后带出命中数。
   total 为 0 时返回空串（各列表空态有自己的文案，计数不要再叠一句「0 条」）。 */
function hitCountLabel(shown, total, unit) {
  unit = unit || '条';
  if (!total) return '';
  return shown === total ? total + ' ' + unit : '命中 ' + shown + ' / ' + total + ' ' + unit;
}

/* ── 账号池服务端分页/筛选参数（v2.1 #1）──────────────────────────────
   账号池的筛选与分页全部下推给 /panel/api/accounts，前端不再对全量数组做
   filter/slice。参数口径与后端契约一一对应（字段名不可改）：
     state: ok→healthy、cool→cooling、paused、disabled（空 = 全部）
     ver:   enterprise→enterprise=1；global→realm=global&enterprise=0；
            cn→realm=cn&enterprise=0（保持旧 UI 三分法互斥语义）
     page_size: 0 表示全部（服务端上限 2000）。 */
function accStateParam(state) {
  const map = { ok: 'healthy', cool: 'cooling', paused: 'paused', disabled: 'disabled' };
  return map[state] || '';
}

/* accQueryString 由筛选状态 + 页码/页长拼服务端查询串；同一查询串同时是 ETag
   缓存的键。参数写入顺序固定，避免等价查询产生两个缓存键。
   版本筛选保持旧 UI 的三分法「企业版 / 国际版 / 国内版」互斥语义：服务端
   realm 与 enterprise 是两个正交维度，所以国内/国际都要再带 enterprise=0
   把企业版排除掉（否则国际版筛选会把海外企业号也带出来）。 */
function accQueryString(filters, page, pageSize) {
  filters = filters || { q: '', state: '', ver: '' };
  const p = new URLSearchParams();
  p.set('page', String(Math.max(1, Math.floor(Number(page) || 1))));
  p.set('page_size', String(Math.max(0, Math.floor(Number(pageSize) || 0))));
  const st = accStateParam(filters.state);
  if (st) p.set('state', st);
  if (filters.q) p.set('q', filters.q);
  if (filters.ver === 'enterprise') p.set('enterprise', '1');
  else if (filters.ver === 'global') { p.set('realm', 'global'); p.set('enterprise', '0'); }
  else if (filters.ver === 'cn') { p.set('realm', 'cn'); p.set('enterprise', '0'); }
  return p.toString();
}

/* etagCachePut 把 {etag,data} 放进按查询串索引的小 LRU（只保留最近 32 个查询，
   避免用户狂敲搜索时缓存无限膨胀）。cache.__keys 记录插入顺序。 */
const ETAG_CACHE_MAX = 32;
function etagCachePut(cache, key, entry) {
  if (!cache || !key) return;
  let keys = cache.__keys;
  if (!Array.isArray(keys)) keys = cache.__keys = [];
  if (keys.indexOf(key) < 0) {
    keys.push(key);
    while (keys.length > ETAG_CACHE_MAX) {
      const old = keys.shift();
      if (old !== key) delete cache[old];
    }
  }
  cache[key] = entry;
}

/* apiCached 带 If-None-Match 的 GET（v2.1 #1）：200 时缓存 {etag,data}；304 时
   直接返回缓存 data 并标记 cached=true（body 为空，不能 parse）。调用方要按
   「200 时校准的 skew + 当前 Date.now()」本地重算倒计时（light 投影没有
   cool_remaining_sec；304 不刷新 now，直接用 now 会冻结倒计时）。
   cache 由调用方持有（账号页用 ACCOUNTS_CACHE），缓存键 = 完整查询串。 */
async function apiCached(path, cache) {
  const h = {};
  const k = localStorage.getItem(LS_KEY);
  if (k) h['Authorization'] = 'Bearer ' + k;
  const prev = cache && cache[path];
  if (prev && prev.etag) h['If-None-Match'] = prev.etag;
  // no-store：禁止浏览器 HTTP 缓存介入，确保条件请求真的到后端、304 能落到这里。
  const r = await fetch('/panel/api/' + path, { headers: h, cache: 'no-store' });
  if (r.status === 401) { onAuthRequired(); throw new Error('密钥无效或未填写'); }
  if (r.status === 304) {
    keyPrompted = false;
    if (!prev) throw new Error('304 但本地缓存缺失');
    return { data: prev.data, etag: prev.etag, cached: true };
  }
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
  keyPrompted = false;
  const etag = (r.headers && typeof r.headers.get === 'function' && r.headers.get('ETag')) || '';
  if (etag) etagCachePut(cache, path, { etag: etag, data: d, at: Date.now() });
  return { data: d, etag: etag, cached: false };
}

/* ── 账号池分页状态（v2.1 #1）──────────────────────────────────────────
   accPageMeta 保存服务端分页响应（total/filtered/page/pages/now），用于计数、
   翻页按钮禁用态与本地倒计时基准；ACCOUNTS_CACHE 按查询串缓存 ETag 响应。
   accReqSeq 用于丢弃过期响应：快速改筛选/翻页时先发的请求可能后到，不能让它
   把新条件的结果覆盖掉（proposal 里「按 epoch 丢弃过期页」的等价做法）。 */
let accPageMeta = null;
let accReqSeq = 0;
/* accClockSkew = 本地 Date.now() - 服务端 now*1000（最近一次 200 响应校准）。
   304 命中时响应没有新 now，但渲染用 Date.now()-skew 得到继续前进的本地时间，
   冷却倒计时因此不会冻结（详见 accNowMs）。 */
let accClockSkew = 0;
const ACCOUNTS_CACHE = {};

/* dotaskBusy 正在执行「做任务」的 uid 集合。
   5s 轮询会把 #accBody 整块 innerHTML 重建，只改被点击的那个按钮节点会丢状态，
   按钮看起来又能点（服务端 per-account 锁会回 409，但交互上像卡住/可重复触发）。
   渲染时查这个集合：进行中的账号直接渲染成 disabled + 「做任务中…」。 */
const dotaskBusy = new Set();

/* accDoTaskBtn 账号行「做任务」按钮：对单账号执行全部可自动任务（后端
   POST /accounts/{uid}/tasks/run，同步接口，约 1-2 分钟）。企业版没有个人成长
   体系（后端 200 + ok:false 拒绝），与「签到」「任务」按钮同口径不渲染。
   抽成纯函数便于 node harness 直接钉住「企业版不出按钮 / 进行中不可重复点」的渲染条件。 */
function accDoTaskBtn(s) {
  if (!s || s.enterprise) return '';
  const busy = dotaskBusy.has(s.uid);
  return '<button class="xs ghost" data-a="dotask" data-u="' + esc(s.uid) + '"' +
    (busy ? ' disabled' : '') +
    ' title="对该账号一键执行全部可自动任务（含真实对话，约 1-2 分钟）">' +
    (busy ? '做任务中…' : '做任务') + '</button>';
}

/* secUntil 绝对时间 → 剩余秒数（已过期/非法值 = 0）。nowMs 由调用方给：账号页
   响应的 now（服务端 unix 秒）可校正浏览器时钟偏差。 */
function secUntil(v, nowMs) {
  if (!v) return 0;
  const t = new Date(v).getTime();
  if (isNaN(t)) return 0;
  return Math.max(0, (t - nowMs) / 1000);
}

/* accCoolRemaining 账号剩余冷却秒数（v2.1 #1）：light 投影不再返回
   cool_remaining_sec（秒级易变字段会破坏 ETag 稳定性），改由绝对时间本地算——
   取 until / breaker_until / degrade_until 中最晚的未到期时刻，再与旧字段
   cool_remaining_sec 取大（full 投影 / 旧后端仍会带它，保持兼容）。 */
function accCoolRemaining(s, nowMs) {
  if (!s) return 0;
  const now = nowMs == null ? Date.now() : Number(nowMs);
  return Math.max(
    Number(s.cool_remaining_sec) || 0,
    secUntil(s.until, now),
    secUntil(s.breaker_until, now),
    secUntil(s.degrade_until, now));
}

/* accCoolKind 冷却原因标签：熔断 / 连败降权优先于普通冷却；cool_kind 区分
   硬积分冷却（hard_credit）与普通限流冷却。 */
function accCoolKind(s, nowMs) {
  if (!s) return '限流冷却';
  const now = nowMs == null ? Date.now() : Number(nowMs);
  const gen = Math.max(Number(s.cool_remaining_sec) || 0, secUntil(s.until, now));
  const bl = secUntil(s.breaker_until, now);
  const dg = secUntil(s.degrade_until, now);
  if (bl > Math.max(gen, dg)) return '熔断';
  if (dg > gen) return '连败降权';
  return s.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却';
}

/* accNowMs 渲染倒计时用的「当前时间」：本地时钟 - 与服务端的偏差（skew）。
   skew 由最近一次 200 响应的 now 校准；304 命中时 meta 不更新，但这里每次都重新
   取 Date.now()，所以倒计时继续走——ETag 稳定（一直 304）不会把冷却倒计时冻结。
   meta 无 skew/now 时直接用本地时钟（兼容旧调用与切片 harness）。 */
function accNowMs(meta) {
  if (meta && Number.isFinite(Number(meta.skew))) return Date.now() - Number(meta.skew);
  if (meta && Number(meta.now) > 0) return Number(meta.now) * 1000;
  return Date.now();
}

// 账号是否处于冷却（限流 / 熔断 / 连败降权），与行内状态判定一致。
function accIsCool(s, nowMs) { return accCoolRemaining(s, nowMs) > 0; }

function accMatch(s, nowMs) {
  const f = accFilters;
  if (f.q) {
    const hay = ((s.nickname || '') + ' ' + (s.uid || '')).toLowerCase();
    if (hay.indexOf(f.q.toLowerCase()) < 0) return false;
  }
  // 版本筛选（独立下拉）：企业版优先分类，其余按 realm 分国际/国内，三者互斥。
  if (f.ver === 'enterprise' && !s.enterprise) return false;
  if (f.ver === 'global' && (s.enterprise || s.realm !== 'global')) return false;
  if (f.ver === 'cn' && (s.enterprise || s.realm === 'global')) return false;
  switch (f.state) {
    case 'ok': return !s.disabled && !s.paused && !accIsCool(s, nowMs);
    case 'cool': return !s.disabled && !s.paused && accIsCool(s, nowMs);
    case 'paused': return !!s.paused;
    case 'disabled': return !!s.disabled;
  }
  return true;
}

function updateAccPager(shown, pages) {
  if ($('accCount')) $('accCount').textContent = '共 ' + accAll.length + ' 个' + (shown !== accAll.length ? ' · 筛出 ' + shown : '');
  if ($('accPageInfo')) $('accPageInfo').textContent = accPage + ' / ' + (pages || 1);
  if ($('accPrev')) $('accPrev').disabled = accPage <= 1;
  if ($('accNext')) $('accNext').disabled = !pages || accPage >= pages;
}

/* updateAccPagerMeta 服务端分页模式的计数/页码：total = 池总数，
   filtered = 筛选后总数（服务端已算好，前端不再数当前页）。 */
function updateAccPagerMeta(meta) {
  if (!meta) return;
  const total = Number(meta.total) || 0;
  const filtered = Number(meta.filtered);
  const shown = isNaN(filtered) ? total : filtered;
  const page = Number(meta.page) || 1;
  const pages = Number(meta.pages) || 1;
  if ($('accCount')) {
    $('accCount').textContent = !total ? ''
      : (shown === total ? '共 ' + total + ' 个' : '命中 ' + shown + ' / 共 ' + total + ' 个');
  }
  if ($('accPageInfo')) $('accPageInfo').textContent = page + ' / ' + pages;
  if ($('accPrev')) $('accPrev').disabled = page <= 1;
  if ($('accNext')) $('accNext').disabled = pages <= 1 || page >= pages;
}

/* accCreditsMax 积分进度条的池内基准。分页模式下当前页只有 100 行，逐页取 max
   会让同一账号的条长随翻页跳动；优先用完整 overview 的全量账号做基准（页面加载
   时后台拉一次），没有全量快照时退回当前页 max。 */
function accCreditsMax(rows) {
  const full = (overviewData && overviewData.accounts) || [];
  let m = 0;
  for (const s of full) m = Math.max(m, Number(s.credits) || 0);
  if (!m) for (const s of (rows || [])) m = Math.max(m, Number(s.credits) || 0);
  return Math.max(1, m);
}

/* renderAccounts 渲染账号表。两种调用方式：
   ① 服务端分页模式 renderAccounts(rows, meta)：rows 已是当前页且已按筛选/排序
      处理（/panel/api/accounts），meta = {total,filtered,page,pages,now}；
      本地只渲染，不再 filter/slice（这是 v2.1 #1 的核心：大池子不整包下发）。
   ② 兼容调用 renderAccounts(array)：无 meta 时退回 v2.0 的客户端筛选+分页
      （node harness 与按数组直接调用的切片测试依赖它，行为保持不变）。 */
function renderAccounts(list, meta) {
  accAll = list || [];
  const server = !!(meta && typeof meta === 'object');
  accPageMeta = server ? meta : null;
  const tb = $('accBody');
  if (!tb) return;
  let pageRows = accAll;
  let filteredCount = accAll.length;
  let pages = 1;
  if (!server) {
    if (!accAll.length) {
      tb.innerHTML = '<tr><td colspan="9"><div class="empty"><div class="big">账号池是空的</div>点击右上角「添加账号」，用浏览器登录一个 WorkBuddy 账号</div></td></tr>';
      updateAccPager(0, 1);
      return;
    }
    const filtered = accAll.filter(accMatch);
    if (!filtered.length) {
      accPage = 1;
      tb.innerHTML = '<tr><td colspan="9"><div class="empty">没有符合筛选条件的账号</div></td></tr>';
      updateAccPager(0, 1);
      return;
    }
    const pr = pageSlice(filtered, accPageSize, accPage);
    accPage = pr.page;
    pageRows = pr.rows;
    filteredCount = filtered.length;
    pages = pr.pages;
  } else if (!accAll.length) {
    // 服务端模式空页：可能是池为空，也可能是筛选无命中——用 meta.total 区分。
    const total = Number(meta.total) || 0;
    tb.innerHTML = '<tr><td colspan="9"><div class="empty">' +
      (total > 0 ? '没有符合筛选条件的账号' : '账号池是空的') + '</div></td></tr>';
    updateAccPagerMeta(meta);
    return;
  }
  // 倒计时基准：本地时钟 - 与服务端 now 的偏差（见 accNowMs）。light 投影没有
  // cool_remaining_sec，冷却时长全部由 until/breaker_until/degrade_until 本地算；
  // 304 命中时仍用 Date.now() 重取，所以倒计时不会因 ETag 稳定而冻结。
  const nowMs = server ? accNowMs(meta) : Date.now();
  // 有总额度（credits_total）→ 进度条按自身 剩余/总额 百分比；旧数据无总额 → 退回池内最高=100%
  // 基准取全量（完整 overview 快照）而非当前页，翻页与筛选时条长稳定不跳。
  const maxCred = accCreditsMax(pageRows);
  tb.innerHTML = pageRows.map(s => {
    const cool = accCoolRemaining(s, nowMs);
    let cls = '', tag;
    if (s.disabled) { cls = 'off'; tag = '<span class="tag bad">已禁用</span>'; }
    else if (s.paused) { cls = 'off'; tag = '<span class="tag warn">已暂停选号</span>'; }
    else if (cool > 0) {
      cls = 'cool';
      tag = '<span class="tag warn">' + accCoolKind(s, nowMs) + ' · ' + dur(cool) + '</span>';
    } else tag = '<span class="tag ok">可用</span>' + (s.in_flight ? '' : '');
    const note = s.reason ? '<div class="hint" style="font-size:11.5px;color:var(--ink-3);margin-top:3px">' + esc(s.reason) + '</div>' : '';
    const rateLimits = rateLimitRowsHtml(s.rate_limited_models, nowMs);
    const short = s.uid.length > 16 ? s.uid.slice(0, 16) + '…' : s.uid;
    // 企业版不限量：上游 limitNum == -1，网关以 credits_total=-1 透出（见 upstream
    // enterpriseUnlimitedTotal）。此时剩余额度不参与展示，直接标「不限」。
    const unlimited = s.credits_total === -1;
    const cred = unlimited ? '不限'
      : (s.credits == null ? '—' : (s.credits_total > 0 ? s.credits + '<span class="of">/' + s.credits_total + '</span>' : String(s.credits)));
    const pct = unlimited ? 100
      : (s.credits_total > 0
        ? Math.min(100, Math.round((s.credits || 0) / s.credits_total * 100))
        : Math.round((s.credits || 0) / maxCred * 100));
    // 成本台账 tooltip（model_costs）：每模型实测单价（≤0 = 实测免费），运维据此
    // 看「为什么总选它」——免费号垄断 / 单价排序一眼可见。
    let credTip;
    if (unlimited) credTip = '企业版不限量（上游 limitNum=-1）';
    else if (s.credits_total > 0) {
      credTip = (s.enterprise ? '企业版剩余额度 ' : '剩余 ')
        + s.credits + ' / ' + (s.enterprise ? '分配 ' : '总额 ') + s.credits_total + '（' + pct + '%）';
    } else credTip = '积分（相对池内最高）';
    const costs = (s.model_costs || []).filter(c => c.model);
    if (costs.length) {
      credTip += '\n实测单价（credits/1K）：\n' + costs.map(c =>
        '  ' + c.model + '：' + (c.cost_per_1k <= 0 ? '免费' : c.cost_per_1k)).join('\n');
    }
    const frozen = s.disabled || cool > 0;
    const tu = s.token_usage || {};
    const req = tu.request_count || 0;
    const totalTok = formatTokenCount(tu.total_tokens);
    const totalTokUnit = totalTok === '—' ? '' : '<em>tok</em>';
    const latency = formatLatency(tu.last_latency_ms);
    const rate = formatRate(tu.last_tokens_per_second);
    const usageTitle = '最近一次：' + req + ' 次 / ' + totalTok + ' / 延迟 ' + latency + ' / ' + rate;
    return '<tr class="' + cls + '" title="uid: ' + esc(s.uid) + '">' +
      '<td class="mark" aria-hidden="true"><i></i></td>' +
      '<td class="who"><div class="nm">' + remarkName(s.uid, s.nickname ? esc(s.nickname) : '<span style="color:var(--ink-3)">未命名</span>') + (s.realm === 'global' ? ' <span class="realm-tag">国际版</span>' : '') + (s.enterprise ? ' <span class="realm-tag">企业版</span>' : '') + '</div><div class="id">' + esc(short) + '</div></td>' +
      '<td>' + tag + note + rateLimits + '</td>' +
      '<td class="cred" title="' + esc(credTip) + '"><div class="n">' + cred + '</div><div class="bar"><i style="width:' + pct + '%"></i></div></td>' +
      '<td class="num">' + (s.success_count || 0) + ' <span style="color:var(--ink-3)">/</span> <span style="color:var(--bad)">' + (s.err_total || 0) + '</span></td>' +
      '<td class="num">' + (s.in_flight || 0) + '</td>' +
      '<td class="num usage-cell" title="' + esc(usageTitle) + '"><span class="usage-line" aria-label="' + esc(usageTitle) + '">' +
        '<span class="usage-item usage-count"><b>' + req + '</b><em>次</em></span>' +
        '<span class="usage-item usage-total"><b>' + totalTok + '</b>' + totalTokUnit + '</span>' +
        '<span class="usage-item usage-latency"><b>' + latency + '</b></span>' +
        '<span class="usage-item usage-rate"><b>' + rate + '</b></span>' +
      '</span></td>' +
      '<td class="num" style="color:var(--ink-3)">' + ago(s.last_success) + '</td>' +
      '<td class="acts">' +
        // 企业版无个人成长体系（签到 400「企业账号不支持该操作」/ 成长任务 403）：
        // 不渲染「签到」「任务」按钮，只留「额度」——点它走 /balance，企业额度由
        // upstream 的 get-enterprise-user-usage 口径填充。
        (s.enterprise ? '' :
          '<button class="xs ghost" data-a="checkin" data-u="' + esc(s.uid) + '"' + (s.checkin_done ? ' title="今日已签到；点击可重新签到并刷新余额"' : '') + '>' + (s.checkin_done ? '已签' : '签到') + '</button>') +
        '<button class="xs ghost" data-a="balance" data-u="' + esc(s.uid) + '"' + (s.enterprise ? ' title="刷新企业版已分配额度（上游 get-enterprise-user-usage）"' : '') + '>' + (s.enterprise ? '额度' : '余额') + '</button>' +
        // 单号积分构成：只查这一个账号（1 次上游请求）。整池对比视图已下线——
        // 逐号实时查询的批量特征有风控风险，明细改为按需单查。
        '<button class="xs ghost" data-a="pk" data-u="' + esc(s.uid) + '" title="查看单号积分构成（仅查这 1 个账号）">积分</button>' +
        (s.enterprise ? '' :
          '<button class="xs ghost" data-a="tasks" data-u="' + esc(s.uid) + '">任务</button>') +
        accDoTaskBtn(s) +
        (frozen ? '<button class="xs primary" data-a="revive" data-u="' + esc(s.uid) + '">解冻</button>'
                : (s.paused ? '<button class="xs primary" data-a="resume" data-u="' + esc(s.uid) + '">恢复选号</button>'
                            : '<button class="xs ghost" data-a="pause" data-u="' + esc(s.uid) + '" title="' + (s.enterprise ? '退出选号，但照常保活 / 刷新额度' : '退出选号，但照常签到 / 活跃上报 / 保活 / 刷新余额') + '">暂停选号</button>')) +
        (s.disabled ? '' : '<button class="xs ghost" data-a="disable" data-u="' + esc(s.uid) + '">禁用</button>') +
        '<button class="xs ghost danger" data-a="remove" data-u="' + esc(s.uid) + '">移除</button>' +
      '</td></tr>';
  }).join('');
  if (server) updateAccPagerMeta(meta);
  else updateAccPager(filteredCount, pages);
}

if ($('accTabs')) document.querySelectorAll('#accTabs .subtab').forEach(b => b.onclick = () => switchAccTab(b.dataset.tab));
/* 到期列表分页（数据同 overview 快照，翻页只重渲染当前页）。 */
if ($('expPageSize')) $('expPageSize').addEventListener('change', () => {
  expPageSize = parseInt($('expPageSize').value, 10) || 0;
  expPage = 1;
  renderExpirySnapshot(overviewData);
});
if ($('expPrev')) $('expPrev').onclick = () => { if (expPage > 1) { expPage--; renderExpirySnapshot(overviewData); } };
if ($('expNext')) $('expNext').onclick = () => { expPage++; renderExpirySnapshot(overviewData); };

/* 筛选栏 / 分页控件接线（元素常驻，事件只注册一次；重渲染只换 tbody）。
   v2.1 #1：筛选与分页全部走服务端参数 /panel/api/accounts——
   搜索输入防抖 250ms，任何筛选条件变化都回到第 1 页重新请求；翻页按钮按服务端
   返回的 pages 判断边界。 */
let accQTimer = null;
function reloadAccountPage() { loadAccountPage(false); }
if ($('accQ')) $('accQ').addEventListener('input', () => {
  clearTimeout(accQTimer);
  accQTimer = setTimeout(() => {
    const v = $('accQ').value.trim();
    if (v === accFilters.q) return;      // 值没变（比如只多敲了空格）不发请求
    accFilters.q = v;
    accPage = 1;
    reloadAccountPage();
  }, 250);
});
if ($('accState')) $('accState').addEventListener('change', () => {
  accFilters.state = $('accState').value;
  accPage = 1;
  reloadAccountPage();
});
if ($('accVer')) $('accVer').addEventListener('change', () => {
  accFilters.ver = $('accVer').value;
  accPage = 1;
  reloadAccountPage();
});
if ($('accPageSize')) $('accPageSize').addEventListener('change', () => {
  accPageSize = parseInt($('accPageSize').value, 10) || 0;
  accPage = 1;
  reloadAccountPage();
});
if ($('accPrev')) $('accPrev').onclick = () => { if (accPage > 1) { accPage--; reloadAccountPage(); } };
if ($('accNext')) $('accNext').onclick = () => {
  const pages = (accPageMeta && Number(accPageMeta.pages)) || 1;
  if (accPage < pages) { accPage++; reloadAccountPage(); }
};

/* ── 模型锁池：搜索 + 状态/地区筛选 + 客户端分页 ─────────────────────────
   数据来自 /overview 的 model_locks（5s 轮询重渲染）；筛选条件与页码是模块
   状态，轮询刷新时保持，翻页/筛选只重渲染当前页。 */
let mlAll = [];
let mlFilters = { q: '', state: '', realm: '' };
let mlPage = 1;
let mlPageSize = 100;                         // 0 = 全部

// mlMatch 模型锁行筛选：q 对模型名做大小写不敏感包含匹配，state / realm 精确匹配。
function mlMatch(r, f) {
  f = f || mlFilters;
  if (f.q && String((r && r.model) || '').toLowerCase().indexOf(f.q.toLowerCase()) < 0) return false;
  if (f.state && String((r && r.state) || '') !== f.state) return false;
  if (f.realm && String((r && r.realm) || '') !== f.realm) return false;
  return true;
}

// renderModelLocks 模型锁池：哪些模型不能用、锁了几个号、还要锁多久。
// 后端 model_locks 已按「整池不可用 → 没号可用 → 部分限流」排好序，这里只做展示。
// 入口只更新数据源；筛选/分页变化时由 renderModelLocksView 用同一份 mlAll 重渲染。
function renderModelLocks(rows) {
  mlAll = rows || [];
  renderModelLocksView();
}

function renderModelLocksView() {
  const tb = $('mlBody');
  if (!tb) return;
  const note = $('mlNote');
  const filtered = mlAll.filter(r => mlMatch(r));
  const pr = pageSlice(filtered, mlPageSize, mlPage);
  mlPage = pr.page;
  updatePagerUI({ count: 'mlCount', info: 'mlPageInfo', prev: 'mlPrev', next: 'mlNext' }, pr,
    hitCountLabel(filtered.length, mlAll.length, '个模型'));
  if (!mlAll.length) {
    tb.innerHTML = '<tr><td colspan="8"><div class="empty">当前没有模型级限流 —— 所有模型均可选</div></td></tr>';
    if (note) note.textContent = '';
    return;
  }
  if (!filtered.length) {
    tb.innerHTML = '<tr><td colspan="8"><div class="empty">没有符合筛选条件的模型</div></td></tr>';
  } else {
    const STATE = { locked: ['bad', '整池不可用'], starved: ['warn', '没号可用'], partial: ['warn', '部分限流'] };
    const left = iso => {
      const ms = parseAPITime(iso);
      return ms ? dur(Math.max(0, Math.round((ms - Date.now()) / 1000))) : '—';
    };
    tb.innerHTML = pr.rows.map(r => {
      const st = STATE[r.state] || ['mute', r.state || '—'];
      const realm = r.realm === 'global' ? '国际版' : '国内版';
      return '<tr>' +
        '<td>' + esc(r.model) + '</td>' +
        '<td><span class="realm-tag">' + realm + '</span></td>' +
        '<td><span class="tag ' + st[0] + '">' + st[1] + '</span></td>' +
        '<td class="num">' + (r.servable || 0) + ' / ' + (r.total || 0) + '</td>' +
        '<td class="num">' + (r.locked || 0) + '</td>' +
        '<td class="num">' + left(r.unlock_at || r.fully_unlock_at) + '</td>' +
        '<td class="num">' + left(r.fully_unlock_at) + '</td>' +
        '<td>' + (r.reason ? '<div class="note">' + esc(r.reason) + '</div>' : '—') + '</td>' +
        '</tr>';
    }).join('');
  }
  if (note) {
    // note 描述整池（不受筛选影响）：筛选只影响当前页看到哪些行。
    const bad = mlAll.filter(r => r.state === 'locked' || r.state === 'starved').length;
    note.textContent = bad ? bad + ' 个模型整池不可用' : mlAll.length + ' 个模型部分限流';
  }
}

let mlQTimer = null;
if ($('mlQ')) $('mlQ').addEventListener('input', () => {
  clearTimeout(mlQTimer);
  mlQTimer = setTimeout(() => { mlFilters.q = $('mlQ').value.trim(); mlPage = 1; renderModelLocksView(); }, 150);
});
if ($('mlState')) $('mlState').addEventListener('change', () => {
  mlFilters.state = $('mlState').value; mlPage = 1; renderModelLocksView();
});
if ($('mlRealm')) $('mlRealm').addEventListener('change', () => {
  mlFilters.realm = $('mlRealm').value; mlPage = 1; renderModelLocksView();
});
if ($('mlPageSize')) $('mlPageSize').addEventListener('change', () => {
  mlPageSize = parseInt($('mlPageSize').value, 10) || 0;
  mlPage = 1;
  renderModelLocksView();
});
if ($('mlPrev')) $('mlPrev').onclick = () => { if (mlPage > 1) { mlPage--; renderModelLocksView(); } };
if ($('mlNext')) $('mlNext').onclick = () => {
  const pages = pageSlice(mlAll.filter(r => mlMatch(r)), mlPageSize, mlPage).pages;
  if (mlPage < pages) { mlPage++; renderModelLocksView(); }
};

/* ── 总览数据（v2.1 #1：lite 轮询 + 按需全量）──────────────────────────
   /panel/api/overview?lite=1 只含计数/元信息/模型锁池（约 1-2 KB），5s 轮询用它
   更新统计条、导航与模型锁池；账号行改由服务端分页接口 /panel/api/accounts 提供。
   完整 overview（带 accounts）只在「积分到期」tab、页面加载（一次性，供积分合计
   与备注弹窗）时按需拉取，并受 60s 新鲜窗口约束——账号池列表不再每 5s 拉全量。 */
let creditsSum = null;            // 积分合计：优先用 overview 的 credits_sum/credits_total_sum
let overviewAt = 0;               // 完整 overview 的拉取时刻（判断 60s 新鲜窗口）
let overviewFetching = false;     // 完整 overview 请求在途（避免重复拉全量）
const OVERVIEW_FULL_TTL_MS = 60 * 1000;

function renderCreditsStat() {
  const el = $('sCredits');
  if (!el) return;
  if (!creditsSum) { el.textContent = '—'; return; }
  el.textContent = creditsSum.tot > 0 ? creditsSum.rem + ' / ' + creditsSum.tot : String(creditsSum.rem);
}

/* applyOverviewMeta 把 overview / overview?lite=1 共有的计数与元信息写进界面。
   两者字段一致（lite 只是没有 accounts），所以同一段逻辑服务两条数据源。 */
function applyOverviewMeta(d) {
  if (!d) return;
  // 轮询间隔随 overview 下发（后端 panel.poll_interval_sec，默认 5，clamp 2-60）：
  // 非法值回落 5。每次成功都刷新一次，面板里改完配置后下一轮 polls 自动跟上。
  pollBaseSec = pollIntervalSec(d.poll_interval_sec);
  $('sTotal').textContent = d.total;
  $('sHealthy').textContent = d.healthy;
  $('sCooling').textContent = d.cooling;
  $('sDisabled').textContent = d.disabled;
  // 暂停选号单列（issue #125）：它只关选号、照常签到保活，与禁用是两种状态。
  if ($('sPaused')) $('sPaused').textContent = d.paused == null ? '-' : d.paused;
  // 积分合计：v2.1 的 lite overview 也带 credits_sum / credits_total_sum（全量 overview
  // 同样有），5s 轮询即可刷新统计条；缺这两个字段时（旧后端/mock）回落完整
  // overview 的 accounts 求和，再没有就保持上一次的值（显示「—」直到拿到数据）。
  if (d.credits_sum != null || d.credits_total_sum != null) {
    creditsSum = { rem: Number(d.credits_sum) || 0, tot: Number(d.credits_total_sum) || 0 };
  } else if (Array.isArray(d.accounts)) {
    let rem = 0, tot = 0;
    for (const s of d.accounts) { rem += Number(s.credits) || 0; tot += Number(s.credits_total) || 0; }
    creditsSum = { rem: rem, tot: tot };
  }
  renderCreditsStat();
  $('sSticky').textContent = d.sticky_sessions;
  $('navSub').textContent = 'v' + d.version;
  $('navVer').textContent = 'v' + d.version;
  $('navRedis').textContent = d.redis_mode === 'upstash' ? 'Redis 镜像' : '本地内存';
  $('navState').textContent = d.healthy > 0 ? '服务正常' : (d.total ? '无可用账号' : '待添加账号');
  const p = $('navPulse');
  p.className = 'pulse' + (d.healthy > 0 ? '' : (d.total ? ' warn' : ' bad'));
  $('accNote').textContent = d.in_flight_full ? d.in_flight_full + ' 个账号在途占满' : '';
  const up = Math.floor(d.uptime_sec);
  $('subMeta').textContent = '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') + Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
  renderModelLocks(d.model_locks);
}

/* loadSummary 只拉 lite overview：5s 轮询的常驻请求，~1-2 KB。 */
async function loadSummary(quiet) {
  try {
    const d = await api('overview?lite=1');
    applyOverviewMeta(d);
    return true;
  } catch (e) { if (!quiet) toast(e.message, 'err'); return false; }
}

/* loadOverview 完整 overview（带 accounts）：到期卡片/备注弹窗用（积分合计优先用
   同响应的 credits_sum，见 applyOverviewMeta）。
   注意：它不再渲染账号表——账号行唯一数据源是服务端分页接口。 */
async function loadOverview(quiet) {
  if (overviewFetching) return false;
  overviewFetching = true;
  try {
    const d = await api('overview');
    overviewData = d;
    overviewAt = Date.now();
    applyOverviewMeta(d);
    // 到期卡片用同一次响应里的按日批次刷新。2 分钟内的实时明细（逐包查询）最新且
    // 更全，保留；过期后由这份快照接管，避免点过一次「检查」后卡片永远停在旧数据。
    const liveFresh = lastPackages && (Date.now() - lastPackagesAt) < EXP_FRESH_MS;
    if ($('expBox') && !liveFresh) renderExpirySnapshot(d);
    return true;
  } catch (e) { if (!quiet) toast(e.message, 'err'); return false; }
  finally { overviewFetching = false; }
}

/* ensureFullOverview 完整 overview 的 60s 新鲜窗口：切「积分到期」tab / 到期 tab
   轮询时先看缓存，过期才真正请求（避免频繁拉 689KB 全量）。force=true 强制刷新。 */
async function ensureFullOverview(quiet, force) {
  if (!force && overviewData && (Date.now() - overviewAt) < OVERVIEW_FULL_TTL_MS) return true;
  return loadOverview(quiet);
}

/* invalidateFullOverview 账号集合/积分可能已变（增删号、签到、刷新余额）后作废
   完整 overview 的新鲜窗口：下次需要全量时再拉，避免操作后继续用旧合计。 */
function invalidateFullOverview() { overviewAt = 0; }

/* loadAccountsPage 当前页账号（服务端筛选/分页 + ETag/304）。
   304 时 apiCached 直接复用缓存 rows；200 时用响应 now 校准 accClockSkew，
   渲染倒计时用 Date.now()-skew 本地重算（light 投影没有 cool_remaining_sec），
   所以「一直 304」时倒计时也继续走、不会冻结。 */
async function loadAccountPage(quiet) {
  const qs = accQueryString(accFilters, accPage, accPageSize);
  const seq = ++accReqSeq;
  try {
    const res = await apiCached('accounts?' + qs, ACCOUNTS_CACHE);
    // 过期响应（先发后到）：用户已经改了筛选/翻页，不能覆盖新结果。
    if (seq !== accReqSeq) return true;
    const d = res.data || {};
    // 只有 200 才带新的服务端 now：用它校准本地时钟偏差；304 沿用上次 skew。
    if (!res.cached && Number(d.now) > 0) {
      accClockSkew = Date.now() - Number(d.now) * 1000;
    }
    const meta = {
      now: Number(d.now) || 0,
      skew: accClockSkew,
      total: Number(d.total) || 0,
      filtered: d.filtered == null ? null : Number(d.filtered),
      page: Number(d.page) || 1,
      pages: Number(d.pages) || 1,
    };
    // 服务端可能把 page 钳到有效范围（筛选后页数变少），以响应为准。
    accPage = meta.page;
    accPageMeta = meta;
    renderAccounts(d.rows || [], meta);
    return true;
  } catch (e) {
    if (seq !== accReqSeq) return true;
    if (!quiet) toast('账号列表加载失败：' + e.message, 'err');
    return false;
  }
}

/* refreshAccountsView 账号池视图的一轮刷新（5s 轮询与操作后刷新共用）：
   ① lite overview（统计条/导航/模型锁池）；
   ② 当前页 accounts（服务端筛选+分页，ETag 命中 304 时复用缓存）；
   ③ 仅当「积分到期」tab 激活时才按 60s 窗口补完整 overview（低频）。
   列表 tab 下不拉全量——这是「账号池不再每 5s 拉 689KB」的落点。 */
async function refreshAccountsView(quiet) {
  const [summaryOK, pageOK] = await Promise.all([loadSummary(quiet), loadAccountPage(quiet)]);
  if (accTab === 'expiry' && view === 'accounts') {
    const ok = await ensureFullOverview(quiet);
    if (ok && overviewData) renderExpirySnapshot(overviewData);
  }
  return !!(summaryOK && pageOK);
}

/* refreshAccountPool 操作后的等价刷新入口（替代旧的 loadOverview(true)）：
   summary + 当前页 accounts，都是小响应；同时作废完整 overview 的新鲜窗口，
   避免操作后继续用旧积分合计/旧到期快照。 */
async function refreshAccountPool(quiet) {
  invalidateFullOverview();
  return refreshAccountsView(quiet);
}


/* runAccountDotask 账号行「做任务」：单账号执行全部可自动任务。
   与「任务」弹窗（看进度、单项执行）并行保留：这个按钮是「没空逐项点」时的
   一把梭，走同步接口 POST /accounts/{uid}/tasks/run，含真实对话，约 1-2 分钟。
   后端契约：{ok,uid,results[{task_code,status,message}],summary{total,done,skipped,error,claimed}}；
   企业版/无凭证走 200 + ok:false + message（不发起上游请求），4xx/5xx 由 api() 抛出。 */
async function runAccountDotask(btn, uid) {
  if (!btn || !confirm('将对该账号依次执行全部可自动任务（含真实对话），预计 1-2 分钟。\n执行期间请勿关闭页面。确认继续？')) return;
  const label = btn.textContent;
  // 标记进行中：5s 轮询重渲染 tbody 后 accDoTaskBtn 会据此继续渲染 disabled，
  // 不会出现「按钮看起来又能点」的假可用态（服务端 per-account 锁会回 409）。
  dotaskBusy.add(uid);
  btn.disabled = true;
  btn.textContent = '做任务中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(uid) + '/tasks/run', { method: 'POST' });
    // ok:false = 门控拒绝（企业版 / 无凭证 / 未知账号等），message 是后端给的可读原因。
    if (r && r.ok === false) { toast(r.message || r.error || '做任务失败', 'err'); return; }
    const results = (r && r.results) || [];
    const s = (r && r.summary) || {};
    const count = k => (s[k] != null ? Number(s[k]) : results.filter(x => x && x.status === k).length);
    const done = count('done'), skipped = count('skipped'), error = count('error');
    if (!results.length && !(Number(s.total) > 0)) {
      toast('没有可执行的任务（可能已全部完成）', 'ok');
      return;
    }
    toast('做任务完成：成功 ' + done + ' 项，跳过 ' + skipped + ' 项' + (error ? '，失败 ' + error + ' 项' : ''),
      error ? 'err' : 'ok');
  } catch (e) {
    toast('做任务失败：' + e.message, 'err');
  } finally {
    dotaskBusy.delete(uid);
    btn.disabled = false;
    btn.textContent = label;
    refreshAccountPool(true);   // 任务可能写了积分/进度，刷新 summary + 当前页
  }
}

$('accBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-a]');
  if (!b) return;
  const u = b.dataset.u, a = b.dataset.a;
  // 「做任务」耗时分钟级：单独走一个分支，确认前不动按钮状态（取消就什么都不发生）。
  if (a === 'dotask') { await runAccountDotask(b, u); return; }
  if (a === 'remove' && !confirm('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。确认移除？')) return;
  if (a === 'disable' && !confirm('禁用后该账号不再参与选号（保号任务默认也跳过），需手动解冻才能恢复。若只是想临时让位、仍要保号，请改用「暂停选号」。确认禁用？')) return;
  b.disabled = true;
  try {
    if (a === 'checkin') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/checkin', { method: 'POST' });
      toast('签到完成' + (r.credits != null ? '，积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + (r.checkin_message ? '（' + r.checkin_message + '）' : ''), 'ok');
    } else if (a === 'balance') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/balance', { method: 'POST' });
      toast('余额已更新：' + r.credits + (r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok');
    } else if (a === 'pk') {
      openPackages(u);
    } else if (a === 'revive') {
      await api('accounts/' + encodeURIComponent(u) + '/revive', { method: 'POST' });
      toast('已解冻', 'ok');
    } else if (a === 'disable') {
      await api('accounts/' + encodeURIComponent(u) + '/disable', { method: 'POST' });
      toast('已禁用', 'ok');
    } else if (a === 'pause') {
      await api('accounts/' + encodeURIComponent(u) + '/pause', { method: 'POST' });
      toast('已暂停选号（签到 / 保活照常）', 'ok');
    } else if (a === 'resume') {
      await api('accounts/' + encodeURIComponent(u) + '/resume', { method: 'POST' });
      toast('已恢复选号', 'ok');
    } else if (a === 'tasks') {
      openTasks(u);
    } else if (a === 'remove') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/remove', { method: 'POST' });
      toast(r.file_error ? '已移除（凭证文件删除失败：' + r.file_error + '）' : '已移除', 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; refreshAccountPool(true); }
});

$('btnCheckinAll').onclick = async () => {
  try { await api('checkin_all', { method: 'POST' }); toast('全部签到已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnKeepaliveAll').onclick = async () => {
  try { await api('keepalive_all', { method: 'POST' }); toast('全部保活已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
// 重新扫描 auths/ 目录：外部脚本批量导入/删除凭证后免重启对齐（只增删账号，
// 已存在账号的积分/冷却状态保留）。后端对「扫到 0 个文件」有拒绝保护。
if ($('btnRescan')) $('btnRescan').onclick = async () => {
  try {
    const r = await api('accounts/rescan', { method: 'POST' });
    toast('扫描 ' + r.scanned + ' 个文件：新增 ' + r.added + '，剔除 ' + r.removed, 'ok');
    refreshAccountPool(true);
  } catch (e) { toast(e.message, 'err'); }
};
$('btnTravelAll').onclick = async () => {
  try { await api('travel_all', { method: 'POST' }); toast('旅行巡检已开始（含领养链路），结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnActivityAll').onclick = async () => {
  try { await api('activity_all', { method: 'POST' }); toast('活跃上报已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};

/* ── 模型 ─────────────────────────────────────────────────────────── */
/* 实测上限标注：scripts/probe_max_tokens.py --panel-out 写入探测结果，
   /panel/api/model_probes 只读透传。探测键带域前缀（cn:glm-5.2），模型表
   显示裸名，按「精确命中或 :后缀」关联。无数据时本列退回上游声称值。 */
function fmtK(n) { n = Number(n || 0); return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n); }
function probeDays(ts) {
  if (!ts) return null;
  const t = new Date(String(ts).replace(' ', 'T'));
  const d = (Date.now() - t.getTime()) / 86400000;
  return isNaN(d) ? null : Math.floor(d);
}
function outCell(m, pr) {
  if (!pr) return '<td class="num">' + (m.max_output_tokens ? fmtK(m.max_output_tokens) : '—') + '</td>';
  const tip = '声称 ' + (pr.claimed ? fmtK(pr.claimed) : '?') + ' · 实测 ' + (pr.measured ? fmtK(pr.measured) : '?') +
    (pr.note ? ' · ' + pr.note : '') + (pr.tested_at ? ' · 探测于 ' + pr.tested_at : '');
  const days = probeDays(pr.tested_at);
  const stale = days !== null && days > 30 ? ' · ' + days + ' 天前' : '';
  if (pr.verdict === 'clamped' && pr.measured) {
    if (pr.claimed && pr.measured < pr.claimed) {
      const x = pr.claimed / pr.measured;
      const xs = (x >= 10 ? Math.round(x) : Math.round(x * 10) / 10) + '×';
      return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--warn);font-weight:600">' +
        fmtK(pr.measured) + ' ⚠</span><div class="note">钳制 ' + xs + stale + '</div></td>';
    }
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ok)">' + fmtK(pr.measured) +
      (pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓') + '</span></td>';
  }
  if (pr.verdict === 'at_least' && pr.measured)
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">≥' + fmtK(pr.measured) + '</span></td>';
  return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">?</span><div class="note">未测出' + stale + '</div></td>';
}

/* rateCell 倍率列：牌价 vs 生效价。上游 credits 是牌价（转正后基准倍率），
   modelPromotions 给当前生效折扣（限时免费 factor=0 / 夜间五折 0.5 等）——
   WorkBuddy 客户端显示的正是生效价。有折扣：生效价大字 + 标签 + 划线牌价，
   悬停带时段说明；无 factor 只有标签（错峰类）：牌价 + 标签。 */
function rateCell(m) {
  const tip = m.promo_note ? ' title="' + esc(m.promo_note) + '"' : '';
  if (m.promo_factor != null && m.promo_credits) {
    const base = m.credits ? ' <s style="color:var(--ink-3);font-size:11.5px">' + esc(m.credits) + '</s>' : '';
    const label = m.promo_label ? ' <span class="tag ok">' + esc(m.promo_label) + '</span>' : '';
    return '<span' + tip + ' style="cursor:help"><b>' + esc(m.promo_credits) + '</b>' + label + base + '</span>';
  }
  if (m.promo_label) {
    return '<span' + tip + ' style="cursor:help">' + (m.credits ? esc(m.credits) : '—') +
      ' <span class="tag warn">' + esc(m.promo_label) + '</span></span>';
  }
  return m.credits ? esc(m.credits) : '—';
}

async function loadModels() {
  const tb = $('mdBody');
  tb.innerHTML = '<tr><td colspan="7"><div class="empty">正在向上游查询…</div></td></tr>';
  try {
    // 探测、模型锁与最近请求都是本地可选增强：任一失败不影响模型目录本身。
    // 可用性标签不额外发推理请求，只用模型锁和最近真实请求结果合成。
    const [d, pr, st, rm] = await Promise.all([
      api('models'),
      api('model_probes').catch(() => ({})),
      api('status').catch(() => ({})),
      api('request_metrics').catch(() => ({})),
    ]);
    mdAll = d.models || [];
    mdProbes = pr.probes || {};
    mdLocks = Array.isArray(st.model_locks) ? st.model_locks : [];
    mdRecent = Array.isArray(rm.recent) ? rm.recent : [];
    if (!mdAll.length) {
      tb.innerHTML = '<tr><td colspan="7"><div class="empty">上游未返回模型</div></td></tr>';
      $('mdCount').textContent = '';
      $('mdNote').textContent = '上游未返回模型';
      return;
    }
    // 探测键带域前缀（cn:glm-5.2），模型表显示裸名，按「精确命中或 :后缀」关联。
    const probeKeys = Object.keys(mdProbes);
    mdProbeOf = id => mdProbes[id] || mdProbes[probeKeys.find(k => k.endsWith(':' + id))];
    const hit = mdAll.filter(m => mdProbeOf(m.id)).length;
    $('mdNote').textContent = mdAll.length + ' 个模型 · 已刷新降级缓存' + (hit ? ' · ' + hit + ' 个有实测上限' : '');
    renderModels();
  } catch (e) {
    mdAll = [];
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">' + esc(e.message) + '</div></td></tr>';
    $('mdCount').textContent = '';
  }
}

/* ── 模型筛选（按条件查询）───────────────────────────────────────────
   模型目录一次拉全（几十条），筛选与排序全部在前端完成：改条件零延迟，且不会
   因为调一次筛选就打一次上游——/panel/api/models 是直连上游的实时查询，很贵。
   条件之间是 AND；每个条件为空即不参与判定。 */
// mdRateValue 当前生效的积分倍率数值：优先促销价（限时免费 = 0），无倍率记为
// Infinity 排到最后（排序时"没有价格"不该冒充最便宜）。
function mdRateValue(m) {
  const raw = (m.promo_credits != null && m.promo_credits !== '') ? m.promo_credits : m.credits;
  const n = parseFloat(String(raw == null ? '' : raw).replace(/[^\d.]/g, ''));
  return Number.isFinite(n) ? n : Infinity;
}

// mdSearchText 参与关键字搜索的字段（ID / 展示名 / 厂商 / 描述 / 标签）。
function mdSearchText(m) {
  return [m.id, m.name, m.vendor, m.description, (m.tags || []).join(' ')]
    .filter(Boolean).join(' ').toLowerCase();
}

// mdMatch 单个模型是否满足全部筛选条件。
function mdMatch(m, f) {
  f = f || mdFilter;
  if (f.q) {
    const text = mdSearchText(m);
    // 空格分词后逐个匹配：多关键词是 AND，便于"cn 视觉"这类组合查询。
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  if (f.realm && !String(m.id || '').startsWith(f.realm + ':')) return false;
  if (f.cap === 'tool' && !m.supports_tool_call) return false;
  if (f.cap === 'vision' && !m.supports_images) return false;
  if (f.cap === 'reasoning' && !m.supports_reasoning) return false;
  if (f.cap === 'default' && !m.is_default) return false;
  if (f.effort === 'off') {
    if (!m.can_disable_thinking) return false;
  } else if (f.effort && !(m.supported_efforts || []).includes(f.effort)) {
    return false;
  }
  const factor = m.promo_factor == null ? null : Number(m.promo_factor);
  if (f.promo === 'promo' && factor == null && !m.promo_label) return false;
  if (f.promo === 'free' && !(factor === 0)) return false;
  if (f.promo === 'discount' && !(factor != null && factor > 0)) return false;
  return true;
}

// mdSortList 按当前排序条件返回新数组（不改动入参，保持上游原始顺序可回溯）。
function mdSortList(list, f) {
  f = f || mdFilter;
  const out = list.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  if (f.sort === 'rate') out.sort((a, b) => mdRateValue(a) - mdRateValue(b));
  else if (f.sort === 'context') out.sort((a, b) => num(b.context_length) - num(a.context_length));
  else if (f.sort === 'output') out.sort((a, b) => num(b.max_output_tokens) - num(a.max_output_tokens));
  else if (f.sort === 'name') out.sort((a, b) => String(a.id || '').localeCompare(String(b.id || '')));
  return out;
}

// mdAvailabilityOf 合成 Global 模型当前可用性：模型锁优先于最近请求结果。
// 只返回有真实本地证据的状态；目录中存在但从未请求过的模型标为“待验证”，
// 避免把“上游列出了模型”误当成“当前账号有权限”。
function mdAvailabilityOf(m, locks, recent) {
  const id = String((m && m.id) || '');
  if (!id.startsWith('global:')) return null;
  locks = Array.isArray(locks) ? locks : mdLocks;
  recent = Array.isArray(recent) ? recent : mdRecent;
  const bare = id.slice('global:'.length);
  const lock = locks.find(r => r && r.realm === 'global' && r.model === bare);
  if (lock) {
    const reason = String(lock.reason || '').trim();
    const lower = reason.toLowerCase();
    if (lock.state === 'partial') {
      return { key: 'partial', label: '部分可用', cls: 'warn',
        tip: '仍有 ' + Number(lock.servable || 0) + ' 个 global 账号可服务；已锁 ' + Number(lock.locked || 0) + ' 个' + (reason ? '；' + reason : '') };
    }
    if (lock.state === 'starved') {
      return { key: 'starved', label: '账号暂不可用', cls: 'mute',
        tip: '没有账号能服务该模型，但并非该模型自身被锁；请检查账号冷却/在途占用' };
    }
    if (lower.startsWith('11158') || lower.includes('not allowed for user type')) {
      return { key: 'denied', label: '无权限', cls: 'bad',
        tip: '当前 global 账号类型不支持该模型' + (reason ? '；' + reason : '') };
    }
    if (lower.startsWith('11102') || lower.includes('service info not found')) {
      return { key: 'missing', label: '上游无此模型', cls: 'bad',
        tip: '上游目录残留，实际请求返回模型不存在' + (reason ? '；' + reason : '') };
    }
    return { key: 'limited', label: '限流/不可用', cls: 'warn',
      tip: '模型级冷却中' + (reason ? '；' + reason : '') };
  }
  const ev = recent.find(e => e && e.model === id);
  if (ev) {
    const status = Number(ev.status || 0);
    const when = ev.time ? new Date(ev.time).toLocaleString('zh-CN', { hour12: false }) : '';
    if (ev.outcome === 'success' || (status >= 200 && status < 300)) {
      return { key: 'ok', label: '可用', cls: 'ok',
        tip: '最近一次真实请求成功' + (when ? ' · ' + when : '') };
    }
    if (status === 429) {
      return { key: 'rate', label: '限流', cls: 'warn',
        tip: '最近一次请求被限流' + (when ? ' · ' + when : '') };
    }
    if (status >= 500) {
      return { key: 'upstream', label: '上游错误', cls: 'bad',
        tip: '最近一次请求上游错误 HTTP ' + status + (when ? ' · ' + when : '') };
    }
    return { key: 'failed', label: '最近失败', cls: 'bad',
      tip: '最近一次请求失败 HTTP ' + (status || '—') + (when ? ' · ' + when : '') };
  }
  return { key: 'unknown', label: '待验证', cls: 'mute',
    tip: '尚无模型锁或近期真实请求记录；目录存在不代表当前账号有权限' };
}

function mdAvailabilityTag(m) {
  const av = mdAvailabilityOf(m);
  if (!av) return '';
  return ' <span class="tag ' + av.cls + ' md-avail" title="' + esc(av.tip) + '">' + esc(av.label) + '</span>';
}

// mdRowHtml 单个模型行（纯渲染，便于独立测试）。
function mdRowHtml(m, pr) {
  const eff = (m.supported_efforts || []).slice();
  if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');
  const effs = eff.length ? eff.map(e => '<span class="tag warn">' + esc(e) + '</span>').join(' ')
    : '<span style="color:var(--ink-3);font-size:12.5px">' + (m.supports_reasoning ? '固定档 · 默认 ' + esc(m.default_effort || '?') : '不支持思考') + '</span>';
  // 能力徽标：默认模型 / 工具调用 / 视觉 / 纯推理（上游目录全字段透出，缺失不显示）
  const caps = [];
  if (m.is_default) caps.push('<span class="tag ok">默认</span>');
  if (m.supports_tool_call) caps.push('<span class="tag warn">工具</span>');
  if (m.supports_images) caps.push('<span class="tag warn">视觉</span>');
  if (m.supports_reasoning && !m.can_disable_thinking) caps.push('<span class="tag warn">思考常开</span>');
  const capHtml = caps.length ? '<div class="id" style="margin-top:2px">' + caps.join(' ') + '</div>' : '';
  const tip = m.description ? ' title="' + esc(m.description) + '"' : '';
  return '<tr><td class="mark" aria-hidden="true"><i></i></td><td class="who"' + tip + '><div class="nm"><span class="mid">' + esc(m.id) + '</span>' + mdAvailabilityTag(m) + '</div><div class="id">' + esc(m.name || '') + '</div>' + capHtml + '</td>' +
    '<td class="num">' + rateCell(m) + '</td>' +
    '<td>' + (m.default_effort ? '<span class="tag ok">' + esc(m.default_effort) + '</span>' : '<span style="color:var(--ink-3)">—</span>') + '</td>' +
    '<td class="efs" style="white-space:normal">' + effs + '</td>' +
    '<td class="num">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td>' +
    outCell(m, pr) + '</tr>';
}

function renderModels() {
  const tb = $('mdBody');
  const list = mdSortList(mdAll.filter(m => mdMatch(m)));
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">没有符合当前筛选条件的模型</div></td></tr>';
  } else {
    tb.innerHTML = list.map(m => mdRowHtml(m, mdProbeOf(m.id))).join('');
  }
  const filtered = list.length !== mdAll.length;
  $('mdCount').textContent = !mdAll.length ? ''
    : filtered ? '命中 ' + list.length + ' / ' + mdAll.length + ' 个模型'
    : mdAll.length + ' 个模型';
  $('mdCount').className = filtered ? 'note src-off' : 'note';
}

function resetModelFilter() {
  mdFilter = { q: '', realm: '', cap: '', effort: '', promo: '', sort: 'default' };
  $('mdQ').value = ''; $('mdRealm').value = ''; $('mdCap').value = '';
  $('mdEffort').value = ''; $('mdPromo').value = ''; $('mdSort').value = 'default';
  renderModels();
}

// 筛选控件：输入框防抖 120ms（长列表逐字符重排不必每键一次），下拉即时。
let mdQTimer = null;
$('mdQ').oninput = () => {
  clearTimeout(mdQTimer);
  mdQTimer = setTimeout(() => { mdFilter.q = $('mdQ').value.trim(); renderModels(); }, 120);
};
for (const [id, key] of [['mdRealm', 'realm'], ['mdCap', 'cap'], ['mdEffort', 'effort'], ['mdPromo', 'promo'], ['mdSort', 'sort']]) {
  const el = $(id);
  if (!el) continue;
  el.onchange = () => { mdFilter[key] = el.value; renderModels(); };
}
$('mdReset').onclick = resetModelFilter;
$('btnModels').onclick = loadModels;

/* ── 日志（频道：全部/任务/对话/系统） ─────────────────────────────── */
/* 日志页筛选 + 客户端分页：频道 chips 与「最近 500 行」的取数协议不变（仍由
   /panel/api/logs 返回全量、前端过滤），只是渲染前多一道关键词过滤 + 切片——
   500 行日志一次铺开在手机端又长又卡。筛选条件与页码是模块状态，5s 轮询重渲染
   时保持；翻页只重画当前页。 */
let logCh = 'all';
let logAll = [];                              // 最近一次拉取的全量日志（筛选/翻页的数据源）
let logFilters = { q: '' };
let logPage = 1;
let logPageSize = 100;                        // 0 = 全部

// logMatch 日志行关键词匹配：只对 text 做大小写不敏感包含匹配；空关键词全通过。
function logMatch(e, q) {
  if (!q) return true;
  return String((e && e.text) || '').toLowerCase().indexOf(String(q).toLowerCase()) >= 0;
}

// filterLogs 频道 + 关键词过滤（纯函数；只筛已拉取的条目，不发新请求）。
function filterLogs(entries, ch, q) {
  return (entries || []).filter(e => e && (ch === 'all' || e.ch === ch) && logMatch(e, q));
}

$('logChips').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-ch]');
  if (!b) return;
  logCh = b.dataset.ch;
  logPage = 1;                       // 换频道 = 换结果集，回到第一页
  document.querySelectorAll('#logChips .chip').forEach(c => c.classList.toggle('on', c === b));
  loadLogs();
});

function renderLogBox(scrollToEnd) {
  const box = $('logBox');
  if (!box) return;
  const filtered = filterLogs(logAll, logCh, logFilters.q);
  const pr = pageSlice(filtered, logPageSize, logPage);
  logPage = pr.page;
  updatePagerUI({ count: 'logCount', info: 'logPageInfo', prev: 'logPrev', next: 'logNext' }, pr,
    hitCountLabel(filtered.length, logAll.length, '行'));
  box.innerHTML = pr.rows.length
    ? pr.rows.map(e => {
      const lvl = /error|失败|错误/.test(e.text) ? ' e' : /warn|冷却|熔断/.test(e.text) ? ' w' : '';
      const t = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : '';
      const ch = logCh === 'all' ? '<i class="lch c-' + esc(e.ch) + '">' + ({ task: '任务', chat: '对话', sys: '系统' }[e.ch] || e.ch) + '</i>' : '';
      return '<span class="ln' + lvl + '">' + ch + esc(t + ' ' + e.text) + '</span>';
    }).join('')
    : '<span style="color:var(--ink-3)">' + (logAll.length ? '没有匹配的日志' : '暂无日志') + '</span>';
  // 自动滚动只发生在「轮询刷新 + 原本就在底部」时；筛选/翻页不追底，否则一打字就跳走。
  if (scrollToEnd) box.scrollTop = box.scrollHeight;
}

let logQTimer = null;
if ($('logQ')) $('logQ').addEventListener('input', () => {
  clearTimeout(logQTimer);
  logQTimer = setTimeout(() => { logFilters.q = $('logQ').value.trim(); logPage = 1; renderLogBox(false); }, 150);
});
if ($('logPageSize')) $('logPageSize').addEventListener('change', () => {
  logPageSize = parseInt($('logPageSize').value, 10) || 0;
  logPage = 1;
  renderLogBox(false);
});
if ($('logPrev')) $('logPrev').onclick = () => { if (logPage > 1) { logPage--; renderLogBox(false); } };
if ($('logNext')) $('logNext').onclick = () => {
  const pages = pageSlice(filterLogs(logAll, logCh, logFilters.q), logPageSize, logPage).pages;
  if (logPage < pages) { logPage++; renderLogBox(false); }
};

async function loadLogs() {
  const box = $('logBox');
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  const limit = ($('reqLimit') && $('reqLimit').value) || 100;
  // 时间范围由归档侧过滤（不是前端筛已拉取的条目）：区间落在更早的时间段时，
  // 「最近 N 条」里根本不会有那些记录，必须让服务端按时间取。
  const rq = trangeQuery('reqRange', false);
  rq.set('limit', limit);
  // 积分历史账号下拉的候选来自账号池；这里同步一次（只在账号集合变化时重建 DOM）。
  syncCreditAccounts();
  try {
    const [d, metrics, requestRows, credit] = await Promise.all([
      api('logs'),
      api('request_metrics').catch(() => ({})),
      api('request_logs?' + rq.toString()).catch(() => ({ entries: [] })),
      api(creditHistoryQuery()).catch(() => null),
    ]);
    // 归档开启时以归档为准——「区间内没有记录」是一个真实结果，不能回落成内存里
    // 的最近 100 条（那会把筛选条件之外、时间范围之外的请求显示出来）。
    // 只有归档关闭时才回落到内存指标，保证没有归档的部署仍能看到最近请求。
    const archiveOn = !!(metrics && metrics.archive && metrics.archive.enabled);
    const recent = archiveOn ? (requestRows.entries || []) : (metrics.recent || []);
    renderRequestMetrics(metrics, recent);
    logAll = d.entries || [];
    renderLogBox(logPin && atEnd);
    const counts = {};
    for (const e of logAll) counts[e.ch] = (counts[e.ch] || 0) + 1;
    $('logNote').textContent = logCh === 'all'
      ? '任务 ' + (counts.task || 0) + ' · 对话 ' + (counts.chat || 0) + ' · 系统 ' + (counts.sys || 0)
      : (logCh === 'task' ? '任务' : logCh === 'chat' ? '对话' : '系统') + ' ' + filterLogs(logAll, logCh, '').length + ' 行';
    renderCreditHistory(credit);
    return true;   // 轮询据此判断本轮成功/失败（失败才退避）
  } catch (e) { /* 概览已提示 */ return false; }
}

function renderRequestMetrics(m, entries) {
  m = m || {};
  const a = m.archive || {};
  $('reqSummary').textContent =
    '已完成 ' + fmtTok(m.completed) +
    ' · 成功 ' + (m.success_rate == null ? '—' : Number(m.success_rate).toFixed(1) + '%') +
    ' · HTTP ' + (m.http_success_rate == null ? '—' : Number(m.http_success_rate).toFixed(1) + '%') +
    ' · 平均 ' + fmtMs(m.avg_duration_ms) +
    ' · 进行中 ' + String(m.in_flight || 0);
  $('reqNote').textContent = a.enabled
    ? 'JSONL 归档 ' + fmtBytes(a.bytes) + (a.dropped_writes ? ' · 丢弃 ' + a.dropped_writes + ' 条' : '') +
      (a.last_error ? ' · 错误：' + a.last_error : '')
    : '仅内存指标，JSONL 归档已关闭';

  reqEntries = entries || [];
  renderRequestTable();
}

/* ── 积分历史（每次真实查到余额与上次比对，变动即留痕）─────────────────
   纯格式化函数集中在此，便于前端测试按切片断言；变动列 +N / −N（U+2212，
   与请求记录一致），余额用千分位。 */
function creditNum(n) {
  return Number(n || 0).toLocaleString('zh-CN');
}
function creditDeltaText(delta) {
  const d = Number(delta || 0);
  if (d > 0) return '+' + creditNum(d);
  if (d < 0) return '−' + creditNum(-d);
  return '0';
}
function creditTimeText(e) {
  if (!e || !e.time) return '—';
  const d = new Date(e.time);
  // 正常链路 time 由 Go 的 time.Time 序列化，恒为 RFC3339；这里挡的是手工改文件/
  // 将来换格式等脏数据——toLocaleTimeString 遇到非法日期不抛异常，只会渲染出
  // "Invalid Date" 这种没意义的字样。
  return isNaN(d.getTime()) ? '—' : d.toLocaleTimeString('zh-CN', { hour12: false });
}
function creditAccountText(e) {
  e = e || {};
  return e.account ? String(e.account) : (e.uid || '—');
}
function creditBalanceText(e) {
  return creditNum(e && e.after);
}
function creditDescText(e) {
  return '余额 ' + creditNum(e && e.before) + ' → ' + creditNum(e && e.after);
}
function creditEntryText(e) {
  return creditTimeText(e) + ' | ' + creditAccountText(e) + ' | ' + creditDeltaText(e && e.delta) +
    ' | ' + creditBalanceText(e) + ' | ' + creditDescText(e);
}
function creditHistoryNote(entries) {
  entries = entries || [];
  if (!entries.length) return '暂无积分变动记录';
  let net = 0;
  for (const e of entries) net += Number(e && e.delta || 0);
  return entries.length + ' 条 · 净 ' + (net > 0 ? '+' : net < 0 ? '−' : '') + creditNum(Math.abs(net));
}

/* 积分历史筛选 + 分页：账号下拉走服务端 credit_history?uid=（后端有 uid 时先全量读
   账本再过滤，比前端筛已截断的 limit 条更完整），分页在前端对已取回条目切片。
   creditFilter/creditPage/creditPageSize 是模块状态，5s 轮询重渲染时保持。 */
let creditFilter = { uid: '' };
let creditPage = 1;
let creditPageSize = 100;                      // 0 = 全部
let creditData = null;                         // 最近一次响应（翻页/改页大小不再发请求）

// creditMatch 积分历史行账号匹配：uid 精确匹配（空筛选全通过）。纯函数便于测试。
function creditMatch(e, uid) {
  if (!uid) return true;
  return String((e && e.uid) || '') === String(uid);
}

function renderCreditHistory(d) {
  const body = $('creditBody'), note = $('creditNote');
  if (!body) return;
  if (d) creditData = d;
  if (!d) {
    if (note) note.textContent = '积分历史不可用';
    body.innerHTML = '<tr><td colspan="5" style="color:var(--ink-3)">积分历史不可用</td></tr>';
    if (typeof updatePagerUI === 'function') {
      updatePagerUI({ info: 'creditPageInfo', prev: 'creditPrev', next: 'creditNext' }, { page: 1, pages: 1 }, '');
    }
    return;
  }
  const raw = d.entries || [];
  // 切片环境（node harness 的对抗测试直接调本函数）可能没有模块级分页状态 /
  // pageSlice：此时退化为「全部渲染」，与加筛选前的行为一致，不得抛异常。
  const uid = (typeof creditFilter === 'object' && creditFilter) ? creditFilter.uid : '';
  const filtered = raw.filter(e => creditMatch(e, uid));
  const size = typeof creditPageSize === 'number' ? creditPageSize : 0;
  const canPage = typeof pageSlice === 'function';
  const pr = canPage
    ? pageSlice(filtered, size, typeof creditPage === 'number' ? creditPage : 1)
    : { rows: filtered.slice(), page: 1, pages: 1, total: filtered.length };
  if (canPage) {
    if (typeof creditPage === 'number') creditPage = pr.page;
    if (typeof updatePagerUI === 'function') {
      const label = typeof hitCountLabel === 'function'
        ? hitCountLabel(filtered.length, raw.length, '条') : null;
      updatePagerUI({ count: 'creditCount', info: 'creditPageInfo', prev: 'creditPrev', next: 'creditNext' }, pr, label);
    }
  }
  // 计数/净额跟随当前账号筛选（不是当前页）：翻页时 header 文案不该跳。
  if (note) note.textContent = creditHistoryNote(filtered);
  body.innerHTML = pr.rows.length
    ? pr.rows.map(e => '<tr>' +
      '<td>' + esc(creditTimeText(e)) + '</td>' +
      '<td>' + esc(creditAccountText(e)) + '</td>' +
      '<td class="num">' + esc(creditDeltaText(e && e.delta)) + '</td>' +
      '<td class="num">' + esc(creditBalanceText(e)) + '</td>' +
      '<td>' + esc(creditDescText(e)) + '</td>' +
      '</tr>').join('')
    : '<tr><td colspan="5" style="color:var(--ink-3)">' +
      (raw.length ? '没有匹配的积分变动记录' : '暂无积分变动记录') + '</td></tr>';
}
function creditLimitValue() {
  const el = $('creditLimit');
  const n = el ? Number(el.value) : 100;
  return n === 300 || n === 1000 ? n : 100;
}

// creditHistoryQuery 积分历史请求路径：带上当前账号筛选（服务端过滤，账本完整）。
function creditHistoryQuery() {
  let q = 'credit_history?limit=' + creditLimitValue();
  if (creditFilter && creditFilter.uid) q += '&uid=' + encodeURIComponent(creditFilter.uid);
  return q;
}
async function loadCreditHistory() {
  syncCreditAccounts();
  let d = null;
  try { d = await api(creditHistoryQuery()); } catch (e) { d = null; }
  renderCreditHistory(d);
}

/* poolAccountList 账号全集（积分历史下拉 / 备注弹窗等需要「全部账号」的场景）：
   v2.1 #1 之后账号表只有当前页 rows（accAll），全量列表优先取完整 overview 快照
   （页面加载时后台拉一次；积分到期 tab 会按 60s 窗口刷新），没有快照才退当前页。 */
function poolAccountList() {
  if (overviewData && Array.isArray(overviewData.accounts) && overviewData.accounts.length) return overviewData.accounts;
  return accAll;
}

/* syncCreditAccounts 积分历史账号下拉：候选来自账号池全集（见 poolAccountList）。
   只在账号集合变化时重建 options——日志页 5s 轮询会反复调到这里，每次换
   innerHTML 会把用户正在展开的下拉框关掉。选中的账号被移除后回落「全部账号」。 */
function syncCreditAccounts() {
  const sel = $('creditAccount');
  if (!sel) return;
  const list = poolAccountList();
  const sig = list.map(s => s.uid + ':' + (s.nickname || '')).join('|');
  if (sel.dataset && sel.dataset.sig === sig) return;
  if (sel.dataset) sel.dataset.sig = sig;
  const opts = ['<option value="">全部账号</option>'];
  for (const s of list) {
    opts.push('<option value="' + esc(s.uid) + '">' + esc(s.nickname || String(s.uid).slice(0, 8)) + '</option>');
  }
  sel.innerHTML = opts.join('');
  const uid = creditFilter && creditFilter.uid ? creditFilter.uid : '';
  sel.value = uid;
  const v = typeof sel.value === 'string' ? sel.value : '';
  if (v !== uid) { creditFilter.uid = v; creditPage = 1; }
}

/* reqMatch 请求记录筛选：q 对 IP/UA/模型/账号/请求 ID 做空格分词的 AND 包含匹配，
   outcome 精确匹配。两者都在已拉取的条目上做（最多 1000 条），不发新请求。 */
function reqMatch(e, f) {
  f = f || reqFilter;
  if (f.outcome && String(e && e.outcome || '') !== f.outcome) return false;
  if (f.q) {
    const text = [e && e.client_ip, e && e.user_agent, e && e.model, e && e.account, e && e.request_id]
      .filter(Boolean).join(' ').toLowerCase();
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  return true;
}

// filterReqEntries 请求记录筛选（纯函数；renderRequestTable 与计数共用，便于测试）。
function filterReqEntries(entries, f) {
  return (entries || []).filter(e => reqMatch(e, f));
}

function reqOutcomeTag(e) {
  const outcome = String(e && e.outcome || '');
  const label = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' }[outcome] || outcome || '—';
  const cls = outcome === 'success' ? 'ok'
    : outcome === 'interrupted' ? 'warn'
    : outcome ? 'bad' : 'mute';
  return '<span class="tag ' + cls + '">' + esc(String(e && e.status || '—') + ' ' + label) + '</span>';
}

function reqTokenCell(e) {
  const total = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  return total ? fmtTok(total) : '—';
}

function reqCreditCell(e) {
  if (!e || !e.credit_known) return '<span class="muted">—</span>';
  const v = Number(e.credit);
  return Number.isFinite(v) ? trimFixed(v.toFixed(2)) : '<span class="muted">—</span>';
}

/* renderRequestTable 渲染请求记录表——只渲染当前页。来源列是这一版的重点：IP 用
   等宽字体方便扫，UA 单行截断（完整值在 title 里，行本身用 requestLogText 作 tooltip）。
   筛选（q / outcome）作用在已拉取的条目上（最多 1000 条），计数与翻页都基于这个
   结果集；筛选变化由调用方重置到第 1 页。 */
function renderRequestTable() {
  const matched = filterReqEntries(reqEntries, reqFilter);
  const pr = pageSlice(matched, reqPageSize, reqPage);
  reqPage = pr.page;
  const tb = $('reqBody');
  if (!tb) return;
  tb.innerHTML = pr.rows.map(e => {
    const when = e && e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
    const ip = e && e.client_ip ? e.client_ip : '';
    const ua = e && e.user_agent ? e.user_agent : '';
    const rid = e && e.request_id ? e.request_id : '';
    return '<tr title="' + esc(requestLogText(e)) + '">' +
      '<td class="num">' + esc(when) + '</td>' +
      '<td>' + reqOutcomeTag(e) + '</td>' +
      '<td>' + esc(e && e.model || '—') + '</td>' +
      '<td>' + esc(e && e.account || '—') + '</td>' +
      '<td>' + (ip ? '<span class="clip ip" title="' + esc(ip) + '">' + esc(ip) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td>' + (ua ? '<span class="clip" title="' + esc(ua) + '">' + esc(ua) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td class="num">' + fmtMs(e && e.duration_ms) + '</td>' +
      '<td class="num">' + reqTokenCell(e) + '</td>' +
      '<td class="num">' + reqCreditCell(e) + '</td>' +
      '<td>' + (rid ? '<span class="clip rid" title="' + esc(rid) + '">' + esc(rid) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '</tr>';
  }).join('') || '<tr><td colspan="10" class="empty">' +
      (reqEntries.length ? '没有符合当前筛选条件的请求记录' : '暂无请求记录') + '</td></tr>';

  const filtered = matched.length !== reqEntries.length;
  // 归档里的旧条目没有来源字段（该功能上线前写入）：这时提示开关/历史原因，
  // 而不是让人以为筛选坏了。
  const hasSource = reqEntries.some(e => e && (e.client_ip || e.user_agent));
  $('reqCount').textContent = hitCountLabel(matched.length, reqEntries.length, '条') +
    (hasSource || !reqEntries.length ? '' : ' · 来源未记录');
  $('reqCount').className = (filtered || !hasSource) ? 'note src-off' : 'note';
  updatePagerUI({ info: 'reqPageInfo', prev: 'reqPrev', next: 'reqNext' }, pr);
}

/* 请求记录筛选控件。搜索框防抖 150ms：最多 1000 行重渲染，不必每键一次。
   这段顶层绑定放在 requestLogText 之前，是为了让"纯函数切片"式前端测试
   （slice requestLogText → fmtBytes）只拿到无副作用的格式化函数。 */
let reqQTimer = null;
if ($('reqQ')) $('reqQ').oninput = () => {
  clearTimeout(reqQTimer);
  reqQTimer = setTimeout(() => { reqFilter.q = $('reqQ').value.trim(); reqPage = 1; renderRequestTable(); }, 150);
};
if ($('reqOutcome')) $('reqOutcome').onchange = () => {
  reqFilter.outcome = $('reqOutcome').value;
  reqPage = 1;
  renderRequestTable();
};
if ($('reqPageSize')) $('reqPageSize').addEventListener('change', () => {
  reqPageSize = parseInt($('reqPageSize').value, 10) || 0;
  reqPage = 1;
  renderRequestTable();
});
if ($('reqPrev')) $('reqPrev').onclick = () => { if (reqPage > 1) { reqPage--; renderRequestTable(); } };
if ($('reqNext')) $('reqNext').onclick = () => {
  const pages = pageSlice(filterReqEntries(reqEntries, reqFilter), reqPageSize, reqPage).pages;
  if (reqPage < pages) { reqPage++; renderRequestTable(); }
};
if ($('reqLimit')) $('reqLimit').onchange = () => { reqPage = 1; loadLogs(); };
if ($('btnReqReload')) $('btnReqReload').onclick = () => { reqPage = 1; loadLogs(); };
// 时间范围：默认「全部历史」——请求记录页的历史行为就是"取最近 N 条"，
// 加一个默认收窄的区间会让打开页面时看到的条数凭空变少。换区间 → 回第 1 页。
if ($('reqRange')) trangeBind('reqRange', () => { reqPage = 1; loadLogs(); }, '0');
if ($('creditLimit')) $('creditLimit').onchange = loadCreditHistory;
if ($('btnCreditReload')) $('btnCreditReload').onclick = loadCreditHistory;
// 积分历史：账号筛选 / 每页条数 / 翻页。账号筛选走服务端 uid 过滤（loadCreditHistory
// 重新取数）；分页只重渲染已取回的数据，不再发请求。
if ($('creditAccount')) $('creditAccount').addEventListener('change', () => {
  creditFilter.uid = $('creditAccount').value;
  creditPage = 1;
  loadCreditHistory();
});
if ($('creditPageSize')) $('creditPageSize').addEventListener('change', () => {
  creditPageSize = parseInt($('creditPageSize').value, 10) || 0;
  creditPage = 1;
  renderCreditHistory(creditData);
});
if ($('creditPrev')) $('creditPrev').onclick = () => { if (creditPage > 1) { creditPage--; renderCreditHistory(creditData); } };
if ($('creditNext')) $('creditNext').onclick = () => {
  const pages = pageSlice(creditData && creditData.entries || [], creditPageSize, creditPage).pages;
  if (creditPage < pages) { creditPage++; renderCreditHistory(creditData); }
};

function requestLogText(e) {
  const when = e && e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
  const outcomeLabel = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' };
  const token = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  let credit = 'credit —';
  if (e && e.credit_known) {
    const value = Number(e.credit);
    if (Number.isFinite(value)) credit = String(Number(value.toFixed(2))) + ' credit';
  }
  return [
    when,
    String(e && e.status || '—') + ' ' + (outcomeLabel[e && e.outcome] || (e && e.outcome) || '—'),
    e && e.model || '—',
    e && e.account || '—',
    e && e.client_ip || '—',
    e && e.user_agent || '—',
    fmtMs(e && e.duration_ms),
    fmtTok(token) + ' tok',
    credit,
    cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens) === '—' ? '' : '命中 ' + cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens),
    e && e.request_id || '—',
  ].filter(Boolean).join(' | ');
}

/* 缓存命中率纯文本（issue #92）：requestLogText 与积分表/kpi 卡共用。
   自包含（不依赖 trimFixed）：前端纯函数切片测试只截取本段。 */
function cacheRateText(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), total = h + m;
  if (!total) return '—';
  return String(Math.round(h / total * 1000) / 10) + '%';
}

function fmtBytes(bytes) {
  const n = Number(bytes || 0);
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1024 / 1024).toFixed(1) + ' MB';
}
$('btnLogPin').onclick = () => {
  logPin = !logPin;
  $('btnLogPin').textContent = '自动滚动：' + (logPin ? '开' : '关');
};

/* ── 配置 ─────────────────────────────────────────────────────────── */
const CFG_MAP = {
  strategy_domestic: ['pool', 'strategy', 'domestic'],
  strategy_international: ['pool', 'strategy', 'international'],
  domestic_account_order: ['pool', 'account_order', 'domestic'],
  international_account_order: ['pool', 'account_order', 'international'],
  listen: ['listen'], api_key: ['api_key'],
  // v2.0.1 #4：面板密钥与网关密钥分离。panel.api_key 为空 = 回落网关 api_key
  // （向后兼容），后端启动日志会提示「面板与网关共用密钥，建议分离」。
  panel_api_key: ['panel', 'api_key'],
  // v2.0.1 #2：面板轮询间隔（秒），overview 响应带出，默认 5 / clamp 2-60。
  poll_interval_sec: ['panel', 'poll_interval_sec'],
  // v2.0.1 #4：非回环监听 + 密钥全空时拒绝启动的显式逃生门（默认 false）。
  allow_anonymous: ['security', 'allow_anonymous'],
  package_detail_limit: ['panel', 'package_detail_limit'],
  checkin_hours: ['schedule', 'checkin_hours'], checkin_enabled: ['schedule', 'checkin_enabled'], growth_hours: ['schedule', 'growth_hours'], growth_enabled: ['schedule', 'growth_enabled'],
  travel_hours: ['schedule', 'travel_hours'], travel_enabled: ['schedule', 'travel_enabled'],
  activity_hours: ['schedule', 'activity_hours'], activity_enabled: ['schedule', 'activity_enabled'],
  keepalive_hours: ['schedule', 'keepalive_hours'], keepalive_enabled: ['schedule', 'keepalive_enabled'],
  balance_refresh_enabled: ['schedule', 'balance_refresh_enabled'], balance_refresh_minutes: ['schedule', 'balance_refresh_minutes'],
  // v2.0.1 #10/#11 子项：全池余额刷新的并发闸门/启动首刷保护，定时任务补跑窗口。
  balance_refresh_concurrency: ['schedule', 'balance_refresh_concurrency'],
  balance_refresh_startup_delay: ['schedule', 'balance_refresh_startup_delay'],
  balance_refresh_startup_force: ['schedule', 'balance_refresh_startup_force'],
  catch_up_minutes: ['schedule', 'catch_up_minutes'],
  task_sweep_enabled: ['schedule', 'task_sweep_enabled'], task_sweep_minutes: ['schedule', 'task_sweep_minutes'],
  include_disabled_in_tasks: ['schedule', 'include_disabled_in_tasks'],
  max_in_flight: ['pool', 'max_in_flight'], max_in_flight_global: ['pool', 'max_in_flight_global'],
  breaker_threshold: ['pool', 'breaker_threshold'],
  degrade_threshold: ['pool', 'degrade_threshold'], degrade_cooldown: ['pool', 'degrade_cooldown'],
  degrade_cooldown_max: ['pool', 'degrade_cooldown_max'],
  cost_explore_interval: ['pool', 'cost_explore_interval'],
  credit_floor: ['pool', 'credit_floor'],
  prefer_expiring: ['pool', 'prefer_expiring'], expiring_soon: ['pool', 'expiring_soon'],
  soft_rate: ['cooldown', 'soft_rate'], soft_rate_max: ['cooldown', 'soft_rate_max'],
  breaker_cooldown: ['pool', 'breaker_cooldown'], breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
  idle_weight_per_hour: ['pool', 'idle_weight_per_hour'], idle_weight_max: ['pool', 'idle_weight_max'],
  ttl: ['session_sticky', 'ttl'],
  timeout_seconds: ['upstream', 'timeout_seconds'], header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
  idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'], user_agent: ['upstream', 'user_agent'],
  prompt_mode: ['prompt', 'mode'], prompt_file: ['prompt', 'file'],
  sanitize_blacklist_fingerprints: ['features', 'sanitize_blacklist_fingerprints'],
  session_sticky_enabled: ['session_sticky', 'enabled'],
  request_client_info: ['logging', 'request_client_info'],
};
/* 「覆盖型」文本字段：空串本身是有意义的取值（= 回落到内置默认），必须照发。
 *
 * 其余文本字段保持「空 = 不下发」的既有语义——那是防误清空的保护，不是 bug：
 * 表单里某个框没填，通常意味着"没改"，把它当成"请清空"会静默抹掉配置。
 *
 * 但覆盖型字段正好相反：清空 = 明确要求回到默认。漏发它们会让面板显示"已保存"
 * 而值其实没变（issue #102 附带发现 2：user_agent 清空后 config.json 里仍是旧值）。
 *
 * 刻意不含顶层 api_key：清空它 = 关闭整个鉴权，误触代价是网关变成无鉴权公开服务。
 * 该字段（以及提示文案"留空 = 不鉴权"与现状不符的问题）单独处理。
 *
 * panel_api_key（panel.api_key）例外：它的空串语义是「回落到网关 api_key」，
 * 不关闭鉴权，所以必须能照发空串（否则面板与网关分离后就再也切不回来了）。
 */
const CLEARABLE_CFG = new Set(['user_agent', 'prompt_file', 'panel_api_key']);

function dig(obj, path) { return path.reduce((o, k) => (o == null ? undefined : o[k]), obj); }
function put(obj, path, val) {
  let o = obj;
  for (let i = 0; i < path.length - 1; i++) { if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {}; o = o[path[i]]; }
  o[path[path.length - 1]] = val;
}

async function loadConfig() {
  try {
    const d = await api('config');
    cfgLoaded = d.config;
    $('cfgPath').textContent = d.path || '';
    const f = $('cfgForm');
    for (const [name, path] of Object.entries(CFG_MAP)) {
      const el = f.elements[name];
      if (!el) continue;
      const v = dig(cfgLoaded, path);
      if (el.type === 'checkbox') el.checked = !!v;
      else if (Array.isArray(v)) el.value = v.join(', ');
      else el.value = v == null ? (name.startsWith('strategy_') ? 'weighted_random' : '') : v;
    }
    markDurationFields(); // 回填后重置校验态（清掉残留红框；现值来自后端必然合法）
    $('cfgNote').textContent = '';
  } catch (e) { toast('读取配置失败：' + e.message, 'err'); }
}
function collectConfig() {
  const f = $('cfgForm'), out = {};
  for (const [name, path] of Object.entries(CFG_MAP)) {
    const el = f.elements[name];
    if (!el) continue;
    let v;
    if (el.type === 'checkbox') v = el.checked;
    else if (el.type === 'number') { v = el.value.trim() === '' ? undefined : Number(el.value); }
    else {
      const raw = el.value.trim();
      // 覆盖型字段空串照发（见 CLEARABLE_CFG）；其余空 = 不下发。
      if (name.endsWith('_account_order')) v = raw.split(/[,，\s]+/).filter(Boolean);
      else if (raw === '') v = CLEARABLE_CFG.has(name) ? '' : undefined;
      else if (name.endsWith('_hours')) v = raw.split(/[,，\s]+/).filter(Boolean).map(Number);
      else v = raw;
    }
    if (v !== undefined) put(out, path, v);
  }
  return out;
}
/* Go 时长字段即时校验：空 = 沿用现值（collectConfig 跳过发送）；非空必须是
   ParseDuration 语法（30m / 2h / 600s / 1h30m，可组合可带小数）。与后端
   config.go normalize() 的 time.ParseDuration 同口径，脏值在前端就地标红，
   不再等到保存被拒。
   单独放行裸 "0"：Go 的 ParseDuration("0") 合法（= 0 时长），多个字段用
   "0 = 关闭/不延迟"表意（expiring_soon、balance_refresh_startup_delay）；
   旧正则只认带单位的写法，会把合法的 0 误标红。 */
const DURATION_RE = /^(0|(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+)$/;
const DURATION_FIELDS = ['soft_rate', 'soft_rate_max', 'breaker_cooldown', 'breaker_cooldown_max',
  'degrade_cooldown', 'degrade_cooldown_max', 'cost_explore_interval', 'expiring_soon', 'ttl',
  'balance_refresh_startup_delay'];
const DURATION_TIP = '格式应为 Go 时长：30m / 2h / 600s / 1h30m（0 = 关闭/不延迟）';
function durationBad(name) {
  const el = $('cfgForm').elements[name];
  if (!el) return false;
  const v = el.value.trim();
  return v !== '' && !DURATION_RE.test(v);
}
function markDurationFields() {
  for (const name of DURATION_FIELDS) {
    const el = $('cfgForm').elements[name];
    if (!el) continue;
    const bad = durationBad(name);
    el.classList.toggle('invalid', bad);
    el.title = bad ? DURATION_TIP : '';
  }
}
$('cfgForm').addEventListener('input', ev => {
  if (DURATION_FIELDS.includes(ev.target.name)) markDurationFields();
});
$('btnEye').onclick = () => {
  const el = $('cfgKey');
  const show = el.type === 'password';
  el.type = show ? 'text' : 'password';
  $('btnEye').textContent = show ? '隐藏' : '显示';
};
$('btnCfgReload').onclick = loadConfig;
$('cfgForm').onsubmit = async ev => {
  ev.preventDefault();
  // 时长字段脏值拦截：标红 + toast 点名，不发保存请求（后端同样会拒，这里前置）。
  markDurationFields();
  const firstBad = DURATION_FIELDS.find(durationBad);
  if (firstBad) {
    const el = $('cfgForm').elements[firstBad];
    el.focus();
    toast('「' + (el.closest('.fld')?.querySelector('.lb')?.textContent || firstBad) + '」' + DURATION_TIP, 'err');
    return;
  }
  const btn = $('btnCfgSave');
  btn.disabled = true; btn.textContent = '保存中…';
  // 保存前的面板密钥现值：用于判断本次是否真的改了 panel.api_key。
  const prevPanelKey = String(dig(cfgLoaded, ['panel', 'api_key']) || '');
  try {
    const r = await api('config', { method: 'POST', body: JSON.stringify(collectConfig()) });
    const n = (r.restart_required || []).length;
    toast(n ? '配置已保存，其中 ' + n + ' 项需重启进程生效' : '配置已保存并立即生效', 'ok');
    // 本机凭据（localStorage: wb2api.key）同步：
    //  - 网关 api_key 是热生效的，和旧版一样立即沿用新值；
    //  - 面板密钥 panel.api_key 的生效点是重启后（后端把它列为 restart_required），
    //    改动含「清空回落网关 key」两种方向。若保存后立刻把本地凭据换成新值，重启
    //    前每次请求都会 401，所以改动时保持旧凭据；重启后旧凭据 401，密钥框会提示
    //    「面板密钥可能已变更（panel.api_key）」，用户重新输入即可。
    const panelKey = (($('cfgPanelKey') || {}).value || '').trim();
    const gatewayKey = $('cfgKey').value.trim();
    if (panelKey === prevPanelKey) {
      const effective = panelKey || gatewayKey;
      if (effective) localStorage.setItem(LS_KEY, effective);
    }
    // 保存成功说明当前凭据仍有效：清掉 401 停表状态，恢复自动轮询。
    pollAuthStopped = false;
    keyPrompted = false;
    refFailures = 0;
    if (!refTimer && !refInFlight) scheduleNextPoll();
    loadConfig();
    refreshAccountPool(true);
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '保存配置'; }
};

/* ── 添加账号 ─────────────────────────────────────────────────────── */
function openAdd() {
  $('addVeil').classList.add('on');
  // 重置到登录标签
  switchAddTab('login');
  $('addPick').hidden = false;
  $('addLoad').hidden = true; $('addReady').hidden = true;
  $('addDone').hidden = true; $('addErr').hidden = true;
  $('importDone').hidden = true; $('importErr').hidden = true;
  $('btnCopyUrl').hidden = true; $('btnOpenUrl').hidden = true;
  $('btnStartLogin').hidden = false; $('btnStartLogin').disabled = false;
  stopPoll();
}
function switchAddTab(tab) {
  document.querySelectorAll('#addTabs .tab').forEach(b => b.classList.toggle('on', b.dataset.tab === tab));
  $('addTabLogin').hidden = tab !== 'login';
  $('addTabImport').hidden = tab !== 'import';
}
document.querySelectorAll('#addTabs .tab').forEach(b => {
  b.onclick = () => switchAddTab(b.dataset.tab);
});
function startAddLogin() {
  const realm = (document.querySelector('input[name="addRealm"]:checked') || {}).value || 'cn';
  $('btnStartLogin').disabled = true;
  $('addLoad').hidden = false; $('addErr').hidden = true;
  api('login/start', { method: 'POST', body: JSON.stringify({ realm }) }).then(r => {
    loginState = r.state;
    $('addUrl').textContent = r.url;
    $('addPick').hidden = true; // 选域锁定（会话已按该域发起）
    $('addLoad').hidden = true; $('addReady').hidden = false;
    $('btnStartLogin').hidden = true;
    $('btnCopyUrl').hidden = false; $('btnOpenUrl').hidden = false;
    loginTimer = setInterval(pollLogin, 3000);
  }).catch(e => {
    $('addLoad').hidden = true;
    $('btnStartLogin').disabled = false;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message;
  });
}
function stopPoll() { if (loginTimer) { clearInterval(loginTimer); loginTimer = null; } }
async function pollLogin() {
  if (!loginState) return;
  try {
    const r = await api('login/poll?state=' + encodeURIComponent(loginState));
    if (r.done) {
      stopPoll();
      $('addReady').hidden = true;
      $('addDone').hidden = false;
      $('addDone').textContent = '已添加 ' + (r.nickname || r.uid) + (r.realm === 'global' ? '（国际版）' : '') + (r.credits >= 0 ? ' · 积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + '，账号已载入池中';
      setTimeout(() => { closeAdd(); refreshAccountPool(true); }, 1600);
    }
  } catch (e) {
    stopPoll();
    $('addReady').hidden = true;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message + '（关闭后重新添加）';
  }
}
function closeAdd() { stopPoll(); loginState = null; $('addVeil').classList.remove('on'); }
$('btnCloseAdd').onclick = closeAdd;
$('btnStartLogin').onclick = startAddLogin;
$('btnOpenUrl').onclick = () => open($('addUrl').textContent, '_blank');
$('btnCopyUrl').onclick = () => navigator.clipboard.writeText($('addUrl').textContent)
  .then(() => toast('链接已复制', 'ok'), () => toast('复制失败，请手动选择复制', 'err'));
$('importFile').onchange = async () => {
  const file = $('importFile').files[0];
  if (!file) return;
  $('importDone').hidden = true; $('importErr').hidden = true;
  const fd = new FormData();
  fd.append('file', file);
  const h = {};
  const k = localStorage.getItem(LS_KEY);
  if (k) h['Authorization'] = 'Bearer ' + k;
  try {
    const r = await fetch('/panel/api/import/cockpit', { method: 'POST', body: fd, headers: h });
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
    $('importDone').hidden = false;
    $('importDone').textContent = '导入完成：成功 ' + d.imported + ' 个' + (d.skipped ? '，跳过 ' + d.skipped + ' 个' : '');
    if (d.errors && d.errors.length) {
      console.warn('import errors:', d.errors);
    }
    refreshAccountPool(true);
  } catch (e) {
    $('importErr').hidden = false;
    $('importErr').textContent = '导入失败：' + e.message;
  }
  $('importFile').value = '';
};

/* ── 顶部动作 ─────────────────────────────────────────────────────── */
$('btnAdd').onclick = openAdd;
$('btnRefresh').onclick = async () => {
  const b = $('btnRefresh');
  const label = b.textContent;
  b.disabled = true; b.textContent = '已提交…';
  try {
    // v2.0.1 #10：后端把 balance_all 改成异步——置位单飞闸门后立即返回
    // {ok:true,started:true}，不再同步等全池刷完（1146 个账号会让反代 504）。
    // 所以这里只等「提交成功」；2.5s 后补拉一次 overview 让首批余额先出现，
    // 剩下的由常规轮询继续刷新。重复点击的 409（同类任务正在执行）由 api()
    // 抛出，catch 直接展示后端 error 文案。
    const r = await api('balance_all', { method: 'POST' });
    if (r && r.ok === false) { toast(r.error || r.message || '刷新提交失败', 'err'); return; }
    toast('已开始刷新余额（后台执行，完成后自动刷新）', 'ok');
    setTimeout(() => refreshAccountPool(true), 2500);
  } catch (e) { toast('刷新失败：' + e.message, 'err'); }
  finally { b.disabled = false; b.textContent = label; }
  if (view === 'logs') loadLogs();
};

/* ── 轮询治理（v2.0.1 #2）─────────────────────────────────────────────
   旧实现是 setInterval(refreshVisible, 5000)，有三处浪费（UPGRADE_PROPOSALS
   #2）：页面隐藏照跑、失败不退避、401 后每 5s 重试并反复抢焦点弹密钥框。
   现改为「自调度 setTimeout」：
     - 基础间隔取 overview.poll_interval_sec（后端 panel.poll_interval_sec，
       默认 5、clamp 2-60、非法回落 5）；首次 overview 成功后才有定时器；
     - 页面隐藏 → 清定时器、不发请求；恢复可见 → 立即刷一次再重新排期；
     - 连续失败按 5s → 10s → 30s → 60s 退避（默认基础 5s 时；即基础间隔的
       1×/2×/6×/12×，上限 60s），成功恢复基础间隔；
     - 401 → 停表不自动重试，密钥框每个失败周期只自动弹一次；用户手动保存
       密钥（btnKey）后 start() 恢复；
     - 每次排期在退避间隔上 ±20% 抖动，避免多标签页同频打后端。
   纯函数（pollIntervalSec / pollBackoffSec / nextPollDelay /
   pollVisibilityAction）集中在本段前部，node harness 可按锚点切片断言。 */

let refTimer = null;          // 下一次轮询的 setTimeout 句柄（不是 setInterval）
let refFailures = 0;          // 连续轮询失败次数（成功清零，驱动退避）
let refInFlight = false;      // 一轮轮询请求在途（响应慢时避免叠加请求）
let pollBaseSec = 5;          // 基础轮询间隔（秒），来自 overview.poll_interval_sec
let pollArmed = false;        // 已进入轮询状态（start() 置位）
let pollAuthStopped = false;  // 401：暂停自动轮询，等用户手动保存密钥
let keyPrompted = false;      // 本失败周期密钥框是否已自动弹过（只弹一次）
let pageHidden = !!(document && document.visibilityState === 'hidden'); // 隐藏即暂停

/* pollIntervalSec 解析 overview.poll_interval_sec → 秒。
   非法值（缺省/null/空串/非数字）回落 5；合法但越界的 clamp 到 2-60。 */
function pollIntervalSec(v) {
  if (v === undefined || v === null || v === '') return 5;
  const n = Number(v);
  if (!Number.isFinite(n)) return 5;
  return Math.min(60, Math.max(2, Math.round(n)));
}

/* pollBackoffSec 连续失败后的退避间隔（秒，未含抖动）：
   失败 1 次仍按基础间隔重试（瞬时抖动不必惩罚），之后按 2× / 6× / 12× 基础
   间隔退避；默认基础 5s 时正好是提案里的 5s → 10s → 30s → 60s。 */
function pollBackoffSec(baseSec, failureCount) {
  const base = pollIntervalSec(baseSec);
  const f = Math.max(0, Math.floor(Number(failureCount) || 0));
  const mult = f <= 1 ? 1 : f === 2 ? 2 : f === 3 ? 6 : 12;
  return Math.min(60, base * mult);
}

/* nextPollDelay 返回下一次 setTimeout 的毫秒数：退避间隔 ±20% 抖动。
   rand01 ∈ [0,1] 由调用方注入（测试可确定化）：0 → -20%，0.5 → 不抖，1 → +20%。
   抖动后仍封顶 60s —— 退避上限是「实际间隔」的上限，不因抖动被突破。 */
function nextPollDelay(baseSec, failureCount, rand01) {
  const base = pollBackoffSec(baseSec, failureCount);
  const r = Number(rand01);
  const rand = Number.isFinite(r) ? Math.min(1, Math.max(0, r)) : 0.5;
  const ms = Math.round(base * (0.8 + 0.4 * rand) * 1000);
  return Math.min(60000, ms);
}

/* pollVisibilityAction 可见性状态机：hidden↔visible 的状态转换 → 动作。
   用「上一次的 hidden 状态」判断，重复的同向 visibilitychange 不重复启停。 */
function pollVisibilityAction(hiddenNow, wasHidden) {
  if (!!hiddenNow === !!wasHidden) return 'noop';
  return hiddenNow ? 'pause' : 'resume';
}

function stopPollTimer() {
  if (refTimer) { clearTimeout(refTimer); refTimer = null; }
}

/* scheduleNextPoll 安排下一次轮询。隐藏 / 401 / 未启动时只清表不排期，
   因此不会发请求——这是「页面隐藏后流量归零」的实现点。 */
function scheduleNextPoll() {
  stopPollTimer();
  if (!pollArmed || pageHidden || pollAuthStopped) return;
  refTimer = setTimeout(pollTick, nextPollDelay(pollBaseSec, refFailures, Math.random()));
}

/* pollTick 一轮轮询：按当前视图分派（accounts/logs/taskscenter，语义与原
   refreshVisible 一致）。请求返回后才排下一次，天然不重叠；refInFlight 再兜
   一层「上一轮还没回来、其它路径又触发排期」的并发。 */
async function pollTick() {
  refTimer = null;
  if (!pollArmed || pageHidden || pollAuthStopped) return;
  if (refInFlight) { scheduleNextPoll(); return; }
  refInFlight = true;
  let ok = true;
  try {
    ok = (await refreshVisible()) !== false;
  } catch (e) {
    ok = false;
  } finally {
    refInFlight = false;
  }
  if (pollAuthStopped) return;          // 401：不自动重试，等用户手动保存密钥
  if (ok) refFailures = 0; else refFailures += 1;
  scheduleNextPoll();
}

/* onAuthRequired 401 统一处理：停止自动轮询（避免后台标签页反复弹密钥框），
   密钥框每个失败周期只自动弹一次；btnKey 保存成功 / 配置保存成功后恢复。 */
function onAuthRequired() {
  pollAuthStopped = true;
  stopPollTimer();
  if (keyPrompted) return;
  keyPrompted = true;
  openKey();
}

/* handleVisibilityChange 可见性切换：隐藏只清定时器（不请求）；恢复立即跑
   一轮（pollTick 会顺带重新排期）。返回 Promise 便于 node harness 等待。 */
function handleVisibilityChange() {
  const hiddenNow = !!(document && document.visibilityState === 'hidden');
  const action = pollVisibilityAction(hiddenNow, pageHidden);
  if (action === 'noop') return Promise.resolve();
  pageHidden = hiddenNow;
  if (action === 'pause') { stopPollTimer(); return Promise.resolve(); }
  return pollArmed ? pollTick() : Promise.resolve();
}
if (document && document.addEventListener) document.addEventListener('visibilitychange', handleVisibilityChange);

/* refreshVisible 保留原有视图分派语义（accounts / logs / taskscenter）；
   返回值只服务轮询失败退避：false = 本轮失败。 */
async function refreshVisible() {
  if (view === 'accounts') return refreshAccountsView(true);
  if (view === 'logs') return loadLogs();
  if (view === 'taskscenter') { reattachQueueView(); return true; }
  return true;
}
function start() {
  pollArmed = true;
  pollAuthStopped = false;
  keyPrompted = false;
  refFailures = 0;
  stopPollTimer();
  // 首查 = lite overview + 当前页 accounts（v2.1 #1）：统计条/导航/账号表各自就位，
  // poll_interval_sec 由 lite overview 更新。首次成功后按该值建立定时器；
  // 首查失败也要排一次重试，否则永远停在空白页。
  refreshAccountsView(true).then(ok => {
    if (ok === false) refFailures += 1;
    scheduleNextPoll();
  });
  // 完整 overview 只在页面加载时后台补一次（积分合计/到期快照/备注弹窗需要全量
  // accounts）；之后只有「积分到期」tab 会按 60s 新鲜窗口再拉，不再每 5s 全量。
  ensureFullOverview(true);
  checkAuthGate();
}
async function checkAuthGate() {
  // 只探鉴权：用 lite overview（1-2 KB），不拉全量账号。
  try { await api('overview?lite=1'); }
  catch (e) { if (String(e.message).includes('密钥') || String(e.message).includes('api_key')) return; }
}
start();

/* ── 积分任务 ─────────────────────────────────────────────────────── */
let taskUID = null;

// 可自动完成的任务（与后端 autoActions 表一致）：判据为行为事件、可经网关复现。
// 其余任务需在官方客户端内交互，面板只展示指引（行 title 提示）。
// 注意：键含点号（Model_chat_GLM5.2）必须加引号，否则会被解析成属性访问 + 数字字面量。
const AUTO_TASKS = {
  'chat_5': '上报 5 条对话活跃事件（自动补足差额）',
  'first_buddy': '上报解锁 → 同意协议 → 领取第一只 Buddy',
  'Model_chat_GLM5.2': '接受任务 → glm-5.2 真实对话一次 → 对齐模型上报',
  'RichMeow_Chat': '桌面指纹事件链上报（已验证可点亮）',
  'Buddy_App': '上报「进入 Buddy 应用」事件链（已验证可点亮）',
  'Buddy_App_QQ': '上报「进入企鹅教师助手」事件链（已验证可点亮）',
  'automation_1': '上报「定时任务创建」事件（已验证可点亮）',
  'Library_read': '上报「读资料库介绍」事件（已验证可点亮）',
  'template_5': '上报「使用模板创建任务」事件组 ×5（三账号实测点亮）',
  'playbook_prompt': '上报「灵感案例做同款发送 Prompt」事件组（三账号实测点亮）',
  'create_canvas': '上报「设计创意画布创建」事件组（三账号实测点亮，+300 分）',
  'expert_5': '真实专家召唤+使用链 ×5（专家市场+真实 chat，三账号实测点亮）',
  'Expert_team_use_3': '真实专家团召唤+使用链 ×3（三账号实测点亮）',
  'Hp_Appearance': '设置主题 API + 皮肤生效事件（两账号实测点亮）',
  'black_cat': '夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足（窗口外提示等 23 点排程）',
  'Expert_lighthouse': '真实轻量云专家召唤+使用链（真实对话 requestId，两账号实测点亮）',
  'skill_1': '真实对话 + skill_info 技能加载事件（实测点亮）',
  'school_season': '校园日（小程序口径）：accept → mini 对话+activityId 上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_1': '小程序首对话（小程序口径）：accept → mini 对话上报 → 领奖（+100c+5e）',
  'Sequential_Tasks_2': '小程序选专家对话（小程序口径）：市场专家 id → accept → expert_actual_use 上报 → 领奖（+200c+5e）',
  'Sequential_Tasks_3': '小程序五次对话（小程序口径）：accept → mini 对话上报 ×5（自动补差额）→ 领奖（+300c+5e）',
  'Sequential_Tasks_4': '小程序定时任务（预留，每日零点解锁一环）：accept → 定时任务创建事件 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_5': '小程序使用 GLM5.2（预留）：accept → 带模型字段的 mini 对话上报 → 领奖（判据待解锁验证）',
  'Sequential_Tasks_6': '小程序十次对话（预留）：accept → mini 对话上报 ×target（自动补差额）→ 领奖',
  'Sequential_Tasks_7': '体验灵感功能（预留，疑 PC 口径）：accept → 灵感事件组（PC+mp 双形态）→ 领奖（判据待解锁验证）'
};

function openTasks(uid) {
  taskUID = uid;
  $('taskWho').textContent = uid.slice(0, 16);
  $('taskVeil').classList.add('on');
  $('btnTaskReload').hidden = false;
  loadTasks();
}
function closeTasks() { $('taskVeil').classList.remove('on'); taskUID = null; }
$('btnCloseTask').onclick = closeTasks;
$('btnTaskReload').onclick = loadTasks;

// 全部接受：把该账号未接受的任务一次性报名（幂等，跳过已接受/已领取）。
$('btnTaskAcceptAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAcceptAll');
  btn.disabled = true; btn.textContent = '接受中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/accept_all', { method: 'POST' });
    const n = r.accepted || 0;
    if (r.failed && r.failed.length) {
      toast(`已接受 ${n} 个，${r.failed.length} 个被上游拒绝（可重试）`, 'err');
    } else {
      toast(n ? `已接受 ${n} 个任务` : (r.message || '所有任务均已接受'), 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '全部接受'; loadTasks(); }
};

// 一键完成全部可自动任务（耗时较长：含真实对话，逐项回读验证）。
$('btnTaskAutoAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAutoAll');
  if (!confirm('将依次执行：补报对话事件、领取 Buddy、glm-5.2 对话、尝试上报。\n过程约 1-2 分钟（含真实对话），确认继续？')) return;
  btn.disabled = true; btn.textContent = '执行中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto_all', { method: 'POST' });
    const okN = (r.results || []).filter(x => x.status === 'done').length;
    const skipN = (r.results || []).filter(x => x.status === 'skipped').length;
    const errN = (r.results || []).filter(x => x.status === 'error').length;
    toast(`执行完成：成功 ${okN} 项，跳过 ${skipN} 项${errN ? '，失败 ' + errN + ' 项' : ''}`, errN ? 'err' : 'ok');
    console.log('auto_all results:', r.results);
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '一键完成可自动任务'; loadTasks(); }
};

async function loadTasks() {
  if (!taskUID) return;
  const st = $('taskState'), tb = $('taskTable');
  st.hidden = false;
  st.className = 'state';
  st.innerHTML = '<span class="dots">查询中</span>';
  tb.hidden = true;
  try {
    const d = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks');
    const list = d.tasks || [];
    if (!list.length) {
      st.className = 'state';
      st.textContent = '该账号暂无任务';
      return;
    }
    // 有进度或可领取的排前面，已领取沉底——一眼看到"现在该做什么"。
    list.sort((a, b) => (a.claimed - b.claimed) || (b.claimable - a.claimable) || String(a.task_code).localeCompare(String(b.task_code)));
    $('taskBody').innerHTML = list.map(t => {
      // 进度：current 可能缺失（0 或被上游省略）——用 ?? 兜底，避免渲染成 "undefined / N"
      const cur = t.current ?? 0, tgt = t.target ?? 0;
      const prog = tgt ? cur + ' / ' + tgt : (tgt === 0 && cur > 0 ? String(cur) : '—');
      const parts = [];
      if (t.credit) parts.push('+' + t.credit + ' 分');
      if (t.energy) parts.push('+' + t.energy + ' 能');
      if (t.reward_buddy) parts.push('Buddy');
      const reward = parts.length ? parts.join(' ') : '—';
      const badge = t.claimed ? '<span class="tag ok">已领取</span>'
        : t.claimable ? '<span class="tag warn">可领取</span>'
        : t.locked ? '<span class="tag mute">未解锁</span>'
        : t.accept_status === 'accepted' ? '<span class="tag mute">进行中</span>'
        : '<span class="tag mute">未接受</span>';
      const acted = t.claimed || t.locked ? ''
        : t.claimable ? '<button class="xs primary" data-t="claim" data-c="' + esc(t.task_code) + '">领取</button>'
        : AUTO_TASKS[t.task_code] ? '<button class="xs primary" data-t="auto" data-c="' + esc(t.task_code) + '" title="' + esc(AUTO_TASKS[t.task_code]) + '">一键完成</button>'
        : t.accept_status === 'accepted' ? ''
        : '<button class="xs" data-t="accept" data-c="' + esc(t.task_code) + '">接受</button>';
      // 操作指引（description/task_desc）挂 title 提示：如何完成交给用户看
      const tip = [t.title, t.task_desc || t.description, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n');
      return '<tr title="' + esc(tip) + '"><td class="mark" aria-hidden="true"><i></i></td>' +
        '<td class="who"><div class="nm">' + esc(t.title || t.task_code) + '</div><div class="id">' + esc(t.task_code) + (t.tag ? ' · ' + esc(t.tag) : '') + '</div></td>' +
        '<td class="num">' + esc(prog) + '</td>' +
        '<td class="num">' + esc(reward) + '</td>' +
        '<td>' + badge + '</td>' +
        '<td class="acts">' + acted + '</td></tr>';
    }).join('');
    st.hidden = true;
    tb.hidden = false;
  } catch (e) {
    st.className = 'state err';
    st.textContent = e.message;
  }
}

$('taskBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-t]');
  if (!b || !taskUID) return;
  const kind = b.dataset.t, code = b.dataset.c;
  b.disabled = true;
  try {
    if (kind === 'auto') {
      // 一键完成：后端执行动作 → 回读进度 → 汇报（耗时可到分钟级，含真实对话）
      b.textContent = '执行中…';
      const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto', {
        method: 'POST', body: JSON.stringify({ task_code: code })
      });
      if (r.skipped) {
        toast(r.message || '已跳过', 'ok');
      } else {
        const advanced = r.progress_before !== r.progress_after;
        let msg = r.message || '已执行';
        if (r.progress_after) msg += `（进度 ${r.progress_before} → ${r.progress_after}）`;
        if (r.claimed) msg += '，奖励已自动到账';
        else if (r.claimable) msg += r.claim_error ? '，可点「领取」重试' : '';
        else if (r.attempt && !advanced) msg += '；进度未动，该任务可能需要官方客户端';
        toast(msg, (r.claimed || advanced) ? 'ok' : 'err');
      }
      refreshAccountPool(true);
    } else {
      const path = 'accounts/' + encodeURIComponent(taskUID) + '/tasks/' + (kind === 'claim' ? 'claim' : 'accept');
      const body = kind === 'claim' ? { task_code: code } : { task_codes: [code] };
      await api(path, { method: 'POST', body: JSON.stringify(body) });
      toast(kind === 'claim' ? '已领取奖励' : '已接受任务', 'ok');
      if (kind === 'claim') refreshAccountPool(true);
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { loadTasks(); }
});

/* ── 任务中心：开学季 + 全账号扫描/队列 ──────────────────────────── */
// 开学季任务单元：✓ 已领（绿）｜◐ x/y 进行中（琥珀）｜○ 未做（灰）
function staskHTML(t) {
  if (!t) return '<span class="stask todo"><span class="mark">·</span>—</span>';
  if (t.status === 'claimed') return '<span class="stask ok"><span class="mark">✓</span>已领</span>';
  if (t.status === 'completed') return '<span class="stask warn"><span class="mark">◆</span>可领</span>';
  if (t.status === 'in_progress') {
    const fr = t.target_count ? '<span class="fr">' + t.progress + '/' + t.target_count + '</span>' : '';
    return '<span class="stask warn"><span class="mark">◐</span>' + fr + '</span>';
  }
  return '<span class="stask todo"><span class="mark">○</span>未做</span>';
}
const LUCK_SVG = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4"><path d="M3.2 5.2 5 1.8l3 2.4 3-2.4 1.8 3.4-1.4 2.6 1.4 2.6-3.4 2.2H6l-3.4-2.2 1.4-2.6z" opacity=".9"/><circle cx="8" cy="9" r="1.1" fill="currentColor" stroke="none"/></svg>';
const SCHOOL_TITLES = {
  share_invite: '分享活动 +100c', desktop_chat_1_time: '桌面端体验 +100c（单次）',
  chat_3_times: '和 AI 对话 3 次 +50c', expert_use: '召唤开学季专家 +50c',
  task_student_verify: '学生认证 +100c（需真实认证，不做）',
};

/* ── 精简 QR 编码器（券码二维码用）────────────────────────────────────
   规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
   完整性：规范允许任选掩码（解码器按格式信息位自行去掩码），固定掩码不影响
   可扫描性；已用 python qrcode 库对多输入多版本做逐像素交叉验证（强制 byte
   模式 + mask 0，5/5 全部 diff=0）。面板 CSP 只允许 self，外链 QR 服务不可用。 */
// qr_gen.js —— 精简 QR 编码器（浏览器用 + node 可跑交叉验证）
// 规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
// 完整性说明：规范允许编码器任选掩码（解码器按格式信息位自行去掩码），
// 固定掩码不影响可扫描性；券码为短文本，v1-5（26 字节起）绰绰有余。

// GF(256) 对数/指数表（本原多项式 0x11d）
const QR_EXP = new Array(512), QR_LOG = new Array(256);
(() => {
  let x = 1;
  for (let i = 0; i < 255; i++) { QR_EXP[i] = x; QR_LOG[x] = i; x <<= 1; if (x & 0x100) x ^= 0x11d; }
  for (let i = 255; i < 512; i++) QR_EXP[i] = QR_EXP[i - 255];
})();
const gmul = (a, b) => (a && b) ? QR_EXP[QR_LOG[a] + QR_LOG[b]] : 0;

// 各版本参数（下标 = 版本-1）：[数据码字数, 纠错码字数]，ECC L 单块
const QR_V = [[19, 7], [34, 10], [55, 15], [80, 20], [108, 26]];
// 对齐图案中心坐标（v2+；与定位图案重叠的位置在放置时跳过）
const QR_ALIGN = [[], [6, 18], [6, 22], [6, 26], [6, 30]];
const QR_MASK = (r, c) => (r + c) % 2 === 0; // 掩码模式 0

// 生成多项式（最高次系数在前，g[0] 恒为 1）
function qrGenPoly(deg) {
  let g = [1];
  for (let i = 0; i < deg; i++) {
    const a = QR_EXP[i], ng = new Array(g.length + 1).fill(0);
    ng[0] = g[0];
    for (let j = 1; j < g.length; j++) ng[j] = g[j] ^ gmul(a, g[j - 1]);
    ng[g.length] = gmul(a, g[g.length - 1]);
    g = ng;
  }
  return g;
}

// Reed-Solomon 求余（综合除法），返回 deg 个纠错码字
function rsRem(data, deg) {
  const g = qrGenPoly(deg);
  const res = data.concat(new Array(deg).fill(0));
  for (let i = 0; i < data.length; i++) {
    const f = res[i];
    if (f) for (let j = 0; j < g.length; j++) res[i + j] ^= gmul(g[j], f);
  }
  return res.slice(data.length);
}

// 文本 → 码字流（byte 模式：0100 + 8 位计数 + 数据 + 终止符 + 0xEC/0x11 填充）
function qrDataCodewords(text, dataCap) {
  const bytes = Array.from(new TextEncoder().encode(text));
  const bits = [];
  const push = (val, n) => { for (let i = n - 1; i >= 0; i--) bits.push((val >> i) & 1); };
  push(4, 4);            // byte 模式
  push(bytes.length, 8); // v1-9 计数 8 位
  for (const b of bytes) push(b, 8);
  const cap = dataCap * 8;
  push(0, Math.min(4, cap - bits.length));   // 终止符
  while (bits.length % 8) bits.push(0);
  const out = [];
  for (let i = 0; i < bits.length; i += 8) {
    let v = 0; for (const b of bits.slice(i, i + 8)) v = (v << 1) | b;
    out.push(v);
  }
  for (let p = 0; out.length < dataCap; p ^= 1) out.push(p ? 0x11 : 0xEC);
  return out;
}

// 主入口：text → 布尔矩阵（true=深色模块）
function qrMatrix(text) {
  const bytes = Array.from(new TextEncoder().encode(text));
  // 版本选择：需求 ≈ 2 码字头 + 文本长度，取首个放得下的版本
  let ver = 0;
  for (let v = 0; v < QR_V.length; v++) { if (bytes.length + 2 <= QR_V[v][0]) { ver = v + 1; break; } }
  if (!ver) throw new Error('QR: text too long (>' + QR_V[4][0] + ' bytes)');
  const [dataCap, ecCap] = QR_V[ver - 1];
  const n = 17 + 4 * ver;

  const M = Array.from({ length: n }, () => new Array(n).fill(false));
  const F = Array.from({ length: n }, () => new Array(n).fill(false)); // 功能模块占位

  const setF = (r, c, v) => { M[r][c] = v; F[r][c] = true; };
  // 定位图案 + 分隔带
  const finder = (r0, c0) => {
    for (let r = -1; r <= 7; r++) for (let c = -1; c <= 7; c++) {
      const rr = r0 + r, cc = c0 + c;
      if (rr < 0 || cc < 0 || rr >= n || cc >= n) continue;
      const dark = r >= 0 && r <= 6 && c >= 0 && c <= 6 && (r === 0 || r === 6 || c === 0 || c === 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4));
      setF(rr, cc, dark);
    }
  };
  finder(0, 0); finder(0, n - 7); finder(n - 7, 0);
  // 校正图形（仅贯穿两定位图案之间：8..n-9，不得覆盖定位图案本体）
  for (let r = 8; r <= n - 9; r++) setF(r, 6, r % 2 === 0);
  for (let c = 8; c <= n - 9; c++) setF(6, c, c % 2 === 0);
  // 对齐图案（v2+，跳过与定位重叠处）
  const align = QR_ALIGN[ver - 1] || [];
  for (const ar of align) for (const ac of align) {
    if (F[ar][ac]) continue;
    for (let r = -2; r <= 2; r++) for (let c = -2; c <= 2; c++)
      setF(ar + r, ac + c, Math.max(Math.abs(r), Math.abs(c)) !== 1);
  }
  // 暗模块 + 格式信息（ECC L=01，掩码 0）——BCH(15,5) + 0x5412 异或。
  // 位序遵循规范（与 python qrcode 逐位对齐验证）：bit i 从 LSB 起数，
  // 副本一走左上角 L 形、副本二走右下 L 形。
  let fmt = (1 << 3) | 0; // L<<3 | mask
  let rem = fmt << 10;
  for (let i = 14; i >= 10; i--) if ((rem >> i) & 1) rem ^= 0x537 << (i - 10);
  fmt = ((fmt << 10) | rem) ^ 0x5412; // 15 位
  const fb = i => (fmt >> i) & 1;
  // 副本一（左上）：位 0..5 → (i,8)；6 → (7,8)；7 → (8,8)
  for (let i = 0; i <= 5; i++) setF(i, 8, !!fb(i));
  setF(7, 8, !!fb(6)); setF(8, 8, !!fb(7));
  // 副本一续 + 副本二（右下）：位 8..14 → (n-15+i, 8)；位 0..7 → (8, n-1-i)；8 → (8,7)；9..14 → (8,14-i)
  for (let i = 8; i <= 14; i++) setF(n - 15 + i, 8, !!fb(i));
  for (let i = 0; i <= 7; i++) setF(8, n - 1 - i, !!fb(i));
  setF(8, 7, !!fb(8));
  for (let i = 9; i <= 14; i++) setF(8, 14 - i, !!fb(i));
  // 暗模块（恒为深色，位于副本一垂直段末端）
  setF(n - 8, 8, true);


  // 数据码字 + 纠错码字 → 位流
  const dcw = qrDataCodewords(text, dataCap);
  const cw = dcw.concat(rsRem(dcw, ecCap));
  const bits = [];
  for (const b of cw) for (let i = 7; i >= 0; i--) bits.push((b >> i) & 1);

  // 蛇形放置（成对列，从右向左，跳过第 6 列），写数据时直接异或掩码
  let bi = 0, up = true;
  for (let x = n - 1; x > 0; x -= 2) {
    if (x === 6) x--;
    for (let i = 0; i < n; i++) {
      const r = up ? n - 1 - i : i;
      for (const c of [x, x - 1]) {
        if (F[r][c]) continue;
        const bit = bi < bits.length ? bits[bi++] : 0;
        M[r][c] = bit ? !QR_MASK(r, c) : QR_MASK(r, c);
      }
    }
    up = !up;
  }
  return M;
}

// 矩阵 → SVG（quiet zone 4 模块）
function qrSVG(M, px) {
  const n = M.length, q = 4, total = n + q * 2;
  let s = '<svg viewBox="0 0 ' + total + ' ' + total + '" width="' + px + '" height="' + px + '" shape-rendering="crispEdges" role="img" style="background:#fff">';
  for (let r = 0; r < n; r++) for (let c = 0; c < n; c++)
    if (M[r][c]) s += '<rect x="' + (c + q) + '" y="' + (r + q) + '" width="1" height="1"/>';
  return s + '</svg>';
}

/* ── 开学季券码查询（弹窗，仿活动页 #/prizes?tab=vouchers）──────────── */
/* copyText：clipboard API 只在 secure context（https/localhost）可用，
   远程 http 面板会拿不到 navigator.clipboard → 降级 execCommand。 */
function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy') ? resolve() : reject(new Error('copy failed')); }
    catch (e) { reject(e); }
    finally { ta.remove(); }
  });
}

function vcCard(v) {
  const expired = v.valid_to && new Date(v.valid_to) < new Date();
  return '<div class="vc' + (expired ? ' expired' : '') + '">' +
    '<div class="hd"><span class="nm">' + esc(v.prize_name || v.sku_code || '券') + '</span>' +
    (expired ? '<span class="tag bad">已过期</span>' : '<span class="tag ok">可使用</span>') + '</div>' +
    '<div class="meta">' +
      (v.valid_to ? '有效期至 ' + esc(v.valid_to) : '长期有效') +
      (v.granted_at ? ' · ' + esc(v.granted_at.slice(0, 10)) + ' 抽中' : '') +
    '</div>' +
    '<div class="sep"></div>' +
    '<div class="ft"><span class="lab">券码</span><code>' + esc(v.code || '-') + '</code>' +
    '<span class="acts">' +
      (v.code ? '<button class="xs ghost" data-qr="' + esc(v.code) + '">二维码</button>' : '') +
      '<button class="xs ghost" data-copy="' + esc(v.code || '') + '">复制</button>' +
    '</span></div>' +
    '</div>';
}

async function loadSchoolVouchers() {
  const body = $('vcBody');
  $('vcVeil').classList.add('on');
  body.innerHTML = '<div class="state"><span class="dots">查询中</span></div>';
  $('vcNote').textContent = '';
  try {
    const d = await api('school/vouchers');
    const arr = d.accounts || [];
    const ok = arr.filter(a => !a.error);
    const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
    body.innerHTML = ok.filter(a => (a.vouchers || []).length).map(a =>
      '<div class="vc-acct"><span class="nm">' + esc(a.nickname || a.uid) + '</span>' +
      '<span>' + a.vouchers.length + ' 张</span></div>' +
      a.vouchers.map(vcCard).join('')
    ).join('') || '<div class="empty"><div class="big">🎟️</div>还没有抽到券</div>';
    $('vcNote').textContent = total ? total + ' 张券 · ' + ok.filter(a => !(a.vouchers || []).length).length + ' 个账号未抽中' : '';
    const errs = arr.filter(a => a.error);
    if (errs.length) {
      body.insertAdjacentHTML('beforeend', '<div class="note" style="color:var(--warn);margin-top:8px">查询失败：' +
        errs.map(a => esc(a.nickname || a.uid.slice(0, 8)) + '（' + esc(a.error) + '）').join('、') + '</div>');
    }
    body.querySelectorAll('button[data-copy]').forEach(b => b.onclick = async () => {
      try { await copyText(b.dataset.copy); toast('券码已复制', 'ok'); }
      catch (e) { toast('复制失败，请手动选择券码', 'err'); }
    });
    // 二维码：券码本体编码为 QR（到店出示扫描），点击切换显示/隐藏
    body.querySelectorAll('button[data-qr]').forEach(b => b.onclick = () => {
      const card = b.closest('.vc');
      const old = card.querySelector('.vc-qr');
      if (old) { old.remove(); return; }
      const box = document.createElement('div');
      box.className = 'vc-qr';
      try { box.innerHTML = qrSVG(qrMatrix(b.dataset.qr), 148); }
      catch (e) { box.innerHTML = '<span class="note">二维码生成失败：' + esc(e.message) + '</span>'; }
      card.appendChild(box);
    });
  } catch (e) {
    body.innerHTML = '<div class="state err">' + esc(e.message) + '</div>';
  }
}
$('btnSchoolVouchers').onclick = loadSchoolVouchers;
$('btnVcClose').onclick = () => $('vcVeil').classList.remove('on');
$('btnVcRefresh').onclick = loadSchoolVouchers;

/* 成长任务队列。lastQueueSeq 记录本页启动过的队列代次：执行结束后的残留 items
   （running=false 但 seq 停在旧值）不再回写视图——否则扫描结果 3 秒后被上一轮
   队列状态覆盖。 */
let queueTimer = null, lastQueueSeq = 0;
const GROWTH_TITLES = {}; // code → 展示名（扫描时从任务列表带出）

/* ── 任务中心队列：筛选（状态 / 账号关键词）+ 客户端分页 ─────────────────
   1146 个账号的扫描结果可能有数千行，一次全量拼 DOM 会卡住主线程：这里按行
   分页，每页只渲染当前页。筛选/页码是模块状态，3s 队列轮询重渲染时保持；
   新数据源（重新扫描 / 启动队列）由调用方把 queuePage 复位到第 1 页。 */
let queueAll = [];              // 最近一次渲染的分组数据源
let queueFilters = { q: '', state: '' };
let queuePage = 1;
let queuePageSize = 100;        // 0 = 全部
let queueProgress = null;       // 队列进度（轮询时传入，筛选/翻页重渲染时复用）
let queueEmptyTitle = '', queueEmptyDesc = '';

// queueFlatten 分组 → 扁平行（行上带账号上下文，才能跨组按行筛选/分页）。
function queueFlatten(groups) {
  const out = [];
  for (const g of groups || []) {
    for (const row of (g && g.rows) || []) out.push({ uid: g.uid, nick: g.nick, row: row });
  }
  return out;
}

// queueMatchRow 单行筛选：state 精确匹配；q 对账号昵称 / uid / 任务代码 / 标题做
// 大小写不敏感包含匹配（空格分词 AND，与请求记录筛选用同一口径）。
function queueMatchRow(item, f) {
  f = f || queueFilters;
  if (f.state && String((item.row && item.row.status) || '') !== f.state) return false;
  if (f.q) {
    const title = (item.row && item.row.kind === 'school')
      ? '开学季闭环' : (GROWTH_TITLES[item.row && item.row.code] || '');
    const hay = [item.nick, item.uid, item.row && item.row.code, title].filter(Boolean).join(' ').toLowerCase();
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (hay.indexOf(kw) < 0) return false;
    }
  }
  return true;
}

// filterQueueRows 分组 → 过滤后的扁平行（纯函数，node harness 可直接钉计数）。
function filterQueueRows(groups, f) {
  return queueFlatten(groups).filter(item => queueMatchRow(item, f));
}

// queueRegroup 扁平行 → 渲染用分组：保持原账号出现顺序；分页后同账号仍归一组，
// 组头「N 项待办」跟随当前页行数（不是全量）。
function queueRegroup(items) {
  const groups = [], by = new Map();
  for (const item of items || []) {
    let g = by.get(item.uid);
    if (!g) { g = { uid: item.uid, nick: item.nick, rows: [] }; by.set(item.uid, g); groups.push(g); }
    g.rows.push(item.row);
  }
  return groups;
}

let queueQTimer = null;
if ($('tcQ')) $('tcQ').addEventListener('input', () => {
  clearTimeout(queueQTimer);
  queueQTimer = setTimeout(() => { queueFilters.q = $('tcQ').value.trim(); queuePage = 1; renderQueueView(); }, 150);
});
if ($('tcState')) $('tcState').addEventListener('change', () => {
  queueFilters.state = $('tcState').value; queuePage = 1; renderQueueView();
});
if ($('tcPageSize')) $('tcPageSize').addEventListener('change', () => {
  queuePageSize = parseInt($('tcPageSize').value, 10) || 0;
  queuePage = 1;
  renderQueueView();
});
if ($('tcPrev')) $('tcPrev').onclick = () => { if (queuePage > 1) { queuePage--; renderQueueView(); } };
if ($('tcNext')) $('tcNext').onclick = () => {
  const pages = pageSlice(filterQueueRows(queueAll, queueFilters), queuePageSize, queuePage).pages;
  if (queuePage < pages) { queuePage++; renderQueueView(); }
};

$('btnScanAll').onclick = async () => {
  const b = $('btnScanAll');
  // 停掉队列轮询：显式扫描 = 切到待办视图。否则在途队列的下一 tick 会把扫描
  // 结果冲掉重渲染回队列视图（服务端执行不受影响，只是不再实时回写本视图）。
  if (queueTimer) { clearInterval(queueTimer); queueTimer = null; }
  b.disabled = true; b.textContent = '扫描中…';
  try {
    const d = await api('tasks/scan_all', { method: 'POST' });
    queuePage = 1;   // 新扫描结果 = 新数据源，回到第 1 页
    renderQueue(groupItems(d), null, '没有待办任务 🎉', '全部账号的成长任务与开学季活动都已完成，明日再来。');
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '扫描待办'; }
};
$('btnRunQueue').onclick = async () => {
  const conc = Number($('qcConc').value) || 1;
  if (!confirm('扫描全部账号待办并排队执行（账号并发 ' + conc + '，账号内串行）。\n含真实对话的任务耗时较长，确认继续？')) return;
  const b = $('btnRunQueue');
  b.disabled = true; b.textContent = '启动中…';
  try {
    const r = await api('tasks/run_queue', { method: 'POST', body: JSON.stringify({ concurrency: conc }) });
    if (!r.started) { toast(r.message || '没有待办任务', 'ok'); return; }
    lastQueueSeq = r.seq || 0;
    queuePage = 1;   // 新一轮队列 = 新数据源
    toast('队列已启动：' + r.total + ' 项（并发 ' + conc + '）', 'ok');
    startQueuePolling();
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '执行全部待办'; }
};
// 扫描结果 → 分组条目（无执行状态）
function groupItems(d) {
  const groups = [];
  for (const a of (d.accounts || [])) {
    const rows = [];
    for (const t of (a.growth || [])) {
      GROWTH_TITLES[t.task_code] = t.title || t.task_code;
      rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? t.current + '/' + t.target : '—', status: 'scan' });
    }
    if (rows.length) groups.push({ uid: a.uid, nick: a.nickname, rows });
  }
  return groups;
}
const ST_WORDS = { done: '完成', running: '执行中', error: '失败', skipped: '跳过', pending: '排队', scan: '待执行' };
function qrowHTML(it) {
  const isSchool = it.kind === 'school';
  const title = isSchool ? '开学季闭环' : (GROWTH_TITLES[it.code] || it.code);
  const dotCls = it.status === 'scan' ? 'wait' : it.status === 'running' ? 'run' : it.status === 'error' ? 'err' : it.status === 'skipped' ? 'skip' : it.status === 'done' ? 'done' : 'wait';
  const stWord = it.status === 'scan' ? '待执行' : (ST_WORDS[it.status] || it.status);
  return '<div class="qrow" title="' + esc(it.message || '') + '">' +
    '<span class="code">' + esc(it.code) + '</span>' +
    '<span class="name"><span class="t">' + esc(title) + '</span>' + (isSchool ? '<span class="tag mute">开学季</span>' : '') + '</span>' +
    '<span class="prog">' + esc(it.prog || '') + '</span>' +
    '<span class="st"><span class="qdot ' + dotCls + '"></span>' + stWord + '</span>' +
    '<span class="msg">' + esc(it.message || '') + '</span>' +
    '</div>';
}
// renderQueue 队列渲染入口：只更新数据源/进度，实际渲染交给 renderQueueView
// （筛选/翻页重渲染时不需要重新传数据，签名保持与既有调用方一致）。
function renderQueue(groups, progress, emptyTitle, emptyDesc) {
  queueAll = groups || [];
  queueProgress = progress === undefined ? null : progress;
  if (emptyTitle != null) queueEmptyTitle = emptyTitle;
  if (emptyDesc != null) queueEmptyDesc = emptyDesc;
  renderQueueView();
}

function renderQueueView() {
  const empty = $('tcEmpty'), list = $('qcList');
  if (!empty || !list) return;
  const allRows = queueFlatten(queueAll).length;
  const filtered = filterQueueRows(queueAll, queueFilters);
  const pr = pageSlice(filtered, queuePageSize, queuePage);
  queuePage = pr.page;
  updatePagerUI({ count: 'qcCount', info: 'tcPageInfo', prev: 'tcPrev', next: 'tcNext' }, pr,
    hitCountLabel(filtered.length, allRows, '项'));
  $('qcSummary').textContent = hitCountLabel(filtered.length, allRows, '项');
  if (!queueAll.length) {
    empty.style.display = '';
    if (queueEmptyTitle) empty.querySelector('.t').textContent = queueEmptyTitle;
    if (queueEmptyDesc) empty.querySelector('.d').textContent = queueEmptyDesc;
    list.innerHTML = '';
    $('qProg').hidden = true;
    return;
  }
  if (!filtered.length) {
    empty.style.display = '';
    empty.querySelector('.t').textContent = '没有符合筛选条件的待办';
    empty.querySelector('.d').textContent = '试试清空关键词，或把状态筛回「全部状态」。';
    list.innerHTML = '';
    updateProgress(queueProgress);
    return;
  }
  empty.style.display = 'none';
  const groups = queueRegroup(pr.rows);
  list.innerHTML = groups.map(g => {
    return '<div class="qgroup"><header><span class="nm">' + esc(g.nick || String(g.uid).slice(0, 12)) +
      '</span><span class="cnt">' + g.rows.length + ' 项待办</span></header>' +
      g.rows.map(qrowHTML).join('') + '</div>';
  }).join('');
  updateProgress(queueProgress);
}
function updateProgress(q) {
  if (!q || !q.items) { $('qProg').hidden = true; return; }
  const total = q.items.length;
  const done = q.items.filter(it => it.status === 'done' || it.status === 'error' || it.status === 'skipped').length;
  $('qProg').hidden = false;
  $('qBarFill').style.width = (total ? Math.round(done / total * 100) : 0) + '%';
  $('qProgText').textContent = (q.running ? '执行中 ' : '已结束 ') + done + ' / ' + total;
}
// 队列状态 → 分组（执行时轮询）
function groupsFromQueue(items) {
  const by = new Map();
  for (const it of items) {
    if (!by.has(it.uid)) by.set(it.uid, { uid: it.uid, nick: it.nickname, rows: [] });
    by.get(it.uid).rows.push({
      kind: it.kind, code: it.code,
      prog: it.kind === 'school' ? '—' : '',
      status: it.status, message: it.message,
    });
  }
  return Array.from(by.values());
}
function startQueuePolling() {
  if (queueTimer) clearInterval(queueTimer);
  queueTimer = setInterval(async () => {
    let q;
    try { q = await api('tasks/queue'); } catch (e) { return; }
    if (!q.started) return;
    // 只渲染本页启动过的那轮队列（刷新页面后不再接管旧队列）。
    if (lastQueueSeq && q.seq !== lastQueueSeq) return;
    if (q.running) {
      renderQueue(groupsFromQueue(q.items || []), q);
      return;
    }
    // 结束：终态只渲染这一次，随即停表。此后残留的 items（running=false）不再
    // 回写视图——曾把用户刚点开的「扫描待办」结果在下一个 tick 冲掉。
    renderQueue(groupsFromQueue(q.items || []), q);
    clearInterval(queueTimer); queueTimer = null;
    toast('任务队列执行结束', 'ok');
  }, 3000);
}
// reattachQueueView 切回任务中心视图时恢复队列进度：仅当本页启动的队列仍在
// 执行才重新开轮询（残留态/别页队列不接管——视图不被旧结果冲掉）。
function reattachQueueView() {
  // 全程异步：go() 在顶层（app.js ~143 行）被调用时，本文件下方 let/const
  //（queueTimer/lastQueueSeq 等）尚未初始化——同步读取即 TDZ ReferenceError
  // 使整个脚本中断。await 之后才碰它们（旧 pollQueueOnce 正是靠开头的 await
  // 侥幸安全）。queueTimer 的"已在跑"判定也挪到 await 后，语义不变。
  (async () => {
    try {
      const q = await api('tasks/queue');
      if (queueTimer) return; // 轮询已在跑（跨视图不中断）
      if (q.started && q.running && (!lastQueueSeq || q.seq === lastQueueSeq)) startQueuePolling();
    } catch (e) { /* 静默 */ }
  })();
}

/* ── 用量 ─────────────────────────────────────────────────────────── */
/* 图表用原生 SVG 手绘：面板是 go:embed 单文件、无构建步骤，引入图表库
   就得带上打包器，得不偿失。这里只需要堆叠柱状图，二十行足够。 */

function fmtTok(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}
function fmtMs(ms) {
  ms = Number(ms || 0);
  if (!ms) return '—';
  if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
  return Math.round(ms) + 'ms';
}
function fmtRate(r) { return r ? Number(r).toFixed(1) + ' tok/s' : '—'; }
function trimFixed(s) {
  if (!String(s).includes('.')) return String(s);
  return String(s).replace(/0+$/, '').replace(/\.$/, '');
}
function fmtCredit(n) {
  const v = Number(n || 0);
  if (!Number.isFinite(v)) return '—';
  return trimFixed(v.toFixed(2));
}
function fmtCreditRatio(v, samples, tokens) {
  if (!samples || !tokens) return '—';
  const n = Number(v || 0);
  if (!Number.isFinite(n)) return '—';
  return trimFixed(n.toFixed(4)) + ' / 1M';
}
function fmtModelRate(rate) {
  const s = String(rate || '').trim();
  return s ? 'x' + s : '—';
}

function usStat(v, k, cls) {
  return '<div class="stat ' + (cls || '') + '"><div class="v">' + esc(v) +
         '</div><div class="k">' + esc(k) + '</div></div>';
}

/* usKpi 用量页的指标卡。比账号池的 .stat 多两样：语义色轨（cls）与副标题（sub，
   放"占比 / 均速率"这类解释性数字）；bar 是卡片内的构成条 HTML，只有需要时才传。 */
function usKpi(v, k, cls, sub, bar) {
  return '<div class="kpi ' + (cls || '') + '">' +
    '<div class="k">' + esc(k) + '</div>' +
    '<div class="v">' + esc(v) + '</div>' +
    (bar || '') +
    (sub ? '<div class="s">' + esc(sub) + '</div>' : '') +
    '</div>';
}

/* usMixBar prompt/completion 占比条。宽度按百分比而不是固定像素：表列宽随窗口变化，
   像素宽度在窄屏会溢出、宽屏又显得没信息。 */
function usMixBar(prompt, completion, total) {
  const t = Number(total || 0);
  if (!t) return '';
  const pp = Math.max(0, Math.min(100, Number(prompt || 0) / t * 100));
  const pc = Math.max(0, Math.min(100, Number(completion || 0) / t * 100));
  return '<span class="us-mix" title="prompt ' + pp.toFixed(1) + '% · completion ' + pc.toFixed(1) + '%">' +
    '<i class="p" style="width:' + pp.toFixed(2) + '%"></i>' +
    '<i class="c" style="width:' + pc.toFixed(2) + '%"></i>' +
    '</span>';
}

/* usPct 占比文案（0 值不显示 "0.0%"，直接 —，避免一行全是零）。 */
function usPct(part, total) {
  const t = Number(total || 0);
  if (!t) return '—';
  return (Number(part || 0) / t * 100).toFixed(1) + '%';
}

/* usRow 生成一行。mid 是插在「名称」之后、请求数之前的额外单元格（如「域」列）。
   withPerf 控制延迟/速率两列；列开关显式传入，避免调用方改动后与表头错列。 */
/* nameHtml 非空时替代 esc(name)（已转义的富文本，见 remarkName）：账号维度的第一列
   需要把备注渲染成主标识、uid8 作为浅色虚体跟随，其余维度仍走纯文本转义。 */
function usRow(name, sub, a, mid, withPerf, nameHtml) {
  return '<tr>' +
    '<td class="mark" aria-hidden="true"></td>' +
    '<td>' + (nameHtml || esc(name)) + (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
    (mid || '') +
    '<td class="num">' + fmtTok(a.requests) + '</td>' +
    '<td class="num">' + (a.errors ? '<span style="color:var(--warn)">' + fmtTok(a.errors) + '</span>' : '—') + '</td>' +
    '<td class="num">' + fmtTok(a.prompt_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.completion_tokens) + '</td>' +
    '<td class="num">' + fmtTok(a.total_tokens) +
      usMixBar(a.prompt_tokens, a.completion_tokens, a.total_tokens) + '</td>' +
    (withPerf
      ? '<td class="num">' + fmtMs(a.avg_latency_ms) + '</td>' +
        '<td class="num">' + fmtRate(a.avg_tokens_per_second) + '</td>'
      : '') +
    '</tr>';
}

/* ── 用量明细：三个维度共用一张表 + 页内切换 ──────────────────────────
   账号 / 模型 / 域三张表此前各自占一个 box，页面纵向拉得很长且表头结构几乎一样。
   现在合成一个 box：表头由维度定义生成，行渲染复用 usRow，切换零请求。 */
const US_DIMS = {
  account: { key: 'by_account', title: '账号', withRealm: true, withPerf: true, span: 10 },
  model: { key: 'by_model', title: '模型', withRealm: false, withPerf: false, span: 7 },
  realm: { key: 'by_realm', title: 'realm', withRealm: false, withPerf: false, span: 7 },
};

// usSortRows 按当前排序字段降序（默认合计 Token，最大者最相关）。
function usSortRows(rows, sort) {
  const out = rows.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  const val = a => sort === 'requests' ? num(a.requests)
    : sort === 'errors' ? num(a.errors)
    : sort === 'latency' ? num(a.avg_latency_ms)
    : num(a.total_tokens);
  out.sort((a, b) => val(b) - val(a));
  return out;
}

function usDimHead(dim) {
  const m = US_DIMS[dim] || US_DIMS.account;
  return '<tr><th class="mark" aria-hidden="true"></th><th>' + esc(m.title) + '</th>' +
    (m.withRealm ? '<th>域</th>' : '') +
    '<th class="num">请求</th><th class="num">失败</th>' +
    '<th class="num">Prompt</th><th class="num">Completion</th><th class="num">合计</th>' +
    (m.withPerf ? '<th class="num">均延迟</th><th class="num">均速率</th>' : '') +
    '</tr>';
}

/* usTabsHtml 维度切换按钮（带条数徽标）。整段 innerHTML 重写而不是逐个改 class：
   容器的 click 监听是委托式的，换掉子节点不会丢事件，代码也更短。 */
function usTabsHtml(dims, active, counts) {
  return dims.map(([k, label]) => {
    const n = counts ? counts[k] : null;
    return '<button data-dim="' + k + '"' + (k === active ? ' class="on"' : '') + '>' +
      esc(label) + (n == null ? '' : '<span class="cnt">' + n + '</span>') + '</button>';
  }).join('');
}

const US_DIM_TABS = [['account', '按账号'], ['model', '按模型'], ['realm', '按域']];

function renderUsageDim() {
  const d = usageData || {};
  const m = US_DIMS[usDim] || US_DIMS.account;
  const rows = usSortRows(d[m.key] || [], usSort);
  $('usDimTabs').innerHTML = usTabsHtml(US_DIM_TABS, usDim, {
    account: (d.by_account || []).length,
    model: (d.by_model || []).length,
    realm: (d.by_realm || []).length,
  });
  $('usDimHead').innerHTML = usDimHead(usDim);
  $('usDimBody').innerHTML = rows.map(x => {
    const isAcct = usDim === 'account';
    const key = isAcct ? String(x.key || '').slice(0, 8) : x.key;
    return usRow(key,
      isAcct ? (x.extra || '') : '',
      x,
      isAcct ? '<td class="num">' + esc(x.realm || '') + '</td>' : '',
      m.withPerf,
      isAcct ? remarkName(x.key, esc(key)) : '');
  }).join('') || '<tr><td colspan="' + m.span + '" class="empty">暂无数据</td></tr>';
  $('usDimNote').textContent = rows.length + ' 行 · 请求数含失败尝试';
}

/* 积分扣除：按账号 / 按模型两个维度共用一张表（同上，两个 box 合成一个）。 */
function usCreditHead(dim) {
  return dim === 'model'
    ? '<tr><th class="mark" aria-hidden="true"></th><th>模型</th><th>积分倍率</th>' +
      '<th class="num">请求</th><th class="num">扣除积分</th>' +
      '<th class="num">有效样本 Token</th><th class="num">积分 / 1M Token</th><th class="num">缓存命中率</th></tr>'
    : '<tr><th class="mark" aria-hidden="true"></th><th>账号</th>' +
      '<th class="num">请求</th><th class="num">扣除积分</th>' +
      '<th class="num">有效样本 Token</th><th class="num">积分 / 1M Token</th><th class="num">缓存命中率</th></tr>';
}

/* 缓存命中率（issue #92）：颜色即健康度——≥90% 绿 / 80–90% 黄 / <80% 红，
   样本不足灰。title 带命中/未命中绝对量，供逐项核对。 */
function cacheRateCell(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), total = h + m;
  if (!total) return '<span class="muted">—</span>';
  const pct = h / total * 100;
  const color = pct >= 90 ? 'var(--ok)' : (pct >= 80 ? 'var(--warn)' : 'var(--bad)');
  const txt = trimFixed(pct.toFixed(1)) + '%';
  return '<span style="color:' + color + '" title="命中 ' + fmtTok(h) + ' / 未命中 ' + fmtTok(m) + ' tok">' + txt + '</span>';
}

function renderCreditDim() {
  const d = usageData || {};
  $('usCreditHead').innerHTML = usCreditHead(usCreditDim);
  const empty = '暂无积分扣除记录；升级前仅含 Token 的历史不会伪造积分。';
  if (usCreditDim === 'model') {
    const models = d.credit_by_model || [];
    $('usCreditBody').innerHTML = models.map(row =>
      '<tr>' +
        '<td class="mark" aria-hidden="true"></td>' +
        '<td>' + esc(row.key || '—') + '</td>' +
        '<td>' + esc(fmtModelRate(row.rate)) + '</td>' +
        '<td class="num">' + fmtTok(row.requests) + '</td>' +
        '<td class="num">' + fmtCredit(row.credits) + '</td>' +
        '<td class="num">' + fmtTok(row.credit_tokens) + '</td>' +
        '<td class="num">' + fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens) + '</td>' +
        '<td class="num">' + cacheRateCell(row.cache_hit_tokens, row.cache_miss_tokens) + '</td>' +
      '</tr>'
    ).join('') || '<tr><td colspan="8" class="empty">' + empty + '</td></tr>';
  } else {
    const accounts = d.credit_by_account || [];
    $('usCreditBody').innerHTML = accounts.map(row => {
      const uid = String(row.key || '');
      const account = row.nickname || uid.slice(0, 8) || '—';
      return '<tr>' +
        '<td class="mark" aria-hidden="true"></td>' +
        '<td>' + remarkName(uid, esc(account)) + '<div class="note">' + esc(row.realm || '') + ' · ' + esc(uid.slice(0, 8)) + '</div></td>' +
        '<td class="num">' + fmtTok(row.requests) + '</td>' +
        '<td class="num">' + fmtCredit(row.credits) + '</td>' +
        '<td class="num">' + fmtTok(row.credit_tokens) + '</td>' +
        '<td class="num">' + fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens) + '</td>' +
        '<td class="num">' + cacheRateCell(row.cache_hit_tokens, row.cache_miss_tokens) + '</td>' +
        '</tr>';
    }).join('') || '<tr><td colspan="7" class="empty">' + empty + '</td></tr>';
  }
  $('usCreditTabs').innerHTML = usTabsHtml([['account', '按账号'], ['model', '按模型']], usCreditDim, {
    account: (d.credit_by_account || []).length,
    model: (d.credit_by_model || []).length,
  });
}

function renderUsage(d) {
  usageData = d || {};
  const t = usageData.totals || {};
  const total = Number(t.total_tokens || 0);
  const pt = Number(t.prompt_tokens || 0);
  const ct = Number(t.completion_tokens || 0);
  const reqs = Number(t.requests || 0);
  const errs = Number(t.errors || 0);
  const okRate = reqs ? (reqs - errs) / reqs * 100 : null;
  // 构成条要的是合法 CSS 宽度，usPct 在无样本时返回 "—"，不能直接拼进 style。
  const pctW = (part) => total ? Math.max(0, Math.min(100, Number(part || 0) / total * 100)).toFixed(2) + '%' : '0%';
  // 六张卡：主指标用强调色，completion 用成功色（与图表里的绿柱呼应），
  // 失败/延迟只在有值时上语义色——全绿全黄的仪表盘等于没有重点。
  $('usStats').innerHTML =
    usKpi(fmtTok(reqs), '请求数', 'c-accent',
      errs ? '其中失败 ' + errs + ' 次' : '全部成功') +
    usKpi(fmtTok(total), '总 token', 'c-accent',
      'prompt ' + usPct(pt, total) + ' · completion ' + usPct(ct, total),
      '<div class="kbar"><i style="width:' + pctW(pt) + ';background:var(--accent)"></i>' +
      '<i style="width:' + pctW(ct) + ';background:var(--ok)"></i></div>') +
    usKpi(fmtTok(pt), 'prompt', 'c-soft', '占比 ' + usPct(pt, total)) +
    usKpi(fmtTok(ct), 'completion', 'c-ok', '占比 ' + usPct(ct, total)) +
    usKpi(String(errs), '失败尝试', errs ? 'c-warn' : 'c-mute',
      okRate == null ? '—' : (errs ? '成功率 ' + okRate.toFixed(1) + '%' : '成功率 100%')) +
    usKpi(fmtMs(t.avg_latency_ms), '平均延迟', 'c-soft',
      t.avg_tokens_per_second ? '吐字 ' + fmtRate(t.avg_tokens_per_second) : '无速率样本');

  // 卡片、明细表与时序图全部按所选窗口统计（切窗口数字随之变化）；
  // 「全部历史」含 90 天前折叠出的日桶。这里标注当前口径与数据起点。
  const winLabel = trangeLabel('usRange');
  // 服务端回显的实际区间优先（自定义区间下它就是权威口径）；滚动窗口没有回显，
  // 用控件自己的标签。
  const rangeEcho = usageData.window_from
    ? String(usageData.window_from).replace('T', ' ').slice(0, 16) +
      (usageData.window_to ? ' → ' + String(usageData.window_to).replace('T', ' ').slice(0, 16) : ' → 现在')
    : '';
  const note = (rangeEcho || winLabel ? (rangeEcho || winLabel) + ' · ' : '') +
    (usageData.buckets || 0) + ' 个分桶' +
    (usageData.since ? ' · 数据自 ' + usageData.since.replace('T', ' ') : '') +
    (usageData.file_bytes ? ' · 文件 ' + (usageData.file_bytes / 1024).toFixed(1) + ' KB' : '');
  $('usNote').textContent = note;
  $('usNote').title = note; // 窄屏单行截断时靠悬停看全

  // 积分扣除的四张卡片与说明。
  $('usCreditStats').innerHTML =
    usKpi(fmtCredit(t.credits), '扣除积分', 'c-accent', '按上游 usage.credit 累计') +
    usKpi(fmtTok(t.credit_tokens), '匹配 Token', 'c-mute', '与积分同时观测到的 Token') +
    usKpi(fmtCreditRatio(t.credits_per_1m_tokens, t.credit_samples, t.credit_tokens),
      '平均积分 / 1M Token', 'c-ok', '越低越划算') +
    usKpi(String(t.credit_samples || 0), '有效积分样本', 'c-mute', '缺字段的历史不参与折算') +
    usKpi(cacheRateText(t.cache_hit_tokens, t.cache_miss_tokens), '缓存命中率', 'c-mute',
      '上游前缀缓存命中 / (命中+未命中)；低命中意味着费用数倍放大');
  $('usCreditNote').textContent =
    (usageData.credit_by_account || []).length + ' 个账号 · ' +
    (usageData.credit_by_model || []).length + ' 个模型倍率分组 · 仅统计与积分同时观测到的 Token';

  renderUsageDim();
  renderCreditDim();
  renderUsageChart(usageData.series || []);
}

// 维度切换 / 排序控件。
if ($('usDimTabs')) $('usDimTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-dim]');
  if (!b) return;
  usDim = b.dataset.dim;
  renderUsageDim();
});
if ($('usCreditTabs')) $('usCreditTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-dim]');
  if (!b) return;
  usCreditDim = b.dataset.dim;
  renderCreditDim();
});
if ($('usSort')) $('usSort').onchange = () => {
  usSort = $('usSort').value;
  renderUsageDim();
};

/* renderUsageChart 画堆叠柱状图。
 *
 * x 轴是**真实时间轴**，不是按序号等距。这一点很重要：数据里存在 1 小时的
 * 间隔，也存在 6~8 小时的断档（没请求的时段不产生桶），等距排布会把 8 小时
 * 画得和 1 小时一样宽，让「什么时候用的」完全失真。
 *
 * 另外不再用 preserveAspectRatio="none"：那会把 viewBox 横向拉伸到容器宽度，
 * 柱子和文字都变形。改为固定比例、按容器宽度自适应高度。
 *
 * viewBox 取 1200×200（原 760×180）：SVG 以 width:100% 渲染，高宽比决定实际
 * 高度——旧比例在 1500px 宽的主区里会撑到 ~355px，只有一两根柱子时整块几乎是
 * 空白。宽 viewBox 把同宽度下的高度压到 ~250px，与下方表格的视觉重量相当。
 *
 * 时间轴用本地时间解析（后端返回的就是本地时区），day 点按当天 00:00 参与定位，
 * 与 hour 点在同一个连续轴上——日桶本来就是他那天所有小时的聚合。
 */

/* parsePointTime 把后端的 t 解析成毫秒时间戳。 */
function parsePointTime(p) {
  // hour: "2026-09-16T13"  day: "2026-09-16"
  const s = p.t.length === 13 ? p.t + ':00:00' : p.t + 'T00:00:00';
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d.getTime();
}

/* fmtTokTimeLabel 时间桶的短标签，与 x 轴刻度同一口径（日桶 MM-DD，小时桶 HH:00）。 */
function fmtTokTimeLabel(p) {
  const d = new Date(p.t);
  return p.scope === 'day'
    ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0')
    : String(d.getHours()).padStart(2, '0') + ':00';
}

function renderUsageChart(series) {
  const host = $('usChart');

  // 丢掉时间解析不出来的点，而不是让 NaN 传染整张图。
  const pts = [];
  for (const p of series) {
    const t = parsePointTime(p);
    if (t === null) continue;
    const pt = Number(p.prompt_tokens || 0);
    const ct = Number(p.completion_tokens || 0);
    pts.push({ t, scope: p.scope, raw: p.t, pt, ct, tt: Number(p.total_tokens || 0) || (pt + ct),
               req: p.requests || 0 });
  }
  if (!pts.length) {
    host.innerHTML = '<div class="us-empty">暂无用量数据。发起一次对话后再刷新。</div>';
    $('usChartNote').textContent = '—';
    return;
  }

  const W = 1200, H = 200, PL = 58, PR = 14, PT = 18, PB = 30;
  const iw = W - PL - PR, ih = H - PT - PB;

  const t0 = pts[0].t;
  const t1 = pts[pts.length - 1].t;
  const span = Math.max(1, t1 - t0);

  const max = Math.max(1, ...pts.map(p => p.tt));
  const peak = pts.reduce((a, b) => (b.tt > a.tt ? b : a), pts[0]);
  const avg = pts.reduce((s, p) => s + p.tt, 0) / pts.length;
  $('usChartNote').textContent =
    pts.length + ' 个点 · 峰值 ' + fmtTok(peak.tt) + ' @ ' + fmtTokTimeLabel(peak) +
    ' · 均值 ' + fmtTok(avg);

  // 柱宽取「最小真实间隔」的 70%，并夹在合理区间内——窗口拉到 30 天时柱子会
  // 变细，但不会细到看不见。
  let minGap = Infinity;
  for (let i = 1; i < pts.length; i++) minGap = Math.min(minGap, pts[i].t - pts[i - 1].t);
  if (!isFinite(minGap) || minGap <= 0) minGap = span;
  const slot = iw * (minGap / span);
  const bw = Math.max(2, Math.min(30, slot * 0.7));

  // 首尾各让出半个柱宽：否则第一个点和最后一个点的柱子会各有一半跑到绘图区外
  // （末点柱子贴着卡片右边缘被切掉），刻度仍用同一个 xOf，标签与柱子始终对齐。
  const xOf = t => PL + bw / 2 + (t - t0) / span * Math.max(1, iw - bw);
  const yOf = v => PT + ih - ih * (v / max);

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" ' +
            'preserveAspectRatio="xMidYMid meet">';

  // 柱体渐变：顶部实、底部略透，堆叠时两段仍能一眼分清（纯色块并排会糊成一片）。
  // 注意 stop-color 必须走 style 而不是 presentation 属性——Blink/WebKit 不解析
  // 属性里的 var()，写成 stop-color="var(--accent)" 会整条渐变失效（柱子全透明）。
  out += '<defs>' +
    '<linearGradient id="usGradP" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(--accent);stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(--accent);stop-opacity:.6"/></linearGradient>' +
    '<linearGradient id="usGradC" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(--ok);stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(--ok);stop-opacity:.6"/></linearGradient>' +
    '</defs>';

  // y 轴网格 + 刻度
  for (let i = 0; i <= 4; i++) {
    const y = PT + ih - (ih * i / 4);
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) +
           '" text-anchor="end">' + fmtTok(max * i / 4) + '</text>';
  }

  // 均值参考线：一眼看出"这根是不是异常高"，比只给刻度省心。
  // 标签放左侧：右侧常被峰值柱占用（峰值柱往往就是最后一根），贴左不会被压住。
  if (avg > 0 && avg < max) {
    const y = yOf(avg);
    out += '<line class="avg" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk-avg" x="' + (PL + 5) + '" y="' + (y - 4).toFixed(1) +
           '" text-anchor="start">均值 ' + fmtTok(avg) + '</text>';
  }

  // 柱子
  const yBase = PT + ih;
  for (const p of pts) {
    const x = xOf(p.t) - bw / 2;
    const hTot = ih * (p.tt / max);
    const hP = p.tt ? hTot * (p.pt / p.tt) : 0;
    const hC = Math.max(p.tt && p.ct ? 1 : 0, hTot - hP);
    // 每根柱子包一个 <g>，把 <title> 放进去。
    //
    // 为什么必须包裹：SVG 里 <title> 描述的是它的**父元素**。此前 <title> 是
    // <rect> 的兄弟节点（rect 自闭合，无法包含子节点），于是全部平铺在 <svg>
    // 根下 —— 整张图只有一个 tooltip（浏览器取第一个），悬停任何柱子都显示同一
    // 份数据（issue #128）。包进 <g> 后 tooltip 跟随该柱，且堆叠的两段
    //（prompt + completion）共用同一个提示。
    out += '<g><title>' + esc(p.raw) + '  ' + fmtTok(p.pt) + ' prompt / ' +
           fmtTok(p.ct) + ' completion / ' + p.req + ' 次</title>';
    // 圆角只给堆叠顶端（贴轴的底边保持方角，柱子才像"立"在基线上）。
    // 类名用 usbar 而不是 bar：账号池的积分条是 .bar{height:3px}，而 SVG2 里
    // height 是 rect 的 CSS 几何属性，同名类会把每根柱子压成 3px 高（踩过）。
    if (hP > 0) out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - hP).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hP.toFixed(2) +
      '" fill="url(#usGradP)"' + (hC > 0 ? '' : ' rx="1.5"') + '/>';
    if (hC > 0) out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - hP - hC).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hC.toFixed(2) +
      '" fill="url(#usGradC)" rx="1.5"/>';
    out += '</g>';
  }

  // 峰值标注：柱子够窄时文字压在柱顶，够宽时贴右侧避免和柱体重叠。
  {
    const px = xOf(peak.t);
    const py = yOf(peak.tt);
    const anchor = px > W - PR - 90 ? 'end' : 'middle';
    out += '<text class="tk-peak" x="' + Math.max(PL, Math.min(W - PR, px)).toFixed(1) +
           '" y="' + Math.max(10, py - 5).toFixed(1) + '" text-anchor="' + anchor + '">' +
           '峰值 ' + fmtTok(peak.tt) + '</text>';
  }

  // x 轴基线画在柱子之后，避免压在柱底
  out += '<line class="ax" x1="' + PL + '" y1="' + yBase + '" x2="' + (W - PR) +
         '" y2="' + yBase + '"/>';

  // x 轴刻度：按真实时间等距取 6 个位置，取该位置**最近的实际柱子**做标签，
  // 所以标签永远落在有数据的点上，不会指到空档里。
  const TICKS = Math.min(6, pts.length);
  const usedLabel = new Set();
  for (let k = 0; k < TICKS; k++) {
    const target = t0 + span * (TICKS === 1 ? 0.5 : k / (TICKS - 1));
    let bi = 0, best = Infinity;
    for (let i = 0; i < pts.length; i++) {
      const d = Math.abs(pts[i].t - target);
      if (d < best) { best = d; bi = i; }
    }
    if (usedLabel.has(bi)) continue;
    usedLabel.add(bi);
    const p = pts[bi];
    // 首尾标签靠边对齐，避免被裁掉
    const cx = xOf(p.t);
    const anchor = cx < PL + 14 ? 'start' : (cx > W - PR - 14 ? 'end' : 'middle');
    out += '<text class="tk" x="' + Math.max(PL, Math.min(W - PR, cx)).toFixed(1) +
           '" y="' + (PT + ih + 15) + '" text-anchor="' + anchor + '">' + esc(fmtTokTimeLabel(p)) + '</text>';
  }

  // 跨天时补一条日期分隔线，让「日界」在长窗口里可见
  let prevDay = null;
  for (const p of pts) {
    const d = new Date(p.t).getDate();
    if (prevDay !== null && d !== prevDay) {
      const x = xOf(p.t).toFixed(1);
      out += '<line class="gl" x1="' + x + '" y1="' + PT + '" x2="' + x + '" y2="' +
             (PT + ih) + '" style="opacity:.45"/>';
    }
    prevDay = d;
  }

  out += '</svg>';
  host.innerHTML = out;
}

function fmtTokTip(v) { return fmtTok(v); }

let usageRateWarmAt = 0;
async function warmUsageModelRates() {
  if (Date.now() - usageRateWarmAt < 10 * 60 * 1000) return;
  try {
    await api('models');
  } catch (e) {
    // 倍率回填是可选增强；失败不阻塞用量统计，10 分钟后再试。
  }
  usageRateWarmAt = Date.now();
}

async function loadUsage() {
  const q = trangeQuery('usRange', true);
  try {
    await warmUsageModelRates();
    const d = await api('usage?' + q.toString());
    renderUsage(d);
  } catch (e) {
    // 失败时三块都要清干净：只改图表会留下上一次窗口的数字，看起来像"刷新成功"。
    usageData = null;
    $('usChart').innerHTML = '<div class="us-empty">读取用量失败：' + esc(e.message) + '</div>';
    $('usChartNote').textContent = '—';
    $('usStats').innerHTML = '';
    $('usCreditStats').innerHTML = '';
    $('usNote').textContent = '—';
    $('usCreditNote').textContent = '—';
    $('usDimNote').textContent = '—';
    $('usDimHead').innerHTML = '';
    $('usCreditHead').innerHTML = '';
    $('usDimTabs').innerHTML = usTabsHtml(US_DIM_TABS, usDim, null);
    $('usCreditTabs').innerHTML = usTabsHtml([['account', '按账号'], ['model', '按模型']], usCreditDim, null);
    $('usDimBody').innerHTML = '<tr><td colspan="10" class="empty">读取用量失败</td></tr>';
    $('usCreditBody').innerHTML = '<tr><td colspan="7" class="empty">读取用量失败</td></tr>';
  }
}

if ($('btnUsage')) $('btnUsage').onclick = loadUsage;
// 时间范围控件绑定：任何改动（预设切换 / 自定义起止）都重新拉一次用量。
if ($('usRange')) trangeBind('usRange', loadUsage);

/* ── 积分构成 ─────────────────────────────────────────────────────── */
/* 一个账号的余额是若干积分包之和。包按来源命名（「国内运营裂变包」「拉新权益包」
   「个人体验版」…），面额从 6 到 1500 不等，且**按次发放**。所以两个任务完成度
   完全一致的账号，余额可能差上千——差别只在包里。这里把逐包明细摊开，并给每个
   包名一个稳定配色，跨账号对比时同色即同类。 */

const PK_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6', '#e2607a',
                   '#5aa9e6', '#8fbf3f', '#b58b5a', '#7d8fa8', '#d4785c'];
const PK_ACCOUNT_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6',
                           '#e2607a', '#20a4a4', '#8fbf3f', '#d4785c',
                           '#7c83db', '#c48a2f', '#b45f8c', '#5aa9e6'];

function pkColor(i) { return PK_COLORS[i % PK_COLORS.length]; }

// pkAccountColorMap 按 UID 稳定分配颜色：排序后分配，账号刷新/重排不会换色。
function pkAccountColorMap(list) {
  const uids = (list || [])
    .filter(a => a && !a.error && a.uid)
    .map(a => String(a.uid))
    .sort();
  const colors = new Map();
  uids.forEach((uid, i) => colors.set(uid, PK_ACCOUNT_COLORS[i % PK_ACCOUNT_COLORS.length]));
  return colors;
}

/* pkBySource 把包按名称归并，得到「来源 → 面额/余额/个数」。这是对比的关键视图：
   两个号的差异一定体现在某几个来源的面额上。 */
function pkBySource(packs) {
  const m = new Map();
  for (const p of packs) {
    // 分组键用 code + name，而不是只 name：上游给「首登赠送」和普通活动包用了
    // **同一个 PackageName 和同一个 PackageCode**，只按 name 会把两类混成一类，
    // 那正是当初「两个号为何差 1500」看不出来的原因。这里至少把 code 带进键里，
    // 并在卡片上显示最早的发放时间。
    const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
    const e = m.get(k) || {
      key: k, name: p.name || '(未命名)', code: p.package_code || '',
      n: 0, remain: 0, size: 0, used: 0, minEnd: '', minCreated: '',
    };
    e.n += 1;
    e.remain += Number(p.remain || 0);
    e.size += Number(p.size || 0);
    e.used += Number(p.used || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (t && (!e.minEnd || t < e.minEnd)) e.minEnd = t;
    const c = (p.created_at || '').slice(0, 10);
    if (c && (!e.minCreated || c < e.minCreated)) e.minCreated = c;
    m.set(k, e);
  }
  return [...m.values()].sort((a, b) => b.size - a.size);
}

const PK_DEFAULT_DETAIL_LIMIT = 5;

function pkDetailLimitValue(raw) {
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : PK_DEFAULT_DETAIL_LIMIT;
}

function pkDetailLimit(cfg) {
  return pkDetailLimitValue(cfg && cfg.panel && cfg.panel.package_detail_limit);
}

const PK_DAY_MS = 24 * 3600 * 1000;

function pkExpiryMs(p) {
  const raw = Number(p && p.expires_at);
  if (Number.isFinite(raw) && raw > 0) return raw;
  const text = String((p && p.end_time) || '').trim();
  if (!text) return null;
  let iso = text.includes('T') ? text : text.replace(' ', 'T');
  if (!/(?:Z|[+-]\d\d:\d\d)$/.test(iso)) iso += '+08:00';
  const parsed = Date.parse(iso);
  return Number.isFinite(parsed) ? parsed : null;
}

// pkDetailCompare 只服务单账号逐包明细：正余额包先按到期时间挑选默认展示项，
// 其余正余额包与已用完包分别折叠；同一到期时间按面额降序。主键跟随视图排序
// 模式（pkSortMode，声明在本区块末尾的绑定块）：end_asc 到期升序、size_desc
// 面额降序（同面额按到期升序）。typeof 守卫：前端 harness 的区域切片求值里
// 没有该全局，回落 end_asc（= 上游原有语义，切片测试的期望序不受影响）。
function pkDetailCompare(a, b) {
  const sizeOf = p => {
    const n = Number(p && p.size);
    return Number.isFinite(n) ? n : 0;
  };
  const mode = (typeof pkSortMode === 'string' && pkSortMode) || 'end_asc';
  if (mode === 'size_desc') {
    const d = sizeOf(b) - sizeOf(a);
    if (d !== 0) return d;
  }
  const ea = pkExpiryMs(a), eb = pkExpiryMs(b);
  if (ea == null && eb != null) return 1;
  if (ea != null && eb == null) return -1;
  if (ea != null && eb != null && ea !== eb) return ea - eb;
  return sizeOf(b) - sizeOf(a);
}

function pkDetailGroups(packs, limit) {
  const active = [], used = [];
  let usedSize = 0, restSize = 0, restRemain = 0;
  for (const p of packs || []) {
    const remain = Number(p && p.remain);
    if (remain > 0) {
      active.push(p);
      continue;
    }
    used.push(p);
    const size = Number(p && p.size);
    if (Number.isFinite(size)) usedSize += size;
  }
  active.sort(pkDetailCompare);
  used.sort(pkDetailCompare);
  const visible = active.slice(0, pkDetailLimitValue(limit));
  const rest = active.slice(visible.length);
  for (const p of rest) {
    const size = Number(p && p.size);
    if (Number.isFinite(size)) restSize += size;
    const remain = Number(p && p.remain);
    if (Number.isFinite(remain)) restRemain += remain;
  }
  return { visible, rest, used, restSize, restRemain, usedSize };
}

function pkCreditOpacity(days) {
  if (days == null || !Number.isFinite(Number(days))) return 1;
  return 0.25 + 0.75 * Math.max(0, Math.min(29, Number(days) - 1)) / 29;
}

function pkExpiryText(expiresAt) {
  if (!expiresAt) return '无到期时间';
  const diff = expiresAt - Date.now();
  if (diff <= 0) return '已到期';
  const minutes = Math.max(1, Math.ceil(diff / 60000));
  if (minutes < 60) return '剩余 ' + minutes + ' 分钟';
  const hours = Math.ceil(diff / 3600000);
  if (hours < 24) return '剩余 ' + hours + ' 小时';
  return '剩余 ' + Math.ceil(diff / PK_DAY_MS) + ' 天';
}

function pkExpiryDateTime(expiresAt) {
  if (!expiresAt) return '—';
  return new Date(expiresAt).toLocaleString('zh-CN', {
    timeZone: 'Asia/Shanghai', hour12: false,
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
  });
}

function pkAccountSegments(a, now) {
  let balance = Math.max(0, Number(a.remain || 0));
  const out = [];
  for (const p of a.packages || []) {
    const remain = Number(p.remain || 0);
    if (!Number.isFinite(remain) || remain <= 0 || balance <= 0) continue;
    const amount = Math.min(balance, remain);
    const expiresAt = pkExpiryMs(p);
    out.push({
      amount,
      expiresAt,
      days: expiresAt == null ? null : Math.max(0, Math.ceil((expiresAt - now) / PK_DAY_MS)),
      source: p.name || '积分',
      uid: String(a.uid || ''),
      accountName: a.nickname || String(a.uid || '').slice(0, 8) || '未命名账号',
    });
    balance -= amount;
  }
  return out.sort((x, y) => {
    if (x.expiresAt == null && y.expiresAt != null) return 1;
    if (x.expiresAt != null && y.expiresAt == null) return -1;
    return (x.expiresAt || 0) - (y.expiresAt || 0);
  });
}

// summarizeCreditDays 对齐 WorkDaddy：按精确剩余天数逐行聚合，无有效到期时间的余额
// 不进入图表，也不猜测到期日。账号内先按总余额约束逐包金额，避免上游重复记录膨胀。
function summarizeCreditDays(list, now) {
  const buckets = new Map();
  let unavailable = 0;
  for (const a of list || []) {
    if (a.error || !Number.isFinite(Number(a.remain))) {
      unavailable++;
      continue;
    }
    for (const segment of pkAccountSegments(a, now)) {
      if (segment.days == null) continue;
      let row = buckets.get(segment.days);
      if (!row) {
        row = { days: segment.days, credits: 0, segments: [] };
        buckets.set(segment.days, row);
      }
      row.credits += segment.amount;
      row.segments.push(segment);
    }
  }
  const rows = [...buckets.values()].sort((a, b) => a.days - b.days);
  for (const row of rows) {
    row.segments.sort((a, b) =>
      (a.expiresAt || Infinity) - (b.expiresAt || Infinity) ||
      a.accountName.localeCompare(b.accountName) ||
      a.source.localeCompare(b.source));
  }
  return { rows, accountCount: (list || []).length, unavailable };
}

function renderExpiryDistribution(list, now) {
  const summary = summarizeCreditDays(list, now);
  const colors = pkAccountColorMap(list);
  const rows = summary.rows.map(row => {
    const total = row.credits || 1;
    const nodes = row.segments.map(segment => {
      const color = colors.get(segment.uid) || 'var(--accent)';
      const title = segment.source + '\n' + fmtTok(segment.amount) + ' 积分\n到期时间 ' +
        pkExpiryDateTime(segment.expiresAt) + '（' + pkExpiryText(segment.expiresAt) + '）\n' +
        segment.accountName;
      return '<span class="pk-expiry-seg" style="--seg-color:' + color +
        ';opacity:' + pkCreditOpacity(segment.days).toFixed(5) +
        ';flex:' + Math.max(0.008, segment.amount / total).toFixed(4) +
        ' 1 0" title="' + esc(title) + '" aria-label="' + esc(title) + '"></span>';
    }).join('');
    return '<div class="pk-expiry-row"><span>' + esc(row.days === 0 ? '已到期' : row.days + ' 天') +
      '</span><div class="pk-expiry-track">' + nodes + '</div><b>' + esc(fmtTok(row.credits)) +
      '</b></div>';
  }).join('');
  const foot = summary.accountCount + ' 个账号' +
    (summary.unavailable ? ' · ' + summary.unavailable + ' 个未获取余额' : '');
  const legend = (list || []).filter(a =>
    a && !a.error && a.uid && pkAccountSegments(a, now).some(s => s.days != null)
  ).map(a => '<span><i style="background:' + (colors.get(String(a.uid)) || 'var(--accent)') +
    '"></i>' + esc(a.nickname || String(a.uid).slice(0, 8)) + '</span>').join('');
  const hdr = '<div class="pk-expiry-hdr"><span>剩余天数</span><span style="text-align:center">各账号该批剩余</span><b>剩余积分</b></div>';
  $('pkExpiry').innerHTML = (rows
    ? hdr + '<div class="pk-expiry-chart">' + rows + '</div>'
    : '<div class="pk-expiry-empty">暂无可汇总积分</div>') +
    (legend ? '<div class="pk-expiry-legend">' + legend + '</div>' : '') +
    '<div class="pk-expiry-foot">' + esc(foot) + '</div>';
}

function renderPackages(d, detailLimit) {
  const list = (d.accounts || []);
  const now = Date.now();
  const expiryColors = pkAccountColorMap(list);
  renderExpiryDistribution(list, now);
  if (!list.length) {
    $('pkSummary').innerHTML = '<div class="empty">没有账号</div>';
    return;
  }

  // 包名 → 稳定色号（跨账号一致，方便肉眼对齐）
  const names = [];
  for (const a of list) for (const s of pkBySource(a.packages || [])) {
    if (!names.includes(s.key)) names.push(s.key);
  }
  names.sort((x, y) => {
    const sz = n => Math.max(...list.map(a => {
      const f = pkBySource(a.packages || []).find(s => s.key === n);
      return f ? f.size : 0;
    }));
    return sz(y) - sz(x);
  });
  const colorOf = n => pkColor(names.indexOf(n));
  // 键 → 展示名，供卡片与明细表共用（同一来源必然同色同名）。
  const labelOf = {};
  for (const a of list) for (const s of pkBySource(a.packages || [])) labelOf[s.key] = s;

  const maxRemain = Math.max(1, ...list.map(a => Number(a.remain || 0)));

  $('pkSummary').innerHTML = list.map(a => {
    if (a.error) {
      return '<div class="pk-card"><div class="who"><span class="nm">' +
        esc((a.nickname || a.uid.slice(0, 8))) + '</span>' +
        '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
        '<div class="err">查询失败：' + esc(a.error) + '</div></div>';
    }
    const srcs = pkBySource(a.packages || []);
    const total = Math.max(1, Number(a.size || 0));
    const bar = srcs.map(s =>
      '<i style="width:' + (s.size / total * 100).toFixed(2) + '%;background:' +
      colorOf(s.key) + '" title="' + esc(s.name) + ' ' + fmtTok(s.size) + '"></i>'
    ).join('');
    const legend = srcs.map(s =>
      '<span><i style="background:' + colorOf(s.key) + '"></i>' +
      esc(s.name.replace(/^CodeBuddy/, '')) + ' x' + s.n + ' · ' + fmtTok(s.size) +
      (s.minCreated ? ' · 首发 ' + esc(s.minCreated.slice(5)) : '') + '</span>'
    ).join('');
    const expiry = pkAccountSegments(a, now);
    const expiryTotal = Math.max(1, expiry.reduce((sum, s) => sum + s.amount, 0));
    const expiryColor = expiryColors.get(String(a.uid)) || 'var(--accent)';
    const expiryBar = expiry.length ? '<div class="expirybar" role="img" aria-label="积分到期分布">' +
      expiry.map(s => {
        const title = s.source + '\n' + fmtTok(s.amount) + ' 积分\n到期时间 ' +
          pkExpiryDateTime(s.expiresAt) + '（' + pkExpiryText(s.expiresAt) + '）';
        return '<i style="background:' + expiryColor +
          ';opacity:' + pkCreditOpacity(s.days).toFixed(5) +
          ';flex:' + Math.max(0.008, s.amount / expiryTotal).toFixed(4) +
          ' 1 0" title="' + esc(title) + '"></i>';
      }).join('') + '</div>' : '';
    return '<div class="pk-card">' +
      '<div class="who"><span class="nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</span>' +
      '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
      '<div class="big">' + fmtTok(a.remain) + '</div>' +
      '<div class="sub">共 ' + fmtTok(a.size) + ' · ' + (a.packages || []).length +
      ' 个包 · 占最高 ' + (Number(a.remain || 0) / maxRemain * 100).toFixed(0) + '%</div>' +
      '<div class="mixbar">' + bar + '</div>' +
      expiryBar +
      '<div class="pk-legend">' + legend + '</div>' +
      '</div>';
  }).join('');

  $('pkNote').textContent = list.length + ' 个账号 · 实时查询上游';

  // 逐包明细：每个账号一个表，排序规则由视图顶部的选择器决定（默认到期近的在前）
  $('pkDetail').innerHTML = list.map(a => {
    if (a.error) return '';
    const groups = pkDetailGroups(a.packages || [], detailLimit);
    const rowOf = (p, rowGroup) => {
      const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
      const sub = (p.sub_product_code || '').replace(/^sp_tcaca_codebuddyide_?/, '') ||
                  (p.package_code || '').replace(/^TCACA_/, '');
      return '<tr' + (rowGroup ? ' class="pk-hidden-row pk-' + rowGroup +
        '-row" data-pk-row="' + rowGroup + '" hidden' : '') +
        '><td class="mark" aria-hidden="true"><i style="background:' +
        colorOf(k) + '"></i></td>' +
      '<td>' + esc(p.name || '(未命名)') +
        (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
      '<td class="num">' + fmtTok(p.size) + '</td>' +
      '<td class="num">' + fmtTok(p.remain) + '</td>' +
      '<td class="num">' + fmtTok(p.used) + '</td>' +
      '<td class="num">' + esc((p.created_at || '').slice(0, 16).replace('T', ' ') || '—') + '</td>' +
      '<td class="num">' + esc((p.end_time || '').slice(0, 10) || '—') + '</td>' +
      '</tr>';
    };
    const groupSummary = (group, label, count, size, remain) =>
      '<tr class="pk-group-summary"><td colspan="7"><button type="button" class="pk-group-toggle"' +
      ' data-pk-group="' + group + '" data-count="' + count + '" data-size="' + size +
      '" data-remain="' + remain + '" aria-expanded="false">' + label + '，展开</button></td></tr>';
    const rows = groups.visible.map(p => rowOf(p, '')).join('');
    const restSummary = groups.rest.length
      ? groupSummary('rest', '其余未用完 ' + groups.rest.length + ' 个包（面额合计 ' +
          fmtTok(groups.restSize) + ' · 剩余 ' + fmtTok(groups.restRemain) + '）',
          groups.rest.length, groups.restSize, groups.restRemain) +
        groups.rest.map(p => rowOf(p, 'rest')).join('')
      : '';
    const usedSummary = groups.used.length
      ? groupSummary('used', '已用完 ' + groups.used.length + ' 个包（面额合计 ' +
          fmtTok(groups.usedSize) + '）', groups.used.length, groups.usedSize, 0) +
        groups.used.map(p => rowOf(p, 'used')).join('')
      : '';
    return '<div class="box"><header><h3>' +
      esc(a.nickname || a.uid.slice(0, 8)) + ' · ' + esc(a.realm || '') +
      '</h3><span class="grow"></span><span class="note">余额 ' + fmtTok(a.remain) +
      ' / 总额 ' + fmtTok(a.size) + ' · 可用 ' + (groups.visible.length + groups.rest.length) + ' 个包' +
      (groups.used.length ? ' / 已用完 ' + groups.used.length + ' 个' : '') +
      ' · 默认展示最早到期 ' + pkDetailLimitValue(detailLimit) + ' 条（' +
      esc(PK_SORT_LABELS[pkSortMode] || '') + '）</span>' +
      '</header><div class="tbl-wrap"><table class="acc"><thead><tr>' +
      '<th class="mark" aria-hidden="true"></th><th>包名 / 来源</th>' +
      '<th class="num">面额</th><th class="num">剩余</th><th class="num">已用</th>' +
      '<th class="num">发放</th><th class="num">到期</th>' +
      '</tr></thead><tbody>' + rows + restSummary + usedSummary + '</tbody></table></div></div>';
  }).join('');
}

if ($('pkDetail')) $('pkDetail').addEventListener('click', ev => {
  const btn = ev.target.closest('button[data-pk-group]');
  if (!btn) return;
  const body = btn.closest('tbody');
  if (!body) return;
  const group = btn.dataset.pkGroup;
  const expanded = btn.getAttribute('aria-expanded') === 'true';
  body.querySelectorAll('tr[data-pk-row="' + group + '"]').forEach(row => { row.hidden = expanded; });
  const count = btn.dataset.count || '0';
  const size = btn.dataset.size || '0';
  const remain = btn.dataset.remain || '0';
  btn.setAttribute('aria-expanded', String(!expanded));
  if (group === 'rest') {
    btn.textContent = expanded
      ? '其余未用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + ' · 剩余 ' +
        fmtTok(remain) + '），展开'
      : '收起其余未用完 ' + count + ' 个包';
  } else {
    btn.textContent = expanded
      ? '已用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + '），展开'
      : '收起已用完 ' + count + ' 个包';
  }
});

/* ── 逐包明细排序模式 ─────────────────────────────────────────────── */
/* 逐包明细的排序规则（选择持久化在 localStorage，跨会话记住）：
   end_asc   到期升序（默认）——快过期的包排最前，提醒优先消耗；无到期时间的
             包（上游没下发 end_time）没有可比的日期，统一垫底，不掺进日期序里；
   size_desc 面额降序——原来的展示口径，看「钱从哪来」。
   行序统一由 pkDetailCompare 实现（明细折叠分组共用同一比较器，切换模式时
   折叠组内行序同步跟随）。本块整体放在 renderPackages / renderExpiryDistribution
   之后：前端 harness 按区域切片求值（[PK_ACCOUNT_COLORS, renderExpiryDistribution)
   与 [PK_DEFAULT_DETAIL_LIMIT, renderPackages)），顶层 localStorage/$ 语句落进
   切片区会让无 DOM 桩的求值环境 ReferenceError——上游测试的切片边界不动。 */
const LS_PK_SORT = 'pkSortMode';
let pkSortMode = localStorage.getItem(LS_PK_SORT) || 'end_asc';
const PK_SORT_LABELS = { end_asc: '按到期升序 · 近的在前', size_desc: '按面额降序' };
// 排序切换时重排明细需要 detailLimit（上游折叠配置），loadPackages 拉到后缓存。
let lastDetailLimit = PK_DEFAULT_DETAIL_LIMIT;

// 排序规则控件：恢复上次选择并绑定切换。
if ($('pkSort')) {
  $('pkSort').value = pkSortMode;
  if ($('pkSort').value !== pkSortMode) {        // localStorage 里存了废弃值：回落默认
    pkSortMode = 'end_asc';
    localStorage.setItem(LS_PK_SORT, pkSortMode);   // 不用 removeItem：harness 桩无此方法
  }
  $('pkSort').onchange = () => {
    pkSortMode = $('pkSort').value;
    localStorage.setItem(LS_PK_SORT, pkSortMode);
    if (lastPackages) renderPackages(lastPackages, lastDetailLimit);   // 数据在内存，直接重排
  };
}

// loadPackages 按需拉取：有缓存（上次任意入口拉到的 packages 数据）先直接渲染，
// 不再「切进积分构成页就打一次上游」——逐账号实时查询在账号多时既慢又压上游。
// 无缓存（首次进入）才自动查一次；显式刷新（force=true，点「刷新」按钮）始终实时查。
async function loadPackages(force) {
  if (!force && lastPackages) {
    renderPackages(lastPackages, lastDetailLimit);
    const ageMin = Math.floor((Date.now() - lastPackagesAt) / 60000);
    if (ageMin > 0) $('pkNote').textContent = lastPackages.accounts.length + ' 个账号 · ' + ageMin + ' 分钟前的数据，点「刷新」更新';
    return;
  }
  $('pkSummary').innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  $('pkDetail').innerHTML = '';
  $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">查询中…</div>';
  try {
    const [d, c] = await Promise.all([
      api('packages'),
      api('config').catch(() => null),
    ]);
    lastPackages = d;                            // 缓存供首页到期卡片与排序切换复用
    lastPackagesAt = Date.now();
    lastDetailLimit = pkDetailLimit(c && c.config);
    renderPackages(d, lastDetailLimit);
  } catch (e) {
    $('pkSummary').innerHTML = '<div class="empty">读取失败：' + esc(e.message) + '</div>';
    $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">读取失败：' + esc(e.message) + '</div>';
  }
}

/* ── 积分到期提醒（首页卡片）────────────────────────────────────────── */
/* 积分不是永久的：签到/任务发的裂变包约一个月失效。只看「剩余积分 ÷ 日消耗」
   会系统性偏乐观——用不完的部分到期直接蒸发。这里把「最近要过期的是哪批、
   有多少、到期前每天要至少消耗多少」顶到首页，数据源与「积分构成」共用
   （lastPackages 缓存，EXP_FRESH_MS 内复用，不重复打上游）。 */

// expBatches 把某账号的包聚合成「到期日 → 该日作废积分」升序列表。
// 只统计 remain>0 且有到期时间的包——没余额/长期包到期没有任何影响。
function expBatches(packs) {
  const byDay = new Map();
  for (const p of packs || []) {
    const r = Number(p.remain || 0);
    const t = (p.end_time || '').slice(0, 10);
    if (r <= 0 || !t) continue;
    byDay.set(t, (byDay.get(t) || 0) + r);
  }
  return [...byDay.entries()]
    .map(([date, remain]) => ({ date, remain }))
    .sort((a, b) => (a.date < b.date ? -1 : 1));
}

function expDaysLeft(dateStr, today) {
  return Math.round((new Date(dateStr + 'T00:00:00') - today) / 86400000);
}

// expiryKeyed 到期提醒的排序结果。**「检查」与首页快照共用这一份实现**，
// 保证两条数据路径（逐包明细 / state 里的按日批次）输出完全一致：排序、今天/明天
// 文案、日均需耗、7 天内合计、随后批次都只有一份实现。
// list 每项形态：{ uid, nickname, packages:[{end_time, remain}], error? }。
// 单独拆出来是为了分页：必须先排序再切片，切片后交给 expiryRowsOf 渲染。
function expiryKeyed(list) {
  const today = new Date(); today.setHours(0, 0, 0, 0);
  // 按「最近到期」升序排（issue #125）。
  //
  // 后端 /panel/api/packages 是按**余额降序**返回的，恰好把 CN 账号都排在前面、
  // global 排在末尾，看起来像"按域分组"，其实只是余额顺序。这里再排一次，让
  // "最快过期的排最前"这个唯一重要的顺序成立，且不分域。
  //
  // 排序键用最早到期批次的日期（expBatches 已按日期升序，故 [0] 即最早）。
  // 查询失败的账号、以及 7 天内无到期的账号没有可比较的到期时间，统一排在最后，
  // 保持它们原本的相对顺序（稳定排序）。
  const keyed = list.map(a => {
    let key = null;
    if (!a.error) {
      const bs = expBatches(a.packages).filter(b => expDaysLeft(b.date, today) >= 0);
      if (bs.length) key = bs[0].date;
    }
    return { a, key };
  });
  keyed.sort((x, y) => {
    if (x.key !== y.key) {
      if (x.key === null) return 1;
      if (y.key === null) return -1;
      return x.key < y.key ? -1 : 1;
    }
    if (x.key === null) return 0;
    // 同日到期的账号按昵称/uid 兜底排序：两条数据路径（检查的余额降序、
    // overview 的 uid 升序）输入顺序不同，加固定 tie-break 才能保证输出一致。
    const ax = String(x.a.nickname || x.a.uid || '');
    const ay = String(y.a.nickname || y.a.uid || '');
    return ax < ay ? -1 : ax > ay ? 1 : 0;
  });
  return { keyed, today };
}

// expiryRowsOf 把排好序的账号渲染成行 HTML（分页只切 keyed，渲染逻辑不变）。
function expiryRowsOf(keyed, today) {
  return keyed.map(({ a }) => {
    const name = a.nickname || String(a.uid || '').slice(0, 8);
    if (a.error) {
      return '<div class="exp-row"><span class="exp-dot" style="background:var(--ink-3)"></span>' +
        '<span class="exp-nm">' + esc(name) + '</span>' +
        '<span class="exp-main err">查询失败：' + esc(a.error) + '</span></div>';
    }
    const bs = expBatches(a.packages).filter(b => expDaysLeft(b.date, today) >= 0);
    if (!bs.length) {
      return '<div class="exp-row"><span class="exp-dot" style="background:var(--ok)"></span>' +
        '<span class="exp-nm">' + esc(name) + '</span>' +
        '<span class="exp-main">7 天内无到期积分</span></div>';
    }
    const first = bs[0];
    const days = expDaysLeft(first.date, today);
    const daily = Math.ceil(first.remain / Math.max(1, days));
    const week = bs.filter(b => expDaysLeft(b.date, today) <= 7)
      .reduce((s, b) => s + b.remain, 0);
    // 危险度：≤3 天红（不抓紧就真没了）、≤7 天琥珀、更远绿。
    // 上游扣包是 FEFO（按失效时刻升序，实测两号口径一致）：这些快过期批次正是
    // 被消耗得最快的，日均需耗给的是「哪怕单靠这个账号的自然流量也能对齐」的参照。
    const cls = days <= 3 ? 'var(--bad)' : days <= 7 ? 'var(--warn)' : 'var(--ok)';
    const dayWord = days === 0 ? '今天到期' : days === 1 ? '明天到期' : days + ' 天后到期';
    const more = bs.length > 4 ? '　等 ' + bs.length + ' 批' : '';
    const rest = bs.slice(1, 4).map(b =>
      '随后 ' + esc(b.date.slice(5)) + ' · ' + fmtTok(b.remain)).join('　') + more;
    return '<div class="exp-row"><span class="exp-dot" style="background:' + cls + '"></span>' +
      '<span class="exp-nm">' + esc(name) + '</span>' +
      '<span class="exp-main">最近到期 <b>' + esc(first.date) + '</b>（' + dayWord +
      '）· 该批 <b>' + fmtTok(first.remain) + '</b> 积分 · 到期前日均需耗 ≥<b>' +
      fmtTok(daily) + '</b>' +
      (week > first.remain ? ' · 7 天内合计 ' + fmtTok(week) : '') +
      (rest ? '<div class="note">' + rest + '</div>' : '') +
      '</span></div>';
  }).join('');
}

// expiryRowsHtml 全量渲染入口（保留原签名，上游与前端 harness 都可能直接调用）。
function expiryRowsHtml(list) {
  const { keyed, today } = expiryKeyed(list);
  return expiryRowsOf(keyed, today);
}

// expSlice 按当前分页状态切出要渲染的账号，并把页码/总数回写到筛选栏。
// 前端 harness 的切片环境没有模块级分页状态（expAll/expPage/expPageSize），
// 此时原样返回全量，输出与不分页时逐字节一致。
function expSlice(keyed) {
  if (typeof expAll === 'undefined' || typeof expPage !== 'number' ||
    typeof expPageSize !== 'number') return keyed;
  expAll = keyed;
  const pages = expPageSize > 0 ? Math.max(1, Math.ceil(expAll.length / expPageSize)) : 1;
  if (expPage > pages) expPage = pages;
  if (expPage < 1) expPage = 1;
  if ($('expCount')) $('expCount').textContent = '共 ' + expAll.length + ' 个';
  if ($('expPageInfo')) $('expPageInfo').textContent = expPage + ' / ' + pages;
  if ($('expPrev')) $('expPrev').disabled = expPage <= 1;
  if ($('expNext')) $('expNext').disabled = expPage >= pages;
  return expPageSize > 0
    ? expAll.slice((expPage - 1) * expPageSize, expPage * expPageSize)
    : expAll;
}

function renderExpiry(d) {
  const list = (d.accounts || []);
  const { keyed, today } = expiryKeyed(list);
  const rows = expiryRowsOf(expSlice(keyed), today);
  $('expList').innerHTML = rows || '<div class="empty">没有账号</div>';
  // 数据新鲜度透明化：走缓存时标注年龄，免得把旧数据误当实时。
  const ageMin = lastPackages ? Math.floor((Date.now() - lastPackagesAt) / 60000) : 0;
  $('expNote').textContent = (lastPackagesAt && ageMin > 0)
    ? list.length + ' 个账号 · ' + ageMin + ' 分钟前的数据，可点「检查」刷新'
    : list.length + ' 个账号 · 实时查询上游';
  // 可见性归 tab 管（本地三级 tab）；无 expBoxShow 的切片环境退回直接显示。
  if (typeof expBoxShow === 'function') expBoxShow();
  else $('expBox').hidden = false;
}

/* 快照版到期提醒：不查上游，直接用 /overview 里的 credits_expiry_buckets。
   后端每次余额刷新（默认每 5 分钟）按自然日聚合并写入 state.json，口径与
   「检查」的逐包按日汇总完全相同；这里把批次还原成 packages 形态后直接复用
   expiryRowsHtml，因此排序/今天明天/日均需耗/7 天内合计/随后批次全部一致。

   旧 state（升级后尚未跑过一轮余额刷新，或 Redis 快照来自旧版本）没有 buckets，
   退回只显示最早一批，保证卡片不空白；下一轮刷新后自动变成完整视图。 */
function renderExpirySnapshot(d) {
  const list = (d && d.accounts) || [];
  const hasBuckets = list.some(a => Array.isArray(a.credits_expiry_buckets) && a.credits_expiry_buckets.length);
  if (hasBuckets) {
    const isoDay = v => {
      const dt = new Date(v);
      if (isNaN(dt.getTime())) return '';
      const p2 = n => String(n).padStart(2, '0');
      return dt.getFullYear() + '-' + p2(dt.getMonth() + 1) + '-' + p2(dt.getDate());
    };
    // 逐账号取批次；升级过渡期/单号刷新失败的账号可能还没有 buckets，用旧的
    // 「最早一批」字段兜底，避免在同一次渲染里被误报成「无到期」。
    const packagesOf = a => {
      const bs = a.credits_expiry_buckets || [];
      if (bs.length) {
        return bs.map(b => ({ end_time: (b.date || '') + 'T00:00:00', remain: b.remain }));
      }
      const day = isoDay(a.credits_earliest_expiry);
      return (day && a.credits_earliest_remaining)
        ? [{ end_time: day + 'T00:00:00', remain: a.credits_earliest_remaining }]
        : [];
    };
    renderExpiry({
      accounts: list.map(a => ({
        uid: a.uid,
        nickname: a.nickname,
        realm: a.realm,
        packages: packagesOf(a),
      })),
    });
    $('expNote').textContent = snapshotNoteText(list.length, d);
    return;
  }
  // 旧 state 兜底：只有最早一批的日期与数量。
  const today = new Date(); today.setHours(0, 0, 0, 0);
  const keyed = list.map(a => {
    // 后端给的是 RFC3339 带时区；无到期信息（长期包/未刷新）的账号排在最后。
    let key = null, ms = 0;
    if (a.credits_earliest_expiry) {
      const t = new Date(a.credits_earliest_expiry);
      if (!isNaN(t.getTime())) { ms = t.getTime(); key = ms; }
    }
    return { a, key, ms };
  });
  keyed.sort((x, y) => {
    if (x.key === null && y.key === null) return 0;
    if (x.key === null) return 1;
    if (y.key === null) return -1;
    return x.key - y.key;
  });
  // 分页：排序后的完整列表留一份，翻页只重渲染当前页（1000+ 个号一页放不下）。
  expAll = keyed;
  const pages = expPageSize > 0 ? Math.max(1, Math.ceil(expAll.length / expPageSize)) : 1;
  if (expPage > pages) expPage = pages;
  if (expPage < 1) expPage = 1;
  const pageRows = expPageSize > 0 ? expAll.slice((expPage - 1) * expPageSize, expPage * expPageSize) : expAll;
  const rows = pageRows.map(({ a, ms }) => {
    const nm = esc(a.nickname || String(a.uid || '').slice(0, 8));
    if (!ms || !a.credits_earliest_remaining) {
      return '<div class="exp-row"><span class="exp-dot" style="background:var(--ok)"></span>' +
        '<span class="exp-nm">' + nm + '</span>' +
        '<span class="exp-main">无近期到期批次</span></div>';
    }
    // 与 expDaysLeft 同口径：按本地自然日算，而不是对精确时间戳 ceil（后者会把
    // 明天/今天各多算一天，日均需耗也随之少一半）。
    const dt = new Date(ms);
    const p2 = n => String(n).padStart(2, '0');
    const when = dt.getFullYear() + '-' + p2(dt.getMonth() + 1) + '-' + p2(dt.getDate());
    const days = Math.max(0, Math.round((new Date(when + 'T00:00:00') - today) / 86400000));
    const remain = Number(a.credits_earliest_remaining || 0);
    const daily = Math.ceil(remain / Math.max(1, days));
    const cls = days <= 3 ? 'var(--bad)' : days <= 7 ? 'var(--warn)' : 'var(--ok)';
    const dayWord = days === 0 ? '今天到期' : days === 1 ? '明天到期' : days + ' 天后到期';
    return '<div class="exp-row"><span class="exp-dot" style="background:' + cls + '"></span>' +
      '<span class="exp-nm">' + nm + '</span>' +
      '<span class="exp-main">最近到期 <b>' + esc(when) + '</b>（' + dayWord +
      '）· 该批 <b>' + fmtTok(remain) + '</b> 积分 · 到期前日均需耗 ≥<b>' +
      fmtTok(daily) + '</b>' +
      '<div class="note">仅最早一批，点「检查」看全部到期批次</div>' +
      '</span></div>';
  }).join('');
  $('expList').innerHTML = rows || '<div class="empty">没有账号</div>';
  // 数据新鲜度由上游的 snapshotNoteText 统一标注；分页与可见性仍归本地 tab 逻辑。
  $('expNote').textContent = snapshotNoteText(list.length, d);
  if ($('expCount')) $('expCount').textContent = '共 ' + expAll.length + ' 个';
  if ($('expPageInfo')) $('expPageInfo').textContent = expPage + ' / ' + pages;
  if ($('expPrev')) $('expPrev').disabled = expPage <= 1;
  if ($('expNext')) $('expNext').disabled = expPage >= pages;
  if (typeof expBoxShow === 'function') expBoxShow();
  else $('expBox').hidden = false;
}

// snapshotNoteText 快照卡片的数据新鲜度标注。界面本身 5 秒轮询一次，但卡片数据
// 只随余额后台刷新更新（默认每 5 分钟）——不标出来的话，5 秒级的界面刷新会让人
// 误以为数据也是 5 秒级新鲜。放在 renderExpirySnapshot 之后：前端 harness 按
// 「renderExpirySnapshot 起」切片求值，函数声明提升让它在同一切片内可用。
function snapshotNoteText(count, d) {
  const iv = Number(d && d.balance_refresh_sec) || 0;
  const at = d && d.balance_refreshed_at ? new Date(d.balance_refreshed_at).getTime() : 0;
  if (iv <= 0) {
    return count + ' 个账号 · 快照（来自本地状态 · 未开启后台刷新，逐包明细见账号行「积分」按钮）';
  }
  const every = iv % 60 === 0 ? iv / 60 + ' 分钟' : iv + ' 秒';
  let tail = '每 ' + every + '后台刷新';
  if (at && !isNaN(at)) {
    const min = Math.floor((Date.now() - at) / 60000);
    tail += '，' + (min < 1 ? '刚刚更新' : min + ' 分钟前更新');
  }
  return count + ' 个账号 · 快照（' + tail + '，逐包明细见账号行「积分」按钮）';
}

async function loadExpiry(force) {
  if (!$('expBox')) return;
  if (expFetching) return;
  const fresh = lastPackages && (Date.now() - lastPackagesAt) < EXP_FRESH_MS;
  if (fresh && !force) { renderExpiry(lastPackages); return; }
  expFetching = true;
  $('expBox').hidden = false;
  if (!$('expList').children.length) $('expList').innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  $('expNote').textContent = '查询中…';
  try {
    const d = await api('packages');
    lastPackages = d;                            // 与「积分构成」视图共用同一份缓存
    lastPackagesAt = Date.now();
    renderExpiry(d);
  } catch (e) {
    $('expNote').textContent = '查询失败：' + esc(e.message);
  }
  expFetching = false;
}


// 「刷新」始终实时查（显式 force，不依赖事件对象真值）
if ($('btnPk')) $('btnPk').onclick = () => loadPackages(true);

/* ── 单号积分构成弹窗（packages?uid=，只查 1 个账号 1 次上游）────────────── */
let pkUID = null;

function openPackages(uid) {
  pkUID = String(uid || '');
  $('pkWho').textContent = pkUID.slice(0, 16);
  $('pkVeil').classList.add('on');
  loadPackageDetail();
}
function closePackages() { $('pkVeil').classList.remove('on'); pkUID = null; }
if ($('btnPkClose')) $('btnPkClose').onclick = closePackages;
if ($('btnPkReload')) $('btnPkReload').onclick = loadPackageDetail;

async function loadPackageDetail() {
  if (!pkUID) return;
  const st = $('pkState'), body = $('pkBody');
  st.hidden = false;
  st.className = 'state';
  st.innerHTML = '<span class="dots">查询中</span>';
  body.innerHTML = '';
  try {
    // 折叠条数复用配置里的 package_detail_limit；config 拉不到就用默认值。
    const [d, c] = await Promise.all([
      api('packages?uid=' + encodeURIComponent(pkUID)),
      api('config').catch(() => null),
    ]);
    const a = (d.accounts || [])[0];
    if (!a) throw new Error('该账号无数据');
    st.hidden = true;
    body.innerHTML = packageDetailHtml(a, pkDetailLimit(c && c.config));
  } catch (e) {
    st.hidden = false;
    st.className = 'state err';
    st.textContent = '查询失败：' + e.message;
  }
}

// packageDetailHtml 渲染单账号卡片 + 逐包明细表（数据来自 packages?uid=）。
function packageDetailHtml(a, detailLimit) {
  const now = Date.now();
  if (a.error) {
    return '<div class="empty">查询失败：' + esc(a.error) + '</div>';
  }
  const srcs = pkBySource(a.packages || []);
  const total = Math.max(1, Number(a.size || 0));
  const bar = srcs.map((s, i) =>
    '<i style="width:' + (s.size / total * 100).toFixed(2) + '%;background:' +
    pkColor(i) + '" title="' + esc(s.name) + ' ' + fmtTok(s.size) + '"></i>'
  ).join('');
  const legend = srcs.map((s, i) =>
    '<span><i style="background:' + pkColor(i) + '"></i>' +
    esc(s.name.replace(/^CodeBuddy/, '')) + ' x' + s.n + ' · ' + fmtTok(s.size) +
    (s.minCreated ? ' · 首发 ' + esc(s.minCreated.slice(5)) : '') + '</span>'
  ).join('');
  const expiry = pkAccountSegments(a, now);
  const expiryTotal = Math.max(1, expiry.reduce((sum, s) => sum + s.amount, 0));
  const expiryColor = pkAccountColorMap([a]).get(String(a.uid)) || 'var(--accent)';
  const expiryBar = expiry.length ? '<div class="expirybar" role="img" aria-label="积分到期分布">' +
    expiry.map(s => {
      const title = s.source + '\n' + fmtTok(s.amount) + ' 积分\n到期时间 ' +
        pkExpiryDateTime(s.expiresAt) + '（' + pkExpiryText(s.expiresAt) + '）';
      return '<i style="background:' + expiryColor +
        ';opacity:' + pkCreditOpacity(s.days).toFixed(5) +
        ';flex:' + Math.max(0.008, s.amount / expiryTotal).toFixed(4) +
        ' 1 0" title="' + esc(title) + '"></i>';
    }).join('') + '</div>' : '';
  const card = '<div class="pk-grid" style="margin-bottom:10px"><div class="pk-card">' +
    '<div class="who"><span class="nm">' + esc(a.nickname || String(a.uid || '').slice(0, 8)) + '</span>' +
    '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
    '<div class="big">' + fmtTok(a.remain) + '</div>' +
    '<div class="sub">共 ' + fmtTok(a.size) + '（已用 ' +
    fmtTok(Math.max(0, Number(a.size || 0) - Number(a.remain || 0))) + '）· ' +
    (a.packages || []).length + ' 个包</div>' +
    '<div class="mixbar">' + bar + '</div>' +
    expiryBar +
    '<div class="pk-legend">' + legend + '</div>' +
    '</div></div>';

  // 逐包明细：默认展示最早到期 N 条（到期升序），「其余未用完」与「已用完」折叠。
  const groups = pkDetailGroups(a.packages || [], detailLimit);
  const colorOf = k => pkColor(srcs.findIndex(s => s.key === k));
  const subOf = p => (p.sub_product_code || '').replace(/^sp_tcaca_codebuddyide_?/, '') ||
    (p.package_code || '').replace(/^TCACA_/, '');
  const rowOf = (p, rowGroup) =>
    '<tr' + (rowGroup ? ' class="pk-hidden-row pk-' + rowGroup +
      '-row" data-pk-row="' + rowGroup + '" hidden' : '') +
      '><td class="mark" aria-hidden="true"><i style="background:' +
      colorOf((p.package_code || '') + '|' + (p.name || '(未命名)')) + '"></i></td>' +
    '<td>' + esc(p.name || '(未命名)') +
      (subOf(p) ? '<div class="note">' + esc(subOf(p)) + '</div>' : '') + '</td>' +
    '<td class="num">' + fmtTok(p.size) + '</td>' +
    '<td class="num">' + fmtTok(p.remain) + '</td>' +
    '<td class="num">' + fmtTok(p.used) + '</td>' +
    '<td class="num">' + esc((p.created_at || '').slice(0, 16).replace('T', ' ') || '—') + '</td>' +
    '<td class="num">' + esc((p.end_time || '').slice(0, 10) || '—') + '</td>' +
    '</tr>';
  const groupSummary = (group, label, count, size, remain) =>
    '<tr class="pk-group-summary"><td colspan="7"><button type="button" class="pk-group-toggle"' +
    ' data-pk-group="' + group + '" data-count="' + count + '" data-size="' + size +
    '" data-remain="' + remain + '" aria-expanded="false">' + label + '，展开</button></td></tr>';
  const rows = groups.visible.map(p => rowOf(p, '')).join('');
  const restSummary = groups.rest.length
    ? groupSummary('rest', '其余未用完 ' + groups.rest.length + ' 个包（面额合计 ' +
        fmtTok(groups.restSize) + ' · 剩余 ' + fmtTok(groups.restRemain) + '）',
        groups.rest.length, groups.restSize, groups.restRemain) +
      groups.rest.map(p => rowOf(p, 'rest')).join('')
    : '';
  const usedSummary = groups.used.length
    ? groupSummary('used', '已用完 ' + groups.used.length + ' 个包（面额合计 ' +
        fmtTok(groups.usedSize) + '）', groups.used.length, groups.usedSize, 0) +
      groups.used.map(p => rowOf(p, 'used')).join('')
    : '';
  return card + '<div class="box"><header><h3>逐包明细</h3><span class="grow"></span>' +
    '<span class="note">余额 ' + fmtTok(a.remain) + ' / 总额 ' + fmtTok(a.size) +
    ' · 可用 ' + (groups.visible.length + groups.rest.length) + ' 个包' +
    (groups.used.length ? ' / 已用完 ' + groups.used.length + ' 个' : '') +
    ' · 默认展示最早到期 ' + pkDetailLimitValue(detailLimit) + ' 条（按到期升序）</span>' +
    '</header><div class="tbl-wrap"><table class="acc"><thead><tr>' +
    '<th class="mark" aria-hidden="true"></th><th>包名 / 来源</th>' +
    '<th class="num">面额</th><th class="num">剩余</th><th class="num">已用</th>' +
    '<th class="num">发放</th><th class="num">到期</th>' +
    '</tr></thead><tbody>' + rows + restSummary + usedSummary + '</tbody></table></div></div>';
}

// 折叠分组：展开 / 收起「其余未用完」「已用完」（事件委托，弹窗重渲染后依然有效）。
if ($('pkBody')) $('pkBody').addEventListener('click', ev => {
  const btn = ev.target.closest('button[data-pk-group]');
  if (!btn) return;
  const body = btn.closest('tbody');
  if (!body) return;
  const group = btn.dataset.pkGroup;
  const expanded = btn.getAttribute('aria-expanded') === 'true';
  body.querySelectorAll('tr[data-pk-row="' + group + '"]').forEach(row => { row.hidden = expanded; });
  const count = btn.dataset.count || '0';
  const size = btn.dataset.size || '0';
  const remain = btn.dataset.remain || '0';
  btn.setAttribute('aria-expanded', String(!expanded));
  if (group === 'rest') {
    btn.textContent = expanded
      ? '其余未用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + ' · 剩余 ' +
        fmtTok(remain) + '），展开'
      : '收起其余未用完 ' + count + ' 个包';
  } else {
    btn.textContent = expanded
      ? '已用完 ' + count + ' 个包（面额合计 ' + fmtTok(size) + '），展开'
      : '收起已用完 ' + count + ' 个包';
  }
});

/* ══ 账号备注（只写本机 localStorage；账号池 / 用量两个视图共用同一份映射）══════
   键取 uid 前 8 位——与 renderUsageDim 账号维度的 key 和积分扣除表的 uid 前缀同源，
   三个表格因此都能直接 join。展示层只替换文本并挂 .rm-ghost 虚体，账号标识本身
   仍由服务端下发，不写回后端；备注是纯展示层信息，不参与选号 / 计费 / 鉴权。 */
function remarksAll() {
  if (remarksCache) return remarksCache;
  try { remarksCache = JSON.parse(localStorage.getItem(LS_REMARKS)) || {}; }
  catch (e) { remarksCache = {}; }
  return remarksCache;
}
function remarkKey(uid) { return String(uid == null ? '' : uid).slice(0, 8); }
function remarkOf(uid) {
  const k = remarkKey(uid);
  return k ? String(remarksAll()[k] || '') : '';
}
/* nameHtml 必须是已转义的 HTML；有备注时返回「备注 + 原标识（浅色虚体）」 */
function remarkName(uid, nameHtml) {
  const r = remarkOf(uid);
  return r ? esc(r) + '<span class="rm-ghost">' + nameHtml + '</span>' : nameHtml;
}
function storeRemarks(map) {
  remarksCache = map;
  try { localStorage.setItem(LS_REMARKS, JSON.stringify(map)); } catch (e) {}
}

/* 编辑器的行来自账号全集（poolAccountList：完整 overview 快照优先，退当前页），
   另外补上「已有备注但当前列表里没有」的键，避免账号被移除后备注看起来凭空消失。 */
function remarkFormRows() {
  const rows = poolAccountList()
    .map(s => ({ uid: remarkKey(s.uid), who: s.nickname || remarkKey(s.uid) }))
    .filter(r => r.uid);
  for (const k of Object.keys(remarksAll())) {
    if (!rows.some(r => r.uid === k)) rows.push({ uid: k, who: k });
  }
  return rows;
}
function renderRemarkBody() {
  const box = $('remarkBody');
  if (!box) return;
  const rows = remarkFormRows();
  box.innerHTML = rows.length
    ? rows.map(r => '<div class="rm-row"><span class="rm-who" title="' + esc(r.who) + '">' + esc(r.who) + '</span>' +
        '<input type="text" data-uid="' + esc(r.uid) + '" value="' + esc(remarksAll()[r.uid] || '') + '" placeholder="备注（留空即清除）" maxlength="24"></div>').join('')
    : '<div class="empty">暂无账号：先在「账号池」加载一次账号列表。</div>';
}
function openRemarks() {
  renderRemarkBody();
  if ($('remarkVeil')) $('remarkVeil').classList.add('on');
}
function closeRemarks() { if ($('remarkVeil')) $('remarkVeil').classList.remove('on'); }
function saveRemarkForm() {
  const map = {};
  document.querySelectorAll('#remarkBody input[data-uid]').forEach(inp => {
    const v = inp.value.trim();
    if (v) map[inp.getAttribute('data-uid')] = v;
  });
  storeRemarks(map);
  closeRemarks();
  // 备注只影响本机展示：用现有 rows 重渲染当前视图即可，无需后端参与（也不发请求）。
  if (view === 'accounts') renderAccounts(accAll, accPageMeta);
  else if (view === 'usage' && usageData) renderUsage(usageData);
  toast('备注已保存（仅本机浏览器）');
}
if ($('btnRemarks')) $('btnRemarks').onclick = openRemarks;
if ($('btnRemarkClose')) $('btnRemarkClose').onclick = closeRemarks;
if ($('btnRemarkClear')) $('btnRemarkClear').onclick = () => { storeRemarks({}); renderRemarkBody(); };
if ($('btnRemarkSave')) $('btnRemarkSave').onclick = saveRemarkForm;

/* ══ 首页「积分到期提醒」折叠（选择记在本机，默认展开）═════════════════════
   展开态是原行为：点「检查」就要能立刻看到结果，所以默认不折叠；用户收起一次后
   由 localStorage 记住。折叠只作用于列表容器，卡片标题与「检查」按钮始终可见。 */
function expFolded() { return localStorage.getItem(LS_EXP_FOLD) === '1'; }
function applyExpFold() {
  const pad = $('expPad'), btn = $('btnExpFold');
  if (!pad || !btn) return;
  const folded = expFolded();
  pad.hidden = folded;
  btn.textContent = folded ? '展开' : '收起';
  btn.setAttribute('aria-expanded', folded ? 'false' : 'true');
}
if ($('btnExpFold')) {
  $('btnExpFold').onclick = () => {
    try { localStorage.setItem(LS_EXP_FOLD, expFolded() ? '0' : '1'); } catch (e) {}
    applyExpFold();
  };
}
applyExpFold();
