#!/usr/bin/env node
/**
 * panel_frontend_check.mjs —— 面板前端工程化检查（v2.3.1 #18，零依赖）
 *
 * 在 CI 里把「Go 编译器看不见的前端契约」提前拦下来：
 *   1) 解析检查：app.js / mobile.js 必须能被 JS 解析器编译（等价 node --check）；
 *   2) DOM id 契约：index.html 的 id 与 app.js/mobile.js 里 $('id') 引用交叉检查
 *      （缺一侧报错；HTML 侧允许白名单——纯 CSS 锚点、按前缀/变量拼出来的元素等）；
 *   3) API 路径白名单：api()/apiCached() 的字面量路径必须命中已知前缀；不允许在
 *      前端硬编码完整 /panel/api/xxx 路径、绝对 URL 或疑似密钥字面量；
 *   4) 体积预算：app.js < 350KB、mobile.js < 25KB、index.html < 130KB。
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
  appJs: 'internal/panel/app.js',
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
   这里的每一项都对应 internal/panel 的既有路由或其动态子路径。
   v2.4.0（Lead 授权）：新增 events（SSE）与 auth/（login/logout/info）前缀。 */
const API_PATH_PREFIXES = [
  'accounts/',
  'accounts?',
  'activity_all',
  'audit?',
  'auth/',
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

// ── 读取 ────────────────────────────────────────────────────────────────
const appJs = read(FILES.appJs);
const mobileJs = read(FILES.mobileJs);
const indexHtml = read(FILES.indexHtml);
if (errors.length) { report(); }

// ── 1) 解析检查（等价 node --check）─────────────────────────────────────
for (const [name, src] of [['app.js', appJs], ['mobile.js', mobileJs]]) {
  try {
    new vm.Script(src, { filename: name });
  } catch (e) {
    fail(`${name} 解析失败：${e && e.message}`);
  }
}

// ── 2) DOM id 契约 ──────────────────────────────────────────────────────
const htmlIds = new Set();
for (const m of indexHtml.matchAll(/\bid="([^"]+)"/g)) htmlIds.add(m[1]);
const jsRefs = new Set();
const jsAll = appJs + '\n' + mobileJs;
for (const m of jsAll.matchAll(/\$\s*\(\s*(['"])([^'"]+)\1\s*\)/g)) jsRefs.add(m[2]);
for (const id of jsRefs) {
  if (!htmlIds.has(id) && !APP_ONLY_ID_ALLOW.has(id)) {
    fail(`app.js 引用了 index.html 中不存在的 id="${id}"（DOM id 契约）`);
  }
}
for (const id of htmlIds) {
  if (jsRefs.has(id) || HTML_ONLY_ID_ALLOW.has(id)) continue;
  if (HTML_ONLY_ID_PREFIX_ALLOW.some(p => id.startsWith(p))) continue;
  fail(`index.html 的 id="${id}" 未被 app.js 引用（DOM id 契约；如为纯样式锚点请加入 HTML_ONLY_ID_ALLOW）`);
}

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

// ── 4) 体积预算 ─────────────────────────────────────────────────────────
for (const [label, rel] of [['app.js', FILES.appJs], ['mobile.js', FILES.mobileJs], ['index.html', FILES.indexHtml]]) {
  const bytes = Buffer.byteLength(label === 'app.js' ? appJs : label === 'mobile.js' ? mobileJs : indexHtml, 'utf8');
  if (bytes >= SIZE_BUDGET[label === 'app.js' ? 'appJs' : label === 'mobile.js' ? 'mobileJs' : 'indexHtml']) {
    fail(`${label} 体积超预算：${kb(bytes)}（上限 ${kb(SIZE_BUDGET[label === 'app.js' ? 'appJs' : label === 'mobile.js' ? 'mobileJs' : 'indexHtml'])}）`);
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
console.log(`panel frontend check: OK (app.js=${kb(sizes.appJs)}, mobile.js=${kb(sizes.mobileJs)}, index.html=${kb(sizes.indexHtml)})`);
process.exit(0);
