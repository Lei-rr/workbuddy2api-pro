/* ── 实时事件 SSE（v2.4.0 #3）──────────────────────────────────────────
   后端 GET /panel/api/events（withAuth，Bearer）：text/event-stream，事件
     snapshot → {now,version,poll_interval_sec,counts{...},credits_sum,
                 credits_total_sum,jobs:[...],events_enabled}
     jobs     → {jobs:[...]}（进度条增量）
     accounts → {counts,credits_sum,credits_total_sum,...}（统计条增量）
     心跳注释 `: ping`（忽略）。
   连接本身用 fetch + ReadableStream（EventSource 不能带 Authorization）。连接期间
   跳过 jobs / lite overview 的 5s 轮询（避免双拉），当前页账号行仍按 5s 刷新（SSE
   不推逐号行数据，冷却倒计时需要它）；断线/隐藏页面暂停并恢复轮询。404/501 或
   events_enabled=false → eventsSupported=false 永久回落轮询，不再重连。 */
const EVENTS_MAX_BACKOFF_MS = 30000;   // 指数退避上限 1s/2s/4s…30s
let eventsSupported = true;            // 后端是否提供 SSE（404/501 后永久 false）
let eventsConnecting = false;          // 建连在途
let eventsConnected = false;           // 流已建立
let eventsHasSnapshot = false;         // 已收到 snapshot/accounts（统计条可用）
let eventsGotAny = false;              // 收到过任何数据事件（jobs 也可暂停轮询）
let eventsAbort = null;                // AbortController（隐藏/退出/回落时断开）
let eventsRetryTimer = null;
let eventsRetryCount = 0;
let eventsEnabledCfg = true;

/* eventsLive 轮询开关：连上且收到过至少一个数据事件才暂停轮询，避免
   「刚连上还没数据就跳过轮询」导致首屏空白。 */
function eventsLive() { return eventsConnected && eventsGotAny; }
function rtBadgeState() {
  if (eventsConnected) return eventsHasSnapshot ? 'live' : 'connecting';
  return eventsSupported ? 'connecting' : 'poll';
}
function setRtBadge(state) {
  const b = $('rtBadge');
  if (!b) return;
  b.hidden = false;
  b.textContent = state === 'live' ? '实时' : (state === 'connecting' ? '连接中' : '轮询');
  b.className = 'rt-badge ' + (state === 'live' ? 'live' : (state === 'connecting' ? 'connecting' : 'poll'));
  b.title = state === 'live'
    ? '实时事件已连接（SSE），统计数据增量推送'
    : (state === 'connecting' ? '正在建立实时连接，暂用轮询' : '实时事件不可用，已回落轮询刷新');
}

/* createSSEParser 纯函数：喂文本块 → 完整事件数组；跨块残帧留到下次。
   兼容 \r\n / \n、多行 data、注释帧（: ping）、event 缺省 message。 */
function createSSEParser() {
  let buf = '';
  return function feed(chunk) {
    buf += String(chunk == null ? '' : chunk);
    const out = [];
    let idx;
    while ((idx = buf.indexOf('\n\n')) >= 0) {
      const frame = buf.slice(0, idx);
      buf = buf.slice(idx + 2);
      const ev = parseSSEFrame(frame);
      if (ev) out.push(ev);
    }
    return out;
  };
}
function parseSSEFrame(frame) {
  if (!frame || !frame.replace(/\s/g, '')) return null;
  const lines = String(frame).split(/\r?\n/);
  let event = 'message';
  const data = [];
  for (const line of lines) {
    if (!line || line.charAt(0) === ':') continue;   // 心跳/注释
    const i = line.indexOf(':');
    const field = i >= 0 ? line.slice(0, i) : line;
    let value = i >= 0 ? line.slice(i + 1) : '';
    if (value.charAt(0) === ' ') value = value.slice(1);
    if (field === 'event') event = value || 'message';
    else if (field === 'data') data.push(value);
  }
  if (!data.length) return null;   // 只有注释/空帧
  return { event: event, data: data.join('\n') };
}
function parseSSEData(ev) {
  try { return JSON.parse(ev.data); } catch (e) { return null; }
}

