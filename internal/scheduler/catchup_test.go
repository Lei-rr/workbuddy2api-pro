// catchup_test.go 钉住 v2.0.1 #11 的「补跑窗口 + 单飞」语义：
//
//   - 批次跨过后续时点（09:00 批次跑到 10:05 跨过 10:00 活跃上报）→ 立即补跑；
//   - 窗口外（时点早于 now-catch_up_minutes）不补；
//   - 今天已成功跑过 → 不重复补（正常到点执行的那轮也记 lastRun）；
//   - catch_up_minutes=0 → 完全关闭补跑（旧语义「错过就等明天」）；
//   - 同类任务正在执行（单飞 gate 被占）→ 补跑跳过，且不误记「今天已跑」。
//
// 时钟经 Scheduler.nowFn 注入固定值，窗口边界/跨天判定不随运行环境墙钟漂移。
// 任务体用空池（遍历 0 账号，不触任何上游），因此本文件的补跑会真正执行、
// 但只留下 lastRun 记录——可确定性断言「补了什么、补了几次」。
package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
)

// catchUpTestScheduler 构造补跑用例的调度器：空池 + 固定时点。
// 夜猫子/成长显式关闭——它们的执行入口依赖真实墙钟（夜猫子窗口判定），
// 在补跑执行型用例里会让结果随跑测试的时刻漂移；它们的「是否该补」放在
// 纯判定用例 TestCatchUpDueIncludesBlackcatAndGrowth 里覆盖。
func catchUpTestScheduler() *Scheduler {
	return New(Config{
		Pool:             pool.New(""),
		CheckinHours:     []int{9},
		TravelHours:      []int{9},
		ActivityHours:    []int{10},
		KeepaliveHours:   []int{22},
		BlackcatDisabled: true,
		GrowthDisabled:   true,
		CatchUpMinutes:   120,
	})
}

// withFixedNow 注入固定时钟（测试结束无需复位：调度器实例随用例丢弃）。
func withFixedNow(s *Scheduler, at time.Time) { s.nowFn = func() time.Time { return at } }

// catchUpTestDay 固定测试自然日（本地时区，与 nextFire 的时点构造口径一致）。
var catchUpTestDay = time.Date(2026, 7, 27, 0, 0, 0, 0, time.Local)

func taskKindSet(kinds []taskKind) map[taskKind]bool {
	m := make(map[taskKind]bool, len(kinds))
	for _, k := range kinds {
		m[k] = true
	}
	return m
}

func hasTaskKind(kinds []taskKind, want taskKind) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

// TestCatchUpDueWindow 窗口判定：今天计划时点落在 (now-catch_up, now] 才补；
// 左边界开（恰好等于窗口起点的时点不补），右边界闭（恰好点补）。
func TestCatchUpDueWindow(t *testing.T) {
	s := catchUpTestScheduler()

	// 10:05：窗口 (08:05, 10:05]，09:00 签到/旅行 + 10:00 活跃都在窗口内；22:00 保活未到。
	now := catchUpTestDay.Add(10*time.Hour + 5*time.Minute)
	got := taskKindSet(s.catchUpDue(now))
	for _, want := range []taskKind{taskCheckin, taskTravel, taskActivity} {
		if !got[want] {
			t.Errorf("10:05 应补 %v，got %v", want, s.catchUpDue(now))
		}
	}
	if got[taskKeepalive] {
		t.Errorf("22:00 保活时点未到不应补，got %v", s.catchUpDue(now))
	}

	// 11:00：窗口 (09:00, 11:00] —— 09:00 恰好等于窗口起点（左开）→ 不补；
	// 10:00 活跃仍在窗口内 → 补。
	now = catchUpTestDay.Add(11 * time.Hour)
	due := s.catchUpDue(now)
	if hasTaskKind(due, taskCheckin) || hasTaskKind(due, taskTravel) {
		t.Errorf("09:00 恰好等于窗口起点（左开）不应补，got %v", due)
	}
	if !hasTaskKind(due, taskActivity) {
		t.Errorf("10:00 仍在窗口内应补，got %v", due)
	}

	// 14:00：窗口 (12:00, 14:00] —— 09/10 点全部在窗口外 → 不补。
	if due := s.catchUpDue(catchUpTestDay.Add(14 * time.Hour)); len(due) != 0 {
		t.Errorf("窗口外不应补，got %v", due)
	}
}

