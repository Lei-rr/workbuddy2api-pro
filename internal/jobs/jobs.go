// Package jobs 全池批量任务 Job Runner（v2.3.0 #10）。
//
// 职责：把「一次全池任务」建模为可观测的 Job——异步执行、进度上报、取消、
// 同 kind 单飞、并发闸门（同时执行的 Job 数上限）与 jobs.json 持久化。
//
// 设计边界：
//   - 本包不依赖 panel/scheduler：任务体由调用方以 TaskFunc 注入，进度经 Progress
//     回调上报，Registry 只做状态机/并发/持久化。因此可独立单测。
//   - 同 kind 单飞与 scheduler 的 BeginTask/EndTask gate 是两层保护：scheduler gate
//     防「手动 vs 定时」撞车，Registry 防「手动重复点击 + 进程内 Job 重入」。panel
//     会先占 scheduler gate 再起 Job，两层都保留旧 409 语义。
//   - 状态机：pending → running → done|failed|canceled；进程重启时上次未完成的
//     （pending/running）由加载逻辑映射为 interrupted。
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Kind 任务类型（与 scheduler 批量任务族、旧 *_all 端点一一对应）。
type Kind string

const (
	KindCheckin   Kind = "checkin"
	KindTravel    Kind = "travel"
	KindActivity  Kind = "activity"
	KindKeepalive Kind = "keepalive"
	KindBalance   Kind = "balance"
	KindTasksRun  Kind = "tasks_run"
)

// Valid 报告是否为已知任务类型（API 路径参数校验用；未知 kind 一律 400）。
func (k Kind) Valid() bool {
	switch k {
	case KindCheckin, KindTravel, KindActivity, KindKeepalive, KindBalance, KindTasksRun:
		return true
	}
	return false
}

// Trigger 触发来源。
type Trigger string

const (
	TriggerManual   Trigger = "manual"   // 面板手动触发
	TriggerSchedule Trigger = "schedule" // 定时器/调度器触发
	TriggerCatchUp  Trigger = "catch_up" // #11 补跑窗口触发
)

// State Job 状态。
type State string

const (
	StatePending     State = "pending"
	StateRunning     State = "running"
	StateDone        State = "done"
	StateFailed      State = "failed"
	StateCanceled    State = "canceled"
	StateInterrupted State = "interrupted" // 进程重启：上次未跑完（加载时映射，不在运行期出现）
)

