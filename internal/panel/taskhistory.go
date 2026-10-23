// taskhistory.go 任务执行历史持久化（v2.2.0 #9 后端）：每轮任务结束追加一条
// data/task-runs.jsonl，供 GET /panel/api/task_runs 查询（面板「任务执行历史」只读表）。
//
// 集成范围（panel 侧可拿到统计的链路；不改 internal/scheduler）：
//   - 单账号「做任务」POST /accounts/{uid}/tasks/run（逐项 summary，中间件捕获）；
//   - 单账号签到/余额刷新（成功/失败/跳过）；
//   - 全量手动批量（checkin_all/travel_all/activity_all/keepalive_all/balance_all）：
//     panel 侧只能拿到「触发时刻账号数 + 起止时间」，调度器内部的逐号统计拿不到 →
//     done/fail/skip 留空（JSON 缺省 = 0），这是 v2.2.0 的已知边界；
//   - 成长任务执行队列（手动「执行全部待办」/ 调度器 growth 时点）：
//     队列结束后按条目状态统计 done/fail/skip；
//   - 任务补领清扫（catch_up，scheduler ticker 驱动）：按 TaskSweepStats 统计。
//
// 定时任务（cron 的签到/旅行/活跃/保活/余额）在 scheduler 内部执行，panel 没有
// 收尾回调（本任务约束不改 scheduler）——其历史需后续 wave 在 scheduler 加运行观察者。
package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/scheduler"
)

const (
	// taskRunsFileName 任务历史文件名（与 state 文件同目录）。
	taskRunsFileName = "task-runs.jsonl"
	// taskRunsDefaultLimit / taskRunsMaxLimit 查询默认与上限条数。
	taskRunsDefaultLimit = 100
	taskRunsMaxLimit     = 1000
	// taskRunBodyMax 「做任务」中间件捕获响应体上限（summary 在响应前部，足够）。
	taskRunBodyMax = 8192
)

// 任务历史 kind 枚举（前端 TASK_KIND_LABEL 映射；稳定契约，只加不改名）。
const (
	taskKindCheckin   = "checkin"
	taskKindTravel    = "travel"
	taskKindActivity  = "activity"
	taskKindKeepalive = "keepalive"
	taskKindBalance   = "balance"
	taskKindTasksRun  = "tasks_run"
	taskKindCatchUp   = "catch_up"
)

// 任务历史 trigger 枚举。
const (
	taskTriggerManual   = "manual"   // 面板手动触发
	taskTriggerSchedule = "schedule" // 定时器/调度器触发
)

// TaskRun 一条任务执行历史（JSONL 单行；GET /panel/api/task_runs 原样倒序返回）。
//
// 字段口径（与前端表格列一一对应）：
//   - id：`<unix毫秒>-<进程内序号>`，同毫秒多次记录也唯一、可按时间排序；
//   - kind / trigger：见上方枚举；
//   - uid：仅单账号任务有值（批量任务为空，省略）；
//   - started_at / finished_at：unix 秒；
//   - total / done / fail / skip：本轮涉及的账号或任务项数、成功/失败/跳过计数；
//     批量手动任务拿不到调度器逐号统计时这些键省略（JSON 缺省，前端显示 0），
//     不写 -1 之类的哨兵值以免污染求和与展示；
//   - error：执行级错误说明（任务项失败数见 fail），正常为空省略。
type TaskRun struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Trigger    string `json:"trigger"`
	UID        string `json:"uid,omitempty"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at"`
	Total      int    `json:"total,omitempty"`
	Done       int    `json:"done,omitempty"`
	Fail       int    `json:"fail,omitempty"`
	Skip       int    `json:"skip,omitempty"`
	Error      string `json:"error,omitempty"`
}

// taskRunSeq 进程内序号：保证同一毫秒内多条记录 id 唯一。
var taskRunSeq atomic.Uint64

// newTaskRunID 生成任务历史 id（毫秒时间戳 + 进程内序号，无第三方依赖）。
func newTaskRunID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixMilli(), taskRunSeq.Add(1))
}

// recordTaskRun 追加一条任务历史（写失败只 WARN，不影响任务执行）。
func (p *Panel) recordTaskRun(tr TaskRun) {
	if p.taskRunsLog == nil {
		return
	}
	if tr.ID == "" {
		tr.ID = newTaskRunID()
	}
	p.taskRunsLog.append(tr)
}

