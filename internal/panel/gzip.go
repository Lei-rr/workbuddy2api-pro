// gzip.go 面板响应 gzip 中间件（v2.6.1 拆分自 panel.go，纯移动）。
// 缓冲后按内容类型/大小/Accept-Encoding 一次性压缩；SSE 在 ServeHTTP 显式绕过。
package panel

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// gzipMinSize 只压缩超过该字节数的响应：小 JSON/错误页压缩收益为负（头部开销
// 可能超过节省的字节），且会让 401/429 这类响应更难在抓包里直接读。
const gzipMinSize = 1024

// gzipWriterPool 复用 gzip.Writer（每次 Reset 到目标 writer），避免每次请求分配
// ~1.2MB 的压缩窗口/哈希表。Close 不归还底层 writer（Reset 已切断引用）。
var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(nil) },
}

// gzipBufPool 复用响应缓冲（v2.5.0 Perf-3）：大响应（首页/账号页，几十 KB~7MB）
// 的生产者是 json.Marshal 一次性 Write，若每次新建 bytes.Buffer 则反复扩容+GC。
// 缓冲在 close 写出后 Reset 归还；上限保护见 releaseGzipBuffer。
var gzipBufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// gzipBufMaxKeep 归还缓冲上限：超过 1MiB 的缓冲不还池（避免极少数全量 overview
// 的 7MB 缓冲长期常驻，把 sync.Pool 变成内存囤积点）；正常账号页 ~50-100KB 会复用。
const gzipBufMaxKeep = 1 << 20

// acquireGzipBuffer 取一个空缓冲（来自池）。
func acquireGzipBuffer() *bytes.Buffer {
	buf := gzipBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

// releaseGzipBuffer 归还缓冲（超上限直接丢弃，交给 GC）。
func releaseGzipBuffer(buf *bytes.Buffer) {
	if buf == nil {
		return
	}
	if buf.Cap() > gzipBufMaxKeep {
		return
	}
	buf.Reset()
	gzipBufPool.Put(buf)
}

// acceptsGzip 解析 Accept-Encoding，是否接受 gzip（含 q=0 显式拒绝）。
func acceptsGzip(r *http.Request) bool {
	if r == nil {
		return false
	}
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(enc), "gzip") {
			continue
		}
		// gzip;q=0 / q=0.0 表示显式拒绝；其余参数（无 q / q=0.5 / q=1）都可压缩。
		return !gzipQZero(params)
	}
	return false
}

// gzipQZero 判断 Accept-Encoding 编码参数里的 q 值是否为 0（= 显式拒绝）。
func gzipQZero(params string) bool {
	for _, p := range strings.Split(params, ";") {
		p = strings.TrimSpace(p)
		if !strings.HasPrefix(p, "q=") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(p[2:]), 64)
		return err == nil && q == 0
	}
	return false
}

// isCompressibleContentType 面板可压缩的内容类型（JSON 接口 + 页面/脚本/样式）。
// 不碰 text/event-stream、图片等（面板目前没有，但显式白名单能防未来新增接口时误压）。
func isCompressibleContentType(ct string) bool {
	mt, _, _ := strings.Cut(ct, ";")
	switch strings.ToLower(strings.TrimSpace(mt)) {
	case "application/json",
		"text/html",
		"text/javascript",
		"application/javascript",
		"text/css":
		return true
	}
	return false
}

// gzipResponseWriter 缓冲面板响应，在 close 时按内容类型/大小/Accept-Encoding
// 决定是否 gzip 后一次性写给真实 ResponseWriter。
//
// 为什么缓冲而不是边写边压：面板只有 /panel/api/events 一个流式接口（SSE），
// 它在 ServeHTTP 中显式绕过本包装；其余响应都能在写头前拿到完整长度（>1KB
// 阈值）并准确维护 Content-Length。
type gzipResponseWriter struct {
	http.ResponseWriter
	req    *http.Request
	buf    *bytes.Buffer
	status int
	wrote  bool
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	if g.wrote {
		return
	}
	g.status = status
	g.wrote = true
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	if !g.wrote {
		g.status = http.StatusOK
		g.wrote = true
	}
	if g.buf == nil { // 防御：非 ServeHTTP 路径直接构造时惰性取池
		g.buf = acquireGzipBuffer()
	}
	return g.buf.Write(p)
}

// close 把缓冲内容按需压缩并写出（ServeHTTP defer 调用；幂等）。
func (g *gzipResponseWriter) close(compressOK bool) {
	status := g.status
	if status == 0 {
		status = http.StatusOK
	}
	h := g.Header()
	// 缓冲归还池：body 需在归还前写出/压缩完（defer close 只调一次）。
	defer releaseGzipBuffer(g.buf)
	var body []byte
	if g.buf != nil {
		body = g.buf.Bytes()
	}

	noBody := g.req != nil && g.req.Method == http.MethodHead
	// 304/204 无实体；已编码（如上游/未来中间件自己压过）不二次压缩。
	compressible := compressOK &&
		!noBody &&
		status != http.StatusNoContent &&
		status != http.StatusNotModified &&
		status != http.StatusPartialContent &&
		h.Get("Content-Encoding") == "" &&
		len(body) > gzipMinSize &&
		isCompressibleContentType(h.Get("Content-Type"))

	if compressible {
		// 压缩后长度不可预知：删掉可能存在的 Content-Length，由 net/http 决定
		// （小响应会自动计算，大响应走 chunked）。
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		g.ResponseWriter.WriteHeader(status)
		gz := gzipWriterPool.Get().(*gzip.Writer)
		gz.Reset(g.ResponseWriter)
		_, _ = gz.Write(body)
		_ = gz.Close()
		gzipWriterPool.Put(gz)
		return
	}

	g.ResponseWriter.WriteHeader(status)
	if noBody {
		return
	}
	_, _ = g.ResponseWriter.Write(body)
}