/* applyLiveCounts 用 SSE 增量更新统计条（不整页刷新、不动账号表/模型锁池）。
   字段缺失就保持现状；credits 优先用推送值。 */
function applyLiveCounts(d) {
  if (!d || typeof d !== 'object') return;
  if (d.poll_interval_sec != null) pollBaseSec = pollIntervalSec(d.poll_interval_sec);
  const c = d.counts || {};
  const put = (id, v) => { if (v != null && $(id)) $(id).textContent = v; };
  put('sTotal', c.total);
  put('sHealthy', c.healthy);
  put('sCooling', c.cooling);
  put('sDisabled', c.disabled);
  put('sPaused', c.paused);
  put('sSticky', c.sticky_sessions);
  if (d.credits_sum != null || d.credits_total_sum != null) {
    creditsSum = { rem: Number(d.credits_sum) || 0, tot: Number(d.credits_total_sum) || 0 };
    renderCreditsStat();
  }
  if (d.version != null) {
    if ($('navSub')) $('navSub').textContent = 'v' + d.version;
    if ($('navVer')) $('navVer').textContent = 'v' + d.version;
  }
  if (c.total != null || c.healthy != null) {
    const total = Number(c.total);
    const healthy = Number(c.healthy);
    if ($('navState')) $('navState').textContent = healthy > 0 ? '服务正常' : (total ? '无可用账号' : '待添加账号');
    if ($('navPulse')) $('navPulse').className = 'pulse' + (healthy > 0 ? '' : (total ? ' warn' : ' bad'));
  }
  // 时钟校准：后端 snapshot 同时给 RFC3339 的 now 与 now_unix（秒）。优先用后者，
  // 让冷却倒计时与账号页的 skew 口径一致（304 期间也不漂移）。
  const nowUnix = Number(d.now_unix);
  if (Number.isFinite(nowUnix) && nowUnix > 0) accClockSkew = Date.now() - nowUnix * 1000;
  else if (typeof d.now === 'number' && d.now > 0) accClockSkew = Date.now() - d.now * 1000;
}
/* handleSSEEvent 事件分发：snapshot/accounts 更新统计条，jobs 更新进度条。 */
function handleSSEEvent(ev) {
  if (!ev || !ev.event) return;
  if (ev.event !== 'snapshot' && ev.event !== 'accounts' && ev.event !== 'jobs') return;
  const d = parseSSEData(ev);
  if (!d) return;
  eventsConnected = true;
  eventsGotAny = true;
  if (ev.event === 'jobs') {
    if (Array.isArray(d.jobs)) renderJobBar(normalizeJobs(d));
    setRtBadge(rtBadgeState());
    return;
  }
  eventsHasSnapshot = true;
  if (d.events_enabled === false) { disableEvents('服务端已关闭实时事件'); return; }
  applyLiveCounts(d);
  if (ev.event === 'snapshot' && Array.isArray(d.jobs)) renderJobBar(normalizeJobs(d));
  setRtBadge(rtBadgeState());
}
/* 环境兜底：缺少 AbortController/TextDecoder 的极简环境下降级，
   保证面板主流程（轮询）不被 SSE 依赖阻断。 */
