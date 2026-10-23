package panel

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/livecfg"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
)

func newTestPanel() *Panel {
	// 启用鉴权：未带 key 的请求一律 401，不进入依赖 Pool/Upstream 的 handler。
	return New(Config{Version: "test", APIKey: "test-key"})
}

// 面板安全响应头必须覆盖：页面、静态脚本、鉴权失败响应。
func TestSecurityHeadersOnAllPanelResponses(t *testing.T) {
	p := newTestPanel()
	paths := []struct{ method, path string }{
		{"GET", "/panel/"},
		{"GET", "/panel/app.js"},
		{"GET", "/panel/api/overview"}, // 401（未提供 key）
		{"POST", "/panel/api/config"},  // 401
		{"GET", "/panel/api/nonexistent"},
	}
	for _, c := range paths {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		h := rec.Header()
		if got := h.Get("Content-Security-Policy"); got == "" {
			t.Errorf("%s %s: missing CSP", c.method, c.path)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s %s: X-Content-Type-Options=%q", c.method, c.path, h.Get("X-Content-Type-Options"))
		}
		if h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s %s: X-Frame-Options=%q", c.method, c.path, h.Get("X-Frame-Options"))
		}
		if h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s %s: Referrer-Policy=%q", c.method, c.path, h.Get("Referrer-Policy"))
		}
	}
}

// CSP 必须禁止内联脚本与 iframe 嵌套（严格策略的核心约束）。
func TestCSPDisallowsInlineScriptAndFraming(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	csp := rec.Header().Get("Content-Security-Policy")

	for _, must := range []string{
		"script-src 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"default-src 'none'",
	} {
		if !strings.Contains(csp, must) {
			t.Errorf("CSP missing %q; got: %s", must, csp)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Errorf("CSP must not allow unsafe-inline scripts; got: %s", csp)
	}
}

// 页面必须引用外部脚本（内联脚本会被上面的 CSP 拦掉，页面将完全不可用）。
func TestIndexReferencesExternalScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `<script src="app.js"></script>`) {
		t.Error("index.html must load app.js externally (inline script is blocked by CSP)")
	}
	// 反例保护：出现内联 <script>...</script> 内容块即为回归
	if strings.Contains(body, "<script>\n") || strings.Contains(body, "<script> ") {
		t.Error("index.html still contains an inline <script> block; CSP would block it")
	}
}

// app.js 必须能作为同源脚本取到且类型正确（否则页面白屏）。
func TestAppScriptServed(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type=%q want javascript", ct)
	}
	if !strings.Contains(rec.Body.String(), "'use strict'") {
		t.Error("app.js body looks wrong")
	}
}

// UID 白名单：拒绝路径穿越与异常字符，放行真实 UUID 形态。
func TestValidUID(t *testing.T) {
	ok := []string{
		"248890d9-bb26-4131-87a7-4ec74d472344",
		"abc_123-XYZ",
		"a",
	}
	bad := []string{
		"",
		"../../evil",
		"x/../../y",
		`..\..\evil`,
		"a/b",
		"a\\b",
		"uid with space",
		"uid\nnewline",
		"uid\x00null",
		"café",
		strings.Repeat("a", 65), // 超长
	}
	for _, u := range ok {
		if !validUID(u) {
			t.Errorf("validUID(%q) = false, want true", u)
		}
	}
	for _, u := range bad {
		if validUID(u) {
			t.Errorf("validUID(%q) = true, want false", u)
		}
	}
}

// 未带密钥的 API 请求必须 401；携带正确密钥则通过鉴权层（不再是 401）。
func TestAuthLayerBehavior(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: code=%d want 401", rec.Code)
	}
	// 用不存在的路由验证"带正确 key 已过鉴权"（避免触碰依赖 nil 的 handler）。
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/panel/api/nonexistent", nil)
	req2.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusUnauthorized {
		t.Error("valid key must pass the auth layer")
	}
}

