// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive 四类独立排程。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/jobs"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即四类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	TravelHours    []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours  []int // 默认 [10]
	KeepaliveHours []int // 默认 [22]
	BlackcatHours  []int // 默认 [23]：夜猫子（23:00–08:00 计数窗口）
	GrowthHours    []int // 默认 [1]：成长任务队列（Sequential 族每日零点解锁一环，
	// 01:00 自动扫描+执行；避开零点整防解锁竞态）

	// ExpiringSoonWindow 快过期积分窗口：签到/余额刷新查余额时，把到期时间
	// <= now+window 的套餐余额标记为"快过期"（pool 据此做最早到期优先，见
	// entry.creditsEarliestExpiry）。<=0 时不做路由门槛；展示仍使用完整逐包数据。
	ExpiringSoonWindow time.Duration

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// BlackcatDisabled 显式关闭夜猫子排程（schedule.blackcat_enabled=false）。
	BlackcatDisabled bool
	// GrowthDisabled 显式关闭成长任务自动排程（schedule.growth_enabled=false）。
	GrowthDisabled bool

	// IncludeDisabledInTasks 让「保号类」四任务（签到 / 活跃上报 / token 保活 / 余额刷新）
	// 对已禁用（disabled）的账号也执行（schedule.include_disabled_in_tasks）。
	// 缺省 false = 保持「禁用的跳过」既有语义；打开后禁用号照常签到保号，但**仍不参与
	// 选号**——pool.pick 侧的 disabled 过滤与本开关无关。
	IncludeDisabledInTasks bool

	// GrowthHook 成长任务队列执行回调（panel.RunGrowthQueueOnce：扫描全部账号
	// 待办并执行，与面板「执行全部待办」按钮同管线）。调度器只管时点不管实现——
	// panel 在 scheduler 之后构造，用 SetGrowthHook 事后挂载；nil 时到点跳过。
	GrowthHook func()

	// TaskSweepHook 任务补领清扫回调（panel.RunTaskSweepOnce：把「已达标未领」
	// 的任务奖励领掉，不跑任何行为链路）。与 GrowthHook 同款延迟挂载；nil 时
	// ticker 到点跳过。见 issue #174：成长队列与「一键完成」都不负责这条补领链路。
	TaskSweepHook func()

	// BalanceRefreshConcurrency 余额全量刷新的并发上限（v2.0.1 #10）：<=0 回落
	// 默认 8，clamp 1..32。后台周期刷新、启动首刷与面板手动刷新共用同一闸门——
	// 1146 号池下每个账号一个无上限 goroutine 会把上游打成风暴（风控风险）。
	BalanceRefreshConcurrency int

	// BalanceRefreshStartupDelay 进程启动后首次余额刷新的延迟（v2.0.1 #10）：
	// 0 = 不延迟（Config 零值，测试/老调用方行为不变）；生产由配置默认给 30s。
	// 目的是避免刚重启就与监听/预热等启动动作叠加对全池发起并发请求。
	BalanceRefreshStartupDelay time.Duration

	// BalanceRefreshStartupForce 账号数超过启动首刷保护阈值（>200）时是否仍强制
	// 首刷（v2.0.1 #10）。缺省 false：大池跳过首刷交给周期刷新，避免重启瞬间
	// 打爆上游；需要立即刷新时显式打开。
	BalanceRefreshStartupForce bool

	// CatchUpMinutes 补跑窗口（v2.0.1 #11）：批次跨过后续计划时点后（如 09:00
	// 签到批次跑到 10:05，跨过 activity_hours 的 10:00），若该任务族今天的计划
	// 时点落在 (now-catch_up_minutes, now] 内且今天尚未成功跑过 → 立即补跑一轮。
	// 0 = 关闭（Config 零值 = 旧行为「错过就等明天」，对老调用方零影响）。
	CatchUpMinutes int

	// CheckinConcurrency 签到批次的账号级并发度（v2.3.1 #11）：<=0 回落默认 3，
	// clamp 1..16。签到是「串行 1146 号≈1 小时」的旧瓶颈，worker pool 后单轮
	// 约 10-20 分钟；并发过高会撞上游风控（默认 3 是保守值，面板可热改）。
	CheckinConcurrency int

	// Jobs 可选注入的 Job Registry（v2.3.1 #11）：非 nil 时定时/补跑批次会创建
	// Job（trigger=schedule/catch_up）进 Job 历史，并支持面板取消（ctx 取消早停）。
	// nil = 纯调度模式（老测试/嵌入式用法，行为与 v2.3.0 一致）。
	// 通常经 New 装配；运行期热挂载用 SetJobsRegistry。
	Jobs *jobs.Registry
}

