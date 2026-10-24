// ratelimit.go 面板鉴权失败限速与来源 IP 提取（v2.6.1 拆分自 panel.go，纯移动）。
package panel

import (
	"container/list"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// ---------------------------------------------------------------------------
// 面板鉴权失败限速（v2.0.1 #4-3）
// ---------------------------------------------------------------------------

const (
	// authFailLimit 每个来源 IP 每分钟允许的鉴权失败次数（token bucket 容量）：
	// 超过后新请求回 429 + Retry-After；正常用户每分钟输错 20 次已远超现实。
	authFailLimit = 20
	// authFailWindow token bucket 回满周期 = 每分钟 20 个令牌。
	authFailWindow = time.Minute
	// authFailLimiterMaxIPs 内存 LRU 上限：1 万 IP，防止被伪造 XFF 刷爆内存。
	authFailLimiterMaxIPs = 10000
)

// authFailBucket 单个 IP 的令牌桶。
type authFailBucket struct {
	tokens float64
	last   time.Time
	elem   *list.Element // LRU 节点（front = 最近使用）
}

// authFailLimiter 按来源 IP 的鉴权失败 token bucket + LRU 上限，并发安全。
// 语义：
//   - blocked：该 IP 令牌耗尽 → 拒绝新请求（不消耗令牌），并给出 Retry-After 秒数；
//   - fail：鉴权失败消耗 1 个令牌（首次失败建桶，容量 authFailLimit）；
//   - success：鉴权成功清桶（正常客户端不被同 IP 的异常尝试拖累）。
//
// 状态全在内存：重启清零（可接受，攻击者也要重来）。
type authFailLimiter struct {
	mu      sync.Mutex
	maxIPs  int
	buckets map[string]*authFailBucket
	lru     *list.List
	now     func() time.Time // 测试可注入时钟
}

func newAuthFailLimiter(maxIPs int) *authFailLimiter {
	if maxIPs <= 0 {
		maxIPs = authFailLimiterMaxIPs
	}
	return &authFailLimiter{
		maxIPs:  maxIPs,
		buckets: make(map[string]*authFailBucket),
		lru:     list.New(),
		now:     time.Now,
	}
}

// refill 按经过时间补充令牌（上限 authFailLimit）；now 为零值时用当前时间。
func (l *authFailLimiter) refill(b *authFailBucket, now time.Time) {
	rate := float64(authFailLimit) / authFailWindow.Seconds() // 令牌/秒
	b.tokens += now.Sub(b.last).Seconds() * rate
	if b.tokens > authFailLimit {
		b.tokens = authFailLimit
	}
	if b.tokens < 0 {
		b.tokens = 0
	}
	b.last = now
}

// touch 把桶移到 LRU 头部（最近使用）。
func (l *authFailLimiter) touch(b *authFailBucket) {
	if b.elem != nil {
		l.lru.MoveToFront(b.elem)
	}
}

// lookup 取桶并刷新 LRU（不存在返回 nil，调用方持锁）。
func (l *authFailLimiter) lookup(ip string) *authFailBucket {
	b := l.buckets[ip]
	if b != nil {
		l.touch(b)
	}
	return b
}

// getOrCreate 取桶，必要时新建并按 LRU 上限淘汰最旧 IP（调用方持锁）。
func (l *authFailLimiter) getOrCreate(ip string, now time.Time) *authFailBucket {
	if b := l.buckets[ip]; b != nil {
		l.touch(b)
		return b
	}
	b := &authFailBucket{tokens: authFailLimit, last: now}
	b.elem = l.lru.PushFront(ip)
	l.buckets[ip] = b
	for len(l.buckets) > l.maxIPs {
		oldest := l.lru.Back()
		if oldest == nil {
			break
		}
		oldIP, _ := oldest.Value.(string)
		l.lru.Remove(oldest)
		delete(l.buckets, oldIP)
	}
	return b
}

// blocked 该 IP 当前是否被限速；返回建议的 Retry-After 秒数（最小 1）。
func (l *authFailLimiter) blocked(ip string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.lookup(ip)
	if b == nil {
		return false, 0
	}
	now := l.now()
	l.refill(b, now)
	if b.tokens >= 1 {
		return false, 0
	}
	// 距离攒够 1 个令牌的时间 = (1 - tokens) / rate。
	rate := float64(authFailLimit) / authFailWindow.Seconds()
	wait := time.Duration((1 - b.tokens) / rate * float64(time.Second))
	if wait < time.Second {
		wait = time.Second
	}
	secs := int((wait + time.Second - 1) / time.Second) // 向上取整
	if secs < 1 {
		secs = 1
	}
	return true, secs
}

// fail 记录一次鉴权失败（消耗 1 个令牌；耗尽后保持 0，由 blocked 拒绝）。
func (l *authFailLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.getOrCreate(ip, now)
	l.refill(b, now)
	if b.tokens >= 1 {
		b.tokens--
	} else {
		b.tokens = 0
	}
}

// success 鉴权成功：清空该 IP 的失败记录（下次失败从满桶重新计数）。
func (l *authFailLimiter) success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[ip]
	if b == nil {
		return
	}
	if b.elem != nil {
		l.lru.Remove(b.elem)
	}
	delete(l.buckets, ip)
}

// isPanelAPIPath 是否面板数据接口路径（限速只作用于这些路径；页面/静态资源可匿名加载）。
func isPanelAPIPath(path string) bool {
	return strings.HasPrefix(path, "/panel/api/")
}

// rateLimitIP 提取限速用的来源 IP：X-Forwarded-For 首段 → X-Real-IP → TCP 对端。
// 口径与 server/logging.go 的 clientIPForLog 一致（反代后拿真实客户端；直连回落
// RemoteAddr，避免所有直连请求共用一个空 key 被一起限速）。
func rateLimitIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if ip := upstream.ExtractClientIP(r); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}
