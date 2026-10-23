// Package notify 事件通知渠道（v2.3.0 #19 Webhook 最小闭环；v2.3.1 扩展多渠道）。
//
// 定位：把网关里"需要人知道"的事件异步推送到一个 HTTP(S) 目标，不引入任何第三方
// 依赖（纯标准库）。渠道（notify.channel）：
//   - webhook（默认）：通用 JSON `{"event","ts","data"}`，交给自建告警网关/脚本消费；
//   - wecom：企业微信群机器人 markdown 消息（body errcode==0 才视为成功）；
//   - telegram：Bot sendMessage 纯文本（2xx 且 ok:true 才视为成功；需 telegram_chat_id）；
//   - bark：iOS Bark 推送（2xx 即成功）。
//
// 设计约束（与 v2.3.0 一致）：
//   - 不阻塞调用方：Emit 只做「编码 + 入有界队列」然后立即返回，真正的 HTTP 发送在
//     单个 worker goroutine 里完成；队列满（默认 64）直接丢弃并计数/告警，绝不反向
//     拖慢签到/任务等主流程；
//   - 失败不致命：单事件最多尝试 2 次（首发 + 短退避重试 1 次），仍失败只记 WARN
//     与失败计数，不 panic、不重排；
//   - 关闭可排空：Close(ctx) 关闭队列后 worker 会把缓冲里的事件发完才退出，超时
//     （ctx 取消）则放弃等待由进程退出兜底；
//   - 安全：日志只打印脱敏后的目标地址（webhook 保留 scheme://host + path 前 12
//     字符；wecom/telegram/bark 的 path 常直接带 key/token，只保留 scheme://host），
//     永不打印完整 URL/query；payload 只含事件数据/概要文本，绝不回传 token/凭证
//     （由调用方保证 data 里不放凭证，本包不读取也不追加任何请求头到 payload）。
//
// 典型接线（main.go，v2.3.1）：
//
//	notifier := notify.NewFromConfig(notify.Config{
//	    WebhookURL: cfg.Notify.WebhookURL, Timeout: cfg.NotifyTimeoutDur, Events: cfg.Notify.Events,
//	    Channel: cfg.Notify.Channel, TelegramChatID: cfg.Notify.TelegramChatID, Title: cfg.Notify.Title,
//	})
//	defer notifier.Close(context.Background()) // 优雅停机时带超时排空
//
// 兼容性：`New(webhookURL, timeout, events)` 等价于 channel=webhook 的 NewFromConfig，
// 行为/payload 与 v2.3.0 完全一致（既有 webhook 消费方零影响）。
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
	// maxDataBytes webhook payload 里 data 字段序列化后的上限（64KB）；超出降级为摘要。
	maxDataBytes = 64 * 1024
	// maxAttempts 单事件总尝试次数（首发 + 重试 1 次）。
	maxAttempts = 2
	// retryBackoff 重试前的短退避（规格要求 ≤500ms）。
	retryBackoff = 300 * time.Millisecond
	// defaultTimeout webhook 单请求超时（配置缺省 10s；New 收到 <=0 时回落）。
	defaultTimeout = 10 * time.Second
	// userAgent 出站 User-Agent，便于接收端识别来源。
	userAgent = "workbuddy2api-pro/notify"
	// maxRespRead 读取响应体的上限（用于渠道业务级成功判定；防异常大响应占内存）。
	maxRespRead = 8 << 10
)

// Config 通知配置（与 cmd/server 的 notify 段同构；NewFromConfig 按其选择渠道，
// New 等价于 Channel=webhook 的简写）。
type Config struct {
	WebhookURL string        // 目标地址；空 = 关闭通知
	Timeout    time.Duration // 单次 HTTP 超时；<=0 回落 10s
	Events     []string      // 订阅事件；空 = 默认三件套

	// Channel 渠道类型："webhook"（默认/空）/ "wecom" / "telegram" / "bark"。
	Channel string
	// TelegramChatID telegram 渠道目标 chat_id（@channel 或数字 id）；telegram 必填。
	TelegramChatID string
	// Title 渠道消息标题（wecom/telegram/bark 展示；空 = 默认标题）。
	Title string
}

// wireEvent 已编码、待发送的一条事件（入队后 data map 不再被引用，无数据竞争）。
type wireEvent struct {
	name string
	body []byte
}

// payload webhook 渠道出站 JSON 结构（固定三字段，绝不夹带其它键）。
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
	adapter    channelAdapter // 渠道适配器（编码 + 成功判定）；enabled=false 时为 nil
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

// New 构建 webhook 渠道通知器（v2.3.0 兼容入口）。
// webhookURL 为空（或非法/非 http(s)）时返回"关闭"实例：Enabled()=false，所有方法
// no-op。timeout<=0 回落 10s；events 为空使用默认集合。
func New(webhookURL string, timeout time.Duration, events []string) *Notifier {
	return NewFromConfig(Config{
		WebhookURL: webhookURL,
		Timeout:    timeout,
		Events:     events,
		Channel:    ChannelWebhook,
	})
}

