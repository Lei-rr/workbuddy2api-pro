package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/logfmt"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用账号默认跳过——
// 若 schedule.include_disabled_in_tasks 打开则一并执行（禁用只关选号，不停保号）。
// 旅行已从签到剥离为独立排程（travel_hours），不再搭签到便车。
// 末尾追加连登管家（streak.go）：可兑换档位自动兑换 + 抽奖次数自动抽完——
// 连登兑换按天数解锁，挂在每日签到后即「到天数那天自动完成兑换→抽奖闭环」。
// 本函数是无 ctx 的外部入口（面板/测试一次性触发）；排程主循环走 runCheckin。
func (s *Scheduler) RunCheckinNow() {
	_ = s.RunCheckinCtx(context.Background(), nil)
}

// RunCheckinCtx 是 RunCheckinNow 的 ctx + 进度变体（v2.3.0 #10 Job Runner 用）：
// ctx 取消时账号循环尽早返回（剩余账号不再发请求、不计数）；progress 逐号上报
// 累计计数（nil = 不关心）。账号级失败只计入 fail，不把「部分失败」升级成整轮
// error（返回恒 nil，保持「任务跑完即成功」的既有语义）。
func (s *Scheduler) RunCheckinCtx(ctx context.Context, progress ProgressFunc) error {
	s.runCheckin(ctx, progress)
	return nil
}

// checkinRetryBackoff 签到/余额查询瞬态错误重试前的短退避（v2.3.1 #11：1-2s；
// 测试可置 0，与 travelAccountDelay 同款「测试可缩短」口径）。
var checkinRetryBackoff = 1500 * time.Millisecond

// isTransientErr 报告 scheduler 层是否应对该错误再做 1 次重试（v2.3.1 #11）。
//
// 分层重试设计（避免重复放大——#11 的痛点之一就是「重试风暴」）：
//   - 网络错误 / 超时 / 5xx：upstream.retryBillingTransient 已对签到/余额这类计费
//     维护调用做过 1+2 次有界重试（2s/4s 退避），scheduler 不再叠加；
//   - 429 软限流（ErrSoftRate）：上游计费路径**不重试**（isTransientBillingErr 只认
//     ErrServer/网络错误），由 scheduler 层补 1 次短退避重试；
//   - 其余 4xx 鉴权/凭证/业务/内容拦截/余额不足：确定性失败，不重试；
//   - ctx 已取消：不重试（停机/取消立即收尾）。
func isTransientErr(ctx context.Context, err error) bool {
	if err == nil || (ctx != nil && ctx.Err() != nil) {
		return false
	}
	var ue *upstream.Error
	if errors.As(err, &ue) {
		return ue.Kind == upstream.ErrSoftRate
	}
	return false
}

// callWithRetry 对 scheduler 层负责的瞬态错误（当前：429 限流）重试 1 次，
// 短退避、ctx 取消立即放弃并返回原始错误；网络/5xx 的重试由 upstream 客户端负责。
func (s *Scheduler) callWithRetry(ctx context.Context, op func() error) error {
	err := op()
	if err == nil || !isTransientErr(ctx, err) {
		return err
	}
	if !sleepCtx(ctx, checkinRetryBackoff) {
		return err
	}
	return op()
}

