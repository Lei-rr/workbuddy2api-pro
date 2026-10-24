// sse.go 面板 SSE 事件推送（v2.4.0 #3）。
//
// 目标：把「5s 全量轮询」降级为「推送 + 降级轮询」——Job 进度、账号池计数/积分
// 变化秒级可见；SSE 不可用（代理缓冲/浏览器不支持/开关关闭）时前端自动回落既有
// 轮询，不会出现空白面板。
//
// 设计要点：
//   - 包级注册表 sync.Map keyed *Panel → *hub，首次连接懒创建；一个 Panel 一个 hub，
//     hub 后台 goroutine 每 2s 构建一次轻量快照并广播（无客户端时自动停止并摘除，
//     不泄漏 goroutine）；
//   - 每客户端独立写循环 + buffered chan（容量 16）：慢客户端（不读/网络卡）由 hub
//     非阻塞检测到队列满 → 直接关闭该连接，绝不阻塞其它客户端与 hub 自身；
//   - 首帧立即推送 snapshot（客户端重连即拿到全量，v1 不做 Last-Event-ID 回放）；
//   - 事件契约见本文件 sseSnapshot / sseJob / sseAccountsEvent 的 JSON 字段；
//   - panel.events_enabled=false → 404（前端据此回落轮询）。配置经 SetEventsEnabled
//     注入（见文件末尾「main 装配」注释）。
package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/jobs"
)

// SSE 运行参数（var 而非 const：测试可注入短间隔/小上限）。
var (
	// sseSnapshotInterval hub 全量快照广播间隔（默认 2s）。快照本身很轻（计数 +
	// 积分合计 + 最近 10 条 Job），2s 足以让 Job 进度条顺滑，又不会造成事件风暴。
	sseSnapshotInterval = 2 * time.Second
	// sseHeartbeatInterval 心跳注释行间隔（默认 15s）：防反代/LB 空闲超时断连。
	sseHeartbeatInterval = 15 * time.Second
	// sseClientBuffer 每客户端待发送队列容量（默认 16）：满即判定为慢客户端并断开。
	sseClientBuffer = 16
	// sseMaxClients 单 Panel hub 最大客户端数（默认 64）。
	sseMaxClients = 64
)

// sseEventsOn 全局面板 SSE 开关（默认 true；main 按 panel.events_enabled 注入）。
// 用包级原子量而非 panel.Config 字段：panel.Config 定义在 panel.go（本 Wave 由
// backend-c 并行修改 RBAC，本任务约定不碰 panel.go）；见文件末尾装配注释。
var sseEventsOn atomic.Bool

func init() { sseEventsOn.Store(true) }

// SetEventsEnabled 设置 SSE 事件推送开关（main 启动时按 panel.events_enabled 注入；
// 关闭后 GET /panel/api/events 返回 404，前端回落轮询）。
func SetEventsEnabled(enabled bool) { sseEventsOn.Store(enabled) }

// EventsEnabled 当前 SSE 开关（供测试/观测）。
func EventsEnabled() bool { return sseEventsOn.Load() }

// sseHubs 包级 hub 注册表：*Panel → *sseHub，首个 SSE 连接时懒创建。
var sseHubs sync.Map

var (
	errHubClosed = errors.New("sse: hub closed")
	errHubFull   = errors.New("sse: too many clients")
)

// sseMessage 一条待发送事件（data 为已序列化 JSON，广播时零分配复用）。
type sseMessage struct {
	event string
	data  []byte
}

// sseClient 单个 SSE 客户端：hub 只往 ch 非阻塞投递；done 关闭后写循环退出。
type sseClient struct {
	ch   chan sseMessage
	done chan struct{}
	once sync.Once
}

func (c *sseClient) close() {
	if c == nil {
		return
	}
	c.once.Do(func() { close(c.done) })
}

// sseHub 单个 Panel 的事件 hub。
type sseHub struct {
	p    *Panel
	stop chan struct{}
	done chan struct{}

	mu       sync.Mutex
	clients  map[*sseClient]struct{}
	stopping bool

	// 变化检测指纹（hub goroutine 独享，无需加锁）：jobs 指纹变化 → 推 jobs 事件；
	// 池计数/积分指纹变化 → 推 accounts 事件。首次 tick 只建基线（首帧 snapshot
	// 已由 handler 立即下发，不重复推增量）。
	primed     bool
	lastJobsFP uint64
	lastAcctFP uint64
}

