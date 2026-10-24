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
  if (v === 'logs') loadLogs(true);   // 进入页面强制刷新重数据（指标/归档/积分历史）
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
   兼容：renderAccounts(array) 仍走本地筛选+分页（旧调用）。 */
let expAll = [];                              // 到期快照（按最近到期升序，含无到期账号）
let expPage = 1;
let expPageSize = 100;                        // 0 = 全部
let accAll = [];
// tag 为 v2.3.1 #12 的标签筛选（服务端 accounts?tag=）；空串 = 全部标签。
let accFilters = { q: '', state: '', ver: '', tag: '' };
let accTagOptions = [];                       // [{tag,count}]：标签下拉数据（loadAccTags 填充）
let accPageSize = 100;                        // 0 = 全部
let accPage = 1;

/* ── 分页纯函数（账号池 / 日志 / 请求记录 / 积分历史 / 模型锁池 / 任务队列共用）──
   pageSlice 只做「钳页码 + 切片」，不读写任何模块状态；updatePagerUI 把结果写回
   一组分页控件。抽成纯函数便于独立测试边界（空列表、页码
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
  filters = filters || { q: '', state: '', ver: '', tag: '' };
  const p = new URLSearchParams();
  p.set('page', String(Math.max(1, Math.floor(Number(page) || 1))));
  p.set('page_size', String(Math.max(0, Math.floor(Number(pageSize) || 0))));
  const st = accStateParam(filters.state);
  if (st) p.set('state', st);
  if (filters.q) p.set('q', filters.q);
  // v2.3.1：标签筛选（服务端 tag= 精确匹配）。URLSearchParams 统一编码一次，
  // 调用方不要把 tag 预编码，避免 %E4%B8%AD 被编码成 %25E4...
  if (filters.tag) p.set('tag', filters.tag);
  if (filters.ver === 'enterprise') p.set('enterprise', '1');
  else if (filters.ver === 'global') { p.set('realm', 'global'); p.set('enterprise', '0'); }
  else if (filters.ver === 'cn') { p.set('realm', 'cn'); p.set('enterprise', '0'); }
  return p.toString();
}

/* ── 标签筛选（v2.3.1，服务端 meta/tags）────────────────────────────────
   后端契约：GET /panel/api/meta/tags → {ok,tags:[{tag,count}]}；账号列表接口
   追加 tag= 做服务端精确筛选。标签只是辅助筛选，任何失败都静默降级，不打断
   账号列表/轮询。 */
const ACC_TAG_LIMIT = 50;

/* normalizeAccTags 把 /meta/tags 响应归一化成 [{tag,count}]：兼容数组
   [{tag,count}] / 对象 {tag:count} / 字符串数组；同名合并 count、过滤空标签、
   按 count 降序（同频按 tag 升序），最多展示 50 个。 */
function normalizeAccTags(d) {
  const raw = Array.isArray(d) ? d : ((d && (d.tags || d.items || d.rows)) || []);
  const byTag = new Map();
  const add = (tag, count) => {
    const t = String(tag == null ? '' : tag).trim().slice(0, 32);
    if (!t) return;
    const n = Number(count);
    byTag.set(t, (byTag.get(t) || 0) + (Number.isFinite(n) ? n : 0));
  };
  if (Array.isArray(raw)) {
    for (const item of raw) {
      if (item == null) continue;
      if (typeof item === 'string') add(item, 0);
      else add(item.tag != null ? item.tag : item.name, item.count);
    }
  } else if (raw && typeof raw === 'object') {
    for (const k of Object.keys(raw)) add(k, raw[k]);
  }
  return Array.from(byTag, ([tag, count]) => ({ tag: tag, count: count }))
    .sort((a, b) => (b.count - a.count) || (a.tag < b.tag ? -1 : a.tag > b.tag ? 1 : 0))
    .slice(0, ACC_TAG_LIMIT);
}

/* accTagOptionsFromMeta /meta/tags 不可用时的降级：从 /meta 快照聚合标签频次
   （与 normalizeAccTags 同口径）。metaAll 由备注功能维护。 */
function accTagOptionsFromMeta(meta) {
  const src = meta || metaAll || {};
  const counts = new Map();
  for (const uid of Object.keys(src)) {
    const tags = (src[uid] && src[uid].tags) || [];
    for (const t of tags) counts.set(t, (counts.get(t) || 0) + 1);
  }
  return normalizeAccTags(Array.from(counts, ([tag, count]) => ({ tag: tag, count: count })));
}

