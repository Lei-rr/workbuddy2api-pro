#!/usr/bin/env node
/**
 * panel_frontend_check.mjs —— 面板前端工程化检查（v2.3.1 #18，零依赖；v2.6.1 模块化）
 *
 * v2.6.1 起前端源码拆到 internal/panel/web/*.js，由 Go 端按文件名排序拼接成单个
 * /panel/app.js 提供给浏览器（无构建步骤、浏览器零变化）。本脚本按同样顺序读取模块、
 * 拼接后再跑以下检查；解析错误会定位到具体模块文件：
 *   1) 解析检查：app.js 各模块与 mobile.js 必须能被 JS 解析器编译（等价 node --check）；
 *   2) DOM id 契约：index.html 的 id 与拼接后 app.js/mobile.js 里 $('id') 引用交叉检查
 *      （缺一侧报错；HTML 侧允许白名单——纯 CSS 锚点、按前缀/变量拼出来的元素等）；
 *   3) API 路径白名单：api()/apiCached() 的字面量路径必须命中已知前缀；不允许在
 *      前端硬编码完整 /panel/api/xxx 路径、绝对 URL 或疑似密钥字面量；
 *   4) 体积预算：拼接后 app.js < 350KB（模块总和）、mobile.js < 25KB、index.html < 130KB。
 *
 * 退出码：0 = 全部通过；1 = 任一检查失败（打印 FAIL 原因与实际值）。
 * 用法：node scripts/panel_frontend_check.mjs
 */
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const FILES = {
  appDir: 'internal/panel/web',
  mobileJs: 'internal/panel/mobile.js',
  indexHtml: 'internal/panel/index.html',
};
const SIZE_BUDGET = {
  appJs: 350 * 1024,
  mobileJs: 25 * 1024,
  indexHtml: 130 * 1024,
};

/* HTML 里存在、但不通过 $('id') 直接引用的 id（白名单）：
   - view-*：app.js 用 '#view-' + v 拼出来切换可见性；
   - 纯容器/样式锚点：由 CSS、querySelector、或分页控件 id 配置表引用；
   - 新控件若两边都直接引用，不应加进这里（进来说明「HTML 有、JS 没接线」）。 */
const HTML_ONLY_ID_ALLOW = new Set([
  'accFilter',
  'addPoll',
  'addTabs',
  'auditBox',
  'cfgAllowAnonymous',
  'cfgBalConc',
  'cfgBalDelay',
  'cfgBalForce',
  'cfgCatchUp',
  'cfgPollInterval',
  'creditCount',
  'creditFilter',
  'creditPageInfo',
  'expFilter',
  'logCount',
  'logFilter',
  'logPageInfo',
  'mdFilter',
  'mlCount',
  'mlFilter',
  'mlPageInfo',
  'navDrawer',
  'qcCount',
  'reqFilter',
  'reqPageInfo',
  'runsBox',
  'tcFilter',
  'tcPageInfo',
]);
const HTML_ONLY_ID_PREFIX_ALLOW = ['view-'];

/* app.js 里 $('id') 引用、但 index.html 没有的 id（正常应为空；预留机制便于
   动态创建的元素，新增时必须在这里登记并写原因）。 */
const APP_ONLY_ID_ALLOW = new Map([]);

/* api()/apiCached() 允许的相对路径前缀（后端面板 API 契约；只增不改名）。
   这里的每一项都对应 internal/panel 的既有路由或其动态子路径。 */
const API_PATH_PREFIXES = [
  'accounts/',
  'accounts?',
  'audit?',
  'balance_all',
  'checkin_all',
  'config',
  'credit_history',
  'events',
  'import/cockpit',
  'jobs/',
  'jobs?',
  'keepalive_all',
  'login/',
  'logs',
  'meta',
  'meta/tags',
  'model_probes',
  'models',
  'overview',
  'overview?',
  'packages',
  'request_logs',
  'request_metrics',
  'school/vouchers',
  'status',
  'task_runs',
  'tasks/',
  'travel_all',
  'activity_all',
  'usage',
];

/* 允许在 fetch 里直接写完整面板路径的白名单（必须写明原因）：
   - /panel/api/import/cockpit：multipart/form-data 上传，api() 的 JSON 封装不适用。
   - /panel/api/events：SSE 流式响应 + AbortController，需要 fetch 原始 Response.body。 */
const ALLOWED_PANEL_PATH_LITERALS = new Set(['/panel/api/import/cockpit', '/panel/api/events']);

const errors = [];
function fail(msg) { errors.push(msg); }
function read(rel) {
  const abs = path.join(ROOT, rel);
  if (!fs.existsSync(abs)) { fail(`缺少文件：${rel}`); return ''; }
  return fs.readFileSync(abs, 'utf8');
}
function kb(n) { return (n / 1024).toFixed(1) + 'KB'; }

/* readAppModules 读取 internal/panel/web/*.js（按文件名字典序；与 Go 端 go:embed
   拼接顺序一致），返回 { files: [{name, src}], src: 拼接结果 }。 */
