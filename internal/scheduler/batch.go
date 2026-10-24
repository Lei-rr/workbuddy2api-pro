package scheduler

import (
	"context"
	"log"
	"sync"

	"github.com/lei-rr/workbuddy2api-pro/internal/jobs"
)

// ---------------------------------------------------------------------------
// 任务单飞 gate（v2.0.1 #10 子项）：按任务族互斥，手动与定时共用
// ---------------------------------------------------------------------------

// BeginTask 尝试占用一类全池任务的单飞 gate：未被占用则占位并返回 true；同类
// 任务正在执行返回 false。调用方**必须**在任务结束后调用 EndTask（defer 最稳）。
// 手动入口（面板 *_all）与定时批次（runBatch/补跑）共用，保证同一时刻最多一轮
// 同类全池任务打上游——双击按钮、或定时与手动同时触发都不会叠加两轮。
func (s *Scheduler) BeginTask(kind TaskKind) bool {
	if !kind.valid() {
		return false
	}
	s.runStateMu.Lock()
	defer s.runStateMu.Unlock()
	if s.running == nil {
		s.running = make(map[TaskKind]bool)
	}
	if s.running[kind] {
		return false
	}
	s.running[kind] = true
	return true
}

// EndTask 释放单飞 gate。未持有也安全（幂等删除），便于 defer 收尾。
func (s *Scheduler) EndTask(kind TaskKind) {
	s.runStateMu.Lock()
	delete(s.running, kind)
	s.runStateMu.Unlock()
}

// TaskRunning 报告某任务族当前是否正在执行（面板重复点击判定 409 用）。
func (s *Scheduler) TaskRunning(kind TaskKind) bool {
	s.runStateMu.Lock()
	defer s.runStateMu.Unlock()
	return s.running[kind]
}

// gateKindOf 把内部排程 kind 映射为导出 TaskKind；夜猫子/成长不在单飞范围
// （无面板批量入口，成长队列另有自身单飞），返回 false 表示不做 gate。
func gateKindOf(k taskKind) (TaskKind, bool) {
	switch k {
	case taskCheckin:
		return TaskCheckin, true
	case taskTravel:
		return TaskTravel, true
	case taskActivity:
		return TaskActivity, true
	case taskKeepalive:
		return TaskKeepalive, true
	case taskBalance:
		return TaskBalance, true
	}
	return "", false
}

// runBatch 并行派发一批任务并**等待全部完成**（同步语义；补跑窗口/测试沿用）。
// 定时主循环已改用 dispatchBatch（不等整批）。
// ctx 取消时由各任务内部的可取消等待快速收尾。
func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("ERROR: scheduler: 任务 goroutine panic: %v", r)
				}
			}()
			s.executeTaskKind(ctx, k)
		}(k)
	}
	wg.Wait()
}

// dispatchBatch 按族独立 goroutine 派发一批任务、**不等待完成**（v2.3.1 #11）：
// 每族先同步抢单飞 gate（保证「同类不叠加」的判定在派发时即确定），再异步执行。
// 派发的 goroutine 登记到 batchWG，Run 退出前统一收敛。
func (s *Scheduler) dispatchBatch(ctx context.Context, kinds []taskKind) {
	for _, k := range kinds {
		if ctx.Err() != nil {
			return
		}
		s.dispatchTaskKind(ctx, k, jobs.TriggerSchedule)
	}
}

// goBatch 启动登记到 batchWG 的任务 goroutine，并 recover panic（v2.6.0 Audit-1）：
// 定时/补跑任务在**后台 goroutine** 里跑，没有 net/http 的 per-request recover 保护，
// 任务体或 hook 的意外 panic 会直接杀进程。这里统一兜底并记 ERROR（ctx 已取消时
// 收尾路径仍会遍历剩余账号，panic 单族不扩散到其他族）。
func (s *Scheduler) goBatch(fn func()) {
	s.batchWG.Add(1)
	go func() {
		defer s.batchWG.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("ERROR: scheduler: 任务 goroutine panic: %v", r)
			}
		}()
		fn()
	}()
}

