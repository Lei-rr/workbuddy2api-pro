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
  // 弹窗可被连续点开：请求前快照 uid + 序号，旧账号/旧请求的响应一律丢弃。
  const uid = pkUID;
  const seq = reqSeqNext('packageDetail');
  const st = $('pkState'), body = $('pkBody');
  st.hidden = false;
  st.className = 'state';
  st.innerHTML = '<span class="dots">查询中</span>';
  body.innerHTML = '';
  try {
    // 折叠条数复用配置里的 package_detail_limit；config 拉不到就用默认值。
    const [d, c] = await Promise.all([
      api('packages?uid=' + encodeURIComponent(uid)),
      api('config').catch(() => null),
    ]);
    if (reqSeqStale('packageDetail', seq) || pkUID !== uid) return;
    const a = (d.accounts || [])[0];
    if (!a) throw new Error('该账号无数据');
    st.hidden = true;
    body.innerHTML = packageDetailHtml(a, pkDetailLimit(c && c.config));
  } catch (e) {
    if (reqSeqStale('packageDetail', seq) || pkUID !== uid) return;
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
    '<th scope="col" class="mark" aria-hidden="true"></th><th scope="col">包名 / 来源</th>' +
    '<th scope="col" class="num">面额</th><th scope="col" class="num">剩余</th><th scope="col" class="num">已用</th>' +
    '<th scope="col" class="num">发放</th><th scope="col" class="num">到期</th>' +
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

