// sse_test.go v2.4.0 #3 SSE 事件推送的服务端契约测试：
//
//   - 响应头（text/event-stream / no-cache / X-Accel-Buffering / 不被 gzip 包裹）；
//   - 首帧 snapshot 字段契约（now/version/poll_interval_sec/counts/积分合计/jobs/events_enabled）；
//   - 心跳 `: ping`、jobs/accounts 变化增量事件；
//   - 开关关闭 404、未鉴权 401、客户端上限 503、慢客户端被丢弃、hub 无客户端后自动摘除；
//   - 多客户端并发 & 断线重连重新拿 snapshot；无 Jobs Registry 时 jobs=[]。
package panel

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/jobs"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
)

const sseTestKey = "sse-test-key"

// newSSEPanel 构造启用 SSE 的面板（pool 有 n 个账号且积分非零；jobs Registry 落
// t.TempDir；Panel 数据目录也在 temp，零仓库污染）。
func newSSEPanel(t *testing.T, accounts int) (*Panel, *pool.Pool, *jobs.Registry) {
	t.Helper()
	dir := t.TempDir()
	pl := pool.New("")
	for i := 0; i < accounts; i++ {
		uid := fmt.Sprintf("u%d", i)
		pl.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
		// credits=total=100/200/...：SSE snapshot 的 credits_sum/credits_total_sum 断言用。
		pl.SetCredits(uid, int64(100*(i+1)), int64(100*(i+1)))
	}
	reg := jobs.New(jobs.Config{
		Path:           filepath.Join(dir, "jobs.json"),
		Retain:         10,
		MaxConcurrency: 2,
	})
	p := New(Config{
		Version:   "2.4.0-test",
		APIKey:    sseTestKey,
		Pool:      pl,
		Jobs:      reg,
		StateFile: filepath.Join(dir, "state.json"),
	})
	t.Cleanup(func() { reg.Close() })
	return p, pl, reg
}

// newSSETestServer 起 httptest 服务并把 Close 注册进 t.Cleanup。
// 注意顺序：t.Cleanup 是 LIFO——测试里后注册的「关闭 SSE body」会先执行，最后才
// Close server（否则 srv.Close 会等长连接，测试挂死）。所以所有 SSE 用例统一用它，
// 不用 defer srv.Close()（defer 早于 Cleanup 执行，会先卡在 server close 上）。
func newSSETestServer(t *testing.T, p http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv
}

// sseEvent 解析后的一条 SSE 帧。
type sseEvent struct {
	event   string
	data    string
	comment string
}

// sseStreamReader 后台读取 SSE 流，按帧投递（测试用 select+timeout 避免永久阻塞）。
type sseStreamReader struct {
	body   io.ReadCloser
	resp   *http.Response
	events chan sseEvent
}

func openSSEStream(t *testing.T, srv *httptest.Server, key string) *sseStreamReader {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/panel/api/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "gzip") // 验证 SSE 不被 gzip 包裹
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	r := &sseStreamReader{body: resp.Body, resp: resp, events: make(chan sseEvent, 128)}
	go func() {
		defer close(r.events)
		br := bufio.NewReader(resp.Body)
		var ev sseEvent
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if ev.event != "" || ev.comment != "" {
					r.events <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, ":"):
				ev.comment = strings.TrimSpace(strings.TrimPrefix(line, ":"))
			case strings.HasPrefix(line, "event:"):
				ev.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				ev.data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
	}()
	t.Cleanup(func() { _ = resp.Body.Close() })
	return r
}

// next 读取下一帧（3s 超时 fail）。
func (r *sseStreamReader) next(t *testing.T) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-r.events:
		if !ok {
			t.Fatal("SSE 流意外关闭")
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("等待 SSE 帧超时")
	}
	return sseEvent{}
}

// close 主动断开（模拟浏览器关闭页面）。
func (r *sseStreamReader) close() { _ = r.body.Close() }

// nextSnapshot 跳过注释/增量，读到下一个 snapshot。
func (r *sseStreamReader) nextSnapshot(t *testing.T) sseSnapshot {
	t.Helper()
	for i := 0; i < 20; i++ {
		ev := r.next(t)
		if ev.event != "snapshot" {
			continue
		}
		var s sseSnapshot
		if err := json.Unmarshal([]byte(ev.data), &s); err != nil {
			t.Fatalf("snapshot JSON: %v; data=%s", err, ev.data)
		}
		return s
	}
	t.Fatal("未收到 snapshot 事件")
	return sseSnapshot{}
}

