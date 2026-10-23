// shutdown_test.go 钉住 v2.2 #14 的停机编排语义（用可注入依赖 + 真实 httptest 端口）：
//
//   - 第一次信号：cancel 后台 ctx → srv.Shutdown 等在途请求（含长请求）结束 →
//     等后台 goroutine 收敛 → **最后**落盘（flush 必须在 Shutdown 之后，避免
//     flush 后又被在途请求写脏——旧实现 5s 硬编码 + 先 flush 的缺陷）；
//   - Shutdown 返回后新连接被拒绝；
//   - 长请求超过 shutdown_timeout → TimedOut=true 但整体仍按时返回（不阻塞退出）；
//   - Timeout 缺省 = 30s。
package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
)

// TestGracefulShutdownPersistsInFlightWrites 端到端「状态落盘被调用」：在途请求
// 写池状态（模拟最后一次成功计数）→ 停机编排等它结束 → 最后 flush → state.json
// 里能看到这次写入（修复旧实现「flush 在 Shutdown 之前，最后一次变更可能丢」）。
func TestGracefulShutdownPersistsInFlightWrites(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := pool.New(fp)
	p.Add(&auth.Auth{UID: "u1"})

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv, addr := startTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		p.SetCredits("u1", 42, 0) // 在途请求写池状态（置 dirty）
	}))
	go func() { _, _ = http.Get("http://" + addr + "/write") }()
	<-started

	shutdownCh := make(chan shutdownResult, 1)
	go func() {
		shutdownCh <- gracefulShutdown(shutdownDeps{
			Shutter: srv,
			Flush:   p.Flush,
			Timeout: 5 * time.Second,
		})
	}()
	// 等监听关闭（Shutdown 已开始等待在途）后放行写操作。
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("server listener not closed by Shutdown")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	res := <-shutdownCh
	if res.TimedOut {
		t.Fatalf("unexpected timeout: %+v", res)
	}

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("state.json should be flushed during shutdown: %v", err)
	}
	if !strings.Contains(string(raw), `"credits": 42`) {
		t.Fatalf("in-flight write must be persisted by final flush:\n%s", raw)
	}
	p.Close()
}

// fakeShutter 注入确定性 Shutdown 行为的 httpShutter 实现（不需要真实端口）。
type fakeShutter struct {
	called   atomic.Bool
	err      error
	onShutdn func()
}

func (f *fakeShutter) Shutdown(ctx context.Context) error {
	f.called.Store(true)
	if f.onShutdn != nil {
		f.onShutdn()
	}
	return f.err
}

// eventLog 记录停机各阶段顺序（并发安全）。
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(s string) {
	l.mu.Lock()
	l.events = append(l.events, s)
	l.mu.Unlock()
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// startTestHTTPServer 在 127.0.0.1:0 起真实 http.Server，返回 server 与监听地址。
func startTestHTTPServer(t *testing.T, h http.Handler) (*http.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return srv, ln.Addr().String()
}

// TestGracefulShutdownOrderAndDrain 核心用例：cancel → Shutdown（等在途完成）→
// 后台收敛 → flush；在途请求拿到完整响应，停机后新连接被拒绝。
func TestGracefulShutdownOrderAndDrain(t *testing.T) {
	log := &eventLog{}
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv, addr := startTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		_, _ = io.WriteString(w, "slow-ok")
		log.add("request-done")
	}))

	// 在途请求（模拟长 SSE/慢请求）。
	type respResult struct {
		body string
		err  error
	}
	respCh := make(chan respResult, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/slow")
		if err != nil {
			respCh <- respResult{err: err}
			return
		}
		defer resp.Body.Close()
		body, rerr := io.ReadAll(resp.Body)
		respCh <- respResult{body: string(body), err: rerr}
	}()
	<-started

	bgCtx, bgCancel := context.WithCancel(context.Background())
	var bg sync.WaitGroup
	bg.Add(1)
	go func() {
		defer bg.Done()
		<-bgCtx.Done()
		log.add("bg-done")
	}()

	shutdownCh := make(chan shutdownResult, 1)
	go func() {
		shutdownCh <- gracefulShutdown(shutdownDeps{
			Shutter:    srv,
			Cancel:     func() { log.add("cancel"); bgCancel() },
			Background: &bg,
			Flush:      func() { log.add("flush") },
			Timeout:    5 * time.Second,
		})
	}()

	// 确认 Shutdown 已经开始（listener 已关）后放行长请求，验证「等它跑完再 flush」。
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err != nil {
			break // 监听已关：Shutdown 已进入等待在途阶段
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("server listener not closed by Shutdown")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)

	res := <-shutdownCh
	got := <-respCh
	if got.err != nil || got.body != "slow-ok" {
		t.Fatalf("in-flight request should complete normally: body=%q err=%v", got.body, got.err)
	}
	if res.TimedOut {
		t.Fatal("in-flight request finished in time, TimedOut should be false")
	}
	if !res.BackgroundDone {
		t.Fatal("background goroutine should converge")
	}

	// 阶段顺序：cancel → bg-done/request-done → flush（flush 必须是最后一步）。
	events := log.snapshot()
	if len(events) < 4 {
		t.Fatalf("missing shutdown phases: %v", events)
	}
	if events[0] != "cancel" {
		t.Fatalf("cancel must be first, got %v", events)
	}
	if events[len(events)-1] != "flush" {
		t.Fatalf("flush must be last (after HTTP drain), got %v", events)
	}
	// 新连接一律被拒（Shutdown 已关闭监听）。
	if conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("new connection should be refused after graceful shutdown")
	}
}