// runCheckin 签到遍历（v2.3.1 #11：账号级 worker pool，默认并发 3）：
//   - ctx 取消后不再派发新账号、等在途返回，且不记「今天已跑」；
//   - 当日已签（本进程内成功/幂等标记过）跳过 checkin RPC，**仍刷新余额/解冻**
//     （21:00 轮次仍需要最新积分；上游 already-checked 幂等也不重复打）；
//   - 仅瞬态错误（网络/超时/5xx/429）重试 1 次（短退避）；4xx 鉴权/业务直接 fail；
//   - 进度口径：done=已处理账号数（不含 skip、含 fail），fail=其中签到/余额失败数，
//     skip=禁用/无凭证/域门控/当日已签跳过数。
//
// 末尾连登管家（RunStreakBonusNow）在整轮 worker 收敛后执行；ctx 取消则不执行。
func (s *Scheduler) runCheckin(ctx context.Context, progress ProgressFunc) {
	expiringSoon := s.ExpiringSoonWindow()
	includeDisabled := s.includeDisabledInTasks()
	stList := s.cfg.Pool.List()
	total := len(stList)
	var done, fail, skip atomic.Int64
	report := func(msg string) {
		reportProgress(progress, int(done.Load()), total, int(fail.Load()), int(skip.Load()), msg)
	}
	if total > 0 {
		conc := s.CheckinConcurrency()
		if conc > total {
			conc = total
		}
		idxCh := make(chan int)
		var wg sync.WaitGroup
		for w := 0; w < conc; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range idxCh {
					if ctx.Err() != nil {
						continue // 取消：快速排空剩余索引，不再发上游请求
					}
					skipped, failed, msg := s.checkinAccount(ctx, stList[i], expiringSoon, includeDisabled)
					if skipped {
						skip.Add(1)
					}
					if failed {
						fail.Add(1)
					}
					if !skipped {
						done.Add(1)
					}
					report(msg)
				}
			}()
		}
	dispatch:
		for i := range stList {
			select {
			case idxCh <- i:
			case <-ctx.Done():
				break dispatch
			}
		}
		close(idxCh)
		wg.Wait()
	}
	if ctx.Err() != nil {
		return
	}
	s.RunStreakBonusNow()
	s.noteTaskDone(taskCheckin, s.now())
}

