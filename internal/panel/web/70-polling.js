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
   pollVisibilityAction）集中在段前，便于纯函数单测。 */

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
   一轮（pollTick 会顺带重新排期）。返回 Promise 便于调用方等待。 */
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
  // v2.3.0 #12：账号备注/标签服务端权威数据。失败静默回落 localStorage；
  // 加载成功后尝试一次性把本机旧备注迁移到服务端（服务端已有数据时不覆盖）。
  loadMeta(true).then(() => maybeMigrateRemarks());
  checkAuthGate();
  connectEvents();   // v2.4.0 #3：启动实时事件流（不可用/关闭时自动回落轮询）
}
async function checkAuthGate() {
  // 只探鉴权：用 lite overview（1-2 KB），不拉全量账号。
  try { await api('overview?lite=1'); }
  catch (e) { if (String(e.message).includes('密钥') || String(e.message).includes('api_key')) return; }
}
start();

