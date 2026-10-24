// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive 四类独立排程。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
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
	"github.com/lei-rr/workbuddy2api-pro/internal/jobs"
	"github.com/lei-rr/workbuddy2api-pro/internal/logfmt"
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

// StartTaskSweep 后台周期性任务补领清扫（独立 ticker goroutine，ctx 取消即停）。
// 与 StartBalanceRefresh 同款结构；interval<=0 不启动（面板关闭开关时 main 不
// 调用即可），运行期可用 SetTaskSweepInterval 热改间隔（下一轮生效）。
//
// 与成长队列（GrowthHook，小时制）的区别：队列执行的是**行为链路**（真实对话、
// 事件上报）成本高；清扫只做 accept/claim 两条幂等读改请求，分钟级跑也几乎零
// 负担，却能补上「达标的任务没人领奖」这条长期缺失的链路（issue #174）。
func (s *Scheduler) StartTaskSweep(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.sweepInterval.Store(int64(interval))
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("ERROR: scheduler: 任务补领清扫循环 panic: %v", r)
			}
		}()
		var logged time.Duration
		for {
			cur := time.Duration(s.sweepInterval.Load())
			if cur != logged {
				log.Printf("scheduler: 任务补领清扫每 %s（暂停中显示 0s）", cur)
				logged = cur
			}
			if cur <= 0 {
				// 被热改暂停：等重排通知（重新启用时唤醒）或退出。
				select {
				case <-ctx.Done():
					return
				case <-s.rearmSweep:
					continue
				}
			}
			timer := time.NewTimer(cur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmSweep:
				timer.Stop() // 间隔已变：立刻按新值重算
			case <-timer.C:
				s.schedMu.Lock()
				hook := s.cfg.TaskSweepHook
				s.schedMu.Unlock()
				if hook != nil {
					s.safeHook("task_sweep", hook)
				}
			}
		}
	}()
}

// RunTaskSweepNow 立刻跑一轮任务补领清扫（手动触发/测试用）。
func (s *Scheduler) RunTaskSweepNow() {
	s.schedMu.Lock()
	hook := s.cfg.TaskSweepHook
	s.schedMu.Unlock()
	if hook != nil {
		s.safeHook("task_sweep", hook)
	}
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