// setSSEInterval 注入短快照间隔（测试加速；t.Cleanup 恢复）。
func setSSEInterval(t *testing.T, interval time.Duration) {
	t.Helper()
	old := sseSnapshotInterval
	sseSnapshotInterval = interval
	t.Cleanup(func() { sseSnapshotInterval = old })
}

// setSSEHeartbeat 注入短心跳间隔（测试加速；t.Cleanup 恢复）。
func setSSEHeartbeat(t *testing.T, interval time.Duration) {
	t.Helper()
	old := sseHeartbeatInterval
	sseHeartbeatInterval = interval
	t.Cleanup(func() { sseHeartbeatInterval = old })
}

// TestSSEHeadersAndFirstSnapshot 响应头 + 首帧 snapshot 全字段契约。
func TestSSEHeadersAndFirstSnapshot(t *testing.T) {
	p, _, _ := newSSEPanel(t, 2)
	srv := newSSETestServer(t, p)

	r := openSSEStream(t, srv, sseTestKey)
	resp := r.resp
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type=%q want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control=%q want no-cache", cc)
	}
	if xa := resp.Header.Get("X-Accel-Buffering"); xa != "no" {
		t.Errorf("X-Accel-Buffering=%q want no", xa)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Errorf("SSE 不应被 gzip 包裹，Content-Encoding=%q", enc)
	}
	if retry := resp.Header.Get("Retry-After"); retry != "" {
		t.Errorf("SSE 不应带 Retry-After: %q", retry)
	}

	snap := r.nextSnapshot(t)
	if snap.Version != "2.4.0-test" {
		t.Errorf("version=%q want 2.4.0-test", snap.Version)
	}
	if snap.PollIntervalSec < 2 || snap.PollIntervalSec > 60 {
		t.Errorf("poll_interval_sec=%d want 2..60（面板轮询间隔同口径）", snap.PollIntervalSec)
	}
	if snap.Counts.Total != 2 || snap.Counts.Healthy != 2 {
		t.Errorf("counts=%+v want total=2 healthy=2", snap.Counts)
	}
	// 积分合计：100+200=300，总额度 300（SetCredits 第二参为 total）
	if snap.CreditsSum != 300 || snap.CreditsTotalSum != 300 {
		t.Errorf("credits_sum=%d total_sum=%d want 300/300", snap.CreditsSum, snap.CreditsTotalSum)
	}
	if !snap.EventsEnabled {
		t.Error("events_enabled 应为 true")
	}
	if len(snap.Jobs) != 0 {
		t.Errorf("jobs=%+v want empty", snap.Jobs)
	}
	if snap.Now == "" || snap.NowUnix <= 0 {
		t.Errorf("now=%q now_unix=%d 应非空", snap.Now, snap.NowUnix)
	}
	if _, err := time.Parse(time.RFC3339Nano, snap.Now); err != nil {
		t.Errorf("now 应为 RFC3339 时间: %v", err)
	}
}

// TestSSEDisabledReturns404 panel.events_enabled=false → 404（前端回落轮询）。
func TestSSEDisabledReturns404(t *testing.T) {
	p, _, _ := newSSEPanel(t, 1)
	SetEventsEnabled(false)
	t.Cleanup(func() { SetEventsEnabled(true) })
	srv := newSSETestServer(t, p)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/panel/api/events", nil)
	req.Header.Set("Authorization", "Bearer "+sseTestKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want 404; body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "events disabled") {
		t.Fatalf("body=%s want error events disabled", body)
	}
	// 关闭时不应创建 hub。
	if _, ok := sseHubs.Load(p); ok {
		t.Fatal("disabled 时不应创建 hub")
	}
}

// TestSSEUnauthorized 无 Bearer → 401（withAuth 已覆盖，此处钉住 SSE 路由也受保护）。
func TestSSEUnauthorized(t *testing.T) {
	p, _, _ := newSSEPanel(t, 1)
	srv := newSSETestServer(t, p)
	resp, err := http.Get(srv.URL + "/panel/api/events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", resp.StatusCode)
	}
}

