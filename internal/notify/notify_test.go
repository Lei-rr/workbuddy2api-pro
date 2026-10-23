package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// webhookRecorder 记录 httptest webhook 收到的请求，并支持注入失败/延迟。
type webhookRecorder struct {
	mu         sync.Mutex
	bodies     [][]byte
	headers    []http.Header
	failStatus int           // 非 0：所有请求返回该状态码
	failFirst  int           // >0：前 N 次返回 500，其后 200（验证"重试一次"）
	delay      time.Duration // 每个请求处理前的固定延迟
}

func (r *webhookRecorder) record(h http.Header, body []byte) (status int, delay time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, body)
	r.headers = append(r.headers, h.Clone())
	status = r.failStatus
	if r.failFirst > 0 && len(r.bodies) <= r.failFirst {
		status = http.StatusInternalServerError
	}
	return status, r.delay
}

func (r *webhookRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func (r *webhookRecorder) snapshot() ([][]byte, []http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.bodies...), append([]http.Header(nil), r.headers...)
}

func (r *webhookRecorder) lastBody(t *testing.T) decodedPayload {
	t.Helper()
	bodies, _ := r.snapshot()
	if len(bodies) == 0 {
		t.Fatal("webhook 没有收到任何请求")
	}
	return decodePayload(t, bodies[len(bodies)-1])
}

// newWebhook 起一个 httptest webhook 服务（测试结束自动关闭）。
func newWebhook(t *testing.T, rec *webhookRecorder) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		status, delay := rec.record(r.Header, body)
		if delay > 0 {
			time.Sleep(delay)
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// waitFor 轮询等待条件成立（异常路径最多等 timeout 后失败）。
func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时（%s）：%s", timeout, desc)
}

// closeNotifier 测试清理：带超时关闭，避免 goroutine 泄漏。
func closeNotifier(t *testing.T, n *Notifier) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.Close(ctx); err != nil {
		t.Logf("notify close: %v", err)
	}
}

// decodePayload 解析出站 payload（event/ts/data 三字段）。
type decodedPayload struct {
	Event string         `json:"event"`
	TS    int64          `json:"ts"`
	Data  map[string]any `json:"data"`
}

func decodePayload(t *testing.T, raw []byte) decodedPayload {
	t.Helper()
	var p decodedPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("payload 不是合法 JSON: %v\n%s", err, raw)
	}
	return p
}

// TestNewDisabled 空 webhook_url：Enabled=false，所有方法 no-op（不 panic、不发请求）。
func TestNewDisabled(t *testing.T) {
	n := New("", time.Second, nil)
	if n.Enabled() {
		t.Fatal("空 webhook_url 应关闭通知")
	}
	if n.Event(EventJobDone) {
		t.Fatal("关闭状态不应订阅任何事件")
	}
	// 全部便捷方法 + Emit + Close 都必须是安全 no-op。
	n.JobDone(map[string]any{"id": "j1"})
	n.AccountDisabled("uid8", "reason")
	n.CheckinFailed("uid8", "boom")
	n.Emit(context.Background(), EventJobDone, map[string]any{"x": 1})
	if err := n.Close(context.Background()); err != nil {
		t.Fatalf("disabled Close=%v want nil", err)
	}
	if s := n.Stats(); s != (Stats{}) {
		t.Fatalf("disabled stats=%+v want 零值", s)
	}

	// nil 接收者同样安全（调用方不必判空）。
	var nilN *Notifier
	if nilN.Enabled() || nilN.Event(EventJobDone) {
		t.Fatal("nil Notifier 不应启用")
	}
	nilN.JobDone(nil)
	if err := nilN.Close(context.Background()); err != nil {
		t.Fatalf("nil Close=%v", err)
	}
	if s := nilN.Stats(); s != (Stats{}) {
		t.Fatalf("nil stats=%+v", s)
	}
}

// TestNewInvalidURLDisabled 非法/非 http(s) URL 视为关闭（不 panic、不发请求）。
func TestNewInvalidURLDisabled(t *testing.T) {
	for _, raw := range []string{
		"ftp://example.com/hook",
		"not a url",
		"http://",
		"/relative/path",
	} {
		n := New(raw, time.Second, nil)
		if n.Enabled() {
			t.Fatalf("%q 应关闭通知", raw)
		}
		if err := n.Close(context.Background()); err != nil {
			t.Fatalf("%q Close=%v", raw, err)
		}
	}
}