// TestCatchUpDueSkipsRanToday 今天已成功跑过的族不补（按自然日，昨天的不算数）。
func TestCatchUpDueSkipsRanToday(t *testing.T) {
	now := catchUpTestDay.Add(10*time.Hour + 5*time.Minute)

	s := catchUpTestScheduler()
	// 三个窗口内的族今天都跑过了（模拟正常到点批次记录），不该再补。
	s.noteTaskDone(taskCheckin, catchUpTestDay.Add(9*time.Hour))
	s.noteTaskDone(taskTravel, catchUpTestDay.Add(9*time.Hour+time.Minute))
	s.noteTaskDone(taskActivity, catchUpTestDay.Add(10*time.Hour+time.Minute))
	if due := s.catchUpDue(now); len(due) != 0 {
		t.Errorf("今天已成功跑过不应重复补，got %v", due)
	}

	// 昨天的记录不阻断今天（lastRun 按天比较）。
	s2 := catchUpTestScheduler()
	yesterday := catchUpTestDay.AddDate(0, 0, -1).Add(10 * time.Hour)
	s2.noteTaskDone(taskCheckin, yesterday)
	s2.noteTaskDone(taskTravel, yesterday)
	s2.noteTaskDone(taskActivity, yesterday)
	got := taskKindSet(s2.catchUpDue(now))
	for _, want := range []taskKind{taskCheckin, taskTravel, taskActivity} {
		if !got[want] {
			t.Errorf("昨天的记录不应阻断今天补跑 %v", want)
		}
	}
}

// TestCatchUpDisabledWhenZero catch_up_minutes=0（及负值 clamp 0）→ 完全不补，
// 保持引入前的「错过就等明天」语义。
func TestCatchUpDisabledWhenZero(t *testing.T) {
	s := catchUpTestScheduler()
	s.SetCatchUpMinutes(0)
	now := catchUpTestDay.Add(10*time.Hour + 5*time.Minute)
	if due := s.catchUpDue(now); due != nil {
		t.Fatalf("catch_up=0 不应补跑，got %v", due)
	}
	if started := s.runCatchUps(context.Background(), now); len(started) != 0 {
		t.Fatalf("catch_up=0 不应启动补跑，got %v", started)
	}

	s.SetCatchUpMinutes(-5) // 负值 clamp 到 0（同关停）
	if due := s.catchUpDue(now); due != nil {
		t.Fatalf("catch_up 负值应 clamp 0（关停），got %v", due)
	}
}

// TestRunBatchCrossingLaterSlotCatchUp 核心场景：09:00 批次（签到+旅行）跑到
// 10:05，跨过 activity_hours 的 10:00。nextWake 对已过时点返回明天，必须靠补跑
// 挽回；补跑只补一次/天/族。
func TestRunBatchCrossingLaterSlotCatchUp(t *testing.T) {
	s := catchUpTestScheduler()
	batchAt := catchUpTestDay.Add(9 * time.Hour)
	withFixedNow(s, batchAt)

	// 模拟 09:00 到点批次：任务体（空池）完成并记录 lastRun。
	s.runBatch(context.Background(), []taskKind{taskCheckin, taskTravel})

	// 批次刚结束时 10:00 还没到：不应补（也验证「正常到点那轮记了 lastRun」）。
	early := batchAt.Add(5 * time.Minute)
	if due := s.catchUpDue(early); len(due) != 0 {
		t.Fatalf("10:00 未到不应补跑，got %v", due)
	}

	// 批次实际跑到 10:05：跨过 10:00 活跃上报时点。
	now := batchAt.Add(time.Hour + 5*time.Minute)
	withFixedNow(s, now)
	due := s.catchUpDue(now)
	if len(due) != 1 || due[0] != taskActivity {
		t.Fatalf("应只补 activity（checkin/travel 今天已跑），got %v", due)
	}
	started := s.runCatchUps(context.Background(), now)
	if len(started) != 1 || started[0] != taskActivity {
		t.Fatalf("实际启动=%v want [activity]", started)
	}
	if !s.taskRanOn(taskActivity, now) {
		t.Fatal("补跑完成后应记录今天已跑")
	}

	// 只补一次/天/族：紧接着再判定/再跑都不再命中。
	if due := s.catchUpDue(now.Add(time.Minute)); len(due) != 0 {
		t.Fatalf("补跑后不应重复命中，got %v", due)
	}
	if again := s.runCatchUps(context.Background(), now.Add(time.Minute)); len(again) != 0 {
		t.Fatalf("补跑后不应重复启动，got %v", again)
	}
}

// TestRunCatchUpsSkipsKindAlreadyRunning 同类单飞：activity 的 gate 被占时补跑
// 跳过（不叠加两轮全池任务），且**不**误记「今天已跑」——gate 释放后仍可补。
func TestRunCatchUpsSkipsKindAlreadyRunning(t *testing.T) {
	s := catchUpTestScheduler()
	now := catchUpTestDay.Add(10*time.Hour + 5*time.Minute)
	withFixedNow(s, now) // 任务体用 now 记录 lastRun，后续判定需同一时钟
	if !s.BeginTask(TaskActivity) {
		t.Fatal("前置：占用 activity gate 失败")
	}

	started := taskKindSet(s.runCatchUps(context.Background(), now))
	if started[taskActivity] {
		t.Fatal("activity gate 被占时补跑不应启动同类任务")
	}
	if s.taskRanOn(taskActivity, now) {
		t.Fatal("被单飞跳过的族不应记录「今天已跑」")
	}
	// 其它族照常补（互不互斥）。
	if !started[taskCheckin] || !started[taskTravel] {
		t.Errorf("其它族应照常补跑，started=%v", started)
	}

	// 释放 gate 后：只剩 activity 待补（其余已记今天已跑）。
	s.EndTask(TaskActivity)
	again := s.runCatchUps(context.Background(), now)
	if len(again) != 1 || again[0] != taskActivity {
		t.Fatalf("gate 释放后应补 activity，got %v", again)
	}
}