// newSSEHub 创建 hub（不启动 goroutine；由 getOrCreateSSEHub 在注册成功前启动）。
func newSSEHub(p *Panel) *sseHub {
	return &sseHub{
		p:       p,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		clients: make(map[*sseClient]struct{}),
	}
}

// getOrCreateSSEHub 懒创建/复用 Panel 的 hub 并注册一个客户端。
func getOrCreateSSEHub(p *Panel) (*sseHub, *sseClient, error) {
	for {
		if v, ok := sseHubs.Load(p); ok {
			h := v.(*sseHub)
			if c, err := h.register(); err == nil {
				return h, c, nil
			} else if errors.Is(err, errHubFull) {
				return nil, nil, err
			}
			// errHubClosed：hub 正在停止（最后一个客户端刚断开），重试创建。
			continue
		}
		h := newSSEHub(p)
		if _, loaded := sseHubs.LoadOrStore(p, h); loaded {
			continue // 并发创建：用已存在的那个
		}
		go h.run()
		c, err := h.register()
		if err == nil {
			return h, c, nil
		}
		if errors.Is(err, errHubFull) {
			return nil, nil, err
		}
	}
}

// register 注册客户端；hub 正在停止返回 errHubClosed，超过上限返回 errHubFull。
func (h *sseHub) register() (*sseClient, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopping {
		return nil, errHubClosed
	}
	if len(h.clients) >= sseMaxClients {
		return nil, errHubFull
	}
	c := &sseClient{
		ch:   make(chan sseMessage, sseClientBuffer),
		done: make(chan struct{}),
	}
	h.clients[c] = struct{}{}
	return c, nil
}

// unregister 移除客户端；最后一个客户端离开时停止 hub 并从注册表摘除（幂等）。
func (h *sseHub) unregister(c *sseClient) {
	h.mu.Lock()
	if _, ok := h.clients[c]; !ok {
		h.mu.Unlock()
		c.close()
		return
	}
	delete(h.clients, c)
	last := len(h.clients) == 0
	if last {
		h.stopping = true
		close(h.stop)
	}
	h.mu.Unlock()
	c.close()
	if last {
		sseHubs.CompareAndDelete(h.p, h)
	}
}

// run hub 主循环：每 sseSnapshotInterval 构建快照并广播；无客户端后退出。
func (h *sseHub) run() {
	defer close(h.done)
	t := time.NewTicker(sseSnapshotInterval)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			h.tick()
		}
	}
}

// tick 构建一次快照 + 变化事件并广播。任何构建失败都不影响 hub（快照字段兜底）。
func (h *sseHub) tick() {
	snapshot, jobsEvent, acctEvent := h.build()
	h.broadcast(sseMessage{event: "snapshot", data: snapshot})
	if jobsEvent != nil {
		h.broadcast(*jobsEvent)
	}
	if acctEvent != nil {
		h.broadcast(*acctEvent)
	}
}

// build 构建 snapshot 与（相对上一 tick 的）jobs/accounts 增量事件。
func (h *sseHub) build() (snap []byte, jobsEvent, acctEvent *sseMessage) {
	s := h.p.buildSSESnapshot()
	raw, err := json.Marshal(s)
	if err != nil {
		raw = []byte(`{"error":"snapshot marshal failed"}`)
	}

	jobsRaw, _ := json.Marshal(s.Jobs)
	jobsFP := fnv64(jobsRaw)
	acctFP := fnv64([]byte(fmt.Sprintf("%d/%d/%d/%d/%d/%d/%d",
		s.Counts.Healthy, s.Counts.Cooling, s.Counts.Paused,
		s.Counts.Disabled, s.Counts.Total, s.CreditsSum, s.CreditsTotalSum)))

	if !h.primed {
		h.primed = true
		h.lastJobsFP, h.lastAcctFP = jobsFP, acctFP
		return raw, nil, nil
	}
	if jobsFP != h.lastJobsFP {
		h.lastJobsFP = jobsFP
		evRaw, _ := json.Marshal(sseJobsEvent{Jobs: s.Jobs})
		jobsEvent = &sseMessage{event: "jobs", data: evRaw}
	}
	if acctFP != h.lastAcctFP {
		h.lastAcctFP = acctFP
		evRaw, _ := json.Marshal(sseAccountsEvent{
			Counts:          s.Counts,
			CreditsSum:      s.CreditsSum,
			CreditsTotalSum: s.CreditsTotalSum,
		})
		acctEvent = &sseMessage{event: "accounts", data: evRaw}
	}
	return raw, jobsEvent, acctEvent
}

