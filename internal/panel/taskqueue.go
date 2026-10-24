// taskqueue.go 成长任务执行队列：状态/触发/逐账号执行/进度（v2.6.1 拆分自 taskcenter.go，纯移动）。
package panel

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// ---------------------------------------------------------------------------
// 执行队列
// ---------------------------------------------------------------------------

// queueItem 队列执行单元。
type queueItem struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Kind     string `json:"kind"` // growth
	Code     string `json:"code"`
	Status   string `json:"status"` // pending | running | done | skipped | error
	Message  string `json:"message,omitempty"`
}

// queueState 队列运行状态。Seq 每次启动 +1——前端只渲染"自己启动的那一轮"，
// 执行结束后的残留 items 不会覆盖后续的扫描结果视图。
type queueState struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	trigger   string // 本轮的触发方式（manual/schedule），写任务历史用
	items     []queueItem
	conc      int
	seq       int
}

// Panel 队列字段在 Panel 结构体上（panel.go）由 initQueue 惰性初始化；
// 这里集中访问器，避免改动 New 构造链。
func (p *Panel) queue() *queueState {
	p.queueOnce.Do(func() { p.q = &queueState{} })
	return p.q
}

// startGrowthQueue 扫描全部账号待办并启动队列（HTTP「执行全部待办」与调度器
// growth 时点共用核心）。返回 (started, total, seq, msg)：seq==-1 表示队列
// 已在执行（冲突）；started=false 时 msg 为无可执行待办的说明。并发夹取
// [1,4]；growth 开关同 HTTP 入参语义；trigger 记入任务历史（manual/schedule）。
func (p *Panel) startGrowthQueue(concurrency int, growth bool, trigger string) (started bool, total int, seq int, msg string) {
	return p.startGrowthQueueCtx(context.Background(), concurrency, growth, trigger)
}

// startGrowthQueueCtx 同 startGrowthQueue，但扫描阶段受 ctx 取消（HTTP 调用方传
// r.Context()；调度器回调无 ctx 用 Background）。
// v2.6.0 P0：扫描阶段改固定 worker 池（fanOutConcurrency），不再每账号一个
// goroutine；ctx 取消后立即停止派发，并回滚 running 占位（避免队列卡死）。
func (p *Panel) startGrowthQueueCtx(ctx context.Context, concurrency int, growth bool, trigger string) (started bool, total int, seq int, msg string) {
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 4 {
		concurrency = 4
	}
	if trigger == "" {
		trigger = taskTriggerManual
	}
	q := p.queue()
	q.mu.Lock()
	if q.running {
		q.mu.Unlock()
		return false, 0, -1, "队列正在执行中（可在任务中心查看进度）"
	}
	// 先占位：扫描（数秒级网络耗时）期间若并发再次触发，直接命中上面的 running
	// 判拒，避免两个 goroutine 同时启动互相覆盖 q.items/q.seq。无待办时回滚。
	q.running = true
	q.startedAt = time.Now()
	q.trigger = trigger
	q.mu.Unlock()

	// 扫描待办（复用扫描逻辑的拉取部分）：先串行筛候选，再有界并发拉任务列表。
	states := p.poolSnapshot()
	type queueCandidate struct{ a *auth.Auth }
	candidates := make([]queueCandidate, 0, len(states))
	for i := range states {
		st := &states[i]
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		// D4 门控：global 账号无 CN 成长任务体系，不发起任何上游调用。
		if a.IsGlobal() {
			continue
		}
		// 企业版门控：同上（企业版无个人成长体系，任务端点上游一律 403）。
		// 企业切换会话（token_source=enterprise_switch）同跳：任务列表读得到，
		// 但执行后行为事件被上游静默丢弃（200 不计分）——每项都是零收益白跑
		// （含 expert_5 等真实对话配额），故不入队并给出重登指引（见
		// auth.IsEnterpriseSession）。
		if a.IsEnterprise() {
			continue
		}
		if a.IsEnterpriseSession() {
			log.Printf("panel: WARN 队列跳过 uid=%s nickname=%q：凭证为企业切换会话（token_source=enterprise_switch），成长任务行为事件会被上游静默丢弃；请移除该账号，在浏览器退出企业切换（个人身份）后重新登录", a.UID, a.Nickname)
			continue
		}
		candidates = append(candidates, queueCandidate{a: a})
	}

	var accts []queueAccount
	var mu sync.Mutex
	scanConc := p.fanOutConcurrency()
	logFanOut("成长队列扫描", len(candidates), scanConc)
	fanOutAccounts(ctx, len(candidates), scanConc, func(_ context.Context, i int) {
		a := candidates[i].a
		one := queueAccount{a: a}
		if growth {
			if tasks, err := p.cfg.Upstream.ListTasks(a); err == nil {
				for _, t := range tasks {
					if growthPending(t) {
						one.grow = append(one.grow, t)
					}
				}
				// 合并小程序口径待办（与 tasksScanAll 同口径：mp 列表是默认口径
				// 超集，按 code 去重；失败静默）。此前此处漏合并——扫描显示
				// mp 待办而队列报"无可执行待办"。
				if mpTasks, mpErr := p.cfg.Upstream.ListTasksMP(a); mpErr == nil {
					seen := map[string]bool{}
					for _, t := range one.grow {
						seen[t.TaskCode] = true
					}
					for _, t := range mpTasks {
						if growthPending(t) && !seen[t.TaskCode] {
							one.grow = append(one.grow, t)
						}
					}
				}
				sort.Slice(one.grow, func(i, j int) bool { // 按 autoActions 顺序（依赖前置）
					return autoActionIndex(one.grow[i].TaskCode) < autoActionIndex(one.grow[j].TaskCode)
				})
			}
		}
		if len(one.grow) > 0 {
			mu.Lock()
			accts = append(accts, one)
			mu.Unlock()
		}
	})
	// ctx 取消（客户端断开/服务停机）：此时扫描未完成、队列尚未启动，回滚占位。
	if ctxErr := ctx.Err(); ctxErr != nil {
		q.mu.Lock()
		q.running = false
		q.startedAt = time.Time{}
		q.mu.Unlock()
		return false, 0, 0, "扫描已取消: " + ctxErr.Error()
	}

	// 组装队列（账号分组，保持顺序）。
	var items []queueItem
	for _, one := range accts {
		for _, t := range one.grow {
			items = append(items, queueItem{UID: one.a.UID, Nickname: one.a.Nickname, Kind: "growth", Code: t.TaskCode, Status: "pending"})
		}
	}
	if len(items) == 0 {
		log.Printf("panel: 队列启动：无可执行待办（全部账号任务已完成）")
		q.mu.Lock()
		q.running = false
		q.startedAt = time.Time{}
		q.mu.Unlock()
		return false, 0, 0, "全部账号没有待办任务"
	}

	q.mu.Lock()
	q.items = items
	q.conc = concurrency
	q.seq++
	seq = q.seq
	q.mu.Unlock()

	go p.runQueueItems(accts, items, concurrency, trigger)
	log.Printf("panel: 队列启动：%d 项（并发 %d，成长 %v）", len(items), concurrency, growth)
	return true, len(items), seq, ""
}