// NewFromConfig 按配置构建通知器（v2.3.1 多渠道路径）。
//
// 防御性校验（config.normalize 已 fail fast，这里兜底"绕过 Load 的裸用/测试"）：
//   - 未知 channel、telegram 缺 chat_id、目标 URL 为空/非法 → 返回关闭实例并 WARN；
//   - channel 为空 → webhook；Title 为空 → "WorkBuddy2API Pro"。
func NewFromConfig(cfg Config) *Notifier {
	channel := strings.ToLower(strings.TrimSpace(cfg.Channel))
	if channel == "" {
		channel = ChannelWebhook
	}
	cfg.Channel = channel
	cfg.Title = strings.TrimSpace(cfg.Title)
	if cfg.Title == "" {
		cfg.Title = defaultTitle
	}
	cfg.Events = normalizeEventNames(cfg.Events)

	rawURL := strings.TrimSpace(cfg.WebhookURL)
	if rawURL == "" {
		// webhook 渠道为空是"正常关闭"（默认部署形态）；其它渠道为空是配置遗漏，给 WARN。
		if channel != ChannelWebhook {
			log.Printf("WARN: notify: 渠道 %s 未配置 notify.webhook_url，通知已关闭", channel)
		}
		return &Notifier{}
	}
	if _, err := parseHTTPURL(rawURL); err != nil {
		log.Printf("WARN: notify: notify.webhook_url 非法或非 http(s)（渠道 %s），通知已关闭 target=%s",
			channel, maskURL(channel, rawURL))
		return &Notifier{}
	}

	var ad channelAdapter
	switch channel {
	case ChannelWebhook:
		ad = webhookAdapter{u: rawURL}
	case ChannelWecom:
		ad = wecomAdapter{u: rawURL, title: cfg.Title}
	case ChannelTelegram:
		chatID := strings.TrimSpace(cfg.TelegramChatID)
		if chatID == "" {
			log.Printf("WARN: notify: 渠道 telegram 缺少 notify.telegram_chat_id，通知已关闭 target=%s",
				maskURL(channel, rawURL))
			return &Notifier{}
		}
		cfg.TelegramChatID = chatID
		ad = telegramAdapter{u: rawURL, chatID: chatID, title: cfg.Title}
	case ChannelBark:
		ad = barkAdapter{u: rawURL, title: cfg.Title}
	default:
		log.Printf("WARN: notify: 未知 notify.channel=%q（可选 webhook/wecom/telegram/bark），通知已关闭", channel)
		return &Notifier{}
	}
	return newNotifier(cfg, ad)
}

// newNotifier 渠道无关的通用构造：事件订阅归一化、队列/worker 启动、启动日志。
func newNotifier(cfg Config, ad channelAdapter) *Notifier {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	subscribed := make(map[string]struct{}, len(cfg.Events))
	for _, e := range cfg.Events {
		if e = strings.TrimSpace(e); e != "" {
			subscribed[e] = struct{}{}
		}
	}
	if len(subscribed) == 0 {
		for _, e := range defaultEvents {
			subscribed[e] = struct{}{}
		}
	}
	cfg.Events = sortedKeys(subscribed)

	n := &Notifier{
		cfg:        cfg,
		enabled:    true,
		adapter:    ad,
		subscribed: subscribed,
		client:     &http.Client{Timeout: cfg.Timeout},
		queue:      make(chan wireEvent, queueSize),
		workerDone: make(chan struct{}),
	}
	go n.worker()
	log.Printf("notify: 通知已启用 channel=%s target=%s timeout=%s title=%q events=%v",
		ad.channel(), ad.target(), cfg.Timeout, cfg.Title, cfg.Events)
	return n
}

// Enabled 通知是否启用（目标地址非空且合法、渠道参数齐全）。
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
// 编码在调用方 goroutine 完成（保证 data 快照不被后续修改），HTTP 发送在 worker。
//
// ctx 只表达"调用方生命周期"：已取消则不入队（计入 dropped）。真正发送使用
// 独立的一次性超时（notify.timeout），不受调用方 ctx 取消影响——否则一次
// 请求取消就会让告警静默丢失。
func (n *Notifier) Emit(ctx context.Context, event string, data map[string]any) {
	if n == nil || !n.enabled || !n.Event(event) {
		return
	}
	if ctx != nil && ctx.Err() != nil {
		n.drop(event, "context canceled")
		return
	}
	body, err := n.adapter.buildBody(event, time.Now().Unix(), data)
	if err != nil {
		n.drop(event, "编码失败: "+err.Error())
		return
	}
	n.enqueue(wireEvent{name: event, body: body})
}

// JobDone 任务批次结束（event job_done）。job 建议携带：id/kind/trigger/total/done/
// fail/skip 等统计（由 #9 任务历史的数据源提供，不含凭证）。
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
	log.Printf("WARN: notify: 事件 %q 发送失败（已尝试 %d 次，渠道 %s）: %v",
		ev.name, maxAttempts, n.adapter.channel(), lastErr)
}

// post 单次 HTTP POST：2xx 只代表传输成功，渠道业务级成功由 adapter.checkResponse
// 二次判定（wecom errcode / telegram ok）。响应体只读取前 8KB 供解析。
func (n *Notifier) post(body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), n.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.adapter.url(), bytes.NewReader(body))
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
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespRead))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("接口返回 HTTP %d", resp.StatusCode)
	}
	if err := n.adapter.checkResponse(respBody); err != nil {
		return err
	}
	return nil
}

// buildPayload 序列化 webhook payload；data 序列化后超过 maxDataBytes 时降级为摘要
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

// maskWebhookURL 日志脱敏（webhook 渠道，保持 v2.3.0 语义）：只保留
// scheme://host + path 前 12 个字符（按 rune 截断），丢弃 query/fragment/userinfo。
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

// normalizeEventNames 事件订阅归一化：去空白/空项、去重、小写（与 config.normalize
// 同口径：notify.events 里大小写混写不会造成"订阅了却收不到"）。
func normalizeEventNames(events []string) []string {
	out := make([]string, 0, len(events))
	seen := make(map[string]struct{}, len(events))
	for _, e := range events {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out
}
