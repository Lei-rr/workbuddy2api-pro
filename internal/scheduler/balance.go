package scheduler

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/logfmt"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
)

// RunBalanceRefreshNow 并发查询余额并更新池内 credits。
// 账号范围：默认跳过禁用账号；schedule.include_disabled_in_tasks 打开时一并刷新
// （轮换用法下据此判断下一个该启用谁）。注意 ReenableIfCredits 对 disabled 是
// no-op，所以本开关**不会**导致禁用号被自动解冻——它只让已有账号的积分保持新鲜。
// 解冻语义与签到一致（ReenableIfCredits：余额 > 0 的冷却账号自动解冻），
// 但不做签到、不刷新 token——只让"积分"这个观测量保持新鲜。
//
// v2.0.1 #10：并发**有界**（信号量 = schedule.balance_refresh_concurrency，默认
// 8、上限 32），绝不再对 1146 个账号各起一个 goroutine（无上限并发是上游风控
// 风暴最危险的路径）。后台周期刷新/启动首刷/面板手动刷新共用本函数与同一把
// 单飞 gate（见 refreshBalanceGated），不会叠加两轮全池余额查询。
func (s *Scheduler) RunBalanceRefreshNow() {
	_ = s.RunBalanceRefreshCtx(context.Background(), nil)
}

// RunBalanceRefreshCtx 是 RunBalanceRefreshNow 的 ctx + 进度变体（v2.3.0 #10
// Job Runner 用）：ctx 取消后不再派发新账号（在途并发请求自然结束）；progress
// 按账号完成顺序上报累计计数（回调需并发安全，Job Runner 内部加锁）。
// 进度口径：done=已处理账号数，fail=余额查询失败数，skip=禁用/无凭证跳过数。
func (s *Scheduler) RunBalanceRefreshCtx(ctx context.Context, progress ProgressFunc) error {
	conc := s.BalanceRefreshConcurrency()
	// sem 即并发闸门：满载时遍历端阻塞，因此同时在途的 goroutine 数恒 <= conc，
	// 与账号总数无关（账号 1 万也只会有 conc 个 goroutine 存活于本函数）。
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	var done, fail, skip atomic.Int64
	expiringSoon := s.ExpiringSoonWindow()
	includeDisabled := s.includeDisabledInTasks()
	stList := s.cfg.Pool.List()
	total := len(stList)
	report := func(msg string) {
		reportProgress(progress, int(done.Load()), total, int(fail.Load()), int(skip.Load()), msg)
	}
loop:
	for _, st := range stList {
		if ctx.Err() != nil {
			break
		}
		if st.Disabled && !includeDisabled {
			skip.Add(1)
			report("")
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			skip.Add(1)
			report("")
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop // 停机/取消：不再派发新账号，等在途收尾
		}
		wg.Add(1)
		go func(a *auth.Auth, uid string) {
			defer wg.Done()
			defer func() { <-sem }()
			remain, creditsTotal, expiring, earliestAt, earliestRemaining, buckets, err := s.cfg.Upstream.UserResourceSnapshot(a, expiringSoon)
			if err != nil {
				log.Printf("balance %s: %v", logfmt.Label(uid, a.Nickname), err)
				fail.Add(1)
				done.Add(1)
				report(fmt.Sprintf("balance %s: %v", logfmt.Label(uid, a.Nickname), err))
				return
			}
			s.cfg.Pool.ReenableIfCredits(uid, remain, creditsTotal)
			s.cfg.Pool.SetCreditsSnapshot(uid, pool.CreditsSnapshot{
				Remain:            remain,
				Total:             creditsTotal,
				Expiring:          expiring,
				EarliestAt:        earliestAt,
				EarliestRemaining: earliestRemaining,
				ExpiryBuckets:     poolExpiryBuckets(buckets),
			})
			done.Add(1)
			report("")
		}(a, st.UID)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil // 取消由 Job Runner 统一按 canceled 落状态；账号级失败只进 fail 计数
	}
	s.lastBalanceRefresh.Store(time.Now().UnixNano())
	s.noteTaskDone(taskBalance, s.now())
	return nil
}