const (
	// defaultBalanceRefreshConcurrency 余额刷新默认并发（v2.0.1 #10 的推荐值 8：
	// 1146 号场景下再高只会给上游 WAF 送人头，再低则全量刷新慢到失去时效）。
	defaultBalanceRefreshConcurrency = 8
	// maxBalanceRefreshConcurrency 并发硬顶 32。
	maxBalanceRefreshConcurrency = 32
	// balanceStartupSkipThreshold 启动首刷的账号数阈值：> 该值且未显式 force 时跳过
	// 首刷（1146 号池重启瞬间的并发风暴是 #10 要消除的最危险路径）。
	balanceStartupSkipThreshold = 200
	// maxCatchUpMinutes 补跑窗口上限 720 分钟（12h）；再宽等价于「全天都补」。
	maxCatchUpMinutes = 720
	// defaultCheckinConcurrency 签到 worker pool 默认并发（v2.3.1 #11）：3 是
	// 「串行 1146 号≈1h」与「上游 WAF 风控」之间的保守折中（QA 观察 429 率再调高）。
	defaultCheckinConcurrency = 3
	// maxCheckinConcurrency 签到并发硬顶 16（配置 clamp 同口径）。
	maxCheckinConcurrency = 16
)

// TaskKind 全池批量任务族标识（单飞 gate 键）。面板批量端点（*_all）与定时
// 批次共用同一把 gate：同一时刻最多一轮同类全池任务，手动/定时不叠加打上游。
type TaskKind string

const (
	TaskCheckin   TaskKind = "checkin"
	TaskTravel    TaskKind = "travel"
	TaskActivity  TaskKind = "activity"
	TaskKeepalive TaskKind = "keepalive"
	TaskBalance   TaskKind = "balance"
)

// valid 报告是否为已知任务族（未知 kind 一律拒绝占 gate，避免拼写错误静默放行）。
func (k TaskKind) valid() bool {
	switch k {
	case TaskCheckin, TaskTravel, TaskActivity, TaskKeepalive, TaskBalance:
		return true
	}
	return false
}

// ProgressFunc 全池任务进度回调（v2.3.0 #10 Job Runner 用）：
//   - done  = 已处理账号数（含 failed/skipped，供进度条单调前进）；
//   - total = 候选账号总数（触发时刻池内账号数）；
//   - fail  = 其中失败数；skip = 其中跳过数（disabled / 无凭证 / 域门控等）；
//   - msg   = 非空时由 Job Runner 追加到 Job.LogTail（失败/关键节点用）。
//
// 回调可并发调用（余额刷新按有界并发逐号上报）；nil 表示不关心进度（旧 Run*Now
// 路径与定时批次），调用方统一经 reportProgress 做 nil 安全上报。
type ProgressFunc func(done, total, fail, skip int, msg string)