// dispatchTaskKind 派发单族任务（不等待完成）：先同步占 gate（同类在跑返回 false），
// 再按是否注入 Job Registry 选择执行方式：
//
//   - 注入 Registry（生产）：创建 Job（trigger=schedule/catch_up）异步执行任务体，
//     gate 在 Job 体内持有到结束——Job 出现在 /panel/api/jobs，面板 cancel → Job ctx
//     取消 → 任务体提前停止；Job 失败不阻断后续调度；
//   - 未注入（老测试/嵌入式）：起 goroutine 直接执行任务体，行为与 v2.3.0 一致。
//
// 无 gate 的族（夜猫子/成长）不建 Job（Job Kind 枚举只覆盖五类批量任务），直接异步。
func (s *Scheduler) dispatchTaskKind(ctx context.Context, k taskKind, trigger jobs.Trigger) bool {
	if ctx.Err() != nil {
		return false
	}
	gk, gated := gateKindOf(k)
	if gated && !s.BeginTask(gk) {
		log.Printf("scheduler: %s 任务正在执行中，本轮跳过（同类单飞）", gk)
		return false
	}
	if !gated {
		// 夜猫子/成长：没有 Job Kind/批量 gate，直接异步执行。
		s.goBatch(func() { s.runTaskBody(ctx, k, nil) })
		return true
	}
	if reg := s.jobsRegistry(); reg != nil {
		total := len(s.cfg.Pool.List())
		job, err := reg.Run(jobs.Kind(gk), trigger, total,
			func(jobCtx context.Context, prog jobs.Progress) error {
				defer s.EndTask(gk)
				s.runTaskBody(jobCtx, k, ProgressFunc(prog))
				return nil // 账号级失败只进 Job.Fail 计数，不把整轮标 failed
			})
		if err != nil {
			// 理论上 gate 已保证同类单飞；Registry 内部异常/关闭时不阻塞调度。
			s.EndTask(gk)
			log.Printf("scheduler: %s 批次 Job 创建失败，回退直接执行: %v", gk, err)
			s.goBatch(func() { s.runTaskBody(ctx, k, nil) })
			return false
		}
		log.Printf("scheduler: %s 定时批次已启动 job=%s trigger=%s", gk, job.ID, trigger)
		return true
	}
	s.goBatch(func() {
		defer s.EndTask(gk)
		s.runTaskBody(ctx, k, nil)
	})
	return true
}

// runTaskBody 执行任务族主体（不含 gate）：统一从 executeTaskKind / dispatchTaskKind
// 调用，保证手动、定时、补跑、Job 化四条路径的任务体完全一致。
// progress 仅在 Job 化路径非 nil；growth 返回 false 表示回调未挂载。
func (s *Scheduler) runTaskBody(ctx context.Context, k taskKind, progress ProgressFunc) bool {
	switch k {
	case taskCheckin:
		s.runCheckin(ctx, progress)
	case taskTravel:
		s.runTravel(ctx, progress)
	case taskActivity:
		s.runActivity(ctx, progress)
	case taskKeepalive:
		s.runKeepalive(ctx, progress)
	case taskBlackcat:
		s.RunBlackcatNow()
	case taskGrowth:
		// 成长任务队列：回调在 panel 侧异步启动（返回不等执行完），nil 未挂载则跳过。
		s.schedMu.Lock()
		hook := s.cfg.GrowthHook
		s.schedMu.Unlock()
		if hook == nil {
			return false
		}
		s.safeHook("growth", hook)
		// 队列已启动（内部单飞）：标今天已跑，补跑窗口不再重复触发。
		s.noteTaskDone(taskGrowth, s.now())
	case taskBalance:
		_ = s.RunBalanceRefreshCtx(ctx, progress)
	}
	return true
}

// executeTaskKind 在单飞 gate 保护下**同步**执行一类任务（补跑/测试沿用）；
// 同类已在跑返回 false（本轮跳过）。定时主循环改走 dispatchTaskKind（异步/Job 化）。
func (s *Scheduler) executeTaskKind(ctx context.Context, k taskKind) bool {
	if gk, gated := gateKindOf(k); gated {
		if !s.BeginTask(gk) {
			log.Printf("scheduler: %s 任务正在执行中，本轮跳过（同类单飞）", gk)
			return false
		}
		defer s.EndTask(gk)
	}
	return s.runTaskBody(ctx, k, nil)
}
