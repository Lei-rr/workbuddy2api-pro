/* ══ 账号备注 / 标签（v2.3.0 #12：服务端权威 + localStorage 降级）══════════════
   后端契约（backend-c）：
     GET  /panel/api/meta                        → {ok, meta:{uid:{note,tags,updated_at}}}
     GET  /panel/api/accounts/{uid}/meta         → {ok, uid, note, tags, updated_at}
     PUT  /panel/api/accounts/{uid}/meta {note,tags}
     DELETE /panel/api/accounts/{uid}/meta
   服务端 data/account-meta.json 是权威数据（note ≤500 字、tags ≤10 个且每个 ≤32 字）；
   本机 localStorage(LS_REMARKS) 只作降级缓存：读失败/写失败时界面仍可用、改动不丢。
   备注键仍按 uid 前 8 位存本机（与 renderUsageDim / 积分扣除表 join 口径一致）；
   服务端键是完整 uid（与 auths/、state.json 同源）。 */
let metaAll = {};           // 完整 uid → {note,tags,updated_at}（服务端权威快照）
let metaLoaded = false;     // /meta 是否成功加载过
let metaMigrationTried = false;   // 本机旧备注→服务端的一次性迁移是否已尝试

function remarksAll() {
  if (remarksCache) return remarksCache;
  try { remarksCache = JSON.parse(localStorage.getItem(LS_REMARKS)) || {}; }
  catch (e) { remarksCache = {}; }
  return remarksCache;
}
function remarkKey(uid) { return String(uid == null ? '' : uid).slice(0, 8); }
function storeRemarks(map) {
  remarksCache = map;
  try { localStorage.setItem(LS_REMARKS, JSON.stringify(map)); } catch (e) {}
}

/* parseTags 标签输入 → 数组：逗号（半角/全角）或空白分隔，去空、去重，
   最多 10 个、每个截到 32 字。与后端 accountmeta.go 的校验口径一致。 */
function parseTags(v) {
  const raw = Array.isArray(v) ? v : String(v == null ? '' : v).split(/[,，\s]+/);
  const out = [];
  for (const item of raw) {
    const t = String(item == null ? '' : item).trim().slice(0, 32);
    if (t && out.indexOf(t) < 0) out.push(t);
    if (out.length >= 10) break;
  }
  return out;
}

/* normalizeMeta 把 /meta 响应归一化成 uid→{note,tags,updated_at}；
   字段缺失/类型不对都容忍（后端未就绪时返回空对象，界面回落 localStorage）。 */
function normalizeMeta(d) {
  const src = (d && (d.meta || d.accounts)) || {};
  const out = {};
  if (!src || typeof src !== 'object') return out;
  for (const uid of Object.keys(src)) {
    if (!uid) continue;
    const m = src[uid] || {};
    out[String(uid)] = {
      note: String(m.note == null ? '' : m.note),
      tags: parseTags(m.tags),
      updated_at: Number(m.updated_at) || 0,
    };
  }
  return out;
}
function metaFor(uid) { return metaAll[String(uid == null ? '' : uid)] || null; }
function metaNoteOf(uid) { const m = metaFor(uid); return m ? m.note : ''; }
function metaTagsOf(uid) { const m = metaFor(uid); return m ? m.tags : []; }

/* remarkOf 备注展示：服务端权威优先，未命中回落 localStorage（降级/迁移前）。 */
function remarkOf(uid) {
  const s = String(uid == null ? '' : uid);
  if (!s) return '';
  const m = metaFor(s);
  if (m && m.note) return m.note;
  const k = remarkKey(s);
  return k ? String(remarksAll()[k] || '') : '';
}
/* nameHtml 必须是已转义的 HTML；有备注时返回「备注 + 原标识（浅色虚体）」 */
function remarkName(uid, nameHtml) {
  const r = remarkOf(uid);
  return r ? esc(r) + '<span class="rm-ghost">' + nameHtml + '</span>' : nameHtml;
}
/* accTagsHtml 账号行标签：最多展示 3 个 + 「+N」（title 里给全量），无标签返回空串。 */
function accTagsHtml(uid) {
  const tags = metaTagsOf(uid);
  if (!tags.length) return '';
  const shown = tags.slice(0, 3);
  const extra = tags.length - shown.length;
  const parts = shown.map(t => '<span class="tag mute" title="' + esc(t) + '">' + esc(t) + '</span>');
  if (extra > 0) parts.push('<span class="tag mute" title="' + esc(tags.slice(3).join('、')) + '">+' + extra + '</span>');
  return '<span class="acc-tags">' + parts.join('') + '</span>';
}

/* mergeServerNotesToLocal 把服务端备注同步进本机缓存（降级用）：
   只覆盖非空备注，不删除本地已有键——本地可能是尚未迁移的旧数据。 */
function mergeServerNotesToLocal() {
  const merged = Object.assign({}, remarksAll());
  let changed = false;
  for (const uid of Object.keys(metaAll)) {
    const k = remarkKey(uid);
    const note = metaAll[uid].note;
    if (k && note && merged[k] !== note) { merged[k] = note; changed = true; }
  }
  if (changed) storeRemarks(merged);
}