/* renderAccTagOptions 填充 #accTagFilter：保留当前选择；所选 tag 不在列表时补一条
   （避免下拉显示与筛选条件不一致）。无数据时只有「全部标签」。 */
function renderAccTagOptions(options) {
  accTagOptions = Array.isArray(options) ? options : [];
  const sel = $('accTagFilter');
  if (!sel) return;
  const cur = String((accFilters && accFilters.tag) || '');
  const opts = ['<option value="">全部标签</option>'];
  let found = false;
  for (const o of accTagOptions) {
    if (o.tag === cur) found = true;
    opts.push('<option value="' + esc(o.tag) + '">' + esc(o.tag) + '（' + o.count + '）</option>');
  }
  if (cur && !found) opts.push('<option value="' + esc(cur) + '">' + esc(cur) + '</option>');
  sel.innerHTML = opts.join('');
  sel.value = cur;
}

/* loadAccTags 拉标签下拉数据。调用点：start（loadMeta 成功后）/备注保存后/
   账号集合变化后——不进 5s 轮询（标签是低频数据）。失败静默降级到 metaAll 聚合，
   再不行保持现有下拉（至少「全部标签」）。 */
async function loadAccTags(quiet) {
  let options = null;
  try {
    const d = await api('meta/tags');
    options = normalizeAccTags(d);
  } catch (e) {
    // 静默降级：后端未就绪/网络失败时用 /meta 快照聚合；不 toast、不影响账号池轮询。
    options = accTagOptionsFromMeta(metaAll);
  }
  renderAccTagOptions(options);
  return options;
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
  let r;
  try {
    r = await fetchTimed('/panel/api/' + path, { headers: h, cache: 'no-store' });
  } catch (e) {
    if (isRequestAbort(e)) throw apiTimeoutError();
    throw e;
  }
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
   抽成纯函数便于独立验证「企业版不出按钮 / 进行中不可重复点」的渲染条件。 */
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
   meta 无 skew/now 时直接用本地时钟（兼容旧调用）。 */
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

/* ACC_MAX_PAGE_SIZE 与服务端 accountsMaxPageSize 对齐：page_size=0（全部）时
   服务端最多返回 2000 行，超出部分前端要明确提示用筛选/分页，避免用户以为
   "全部"就真的是全部（v2.1.0 QA P2）。 */
const ACC_MAX_PAGE_SIZE = 2000;

/* updateAccPagerMeta 服务端分页模式的计数/页码：total = 池总数，
   filtered = 筛选后总数（服务端已算好，前端不再数当前页）。
   shownRows = 本次实际渲染的行数：page_size=0 且 total > shownRows 时说明被服务端
   上限截断，在 accTruncHint 里提示（计数文案保持简洁，移动端不挤爆）。 */
function updateAccPagerMeta(meta, shownRows) {
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
  const shownCount = shownRows == null ? null : Number(shownRows);
  // 「全部」模式的截断只跟筛选后的集合有关（带筛选时 total 是池总数，不能用它判断）。
  const truncated = Number(meta.page_size) === 0 && shownCount !== null &&
    shownCount > 0 && (isNaN(filtered) ? total : filtered) > shownCount;
  if ($('accTruncHint')) {
    $('accTruncHint').hidden = !truncated;
    $('accTruncHint').textContent = truncated
      ? '仅显示前 ' + shownCount + ' 个（服务端上限 ' + ACC_MAX_PAGE_SIZE + '，建议用筛选或分页）'
      : '';
  }
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
      （按数组直接调用的旧路径依赖它，行为保持不变）。 */
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
    // v2.3.1：带标签筛选且池子里有账号时，明确提示「无账号使用该标签」（比笼统的
    // 「没有符合筛选条件的账号」更能定位到是标签条件的问题）。
    const total = Number(meta.total) || 0;
    const tag = String((accFilters && accFilters.tag) || '');
    tb.innerHTML = '<tr><td colspan="9"><div class="empty">' +
      (tag ? '无账号使用该标签「' + esc(tag) + '」'
           : (total > 0 ? '没有符合筛选条件的账号' : '账号池是空的')) + '</div></td></tr>';
    updateAccPagerMeta(meta, 0);
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
    // 数值统一 Number() 兜底：后端正常给 JSON number，但旧数据/上游字符串不能原样
    // 拼进 innerHTML（XSS 兜底），也避免显示 NaN。
    const creditsRaw = (s.credits == null || s.credits === '') ? null : s.credits;
    const creditsNum = numOr0(creditsRaw);
    const creditsTotal = numOr0(s.credits_total);
    const successCount = numOr0(s.success_count);
    const errTotal = numOr0(s.err_total);
    const inFlight = numOr0(s.in_flight);
    const unlimited = creditsTotal === -1;
    const cred = unlimited ? '不限'
      : (creditsRaw == null ? '—' : (creditsTotal > 0 ? creditsNum + '<span class="of">/' + creditsTotal + '</span>' : String(creditsNum)));
    const pct = unlimited ? 100
      : (creditsTotal > 0
        ? Math.min(100, Math.round(creditsNum / creditsTotal * 100))
        : Math.round(creditsNum / maxCred * 100));
    // 成本台账 tooltip（model_costs）：每模型实测单价（≤0 = 实测免费），运维据此
    // 看「为什么总选它」——免费号垄断 / 单价排序一眼可见。
    let credTip;
    if (unlimited) credTip = '企业版不限量（上游 limitNum=-1）';
    else if (creditsTotal > 0) {
      credTip = (s.enterprise ? '企业版剩余额度 ' : '剩余 ')
        + creditsNum + ' / ' + (s.enterprise ? '分配 ' : '总额 ') + creditsTotal + '（' + pct + '%）';
    } else credTip = '积分（相对池内最高）';
    const costs = (s.model_costs || []).filter(c => c.model);
    if (costs.length) {
      credTip += '\n实测单价（credits/1K）：\n' + costs.map(c =>
        '  ' + c.model + '：' + (c.cost_per_1k <= 0 ? '免费' : c.cost_per_1k)).join('\n');
    }
    const frozen = s.disabled || cool > 0;
    const tu = s.token_usage || {};
    const req = numOr0(tu.request_count);
    const tokRaw = tu.total_tokens;
    const tokNum = Number(tokRaw);
    const totalTok = (tokRaw == null || tokRaw === '' || !Number.isFinite(tokNum) || tokNum < 0) ? '—' : fmtTok(tokNum);
    const totalTokUnit = totalTok === '—' ? '' : '<em>tok</em>';
    const latency = fmtMs(tu.last_latency_ms);
    const rate = fmtRate(tu.last_tokens_per_second);
    const usageTitle = '最近一次：' + req + ' 次 / ' + totalTok + ' / 延迟 ' + latency + ' / ' + rate;
    return '<tr class="' + cls + '" title="uid: ' + esc(s.uid) + '">' +
      '<td class="mark" aria-hidden="true"><i></i></td>' +
      '<td class="who"><div class="nm">' + remarkName(s.uid, s.nickname ? esc(s.nickname) : '<span style="color:var(--ink-3)">未命名</span>') + (s.realm === 'global' ? ' <span class="realm-tag">国际版</span>' : '') + (s.enterprise ? ' <span class="realm-tag">企业版</span>' : '') + accTagsHtml(s.uid) + '</div><div class="id">' + esc(short) + '</div></td>' +
      '<td>' + tag + note + rateLimits + '</td>' +
      '<td class="cred" title="' + esc(credTip) + '"><div class="n">' + cred + '</div><div class="bar"><i style="width:' + pct + '%"></i></div></td>' +
      '<td class="num">' + successCount + ' <span style="color:var(--ink-3)">/</span> <span style="color:var(--bad)">' + errTotal + '</span></td>' +
      '<td class="num">' + inFlight + '</td>' +
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
  if (server) updateAccPagerMeta(meta, accAll.length);
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
// v2.3.1 标签筛选：选择后回第 1 页并带 tag= 重新请求（全部标签 = 空值清空筛选）。
if ($('accTagFilter')) $('accTagFilter').addEventListener('change', () => {
  accFilters.tag = $('accTagFilter').value;
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
      const known = STATE[r.state];
      const st = known || ['mute', '—'];
      // 未知 state 只作为转义后的 title 提示，标签 class/文案走白名单（XSS 兜底）。
      const stTitle = known ? '' : ' title="未知状态：' + esc(r.state || '') + '"';
      const realm = r.realm === 'global' ? '国际版' : '国内版';
      return '<tr>' +
        '<td>' + esc(r.model) + '</td>' +
        '<td><span class="realm-tag">' + realm + '</span></td>' +
        '<td><span class="tag ' + st[0] + '"' + stTitle + '>' + esc(st[1]) + '</span></td>' +
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