// checkinAccount 单账号签到 + 余额刷新（v2.3.1 #11 worker pool 调用；Pool/Upstream
// 本身线程安全，worker 之间无共享可变状态）。返回 skipped=跳过签到（禁用/无凭证/
// 域门控/当日已签）、failed=签到或余额查询失败、msg=失败摘要（写入 Job.LogTail）。
func (s *Scheduler) checkinAccount(ctx context.Context, st pool.Status, expiringSoon time.Duration, includeDisabled bool) (skipped, failed bool, msg string) {
	if st.Disabled && !includeDisabled {
		return true, false, ""
	}
	a := s.cfg.Pool.AuthByUID(st.UID)
	if a == nil || a.RefreshTokenValue() == "" {
		return true, false, ""
	}
	// D4 门控：realm=global 账号无签到体系，直接跳过（不发起任何上游调用，避免风控）。
	// 经 auth.Realm() 统一判定：逃生门（global.enabled=false）下 global 账号被降级为 cn、
	// 按 CN 处理——这是逃生门的刻意语义（纯 CN 部署锁死一切 global），与引用处一致。
	if a.IsGlobal() {
		return true, false, ""
	}
	// 当日已签（进程内标记）：跳过 checkin RPC，但**仍刷新余额**——21:00 轮次
	// 仍需要最新积分/解冻；企业版本就不调签到 RPC（不算「已签跳过」）。
	alreadySigned := !a.IsEnterprise() && s.checkinDoneToday(st.UID)
	// 企业版无签到体系：上游对 POST /v2/billing/meter/daily-checkin 直接
	// 400 code 10001「企业账号不支持该操作」。跳过签到，但余额查询照常跑
	//（企业额度走 get-enterprise-user-usage 口径，是 credit_floor 判定与面板额度
	// 展示的唯一数据源）。
	if !a.IsEnterprise() && !alreadySigned {
		err := s.callWithRetry(ctx, func() error {
			return s.cfg.Upstream.DailyCheckin(a)
		})
		switch {
		case err == nil:
			// 首次签到成功此前静默——排查「签到到底跑没跑」时无迹可循（幂等行只在
			// 重复触发时出现），成功也落一行。
			s.cfg.Pool.NoteCheckinDone(st.UID)
			s.markCheckinDone(st.UID)
			log.Printf("checkin %s: 签到成功", logfmt.Label(st.UID, st.Nickname))
		case upstream.IsAlreadyCheckin(err):
			// "今天已签到"是幂等成功（上游对重复签到返回 code!=0），不再当失败打 error 行。
			s.cfg.Pool.NoteCheckinDone(st.UID)
			s.markCheckinDone(st.UID)
			log.Printf("checkin %s: 今天已签到（幂等）", logfmt.Label(st.UID, st.Nickname))
		default:
			log.Printf("checkin %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			failed = true
			msg = fmt.Sprintf("checkin %s: %v", logfmt.Label(st.UID, st.Nickname), err)
		}
	}
	// 分桶查余额：配置窗口内的积分单独标记，同时记录最早未来到期批次与按自然日
	// 聚合的全部批次（首页快照与「检查」共用同一口径）；瞬态错误重试 1 次。
	var (
		remain, creditsTotal, expiring, earliestRemaining int64
		earliestAt                                        time.Time
		buckets                                           []upstream.ExpiryBucket
	)
	err := s.callWithRetry(ctx, func() error {
		var e error
		remain, creditsTotal, expiring, earliestAt, earliestRemaining, buckets, e =
			s.cfg.Upstream.UserResourceSnapshot(a, expiringSoon)
		return e
	})
	if err != nil {
		log.Printf("user-resource %s: %v", logfmt.Label(st.UID, st.Nickname), err)
		failed = true
		msg = fmt.Sprintf("user-resource %s: %v", logfmt.Label(st.UID, st.Nickname), err)
	} else {
		s.cfg.Pool.ReenableIfCredits(st.UID, remain, creditsTotal)
		s.cfg.Pool.SetCreditsSnapshot(st.UID, pool.CreditsSnapshot{
			Remain:            remain,
			Total:             creditsTotal,
			Expiring:          expiring,
			EarliestAt:        earliestAt,
			EarliestRemaining: earliestRemaining,
			ExpiryBuckets:     poolExpiryBuckets(buckets),
		})
	}
	if alreadySigned {
		skipped = true
	}
	return skipped, failed, msg
}

// RunActivityNow 立即对池内所有可用账号执行一次对话活跃上报。
// 禁用账号默认跳过（schedule.include_disabled_in_tasks 打开时一并上报）；
// 无 AccessToken 的跳过；账号间限速 activityAccountDelay。
// CN 与 global 账号**都上报**（PR #45 实测国际版 /v2/report 在 workbuddy.ai 上
// code=0 OK，点亮连登）；一条上报同时点亮 growth 连登 + 解锁 first_buddy 任务。
// 上报成功后续跑 streak 自检（checkActivityStreak）：回读连登天数，发现
// 「上报 200 但 streak 没涨」的静默丢弃（只读 oracle，不做重试）。
// RunActivityNow 是无 ctx 的外部入口（面板/测试一次性触发）；排程主循环走
// runActivity（ctx 取消时立即放弃剩余账号，不等限速睡满）。
func (s *Scheduler) RunActivityNow() {
	_ = s.RunActivityCtx(context.Background(), nil)
}

// RunActivityCtx 是 RunActivityNow 的 ctx + 进度变体（v2.3.0 #10 Job Runner 用）。
func (s *Scheduler) RunActivityCtx(ctx context.Context, progress ProgressFunc) error {
	s.runActivity(ctx, progress)
	return nil
}

// runActivity 活跃上报遍历，随 ctx 取消立即退出。
// 进度口径：done=已处理账号数，fail=上报失败数，skip=禁用/无 token/企业门控跳过数。
func (s *Scheduler) runActivity(ctx context.Context, progress ProgressFunc) {
	first := true
	includeDisabled := s.includeDisabledInTasks()
	stList := s.cfg.Pool.List()
	total := len(stList)
	done, fail, skip := 0, 0, 0
	for _, st := range stList {
		if ctx.Err() != nil {
			return
		}
		if st.Disabled && !includeDisabled {
			skip++
			reportProgress(progress, done, total, fail, skip, "")
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			skip++
			reportProgress(progress, done, total, fail, skip, "")
			continue
		}
		// 企业版跳过活跃上报：/v2/report 本身返回 200（不报错），但它唯一的作用是
		// 点亮 growth 连登天数 / 解锁 first_buddy 领养——这两者对企业号都是 403
		// （「growth system is only available for personal users」），即**零收益**。
		// 该类"静默无效"最易被忽略（日志无异常），故显式跳过，不给上游多发一次请求。
		// 企业切换会话（token_source=enterprise_switch）同跳：report 返回 200 却被
		// 上游静默丢弃（连登不涨），同样是零收益（见 auth.IsEnterpriseSession）。
		if a.IsEnterpriseSession() {
			skip++
			reportProgress(progress, done, total, fail, skip, "")
			continue
		}
		// global 账号同样上报（PR #45 实测国际版 /v2/report 在 workbuddy.ai 上 code=0 OK，
		// 点亮连登）；realmBase 路由/头由 upstream.billingJSON/BillingHeaders 按 realm 切，
		// 无需改动 upstream。此处曾按「D4 门控：global 无活跃体系」跳过 global，实测该
		// 判断不成立——国际版 /v2/report 可用，跳过即国际版账号永远点不亮连登（上游
		// a190252 同口径修复）。checkin/travel 的 global 门控不受影响，仍跳过。
		if !first {
			if !sleepCtx(ctx, activityAccountDelay) {
				return // 优雅停机：不等限速睡满，剩余账号下轮再报
			}
		}
		first = false
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		if err := s.cfg.Upstream.ReportChatActivity(a, cid, ""); err != nil {
			log.Printf("activity %s: %v", logfmt.Label(a.UID, a.Nickname), err)
			fail++
			done++
			reportProgress(progress, done, total, fail, skip,
				fmt.Sprintf("activity %s: %v", logfmt.Label(a.UID, a.Nickname), err))
			continue
		}
		s.checkActivityStreak(a) // 上报成功 → 回读 streak 自检
		done++
		reportProgress(progress, done, total, fail, skip, "")
	}
	if ctx.Err() == nil {
		s.noteTaskDone(taskActivity, s.now())
	}
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("activity %s: streak check failed (report OK): %v", logfmt.Label(a.UID, a.Nickname), err)
		return true
	}
	if days == 0 {
		log.Printf("activity %s: report OK but streak.days=0 (silent drop?)", logfmt.Label(a.UID, a.Nickname))
		return true
	}
	log.Printf("activity %s: streak days=%d", logfmt.Label(a.UID, a.Nickname), days)
	return false
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
// 禁用账号默认跳过；若 schedule.include_disabled_in_tasks 打开，禁用号也会续期 token
// （这是轮换用法下把闲置号保持可用的关键），但**不再对已禁用的号重复计数 12153**——
// 它已经是终态，再计一次只会打出一行「— 禁用」的误导日志。
// 12153 禁用走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不再立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
func (s *Scheduler) RunKeepaliveNow() {
	_ = s.RunKeepaliveCtx(context.Background(), nil)
}

// RunKeepaliveCtx 是 RunKeepaliveNow 的 ctx + 进度变体（v2.3.0 #10 Job Runner 用）。
func (s *Scheduler) RunKeepaliveCtx(ctx context.Context, progress ProgressFunc) error {
	s.runKeepalive(ctx, progress)
	return nil
}

// runKeepalive token 保活遍历（ctx 感知 + 进度）。
// 进度口径：done=已处理账号数，fail=刷新失败数，skip=禁用/无 refresh_token 跳过数。
func (s *Scheduler) runKeepalive(ctx context.Context, progress ProgressFunc) {
	includeDisabled := s.includeDisabledInTasks()
	stList := s.cfg.Pool.List()
	total := len(stList)
	done, fail, skip := 0, 0, 0
	for _, st := range stList {
		if ctx.Err() != nil {
			return
		}
		if st.Disabled && !includeDisabled {
			skip++
			reportProgress(progress, done, total, fail, skip, "")
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			skip++
			reportProgress(progress, done, total, fail, skip, "")
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead && !st.Disabled {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("keepalive %s: 连续 %d 次 12153 session dead — 禁用", logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
				}
			}
			fail++
			done++
			reportProgress(progress, done, total, fail, skip,
				fmt.Sprintf("keepalive %s: %v", logfmt.Label(st.UID, st.Nickname), err))
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // 刷新成功清误判计数，失败不该累计
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
		}
		done++
		reportProgress(progress, done, total, fail, skip, "")
	}
	if ctx.Err() == nil {
		s.noteTaskDone(taskKeepalive, s.now())
	}
}

// poolExpiryBuckets 把 upstream 的按日到期批次转换为 pool 结构。两个包字段同构，
// 手工转换是为了不让底层 pool 反向依赖 upstream。
func poolExpiryBuckets(in []upstream.ExpiryBucket) []pool.ExpiryBucket {
	if len(in) == 0 {
		return nil
	}
	out := make([]pool.ExpiryBucket, len(in))
	for i, b := range in {
		out[i] = pool.ExpiryBucket{Date: b.Date, ExpireAt: b.ExpireAt, Remain: b.Remain}
	}
	return out
}
