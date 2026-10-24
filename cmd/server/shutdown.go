// shutdown.go 优雅停机编排（v2.2 #14）。
//
// 设计目标：把「收到信号之后做什么」从 main 的装配代码里抽成可注入依赖的纯编排，
// 这样可以用 httptest 真实 server 做确定性单测（在途请求完成、新请求拒绝、
// 落盘发生在 HTTP 停机之后），而不必起子进程发信号。
//
// 停机顺序（与 UPGRADE_PROPOSALS #14 建议一致）：
//  1. cancel 调度器/后台任务 ctx —— 不再发起新的上游请求/新任务；
//  2. srv.Shutdown(ctx) —— 关监听（拒绝新请求）+ 等在途 HTTP（含 SSE 长流）
//     自然结束，上限 server.shutdown_timeout（默认 30s）；
//  3. 等待后台任务 goroutine 收敛（同一时间预算；超时只 WARN，不阻塞退出）；
//     3b. 等待 scheduler 包内独立周期循环（余额刷新/任务清扫）退出（v2.6.0 P0-4）；
//  4. 最终落盘（pool.Flush）—— **必须在 HTTP 停机之后**：在途请求成功计数还在
//     写池状态，先 flush 后停机会让最后一次变更丢在「已 flush 但 flusher 已停」
//     的缝隙里（旧实现 5s 硬编码 + flush 在 Shutdown 之前的缺陷）。
//
// 退出码/二次信号由 main 负责：本函数只做编排并返回结果供日志/测试断言。
package main

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// defaultShutdownTimeout 停机默认上限（与 config 默认 "30s" 一致；依赖注入测试可覆盖）。
const defaultShutdownTimeout = 30 * time.Second

// httpShutter 抽象「停止接收新请求并等待在途请求结束」的能力。
// *http.Server 满足（Shutdown）；测试用 fake 注入确定性行为。
type httpShutter interface {
	Shutdown(ctx context.Context) error
}

// shutdownDeps 停机编排依赖（main 注入真实实现；测试注入 fake/httptest）。
type shutdownDeps struct {
	// Shutter 停止接收新请求并等待在途请求（*http.Server）。nil = 跳过该阶段。
	Shutter httpShutter
	// Cancel 取消调度器/余额刷新/预热等后台 ctx，让它们停止取新任务。nil = 跳过。
	Cancel context.CancelFunc
	// Background 后台任务 goroutine 的 WaitGroup（scheduler.Run / warmModelRates）。
	// nil = 无需等待。
	Background *sync.WaitGroup
	// WaitExtra 可选：等待不在 Background 里的其他后台循环收敛（v2.6.0 Audit-1 P0-4：
	// scheduler 的余额刷新 / 任务补领清扫等周期 goroutine）。在 Flush **之前**调用，
	// 保证「停止新任务 → 等所有会写池的后台 goroutine 退出 → Flush」的顺序。
	// 返回 true = 预算内收敛；false = 预算到期（只 WARN，不阻塞退出）。
	WaitExtra func(ctx context.Context) bool
	// Flush 最终落盘（pool.Flush）。nil = 跳过。
	Flush func()
	// Timeout 整体停机时间预算（<=0 回落默认 30s）。
	Timeout time.Duration
	// Logf 日志出口（nil = log.Printf）。
	Logf func(format string, args ...any)
}

// shutdownResult 停机结果（供 main 汇总日志与测试断言）。
type shutdownResult struct {
	// TimedOut 在途 HTTP 请求未在时间预算内结束（Shutdown 返回 deadline exceeded）。
	TimedOut bool
	// BackgroundDone 后台任务 goroutine 在时间预算内收敛。
	BackgroundDone bool
	// Elapsed 停机编排耗时。
	Elapsed time.Duration
}

// gracefulShutdown 按固定顺序执行停机编排并返回结果（幂等性由调用方保证：
// main 只调用一次；二次信号走 os.Exit 强退路径）。
func gracefulShutdown(d shutdownDeps) shutdownResult {
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}
	logf := d.Logf
	if logf == nil {
		logf = log.Printf
	}
	start := time.Now()
	res := shutdownResult{}

	// 1) 取消后台 ctx：调度器不再取新账号/新任务，余额刷新与模型预热放弃剩余工作。
	if d.Cancel != nil {
		logf("shutdown: 取消调度器/后台任务 ctx（不再发起新的上游请求）")
		d.Cancel()
	}

	// 2) 停止接收新请求并等在途请求结束。预算覆盖「HTTP 在途 + 后台收敛」两段。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if d.Shutter != nil {
		logf("shutdown: 停止接收新请求，等待在途请求结束（上限 %s）", timeout)
		if err := d.Shutter.Shutdown(shutdownCtx); err != nil {
			// context.DeadlineExceeded 是「长请求没等到」的预期超时，不是编排错误。
			res.TimedOut = errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
			if res.TimedOut {
				logf("WARN: shutdown: 在途请求未在时限内结束: %v（继续收尾，不阻塞退出）", err)
			} else {
				logf("WARN: shutdown: 停止接收请求时出错: %v（继续收尾）", err)
			}
		}
	}

	// 3) 等待后台任务 goroutine 收敛（同一预算；超时只 WARN —— 进程退出是最终手段）。
	if d.Background != nil {
		done := make(chan struct{})
		go func() {
			d.Background.Wait()
			close(done)
		}()
		select {
		case <-done:
			res.BackgroundDone = true
			logf("shutdown: 后台任务已全部收敛")
		case <-shutdownCtx.Done():
			logf("WARN: shutdown: 后台任务未在时限内收敛（%v），继续收尾", shutdownCtx.Err())
		}
	}

	// 3b) 等待 scheduler 等包内的独立周期循环（v2.6.0 Audit-1 P0-4）：它们不在
	// Background 里，但余额刷新/任务清扫会写池（credits/冷却/到期快照）。必须赶在
	// Flush 之前确认它们已退出，否则会出现「Flush 之后仍有 goroutine 写池」的竞态。
	if d.WaitExtra != nil {
		if d.WaitExtra(shutdownCtx) {
			logf("shutdown: 调度器后台循环已收敛")
		} else {
			logf("WARN: shutdown: 调度器后台循环未在时限内收敛（%v），继续收尾", shutdownCtx.Err())
		}
	}

	// 4) 最终落盘：必须在 HTTP/后台停机之后（避免 flush 后又被在途请求写脏）。
	if d.Flush != nil {
		logf("shutdown: 最终落盘 pool state")
		d.Flush()
	}
	res.Elapsed = time.Since(start)
	return res
}