// reportProgress nil 安全的进度上报。
func reportProgress(progress ProgressFunc, done, total, fail, skip int, msg string) {
	if progress != nil {
		progress(done, total, fail, skip, msg)
	}
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// schedMu 保护排程参数（时点/开关）；Reconfigure 可在运行期热改（面板保存配置时调用）。
	// rearmSchedule/rearmBalance/rearmSweep 是「排程已变，立即重算」通知：Run 与
	// 各周期循环各自消费，分别用独立 channel（同 channel 被多个 select 消费会丢信号）。
	schedMu       sync.Mutex
	rearmSchedule chan struct{}
	rearmBalance  chan struct{}
	rearmSweep    chan struct{}

	// balanceInterval 余额刷新间隔（纳秒，0=暂停）。atomic 读写：执行循环每轮读当前值，
	// SetBalanceInterval 可任意时刻热改（面板保存配置）。
	balanceInterval atomic.Int64

	// sweepInterval 任务补领清扫间隔（纳秒，0=暂停）。语义同 balanceInterval：
	// SetTaskSweepInterval 可热改（面板保存配置后下一轮生效）。
	sweepInterval atomic.Int64

	// lastBalanceRefresh 最近一次后台余额刷新的完成时刻（Unix 纳秒，0=尚未跑过）。
	// 供面板「到期提醒」标注数据年龄：卡片展示的是这份快照，必须让年龄可读，
	// 否则界面 5 秒轮询会让人误以为数据也是 5 秒级新鲜。
	lastBalanceRefresh atomic.Int64

	// balanceConcurrency 余额全量刷新并发上限（v2.0.1 #10）。atomic 读写：面板保存
	// 配置经 SetBalanceRefreshConcurrency 热改，下一轮刷新生效。
	balanceConcurrency atomic.Int64

	// checkinConcurrency 签到 worker pool 账号级并发度（v2.3.1 #11）。atomic 读写：
	// 面板保存配置经 SetCheckinConcurrency 热改，下一轮签到生效（在途 worker 不打断）。
	checkinConcurrency atomic.Int64

	// checkinMu/checkinDoneDay 当日已签标记（v2.3.1 #11）：uid → 本地日期
	// "2006-01-02"。同一进程内同日再跑（手动/定时/补跑）跳过 checkin RPC、仍刷新
	// 余额；进程重启清空（上游 already-checked 幂等，重跑安全，无需持久化）。
	checkinMu      sync.Mutex
	checkinDoneDay map[string]string

	// batchWG 定时/补跑派发的任务族 goroutine（v2.3.1 #11 按族解耦后不再阻塞主
	// 循环）：Run 退出前在此收敛（ctx 已取消，各任务体会尽快收尾）。
	batchWG sync.WaitGroup

	// bgWG 独立周期循环 goroutine（StartBalanceRefresh / StartTaskSweep）的收敛用
	//（v2.6.0 Audit-1 P0-4）：main 停机顺序必须是「停新任务 → 等所有会写池的后台
	// goroutine 退出 → Flush」。这两个循环不在 Run 里，此前未被任何 WaitGroup 登记，
	// 停机 Flush 之后仍可能写池（余额刷新会写 credits/冷却/到期快照）。
	bgWG sync.WaitGroup

	// jobsReg 可选 Job Registry（v2.3.1 #11）：定时/补跑批次写 Job 历史并支持取消。
	// 由 schedMu 保护（SetJobsRegistry 可在运行期热挂载）。
	jobsReg *jobs.Registry

	// nowFn 可注入时钟（测试用；生产恒为 time.Now）。补跑窗口判定与 lastRun 记录
	// 共用同一时钟，保证「窗口边界/跨天」用例可确定性复现（不随运行环境墙钟漂移）。
	nowFn func() time.Time

	// runStateMu 保护运行期瞬时状态：单飞 gate（running）与各任务族最近成功跑完
	// 的时刻（lastRun）。两者都只服务「本轮/今天是否已跑」的进程内判断，重启即
	// 清零；不参与选号与持久化，无需落盘。
	runStateMu sync.Mutex
	running    map[TaskKind]bool
	lastRun    map[taskKind]time.Time
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.BlackcatHours) == 0 {
		cfg.BlackcatHours = []int{23}
	}
	// v2.0.1 #10/#11 新配置的零值归一：并发 <=0 → 默认 8、上限 32；补跑窗口
	// 负值钳 0（=关闭）、上限 720。config 的 normalize 已做同一套夹取，这里兜底
	// 直接构造 Config 的调用方（测试/嵌入式用法），保证运行期只见到合法值。
	cfg.BalanceRefreshConcurrency = normalizeBalanceRefreshConcurrency(cfg.BalanceRefreshConcurrency)
	cfg.CheckinConcurrency = normalizeCheckinConcurrency(cfg.CheckinConcurrency)
	if cfg.CatchUpMinutes < 0 {
		cfg.CatchUpMinutes = 0
	} else if cfg.CatchUpMinutes > maxCatchUpMinutes {
		cfg.CatchUpMinutes = maxCatchUpMinutes
	}
	s := &Scheduler{
		cfg:            cfg,
		adoptTried:     make(map[string]string),
		rearmSchedule:  make(chan struct{}, 1),
		rearmBalance:   make(chan struct{}, 1),
		rearmSweep:     make(chan struct{}, 1),
		nowFn:          time.Now,
		running:        make(map[TaskKind]bool),
		lastRun:        make(map[taskKind]time.Time),
		checkinDoneDay: make(map[string]string),
		jobsReg:        cfg.Jobs,
	}
	s.balanceConcurrency.Store(int64(cfg.BalanceRefreshConcurrency))
	s.checkinConcurrency.Store(int64(cfg.CheckinConcurrency))
	return s
}