// TestSSEHeartbeat 心跳注释行 `: ping` 按间隔发送。
func TestSSEHeartbeat(t *testing.T) {
	setSSEHeartbeat(t, 25*time.Millisecond)
	p, _, _ := newSSEPanel(t, 1)
	srv := newSSETestServer(t, p)
	r := openSSEStream(t, srv, sseTestKey)
	r.nextSnapshot(t)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ev := r.next(t)
		if ev.comment == "ping" {
			return
		}
	}
	t.Fatal("未收到心跳注释行 : ping")
}

// TestSSEJobsBroadcast Job 列表变化 → 推 event: jobs（含新 Job 精简字段）。
func TestSSEJobsBroadcast(t *testing.T) {
	setSSEInterval(t, 20*time.Millisecond)
	p, _, reg := newSSEPanel(t, 1)
	srv := newSSETestServer(t, p)
	r := openSSEStream(t, srv, sseTestKey)
	r.nextSnapshot(t)
	// 再读一帧：确认 hub 已完成首个 tick 并建立基线（否则「变更前基线」可能包含
	// 紧接着 Record 的 Job，导致变化检测永远不触发）。
	r.nextSnapshot(t)

	rec := reg.Record(jobs.Job{
		Kind: jobs.KindTasksRun, Trigger: jobs.TriggerManual, State: jobs.StateDone,
		Total: 3, Done: 2, Skip: 1,
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ev := r.next(t)
		if ev.event != "jobs" {
			continue
		}
		var payload sseJobsEvent
		if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
			t.Fatalf("jobs JSON: %v; data=%s", err, ev.data)
		}
		if len(payload.Jobs) != 1 || payload.Jobs[0].ID != rec.ID || payload.Jobs[0].Kind != "tasks_run" {
			t.Fatalf("jobs payload=%+v want id=%s", payload.Jobs, rec.ID)
		}
		return
	}
	t.Fatal("未收到 jobs 增量事件")
}

// TestSSEAccountsBroadcast 池计数/积分变化 → 推 event: accounts。
func TestSSEAccountsBroadcast(t *testing.T) {
	setSSEInterval(t, 20*time.Millisecond)
	p, pl, _ := newSSEPanel(t, 1)
	srv := newSSETestServer(t, p)
	r := openSSEStream(t, srv, sseTestKey)
	r.nextSnapshot(t)
	// 等 hub 首个 tick 建立基线（同 Jobs 用例的解释）。
	r.nextSnapshot(t)

	// 新账号加入（total 1→2）。
	pl.Add(&auth.Auth{UID: "u-extra", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ev := r.next(t)
		if ev.event != "accounts" {
			continue
		}
		var payload sseAccountsEvent
		if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
			t.Fatalf("accounts JSON: %v; data=%s", err, ev.data)
		}
		if payload.Counts.Total != 2 {
			t.Fatalf("accounts payload counts=%+v want total=2", payload.Counts)
		}
		return
	}
	t.Fatal("未收到 accounts 增量事件")
}

// TestSSESlowClientDropped hub 级确定性测试：客户端队列塞满即被断开（不阻塞 hub）。
func TestSSESlowClientDropped(t *testing.T) {
	p, _, _ := newSSEPanel(t, 1)
	h := newSSEHub(p)
	c, err := h.register()
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	msg := sseMessage{event: "snapshot", data: []byte(`{}`)}
	for i := 0; i < sseClientBuffer; i++ {
		h.broadcast(msg) // 填满缓冲（没有写循环消费）
	}
	select {
	case <-c.done:
		// 尚未触发（缓冲区刚好填满，未溢出）
		t.Fatalf("缓冲未溢出时不应关闭客户端")
	default:
	}
	h.broadcast(msg) // 第 17 条 → 慢客户端丢弃
	select {
	case <-c.done:
	case <-time.After(time.Second):
		t.Fatal("慢客户端应被关闭")
	}
	h.mu.Lock()
	n := len(h.clients)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("慢客户端应从 hub 摘除，clients=%d", n)
	}
}