// ---------------------------------------------------------------------------
// v2.0.1 #4-1：面板密钥与网关密钥分离
// ---------------------------------------------------------------------------

// panelRequest 构造并执行一次面板请求：key 非空时带 Bearer，hdr 追加请求头，
// remoteAddr 非空时覆盖 TCP 对端（限速测试用）。
func panelRequest(p *Panel, method, path, key, remoteAddr string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// panelStatus 执行请求并返回状态码（/panel/api/config 在 LoadConfig=nil 时回 501，
// 用来验证"已通过鉴权层"——不依赖 Pool/Upstream 的重 handler）。
func panelStatus(p *Panel, method, path, key, remoteAddr string, hdr map[string]string) int {
	return panelRequest(p, method, path, key, remoteAddr, hdr).Code
}

// gunzipBody 解压 gzip 响应体（失败即测试失败）。
func gunzipBody(t *testing.T, raw []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	return out
}

// TestPanelAPIKeySeparatedFromGatewayKey 面板独立密钥（panel.api_key）生效：
// /panel/* 只认面板 key，网关 api_key 进不来；两者都不带 → 401。
func TestPanelAPIKeySeparatedFromGatewayKey(t *testing.T) {
	p := New(Config{Version: "test", PanelAPIKey: "panel-key", APIKey: "gateway-key"})
	if got := panelStatus(p, "GET", "/panel/api/config", "panel-key", "", nil); got != http.StatusNotImplemented {
		t.Fatalf("panel key status=%d want 501（已通过鉴权）", got)
	}
	if got := panelStatus(p, "GET", "/panel/api/config", "gateway-key", "", nil); got != http.StatusUnauthorized {
		t.Fatalf("gateway key on panel status=%d want 401（密钥必须分离）", got)
	}
	if got := panelStatus(p, "GET", "/panel/api/config", "", "", nil); got != http.StatusUnauthorized {
		t.Fatalf("no key status=%d want 401", got)
	}
}

// TestPanelAPIKeyFallbackToGateway 未配置 panel.api_key 时回落到网关 key（向后兼容）：
// 静态字段与 Live 快照两种来源都要生效。
func TestPanelAPIKeyFallbackToGateway(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "gateway-key"})
	if got := panelStatus(p, "GET", "/panel/api/config", "gateway-key", "", nil); got != http.StatusNotImplemented {
		t.Fatalf("static gateway key status=%d want 501", got)
	}

	live := livecfg.New(livecfg.Snapshot{APIKey: "live-key"})
	p2 := New(Config{Version: "test", APIKey: "static-key", Live: live})
	if got := panelStatus(p2, "GET", "/panel/api/config", "live-key", "", nil); got != http.StatusNotImplemented {
		t.Fatalf("live gateway key status=%d want 501", got)
	}
	if got := panelStatus(p2, "GET", "/panel/api/config", "static-key", "", nil); got != http.StatusUnauthorized {
		t.Fatalf("static key overridden by live status=%d want 401", got)
	}
}

// TestPanelAPIKeyOverridesLive panel.api_key 优先级最高：即使 Live 快照里有网关 key，
// 面板也只认独立面板密钥。
func TestPanelAPIKeyOverridesLive(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{APIKey: "gateway-key"})
	p := New(Config{Version: "test", PanelAPIKey: "panel-key", APIKey: "gateway-key", Live: live})
	if got := panelStatus(p, "GET", "/panel/api/config", "panel-key", "", nil); got != http.StatusNotImplemented {
		t.Fatalf("panel key status=%d want 501", got)
	}
	if got := panelStatus(p, "GET", "/panel/api/config", "gateway-key", "", nil); got != http.StatusUnauthorized {
		t.Fatalf("gateway key on panel status=%d want 401", got)
	}
}

// ---------------------------------------------------------------------------
// v2.0.1 #4-3：鉴权失败按 IP 限速（20 次/分钟 → 429 + Retry-After）
// ---------------------------------------------------------------------------