// TestGracefulShutdownFlushAfterShutdown 用 fake shutter 确定性钉住「flush 在
// Shutdown 之后」的顺序（真实端口用例的补充）。
func TestGracefulShutdownFlushAfterShutdown(t *testing.T) {
	log := &eventLog{}
	fake := &fakeShutter{onShutdn: func() { log.add("shutdown") }}
	res := gracefulShutdown(shutdownDeps{
		Shutter: fake,
		Cancel:  func() { log.add("cancel") },
		Flush:   func() { log.add("flush") },
		Timeout: time.Second,
	})
	want := []string{"cancel", "shutdown", "flush"}
	got := log.snapshot()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order=%v want %v", got, want)
	}
	if res.TimedOut {
		t.Fatalf("unexpected timeout: %+v", res)
	}
}

// TestGracefulShutdownTimeoutCapped 长请求超过预算：Shutdown 返回 deadline exceeded
// → TimedOut=true，整体按预算返回（不无限等），flush 仍执行。
func TestGracefulShutdownTimeoutCapped(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv, addr := startTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
	}))
	go func() {
		_, _ = http.Get("http://" + addr + "/hang")
	}()
	<-started

	flushed := atomic.Bool{}
	start := time.Now()
	res := gracefulShutdown(shutdownDeps{
		Shutter: srv,
		Flush:   func() { flushed.Store(true) },
		Timeout: 150 * time.Millisecond,
	})
	elapsed := time.Since(start)
	close(release) // 收尾：放行 handler，避免清理阶段长时间阻塞
	if !res.TimedOut {
		t.Fatalf("expected TimedOut=true (srv.Shutdown deadline exceeded), got %+v", res)
	}
	if !flushed.Load() {
		t.Fatal("flush must still run after timeout")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("shutdown must respect timeout budget, took %v", elapsed)
	}
}

// TestGracefulShutdownBackgroundTimeout 后台任务不收敛：只 WARN、不阻塞退出；
// BackgroundDone=false；耗时仍受预算约束。
func TestGracefulShutdownBackgroundTimeout(t *testing.T) {
	hold := make(chan struct{})
	var bg sync.WaitGroup
	bg.Add(1)
	go func() { defer bg.Done(); <-hold }()

	start := time.Now()
	res := gracefulShutdown(shutdownDeps{
		Background: &bg,
		Timeout:    120 * time.Millisecond,
	})
	elapsed := time.Since(start)
	close(hold) // 清理等待中的 goroutine
	if res.BackgroundDone {
		t.Fatal("blocked background task should not report done")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("shutdown must not block on background tasks, took %v", elapsed)
	}
}

// TestGracefulShutdownDetectsCanceledShutter fake shutter 返回 context.Canceled 也按
// 「未在时限内结束」记录 TimedOut（编排只 WARN，不影响后续 flush）。
func TestGracefulShutdownDetectsCanceledShutter(t *testing.T) {
	res := gracefulShutdown(shutdownDeps{
		Shutter: &fakeShutter{err: context.Canceled},
		Flush:   func() {},
		Timeout: time.Second,
	})
	if !res.TimedOut {
		t.Fatalf("context.Canceled should set TimedOut, got %+v", res)
	}
}

// TestGracefulShutdownDefaultTimeout 显式 Timeout=0 → 使用默认 30s 预算。
func TestGracefulShutdownDefaultTimeout(t *testing.T) {
	var remaining time.Duration
	fake := &fakeShutter{onShutdn: func() {}}
	shutter := &deadlineProbe{inner: fake, remaining: &remaining}
	_ = gracefulShutdown(shutdownDeps{Shutter: shutter, Flush: func() {}, Timeout: 0})
	if remaining < 25*time.Second || remaining > 31*time.Second {
		t.Fatalf("default shutdown budget=%v want ~30s", remaining)
	}
}

// deadlineProbe 记录传入 Shutdown 的 ctx 剩余时间。
type deadlineProbe struct {
	inner     httpShutter
	remaining *time.Duration
}

func (d *deadlineProbe) Shutdown(ctx context.Context) error {
	dl, ok := ctx.Deadline()
	if ok {
		*d.remaining = time.Until(dl)
	}
	return d.inner.Shutdown(ctx)
}

// TestGracefulShutdownNilDeps nil 依赖不 panic（防御性：部分依赖未装配的测试路径）。
func TestGracefulShutdownNilDeps(t *testing.T) {
	res := gracefulShutdown(shutdownDeps{})
	if res.TimedOut {
		t.Fatalf("nil deps should not time out: %+v", res)
	}
}

// TestGracefulShutdownShutterErrorStillFlushes Shutdown 返回非超时错误（如内部错误）
// 时：TimedOut=false，但 flush 仍必须执行（收尾不因停机阶段报错而跳过）。
func TestGracefulShutdownShutterErrorStillFlushes(t *testing.T) {
	flushed := atomic.Bool{}
	res := gracefulShutdown(shutdownDeps{
		Shutter: &fakeShutter{err: errors.New("boom")},
		Flush:   func() { flushed.Store(true) },
		Timeout: time.Second,
	})
	if !flushed.Load() {
		t.Fatal("flush must run even when Shutdown returns a non-timeout error")
	}
	if res.TimedOut {
		t.Fatalf("non-deadline error should not be reported as TimedOut: %+v", res)
	}
}
