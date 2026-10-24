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

/* loadSummary 只拉 lite overview：5s 轮询的常驻请求，~1-2 KB。
   v2.4.0 #3：SSE 已推送 snapshot/accounts 时跳过（统计条由事件增量更新）。 */
async function loadSummary(quiet) {
  if (eventsLive()) return true;
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
    maybeMigrateRemarks();   // 账号列表就位后才有 uid8→全 uid 的解析表，再尝试迁移旧备注
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
   ③ 批量任务进度 jobs（v2.3.0 #10：小响应，复用同一 5s 节奏；失败不影响本视图）；
   ④ 仅当「积分到期」tab 激活时才按 60s 窗口补完整 overview（低频）。
   列表 tab 下不拉全量——这是「账号池不再每 5s 拉 689KB」的落点。 */
async function refreshAccountsView(quiet) {
  // v2.4.0 #3：SSE 已连上并收到数据后，统计条由 snapshot/accounts 事件增量更新，
  // jobs 由 jobs 事件推送——这两路 5s 轮询跳过（避免双拉）；当前页账号行仍按
  // 轮询刷新（SSE 不推逐号行，冷却倒计时依赖它）。
  const live = eventsLive();
  const [summaryOK, pageOK] = await Promise.all([
    live ? Promise.resolve(true) : loadSummary(quiet),
    loadAccountPage(quiet),
  ]);
  if (!live) loadJobs(true);   // 进度条跟随轮询刷新；404/失败静默（不影响账号池刷新与退避）
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

/* refreshFullOverviewInBackground 静默补拉一次完整 overview。
   v2.1.0 QA P2：账号增/删/重扫/登录/导入后，列表 tab 不会主动拉全量；而积分历史
   下拉（poolAccountList）、积分条基准（accCreditsMax）、到期快照都依赖完整
   accounts → 不补拉会一直滞后到切「积分到期」或整页刷新。这里在账号集合变更的
   收尾处后台补一次（force 绕过 60s 窗口），失败/在途都静默忽略，不影响当前视图
   与轮询退避。 */
function refreshFullOverviewInBackground() {
  invalidateFullOverview();
  try { ensureFullOverview(true, true); } catch (e) { /* 静默：补拉失败不影响主流程 */ }
}

/* refreshAccountPoolAfterMutation 账号集合变更（增/删/重扫/导入/登录成功）后的
   统一收尾：先刷新 summary + 当前页，再后台补拉完整 overview（见上）；标签计数
   随账号集合变化，顺带刷新标签下拉（静默）。 */
function refreshAccountPoolAfterMutation(quiet) {
  refreshAccountPool(quiet);
  refreshFullOverviewInBackground();
  loadAccTags(true);
}


/* ── 批量任务进度/取消（v2.3.0 #10：Job Runner）───────────────────────────
   后端实际契约（backend-b task-18 最终版）：
     GET  /panel/api/jobs?limit=20     → {ok,jobs:[Job]}（新在前；limit 缺省 20）
         Job = {id,kind,trigger,state,total,done,fail,skip,started_at,finished_at,
                error,log_tail}；started_at/finished_at 是 Go time.Time 的 RFC3339
                字符串（不是 unix 秒）；state ∈ pending|running|done|failed|canceled|
                interrupted（重启中断，按未知态兜底展示）。
     GET  /panel/api/jobs/{id}         → {ok,job}（UI 未用，列表已够）
     POST /panel/api/jobs/{kind}/run   → {ok,started,job_id}；同 kind 单飞 → 409 {error}
         kind 仅 checkin/travel/activity/keepalive/balance（tasks_run 不可触发，只进历史）
     POST /panel/api/jobs/{id}/cancel  → {ok,canceled:true|false}（false=已结束）
    旧批量端点 checkin_all/... 返回 {ok,started,job_id}（旧字段保留）。
   进度语义：done = 已处理账号（含 fail、不含 skip），total = 触发时池内账号数；
   进度条按 (done+skip)/total 计算「已处理」，失败/跳过并列展示。取消只停后续账号
   （在途请求自然结束），确认弹窗已写明。
   批量按钮仍走旧端点，本段只负责：① 进度条轮询（复用账号池 5s 节奏，模块内并发
   保护）；② 取消；③ toast「已开始（job 进度见顶部）」，409 时原样展示后端 error。
   后端未提供 Job API（404/501）时置 jobsSupported=false 并不再空打，行为退回 v2.2。 */
const JOBS_LIMIT = 20;
const JOB_KIND_LABEL = {
  checkin: '全部签到', travel: '旅行巡检', activity: '活跃上报',
  keepalive: '全部保活', balance: '刷新余额', tasks_run: '单号做任务', catch_up: '补跑任务',
};
const JOB_STATE_LABEL = {
  pending: '排队中', running: '执行中', done: '已完成', failed: '失败',
  canceled: '已取消', interrupted: '已中断',
};
let jobsActive = null;      // 进度条当前展示的 running job
let jobsSupported = true;   // 后端是否提供 Job API（404/501 后置 false）
let jobsLoading = false;    // 一轮 jobs 请求在途（避免叠加）

function normalizeJob(raw) {
  const j = raw || {};
  const num = v => { const n = Number(v); return Number.isFinite(n) ? n : 0; };
  return {
    id: String(j.id != null ? j.id : (j.job_id != null ? j.job_id : '')),
    kind: String(j.kind != null ? j.kind : ''),
    trigger: String(j.trigger != null ? j.trigger : ''),
    state: String(j.state != null ? j.state : ''),
    total: num(j.total),
    done: num(j.done),
    fail: num(j.fail != null ? j.fail : j.failed),
    skip: num(j.skip != null ? j.skip : j.skipped),
    started_at: j.started_at != null ? j.started_at : (j.started != null ? j.started : ''),
    finished_at: j.finished_at != null ? j.finished_at : (j.finished != null ? j.finished : ''),
    error: String(j.error != null ? j.error : ''),
  };
}
function normalizeJobs(d) {
  const list = (d && (d.jobs || d.entries || d.items)) || [];
  return Array.isArray(list) ? list.map(normalizeJob) : [];
}
/* pickActiveJob 只展示 running：pending 还没真正跑、done/failed/canceled 是历史态。 */
function pickActiveJob(jobs) {
  const list = Array.isArray(jobs) ? jobs : [];
  for (const j of list) if (j && j.state === 'running') return j;
  return null;
}
function jobKindText(kind) {
  const code = String(kind || '');
  return JOB_KIND_LABEL[code] || code || '任务';
}
function jobStateText(state) {
  const code = String(state || '');
  return JOB_STATE_LABEL[code] || code || '';
}
function jobTriggerText(trigger) {
  const code = String(trigger || '');
  return { manual: '手动', schedule: '定时', catch_up: '补跑', startup: '启动' }[code] || code || '';
}
/* renderJobBar 把 jobs 列表写成顶部进度条：有 running 显示类型/进度/失败/跳过/取消，
/* renderJobBar 把 jobs 列表写成顶部进度条：有 running 显示类型/进度/失败/跳过/取消，
   否则隐藏（不占布局）。进度口径：done 含失败但不含跳过，所以「已处理」= done+skip
   （进度条宽度用它）；失败/跳过并列显示。started_at 是 RFC3339 字符串，按本地时间展示。 */
function renderJobBar(jobs) {
  jobsActive = pickActiveJob(jobs);
  const bar = $('jobBar');
  if (!bar) return;
  if (!jobsActive) { bar.hidden = true; return; }
  const j = jobsActive;
  bar.hidden = false;
  if ($('jobKind')) $('jobKind').textContent = jobKindText(j.kind) + '（' + jobStateText(j.state) + '）';
  const processed = j.done + j.skip;
  const pct = j.total > 0 ? Math.min(100, Math.round(processed / j.total * 100)) : 0;
  if ($('jobBarFill')) $('jobBarFill').style.width = pct + '%';
  const parts = [processed + ' / ' + j.total];
  if (j.fail) parts.push('失败 ' + j.fail);
  if (j.skip) parts.push('跳过 ' + j.skip);
  if ($('jobProgText')) $('jobProgText').textContent = parts.join(' · ');
  if ($('jobMeta')) {
    const trig = jobTriggerText(j.trigger);
    $('jobMeta').textContent = (trig ? trig + ' · ' : '') +
      '开始 ' + fmtRunTime(j.started_at) + (j.error ? ' · ' + j.error : '');
  }
  if ($('btnJobCancel')) $('btnJobCancel').disabled = j.state !== 'running';
}
/* loadJobs 一轮 jobs 拉取（静默：失败不打断账号池刷新，也不计入轮询退避）。
   v2.4.0 #3：SSE jobs 事件已推送时跳过（断线后自动恢复）。 */
async function loadJobs(quiet) {
  if (eventsLive()) return true;
  if (!jobsSupported || jobsLoading) return false;
  jobsLoading = true;
  try {
    const d = await api('jobs?limit=' + JOBS_LIMIT);
    renderJobBar(normalizeJobs(d));
    return true;
  } catch (e) {
    const msg = String((e && e.message) || '');
    if (/HTTP (404|501)/.test(msg)) {
      // 后端尚未提供 Job API：隐藏进度条并停止空打（旧端点仍可用，行为退回 v2.2）。
      jobsSupported = false;
      renderJobBar([]);
    } else if (!quiet) {
      toast('读取任务进度失败：' + msg, 'err');
    }
    return false;
  } finally {
    jobsLoading = false;
  }
}
/* cancelJob 取消当前 running job：confirm 说明「只停后续账号」，成功/失败都 toast。
   后端返回 {ok,canceled:true|false}：false = 请求到达时任务已结束（无需取消）。 */
async function cancelJob() {
  const j = jobsActive;
  if (!j || !j.id) return;
  if (!confirm('取消「' + jobKindText(j.kind) + '」：只停后续账号，当前正在执行的账号会跑完。确认取消？')) return;
  const btn = $('btnJobCancel');
  if (btn) btn.disabled = true;
  try {
    const r = await api('jobs/' + encodeURIComponent(j.id) + '/cancel', { method: 'POST' });
    if (r && r.canceled === false) toast('任务已结束，无需取消', 'ok');
    else toast('已请求取消：当前账号跑完后停止后续', 'ok');
  } catch (e) {
    toast('取消失败：' + e.message, 'err');
  } finally {
    loadJobs(true);
  }
}
/* runBatchTask 批量端点统一调用：成功 toast 指向顶部进度条；409 等错误原样展示后端文案。 */
async function runBatchTask(path, label, btn) {
  const b = btn || null;
  if (b) b.disabled = true;
  try {
    const r = await api(path, { method: 'POST' });
    if (r && r.ok === false) {
      toast(r.error || r.message || (label + '提交失败'), 'err');
      return;
    }
    toast(label + '已开始（job 进度见顶部）', 'ok');
    loadJobs(true);   // 立刻刷新进度条，不等下一轮轮询
  } catch (e) {
    // 409（同类任务正在执行）等错误：后端 error 文案直接展示。
    toast(label + '失败：' + e.message, 'err');
  } finally {
    if (b) b.disabled = false;
  }
}
if ($('btnJobCancel')) $('btnJobCancel').onclick = cancelJob;
$('btnCheckinAll').onclick = () => runBatchTask('checkin_all', '全部签到', $('btnCheckinAll'));
$('btnKeepaliveAll').onclick = () => runBatchTask('keepalive_all', '全部保活', $('btnKeepaliveAll'));
$('btnTravelAll').onclick = () => runBatchTask('travel_all', '旅行巡检', $('btnTravelAll'));
$('btnActivityAll').onclick = () => runBatchTask('activity_all', '活跃上报', $('btnActivityAll'));


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
  finally {
    b.disabled = false;
    // 移除账号会改变账号集合：除当前页外还要后台补拉完整 overview（积分历史
    // 下拉/积分条基准/到期快照都依赖全量 accounts，v2.1.0 QA P2）。
    if (a === 'remove') {
      // 服务端删号时已同步 forget meta（v2.3.0 #12）；本机缓存同步清掉，避免残留展示。
      try { if (metaAll && metaAll[u] !== undefined) delete metaAll[u]; } catch (e) {}
      refreshAccountPoolAfterMutation(true);
    } else refreshAccountPool(true);
  }
});

// 重新扫描 auths/ 目录：外部脚本批量导入/删除凭证后免重启对齐（只增删账号，
// 已存在账号的积分/冷却状态保留）。后端对「扫到 0 个文件」有拒绝保护。
if ($('btnRescan')) $('btnRescan').onclick = async () => {
  try {
    const r = await api('accounts/rescan', { method: 'POST' });
    toast('扫描 ' + r.scanned + ' 个文件：新增 ' + r.added + '，剔除 ' + r.removed, 'ok');
    refreshAccountPoolAfterMutation(true);   // 账号集合变了：当前页 + 全量快照都刷新
  } catch (e) { toast(e.message, 'err'); }
};

