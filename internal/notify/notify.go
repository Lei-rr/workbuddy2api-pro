// Package notify 事件通知渠道（v2.3.0 #19：Webhook 最小闭环）。
//
// 定位：把网关里"需要人知道"的事件异步推送到一个 HTTP(S) Webhook（群机器人 /
// 自建告警网关都可以），不引入任何第三方依赖（纯标准库）。
//
// 设计约束：
//   - 不阻塞调用方：Emit 只做「序列化 + 入有界队列」然后立即返回，真正的 HTTP
//     发送在单个 worker goroutine 里完成；队列满（默认 64）直接丢弃并计数/告警，
//     绝不反向拖慢签到/任务等主流程；
//   - 失败不致命：单事件最多尝试 2 次（首发 + 短退避重试 1 次），仍失败只记 WARN
//     与失败计数，不 panic、不重排；
//   - 关闭可排空：Close(ctx) 关闭队列后 worker 会把缓冲里的事件发完才退出，超时
//     （ctx 取消）则放弃等待由进程退出兜底；
//   - 安全：日志只打印脱敏后的目标地址（scheme://host + path 前 12 字符），永不
//     打印完整 URL/query（Webhook URL 常自带 token）；payload 固定为
//     {"event","ts","data"} 三个字段，绝不回传任何 token/凭证（由调用方保证 data
//     里不放凭证，本包不读取也不追加任何请求头到 payload）。
//
// 典型接线（main.go）：
//
//	notifier := notify.New(cfg.Notify.WebhookURL, cfg.NotifyTimeoutDur, cfg.Notify.Events)
//	defer notifier.Close(context.Background()) // 优雅停机时带超时排空
//
// 事件钩子（由 main / panel / scheduler 在对应位置调用）：
//   - notifier.JobDone(map[string]any{...})        → job_done
//   - notifier.AccountDisabled(uid8, reason)       → account_disabled
//   - notifier.CheckinFailed(uid8, errMsg)         → checkin_failed
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 事件名常量（与 json 里的 event 字段一致；也是 notify.events 订阅的键）。
const (
	EventJobDone         = "job_done"         // 记录/任务批次结束（含统计）
	EventAccountDisabled = "account_disabled" // 账号被上游判禁用（连续 session dead 等）
	EventCheckinFailed   = "checkin_failed"   // 单号/批量签到失败
)

// defaultEvents 订阅为空时的默认事件集合。
var defaultEvents = []string{EventJobDone, EventAccountDisabled, EventCheckinFailed}

const (
	// queueSize 异步发送队列容量：签到大批次连续失败时最多积压 64 条，超出入队即丢。
	queueSize = 64
	// maxDataBytes payload 里 data 字段序列化后的上限（64KB）；超出降级为摘要。
	maxDataBytes = 64 * 1024
	// maxAttempts 单事件总尝试次数（首发 + 重试 1 次）。
	maxAttempts = 2
	// retryBackoff 重试前的短退避（规格要求 ≤500ms）。
	retryBackoff = 300 * time.Millisecond
	// defaultTimeout webhook 单请求超时（配置缺省 10s；New 收到 <=0 时回落）。
	defaultTimeout = 10 * time.Second
	// userAgent 出站 User-Agent，便于接收端识别来源。
	userAgent = "workbuddy2api-pro/notify"
)

// Config 通知配置（与 cmd/server 的 notify 段同构；New 直接接收三个字段，
// 保留本类型供接线/文档/测试使用）。
type Config struct {
	WebhookURL string        // 空 = 关闭通知
	Timeout    time.Duration // 单次 HTTP 超时；<=0 回落 10s
	Events     []string      // 订阅事件；空 = 默认三件套
}

// wireEvent 已序列化、待发送的一条事件（入队后 data map 不再被引用，无数据竞争）。
type wireEvent struct {
	name string
	body []byte
}

// payload 出站 JSON 结构（固定三字段，绝不夹带其它键）。
type payload struct {
	Event string         `json:"event"`
	TS    int64          `json:"ts"` // unix 秒（事件产生时刻，不是发送时刻）
	Data  map[string]any `json:"data"`
}

// Stats 通知器运行期计数（供日志/观测/测试；全 atomic，读无锁）。
type Stats struct {
	Sent    int64 // 成功发送的事件数
	Failed  int64 // 重试后仍失败的事件数
	Dropped int64 // 队列满/ctx 取消/序列化失败而丢弃的事件数
}