// refreshBalanceGated 在单飞 gate 保护下跑一轮全量余额刷新（后台周期/启动首刷
// 用）：同类（面板手动 balance_all）正在执行时返回 false 并跳过本轮，绝不叠加
// 两轮全池余额查询打上游。
func (s *Scheduler) refreshBalanceGated() bool {
	if !s.BeginTask(TaskBalance) {
		return false
	}
	defer s.EndTask(TaskBalance)
	s.RunBalanceRefreshNow()
	return true
}

// balanceStartupSkip 启动首刷是否因账号数过大而跳过（纯函数便于单测）：
// 未显式 force 且账号数 > 阈值（200）→ 跳过，交给周期刷新。
func balanceStartupSkip(accounts int, force bool) bool {
	return !force && accounts > balanceStartupSkipThreshold
}

// StartBalanceRefresh 后台周期性余额刷新（独立 ticker goroutine，ctx 取消即停）。
// interval<=0 不启动（schedule.balance_refresh_enabled=false 时 main 不调用即可）。
// 独立于 Run 的小时制排程：余额是分钟级观测量，不值得为它扩展 nextFire 的粒度。
// 运行期可用 SetBalanceInterval 热改间隔（下一轮生效）。
func (s *Scheduler) StartBalanceRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.balanceInterval.Store(int64(interval))
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("ERROR: scheduler: 余额后台刷新循环 panic: %v", r)
			}
		}()
		// 启动首刷保护（v2.0.1 #10）：不再「启动即全池无上限刷」。
		// 1) 先按 schedule.balance_refresh_startup_delay 延迟（0=不延迟），让监听/
		//    预热等启动动作先稳定，避免重启瞬间与其它启动流量叠加；
		// 2) 账号数 > 200 且未显式 schedule.balance_refresh_startup_force=true →
		//    跳过首刷（打日志说明恢复方式），把首次全量刷新交给周期 ticker；
		// 3) 执行时同样走单飞 gate（与面板手动刷新互斥）。
		// ctx 在延迟期间取消 → 直接退出（不抢跑首刷）。
		if !s.balanceFirstRefresh(ctx) {
			return
		}
		var logged time.Duration
		for {
			cur := time.Duration(s.balanceInterval.Load())
			if cur != logged {
				log.Printf("scheduler: 余额后台刷新每 %s（暂停中显示 0s）", cur)
				logged = cur
			}
			if cur <= 0 {
				// 被热改暂停：等重排通知（重新启用时唤醒）或退出。
				select {
				case <-ctx.Done():
					return
				case <-s.rearmBalance:
					continue
				}
			}
			timer := time.NewTimer(cur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmBalance:
				timer.Stop() // 间隔已变：立刻按新值重算
			case <-timer.C:
				if !s.refreshBalanceGated() {
					log.Printf("scheduler: 余额周期刷新跳过（全量余额刷新正在执行中）")
				}
			}
		}
	}()
}

// balanceFirstRefresh 启动首刷（延迟 + 大池跳过 + 单飞 gate）。返回 false 表示
// ctx 在延迟期间被取消、goroutine 应直接退出；其余情况返回 true（首刷可能被
// 跳过，但周期循环照常）。
func (s *Scheduler) balanceFirstRefresh(ctx context.Context) bool {
	s.schedMu.Lock()
	delay := s.cfg.BalanceRefreshStartupDelay
	force := s.cfg.BalanceRefreshStartupForce
	poolRef := s.cfg.Pool
	s.schedMu.Unlock()

	if delay > 0 {
		log.Printf("scheduler: 余额启动首刷延迟 %s（schedule.balance_refresh_startup_delay）", delay)
		if !sleepCtx(ctx, delay) {
			return false
		}
	}
	accounts := 0
	if poolRef != nil {
		accounts = len(poolRef.List())
	}
	if balanceStartupSkip(accounts, force) {
		log.Printf("scheduler: 账号数 %d > %d，跳过启动首刷（schedule.balance_refresh_startup_force=false 时大池默认跳过，避免重启瞬间打爆上游；周期刷新照常）",
			accounts, balanceStartupSkipThreshold)
		return true
	}
	if !s.refreshBalanceGated() {
		log.Printf("scheduler: 启动首刷跳过（全量余额刷新正在执行中）")
	}
	return true
}