// TestEnabledAndEventSemantics Enabled/Event 语义：默认三件套；显式订阅只认列表；
// 去空白/去重；大小写敏感。
func TestEnabledAndEventSemantics(t *testing.T) {
	srv := newWebhook(t, &webhookRecorder{})

	n := New(srv.URL, time.Second, nil)
	t.Cleanup(func() { closeNotifier(t, n) })
	if !n.Enabled() {
		t.Fatal("合法 URL 应启用")
	}
	for _, ev := range []string{EventJobDone, EventAccountDisabled, EventCheckinFailed} {
		if !n.Event(ev) {
			t.Errorf("默认应订阅 %q", ev)
		}
	}
	if n.Event("unknown_event") {
		t.Fatal("未配置的事件不应订阅")
	}
	if got := strings.Join(n.cfg.Events, ","); got != "account_disabled,checkin_failed,job_done" {
		t.Errorf("默认事件集合=%q（应稳定排序三件套）", got)
	}

	n2 := New(srv.URL, time.Second, []string{" job_done ", "", "job_done", EventCheckinFailed})
	t.Cleanup(func() { closeNotifier(t, n2) })
	if !n2.Event(EventJobDone) || !n2.Event(EventCheckinFailed) {
		t.Fatal("显式订阅应生效")
	}
	if n2.Event(EventAccountDisabled) {
		t.Fatal("显式订阅不应包含未列出的事件")
	}
	if got := strings.Join(n2.cfg.Events, ","); got != "checkin_failed,job_done" {
		t.Errorf("显式事件集合=%q（应去空白/去重/排序）", got)
	}
	if n2.Event("JOB_DONE") {
		t.Fatal("事件名应大小写敏感")
	}
}

// TestEmitDeliversPayload 出站 payload：event/ts/data 三字段、内容正确、请求头正确、
// 不夹带任何凭证字段（调用方约定：data 不放 token；本包固定三字段不追加内容）。
func TestEmitDeliversPayload(t *testing.T) {
	rec := &webhookRecorder{}
	srv := newWebhook(t, rec)
	n := New(srv.URL, 2*time.Second, nil)
	t.Cleanup(func() { closeNotifier(t, n) })

	before := time.Now().Unix()
	n.Emit(context.Background(), EventJobDone, map[string]any{
		"id":    "job-1",
		"total": 12,
		"done":  10,
		"fail":  2,
		"uid8":  "248890d9",
	})
	waitFor(t, 3*time.Second, "webhook 收到 1 条事件", func() bool { return rec.count() == 1 })

	bodies, headers := rec.snapshot()
	p := decodePayload(t, bodies[0])
	if p.Event != EventJobDone {
		t.Errorf("event=%q want %q", p.Event, EventJobDone)
	}
	if p.TS < before || p.TS > time.Now().Unix()+1 {
		t.Errorf("ts=%d 不在合理区间（before=%d now=%d）", p.TS, before, time.Now().Unix())
	}
	if p.Data["id"] != "job-1" || p.Data["uid8"] != "248890d9" {
		t.Errorf("data 字段丢失: %+v", p.Data)
	}
	if got, ok := p.Data["total"].(float64); !ok || got != 12 {
		t.Errorf("data.total=%v want 12", p.Data["total"])
	}

	// 请求头：JSON + 固定 UA。
	if ct := headers[0].Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q", ct)
	}
	if ua := headers[0].Get("User-Agent"); ua != userAgent {
		t.Errorf("User-Agent=%q want %q", ua, userAgent)
	}

	// 顶层只允许 event/ts/data：本包绝不把 token/凭证塞进 payload。
	var top map[string]json.RawMessage
	if err := json.Unmarshal(bodies[0], &top); err != nil {
		t.Fatal(err)
	}
	if len(top) != 3 {
		t.Errorf("payload 顶层字段=%v want 恰好 event/ts/data", keysOf(top))
	}
	for _, k := range []string{"event", "ts", "data"} {
		if _, ok := top[k]; !ok {
			t.Errorf("payload 缺字段 %q", k)
		}
	}
	raw := string(bodies[0])
	for _, banned := range []string{"Authorization", "Bearer", "access_token", "api_key"} {
		if strings.Contains(raw, banned) {
			t.Errorf("payload 不应包含凭证痕迹 %q: %s", banned, raw)
		}
	}
}