// Notifier 并发安全的事件通知器。零值或空配置 = 关闭（Enabled=false，所有方法 no-op）。
type Notifier struct {
	cfg        Config
	enabled    bool
	subscribed map[string]struct{}
	client     *http.Client

	queue      chan wireEvent
	workerDone chan struct{} // worker 退出时关闭（Close 等待它）

	mu     sync.RWMutex // 保护 closed 与 queue 的关闭/入队互斥
	closed bool

	sent    atomic.Int64
	failed  atomic.Int64
	dropped atomic.Int64
}

// New 构建通知器。webhookURL 为空（或非法/非 http(s)）时返回"关闭"实例：
// Enabled()=false，Emit/Close 等全部 no-op（调用方无需各处分叉判断）。
//
// timeout<=0 回落 10s（配置侧已 clamp 1..60s，这里只做最后兜底）；events 为空
// 使用默认集合 job_done/account_disabled/checkin_failed。
func New(webhookURL string, timeout time.Duration, events []string) *Notifier {
	webhookURL = strings.TrimSpace(webhookURL)
	if webhookURL == "" {
		return &Notifier{}
	}
	u, err := url.Parse(webhookURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		log.Printf("WARN: notify: webhook_url 非法或非 http(s)，通知已关闭 target=%s", maskWebhookURL(webhookURL))
		return &Notifier{}
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	subscribed := make(map[string]struct{}, len(events))
	for _, e := range events {
		if e = strings.TrimSpace(e); e != "" {
			subscribed[e] = struct{}{}
		}
	}
	if len(subscribed) == 0 {
		for _, e := range defaultEvents {
			subscribed[e] = struct{}{}
		}
	}

	n := &Notifier{
		cfg: Config{
			WebhookURL: webhookURL,
			Timeout:    timeout,
			Events:     sortedKeys(subscribed),
		},
		enabled:    true,
		subscribed: subscribed,
		client:     &http.Client{Timeout: timeout},
		queue:      make(chan wireEvent, queueSize),
		workerDone: make(chan struct{}),
	}
	go n.worker()
	log.Printf("notify: webhook 通知已启用 target=%s timeout=%s events=%v",
		maskWebhookURL(webhookURL), timeout, n.cfg.Events)
	return n
}

// NewFromConfig 便于 main 一行接线（Config 与 config.json 的 notify 段同构）。
func NewFromConfig(cfg Config) *Notifier {
	return New(cfg.WebhookURL, cfg.Timeout, cfg.Events)
}

// Enabled 通知是否启用（webhook_url 非空且合法）。
func (n *Notifier) Enabled() bool { return n != nil && n.enabled }

// Event 判断某事件名是否被订阅（关闭状态恒 false；订阅空=默认集合）。
func (n *Notifier) Event(name string) bool {
	if n == nil || !n.enabled {
		return false
	}
	_, ok := n.subscribed[name]
	return ok
}

// Stats 返回累计计数（原子读，可在任意 goroutine 调用）。
func (n *Notifier) Stats() Stats {
	if n == nil {
		return Stats{}
	}
	return Stats{Sent: n.sent.Load(), Failed: n.failed.Load(), Dropped: n.dropped.Load()}
}

// Emit 异步发送一条事件；未启用或事件未订阅时直接返回。调用方永不阻塞：
// 序列化在调用方 goroutine 完成（保证 data 快照不被后续修改），HTTP 发送在 worker。
//
// ctx 只表达"调用方生命周期"：已取消则不入队（计入 dropped）。真正发送使用
// 独立的一次性超时（webhook 超时配置），不受调用方 ctx 取消影响——否则一次
// 请求取消就会让告警静默丢失。
func (n *Notifier) Emit(ctx context.Context, event string, data map[string]any) {
	if n == nil || !n.enabled || !n.Event(event) {
		return
	}
	if ctx != nil && ctx.Err() != nil {
		n.drop(event, "context canceled")
		return
	}
	body, err := buildPayload(event, time.Now().Unix(), data)
	if err != nil {
		n.drop(event, "序列化失败: "+err.Error())
		return
	}
	n.enqueue(wireEvent{name: event, body: body})
}

// JobDone 任务批次结束（event job_done）。job 建议携带：id/kind/trigger/total/done/
// fail/skip/finished_at 等统计（由 #9 任务历史的数据源提供，不含凭证）。
func (n *Notifier) JobDone(job map[string]any) {
	n.Emit(context.Background(), EventJobDone, job)
}

// AccountDisabled 账号被禁用（event account_disabled）。uid 建议传脱敏短 uid（前 8 位）。
func (n *Notifier) AccountDisabled(uid, reason string) {
	n.Emit(context.Background(), EventAccountDisabled, map[string]any{"uid": uid, "reason": reason})
}

// CheckinFailed 签到失败（event checkin_failed）。errMsg 为上游错误摘要。
func (n *Notifier) CheckinFailed(uid, errMsg string) {
	n.Emit(context.Background(), EventCheckinFailed, map[string]any{"uid": uid, "error": errMsg})
}

// Close 关闭通知器：停止接收新事件，把队列里已缓冲的事件发完（尽力而为），
// 超时（ctx 取消）返回错误并放弃等待（worker 仍在后台收尾，进程退出兜底）。
// 可重复调用（幂等）；未启用时恒 nil。
func (n *Notifier) Close(ctx context.Context) error {
	if n == nil || !n.enabled {
		return nil
	}
	n.mu.Lock()
	if !n.closed {
		n.closed = true
		close(n.queue) // worker range 会在排空后退出
	}
	n.mu.Unlock()

	waitCtx := ctx
	if waitCtx == nil {
		waitCtx = context.Background()
	}
	select {
	case <-n.workerDone:
		return nil
	case <-waitCtx.Done():
		return fmt.Errorf("notify: close: %w", waitCtx.Err())
	}
}

// enqueue 非阻塞入队；队列满/已关闭只计数并按节流打 WARN（绝不阻塞调用方）。
func (n *Notifier) enqueue(ev wireEvent) {
	n.mu.RLock()
	if n.closed {
		n.mu.RUnlock()
		n.drop(ev.name, "通知器已关闭")
		return
	}
	select {
	case n.queue <- ev:
		n.mu.RUnlock()
	default:
		n.mu.RUnlock()
		n.drop(ev.name, "发送队列已满")
	}
}

// drop 丢弃计数 + 节流 WARN（第 1 次与每 100 次打一条，避免 webhook 故障时刷屏）。
func (n *Notifier) drop(event, reason string) {
	d := n.dropped.Add(1)
	if d == 1 || d%100 == 0 {
		log.Printf("WARN: notify: 丢弃事件 %q（%s；累计丢弃 %d）", event, reason, d)
	}
}

// worker 单 goroutine 顺序消费队列（顺序发送，避免对接收端形成并发风暴）。
func (n *Notifier) worker() {
	defer close(n.workerDone)
	for ev := range n.queue {
		n.send(ev)
	}
}

// send 发送一条事件：最多 maxAttempts 次，失败间隔短退避；最终失败只记 WARN。
func (n *Notifier) send(ev wireEvent) {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := n.post(ev.body); err != nil {
			lastErr = err
			if attempt < maxAttempts {
				time.Sleep(retryBackoff)
			}
			continue
		}
		n.sent.Add(1)
		return
	}
	n.failed.Add(1)
	log.Printf("WARN: notify: 事件 %q 发送失败（已尝试 %d 次）: %v", ev.name, maxAttempts, lastErr)
}