function readAppModules() {
  const dir = path.join(ROOT, FILES.appDir);
  if (!fs.existsSync(dir)) {
    fail(`缺少前端模块目录：${FILES.appDir}`);
    return { files: [], src: '' };
  }
  const names = fs.readdirSync(dir).filter(n => n.endsWith('.js')).sort();
  if (!names.length) {
    fail(`${FILES.appDir} 下没有 *.js 模块`);
    return { files: [], src: '' };
  }
  const files = names.map(name => ({ name, src: fs.readFileSync(path.join(dir, name), 'utf8') }));
  return { files, src: files.map(f => f.src).join('') };
}

// ── 读取 ────────────────────────────────────────────────────────────────
const appModules = readAppModules();
const appJs = appModules.src;
const mobileJs = read(FILES.mobileJs);
const indexHtml = read(FILES.indexHtml);
if (errors.length) { report(); }

// ── 1) 解析检查（等价 node --check）：先逐模块定位错误，再检查拼接结果 ─────
for (const f of appModules.files) {
  try {
    new vm.Script(f.src, { filename: f.name });
  } catch (e) {
    fail(`${FILES.appDir}/${f.name} 解析失败：${e && e.message}`);
  }
}
try {
  new vm.Script(appJs, { filename: 'app.js' });
} catch (e) {
  fail(`app.js 拼接结果解析失败：${e && e.message}`);
}
try {
  new vm.Script(mobileJs, { filename: 'mobile.js' });
} catch (e) {
  fail(`mobile.js 解析失败：${e && e.message}`);
}

