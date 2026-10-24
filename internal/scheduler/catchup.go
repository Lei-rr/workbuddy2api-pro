package scheduler

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/jobs"
)

// noteTaskDone 记录某任务族最近一次成功跑完的时刻（内存，重启清零）。
// 补跑窗口据此判断「今天该族是否已成功跑过」：正常到点执行的那轮也要记，
// 否则批内跨过后续时点后会把「今天已经跑过」的族再补一遍。
func (s *Scheduler) noteTaskDone(k taskKind, at time.Time) {
	s.runStateMu.Lock()
	if s.lastRun == nil {
		s.lastRun = make(map[taskKind]time.Time)
	}
	s.lastRun[k] = at
	s.runStateMu.Unlock()
}

// taskRanOn 报告该任务族在 day 所属自然日是否已成功跑过。
func (s *Scheduler) taskRanOn(k taskKind, day time.Time) bool {
	s.runStateMu.Lock()
	at, ok := s.lastRun[k]
	s.runStateMu.Unlock()
	return ok && sameLocalDay(at, day)
}

// sameLocalDay 判断两个时刻是否同一自然日（按 day 的时区解释，与 nextFire 用
// now.Location() 构造时点的口径一致）。
func sameLocalDay(a, b time.Time) bool {
	a = a.In(b.Location())
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

// SetCatchUpMinutes 热更新补跑窗口（分钟，0=关闭；越界 clamp 0..720）。
// 面板保存 schedule.catch_up_minutes 后立即生效（下一轮补跑判定读新值）。
func (s *Scheduler) SetCatchUpMinutes(minutes int) {
	if minutes < 0 {
		minutes = 0
	} else if minutes > maxCatchUpMinutes {
		minutes = maxCatchUpMinutes
	}
	s.schedMu.Lock()
	s.cfg.CatchUpMinutes = minutes
	s.schedMu.Unlock()
}

// catchUpWindow 当前补跑窗口（0=关闭）；schedMu 下读，与 Reconfigure/SetCatchUpMinutes 同步。
func (s *Scheduler) catchUpWindow() time.Duration {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return time.Duration(s.cfg.CatchUpMinutes) * time.Minute
}

// checkinDayKey 进程内「当日已签」标记的日期键（本地时区自然日；与上游签到重置
// 口径一致——上游按本地/业务日重置，用本地日期即可，不做 CST 特殊处理）。
func checkinDayKey(t time.Time) string { return t.Format("2006-01-02") }

// checkinDoneToday 该账号本进程内今天是否已签到成功（v2.3.1 #11 跳过已签）。
func (s *Scheduler) checkinDoneToday(uid string) bool {
	day := checkinDayKey(s.now())
	s.checkinMu.Lock()
	defer s.checkinMu.Unlock()
	return s.checkinDoneDay[uid] == day
}

// markCheckinDone 标记该账号今天已签到成功（含上游 already-checked 幂等）。
func (s *Scheduler) markCheckinDone(uid string) {
	day := checkinDayKey(s.now())
	s.checkinMu.Lock()
	s.checkinDoneDay[uid] = day
	s.checkinMu.Unlock()
}

// ---------------------------------------------------------------------------
// 补跑窗口（v2.0.1 #11 子项）
// ---------------------------------------------------------------------------

// catchUpDue 返回 now 时刻应补跑的任务族列表。
//
// 判定：某族**今天**的某个计划整点落在 (now-catch_up, now] 内（左开右闭）、该族
// 今天尚未成功跑过、且窗口开启 → 需要补跑。典型场景是两个任务的交接点：
//   - 09:00 签到+旅行批次在 1146 号下跑到 10:05，跨过 activity_hours 的 10:00；
//   - 21:00 签到批次跨过 22:00 保活、23:00 夜猫子同理。
//
// nextWake 对「已经过去的时点」一律返回明天，所以这些时点一旦被批次跨过就会
// 整天漏跑；本函数是唯一的挽回入口。正常到点执行的那轮已记 lastRun，因此不会
// 重复补跑（补跑只补一次/天/族）。
func (s *Scheduler) catchUpDue(now time.Time) []taskKind {
	s.schedMu.Lock()
	minutes := s.cfg.CatchUpMinutes
	checkinHours := s.cfg.CheckinHours
	travelHours := s.cfg.TravelHours
	activityHours := s.cfg.ActivityHours
	keepaliveHours := s.cfg.KeepaliveHours
	blackcatHours := s.cfg.BlackcatHours
	growthHours := s.cfg.GrowthHours
	checkinOff, travelOff := s.cfg.CheckinDisabled, s.cfg.TravelDisabled
	activityOff, keepaliveOff := s.cfg.ActivityDisabled, s.cfg.KeepaliveDisabled
	blackcatOff, growthOff := s.cfg.BlackcatDisabled, s.cfg.GrowthDisabled
	s.schedMu.Unlock()
	if minutes <= 0 {
		return nil
	}
	windowStart := now.Add(-time.Duration(minutes) * time.Minute)
	type family struct {
		kind  taskKind
		hours []int
		off   bool
	}
	families := []family{
		{taskCheckin, checkinHours, checkinOff},
		{taskTravel, travelHours, travelOff},
		{taskActivity, activityHours, activityOff},
		{taskKeepalive, keepaliveHours, keepaliveOff},
		{taskBlackcat, blackcatHours, blackcatOff},
		{taskGrowth, growthHours, growthOff},
	}
	var due []taskKind
	for _, f := range families {
		if f.off || s.taskRanOn(f.kind, now) {
			continue
		}
		// 同类正在执行（手动 Job/定时批次/上一轮补跑）→ 本轮不补：单飞 gate 保证
		// 不会叠加两轮全池任务；提前过滤避免「刚派发完就判补跑」的日志噪音。
		if gk, gated := gateKindOf(f.kind); gated && s.TaskRunning(gk) {
			continue
		}
		for _, h := range f.hours {
			slot := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
			// 窗口左开右闭：(now-catch_up, now]；恰好等于窗口起点的时点不补
			// （避免窗口边缘抖动导致「差一秒补一整轮」）。
			if slot.After(windowStart) && !slot.After(now) {
				due = append(due, f.kind)
				break
			}
		}
	}
	return due
}

// runCatchUps 立即补跑窗口命中的任务族；返回实际启动的族（测试/日志可观测）。
// 每个族都走与定时批次相同的单飞 gate：同类正在执行时跳过本轮（绝不在一轮全池
// 任务之上再叠一轮）。补跑只补一次/天/族：任务体跑完即记 lastRun，下次判定不再命中。
func (s *Scheduler) runCatchUps(ctx context.Context, now time.Time) []taskKind {
	if ctx.Err() != nil {
		return nil
	}
	due := s.catchUpDue(now)
	if len(due) == 0 {
		return nil
	}
	log.Printf("scheduler: 补跑窗口命中 %v（%d 分钟内错过的时点，今天尚未跑过）",
		due, s.catchUpWindow()/time.Minute)
	// v2.3.1 #11：注入 Job Registry 时补跑批次也进 Job 历史（trigger=catch_up），
	// 异步派发不阻塞主循环；未注入时保持 v2.3.0 同步语义（返回时任务已跑完，
	// 既有确定性测试依赖该时序）。
	if s.jobsRegistry() != nil {
		var started []taskKind
		for _, k := range due {
			if s.dispatchTaskKind(ctx, k, jobs.TriggerCatchUp) {
				started = append(started, k)
			}
		}
		return started
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		started []taskKind
	)
	for _, k := range due {
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			if !s.executeTaskKind(ctx, k) {
				return
			}
			mu.Lock()
			started = append(started, k)
			mu.Unlock()
		}(k)
	}
	wg.Wait()
	return started
}

// sleepCtx 可取消的等待：ctx 取消立即返回 false（优雅停机不必等限速睡醒），
// 等满返回 true。d<=0 立即放行。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
