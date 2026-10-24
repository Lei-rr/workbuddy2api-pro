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
      // 频道来自后端枚举；未知值只接受白名单 class，显示文案回退「系统」，
      // 不让异常频道名进入 innerHTML 文本位置（XSS 兜底）。
      const chKey = String((e && e.ch) || '');
      const chSafe = /^(task|chat|sys)$/.test(chKey) ? chKey : 'sys';
      const chName = { task: '任务', chat: '对话', sys: '系统' }[chSafe];
      const ch = logCh === 'all' ? '<i class="lch c-' + esc(chSafe) + '">' + esc(chName) + '</i>' : '';
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

/* LOGS_HEAVY_TTL_MS 运行日志页「重数据」刷新间隔（v2.6.0）：
   请求指标 / 请求归档 / 积分历史属于大响应（JSONL 扫描、数百 KB），不再跟随 5s
   轮询——首次进入、用户点刷新/改时间范围立即拉；其余时间最多 30s 一次。
   轻量 /logs（内存日志环）仍每轮刷新，日志框与计数不会空。 */
const LOGS_HEAVY_TTL_MS = 30000;
let logsHeavyAt = 0;

/* loadLogs(forceHeavy)：forceHeavy=true 强制重拉重数据（用户操作/进入页面）。 */
async function loadLogs(forceHeavy) {
  const seq = reqSeqNext('logs');
  const box = $('logBox');
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  const limit = ($('reqLimit') && $('reqLimit').value) || 100;
  // 时间范围由归档侧过滤（不是前端筛已拉取的条目）：区间落在更早的时间段时，
  // 「最近 N 条」里根本不会有那些记录，必须让服务端按时间取。
  const rq = trangeQuery('reqRange', false);
  rq.set('limit', limit);
  // 积分历史账号下拉的候选来自账号池；这里同步一次（只在账号集合变化时重建 DOM）。
  syncCreditAccounts();
  const heavyDue = forceHeavy === true || !logsHeavyAt || (Date.now() - logsHeavyAt) >= LOGS_HEAVY_TTL_MS;
  try {
    const d = await api('logs');
    if (reqSeqStale('logs', seq)) return true;
    let metrics = null, requestRows = { entries: [] }, credit = null;
    if (heavyDue) {
      [metrics, requestRows, credit] = await Promise.all([
        api('request_metrics').catch(() => ({})),
        api('request_logs?' + rq.toString()).catch(() => ({ entries: [] })),
        api(creditHistoryQuery()).catch(() => null),
      ]);
      if (reqSeqStale('logs', seq)) return true;
      logsHeavyAt = Date.now();
      // 归档开启时以归档为准——「区间内没有记录」是一个真实结果，不能回落成内存里
      // 的最近 100 条（那会把筛选条件之外、时间范围之外的请求显示出来）。
      // 只有归档关闭时才回落到内存指标，保证没有归档的部署仍能看到最近请求。
      const archiveOn = !!(metrics && metrics.archive && metrics.archive.enabled);
      const recent = archiveOn ? (requestRows.entries || []) : (metrics.recent || []);
      renderRequestMetrics(metrics, recent);
      renderCreditHistory(credit);
    }
    logAll = d.entries || [];
    renderLogBox(logPin && atEnd);
    const counts = {};
    for (const e of logAll) counts[e.ch] = (counts[e.ch] || 0) + 1;
    $('logNote').textContent = logCh === 'all'
      ? '任务 ' + (counts.task || 0) + ' · 对话 ' + (counts.chat || 0) + ' · 系统 ' + (counts.sys || 0)
      : (logCh === 'task' ? '任务' : logCh === 'chat' ? '对话' : '系统') + ' ' + filterLogs(logAll, logCh, '').length + ' 行';
    return true;   // 轮询据此判断本轮成功/失败（失败才退避）
  } catch (e) {
    if (reqSeqStale('logs', seq)) return true;
    return false;
  }
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
/* creditEntryText 组合文本（渲染链路与对抗测试共享）：时间 | 账号 | 变动 | 余额 | 描述。
   保留独立函数是有意的对外契约（credit_frontend_adversarial_test.go 直接断言脏时间回退）。 */
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
  // 直接调用本函数的环境可能没有模块级分页状态 /
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

/* ── 操作审计 / 任务执行历史（v2.2.0 #5/#9，只读、点击拉取）───────────────
   两个区块都**不轮询**：用户点「加载」才请求一次，不给面板加常驻流量。
   后端契约（v2.2.0，字段名以 backend-c 最终为准，这里全部做宽松兜底）：
     GET /panel/api/audit?limit=200     → {ok, entries:[{ts,ip,action,uid,ok,detail}]}
     GET /panel/api/task_runs?limit=100 → {ok, runs:[{id,kind,trigger,started_at,
                                             finished_at,total,done,fail,skip,error}]}
   渲染函数是纯函数（只吃数组 + 写固定 DOM 节点），便于独立测试。 */
const AUDIT_LIMIT = 200;
const TASK_RUNS_LIMIT = 100;
let auditData = [], taskRunsData = [];

/* 审计动作枚举与 backend-c 的 audit.go 常量对齐（未知值原样显示，便于后端扩展）。 */
const AUDIT_ACTION_LABEL = {
  login_start: '获取授权链接', login: '添加账号', import: '导入账号',
  account_remove: '移除账号', account_disable: '禁用账号', account_revive: '解冻账号',
  account_pause: '暂停选号', account_resume: '恢复选号',
  account_checkin: '单号签到', account_balance: '单号刷新余额',
  accounts_rescan: '重新扫描',
  task_accept: '接受任务', task_accept_all: '全部接受任务', task_claim: '领取任务',
  task_auto: '一键完成任务', task_auto_all: '一键完成全部任务',
  tasks_run: '单号做任务', tasks_run_queue: '执行任务队列',
  checkin_all: '全部签到', travel_all: '旅行巡检', activity_all: '活跃上报',
  keepalive_all: '全部保活', balance_all: '刷新全部余额',
  usage_save: '保存用量数据', config_save: '保存配置', config_rollback: '回滚配置',
};
const TASK_KIND_LABEL = {
  checkin: '签到', travel: '旅行', activity: '活跃上报', keepalive: '保活',
  balance: '刷新余额', tasks_run: '单号做任务', catch_up: '补跑',
  growth: '成长任务', task_sweep: '任务补领',
};
const TASK_TRIGGER_LABEL = { manual: '手动', schedule: '定时', catch_up: '补跑', startup: '启动' };

/* fmtRunTime 时间戳 → 本地「MM-DD HH:mm:ss」：兼容 RFC3339 字符串、unix 秒/毫秒
   与零值/脏数据（历史 JSONL 里可能有缺失或空 time），非法一律显示「—」。 */
function fmtRunTime(v) {
  if (v == null || v === '') return '—';
  let ms;
  if (typeof v === 'number') {
    ms = v > 1e12 ? v : v * 1000;
  } else {
    const s = String(v).trim();
    if (!s) return '—';
    if (/^\d+(\.\d+)?$/.test(s)) { const n = Number(s); ms = n > 1e12 ? n : n * 1000; }
    else ms = Date.parse(s);
  }
  if (!isFinite(ms) || ms <= 0) return '—';
  const d = new Date(ms);
  if (isNaN(d.getTime())) return '—';
  const p2 = n => String(n).padStart(2, '0');
  return p2(d.getMonth() + 1) + '-' + p2(d.getDate()) + ' ' +
    p2(d.getHours()) + ':' + p2(d.getMinutes()) + ':' + p2(d.getSeconds());
}
function numOr0(v) { const n = Number(v); return Number.isFinite(n) ? n : 0; }

/* normalizeAuditEntries 审计条目兜底：字段别名（ts/time、ip/client_ip、
   action/act、detail/message/error）+ 缺失 + ok 非布尔都容忍。ok 缺失时保留
   null（界面显示「—」），不武断当成成功。 */
function normalizeAuditEntries(d) {
  const list = (d && (d.entries || d.items || d.rows)) || [];
  if (!Array.isArray(list)) return [];
  return list.map(raw => {
    const e = raw || {};
    const okRaw = e.ok !== undefined ? e.ok : e.success;
    const pick = (a, b, c) => (a !== undefined && a !== null) ? a : ((b !== undefined && b !== null) ? b : c);
    return {
      ts: pick(e.ts, e.time, ''),
      ip: String(pick(e.ip, e.client_ip, '')),
      action: String(pick(e.action, e.act, '')),
      uid: String(pick(e.uid, '', '')),
      ok: (okRaw === undefined || okRaw === null) ? null : !!okRaw,
      detail: String(pick(e.detail, e.message, pick(e.error, '', ''))),
    };
  });
}
function auditActionText(action) {
  const code = String(action || '');
  return AUDIT_ACTION_LABEL[code] || code || '—';
}
function auditUidText(uid) {
  const s = String(uid || '');
  if (!s) return '—';
  return s.length > 16 ? s.slice(0, 16) + '…' : s;
}
/* auditRowHtml 单行渲染（纯函数，便于测试列/标签/转义）：时间/IP/动作/账号/结果/详情 */
function auditRowHtml(e) {
  const result = e.ok === null ? '<span class="tag mute">—</span>'
    : (e.ok ? '<span class="tag ok">成功</span>' : '<span class="tag bad">失败</span>');
  return '<tr>' +
    '<td>' + esc(fmtRunTime(e.ts)) + '</td>' +
    '<td>' + esc(e.ip || '—') + '</td>' +
    '<td title="' + esc(e.action || '') + '">' + esc(auditActionText(e.action)) + '</td>' +
    '<td>' + esc(auditUidText(e.uid)) + '</td>' +
    '<td>' + result + '</td>' +
    '<td title="' + esc(e.detail || '') + '">' + esc(e.detail || '—') + '</td>' +
    '</tr>';
}
function renderAudit(entries) {
  auditData = Array.isArray(entries) ? entries : [];
  const body = $('auditBody'), table = $('auditTable'), state = $('auditState'), note = $('auditNote');
  if (!body) return;
  if (table) table.hidden = !auditData.length;
  if (note) note.textContent = auditData.length ? '最近 ' + auditData.length + ' 条' : '—';
  if (state) {
    state.hidden = auditData.length > 0;
    state.className = 'state';
    state.textContent = auditData.length ? '' : '暂无审计记录（面板写操作成功后会自动留痕）';
  }
  body.innerHTML = auditData.map(auditRowHtml).join('');
}

/* normalizeTaskRuns 任务历史兜底：runs/entries/items 别名 + 字段别名 + 数字缺失。 */
function normalizeTaskRuns(d) {
  const list = (d && (d.runs || d.entries || d.items || d.rows)) || [];
  if (!Array.isArray(list)) return [];
  return list.map(raw => {
    const r = raw || {};
    const pick = (a, b, c) => (a !== undefined && a !== null) ? a : ((b !== undefined && b !== null) ? b : c);
    return {
      id: String(pick(r.id, '', '')),
      kind: String(pick(r.kind, r.task, '')),
      trigger: String(pick(r.trigger, '', '')),
      started_at: pick(r.started_at, r.started, ''),
      finished_at: pick(r.finished_at, r.finished, ''),
      total: numOr0(pick(r.total, r.count, 0)),
      done: numOr0(pick(r.done, r.success, 0)),
      fail: numOr0(pick(r.fail, r.failed, pick(r.error_count, 0, 0))),
      skip: numOr0(pick(r.skip, r.skipped, 0)),
      error: String(pick(r.error, '', '')),
    };
  });
}
function taskKindText(kind) {
  const code = String(kind || '');
  return TASK_KIND_LABEL[code] || code || '—';
}
function taskTriggerText(trigger) {
  const code = String(trigger || '');
  return TASK_TRIGGER_LABEL[code] || code || '—';
}
/* taskRunRowHtml 单行渲染（纯函数）：开始/结束/类型/触发/总数/完成/失败/跳过/错误 */
function taskRunRowHtml(r) {
  // 计数统一 Number() 兜底：后端正常给 number；旧数据/上游字符串不能原样拼 HTML。
  return '<tr>' +
    '<td>' + esc(fmtRunTime(r.started_at)) + '</td>' +
    '<td>' + esc(fmtRunTime(r.finished_at)) + '</td>' +
    '<td title="' + esc(r.kind || '') + '">' + esc(taskKindText(r.kind)) + '</td>' +
    '<td title="' + esc(r.trigger || '') + '">' + esc(taskTriggerText(r.trigger)) + '</td>' +
    '<td class="num">' + numOr0(r.total) + '</td>' +
    '<td class="num">' + numOr0(r.done) + '</td>' +
    '<td class="num">' + numOr0(r.fail) + '</td>' +
    '<td class="num">' + numOr0(r.skip) + '</td>' +
    '<td title="' + esc(r.error || '') + '">' + esc(r.error || '—') + '</td>' +
    '</tr>';
}
function renderTaskRuns(runs) {
  taskRunsData = Array.isArray(runs) ? runs : [];
  const body = $('runsBody'), table = $('runsTable'), state = $('runsState'), note = $('runsNote');
  if (!body) return;
  if (table) table.hidden = !taskRunsData.length;
  if (note) note.textContent = taskRunsData.length ? '最近 ' + taskRunsData.length + ' 条' : '—';
  if (state) {
    state.hidden = taskRunsData.length > 0;
    state.className = 'state';
    state.textContent = taskRunsData.length ? '' : '暂无任务执行记录（批量任务/单号任务结束后自动记录）';
  }
  body.innerHTML = taskRunsData.map(taskRunRowHtml).join('');
}

/* loadAudit / loadTaskRuns 点击才拉一次（不轮询）。失败：行内错误态 + toast，
   按钮恢复可点（可重试）。 */
async function loadAudit() {
  const btn = $('btnAuditLoad'), state = $('auditState');
  if (btn) { btn.disabled = true; btn.textContent = '加载中…'; }
  if (state) { state.hidden = false; state.className = 'state'; state.textContent = '加载中…'; }
  try {
    const d = await api('audit?limit=' + AUDIT_LIMIT);
    renderAudit(normalizeAuditEntries(d));
  } catch (e) {
    const msg = '读取操作审计失败：' + e.message;
    if (state) { state.hidden = false; state.className = 'state err'; state.textContent = msg; }
    toast(msg, 'err');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = '加载'; }
  }
}
async function loadTaskRuns() {
  const btn = $('btnRunsLoad'), state = $('runsState');
  if (btn) { btn.disabled = true; btn.textContent = '加载中…'; }
  if (state) { state.hidden = false; state.className = 'state'; state.textContent = '加载中…'; }
  try {
    const d = await api('task_runs?limit=' + TASK_RUNS_LIMIT);
    renderTaskRuns(normalizeTaskRuns(d));
  } catch (e) {
    const msg = '读取任务执行历史失败：' + e.message;
    if (state) { state.hidden = false; state.className = 'state err'; state.textContent = msg; }
    toast(msg, 'err');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = '加载'; }
  }
}
if ($('btnAuditLoad')) $('btnAuditLoad').onclick = loadAudit;
if ($('btnRunsLoad')) $('btnRunsLoad').onclick = loadTaskRuns;


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
if ($('reqLimit')) $('reqLimit').onchange = () => { reqPage = 1; loadLogs(true); };
if ($('btnReqReload')) $('btnReqReload').onclick = () => { reqPage = 1; loadLogs(true); };
// 时间范围：默认「全部历史」——请求记录页的历史行为就是"取最近 N 条"，
// 加一个默认收窄的区间会让打开页面时看到的条数凭空变少。换区间 → 回第 1 页。
if ($('reqRange')) trangeBind('reqRange', () => { reqPage = 1; loadLogs(true); }, '0');
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

