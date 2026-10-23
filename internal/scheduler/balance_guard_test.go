// balance_guard_test.go 钉住 v2.0.1 #10 的两个子项：
//
//   - RunBalanceRefreshNow 的并发闸门有界（信号量=balance_refresh_concurrency），
//     不再对每个账号起一个无上限 goroutine；后台周期/启动首刷/手动刷新共用同一
//     单飞 gate（同类不叠加）。
//   - 启动首刷保护：先按 balance_refresh_startup_delay 延迟；账号数 > 200 且未
//     显式 force → 跳过首刷（周期刷新照常，配置可恢复）。
package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// balanceFake 模拟 get-user-resource：统计调用数/在途峰值，可注入响应延迟。
type balanceFake struct {
	calls       atomic.Int32
	inflight    atomic.Int32
	maxInflight atomic.Int32
	delay       time.Duration
}

func (f *balanceFake) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		cur := f.inflight.Add(1)
		for {
			m := f.maxInflight.Load()
			if cur <= m || f.maxInflight.CompareAndSwap(m, cur) {
				break
			}
		}
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		f.inflight.Add(-1)
		w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`))
	}))
}

// balanceTestPool 构造 n 个可用账号（只用于余额刷新遍历，无凭证文件落盘）。
func balanceTestPool(n int) *pool.Pool {
	p := pool.New("")
	for i := 0; i < n; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%03d", i), AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	return p
}

func balanceTestClient(srv *httptest.Server) *upstream.Client {
	return &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
}

// TestRunBalanceRefreshBoundedConcurrency 16 个账号、并发 3：调用 16 次、峰值在途
// <= 3（有界），且确实并发（峰值 >= 2，不是退化成串行）。
func TestRunBalanceRefreshBoundedConcurrency(t *testing.T) {
	f := &balanceFake{delay: 10 * time.Millisecond}
	srv := f.server()
	defer srv.Close()

	p := balanceTestPool(16)
	s := New(Config{
		Pool:                      p,
		Upstream:                  balanceTestClient(srv),
		BalanceRefreshConcurrency: 3,
	})
	s.RunBalanceRefreshNow()

	if got := f.calls.Load(); got != 16 {
		t.Errorf("calls=%d want 16", got)
	}
	if max := f.maxInflight.Load(); max > 3 {
		t.Errorf("峰值在途=%d 超过并发上限 3（无上限 goroutine 回归？）", max)
	} else if max < 2 {
		t.Errorf("峰值在途=%d，未观察到并发（闸门退化成串行）", max)
	}
}

// TestBalanceRefreshConcurrencyNormalize 并发配置归一：默认 8、<=0 回落 8、
// 上限 32；setter 热改同样夹取（面板保存走这条路径）。
func TestBalanceRefreshConcurrencyNormalize(t *testing.T) {
	s := New(Config{})
	if got := s.BalanceRefreshConcurrency(); got != 8 {
		t.Errorf("默认并发=%d want 8", got)
	}
	s.SetBalanceRefreshConcurrency(0)
	if got := s.BalanceRefreshConcurrency(); got != 8 {
		t.Errorf("并发 0 应回落 8，got %d", got)
	}
	s.SetBalanceRefreshConcurrency(99)
	if got := s.BalanceRefreshConcurrency(); got != 32 {
		t.Errorf("并发 99 应 clamp 32，got %d", got)
	}
	s.SetBalanceRefreshConcurrency(4)
	if got := s.BalanceRefreshConcurrency(); got != 4 {
		t.Errorf("并发 4 应保持，got %d", got)
	}
	if got := New(Config{BalanceRefreshConcurrency: 100}).BalanceRefreshConcurrency(); got != 32 {
		t.Errorf("构造参数 100 应 clamp 32，got %d", got)
	}
}

// TestBalanceStartupSkipDecision 启动首刷跳过判定的纯函数口径：
// 只有「未 force 且账号数 > 200」才跳过。
func TestBalanceStartupSkipDecision(t *testing.T) {
	cases := []struct {
		accounts int
		force    bool
		want     bool
	}{
		{0, false, false},
		{200, false, false}, // 恰好阈值不跳过
		{201, false, true},  // 超过阈值跳过
		{5000, false, true}, // 大池跳过
		{201, true, false},  // 显式 force：照刷
		{5000, true, false}, // 显式 force：大池也照刷
	}
	for _, tc := range cases {
		if got := balanceStartupSkip(tc.accounts, tc.force); got != tc.want {
			t.Errorf("balanceStartupSkip(%d, force=%v)=%v want %v", tc.accounts, tc.force, got, tc.want)
		}
	}
}

// TestStartBalanceRefreshSkipsLargePoolStartup 账号数 > 200 且未 force：
// 启动首刷整体跳过（不打任何上游请求），也不落完成时刻；周期刷新不受影响。
func TestStartBalanceRefreshSkipsLargePoolStartup(t *testing.T) {
	f := &balanceFake{}
	srv := f.server()
	defer srv.Close()

	p := balanceTestPool(balanceStartupSkipThreshold + 1)
	s := New(Config{Pool: p, Upstream: balanceTestClient(srv)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartBalanceRefresh(ctx, time.Hour)

	time.Sleep(200 * time.Millisecond) // 首刷是延迟 0 的异步动作，给足调度时间
	if got := f.calls.Load(); got != 0 {
		t.Fatalf("大池未 force 时启动首刷应跳过，calls=%d", got)
	}
	if !s.BalanceRefreshedAt().IsZero() {
		t.Fatal("跳过首刷不应记录完成时刻")
	}
}

// TestStartBalanceRefreshStartupDelay 启动首刷延迟：延迟期内不发请求，延迟满后
// 自动执行一轮（force 打开，排除大池跳过分支）。
func TestStartBalanceRefreshStartupDelay(t *testing.T) {
	f := &balanceFake{}
	srv := f.server()
	defer srv.Close()

	p := balanceTestPool(2)
	s := New(Config{
		Pool:                       p,
		Upstream:                   balanceTestClient(srv),
		BalanceRefreshStartupDelay: 120 * time.Millisecond,
		BalanceRefreshStartupForce: true,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartBalanceRefresh(ctx, time.Hour)

	time.Sleep(40 * time.Millisecond)
	if !s.BalanceRefreshedAt().IsZero() {
		t.Fatal("延迟未满不应开始首刷")
	}
	deadline := time.Now().Add(3 * time.Second)
	for s.BalanceRefreshedAt().IsZero() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.BalanceRefreshedAt().IsZero() {
		t.Fatal("延迟满后首刷未执行")
	}
	if got := f.calls.Load(); got != 2 {
		t.Errorf("首刷 calls=%d want 2", got)
	}
}

// TestStartBalanceRefreshFirstRefreshSingleFlight 启动首刷与手动刷新共用单飞
// gate：gate 被占（手动 balance_all 正在执行）→ 首刷跳过、不并发打上游。
func TestStartBalanceRefreshFirstRefreshSingleFlight(t *testing.T) {
	f := &balanceFake{}
	srv := f.server()
	defer srv.Close()

	p := balanceTestPool(2)
	s := New(Config{Pool: p, Upstream: balanceTestClient(srv)})
	if !s.BeginTask(TaskBalance) {
		t.Fatal("前置：占用 balance gate 失败")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartBalanceRefresh(ctx, time.Hour)

	time.Sleep(150 * time.Millisecond)
	if got := f.calls.Load(); got != 0 {
		t.Fatalf("gate 被占时启动首刷应跳过，calls=%d", got)
	}
	s.EndTask(TaskBalance)
}