// TestConvenienceMethods 三个便捷方法的 event/data 契约。
func TestConvenienceMethods(t *testing.T) {
	rec := &webhookRecorder{}
	srv := newWebhook(t, rec)
	n := New(srv.URL, 2*time.Second, nil)
	t.Cleanup(func() { closeNotifier(t, n) })

	n.AccountDisabled("248890d9", "session_dead x3")
	n.CheckinFailed("248890d9", "HTTP 500")
	waitFor(t, 3*time.Second, "收到 2 条事件", func() bool { return rec.count() == 2 })

	bodies, _ := rec.snapshot()
	got := make(map[string]decodedPayload, 2)
	for _, b := range bodies {
		p := decodePayload(t, b)
		got[p.Event] = p
	}
	if p, ok := got[EventAccountDisabled]; !ok || p.Data["uid"] != "248890d9" || p.Data["reason"] != "session_dead x3" {
		t.Errorf("account_disabled payload=%+v", got[EventAccountDisabled])
	}
	if p, ok := got[EventCheckinFailed]; !ok || p.Data["uid"] != "248890d9" || p.Data["error"] != "HTTP 500" {
		t.Errorf("checkin_failed payload=%+v", got[EventCheckinFailed])
	}
}

// TestUnsubscribedEventNotSent 未订阅事件不发请求。
func TestUnsubscribedEventNotSent(t *testing.T) {
	rec := &webhookRecorder{}
	srv := newWebhook(t, rec)
	n := New(srv.URL, 2*time.Second, []string{EventJobDone})
	t.Cleanup(func() { closeNotifier(t, n) })

	n.AccountDisabled("uid8", "reason")
	n.CheckinFailed("uid8", "err")
	time.Sleep(150 * time.Millisecond)
	if got := rec.count(); got != 0 {
		t.Fatalf("未订阅事件不应发送，收到 %d 次请求", got)
	}
	n.JobDone(map[string]any{"id": "j1"})
	waitFor(t, 3*time.Second, "已订阅事件应发送", func() bool { return rec.count() == 1 })
}

// TestRetryOnceThenSuccess 首次 500、重试一次成功：请求计数=2，Sent=1。
func TestRetryOnceThenSuccess(t *testing.T) {
	rec := &webhookRecorder{failFirst: 1}
	srv := newWebhook(t, rec)
	n := New(srv.URL, 2*time.Second, nil)
	t.Cleanup(func() { closeNotifier(t, n) })

	n.JobDone(map[string]any{"id": "j1"})
	waitFor(t, 5*time.Second, "重试后计数=2 且 Sent=1", func() bool {
		return rec.count() == 2 && n.Stats().Sent == 1
	})
	if s := n.Stats(); s.Failed != 0 {
		t.Fatalf("stats=%+v，重试成功后不应计入 failed", s)
	}
}

// TestServerErrorWarnNoPanic 持续 500：只重试一次（计数=2）、计 failed，不 panic。
func TestServerErrorWarnNoPanic(t *testing.T) {
	rec := &webhookRecorder{failStatus: http.StatusInternalServerError}
	srv := newWebhook(t, rec)
	n := New(srv.URL, 2*time.Second, nil)
	t.Cleanup(func() { closeNotifier(t, n) })

	n.JobDone(map[string]any{"id": "j1"})
	waitFor(t, 5*time.Second, "失败计数=1 且请求=2", func() bool {
		return n.Stats().Failed == 1 && rec.count() == 2
	})
	if s := n.Stats(); s.Sent != 0 {
		t.Fatalf("stats=%+v want failed=1 sent=0", s)
	}
}

// TestConcurrentEmitNoDrop 并发 Emit 在缓冲容量内：全部发送、无丢弃。
func TestConcurrentEmitNoDrop(t *testing.T) {
	rec := &webhookRecorder{}
	srv := newWebhook(t, rec)
	n := New(srv.URL, 2*time.Second, nil)
	t.Cleanup(func() { closeNotifier(t, n) })

	const total = 50 // <= queueSize(64)，单 worker 慢速消费也不该丢
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n.JobDone(map[string]any{"i": i})
		}(i)
	}
	wg.Wait()
	waitFor(t, 5*time.Second, "全部事件发送完成", func() bool { return n.Stats().Sent == total })
	if s := n.Stats(); s.Dropped != 0 || s.Failed != 0 {
		t.Fatalf("stats=%+v want 无丢弃/失败", s)
	}
	if got := rec.count(); got != total {
		t.Fatalf("webhook 收到 %d 次 want %d", got, total)
	}
}