// RunGrowthQueueOnce 调度器 growth 时点回调（sch.SetGrowthHook 挂载）：与
// 「执行全部待办」按钮完全同管线（成长，串行并发 1）。Sequential 族
// 每日零点解锁一环，此前只能手动扫描推进；此回调让链条每天自动走一环。
// 异步执行（startGrowthQueue 启动 goroutine 即返），已在跑/无待办安全跳过。
// v2.2.0 #9：队列执行历史 trigger=schedule（区分手动「执行全部待办」）。
func (p *Panel) RunGrowthQueueOnce() {
	started, total, _, _ := p.startGrowthQueue(1, true, taskTriggerSchedule)
	if started {
		log.Printf("panel: 定时成长任务队列已启动（%d 项）", total)
	}
}

// runQueueItems 队列执行主体：按账号分组，账号内串行（per-account 锁），
// 账号间并发（信号量）。每项结果写回队列状态。
//
// v2.2.0 #9：收尾按 items 状态统计并写一条任务执行历史（kind=tasks_run，
// trigger 由调用方传入 manual/schedule）——这是 panel 侧能拿到精确逐项计数的
// 任务链路之一。
func (p *Panel) runQueueItems(accts []queueAccount, items []queueItem, concurrency int, trigger string) {
	q := p.queue()
	started := time.Now()
	defer func() {
		q.mu.Lock()
		q.running = false
		total := len(q.items)
		done, fail, skip := 0, 0, 0
		for _, it := range q.items {
			switch it.Status {
			case "done":
				done++
			case "error":
				fail++
			case "skipped":
				skip++
			}
		}
		q.mu.Unlock()
		log.Printf("panel: 队列执行结束（共 %d 项）", total)
		p.recordTaskRun(TaskRun{
			Kind: taskKindTasksRun, Trigger: trigger,
			StartedAt: started.Unix(), FinishedAt: time.Now().Unix(),
			Total: total, Done: done, Fail: fail, Skip: skip,
		})
	}()

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, one := range accts {
		wg.Add(1)
		go func(one queueAccount) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// per-account 互斥：与单任务/一键完成共用一把锁。
			if !p.tryLockAccount(one.a.UID) {
				p.queueSet(q, one.a.UID, func(it *queueItem) {
					it.Status, it.Message = "skipped", "该账号有其它任务动作在执行，跳过"
				})
				return
			}
			defer p.unlockAccount(one.a.UID)
			// 前置：批量接受尚未接受的任务。上游对 not_accepted 的任务不计数——
			// 面板「一键完成」一直有这步，队列路径此前漏了（表现为上报 200 但进度
			// 一直 not_accepted、无法领奖）。失败不阻塞（行为事件才是进度判据）。
			if accepted := p.acceptPendingTasks(one.a); accepted > 0 {
				time.Sleep(reportGap) // 给上游状态流转留时间
			}
			for i := range q.items {
				uid, kind, code := q.snapshotAt(i)
				if uid != one.a.UID {
					continue
				}
				p.queueMarkAt(i, "running", "")
				var msg string
				var err error
				switch kind {
				case "growth":
					msg, err = p.runGrowthQueued(one.a, code)
				}
				if err != nil {
					p.queueMarkAt(i, "error", err.Error())
				} else {
					p.queueMarkAt(i, "done", msg)
				}
				time.Sleep(reportGap) // 项间节流
			}
		}(one)
	}
	wg.Wait()
}

