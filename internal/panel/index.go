// index.go 面板静态资源与安全响应头。
//
// 资源经 go:embed 打进二进制（随服务部署，无外部构建步骤）：
//   - index.html  页面骨架
//   - web/*.js    全部前端逻辑的源码模块（v2.6.1：按业务拆分，启动时按文件名排序
//     拼接为单个 app.js 提供给浏览器——URL/加载顺序/CSP/缓存行为完全不变）
//   - mobile.css  手机端（≤760px）适配样式（本仓库自有，上游不维护 → 同步上游不冲突）
//   - mobile.js   手机端联动逻辑（抽屉开合、宽表卡片化字段名），须在 app.js 之后加载
//
// 安全头对"面板页面与全部 /panel/api/* 响应"统一生效：CSP 限制脚本只能来自本服务，
// 禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，声明不收搜索引擎（noindex），
// 并声明不泄露 Referer 出去。
//
// 缓存（v2.0.1 #2）：app.js / mobile.js / mobile.css 内容随二进制固定 → 启动时按
// SHA-256 算强 ETag + 1 小时公共缓存（must-revalidate 兜底），If-None-Match 命中回
// 304；index.html 是"必须即时更新"的页面骨架 → 弱 ETag + Cache-Control: no-cache。
package panel

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"sort"
	"strings"
)

//go:embed index.html
var indexHTML []byte

// appJSSources 是前端逻辑的源码模块（internal/panel/web/*.js）。模块本身不直接对外，
// 只用于在包初始化时拼接出单一 app.js；模块边界即原文件的业务分区。
//
//go:embed web/*.js
var appJSSources embed.FS

// appJS 把 web/*.js 按文件名字典序拼成单一前端脚本，作为 /panel/app.js 的响应体。
// 顺序有意义：模块间共享全局状态，必须保持拆分前的原始执行顺序（文件名前缀保证）。
var appJS = concatAppJSSources()

// concatAppJSSources 拼接前端模块；内容来自 go:embed（编译期固定），失败属构建错误。
func concatAppJSSources() []byte {
	names, err := fs.Glob(appJSSources, "web/*.js")
	if err != nil {
		panic("panel: 读取 web/*.js 模块失败: " + err.Error())
	}
	if len(names) == 0 {
		panic("panel: web/*.js 模块为空")
	}
	sort.Strings(names)
	var buf bytes.Buffer
	for _, name := range names {
		b, err := fs.ReadFile(appJSSources, name)
		if err != nil {
			panic("panel: 读取 " + name + " 失败: " + err.Error())
		}
		buf.Write(b)
	}
	return buf.Bytes()
}

//go:embed mobile.css
var mobileCSS []byte

//go:embed mobile.js
var mobileJS []byte

// 静态资源 ETag 在包初始化时按嵌入内容计算（内容随二进制发布，运行期不变）：
// 强 ETag 供脚本/样式使用（内容变化 → ETag 变 → 浏览器重新下载）；
// index.html 用弱 ETag + no-cache（每次使用前回源校验，改版立即生效）。
var (
	indexHTMLEtag = weakAssetETag(indexHTML)
	appJSEtag     = assetETag(appJS)
	mobileCSSEtag = assetETag(mobileCSS)
	mobileJSEtag  = assetETag(mobileJS)
)

// 缓冲策略常量：
//   - 脚本/样式内容随二进制固定，允许浏览器缓存 1 小时；must-revalidate 保证过期后
//     必须回来校验（ETag 命中 304 只花一个往返，改版后最多一次旧内容窗口）。
//   - index.html 必须即时更新（面板功能依赖新页面结构），no-cache 每次校验弱 ETag。
const (
	cacheControlStatic = "public, max-age=3600, must-revalidate"
	cacheControlIndex  = "no-cache"
)

// assetETag 返回强 ETag `"sha256-<hex>"`。
func assetETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"sha256-` + hex.EncodeToString(sum[:]) + `"`
}

// weakAssetETag 返回弱 ETag `W/"sha256-<hex>"`（语义：内容等价即可复用）。
func weakAssetETag(body []byte) string {
	return "W/" + assetETag(body)
}

// ifNoneMatch 判断 If-None-Match 是否命中给定 ETag（GET/HEAD 用弱比较：
// 忽略 W/ 前缀；支持多值与 "*"）。
func ifNoneMatch(r *http.Request, etag string) bool {
	inm := r.Header.Get("If-None-Match")
	if inm == "" {
		return false
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, part := range strings.Split(inm, ",") {
		p := strings.TrimSpace(part)
		if p == "*" {
			return true
		}
		if strings.TrimPrefix(p, "W/") == want {
			return true
		}
	}
	return false
}

// servePanelAsset 输出内嵌静态资源：先按 ETag 判断 304，再写出内容与缓存头。
// 304 响应不带 Content-Type/Content-Length（RFC 9110 允许省略），但保留 ETag、
// Cache-Control 与 Vary 供浏览器更新缓存元数据。
func servePanelAsset(w http.ResponseWriter, r *http.Request, body []byte, contentType, etag, cacheControl string) {
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", cacheControl)
	if ifNoneMatch(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// csp 内容安全策略（严格版，无需 unsafe-inline）：
//   - default-src 'none'        默认全禁，逐个开口
//   - script-src 'self'         只跑同源脚本（app.js / mobile.js）；页面无内联事件处理器/内联脚本
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是设计取舍：页面有少量 style="..." 属性（进度条宽度、表格列宽），
//     允许内联样式不会导致脚本执行；仍禁止外部样式域与 @import 外链。
//   - connect-src 'self'        前端 fetch 只能打本服务
//   - img-src 'self' data:      图标/内联图
//   - form-action 'none'        页面无表单提交目标（配置页是 JS 提交）
//   - frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
//   - base-uri 'none'          禁止注入 <base> 改写相对路径
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setSecurityHeaders 写入面板统一安全响应头（页面与 API 都要，API 也含 JSON 数据）。
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff") // 禁 MIME 嗅探
	w.Header().Set("X-Frame-Options", "DENY")           // 老浏览器兜底（CSP frame-ancestors 的等价项）
	w.Header().Set("Referrer-Policy", "no-referrer")    // 不外泄面板地址给外部站点
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	// 面板是管理界面，不该被搜索引擎收录（v2.0.1 #2）。
	w.Header().Set("X-Robots-Tag", "noindex")
}

// index 输出面板页面（静态无秘密；数据接口 /panel/api/* 才走鉴权）。
// no-cache + 弱 ETag：页面结构随版本更新，必须每次回源校验（304 时零传输）。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	servePanelAsset(w, r, indexHTML, "text/html; charset=utf-8", indexHTMLEtag, cacheControlIndex)
}

// appScript 输出前端逻辑（同源脚本，供 CSP script-src 'self' 加载）。
func (p *Panel) appScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	servePanelAsset(w, r, appJS, "text/javascript; charset=utf-8", appJSEtag, cacheControlStatic)
}

// mobileStyle 输出手机端适配样式（同源样式表，CSP style-src 'self' 加载）。
// 单独成文而非写进 index.html：上游频繁改 index.html，独立文件可避免同步上游时反复冲突。
func (p *Panel) mobileStyle(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	servePanelAsset(w, r, mobileCSS, "text/css; charset=utf-8", mobileCSSEtag, cacheControlStatic)
}

// mobileScript 输出手机端联动逻辑（同源脚本，须在 app.js 之后加载）。
func (p *Panel) mobileScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	servePanelAsset(w, r, mobileJS, "text/javascript; charset=utf-8", mobileJSEtag, cacheControlStatic)
}