// TestQueueOverflowDrops 队列满（阻塞的 webhook）时丢弃并计数，绝不阻塞调用方。
func TestQueueOverflowDrops(t *testing.T) {
	release := make(chan struct{})
	var closeOnce sync.Once
	unblock := func() { closeOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		unblock()
		srv.Close()
	})
	n := New(srv.URL, 2*time.Second, nil)
	t.Cleanup(func() { closeNotifier(t, n) })

	// 单 worker 被第一个请求阻塞 → 后面 64 个缓冲填满 → 其余全部丢弃。
	const total = 200
	start := time.Now()
	for i := 0; i < total; i++ {
		n.JobDone(map[string]any{"i": i})
	}
	if d := n.Stats().Dropped; d == 0 {
		t.Fatalf("队列溢出应出现丢弃，stats=%+v", n.Stats())
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Emit 不应阻塞调用方（%v 完成 %d 次）", elapsed, total)
	}

	unblock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.Close(ctx); err != nil {
		t.Fatalf("Close=%v（排空剩余事件应成功）", err)
	}
	s := n.Stats()
	if s.Sent+s.Dropped != total {
		t.Fatalf("sent=%d dropped=%d want 合计 %d（failed=%d）", s.Sent, s.Dropped, total, s.Failed)
	}
}

// TestCloseDrainsAndStops Close：把已缓冲事件发完才返回；之后 Emit 不再发送；可重复调用。
func TestCloseDrainsAndStops(t *testing.T) {
	rec := &webhookRecorder{delay: 5 * time.Millisecond}
	srv := newWebhook(t, rec)
	n := New(srv.URL, 2*time.Second, nil)

	const total = 10
	for i := 0; i < total; i++ {
		n.JobDone(map[string]any{"i": i})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.Close(ctx); err != nil {
		t.Fatalf("Close=%v", err)
	}
	if got := rec.count(); got != total {
		t.Fatalf("Close 返回前应排空全部事件：收到 %d want %d", got, total)
	}

	// Close 后不再发送（Emit 进入 no-op + dropped 计数）。
	before := n.Stats().Dropped
	n.JobDone(map[string]any{"i": 999})
	time.Sleep(150 * time.Millisecond)
	if got := rec.count(); got != total {
		t.Fatalf("Close 后不应再发送：收到 %d", got)
	}
	if n.Stats().Dropped <= before {
		t.Fatalf("Close 后 Emit 应计入 dropped：before=%d after=%d", before, n.Stats().Dropped)
	}
	// 幂等。
	if err := n.Close(context.Background()); err != nil {
		t.Fatalf("重复 Close=%v", err)
	}
}

// TestCloseTimeoutAbandons Close 超时放弃等待（返回 ctx 错误），worker 后台收尾；
// 释放 webhook 后第二次 Close 能正常等到排空。
func TestCloseTimeoutAbandons(t *testing.T) {
	release := make(chan struct{})
	var closeOnce sync.Once
	unblock := func() { closeOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		unblock()
		srv.Close()
	})
	n := New(srv.URL, 2*time.Second, nil)
	n.JobDone(map[string]any{"i": 1})
	time.Sleep(50 * time.Millisecond) // 让 worker 取到事件并卡在 HTTP 上

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := n.Close(ctx)
	if err == nil {
		t.Fatal("webhook 卡住时 Close 应超时返回错误")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close err=%v want DeadlineExceeded", err)
	}

	unblock()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := n.Close(ctx2); err != nil {
		t.Fatalf("释放后 Close=%v want nil", err)
	}
}

// TestPayloadTruncation data 序列化超过 64KB → 降级为摘要（truncated/original_bytes/keys）。
func TestPayloadTruncation(t *testing.T) {
	rec := &webhookRecorder{}
	srv := newWebhook(t, rec)
	n := New(srv.URL, 2*time.Second, nil)
	t.Cleanup(func() { closeNotifier(t, n) })

	// 未超限：原样保留。
	okData := map[string]any{"id": "j-small", "blob": strings.Repeat("x", 60*1024)}
	n.Emit(context.Background(), EventJobDone, okData)
	waitFor(t, 3*time.Second, "小 payload 送达", func() bool { return rec.count() == 1 })
	if p := rec.lastBody(t); p.Data["truncated"] == true {
		t.Fatal("60KB data 不应触发截断")
	}

	// 超限：摘要化（不发送 70KB 原文）。
	big := strings.Repeat("y", 70*1024)
	n.Emit(context.Background(), EventJobDone, map[string]any{"id": "j-big", "blob": big, "extra": 1})
	waitFor(t, 3*time.Second, "大 payload 送达", func() bool { return rec.count() == 2 })
	bodies, _ := rec.snapshot()
	raw := bodies[1]
	if len(raw) > 16*1024 {
		t.Fatalf("截断后的 payload 仍过大：%d B", len(raw))
	}
	p := decodePayload(t, raw)
	if p.Data["truncated"] != true {
		t.Fatalf("超限 data 应标记 truncated: %+v", p.Data)
	}
	if orig, ok := p.Data["original_bytes"].(float64); !ok || int(orig) <= maxDataBytes {
		t.Fatalf("original_bytes=%v want > %d", p.Data["original_bytes"], maxDataBytes)
	}
	keys, ok := p.Data["keys"].([]any)
	if !ok || len(keys) != 3 {
		t.Fatalf("摘要 keys=%v want 3 个顶层键", p.Data["keys"])
	}
	if strings.Contains(string(raw), big[:1024]) {
		t.Fatal("截断后的 payload 不应包含原始大字段内容")
	}
}

