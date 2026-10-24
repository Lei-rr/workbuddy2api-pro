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
   放后段不会踩 TDZ；保持该顺序可避免初始化前访问。 */
/* 视图级筛选状态（模块级声明放在文件顶部，避免顶层 go() 早于声明执行时踩 TDZ）。 */
let mdFilter = { q: '', realm: '', cap: '', effort: '', promo: '', sort: 'default' };
let mdAll = [], mdProbes = {}, mdProbeOf = () => undefined;
let mdLocks = [], mdRecent = [];
let reqFilter = { q: '', outcome: '' };
let reqEntries = [];
let reqPage = 1, reqPageSize = 100;           // 客户端分页（0 = 全部）
let usDim = 'account', usCreditDim = 'account', usSort = 'total';
let usageData = null;

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
/* API_TIMEOUT_MS 常规 JSON API 的超时（15s）。SSE 是长连接，单独用 fetch+流读取，
   不套这个超时。超时按普通请求失败处理：调用方 toast，轮询按退避继续。 */
const API_TIMEOUT_MS = 15000;
/* fetchTimed 给 fetch 加一次性超时（无 AbortController 的极简环境退化为原 fetch）。 */
function fetchTimed(url, opts, timeoutMs) {
  const ms = Number(timeoutMs) > 0 ? Number(timeoutMs) : API_TIMEOUT_MS;
  if (typeof AbortController !== 'function' || typeof setTimeout !== 'function') {
    return fetch(url, opts);
  }
  const ac = new AbortController();
  const merged = Object.assign({}, opts || {}, { signal: ac.signal });
  const timer = setTimeout(() => { try { ac.abort(); } catch (e) { /* 请求已结束 */ } }, ms);
  return fetch(url, merged).then(
    r => { clearTimeout(timer); return r; },
    e => { clearTimeout(timer); throw e; });
}
function apiTimeoutError() {
  const e = new Error('请求超时（' + Math.round(API_TIMEOUT_MS / 1000) + 's），请检查网络或反向代理');
  e.name = 'TimeoutError';
  return e;
}
function isRequestAbort(e) { return !!e && (e.name === 'AbortError' || e.name === 'TimeoutError'); }

/* REQ_SEQ 同一资源的并发请求序号：只允许最后发出的请求写回 UI。
   账号页历史上有专用的 accReqSeq；其余视图统一走这里，避免"先发后到"覆盖新数据。 */
const REQ_SEQ = Object.create(null);
function reqSeqNext(key) { REQ_SEQ[key] = (REQ_SEQ[key] || 0) + 1; return REQ_SEQ[key]; }
function reqSeqStale(key, seq) { return REQ_SEQ[key] !== seq; }

async function api(path, opts = {}) {
  const h = Object.assign({}, opts.headers || {});
  const k = localStorage.getItem(LS_KEY);
  if (k) h['Authorization'] = 'Bearer ' + k;
  if (opts.body) h['Content-Type'] = 'application/json';
  let r;
  try {
    r = await fetchTimed('/panel/api/' + path, Object.assign({}, opts, { headers: h }));
  } catch (e) {
    if (isRequestAbort(e)) throw apiTimeoutError();
    throw e;
  }
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

/* ── 弹层可访问性（v2.6.0）──────────────────────────────────────────────
   HTML 侧给 .veil 补 role=dialog/aria-modal/aria-labelledby；这里只做行为：
   打开时焦点移入弹窗、Tab 在弹窗内循环、Esc 关闭（登录层 data-lock 除外）、
   关闭后焦点归还触发元素。用属性观察器集中处理，避免改 6 个 open/close 函数。 */
const FOCUSABLE_SEL = 'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])';
function veilFocusables(root) {
  if (!root || typeof root.querySelectorAll !== 'function') return [];
  return Array.prototype.slice.call(root.querySelectorAll(FOCUSABLE_SEL)).filter(el => el && !el.hidden);
}
/* trapNextIndex 纯函数：Tab/Shift+Tab 在 [0,len) 内循环（便于独立测试）。 */
function trapNextIndex(len, index, shift) {
  if (!(Number(len) > 0)) return -1;
  const i = Number.isFinite(Number(index)) ? Number(index) : -1;
  if (shift) return i <= 0 ? Number(len) - 1 : i - 1;
  return i >= Number(len) - 1 ? 0 : i + 1;
}
function openVeilList() {
  if (!document || typeof document.querySelectorAll !== 'function') return [];
  return Array.prototype.slice.call(document.querySelectorAll('.veil.on'));
}
let veilFocusReturn = null;
function veilOnKeydown(ev) {
  if (!ev) return;
  const veils = openVeilList();
  if (!veils.length) return;
  const top = veils[veils.length - 1];
  if (ev.key === 'Tab') {
    const dlg = top.querySelector ? top.querySelector('.dlg') : null;
    const list = veilFocusables(dlg);
    if (!list.length) return;
    const active = (typeof document !== 'undefined' && document.activeElement) || null;
    const idx = list.indexOf(active);
    const from = idx >= 0 ? idx : (ev.shiftKey ? 0 : -1);
    const next = trapNextIndex(list.length, from, !!ev.shiftKey);
    if (next >= 0 && list[next] && list[next].focus) {
      ev.preventDefault();
      try { list[next].focus(); } catch (e) { /* 焦点目标已卸载 */ }
    }
    return;
  }
  if (ev.key !== 'Escape') return;
  // 登录层（data-lock）必须输入密钥，Esc 不关闭。
  if (top.getAttribute && top.getAttribute('data-lock') === '1') return;
  ev.preventDefault();
  if (top.id === 'remarkVeil') { closeRemarks(); return; }
  if (top.id === 'addVeil') { closeAdd(); return; }
  if (top.id === 'taskVeil') { closeTasks(); return; }
  if (top.id === 'pkVeil') { closePackages(); return; }
  top.classList.remove('on');   // vcVeil 等只有显隐的弹层
}
if (document && typeof document.addEventListener === 'function') document.addEventListener('keydown', veilOnKeydown);
/* veils 的 class 变化 → 焦点进入/归还（只观察 6 个固定弹层，成本可忽略）。 */
function veilFocusManage() {
  if (!document || typeof document.querySelectorAll !== 'function' || typeof MutationObserver !== 'function') return;
  const obs = new MutationObserver(muts => {
    for (const m of muts) {
      const el = m.target;
      if (!el || !el.classList) continue;
      const open = el.classList.contains('on');
      if (open && el.dataset && el.dataset.a11yOpen !== '1') {
        el.dataset.a11yOpen = '1';
        veilFocusReturn = (typeof document !== 'undefined' && document.activeElement) || null;
        const dlg = el.querySelector ? el.querySelector('.dlg') : null;
        const list = veilFocusables(dlg);
        const first = list[0] || (dlg && dlg.focus ? dlg : null);
        if (first && first.focus) setTimeout(() => { try { first.focus(); } catch (e) { /* 已关闭 */ } }, 0);
      } else if (!open && el.dataset && el.dataset.a11yOpen === '1') {
        el.dataset.a11yOpen = '';
        const back = veilFocusReturn;
        veilFocusReturn = null;
        if (back && back.focus) setTimeout(() => { try { back.focus(); } catch (e) { /* 已卸载 */ } }, 0);
      }
    }
  });
  document.querySelectorAll('.veil').forEach(v => obs.observe(v, { attributes: true, attributeFilter: ['class'] }));
}
veilFocusManage();
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

