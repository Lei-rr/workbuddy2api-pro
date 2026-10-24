package scheduler

import (
	"log"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/jobs"
)

// ExpiringSoonWindow 返回当前快过期路由窗口（读取时与热配置写在 schedMu 下同步）。
func (s *Scheduler) ExpiringSoonWindow() time.Duration {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.ExpiringSoonWindow
}

// SetExpiringSoonWindow 热更新快过期路由窗口。窗口变化时清空池内旧快照，避免在下一轮
// 余额刷新覆盖前，继续用旧窗口得出的最早到期顺序选号。
func (s *Scheduler) SetExpiringSoonWindow(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.schedMu.Lock()
	changed := s.cfg.ExpiringSoonWindow != d
	s.cfg.ExpiringSoonWindow = d
	poolRef := s.cfg.Pool
	s.schedMu.Unlock()
	if changed && poolRef != nil {
		poolRef.ClearExpiringSnapshots()
	}
}

// Reconfigure 热更新排程参数（面板保存配置后调用）：改时点/开关并通知运行中的循环重算。
// 空 hours 视为「未配置」保留原值（与 config.normalize 的回落语义一致）。
// SetGrowthHook 挂载/替换成长任务队列回调（panel 构造晚于 scheduler，事后接线）。
func (s *Scheduler) SetGrowthHook(fn func()) {
	s.schedMu.Lock()
	s.cfg.GrowthHook = fn
	s.schedMu.Unlock()
}

// includeDisabledInTasks 返回保号类任务是否应覆盖禁用账号（配置快照，供循环开头取一次）。
func (s *Scheduler) includeDisabledInTasks() bool {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.IncludeDisabledInTasks
}

// SetIncludeDisabledInTasks 热更新「保号类任务是否覆盖禁用账号」
// （schedule.include_disabled_in_tasks）。
// 用独立 setter 而非并入 Reconfigure：后者已有 12 个位置参数，继续追加会让调用点难以
// 校对；本开关语义独立（只影响账号过滤，不影响时点），单独设值更清晰。
func (s *Scheduler) SetIncludeDisabledInTasks(v bool) {
	s.schedMu.Lock()
	s.cfg.IncludeDisabledInTasks = v
	s.schedMu.Unlock()
}

// SetBalanceRefreshConcurrency 热更新余额全量刷新并发上限：<=0 回落默认 8，上限 32。
// 只影响下一轮刷新（已在途的请求按旧闸门跑完）；面板保存后立即生效。
func (s *Scheduler) SetBalanceRefreshConcurrency(n int) {
	s.balanceConcurrency.Store(int64(normalizeBalanceRefreshConcurrency(n)))
}

// BalanceRefreshConcurrency 当前余额全量刷新并发上限（供日志/观测）。
func (s *Scheduler) BalanceRefreshConcurrency() int {
	if v := s.balanceConcurrency.Load(); v > 0 {
		return int(v)
	}
	return defaultBalanceRefreshConcurrency
}

// SetCheckinConcurrency 热更新签到账号级并发度（v2.3.1 #11）：<=0 回落默认 3，上限 16。
// 只影响下一轮签到（在途 worker 按旧并发跑完）；面板保存配置后立即生效。
func (s *Scheduler) SetCheckinConcurrency(n int) {
	s.checkinConcurrency.Store(int64(normalizeCheckinConcurrency(n)))
}

// CheckinConcurrency 当前签到账号级并发度（供日志/观测）。
func (s *Scheduler) CheckinConcurrency() int {
	if v := s.checkinConcurrency.Load(); v > 0 {
		return int(v)
	}
	return defaultCheckinConcurrency
}

// SetJobsRegistry 运行期挂载/替换 Job Registry（v2.3.1 #11）：非 nil 后定时/
// 补跑批次会创建 Job（trigger=schedule/catch_up）。传 nil 可关闭 Job 化（测试/嵌入式）。
func (s *Scheduler) SetJobsRegistry(reg *jobs.Registry) {
	s.schedMu.Lock()
	s.jobsReg = reg
	s.schedMu.Unlock()
}

// jobsRegistry 当前 Job Registry（可能为 nil）。
func (s *Scheduler) jobsRegistry() *jobs.Registry {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.jobsReg
}

// safeHook 调用面板注入的 hook 并 recover panic（v2.6.0 Audit-1）：hooks
// （GrowthHook/TaskSweepHook）由 panel 构造后注入，一次 panic 会杀进程；hook 是旁路
// 触发，失败只应记日志、不应影响调度主循环。
func (s *Scheduler) safeHook(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("ERROR: scheduler: %s hook panic: %v", name, r)
		}
	}()
	fn()
}

// SetTaskSweepHook 挂载/替换任务补领清扫回调（panel 构造晚于 scheduler，事后接线）。
func (s *Scheduler) SetTaskSweepHook(fn func()) {
	s.schedMu.Lock()
	s.cfg.TaskSweepHook = fn
	s.schedMu.Unlock()
}

// SetTaskSweepInterval 热改任务补领清扫间隔；<=0 表示暂停循环（面板关闭该开关时）。
func (s *Scheduler) SetTaskSweepInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.sweepInterval.Store(int64(d))
	poke(s.rearmSweep)
}

// SetBalanceInterval 热改余额刷新间隔；<=0 表示暂停循环（面板关闭该开关时）。
func (s *Scheduler) SetBalanceInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.balanceInterval.Store(int64(d))
	poke(s.rearmBalance)
}

// BalanceInterval 当前余额后台刷新间隔（0=暂停）。面板 overview 据此告诉用户
// 「到期提醒」卡片的数据节奏，而不是让 5 秒界面轮询冒充数据新鲜度。
func (s *Scheduler) BalanceInterval() time.Duration {
	return time.Duration(s.balanceInterval.Load())
}

// BalanceRefreshedAt 最近一次后台余额刷新的完成时刻（零值 = 尚未跑过）。
func (s *Scheduler) BalanceRefreshedAt() time.Time {
	if v := s.lastBalanceRefresh.Load(); v != 0 {
		return time.Unix(0, v)
	}
	return time.Time{}
}