// TestEmitCanceledContext ctx 已取消：不入队（计入 dropped），不发请求。
func TestEmitCanceledContext(t *testing.T) {
	rec := &webhookRecorder{}
	srv := newWebhook(t, rec)
	n := New(srv.URL, 2*time.Second, nil)
	t.Cleanup(func() { closeNotifier(t, n) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n.Emit(ctx, EventJobDone, map[string]any{"id": "j1"})
	time.Sleep(100 * time.Millisecond)
	if got := rec.count(); got != 0 {
		t.Fatalf("ctx 已取消不应发送，收到 %d 次请求", got)
	}
	if d := n.Stats().Dropped; d != 1 {
		t.Fatalf("dropped=%d want 1", d)
	}
}

// TestClientTimeout 单请求超时：2 次尝试都超时 → failed=1、请求计数=2，不 panic。
func TestClientTimeout(t *testing.T) {
	rec := &webhookRecorder{delay: 250 * time.Millisecond}
	srv := newWebhook(t, rec)
	n := New(srv.URL, 80*time.Millisecond, nil)
	t.Cleanup(func() { closeNotifier(t, n) })

	n.JobDone(map[string]any{"id": "j1"})
	waitFor(t, 5*time.Second, "两次超时尝试完成", func() bool {
		return n.Stats().Failed == 1 && rec.count() == 2
	})
}

// TestMaskWebhookURL 日志脱敏：只留 scheme://host + path 前 12 字符，绝不打印 token/query。
func TestMaskWebhookURL(t *testing.T) {
	cases := []struct {
		raw     string
		want    string
		mustNot []string
	}{
		{
			raw:     "https://hooks.example.com/services/T000/B000/XXXX?token=SECRET&x=1",
			want:    "https://hooks.example.com/services/T0…",
			mustNot: []string{"SECRET", "token", "XXXX"},
		},
		{
			raw:  "http://127.0.0.1:8080/hook",
			want: "http://127.0.0.1:8080/hook",
		},
		{
			raw:  "https://example.com",
			want: "https://example.com/",
		},
		{
			raw:  "not a url",
			want: "invalid-url",
		},
		{
			raw:  "",
			want: "",
		},
	}
	for _, tc := range cases {
		got := maskWebhookURL(tc.raw)
		if got != tc.want {
			t.Errorf("maskWebhookURL(%q)=%q want %q", tc.raw, got, tc.want)
		}
		for _, banned := range tc.mustNot {
			if strings.Contains(got, banned) {
				t.Errorf("maskWebhookURL(%q)=%q 泄露了 %q", tc.raw, got, banned)
			}
		}
	}
}

// keysOf 返回 map 的键（测试辅助，排序后便于断言输出）。
func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestFailureLogMasksWebhookURL 失败路径的日志同样不得泄露完整 webhook URL：
// query/path 里常带 token，只允许出现脱敏后的 scheme://host/path前12字符。
func TestFailureLogMasksWebhookURL(t *testing.T) {
	var buf bytes.Buffer
	oldOut := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(oldOut)

	// 先起后立刻关：拿到形如 http://127.0.0.1:PORT 的地址，连接会被拒绝（快速失败）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	urlWithToken := srv.URL + "/hook?token=SECRET123"
	srv.Close()

	n := New(urlWithToken, 300*time.Millisecond, nil)
	t.Cleanup(func() { closeNotifier(t, n) })
	n.JobDone(map[string]any{"id": "j1"})
	waitFor(t, 5*time.Second, "两次失败尝试完成", func() bool { return n.Stats().Failed == 1 })

	logs := buf.String()
	for _, banned := range []string{"SECRET123", "token="} {
		if strings.Contains(logs, banned) {
			t.Fatalf("日志泄露 webhook URL 敏感部分 %q:\n%s", banned, logs)
		}
	}
	// 脱敏后的 scheme://host/path 前缀应出现（证明日志可定位目标又不含 query）。
	if !strings.Contains(logs, "/hook") || !strings.Contains(logs, "WARN: notify:") {
		t.Fatalf("日志应包含脱敏目标 WARN；got:\n%s", logs)
	}
}