// normalizeCheckinConcurrency 归一跳过并发：<=0 回落默认 3，上限 16。
func normalizeCheckinConcurrency(n int) int {
	if n <= 0 {
		return defaultCheckinConcurrency
	}
	if n > maxCheckinConcurrency {
		return maxCheckinConcurrency
	}
	return n
}

// normalizeBalanceRefreshConcurrency 归一余额刷新并发：<=0 回落默认 8，上限 32。
func normalizeBalanceRefreshConcurrency(n int) int {
	if n <= 0 {
		return defaultBalanceRefreshConcurrency
	}
	if n > maxBalanceRefreshConcurrency {
		return maxBalanceRefreshConcurrency
	}
	return n
}

// now 返回调度器当前时刻（生产 time.Now；测试可替换 nowFn 注入固定时钟）。
// 注意 &Scheduler{} 这类直接字面量构造（老测试）没有初始化 nowFn，也必须可用。
func (s *Scheduler) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

func (s *Scheduler) Reconfigure(checkinHours, travelHours, activityHours, keepaliveHours, blackcatHours, growthHours []int,
	checkinDisabled, travelDisabled, activityDisabled, keepaliveDisabled, blackcatDisabled, growthDisabled bool) {
	s.schedMu.Lock()
	if len(checkinHours) > 0 {
		s.cfg.CheckinHours = checkinHours
	}
	if len(travelHours) > 0 {
		s.cfg.TravelHours = travelHours
	}
	if len(activityHours) > 0 {
		s.cfg.ActivityHours = activityHours
	}
	if len(keepaliveHours) > 0 {
		s.cfg.KeepaliveHours = keepaliveHours
	}
	if len(blackcatHours) > 0 {
		s.cfg.BlackcatHours = blackcatHours
	}
	if len(growthHours) > 0 {
		s.cfg.GrowthHours = growthHours
	}
	s.cfg.CheckinDisabled = checkinDisabled
	s.cfg.TravelDisabled = travelDisabled
	s.cfg.ActivityDisabled = activityDisabled
	s.cfg.KeepaliveDisabled = keepaliveDisabled
	s.cfg.BlackcatDisabled = blackcatDisabled
	s.cfg.GrowthDisabled = growthDisabled
	s.schedMu.Unlock()
	poke(s.rearmSchedule)
	poke(s.rearmBalance)
}