function newAbortController() {
  if (typeof AbortController === 'function') return new AbortController();
  return { signal: null, abort() {} };
}
function newTextDecoder() {
  if (typeof TextDecoder === 'function') return new TextDecoder();
  return { decode(v) { return String(v == null ? '' : v); } };
}
async function pumpEvents(r, ac) {
  const reader = r.body.getReader();
  const dec = newTextDecoder();
  const feed = createSSEParser();
  try {
    for (;;) {
      const step = await reader.read();
      if (step.done) break;
      const events = feed(dec.decode(step.value, { stream: true }));
      for (const ev of events) handleSSEEvent(ev);
      if (ac && ac.signal && ac.signal.aborted) break;
    }
  } catch (e) { /* 网络错误/主动 abort：走统一关闭分支 */ }
  finally { onEventsClosed(); }
}
/* connectEvents 建连（幂等：在途/已连/隐藏/未登录时直接返回）。 */
async function connectEvents() {
  if (!eventsSupported || !eventsEnabledCfg || eventsConnecting || eventsConnected) return;
  if (pollAuthStopped || pageHidden) return;
  eventsConnecting = true;
  const ac = newAbortController();
  eventsAbort = ac;
  try {
    const h = {};
    const k = localStorage.getItem(LS_KEY);
    if (k) h['Authorization'] = 'Bearer ' + k;
    const r = await fetch('/panel/api/events', { headers: h, signal: ac.signal, cache: 'no-store' });
    if (r.status === 401) { eventsConnecting = false; onAuthRequired(); return; }
    if (r.status === 404 || r.status === 501) { eventsConnecting = false; disableEvents('HTTP ' + r.status); return; }
    if (!r.ok || !r.body || !r.body.getReader) throw new Error('HTTP ' + r.status);
    eventsConnecting = false;
    eventsConnected = true;
    eventsHasSnapshot = false;
    eventsGotAny = false;
    eventsRetryCount = 0;
    setRtBadge('connecting');
    pumpEvents(r, ac);   // 后台持续读流；关闭时 onEventsClosed 负责重连/回落
  } catch (e) {
    eventsConnecting = false;
    if (e && e.name === 'AbortError') return;   // 主动断开（隐藏/退出/回落）
    scheduleEventsRetry();
  }
}
function onEventsClosed() {
  const wasSupported = eventsSupported;
  eventsConnected = false;
  eventsHasSnapshot = false;
  eventsGotAny = false;
  eventsAbort = null;
  setRtBadge(rtBadgeState());
  if (wasSupported && !pageHidden && !pollAuthStopped) scheduleEventsRetry();
  else if (wasSupported) scheduleNextPoll();   // 隐藏/未登录：交给轮询兜底
}
function scheduleEventsRetry() {
  if (!eventsSupported || eventsRetryTimer || pageHidden || pollAuthStopped) return;
  const ms = Math.min(EVENTS_MAX_BACKOFF_MS, 1000 * Math.pow(2, Math.min(eventsRetryCount, 5)));
  eventsRetryCount += 1;
  setRtBadge('connecting');
  eventsRetryTimer = setTimeout(() => { eventsRetryTimer = null; connectEvents(); }, ms);
}
function abortEvents() {
  if (eventsAbort) { try { eventsAbort.abort(); } catch (e) { /* 已关闭 */ } eventsAbort = null; }
  if (eventsRetryTimer) { clearTimeout(eventsRetryTimer); eventsRetryTimer = null; }
  eventsConnecting = false;
  eventsConnected = false;
  eventsHasSnapshot = false;
  eventsGotAny = false;
}
/* disableEvents 永久回落：404/501 或服务端 events_enabled=false。 */
function disableEvents(reason) {
  eventsSupported = false;
  abortEvents();
  const b = $('rtBadge');
  if (b) { b.hidden = false; b.className = 'rt-badge poll'; b.textContent = '轮询'; b.title = '实时事件不可用，已回落轮询刷新' + (reason ? '（' + reason + '）' : ''); }
  scheduleNextPoll();
}
/* 页面隐藏：断流省电（回来时 pollTick/visibility 重连）；可见：立即重连。 */
function eventsVisibilityChange() {
  const hiddenNow = !!(document && document.visibilityState === 'hidden');
  if (hiddenNow) abortEvents();
  else if (eventsSupported && eventsEnabledCfg) connectEvents();
}
if (document && document.addEventListener) document.addEventListener('visibilitychange', eventsVisibilityChange);