async function loadMeta(quiet) {
  // 请求序号：打开备注弹窗时的加载与保存/迁移并发时，只允许最后一次请求写回 metaAll。
  const seq = reqSeqNext('meta');
  try {
    const d = await api('meta');
    if (reqSeqStale('meta', seq)) return false;
    metaAll = normalizeMeta(d);
    metaLoaded = true;
    mergeServerNotesToLocal();
    maybeMigrateRemarks();
    loadAccTags(true);   // v2.3.1：备注/标签加载后同步标签筛选下拉（静默）
    return true;
  } catch (e) {
    if (reqSeqStale('meta', seq)) return false;
    // 接口未就绪/网络失败：界面继续用 localStorage（remarkOf 已回落），不阻塞面板。
    if (!quiet) toast('读取账号备注失败，已回落本机缓存：' + e.message, 'err');
    return false;
  }
}

/* maybeMigrateRemarks 一次性迁移：服务端 meta 为空 且 本机存在旧备注时，
   用当前账号全集把 uid8 解析成完整 uid 后逐个 PUT 导入；成功后清理已迁移的本地键，
   全失败则静默保留本地（下次打开再试，或用户手动保存时自然写入服务端）。
   服务端已有任意 meta 时不自动导入，避免覆盖服务端数据。 */
function maybeMigrateRemarks() {
  if (metaMigrationTried || !metaLoaded) return;
  const local = remarksAll();
  const keys = Object.keys(local).filter(k => local[k]);
  if (!keys.length || Object.keys(metaAll).length > 0) { metaMigrationTried = true; return; }
  const pool = poolAccountList();
  if (!pool.length) return;   // 账号列表还没加载：等 loadAccountPage 成功后再试
  metaMigrationTried = true;
  const uidOf = k => {
    const hit = pool.find(s => s && remarkKey(s.uid) === k);
    return hit ? String(hit.uid) : '';
  };
  const pairs = [];
  for (const k of keys) {
    const uid = uidOf(k);
    if (uid) pairs.push({ k: k, uid: uid, note: local[k] });
  }
  if (!pairs.length) return;   // 全部解析不到完整 uid：保留本地（不回写错键）
  Promise.allSettled(pairs.map(p => api('accounts/' + encodeURIComponent(p.uid) + '/meta', {
    method: 'PUT',
    body: JSON.stringify({ note: String(p.note).slice(0, 500), tags: [] }),
  }))).then(results => {
    const migrated = [];
    results.forEach((r, i) => {
      if (r.status !== 'fulfilled') return;
      const p = pairs[i];
      metaAll[p.uid] = { note: String(p.note).slice(0, 500), tags: [], updated_at: Math.floor(Date.now() / 1000) };
      migrated.push(p.k);
    });
    if (!migrated.length) return;   // 全失败：保留本地，静默
    const remain = {};
    for (const k of Object.keys(local)) {
      if (migrated.indexOf(k) < 0) remain[k] = local[k];
    }
    storeRemarks(remain);
    toast('已迁移本机备注到服务端（' + migrated.length + ' 条）', 'ok');
    renderAfterMetaChange();
  });
}

/* renderAfterMetaChange 备注/标签变化后重渲染当前视图（无后端请求）。 */
function renderAfterMetaChange() {
  if (view === 'accounts') renderAccounts(accAll, accPageMeta);
  else if (view === 'usage' && usageData) renderUsage(usageData);
}

/* 编辑器的行来自：账号全集（poolAccountList）+ 服务端 meta 里有记录的 uid
   （被筛掉/不在当前页也能编辑）+ 本机旧备注（uid8，未解析到完整 uid 时标 legacy）。 */
function remarkFormRows() {
  const map = new Map();
  const fullUids = new Set();
  for (const s of poolAccountList()) {
    if (!s || !s.uid) continue;
    const uid = String(s.uid);
    fullUids.add(uid);
    map.set(uid, { uid: uid, who: s.nickname || uid.slice(0, 8), legacy: false });
  }
  for (const uid of Object.keys(metaAll)) {
    if (!map.has(uid)) map.set(uid, { uid: uid, who: uid, legacy: false });
  }
  for (const k of Object.keys(remarksAll())) {
    if (!remarksAll()[k]) continue;
    let uid = '';
    for (const full of fullUids) { if (remarkKey(full) === k) { uid = full; break; } }
    if (uid) {
      if (!map.has(uid)) map.set(uid, { uid: uid, who: uid.slice(0, 8), legacy: false });
    } else if (!map.has(k)) {
      map.set(k, { uid: k, who: k, legacy: true });
    }
  }
  return Array.from(map.values());
}