// ── 2) DOM id 契约 ──────────────────────────────────────────────────────
const htmlIds = new Set();
for (const m of indexHtml.matchAll(/\bid="([^"]+)"/g)) htmlIds.add(m[1]);
const jsRefs = new Set();
const jsAll = appJs + '\n' + mobileJs;
for (const m of jsAll.matchAll(/\$\s*\(\s*(['"])([^'"]+)\1\s*\)/g)) jsRefs.add(m[2]);
/* HTML 内部引用也算「有消费方」：aria-labelledby / aria-controls / aria-describedby
   的 token 列表、<label for="…">。否则给弹层补 aria 语义会被误判为孤儿 id。 */
const htmlInternalRefs = new Set();
for (const attr of ['aria-labelledby', 'aria-controls', 'aria-describedby']) {
  const re = new RegExp('\\b' + attr + '="([^"]+)"', 'g');
  for (const m of indexHtml.matchAll(re)) {
    for (const tok of m[1].trim().split(/\s+/)) if (tok) htmlInternalRefs.add(tok);
  }
}
for (const m of indexHtml.matchAll(/<label[^>]*\bfor="([^"]+)"/g)) htmlInternalRefs.add(m[1]);
const referencedIds = new Set([...jsRefs, ...htmlInternalRefs]);
for (const id of jsRefs) {
  if (!htmlIds.has(id) && !APP_ONLY_ID_ALLOW.has(id)) {
    fail(`app.js 引用了 index.html 中不存在的 id="${id}"（DOM id 契约）`);
  }
}
for (const id of htmlIds) {
  if (referencedIds.has(id) || HTML_ONLY_ID_ALLOW.has(id)) continue;
  if (HTML_ONLY_ID_PREFIX_ALLOW.some(p => id.startsWith(p))) continue;
  fail(`index.html 的 id="${id}" 未被 app.js/aria 引用（DOM id 契约；如为纯样式锚点请加入 HTML_ONLY_ID_ALLOW）`);
}

// ── 2b) 可访问性契约（v2.6.0）：弹层 dialog 语义 / toast 状态播报 ─────────
const veilBlocks = [...indexHtml.matchAll(/<div class="veil" id="([^"]+)"([^>]*)>/g)];
for (const m of veilBlocks) {
  const id = m[1], attrs = m[2] || '';
  if (!/\brole="dialog"/.test(attrs)) fail(`弹层 #${id} 缺少 role="dialog"`);
  if (!/\baria-modal="true"/.test(attrs)) fail(`弹层 #${id} 缺少 aria-modal="true"`);
  const labelled = (attrs.match(/aria-labelledby="([^"]+)"/) || [])[1] || '';
  if (!labelled) fail(`弹层 #${id} 缺少 aria-labelledby`);
  else if (!htmlIds.has(labelled.split(/\s+/)[0])) fail(`弹层 #${id} 的 aria-labelledby 指向不存在的 id`);
}
if (indexHtml.includes('id="toasts"') && !/id="toasts"[^>]*aria-live="polite"/.test(indexHtml)) {
  fail('toast 容器 #toasts 缺少 aria-live="polite"（状态变化读屏不可感知）');
}
// 表头列语义：静态 <th> 必须带 scope（动态表格由 app.js 单独保证）
const thNoScope = [...indexHtml.matchAll(/<th\b[^>]*>/g)].filter(m => !/\bscope=/.test(m[0])).length;
if (thNoScope) fail(`index.html 有 ${thNoScope} 个 <th> 缺少 scope="col"（表头列语义）`);

// ── 3) API 路径白名单 ───────────────────────────────────────────────────
/* scanJs 极简词法扫描：返回 { strings, masked }。
   - strings：所有字符串字面量（不含注释里的文本）；
   - masked：把注释替换成空格后的源码（保留字符串与行号），用于 api() 调用点扫描。
   处理 //、/* *\/、单/双引号与模板串（模板串内 ${} 只按字面跳过即可，不承载契约）。 */
function scanJs(src) {
  const strings = [];
  const masked = src.split('');
  const n = src.length;
  let i = 0;
  const blank = (from, to) => { for (let k = from; k < to; k++) if (masked[k] !== '\n') masked[k] = ' '; };
  while (i < n) {
    const c = src[i];
    if (c === '/' && src[i + 1] === '/') {
      let j = i; while (j < n && src[j] !== '\n') j++;
      blank(i, j); i = j; continue;
    }
    if (c === '/' && src[i + 1] === '*') {
      let j = i + 2; while (j < n && !(src[j] === '*' && src[j + 1] === '/')) j++;
      blank(i, Math.min(j + 2, n)); i = Math.min(j + 2, n); continue;
    }
    if (c === '\'' || c === '"') {
      const quote = c; let j = i + 1; let val = '';
      while (j < n) {
        const d = src[j];
        if (d === '\\') { val += src[j + 1] === undefined ? '' : src[j + 1]; j += 2; continue; }
        if (d === quote || d === '\n') break;
        val += d; j++;
      }
      strings.push({ value: val, line: src.slice(0, i).split('\n').length });
      i = j + 1; continue;
    }
    if (c === '`') {
      let j = i + 1; let val = '';
      while (j < n) {
        if (src[j] === '\\') { val += src[j + 1] === undefined ? '' : src[j + 1]; j += 2; continue; }
        if (src[j] === '`') break;
        val += src[j]; j++;
      }
      strings.push({ value: val, line: src.slice(0, i).split('\n').length });
      i = j + 1; continue;
    }
    i++;
  }
  return { strings: strings, masked: masked.join('') };
}

const scanned = scanJs(jsAll);
const apiLiterals = [];
for (const m of scanned.masked.matchAll(/\bapi(?:Cached)?\s*\(\s*(['"])([^'"]+)\1/g)) apiLiterals.push(m[2]);
for (const lit of apiLiterals) {
  if (/^https?:\/\//.test(lit)) { fail(`前端出现绝对 URL 请求：${lit}`); continue; }
  if (!API_PATH_PREFIXES.some(p => lit.startsWith(p))) {
    fail(`api() 路径不在白名单内：'${lit}'（新增端点请同步 API_PATH_PREFIXES）`);
  }
}
// 完整面板路径只能出现在集中前缀或白名单字面量里（注释不计）。
for (const s of scanned.strings) {
  if (!s.value.includes('/panel/api/')) continue;
  if (s.value === '/panel/api/') continue;
  if (ALLOWED_PANEL_PATH_LITERALS.has(s.value)) continue;
  fail(`硬编码完整面板路径 ${s.value}（第 ${s.line} 行；应走 api('相对路径')，或在 ALLOWED_PANEL_PATH_LITERALS 登记原因）`);
}
for (const s of scanned.strings) {
  if (/https?:\/\//.test(s.value)) fail(`前端不允许外部绝对 URL：${s.value}（第 ${s.line} 行）`);
}
// 硬编码密钥形态（sk- 前缀的长随机串）不应出现在前端。
for (const s of scanned.strings) {
  const m = s.value.match(/\bsk-[A-Za-z0-9_-]{16,}/);
  if (m) fail(`前端出现疑似硬编码 API key：${m[0].slice(0, 12)}…（第 ${s.line} 行）`);
}

// ── 4) 体积预算（app.js = web/*.js 拼接后的实际响应体）────────────────────
for (const [label, bytes, budget] of [
  ['app.js', Buffer.byteLength(appJs, 'utf8'), SIZE_BUDGET.appJs],
  ['mobile.js', Buffer.byteLength(mobileJs, 'utf8'), SIZE_BUDGET.mobileJs],
  ['index.html', Buffer.byteLength(indexHtml, 'utf8'), SIZE_BUDGET.indexHtml],
]) {
  if (bytes >= budget) {
    fail(`${label} 体积超预算：${kb(bytes)}（上限 ${kb(budget)}）`);
  }
}

// ── 结果 ────────────────────────────────────────────────────────────────
function report() {
  for (const e of errors) console.error('FAIL: ' + e);
  console.error(`panel frontend check: FAILED (${errors.length} 项)`);
  process.exit(1);
}
if (errors.length) report();
const sizes = {
  appJs: Buffer.byteLength(appJs, 'utf8'),
  mobileJs: Buffer.byteLength(mobileJs, 'utf8'),
  indexHtml: Buffer.byteLength(indexHtml, 'utf8'),
};
console.log(`panel frontend check: OK (app.js=${kb(sizes.appJs)} [${appModules.files.length} modules], mobile.js=${kb(sizes.mobileJs)}, index.html=${kb(sizes.indexHtml)})`);
process.exit(0);