// broadcast 非阻塞广播：慢客户端（待发送队列已满）立即断开并摘除，不阻塞 hub。
func (h *sseHub) broadcast(msg sseMessage) {
	h.mu.Lock()
	clients := make([]*sseClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()
	for _, c := range clients {
		select {
		case c.ch <- msg:
		default:
			// 队列满 = 客户端读得太慢：丢弃剩余事件并关闭连接（前端会重连拿新快照）。
			h.unregister(c)
		}
	}
}

// snapshotBytes 构建当前快照 JSON（handler 首帧直接下发用）。
func (h *sseHub) snapshotBytes() []byte {
	raw, err := json.Marshal(h.p.buildSSESnapshot())
	if err != nil {
		return []byte(`{"error":"snapshot marshal failed"}`)
	}
	return raw
}

// fnv64 计算指纹（变化检测用；快、无分配压力）。
func fnv64(b []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(b)
	return h.Sum64()
}

// ---------------------------------------------------------------------------
// 事件 payload 契约（JSON 字段即前端契约；只增不改）
// ---------------------------------------------------------------------------

// sseCounts 账号池计数（与 /panel/api/overview 同口径，paused 独立于 disabled）。
type sseCounts struct {
	Healthy  int `json:"healthy"`
	Cooling  int `json:"cooling"`
	Paused   int `json:"paused"`
	Disabled int `json:"disabled"`
	Total    int `json:"total"`
}

// sseJob Job 精简视图（去掉 log_tail，控制事件体积；字段与 /panel/api/jobs 同名）。
type sseJob struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Trigger    string     `json:"trigger"`
	State      string     `json:"state"`
	Total      int        `json:"total"`
	Done       int        `json:"done"`
	Fail       int        `json:"fail"`
	Skip       int        `json:"skip"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// sseSnapshot event: snapshot 的 data（首次连接与每 2s 全量；客户端以它自愈）。
type sseSnapshot struct {
	// Now 服务端当前时间（RFC3339；前端可用来校正本地时钟/展示“更新于”）。
	Now string `json:"now"`
	// NowUnix 同 Now 的 unix 秒（兼容只做数值比较的前端，额外便利字段）。
	NowUnix         int64     `json:"now_unix"`
	Version         string    `json:"version"`
	PollIntervalSec int       `json:"poll_interval_sec"`
	Counts          sseCounts `json:"counts"`
	CreditsSum      int64     `json:"credits_sum"`
	CreditsTotalSum int64     `json:"credits_total_sum"`
	Jobs            []sseJob  `json:"jobs"` // 最近 10 条（无 Registry 时恒为 []）
	EventsEnabled   bool      `json:"events_enabled"`
}

// sseJobsEvent event: jobs 的 data（Job 列表指纹变化时推送，含最近 10 条精简）。
type sseJobsEvent struct {
	Jobs []sseJob `json:"jobs"`
}

// sseAccountsEvent event: accounts 的 data（池计数/积分合计变化时推送）。
type sseAccountsEvent struct {
	Counts          sseCounts `json:"counts"`
	CreditsSum      int64     `json:"credits_sum"`
	CreditsTotalSum int64     `json:"credits_total_sum"`
}

// buildSSESnapshot 构建轻量快照：用 pool.Summary()（O(1) 缓存）一次取齐计数/积分，
// 不再做全池扫描（v2.5.0 Perf-3：10k 账号快照 <1ms）。
func (p *Panel) buildSSESnapshot() sseSnapshot {
	now := time.Now()
	s := sseSnapshot{
		Now:             now.Format(time.RFC3339Nano),
		NowUnix:         now.Unix(),
		Version:         p.cfg.Version,
		PollIntervalSec: p.pollIntervalSec(),
		EventsEnabled:   EventsEnabled(),
		Jobs:            []sseJob{}, // 空数组（不是 null），前端无需判空
	}
	if p.cfg.Pool != nil {
		sum := p.poolSummary()
		s.Counts = sseCounts{Healthy: sum.Healthy, Cooling: sum.Cooling, Paused: sum.Paused, Disabled: sum.Disabled, Total: sum.Total}
		s.CreditsSum, s.CreditsTotalSum = sum.CreditsSum, sum.CreditsTotalSum
	}
	if p.cfg.Jobs != nil {
		list := p.cfg.Jobs.List(10)
		if len(list) > 0 {
			s.Jobs = make([]sseJob, 0, len(list))
			for _, j := range list {
				s.Jobs = append(s.Jobs, sseJobFromJob(j))
			}
		}
	}
	return s
}

// sseJobFromJob jobs.Job → 精简 SSE 视图（时间保持 RFC3339，与 Job API 一致）。
func sseJobFromJob(j *jobs.Job) sseJob {
	if j == nil {
		return sseJob{}
	}
	out := sseJob{
		ID:        j.ID,
		Kind:      string(j.Kind),
		Trigger:   string(j.Trigger),
		State:     string(j.State),
		Total:     j.Total,
		Done:      j.Done,
		Fail:      j.Fail,
		Skip:      j.Skip,
		StartedAt: j.StartedAt,
	}
	if !j.FinishedAt.IsZero() {
		t := j.FinishedAt
		out.FinishedAt = &t
	}
	return out
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

// events GET /panel/api/events（路由已 withAuth）：SSE 事件流。
//
// v1 语义：断线重连即重新发 snapshot（不做 Last-Event-ID 回放——快照本身是幂等
// 全量，回放增量的复杂度不值得）；客户端不支持/代理缓冲导致不可用时前端回落轮询。
func (p *Panel) events(w http.ResponseWriter, r *http.Request) {
	if !EventsEnabled() {
		writeErr(w, http.StatusNotFound, "events disabled")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	h, client, err := getOrCreateSSEHub(p)
	if err != nil {
		if errors.Is(err, errHubFull) {
			writeErr(w, http.StatusServiceUnavailable, "too many SSE clients")
		} else {
			writeErr(w, http.StatusServiceUnavailable, "events unavailable")
		}
		return
	}
	defer h.unregister(client)

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream; charset=utf-8")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// 断线重连间隔（浏览器 EventSource 语义：3s，配合服务端 15s 心跳）。
	if _, err := io.WriteString(w, "retry: 3000\n\n"); err != nil {
		return
	}
	if !writeSSE(w, flusher, sseMessage{event: "snapshot", data: h.snapshotBytes()}) {
		return
	}
	hb := time.NewTicker(sseHeartbeatInterval)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-client.done:
			return
		case msg := <-client.ch:
			if !writeSSE(w, flusher, msg) {
				return
			}
		case <-hb.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSE 写一条 SSE 事件并 flush；写失败返回 false（调用方据此退出清理）。
func writeSSE(w http.ResponseWriter, f http.Flusher, msg sseMessage) bool {
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.event, msg.data); err != nil {
		return false
	}
	f.Flush()
	return true
}

// ---------------------------------------------------------------------------
// main 装配说明（本任务只改 sse.go；main.go 由 backend-a 本轮改，Lead 集成时补）：
//
//	panel.SetEventsEnabled(cfg.Panel.EventsEnabled)
//
// 放在 main.go 的 panel.New(...) 附近（handler 运行期读全局原子量）。
// 若后续希望改为 panel.Config.EventsEnabled 字段注入（需改 panel.go 的 Config 定义），
// 保留本文件 SetEventsEnabled 作为兼容层即可。
// ---------------------------------------------------------------------------
