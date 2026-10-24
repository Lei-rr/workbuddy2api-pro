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

// expiryRowsHtml 全量渲染入口（保留原签名，直接调用方仍可复用）。
function expiryRowsHtml(list) {
  const { keyed, today } = expiryKeyed(list);
  return expiryRowsOf(keyed, today);
}

// expSlice 按当前分页状态切出要渲染的账号，并把页码/总数回写到筛选栏。
// 直接调用方可能没有模块级分页状态（expAll/expPage/expPageSize），
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
// 误以为数据也是 5 秒级新鲜。放在 renderExpirySnapshot 之后：区域片段独立求值按
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