// Job 一次全池任务执行记录（jobs.json 持久化 + GET /jobs API 返回）。
//
// 时间字段用 time.Time（JSON = RFC3339 字符串），与面板其它 API（Status 的
// last_success_time 等）口径一致；前端展示时自行格式化。
type Job struct {
	ID         string    `json:"id"`
	Kind       Kind      `json:"kind"`
	Trigger    Trigger   `json:"trigger"`
	State      State     `json:"state"`
	Total      int       `json:"total"`
	Done       int       `json:"done"`
	Fail       int       `json:"fail"`
	Skip       int       `json:"skip"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	// Error 执行级错误（fn 返回 error / panic 恢复）；账号级失败只进 Fail 计数。
	Error string `json:"error,omitempty"`
	// LogTail 最近 50 条进度/错误消息（环形，升序 = 旧→新）。
	LogTail []string `json:"log_tail,omitempty"`
}

// LogTailMax LogTail 保留条数上限。
const LogTailMax = 50

// Progress 进度回调：done/fail/skip 为**累计值**，total 为总账号/任务项数；
// msg 非空时追加到 Job.LogTail。实现方需容忍并发调用（Registry 内部加锁）。
type Progress func(done, total, fail, skip int, msg string)

// TaskFunc Job 执行体。ctx 随 Cancel(id)/进程停机取消；progress 可为 nil（调用方
// 用 Report 辅助函数做 nil 安全上报）。返回 error → Job.State=failed（Error 记录）；
// 若 ctx 已取消则优先记为 canceled。
type TaskFunc func(ctx context.Context, progress Progress) error

var (
	// ErrRunning 同类任务正在执行（panel 映射 HTTP 409）。
	ErrRunning = errors.New("jobs: 同类任务正在执行中")
	// ErrClosed Registry 已关闭（优雅停机/Close 后不再接受新 Job）。
	ErrClosed = errors.New("jobs: 任务注册表已关闭，不再接受新任务")
	// ErrNotFound Job id 不存在（panel 映射 404）。
	ErrNotFound = errors.New("jobs: 任务不存在")
)

// Config Registry 配置。
type Config struct {
	// Path jobs.json 路径（空 = 不持久化，仅内存；测试/嵌入用法）。
	Path string
	// Retain 保留的最近 Job 条数（<=0 回落 50，上限 500——与 config jobs.retain 同口径）。
	Retain int
	// MaxConcurrency 同时执行的 Job 数上限（<=0 回落 4）。不同 kind 的 Job 可并行，
	// 超过上限的 Job 保持 pending 等待执行槽（进度接口可见）。
	MaxConcurrency int
	// BaseContext 根 ctx：取消后运行中的 Job 全部取消、拒绝新 Job。nil = Background。
	// main 传优雅停机的 ctx（W1 #14 链路），保证 SIGTERM 时 Job 能收尾为 canceled。
	BaseContext context.Context
	// WaitGroup 可选：每个 Job 的后台 goroutine 登记到它，供优雅停机等待收敛
	//（main 把 jobs 也纳入 shutdownDeps.Background 的等待集合）。
	WaitGroup *sync.WaitGroup
	// Now 时钟注入（测试用；nil = time.Now）。
	Now func() time.Time
	// OnFinish 可选：Job 进入终态（或 Record）后同步回调（通知/审计接线用；
	// 回调里不得反向调用 Registry 的阻塞方法——会等自己）。
	OnFinish func(job *Job)
}

// persisted jobs.json 文件格式（带 version 便于后续演进）。
type persisted struct {
	Version int    `json:"version"`
	Jobs    []*Job `json:"jobs"`
}

// Registry Job 注册表（并发安全）。
type Registry struct {
	mu           sync.Mutex
	jobs         []*Job // 旧→新（List 倒序输出）
	byID         map[string]*Job
	cancels      map[string]context.CancelFunc // 运行中 Job → cancel
	runningKinds map[Kind]bool                 // 同 kind 单飞
	sem          chan struct{}                 // 并发闸门（同时执行的 Job 数）
	base         context.Context
	closed       bool
	path         string
	retain       int
	jobWG        sync.WaitGroup // 内部：所有 Job goroutine（Wait 用）
	extWG        *sync.WaitGroup
	now          func() time.Time
	onFinish     func(*Job)
	seq          atomic.Uint64
	persistMu    sync.Mutex // 串行化 jobs.json 写盘（避免 tmp 路径并发写）
}

// New 构建 Registry 并加载已有 jobs.json（缺失按首次启动；损坏 WARN 后从空开始）。
func New(cfg Config) *Registry {
	if cfg.Retain <= 0 {
		cfg.Retain = 50
	}
	if cfg.Retain > 500 {
		cfg.Retain = 500
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 4
	}
	base := cfg.BaseContext
	if base == nil {
		base = context.Background()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	r := &Registry{
		byID:         make(map[string]*Job),
		cancels:      make(map[string]context.CancelFunc),
		runningKinds: make(map[Kind]bool),
		sem:          make(chan struct{}, cfg.MaxConcurrency),
		base:         base,
		path:         cfg.Path,
		retain:       cfg.Retain,
		extWG:        cfg.WaitGroup,
		now:          now,
		onFinish:     cfg.OnFinish,
	}
	r.load()
	return r
}

// BaseContext 返回 Registry 的根 ctx（测试/接线观测用）。
func (r *Registry) BaseContext() context.Context { return r.base }

// MaxConcurrency 当前并发闸门上限（日志/观测用）。
func (r *Registry) MaxConcurrency() int { return cap(r.sem) }

// newIDLocked 生成 Job id：`<kind>-<unix毫秒>-<进程内序号>`（同毫秒也能唯一）。
// 调用方需持有 r.mu（保证追加顺序与 id 生成顺序一致；序号本身是 atomic）。
func (r *Registry) newIDLocked(kind Kind) string {
	return fmt.Sprintf("%s-%d-%d", kind, r.now().UnixMilli(), r.seq.Add(1))
}

// Run 异步启动一个 Job：立即返回启动前的 **pending 快照**（深拷贝，调用方可安全
// 读取/丢弃；Registry 内部 Job 字段的后续变化不会竞争性地写这份副本）。
//
//   - 同 kind 已有 Job 在跑/排队 → ErrRunning（panel 映射 409）；
//   - Registry 已关闭或根 ctx 已取消 → ErrClosed；
//   - 未知 kind / fn == nil → 参数错误（普通 error）。
//
// 执行槽（MaxConcurrency）在后台 goroutine 里获取：超限 Job 保持 pending 直到
// 有槽位释放（进度 API 可见 pending → running 的迁移）。
//
// v2.6.2（CI race job）：快照在 r.mu 内生成——此前在后台 goroutine 启动后无锁
// cloneJob(job) 返回，与 execute() 的字段写入构成数据竞争（audit CI 首跑抓到）。
func (r *Registry) Run(kind Kind, trigger Trigger, total int, fn TaskFunc) (*Job, error) {
	if !kind.Valid() {
		return nil, fmt.Errorf("jobs: 未知任务类型 %q", kind)
	}
	if fn == nil {
		return nil, errors.New("jobs: TaskFunc 不能为空")
	}
	if total < 0 {
		total = 0
	}
	r.mu.Lock()
	if r.closed || r.base.Err() != nil {
		r.mu.Unlock()
		return nil, ErrClosed
	}
	if r.runningKinds[kind] {
		r.mu.Unlock()
		return nil, ErrRunning
	}
	job := &Job{
		ID:        r.newIDLocked(kind),
		Kind:      kind,
		Trigger:   trigger,
		State:     StatePending,
		Total:     total,
		StartedAt: r.now(),
	}
	r.jobs = append(r.jobs, job)
	r.byID[job.ID] = job
	r.runningKinds[kind] = true
	// WaitGroup.Add 必须在锁内、且在任何 Wait 观察前完成（Close 同锁串行化，
	// 因此 Close 之后的 Run 必被 ErrClosed 拒绝，Wait 不会与新 Add 竞争）。
	r.jobWG.Add(1)
	if r.extWG != nil {
		r.extWG.Add(1)
	}
	// 快照必须在锁内生成（v2.6.2）：后台 execute() 会以 r.mu 保护地改写 Job 字段，
	// 锁外 cloneJob 读到半更新状态既是数据竞争，也可能返回撕裂快照。
	snap := cloneJob(job)
	r.mu.Unlock()
	r.persist()
	go func() {
		defer r.jobWG.Done()
		if r.extWG != nil {
			defer r.extWG.Done()
		}
		r.execute(job, fn)
	}()
	return snap, nil
}

// Wait 等待所有已启动 Job 收敛；ctx 取消返回 false（有上限——停机不能无限卡住）。
// 应在 Close/根 ctx 取消之后调用（此后不会再有新 Job 登记）。
func (r *Registry) Wait(ctx context.Context) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	go func() {
		r.jobWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// execute Job 后台执行体：抢执行槽 → running → 调任务体 → 终态 + 落盘 + 回调。
func (r *Registry) execute(job *Job, fn TaskFunc) {
	// 1) 并发闸门：等待一个执行槽。根 ctx 取消（停机）在排队期即放弃。
	select {
	case r.sem <- struct{}{}:
	case <-r.base.Done():
		r.finish(job, StateCanceled, "")
		return
	}
	defer func() { <-r.sem }()

	// 2) pending → running。竞态兜底：排队期间 Registry 可能已被 Close。
	ctx, cancel := context.WithCancel(r.base)
	r.mu.Lock()
	if r.closed || r.base.Err() != nil {
		r.mu.Unlock()
		cancel()
		r.finish(job, StateCanceled, "")
		return
	}
	job.State = StateRunning
	job.StartedAt = r.now()
	r.cancels[job.ID] = cancel
	// v2.6.2：登记 cancel 后复检 closed/base（Close 可能在本 goroutine 通过上方
	// closed 检查之后、登记之前发生——那只复制了已有 cancels，会漏掉本 Job）。
	// 命中则清登记并走终态 canceled，避免 Close 后仍有 Job 在跑（与上方检查同语义）。
	if r.closed || r.base.Err() != nil {
		delete(r.cancels, job.ID)
		delete(r.runningKinds, job.Kind)
		r.mu.Unlock()
		cancel()
		r.finish(job, StateCanceled, "")
		return
	}
	r.mu.Unlock()
	defer cancel()
	r.persist()

	// 3) 进度回调：只更新运行中的 Job；计数为累计值，msg 追加 LogTail。
	//    并发任务（余额刷新）可能乱序上报累计值 → 逐字段取单调最大值，避免进度条回退。
	progress := Progress(func(done, total, fail, skip int, msg string) {
		r.mu.Lock()
		if job.State != StateRunning {
			r.mu.Unlock()
			return
		}
		if total > 0 && total > job.Total {
			job.Total = total
		}
		if done > job.Done {
			job.Done = done
		}
		if fail > job.Fail {
			job.Fail = fail
		}
		if skip > job.Skip {
			job.Skip = skip
		}
		if msg != "" {
			appendLogTail(job, msg)
		}
		r.mu.Unlock()
	})

	// 4) 执行任务体（panic 兜底为 failed，不让注册表协程崩掉进程）。
	err := invokeTask(ctx, fn, progress)

	// 5) 终态判定：ctx 取消优先（Cancel/停机都走这里），其次 error，否则 done。
	r.mu.Lock()
	delete(r.cancels, job.ID)
	delete(r.runningKinds, job.Kind)
	state := StateDone
	errMsg := ""
	switch {
	case ctx.Err() != nil:
		state = StateCanceled
	case err != nil:
		state = StateFailed
		errMsg = err.Error()
	}
	r.mu.Unlock()
	r.finish(job, state, errMsg)
}

// invokeTask 调任务体并 recover panic → error（失败信息进 Job.Error/LogTail）。
func invokeTask(ctx context.Context, fn TaskFunc, progress Progress) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("任务执行 panic: %v", p)
		}
	}()
	return fn(ctx, progress)
}

// finish 把 Job 置为终态、落盘并触发 OnFinish 回调（同 kind 单飞在此统一释放，
// 保证「排队的 gate 放弃路径」也不会泄漏 runningKinds 标记）。
func (r *Registry) finish(job *Job, state State, errMsg string) {
	r.mu.Lock()
	// 重复 finish 防御（执行体正常路径只调一次；这里幂等避免状态回退）。
	if job.State == StateDone || job.State == StateFailed || job.State == StateCanceled || job.State == StateInterrupted {
		r.mu.Unlock()
		return
	}
	job.State = state
	job.FinishedAt = r.now()
	if job.StartedAt.IsZero() {
		job.StartedAt = job.FinishedAt
	}
	if errMsg != "" {
		job.Error = errMsg
		appendLogTail(job, errMsg)
	}
	delete(r.cancels, job.ID)
	delete(r.runningKinds, job.Kind)
	r.trimLocked()
	clone := cloneJob(job)
	onFinish := r.onFinish
	r.mu.Unlock()
	r.persist()
	if onFinish != nil {
		onFinish(clone)
	}
}

// Record 追加一条**已完成**的 Job 记录（不运行任务体；用于单号 tasks/run 这类
// 同步执行完毕、只需写历史供面板查看的操作）。返回记录快照。
func (r *Registry) Record(job Job) *Job {
	r.mu.Lock()
	if job.ID == "" {
		kind := job.Kind
		if !kind.Valid() {
			kind = KindTasksRun
			job.Kind = kind
		}
		job.ID = r.newIDLocked(kind)
	}
	if job.StartedAt.IsZero() {
		job.StartedAt = r.now()
	}
	if job.FinishedAt.IsZero() {
		job.FinishedAt = r.now()
	}
	if job.State == "" {
		job.State = StateDone
	}
	rec := cloneJob(&job)
	r.jobs = append(r.jobs, rec)
	r.byID[rec.ID] = rec
	r.trimLocked()
	clone := cloneJob(rec)
	onFinish := r.onFinish
	r.mu.Unlock()
	r.persist()
	if onFinish != nil {
		onFinish(clone)
	}
	return clone
}

// Cancel 取消一个 Job：不存在 → ErrNotFound；仍在排队/运行 → 触发 ctx 取消并返回
// true（终态由执行体异步落为 canceled，轮询 GET /jobs/{id} 可见）；已结束 → false。
func (r *Registry) Cancel(id string) (bool, error) {
	r.mu.Lock()
	job, ok := r.byID[id]
	if !ok {
		r.mu.Unlock()
		return false, ErrNotFound
	}
	if job.State != StatePending && job.State != StateRunning {
		r.mu.Unlock()
		return false, nil
	}
	cancel := r.cancels[id]
	r.mu.Unlock()
	if cancel == nil {
		// pending 且尚未登记 cancel：后台 execute 抢到槽后会先检查根 ctx/closed；
		// 这里没有可直接取消的 ctx，返回 false 让前端提示「尚未开始，稍后重试」。
		return false, nil
	}
	cancel()
	return true, nil
}

// Get 返回 Job 快照；不存在 → ErrNotFound。
func (r *Registry) Get(id string) (*Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneJob(job), nil
}

// List 返回最近 limit 条 Job（新的在前）；limit<=0 回落 20，上限 500。
func (r *Registry) List(limit int) []*Job {
	if limit <= 0 {
		limit = 20
	}
	if limit > 500 {
		limit = 500
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Job, 0, limit)
	for i := len(r.jobs) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, cloneJob(r.jobs[i]))
	}
	return out
}

// Close 停止接受新 Job 并取消所有运行中的 Job（优雅停机；幂等）。
func (r *Registry) Close() {
	r.mu.Lock()
	already := r.closed
	r.closed = true
	cancels := make([]context.CancelFunc, 0, len(r.cancels))
	for _, c := range r.cancels {
		cancels = append(cancels, c)
	}
	r.mu.Unlock()
	if already {
		return
	}
	for _, c := range cancels {
		c()
	}
}

// ---------------------------------------------------------------------------
// 内部：持久化 / 快照 / 裁剪 / LogTail
// ---------------------------------------------------------------------------

// trimLocked 按 retain 裁剪历史：只淘汰已终态的最旧条目；未完成（pending/running）
// 的 Job 永不淘汰（否则进度 API 会丢运行中任务）。调用方需持有 r.mu。
func (r *Registry) trimLocked() {
	if len(r.jobs) <= r.retain {
		return
	}
	// 从旧到新扫描，统计已终态条数；先标记可淘汰的终态条目，直到总数 <= retain。
	finished := 0
	for _, j := range r.jobs {
		if terminal(j.State) {
			finished++
		}
	}
	if finished <= r.retain {
		return
	}
	drop := finished - r.retain
	kept := make([]*Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		if drop > 0 && terminal(j.State) {
			delete(r.byID, j.ID)
			drop--
			continue
		}
		kept = append(kept, j)
	}
	r.jobs = kept
}

// terminal 报告状态是否为终态。
func terminal(s State) bool {
	switch s {
	case StateDone, StateFailed, StateCanceled, StateInterrupted:
		return true
	}
	return false
}

// appendLogTail 追加一条 LogTail（环形裁剪到 LogTailMax）。调用方需持有 r.mu。
func appendLogTail(job *Job, msg string) {
	job.LogTail = append(job.LogTail, msg)
	if len(job.LogTail) > LogTailMax {
		job.LogTail = job.LogTail[len(job.LogTail)-LogTailMax:]
	}
}

// cloneJob 深拷贝 Job（LogTail 独立底层数组），避免把内部可变状态泄漏给调用方。
//
// 并发契约（v2.6.2）：**读取共享 Job（r.jobs / r.byID 中的内部指针）时必须持有
// r.mu**；本函数不做加锁，因为所有生产调用点（Run/finish/Record/persist/Get/List）
// 都在 r.mu 内调用。唯一例外是 Record 传入的未共享局部副本（&job）。若将来新增
// 锁外调用点，必须改为在 r.mu 内读取后再拷贝。
func cloneJob(j *Job) *Job {
	if j == nil {
		return nil
	}
	c := *j
	if len(j.LogTail) > 0 {
		c.LogTail = append([]string(nil), j.LogTail...)
	}
	return &c
}

// persist 原子写 jobs.json（0600；保留最近 retain 条）。写失败只 WARN——Job 历史
// 是观测数据，不能反过来影响任务执行。
func (r *Registry) persist() {
	if r.path == "" {
		return
	}
	r.persistMu.Lock()
	defer r.persistMu.Unlock()

	r.mu.Lock()
	snapshotJobs := make([]*Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		snapshotJobs = append(snapshotJobs, cloneJob(j))
	}
	r.mu.Unlock()

	raw, err := json.MarshalIndent(persisted{Version: 1, Jobs: snapshotJobs}, "", "  ")
	if err != nil {
		log.Printf("WARN: jobs: 序列化 jobs.json 失败: %v", err)
		return
	}
	if dir := filepath.Dir(r.path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("WARN: jobs: 写 jobs.json.tmp 失败: %v", err)
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		log.Printf("WARN: jobs: 替换 jobs.json 失败: %v", err)
		_ = os.Remove(tmp)
	}
}

// load 加载 jobs.json：损坏 → WARN 后从空开始（不 fatal）；上次 pending/running
// （进程重启时未跑完）→ interrupted，并保留原计数/时间供面板显示「上次中断」。
func (r *Registry) load() {
	if r.path == "" {
		return
	}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("WARN: jobs: 读取 %s 失败（从空历史开始）: %v", r.path, err)
		}
		return
	}
	var f persisted
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Printf("WARN: jobs: 解析 %s 失败（从空历史开始）: %v", r.path, err)
		return
	}
	for _, j := range f.Jobs {
		if j == nil || j.ID == "" {
			continue
		}
		if j.State == StateRunning || j.State == StatePending {
			j.State = StateInterrupted
			j.FinishedAt = r.now()
			if j.Error == "" {
				j.Error = "进程重启，任务中断"
			}
		}
		r.jobs = append(r.jobs, j)
		r.byID[j.ID] = j
	}
	r.trimLocked()
}