// post 单次 HTTP POST；2xx 视为成功，其它状态码/网络错误返回 error。
func (n *Notifier) post(body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), n.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := n.client.Do(req)
	if err != nil {
		// url.Error 的 Error() 会带上完整 URL（含 path/query 里的 token），日志里
		// 只保留底层错误原因——脱敏约束同样适用于失败路径。
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return fmt.Errorf("webhook 请求失败: %w", err)
	}
	defer resp.Body.Close()
	// 丢弃响应体（限读 4KB 防异常大响应占内存），保证连接可复用。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook 返回 HTTP %d", resp.StatusCode)
	}
	return nil
}

// buildPayload 序列化 payload；data 序列化后超过 maxDataBytes 时降级为摘要
// （保留 truncated/original_bytes/keys），既保证告警可达又不阻塞主流程。
func buildPayload(event string, ts int64, data map[string]any) ([]byte, error) {
	if data == nil {
		data = map[string]any{}
	}
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if len(dataJSON) > maxDataBytes {
		keys := make([]string, 0, len(data))
		for k := range data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		original := len(dataJSON)
		data = map[string]any{
			"truncated":      true,
			"original_bytes": original,
			"keys":           keys,
		}
	}
	return json.Marshal(payload{Event: event, TS: ts, Data: data})
}

// maskWebhookURL 日志脱敏：只保留 scheme://host + path 前 12 个字符（按 rune 截断），
// 丢弃 query/fragment/userinfo——Webhook URL 常把 token 放在 path/query 里。
func maskWebhookURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "invalid-url"
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if r := []rune(path); len(r) > 12 {
		path = string(r[:12]) + "…"
	}
	return u.Scheme + "://" + u.Host + path
}

// sortedKeys 把订阅集合转成稳定排序的事件名切片（日志/配置回显可复现）。
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