function renderRemarkBody() {
  const box = $('remarkBody');
  if (!box) return;
  const rows = remarkFormRows();
  box.innerHTML = rows.length
    ? rows.map(r => {
      const note = r.legacy ? String(remarksAll()[r.uid] || '') : (metaNoteOf(r.uid) || remarkOf(r.uid));
      const tags = r.legacy ? [] : metaTagsOf(r.uid);
      return '<div class="rm-row" data-uid="' + esc(r.uid) + '"' + (r.legacy ? ' data-legacy="1"' : '') + '>' +
        '<span class="rm-who" title="' + esc(r.who) + '">' + esc(r.who) + '</span>' +
        '<div class="rm-fields">' +
        '<input type="text" class="rm-note" data-uid="' + esc(r.uid) + '" value="' + esc(note) +
        '" placeholder="备注（留空即清除，≤500 字）" maxlength="500">' +
        '<input type="text" class="rm-tags" data-uid="' + esc(r.uid) + '" value="' + esc(tags.join(', ')) +
        '" placeholder="标签：逗号/空格分隔（≤10 个，每个 ≤32 字）" maxlength="400">' +
        '</div></div>';
    }).join('')
    : '<div class="empty">暂无账号：先在「账号池」加载一次账号列表。</div>';
}
async function openRemarks() {
  if ($('remarkVeil')) $('remarkVeil').classList.add('on');
  renderRemarkBody();                 // 先用已加载的 meta/本地缓存渲染，避免空白
  if (!metaLoaded) {
    await loadMeta(true);             // 打开时优先拉服务端最新数据；失败保留本地渲染
    renderRemarkBody();
  }
}
function closeRemarks() { if ($('remarkVeil')) $('remarkVeil').classList.remove('on'); }
/* saveRemarkForm 保存：逐账号 PUT（清空则 DELETE），本地缓存先写（失败不丢改动）；
   部分失败提示数量并保留本地，可再次保存重试。 */
async function saveRemarkForm() {
  const box = $('remarkBody');
  if (!box) return;
  const rows = Array.from(box.querySelectorAll('.rm-row'));
  if (!rows.length) { closeRemarks(); return; }
  const nextLocal = {};
  const changes = [];
  for (const row of rows) {
    const uid = String(row.getAttribute('data-uid') || '');
    if (!uid) continue;
    const legacy = row.getAttribute('data-legacy') === '1';
    const noteInp = row.querySelector('.rm-note');
    const tagsInp = row.querySelector('.rm-tags');
    const note = String((noteInp && noteInp.value) || '').trim().slice(0, 500);
    const tags = parseTags(tagsInp ? tagsInp.value : '');
    if (note) nextLocal[remarkKey(uid)] = note;
    if (legacy) continue;   // 旧本机键解析不到完整 uid：只更新本地，不把 uid8 写到服务端
    const prev = metaFor(uid);
    const prevNote = prev ? prev.note : '';
    const prevTags = prev ? prev.tags : [];
    if (prevNote === note && prevTags.join('\u0001') === tags.join('\u0001')) continue;
    changes.push({ uid: uid, note: note, tags: tags, hasPrev: !!prev });
  }
  storeRemarks(nextLocal);   // 本地兜底：服务端写失败也不丢改动
  if (!changes.length) { closeRemarks(); toast('没有需要保存的改动'); return; }
  const btn = $('btnRemarkSave');
  if (btn) btn.disabled = true;
  const results = await Promise.allSettled(changes.map(ch => {
    const path = 'accounts/' + encodeURIComponent(ch.uid) + '/meta';
    if (!ch.note && !ch.tags.length && ch.hasPrev) return api(path, { method: 'DELETE' });
    return api(path, { method: 'PUT', body: JSON.stringify({ note: ch.note, tags: ch.tags }) });
  }));
  let ok = 0, failed = 0;
  changes.forEach((ch, i) => {
    if (results[i].status !== 'fulfilled') { failed++; return; }
    ok++;
    if (!ch.note && !ch.tags.length) delete metaAll[ch.uid];
    else metaAll[ch.uid] = { note: ch.note, tags: ch.tags, updated_at: Math.floor(Date.now() / 1000) };
  });
  if (btn) btn.disabled = false;
  closeRemarks();
  if (failed) toast('已保存 ' + ok + ' 条，失败 ' + failed + ' 条（本机缓存已保留，可重试）', 'err');
  else toast('备注/标签已保存到服务端（' + ok + ' 条）', 'ok');
  renderAfterMetaChange();
  // v2.3.1：标签集合/计数可能变化——刷新标签下拉；若正按标签筛选，列表也要重取，
  // 避免选中标签已被删掉却还显示旧筛选结果。
  loadAccTags(true);
  if (view === 'accounts') refreshAccountPool(true);
}
if ($('btnRemarks')) $('btnRemarks').onclick = openRemarks;
if ($('btnRemarkClose')) $('btnRemarkClose').onclick = closeRemarks;
if ($('btnRemarkClear')) $('btnRemarkClear').onclick = () => {
  // 只清编辑框与本地缓存；点「保存」后空值会触发服务端 DELETE。
  document.querySelectorAll('#remarkBody .rm-note, #remarkBody .rm-tags').forEach(inp => { inp.value = ''; });
  storeRemarks({});
  toast('已清空编辑框（点「保存」同步删除服务端备注/标签）');
};
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