// TestAuthFailureRateLimit 同一 IP 连续鉴权失败超过 20 次/分钟 → 429 + Retry-After；
// 限速只作用于 /panel/api/*，面板页面仍可匿名加载。
func TestAuthFailureRateLimit(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "secret-key"})
	const ip = "203.0.113.7:5555"
	for i := 0; i < authFailLimit; i++ {
		if got := panelStatus(p, "GET", "/panel/api/config", "", ip, nil); got != http.StatusUnauthorized {
			t.Fatalf("failure #%d status=%d want 401", i+1, got)
		}
	}
	rec := panelRequest(p, "GET", "/panel/api/config", "", ip, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limited status=%d want 429", rec.Code)
	}
	ra := rec.Header().Get("Retry-After")
	if ra == "" {
		t.Fatal("429 must carry Retry-After")
	}
	if n, err := strconv.Atoi(ra); err != nil || n < 1 || n > 60 {
		t.Fatalf("Retry-After=%q want 1-60 秒", ra)
	}
	if !strings.Contains(rec.Body.String(), "too many") {
		t.Fatalf("429 body should explain rate limiting: %s", rec.Body.String())
	}

	// 页面不是 /panel/api/*：被限 IP 仍能打开面板（否则用户会误以为面板挂了）。
	if got := panelStatus(p, "GET", "/panel/", "", ip, nil); got != http.StatusOK {
		t.Fatalf("page status=%d want 200（限速只作用于 API）", got)
	}
}

// TestAuthSuccessClearsRateLimit 鉴权成功一次清桶：正常客户端不会因同 IP 的历史失败
// 被持续拖累（清桶后重新拥有完整 20 次失败额度）。
func TestAuthSuccessClearsRateLimit(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "secret-key"})
	for i := 0; i < 10; i++ {
		if got := panelStatus(p, "GET", "/panel/api/config", "", "", nil); got != http.StatusUnauthorized {
			t.Fatalf("warmup failure #%d status=%d want 401", i+1, got)
		}
	}
	// 成功一次 → 过鉴权（501 = 已进入 handler）且清空失败桶。
	if got := panelStatus(p, "GET", "/panel/api/config", "secret-key", "", nil); got != http.StatusNotImplemented {
		t.Fatalf("success status=%d want 501", got)
	}
	// 清桶后应重新允许完整 20 次失败（若没清，10+11=21 时就会 429）。
	for i := 0; i < authFailLimit; i++ {
		if got := panelStatus(p, "GET", "/panel/api/config", "", "", nil); got != http.StatusUnauthorized {
			t.Fatalf("post-success failure #%d status=%d want 401（成功应清桶）", i+1, got)
		}
	}
	if got := panelStatus(p, "GET", "/panel/api/config", "", "", nil); got != http.StatusTooManyRequests {
		t.Fatalf("21st post-success failure status=%d want 429", got)
	}
}

// TestAuthFailureRateLimitPerIP 限速按来源 IP 隔离：一个 IP 被限不影响其他 IP。
func TestAuthFailureRateLimitPerIP(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "secret-key"})
	const badIP = "203.0.113.7:5555"
	for i := 0; i < authFailLimit; i++ {
		panelRequest(p, "GET", "/panel/api/config", "", badIP, nil)
	}
	if got := panelStatus(p, "GET", "/panel/api/config", "", badIP, nil); got != http.StatusTooManyRequests {
		t.Fatalf("bad ip status=%d want 429", got)
	}
	// 另一个 IP 用正确 key 完全不受影响。
	if got := panelStatus(p, "GET", "/panel/api/config", "secret-key", "198.51.100.9:6666", nil); got != http.StatusNotImplemented {
		t.Fatalf("other ip valid key status=%d want 501", got)
	}
	// 另一个 IP 无 key 仍是普通 401（尚未超限）。
	if got := panelStatus(p, "GET", "/panel/api/config", "", "198.51.100.9:6666", nil); got != http.StatusUnauthorized {
		t.Fatalf("other ip failure status=%d want 401", got)
	}
}