// TestRunCatchUpsRespectsCanceledContext ctx 已取消（优雅停机）→ 不启动补跑。
func TestRunCatchUpsRespectsCanceledContext(t *testing.T) {
	s := catchUpTestScheduler()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	now := catchUpTestDay.Add(10*time.Hour + 5*time.Minute)
	if started := s.runCatchUps(ctx, now); started != nil {
		t.Fatalf("ctx 已取消不应补跑，got %v", started)
	}
	if s.taskRanOn(taskCheckin, now) {
		t.Fatal("ctx 已取消不应记录已跑")
	}
}

// TestCatchUpDueIncludesBlackcatAndGrowth 纯判定：夜猫子/成长也属小时制任务族，
// 时点落在窗口内且今天未跑时应进补跑候选（执行入口沿用既有 Run* 函数）。
func TestCatchUpDueIncludesBlackcatAndGrowth(t *testing.T) {
	s := New(Config{
		Pool:              pool.New(""),
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		BlackcatHours:     []int{23},
		GrowthHours:       []int{1},
		CatchUpMinutes:    120,
	})
	// 23:30：窗口 (21:30, 23:30] → 夜猫子 23:00 命中。
	if due := s.catchUpDue(catchUpTestDay.Add(23*time.Hour + 30*time.Minute)); !hasTaskKind(due, taskBlackcat) {
		t.Errorf("23:30 应补夜猫子，got %v", due)
	}
	// 次日 02:00：窗口 (00:00, 02:00] → 成长 01:00 命中。
	if due := s.catchUpDue(catchUpTestDay.AddDate(0, 0, 1).Add(2 * time.Hour)); !hasTaskKind(due, taskGrowth) {
		t.Errorf("02:00 应补成长任务，got %v", due)
	}
}

// TestRunBatchSkipsKindWhenGateHeld 定时批次（runBatch）同样走单飞 gate：同类
// 手动任务正在执行时本轮跳过、不记录「今天已跑」；gate 释放后下轮照常执行。
func TestRunBatchSkipsKindWhenGateHeld(t *testing.T) {
	s := catchUpTestScheduler()
	now := catchUpTestDay.Add(9 * time.Hour)
	withFixedNow(s, now)
	if !s.BeginTask(TaskActivity) {
		t.Fatal("前置：占用 activity gate 失败")
	}
	s.runBatch(context.Background(), []taskKind{taskActivity})
	if s.taskRanOn(taskActivity, now) {
		t.Fatal("gate 被占时定时批次不应执行 activity，也不应记录已跑")
	}
	s.EndTask(TaskActivity)
	s.runBatch(context.Background(), []taskKind{taskActivity})
	if !s.taskRanOn(taskActivity, now) {
		t.Fatal("gate 释放后定时批次应执行 activity 并记录已跑")
	}
}

// TestTaskGateSingleFlight 单飞 gate API：占位互斥、释放可重入、族间互不影响、
// 未知 kind 拒绝（拼写错误不能静默放行）。
func TestTaskGateSingleFlight(t *testing.T) {
	s := New(Config{})

	if s.TaskRunning(TaskCheckin) {
		t.Fatal("初始不应在跑")
	}
	if !s.BeginTask(TaskCheckin) {
		t.Fatal("首次占位应成功")
	}
	if !s.TaskRunning(TaskCheckin) {
		t.Fatal("占位后 TaskRunning 应为 true")
	}
	if s.BeginTask(TaskCheckin) {
		t.Fatal("同一族重复占位应失败（单飞）")
	}
	// 族间互不影响：签到在跑不挡余额刷新。
	if !s.BeginTask(TaskBalance) {
		t.Fatal("不同族之间不应互斥")
	}
	s.EndTask(TaskBalance)
	s.EndTask(TaskCheckin)
	if s.TaskRunning(TaskCheckin) {
		t.Fatal("释放后不应在跑")
	}
	if !s.BeginTask(TaskKeepalive) {
		t.Fatal("释放后应能再次占位")
	}
	s.EndTask(TaskKeepalive)
	// 未持有也安全（幂等删除）。
	s.EndTask(TaskTravel)

	if s.BeginTask(TaskKind("bogus")) {
		t.Fatal("未知 kind 应拒绝占位")
	}
	if s.TaskRunning(TaskKind("bogus")) {
		t.Fatal("未知 kind 不应在跑")
	}
}
