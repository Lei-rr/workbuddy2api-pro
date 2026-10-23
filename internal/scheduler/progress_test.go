// progress_test.go 钉住 v2.3.0 #10 的 scheduler ctx + 进度变体契约：
//
//   - 旧 Run*Now 与新的 Run*Ctx(ctx, progress) 行为兼容（ctx=Background/progress=nil）；
//   - 逐号上报累计计数：done=已处理账号数、fail、skip、total；
//   - ctx 取消后账号循环尽早停止（不再处理剩余账号）；
//   - 余额刷新并发上报不 panic（回调需并发安全）。
package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// progressCapture 线程安全的进度快照收集器。
type progressCapture struct {
	mu    sync.Mutex
	calls []struct {
		Done, Total, Fail, Skip int
		Msg                     string
	}
}

func (c *progressCapture) fn(done, total, fail, skip int, msg string) {
	c.mu.Lock()
	c.calls = append(c.calls, struct {
		Done, Total, Fail, Skip int
		Msg                     string
	}{done, total, fail, skip, msg})
	c.mu.Unlock()
}

// last 返回最后一次进度（无任何上报时 ok=false）。
func (c *progressCapture) last() (done, total, fail, skip int, msg string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		return 0, 0, 0, 0, "", false
	}
	last := c.calls[len(c.calls)-1]
	return last.Done, last.Total, last.Fail, last.Skip, last.Msg, true
}

func (c *progressCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// newProgressPool 建 n 个可用 CN 账号的池。
func newProgressPool(n int) *pool.Pool {
	p := pool.New("")
	for i := 0; i < n; i++ {
		uid := string(rune('a' + i))
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	return p
}

// TestRunCheckinCtxProgress 签到逐号上报：2 个账号全部成功 → 最后一次 done=2/total=2。
func TestRunCheckinCtxProgress(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 100}
	srv := f.server()
	defer srv.Close()

	p := newProgressPool(2)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	cap := &progressCapture{}
	if err := s.RunCheckinCtx(context.Background(), cap.fn); err != nil {
		t.Fatalf("RunCheckinCtx: %v", err)
	}
	done, total, fail, skip, _, ok := cap.last()
	if !ok || done != 2 || total != 2 || fail != 0 || skip != 0 {
		t.Fatalf("last progress = (%d/%d fail=%d skip=%d ok=%v) want 2/2/0/0", done, total, fail, skip, ok)
	}
	if got := f.checkinCalls.Load(); got != 2 {
		t.Fatalf("checkin calls=%d want 2", got)
	}
}

// TestRunCheckinCtxCancelStopsLoop ctx 在进度回调里被取消：派发即停，剩余账号
// 不再处理（v2.3.1 #11 worker pool；并发固定 1 让「取消后不再处理」可确定断言）。
func TestRunCheckinCtxCancelStopsLoop(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 100}
	srv := f.server()
	defer srv.Close()

	p := newProgressPool(5)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, CheckinConcurrency: 1})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var canceled atomic.Bool
	progress := func(done, total, fail, skip int, msg string) {
		if done == 1 && !canceled.Swap(true) {
			cancel() // 处理完第 1 个账号后取消
		}
	}
	_ = s.RunCheckinCtx(ctx, progress)

	if got := f.checkinCalls.Load(); got != 1 {
		t.Fatalf("canceled checkin calls=%d want 1（ctx 取消后不再处理剩余账号）", got)
	}
	// 半轮不记「今天已跑」：补跑窗口仍可在下一轮补上。
	if s.taskRanOn(taskCheckin, s.now()) {
		t.Fatal("canceled checkin run must not mark today as done")
	}
}

// TestRunTravelCtxCountsFailures 旅行逐号上报：上游全部 500 → fail 计数增加，
// total=账号数（遍历仍会走完，单号失败不中断）。
func TestRunTravelCtxCountsFailures(t *testing.T) {
	fastTravel(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newProgressPool(2)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	cap := &progressCapture{}
	if err := s.RunTravelCtx(context.Background(), cap.fn); err != nil {
		t.Fatalf("RunTravelCtx: %v", err)
	}
	done, total, fail, _, _, ok := cap.last()
	if !ok || done != 2 || total != 2 || fail != 2 {
		t.Fatalf("last progress = (%d/%d fail=%d ok=%v) want 2/2/2", done, total, fail, ok)
	}
}

// TestRunBalanceRefreshCtxProgress 余额刷新（并发路径）：进度回调可并发，
// 最终计数 done=账号数；ctx 已取消时直接返回不发请求。
func TestRunBalanceRefreshCtxProgress(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":55,"CycleCapacityUsed":0}]}}}}`))
	}))
	defer srv.Close()

	p := newProgressPool(3)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, BalanceRefreshConcurrency: 2})

	cap := &progressCapture{}
	if err := s.RunBalanceRefreshCtx(context.Background(), cap.fn); err != nil {
		t.Fatalf("RunBalanceRefreshCtx: %v", err)
	}
	done, total, fail, _, _, ok := cap.last()
	if !ok || done != 3 || total != 3 || fail != 0 {
		t.Fatalf("last progress = (%d/%d fail=%d ok=%v) want 3/3/0", done, total, fail, ok)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("balance calls=%d want 3", got)
	}

	// ctx 已取消：不派发任何请求。
	before := calls.Load()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = s.RunBalanceRefreshCtx(ctx, nil)
	if got := calls.Load(); got != before {
		t.Fatalf("canceled balance refresh should not call upstream, calls %d→%d", before, got)
	}
}

// TestRunKeepaliveCtxCountsFailure 保活逐号上报：刷新失败账号计入 fail。
func TestRunKeepaliveCtxCountsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"code":10001,"msg":"bad"}`))
	}))
	defer srv.Close()

	// fakeUpstream 只模拟 billing；keepalive 走 refresh 路径，这里用独立 server 全 400。
	f := &fakeUpstream{}
	_ = f
	p := newProgressPool(2)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	cap := &progressCapture{}
	if err := s.RunKeepaliveCtx(context.Background(), cap.fn); err != nil {
		t.Fatalf("RunKeepaliveCtx: %v", err)
	}
	done, total, fail, _, _, ok := cap.last()
	if !ok || done != 2 || total != 2 || fail != 2 {
		t.Fatalf("last progress = (%d/%d fail=%d ok=%v) want 2/2/2", done, total, fail, ok)
	}
}

// TestRunCtxVariantsWithNilProgress 旧入口语义保持：panic-free（progress=nil）。
func TestRunCtxVariantsWithNilProgress(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 10}
	srv := f.server()
	defer srv.Close()
	p := newProgressPool(1)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	ctx := context.Background()
	if err := s.RunCheckinCtx(ctx, nil); err != nil {
		t.Fatalf("checkin nil progress: %v", err)
	}
	if err := s.RunBalanceRefreshCtx(ctx, nil); err != nil {
		t.Fatalf("balance nil progress: %v", err)
	}
	if err := s.RunKeepaliveCtx(ctx, nil); err != nil {
		t.Fatalf("keepalive nil progress: %v", err)
	}
	fastTravel(t)
	if err := s.RunTravelCtx(ctx, nil); err != nil {
		t.Fatalf("travel nil progress: %v", err)
	}
	if err := s.RunActivityCtx(ctx, nil); err != nil {
		t.Fatalf("activity nil progress: %v", err)
	}
}