// TestSSEMaxClients 单 hub 客户端上限：超过返回 503（不无限累积连接）。
func TestSSEMaxClients(t *testing.T) {
	old := sseMaxClients
	sseMaxClients = 2
	t.Cleanup(func() { sseMaxClients = old })

	p, _, _ := newSSEPanel(t, 1)
	srv := newSSETestServer(t, p)

	r1 := openSSEStream(t, srv, sseTestKey)
	r1.nextSnapshot(t)
	r2 := openSSEStream(t, srv, sseTestKey)
	r2.nextSnapshot(t)
	defer r1.close()
	defer r2.close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/panel/api/events", nil)
	req.Header.Set("Authorization", "Bearer "+sseTestKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("third GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("third client status=%d want 503; body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "too many") {
		t.Fatalf("body=%s want too many clients", body)
	}
}

// TestSSEHubCleanupAfterDisconnect 最后一个客户端断开后 hub 停止并摘除（无 goroutine 泄漏）。
func TestSSEHubCleanupAfterDisconnect(t *testing.T) {
	p, _, _ := newSSEPanel(t, 1)
	srv := newSSETestServer(t, p)
	r := openSSEStream(t, srv, sseTestKey)
	r.nextSnapshot(t)
	if _, ok := sseHubs.Load(p); !ok {
		t.Fatal("连接期间应有 hub")
	}
	r.close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := sseHubs.Load(p); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("客户端断开后 hub 未从注册表摘除（goroutine 泄漏）")
}

// TestSSEMultipleClientsConcurrent 多客户端并发：各自拿到首帧 snapshot。
func TestSSEMultipleClientsConcurrent(t *testing.T) {
	p, _, _ := newSSEPanel(t, 2)
	srv := newSSETestServer(t, p)

	var wg sync.WaitGroup
	errs := make(chan string, 4)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/panel/api/events", nil)
			req.Header.Set("Authorization", "Bearer "+sseTestKey)
			client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
			resp, err := client.Do(req)
			if err != nil {
				errs <- "do: " + err.Error()
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Sprintf("status=%d", resp.StatusCode)
				return
			}
			// 读到首个 snapshot 的 data 行为止。
			br := bufio.NewReader(resp.Body)
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					errs <- "read: " + err.Error()
					return
				}
				if strings.HasPrefix(line, "data:") && strings.Contains(line, `"events_enabled"`) {
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("并发客户端失败: %s", e)
	}
}

// TestSSEReconnectGetsSnapshot 断线重连：新连接立即重新拿到 snapshot（v1 无回放）。
func TestSSEReconnectGetsSnapshot(t *testing.T) {
	p, _, _ := newSSEPanel(t, 1)
	srv := newSSETestServer(t, p)

	r1 := openSSEStream(t, srv, sseTestKey)
	if snap := r1.nextSnapshot(t); snap.Version != "2.4.0-test" {
		t.Fatalf("first snapshot version=%q", snap.Version)
	}
	r1.close()

	r2 := openSSEStream(t, srv, sseTestKey)
	defer r2.close()
	if snap := r2.nextSnapshot(t); snap.Counts.Total != 1 {
		t.Fatalf("reconnect snapshot counts=%+v want total=1", snap.Counts)
	}
}

// TestSSENoJobsRegistryJobsEmptyArray 无 Jobs Registry 时 jobs 字段为 []（不是 null）。
func TestSSENoJobsRegistryJobsEmptyArray(t *testing.T) {
	dir := t.TempDir()
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p := New(Config{Version: "2.4.0-test", APIKey: sseTestKey, Pool: pl, StateFile: filepath.Join(dir, "state.json")})
	srv := newSSETestServer(t, p)
	r := openSSEStream(t, srv, sseTestKey)
	if snap := r.nextSnapshot(t); snap.Jobs == nil {
		t.Fatal("jobs 应为空数组而非 null")
	}
	// 原始 JSON 也确认是 "jobs":[]（前端 JSON.parse 后同样要求数组）。
	if !strings.Contains(string(mustSnapshotData(t, srv)), `"jobs":[]`) {
		t.Fatalf("snapshot data 应含 jobs:[]；got %s", mustSnapshotData(t, srv))
	}
}

// mustSnapshotData 另开一条流读首个 snapshot 的原始 data 行（用于原始 JSON 断言）。
func mustSnapshotData(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/panel/api/events", nil)
	req.Header.Set("Authorization", "Bearer "+sseTestKey)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.HasPrefix(line, "data:") {
			return []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
}