// queueAccount 队列执行的账号单元（runQueueItems 参数）。
type queueAccount struct {
	a    *auth.Auth
	grow []upstream.Task
}

// snapshotAt 锁内读条目三元组（避免锁外持有指针）。
func (q *queueState) snapshotAt(i int) (uid, kind, code string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items[i].UID, q.items[i].Kind, q.items[i].Code
}

// queueMarkAt 按索引更新队列条目状态（条目数组固定不再增删）。
func (p *Panel) queueMarkAt(i int, status, msg string) {
	q := p.queue()
	q.mu.Lock()
	q.items[i].Status, q.items[i].Message = status, msg
	q.mu.Unlock()
}

// queueSet 按 uid 批量改状态。
func (p *Panel) queueSet(q *queueState, uid string, fn func(*queueItem)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].UID == uid {
			fn(&q.items[i])
		}
	}
}

// acceptPendingTasks 批量接受该账号未接受的任务，返回接受的个数（失败返回 0 不阻塞）。
func (p *Panel) acceptPendingTasks(a *auth.Auth) int {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return 0
	}
	var codes []string
	for _, t := range tasks {
		if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
			codes = append(codes, t.TaskCode)
		}
	}
	if len(codes) == 0 {
		return 0
	}
	if err := p.cfg.Upstream.AcceptTasks(a, codes); err != nil {
		log.Printf("panel: 队列 accept uid=%s: %v（不阻塞）", a.UID, err)
		return 0
	}
	log.Printf("panel: 队列 accept uid=%s: 已接受 %d 个任务", a.UID, len(codes))
	return len(codes)
}

// runGrowthQueued 执行单个成长任务（动作 + 回读 + 自动领奖；与
// accountTaskAuto 同语义，结果以文字返回）。
func (p *Panel) runGrowthQueued(a *auth.Auth, code string) (string, error) {
	act := autoActionFor(code)
	if act == nil {
		return "", fmt.Errorf("任务 %s 无自动动作", code)
	}
	// taskByCode 已双口径（mp 专属码自动回落 mp 列表）。
	before, err := p.taskByCode(a, code)
	if err != nil {
		return "", err
	}
	if before == nil {
		return "该账号无此任务", nil
	}
	isMP := isMPTaskCode(code)
	if before.Claimed {
		return "已完成（已领取）", nil
	}
	msg, err := act.run(p, a)
	if err != nil {
		return "", err
	}
	var after *upstream.Task
	if isMP {
		after, _ = p.taskByCodeMP(a, code)
	} else {
		after, _ = p.taskByCodeWaiting(a, code)
	}
	if after != nil && after.Claimable {
		var credit, energy int64
		var cerr error
		if isMP {
			credit, energy, cerr = p.cfg.Upstream.ClaimRewardMP(a, code)
		} else {
			credit, energy, cerr = p.cfg.Upstream.ClaimReward(a, code)
		}
		if cerr == nil && (credit > 0 || energy > 0) {
			msg += fmt.Sprintf("；自动领奖 +%d 分 +%d 能", credit, energy)
		}
	}
	if after != nil {
		msg += "（进度 " + taskProgressText(after) + "）"
	}
	log.Printf("panel: 队列 growth uid=%s code=%s: %s", a.UID, code, msg)
	return msg, nil
}
