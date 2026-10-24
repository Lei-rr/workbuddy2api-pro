// wait_recover_test.go v2.6.0（Audit-1 P0-4/P1）：scheduler 后台循环收敛与 panic 兜底。
package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/jobs"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
)

// TestWaitBackgroundConverges ctx 取消后周期循环退出，WaitBackground 返回 true。
func TestWaitBackgroundConverges(t *testing.T) {
	s := New(Config{Pool: pool.New("")})
	ctx, cancel := context.WithCancel(context.Background())
	s.StartBalanceRefresh(ctx, time.Hour)
	s.StartTaskSweep(ctx, time.Hour)

	// 给循环一点启动时间（首刷在空池上即时完成）。
	time.Sleep(20 * time.Millisecond)
	cancel()

	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	if !s.WaitBackground(wctx) {
		t.Fatal("ctx 取消后后台循环应在预算内收敛")
	}
	// 幂等：再次等待应立即返回 true。
	wctx2, wcancel2 := context.WithTimeout(context.Background(), time.Second)
	defer wcancel2()
	if !s.WaitBackground(wctx2) {
		t.Fatal("后台循环已收敛时 WaitBackground 应返回 true")
	}
}

// TestWaitBackgroundTimeout 预算先到期返回 false（调用方据此 WARN，不阻塞退出）。
func TestWaitBackgroundTimeout(t *testing.T) {
	s := New(Config{Pool: pool.New("")})
	ctx, cancel := context.WithCancel(context.Background())
	s.StartBalanceRefresh(ctx, time.Hour)
	s.StartTaskSweep(ctx, time.Hour)
	time.Sleep(20 * time.Millisecond)

	// 循环仍在跑（根 ctx 未取消）→ 给一个已过期的等待预算，必须返回 false。
	expired, ecancel := context.WithCancel(context.Background())
	ecancel()
	if s.WaitBackground(expired) {
		t.Fatal("后台循环未退出时 WaitBackground 不应返回 true")
	}

	cancel()
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	if !s.WaitBackground(wctx) {
		t.Fatal("根 ctx 取消后应收敛")
	}
}

// TestSafeHookRecoversPanic hook panic 被兜底，不向上抛。
func TestSafeHookRecoversPanic(t *testing.T) {
	s := New(Config{})
	s.safeHook("test", func() { panic("boom") }) // 不 panic 即通过
}

// TestBatchGoroutineRecoversPanic 任务 goroutine panic 被 goBatch recover，进程不崩。
func TestBatchGoroutineRecoversPanic(t *testing.T) {
	s := New(Config{Pool: pool.New(""), GrowthHook: func() { panic("growth hook panic") }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !s.dispatchTaskKind(ctx, taskGrowth, jobs.TriggerSchedule) {
		t.Fatal("growth 任务应被派发")
	}
	s.batchWG.Wait() // panic 若未被 recover，测试进程会直接崩溃
}

// TestDispatchBatchSurvivesPanicHook 定时批量派发（growth 走 goBatch）同样兜底。
func TestDispatchBatchSurvivesPanicHook(t *testing.T) {
	s := New(Config{Pool: pool.New(""), GrowthHook: func() { panic("boom") }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.dispatchBatch(ctx, []taskKind{taskGrowth})
	s.batchWG.Wait()
}