// taskRunsHandler 返回最近 N 条任务执行历史（新的在前）；文件不存在 → []。
func (p *Panel) taskRunsHandler(w http.ResponseWriter, r *http.Request) {
	limit := tailLimit(r, taskRunsDefaultLimit, taskRunsMaxLimit)
	runs, err := p.taskRunsLog.readLast(limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read task runs: "+err.Error())
		return
	}
	if runs == nil {
		runs = []json.RawMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "runs": runs, "limit": limit})
}

// withTaskRunCapture 包装单账号「做任务」handler：从成功响应 JSON 的 summary 提取
// 逐项计数，落一条 kind=tasks_run 历史（v2.2.0 #9）。
//
// 只在实际执行完成（HTTP 2xx + ok:true + summary 存在）时记录：门控拒绝（200+ok:false）
// 不记；超时 504 也不记（后台流水线仍在跑，面板侧拿不到最终计数）。
func (p *Panel) withTaskRunCapture(kind, trigger string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ar := &auditResponse{ResponseWriter: w, limit: taskRunBodyMax}
		started := time.Now()
		next(ar, r)
		if ar.status < 200 || ar.status >= 300 {
			return
		}
		var body struct {
			OK      *bool  `json:"ok"`
			UID     string `json:"uid"`
			Summary *struct {
				Total   int `json:"total"`
				Done    int `json:"done"`
				Skipped int `json:"skipped"`
				Error   int `json:"error"`
			} `json:"summary"`
		}
		if json.Unmarshal(ar.body.Bytes(), &body) != nil || body.OK == nil || !*body.OK || body.Summary == nil {
			return
		}
		p.recordTaskRun(TaskRun{
			Kind:       kind,
			Trigger:    trigger,
			UID:        body.UID,
			StartedAt:  started.Unix(),
			FinishedAt: time.Now().Unix(),
			Total:      body.Summary.Total,
			Done:       body.Summary.Done,
			Fail:       body.Summary.Error,
			Skip:       body.Summary.Skipped,
		})
	}
}

// batchTaskRun 一次全量批量任务的开始上下文（结束后写历史用）。
type batchTaskRun struct {
	kind    string
	trigger string
	started time.Time
	total   int
}

// beginBatchTaskRun 记录批量任务开始信息；total = 触发时刻池内账号数（近似口径，
// 调度器内部还会按 disabled/global/enterprise 等条件过滤，面板侧拿不到精确值）。
func (p *Panel) beginBatchTaskRun(kind, trigger string) batchTaskRun {
	total := 0
	if p.cfg.Pool != nil {
		total = len(p.cfg.Pool.List())
	}
	return batchTaskRun{kind: kind, trigger: trigger, started: time.Now(), total: total}
}

// finishBatchTaskRun 批量任务执行结束写一条历史：只填「触发时刻总数 + 起止时间」，
// done/fail/skip 留空（调度器内部逐号统计 panel 侧不可得；见文件头说明）。
func (p *Panel) finishBatchTaskRun(b batchTaskRun) {
	p.recordTaskRun(TaskRun{
		Kind:       b.kind,
		Trigger:    b.trigger,
		StartedAt:  b.started.Unix(),
		FinishedAt: time.Now().Unix(),
		Total:      b.total,
	})
}

// triggerBatchTask 异步触发一次全量任务并写任务历史（v2.2.0 #9 集成点）：
//
//   - 先占调度器同 kind 单飞 gate（v2.0.1 #10）；已在跑返回 false，调用方回 409；
//   - 占位成功 → 后台 goroutine 执行 run，结束后记一条 trigger=manual 的历史，
//     再释放 gate（顺序保证「历史写完才算结束」，避免前端看到 running=false 却
//     查不到本轮记录的时间窗）。
//
// run 不能是 nil；调用方保证 sch 非 nil（上层已判）。
func (p *Panel) triggerBatchTask(sch *scheduler.Scheduler, gate scheduler.TaskKind, kind string, run func()) bool {
	if sch == nil || run == nil {
		return false
	}
	if !sch.BeginTask(gate) {
		return false
	}
	b := p.beginBatchTaskRun(kind, taskTriggerManual)
	go func() {
		defer sch.EndTask(gate)
		run()
		p.finishBatchTaskRun(b)
	}()
	return true
}