// TestRateLimitIPExtraction 来源 IP 口径：X-Forwarded-For 首段 → X-Real-IP → TCP 对端
// （与 server/logging.go 的 clientIPForLog 一致）。
func TestRateLimitIPExtraction(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	if got := rateLimitIP(r); got != "192.0.2.1" {
		t.Errorf("tcp peer ip=%q want 192.0.2.1", got)
	}
	r.Header.Set("X-Real-IP", "10.0.0.1")
	if got := rateLimitIP(r); got != "10.0.0.1" {
		t.Errorf("x-real-ip=%q want 10.0.0.1", got)
	}
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")
	if got := rateLimitIP(r); got != "1.2.3.4" {
		t.Errorf("xff first hop=%q want 1.2.3.4", got)
	}
	if got := rateLimitIP(nil); got != "" {
		t.Errorf("nil request ip=%q want empty", got)
	}
}

// TestAuthRateLimitUsesForwardedIP 反代场景按真实客户端（XFF 首段）限速：
// 同一代理出口下，一个真实客户端被限不会波及另一个。
func TestAuthRateLimitUsesForwardedIP(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "secret-key"})
	xff := map[string]string{"X-Forwarded-For": "203.0.113.50"}
	for i := 0; i < authFailLimit; i++ {
		if got := panelStatus(p, "GET", "/panel/api/config", "", "10.0.0.1:1234", xff); got != http.StatusUnauthorized {
			t.Fatalf("proxied failure #%d status=%d want 401", i+1, got)
		}
	}
	if got := panelStatus(p, "GET", "/panel/api/config", "", "10.0.0.1:1234", xff); got != http.StatusTooManyRequests {
		t.Fatalf("same forwarded ip status=%d want 429", got)
	}
	other := map[string]string{"X-Forwarded-For": "203.0.113.51"}
	if got := panelStatus(p, "GET", "/panel/api/config", "", "10.0.0.1:1234", other); got != http.StatusUnauthorized {
		t.Fatalf("different forwarded ip status=%d want 401（限速键取 XFF 首段）", got)
	}
}

// TestAuthFailLimiterLRU LRU 上限：超过上限淘汰最旧 IP；成功清桶删除条目。
func TestAuthFailLimiterLRU(t *testing.T) {
	l := newAuthFailLimiter(3)
	for _, ip := range []string{"a", "b", "c"} {
		l.fail(ip)
	}
	l.fail("d") // 超上限 → 淘汰最旧的 a
	l.mu.Lock()
	_, hasA := l.buckets["a"]
	size := len(l.buckets)
	l.mu.Unlock()
	if hasA || size != 3 {
		t.Fatalf("LRU cap: buckets=%d hasA=%v want 3/false", size, hasA)
	}
	l.success("b")
	l.mu.Lock()
	_, hasB := l.buckets["b"]
	size = len(l.buckets)
	l.mu.Unlock()
	if hasB || size != 2 {
		t.Fatalf("success clear: buckets=%d hasB=%v want 2/false", size, hasB)
	}
}

// TestAuthFailLimiterRefill token bucket 按时间回补：耗尽 → blocked；回补 1 个令牌
// 后放行一次；再次失败立刻重新 blocked。
func TestAuthFailLimiterRefill(t *testing.T) {
	l := newAuthFailLimiter(100)
	now := time.Now()
	l.now = func() time.Time { return now }
	for i := 0; i < authFailLimit; i++ {
		l.fail("ip")
	}
	if blocked, _ := l.blocked("ip"); !blocked {
		t.Fatal("exhausted bucket should be blocked")
	}
	now = now.Add(3 * time.Second) // 3s × (20/60 tokens/s) = 1 个令牌
	if blocked, _ := l.blocked("ip"); blocked {
		t.Fatal("bucket should refill one token after 3s")
	}
	l.fail("ip") // 用掉刚回补的令牌
	if blocked, _ := l.blocked("ip"); !blocked {
		t.Fatal("bucket should be blocked again after consuming the refilled token")
	}
}