// poke 非阻塞发一次唤醒信号（已有待处理信号则忽略，语义等价）。
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskBlackcat
	taskGrowth
	// taskBalance 没有小时制排程（由独立 ticker 驱动），仅用于 lastRun 记录与
	// 单飞 gate 映射，使余额族的完成时刻与其它族同口径。
	taskBalance
)

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
// 排程参数在 schedMu 下快照，与 Reconfigure 的并发写隔离。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	s.schedMu.Lock()
	checkinHours, keepaliveHours, blackcatHours := s.cfg.CheckinHours, s.cfg.KeepaliveHours, s.cfg.BlackcatHours
	travelHours, activityHours := s.cfg.TravelHours, s.cfg.ActivityHours
	checkinOff, keepaliveOff, blackcatOff := s.cfg.CheckinDisabled, s.cfg.KeepaliveDisabled, s.cfg.BlackcatDisabled
	growthHours, growthOff := s.cfg.GrowthHours, s.cfg.GrowthDisabled
	travelOff, activityOff := s.cfg.TravelDisabled, s.cfg.ActivityDisabled
	s.schedMu.Unlock()

	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if !checkinOff {
		slots = append(slots, slot{nextFire(now, checkinHours), taskCheckin})
	}
	if !travelOff {
		slots = append(slots, slot{nextFire(now, travelHours), taskTravel})
	}
	if !activityOff {
		slots = append(slots, slot{nextFire(now, activityHours), taskActivity})
	}
	if !keepaliveOff {
		slots = append(slots, slot{nextFire(now, keepaliveHours), taskKeepalive})
	}
	if !blackcatOff {
		slots = append(slots, slot{nextFire(now, blackcatHours), taskBlackcat})
	}
	if !growthOff {
		slots = append(slots, slot{nextFire(now, growthHours), taskGrowth})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// wakeupGraceDelay 迟到唤醒补跑的派发前网络宽限：Windows Modern Standby exit 后
// 网络栈/DNS 1-2s 才恢复（issue #152 实测 dial tcp lookup no such host 与
// Kernel-Power 507 standby exit ≤1s 重合），宽限 5s 覆盖 90%+ 唤醒场景。
// 只对迟到补跑生效（准点触发零延迟），零配置。测试可缩短（与
// travelAccountDelay「测试可置 0」同口径）。
var wakeupGraceDelay = 5 * time.Second

// wakeupLateThreshold 迟到判定阈值：now 晚于槽位计划时刻超过 1s 才算迟到补跑。
// 毫秒级抖动（timer 正常触发的偏移量级）不算，避免准点触发被误宽限。
const wakeupLateThreshold = 1 * time.Second

// awaitWakeupGrace 迟到唤醒补跑派发前的网络宽限：槽位时刻已过点超过阈值
// （机器刚从睡眠唤醒）时先等满 wakeupGraceDelay 让网络栈/DNS 就绪再派发。
// 准点/阈值内抖动零延迟直接放行。ctx 取消立即返回 false（优雅停机不等宽限
// 睡满，本批放弃，下轮 nextWake 照旧从"现在"起算）。返回是否继续派发。
func awaitWakeupGrace(ctx context.Context, planned time.Time) bool {
	if late := time.Since(planned); late <= wakeupLateThreshold {
		return ctx.Err() == nil // 准点触发：零延迟放行
	}
	log.Printf("wakeup grace %s: late catch-up for slot %s", wakeupGraceDelay, planned.Format("15:04"))
	return sleepCtx(ctx, wakeupGraceDelay)
}

// wallclockCheckStep 墙钟校验段长：等待槽位时单次 timer 的最大时长，每段醒来用
// 墙钟重判是否到点。值是「时点精度」与「空闲唤醒频率」的折中——60s 段内时点
// 偏差上限 60s，对签到/保活类任务足够。
const wallclockCheckStep = time.Minute

// slotWake waitSlot 的三态结果。
type slotWake int

const (
	slotFired  slotWake = iota // 墙钟已到达计划时点：补跑本批
	slotRearm                  // 排程已变（Reconfigure）：上层重算下一次唤醒
	slotCancel                 // ctx 取消：上层优雅退出
)

// waitSlot 分段等待到 next 的**墙钟**时刻（next 由 nextFire 用 time.Date 构造、
// 不携带单调读数，time.Until 对它是纯墙钟差）。
//
// 为什么不一把 time.NewTimer(time.Until(next)) 睡到底：timer 的等待基于单调时钟，
// macOS / Windows Modern Standby 睡眠会冻结它——睡眠时长不足整个等待时，fire
// 被顺延「睡眠时长」（墙钟已过点、timer 还要继续等），时点被错过且不会立即补跑；
// 睡眠时长超过整个等待时倒是无害的（唤醒瞬间 timer 到期，awaitWakeupGrace 补跑）。
// 分段睡、每段醒来用墙钟重判，把冻结的影响限制在一段之内：睡眠结束后的第一段
// 末尾必然发现「墙钟已越过时点」并立即补跑，偏差上限 = step + 睡眠落段余量。
//
// ctx 取消 / rearmSchedule（在线改配置重排）在每段的 select 里随时返回，段长
// 不影响两者响应性。返回三态见 slotWake。
func (s *Scheduler) waitSlot(ctx context.Context, next time.Time, step time.Duration) slotWake {
	for {
		wallRemain := time.Until(next)
		if wallRemain <= 0 {
			return slotFired
		}
		d := wallRemain
		if d > step {
			d = step
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return slotCancel
		case <-s.rearmSchedule:
			timer.Stop()
			return slotRearm
		case <-timer.C:
			// 段末回到循环顶用墙钟重判：正常推进时若干段后到点；单调时钟被
			// 睡眠冻结时，墙钟大幅前进，至多一段之后即到点补跑。
		}
	}
}

// Run 主循环，阻塞直到 ctx 取消。
// Reconfigure 触发 rearmSchedule 时提前唤醒重算（新时点/开关立即生效）。
//
// v2.3.1 #11 按族解耦：到点批次不再 wg.Wait 整批——每族独立 goroutine 派发，
// 慢签到不阻塞同/后一时点的旅行/活跃/保活（修复「批次跨过后续时点导致整天漏跑」）。
// 退出前等待本进程派发的任务族收敛（ctx 已取消，任务体内部会尽快收尾）。
func (s *Scheduler) Run(ctx context.Context) {
	s.runLoop(ctx)
	s.batchWG.Wait()
}

// runLoop 是 Run 的排程循环主体（拆出便于退出收敛逻辑与测试）。
func (s *Scheduler) runLoop(ctx context.Context) {
	for {
		// 补跑窗口（v2.0.1 #11）：启动后首轮、以及每批任务结束后（循环回到顶部）
		// 先检查一次「今天错过的时点」。批次/补跑本身可能跨过后续时点（如 09:00
		// 签到批次跑到 10:05，跨过 10:00 活跃上报）——nextWake 对已过时点返回
		// 明天，不在这里补就会整天漏跑（保活/活跃的 P0 漏跑风险）。补跑走单飞
		// gate 且每族当天只补一次，循环有界；窗口关闭（0）时是空操作。
		s.runCatchUps(ctx, s.now())
		if ctx.Err() != nil {
			return
		}
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// 四类任务全部禁用：不空转，等重排通知（在线改配置重新启用）或退出信号。
			select {
			case <-ctx.Done():
				return
			case <-s.rearmSchedule:
				continue
			}
		}
		switch s.waitSlot(ctx, next, wallclockCheckStep) {
		case slotCancel:
			return
		case slotRearm:
			continue // 排程已变：重算下一次唤醒
		case slotFired:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			// 迟到唤醒（睡眠跨过槽位时刻，timer 在唤醒瞬间才到期）先等网络宽限：
			// 唤醒瞬间 DNS 未就绪，零宽限派发等于把唯一一次补跑机会打在注定失败
			// 的窗口里（issue #152）；准点触发零延迟不受影响。
			if !awaitWakeupGrace(ctx, next) {
				return // ctx 取消：放弃本批，优雅退出
			}
			// 唤醒时每族**独立 goroutine** 派发（v2.3.1 #11 按族解耦）：不再等整批
			// 跑完，慢签到（1146 号串行≈1h→worker pool 后 10-20min）不会阻塞同一
			// 时点的旅行/活跃，也不会吞掉后一时点的批次；单飞 gate 保证同类不叠加。
			s.dispatchBatch(ctx, kinds)
		}
	}
}

// WaitBackground 等待 StartBalanceRefresh / StartTaskSweep 启动的周期循环退出
// （v2.6.0 Audit-1 P0-4）。返回 true = 全部收敛；false = ctx 先到期（调用方 WARN 后
// 继续停机，但不得在此之前认为「后台已停止」）。
// 调用约定：先 cancel 根 ctx 再调用（循环只在 ctx 取消/服务结束才退出）。
func (s *Scheduler) WaitBackground(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		s.bgWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}