// ---------------------------------------------------------------------------
// v2.0.1 #2：gzip 压缩 + 静态资源缓存 + X-Robots-Tag + overview 轮询间隔
// ---------------------------------------------------------------------------

// TestGzipJSONResponse 面板 JSON 接口 gzip：Content-Encoding/Vary/解压后 JSON 等价，
// 不带 Accept-Encoding 时不压缩。
func TestGzipJSONResponse(t *testing.T) {
	pl := pool.New("")
	for i := 0; i < 60; i++ {
		pl.Add(&auth.Auth{UID: fmt.Sprintf("uid-%03d", i), Nickname: fmt.Sprintf("nick-%03d", i)})
	}
	p := New(Config{Version: "test", Pool: pl})

	plain := panelRequest(p, "GET", "/panel/api/overview", "", "", nil)
	if plain.Code != http.StatusOK {
		t.Fatalf("plain overview status=%d body=%s", plain.Code, plain.Body.String())
	}
	if ce := plain.Header().Get("Content-Encoding"); ce != "" {
		t.Fatalf("plain response Content-Encoding=%q want empty", ce)
	}
	if plain.Body.Len() <= gzipMinSize {
		t.Fatalf("测试数据太小（%d B），无法验证 >1KB 压缩阈值", plain.Body.Len())
	}

	compressed := panelRequest(p, "GET", "/panel/api/overview", "", "", map[string]string{"Accept-Encoding": "gzip, deflate, br"})
	if compressed.Code != http.StatusOK {
		t.Fatalf("compressed overview status=%d", compressed.Code)
	}
	if got := compressed.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding=%q want gzip", got)
	}
	if got := compressed.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("Vary=%q must include Accept-Encoding", got)
	}
	if got := compressed.Header().Get("Content-Length"); got != "" {
		t.Fatalf("compressed response must drop Content-Length, got %q", got)
	}
	if compressed.Body.Len() >= plain.Body.Len() {
		t.Fatalf("gzip body %d B not smaller than plain %d B", compressed.Body.Len(), plain.Body.Len())
	}
	decoded := gunzipBody(t, compressed.Body.Bytes())
	var resp struct {
		Version         string            `json:"version"`
		Total           int               `json:"total"`
		PollIntervalSec int               `json:"poll_interval_sec"`
		Accounts        []json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(decoded, &resp); err != nil {
		t.Fatalf("gunzipped overview is not valid JSON: %v", err)
	}
	if resp.Version != "test" || resp.Total != 60 || len(resp.Accounts) != 60 {
		t.Fatalf("gunzipped overview mismatch: version=%q total=%d accounts=%d", resp.Version, resp.Total, len(resp.Accounts))
	}

	// HTTP/1.1 客户端常见的 q 值：q=0 显式拒绝 gzip；q=0.5 仍然压缩。
	q0 := panelRequest(p, "GET", "/panel/api/overview", "", "", map[string]string{"Accept-Encoding": "gzip;q=0"})
	if got := q0.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("gzip;q=0 must not compress, got Content-Encoding=%q", got)
	}
	q00 := panelRequest(p, "GET", "/panel/api/overview", "", "", map[string]string{"Accept-Encoding": "gzip; q=0.0"})
	if got := q00.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("gzip;q=0.0 must not compress, got Content-Encoding=%q", got)
	}
	qhalf := panelRequest(p, "GET", "/panel/api/overview", "", "", map[string]string{"Accept-Encoding": "gzip;q=0.5"})
	if got := qhalf.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("gzip;q=0.5 should compress, got Content-Encoding=%q", got)
	}
}

// TestGzipStaticAssets 静态资源 gzip（app.js/index.html >1KB）；HEAD 与小响应（401）
// 跳过压缩；304 无实体不压缩。
func TestGzipStaticAssets(t *testing.T) {
	p := newTestPanel()
	rec := panelRequest(p, "GET", "/panel/app.js", "", "", map[string]string{"Accept-Encoding": "gzip"})
	if rec.Code != http.StatusOK {
		t.Fatalf("app.js status=%d", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("app.js Content-Encoding=%q want gzip", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("app.js Content-Type=%q", ct)
	}
	if got := gunzipBody(t, rec.Body.Bytes()); !bytes.Equal(got, appJS) {
		t.Fatalf("gunzipped app.js mismatch: %d vs %d bytes", len(got), len(appJS))
	}

	idx := panelRequest(p, "GET", "/panel/", "", "", map[string]string{"Accept-Encoding": "gzip"})
	if got := idx.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("index.html Content-Encoding=%q want gzip", got)
	}
	if got := gunzipBody(t, idx.Body.Bytes()); !bytes.Equal(got, indexHTML) {
		t.Fatalf("gunzipped index.html mismatch")
	}

	// HEAD：无实体，不应出现 Content-Encoding，也不写 body。
	head := panelRequest(p, "HEAD", "/panel/app.js", "", "", map[string]string{"Accept-Encoding": "gzip"})
	if got := head.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("HEAD Content-Encoding=%q want empty", got)
	}
	if head.Body.Len() != 0 {
		t.Fatalf("HEAD body length=%d want 0", head.Body.Len())
	}

	// 小响应（<1KB 的 401 JSON）不压缩。
	small := panelRequest(p, "GET", "/panel/api/config", "", "", map[string]string{"Accept-Encoding": "gzip"})
	if small.Code != http.StatusUnauthorized {
		t.Fatalf("small response status=%d want 401", small.Code)
	}
	if got := small.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("small response Content-Encoding=%q want empty（<1KB 不压缩）", got)
	}
}

// TestStaticAssetETagAndCache 静态资源 ETag = 内容 SHA-256；脚本/样式 Cache-Control
// 1 小时 + must-revalidate；index.html 弱 ETag + no-cache；If-None-Match 命中 304。
func TestStaticAssetETagAndCache(t *testing.T) {
	p := newTestPanel()
	sum := sha256.Sum256(appJS)
	wantETag := `"sha256-` + hex.EncodeToString(sum[:]) + `"`

	rec := panelRequest(p, "GET", "/panel/app.js", "", "", nil)
	if got := rec.Header().Get("ETag"); got != wantETag {
		t.Fatalf("app.js ETag=%q want %q", got, wantETag)
	}
	if got := rec.Header().Get("Cache-Control"); got != cacheControlStatic {
		t.Fatalf("app.js Cache-Control=%q want %q", got, cacheControlStatic)
	}

	// If-None-Match 命中（多值 + 前导无关 ETag）→ 304，无 body，保留缓存元数据。
	rec304 := panelRequest(p, "GET", "/panel/app.js", "", "", map[string]string{
		"If-None-Match": `"stale-etag", ` + wantETag,
	})
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("app.js 304 status=%d want 304", rec304.Code)
	}
	if rec304.Body.Len() != 0 {
		t.Fatalf("304 body length=%d want 0", rec304.Body.Len())
	}
	if got := rec304.Header().Get("ETag"); got != wantETag {
		t.Fatalf("304 ETag=%q want %q", got, wantETag)
	}
	if got := rec304.Header().Get("Cache-Control"); got != cacheControlStatic {
		t.Fatalf("304 Cache-Control=%q want %q", got, cacheControlStatic)
	}

	// index.html：弱 ETag + no-cache（页面骨架必须即时更新）。
	idx := panelRequest(p, "GET", "/panel/", "", "", nil)
	idxETag := idx.Header().Get("ETag")
	if !strings.HasPrefix(idxETag, `W/"sha256-`) {
		t.Fatalf("index.html ETag=%q want weak W/\"sha256-...\"", idxETag)
	}
	if got := idx.Header().Get("Cache-Control"); got != cacheControlIndex {
		t.Fatalf("index.html Cache-Control=%q want %q", got, cacheControlIndex)
	}
	idx304 := panelRequest(p, "GET", "/panel/", "", "", map[string]string{"If-None-Match": idxETag})
	if idx304.Code != http.StatusNotModified || idx304.Body.Len() != 0 {
		t.Fatalf("index.html 304 status=%d body=%d", idx304.Code, idx304.Body.Len())
	}

	// mobile.css / mobile.js 与 app.js 同策略（强 ETag + 1 小时缓存）。
	for _, path := range []string{"/panel/mobile.css", "/panel/mobile.js"} {
		r := panelRequest(p, "GET", path, "", "", nil)
		if r.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, r.Code)
		}
		if et := r.Header().Get("ETag"); !strings.HasPrefix(et, `"sha256-`) {
			t.Fatalf("%s ETag=%q want strong sha256 ETag", path, et)
		}
		if cc := r.Header().Get("Cache-Control"); cc != cacheControlStatic {
			t.Fatalf("%s Cache-Control=%q want %q", path, cc, cacheControlStatic)
		}
	}

	// 带 gzip 的 304：无实体，不应出现 Content-Encoding。
	rec304gz := panelRequest(p, "GET", "/panel/app.js", "", "", map[string]string{
		"If-None-Match":   wantETag,
		"Accept-Encoding": "gzip",
	})
	if rec304gz.Code != http.StatusNotModified {
		t.Fatalf("304+gzip status=%d", rec304gz.Code)
	}
	if got := rec304gz.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("304 Content-Encoding=%q want empty", got)
	}
}

// TestXRobotsNoindex 面板所有响应（页面/静态/API 401）都带 X-Robots-Tag: noindex。
func TestXRobotsNoindex(t *testing.T) {
	p := newTestPanel()
	for _, path := range []string{"/panel/", "/panel/app.js", "/panel/mobile.js", "/panel/mobile.css", "/panel/api/config"} {
		rec := panelRequest(p, "GET", path, "", "", nil)
		if got := rec.Header().Get("X-Robots-Tag"); got != "noindex" {
			t.Errorf("%s X-Robots-Tag=%q want noindex", path, got)
		}
	}
}

// TestOverviewPollIntervalSec overview 下发 poll_interval_sec（v2.0.1 #2 前端据此轮询）：
// 静态配置生效；Live 快照优先（保存配置热生效）；未配置回落默认 5。
func TestOverviewPollIntervalSec(t *testing.T) {
	readPoll := func(p *Panel) int {
		t.Helper()
		rec := panelRequest(p, "GET", "/panel/api/overview", "", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("overview status=%d", rec.Code)
		}
		var resp struct {
			PollIntervalSec int `json:"poll_interval_sec"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode overview: %v", err)
		}
		return resp.PollIntervalSec
	}

	if got := readPoll(New(Config{Version: "test", Pool: pool.New(""), PollIntervalSec: 7})); got != 7 {
		t.Fatalf("configured poll_interval_sec=%d want 7", got)
	}
	if got := readPoll(New(Config{Version: "test", Pool: pool.New("")})); got != defaultPollIntervalSec {
		t.Fatalf("default poll_interval_sec=%d want %d", got, defaultPollIntervalSec)
	}
	live := livecfg.New(livecfg.Snapshot{PollIntervalSec: 12})
	if got := readPoll(New(Config{Version: "test", Pool: pool.New(""), PollIntervalSec: 7, Live: live})); got != 12 {
		t.Fatalf("live poll_interval_sec=%d want 12（保存配置热生效）", got)
	}
}
