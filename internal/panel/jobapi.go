// jobapi.go 全池任务 Job API（v2.3.0 #10）与 #9 任务历史接线。
//
// 端点契约（JSON；均需面板鉴权 Bearer）：
//
//	GET  /panel/api/jobs?limit=20     → {"ok":true,"jobs":[Job...]}（新的在前）
//	GET  /panel/api/jobs/{id}         → {"ok":true,"job":Job}（不存在 404）
//	POST /panel/api/jobs/{kind}/run   → 200 {"ok":true,"started":true,"job_id":"..."}
//	                                    409 {"ok":false,"error":"...正在执行中..."}
//	POST /panel/api/jobs/{id}/cancel  → {"ok":true,"canceled":true|false}（不存在 404）
//
// 旧批量端点 checkin_all/travel_all/activity_all/keepalive_all/balance_all 保留
// ok/started 字段，额外返回 job_id（Registry 未装配时退回 v2.2 的异步实现、无
// job_id——嵌入式/老测试兼容）。409 语义不变：先占 scheduler 同 kind 单飞 gate。
//
// 设计要点：
//   - scheduler gate 与 jobs.Registry 同 kind 单飞是两层保护：gate 保证「手动 vs
//     定时」不撞车，Registry 保证手动重复点击返回同一 409 且 Job 状态可查；
//   - 进度：scheduler.Run*Ctx 逐号回 cumulative(done,total,fail,skip,msg)，其中
//     done=已处理账号数（不含 skip；含 fail），fail/skip 为其中失败/跳过数；
//   - Job 持久化 data/jobs.json（与 state 同目录），重启后上次 running→interrupted。
package panel

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/jobs"
	"github.com/lei-rr/workbuddy2api-pro/internal/scheduler"
)

// jobsDefaultListLimit / jobsMaxListLimit GET /jobs 默认与上限条数。
const (
	jobsDefaultListLimit = 20
	jobsMaxListLimit     = 500
)

// jobSpec 一类可执行 Job 的元信息 + 任务体。
type jobSpec struct {
	kind jobs.Kind
	gate scheduler.TaskKind
	// run 任务体：ctx 随取消/停机取消，progress 可为 nil。
	run func(ctx context.Context, progress scheduler.ProgressFunc) error
	// conflict 409 文案（与旧端点保持一致，前端 toast 直接展示）。
	conflict string
}

// jobSpecFor 返回 kind 对应的执行规格；未知/不可执行 kind 返回 false。
// tasks_run 属「单号同步任务」历史，不能经 /jobs/{kind}/run 触发（不存在全池变体）。
func (p *Panel) jobSpecFor(kind jobs.Kind) (jobSpec, bool) {
	sch := p.cfg.Scheduler
	if sch == nil {
		return jobSpec{}, false
	}
	switch kind {
	case jobs.KindCheckin:
		return jobSpec{
			kind: jobs.KindCheckin, gate: scheduler.TaskCheckin,
			conflict: "全量签到正在执行中，请等本轮结束后再试",
			run:      func(ctx context.Context, prog scheduler.ProgressFunc) error { return sch.RunCheckinCtx(ctx, prog) },
		}, true
	case jobs.KindTravel:
		return jobSpec{
			kind: jobs.KindTravel, gate: scheduler.TaskTravel,
			conflict: "全量旅行巡检正在执行中，请等本轮结束后再试",
			run:      func(ctx context.Context, prog scheduler.ProgressFunc) error { return sch.RunTravelCtx(ctx, prog) },
		}, true
	case jobs.KindActivity:
		return jobSpec{
			kind: jobs.KindActivity, gate: scheduler.TaskActivity,
			conflict: "全量活跃上报正在执行中，请等本轮结束后再试",
			run:      func(ctx context.Context, prog scheduler.ProgressFunc) error { return sch.RunActivityCtx(ctx, prog) },
		}, true
	case jobs.KindKeepalive:
		return jobSpec{
			kind: jobs.KindKeepalive, gate: scheduler.TaskKeepalive,
			conflict: "全量 token 保活正在执行中，请等本轮结束后再试",
			run:      func(ctx context.Context, prog scheduler.ProgressFunc) error { return sch.RunKeepaliveCtx(ctx, prog) },
		}, true
	case jobs.KindBalance:
		return jobSpec{
			kind: jobs.KindBalance, gate: scheduler.TaskBalance,
			conflict: "全量余额刷新正在执行中，请等本轮结束后再试",
			run: func(ctx context.Context, prog scheduler.ProgressFunc) error {
				err := sch.RunBalanceRefreshCtx(ctx, prog)
				// 与旧 balance_all 行为保持一致：刷新后同步昵称（issue #94）。
				p.syncNicknames()
				return err
			},
		}, true
	}
	return jobSpec{}, false
}

// triggerJob 起一个全量任务 Job：先占 scheduler 同 kind gate（409 语义不变），
// 再注册 Registry（有 job_id/进度/取消/持久化）。
//
// 返回 (jobID, started)：started=false = 同类任务正在执行（409）。Registry 未装配
// 时退回 v2.2 的 triggerBatchTask（异步执行 + task-runs.jsonl 历史，无 job_id）。
func (p *Panel) triggerJob(spec jobSpec) (jobID string, started bool) {
	sch := p.cfg.Scheduler
	if sch == nil {
		return "", false
	}
	if !sch.BeginTask(spec.gate) {
		return "", false
	}
	if p.cfg.Jobs == nil {
		// 兼容路径：无 Job Registry（嵌入式/老测试）→ 保持 v2.2 异步语义。
		b := p.beginBatchTaskRun(string(spec.kind), taskTriggerManual)
		go func() {
			defer sch.EndTask(spec.gate)
			_ = spec.run(context.Background(), nil)
			p.finishBatchTaskRun(b)
		}()
		return "", true
	}
	total := 0
	if p.cfg.Pool != nil {
		total = len(p.cfg.Pool.List())
	}
	startedAt := time.Now()
	job, err := p.cfg.Jobs.Run(spec.kind, jobs.TriggerManual, total,
		func(ctx context.Context, prog jobs.Progress) error {
			defer sch.EndTask(spec.gate)
			// 进度旁路记录终态计数，供 #9 task-runs.jsonl 历史（旧接口）复用。
			// 余额刷新等并发任务体会并发回调 progress → 用 atomic 汇总，最终值取
			// 最后一次上报（所有 worker 完成后的累计值即全量统计）。
			var lastDone, lastTotal, lastFail, lastSkip atomic.Int64
			wrapped := scheduler.ProgressFunc(func(done, t, fail, skip int, msg string) {
				lastDone.Store(int64(done))
				lastTotal.Store(int64(t))
				lastFail.Store(int64(fail))
				lastSkip.Store(int64(skip))
				if prog != nil {
					prog(done, t, fail, skip, msg)
				}
			})
			runErr := spec.run(ctx, wrapped)
			success := int(lastDone.Load() - lastFail.Load())
			if success < 0 {
				success = 0
			}
			p.recordTaskRun(TaskRun{
				Kind:       string(spec.kind),
				Trigger:    taskTriggerManual,
				StartedAt:  startedAt.Unix(),
				FinishedAt: time.Now().Unix(),
				Total:      int(lastTotal.Load()),
				Done:       success,
				Fail:       int(lastFail.Load()),
				Skip:       int(lastSkip.Load()),
			})
			return runErr
		})
	if err != nil {
		sch.EndTask(spec.gate)
		return "", false
	}
	return job.ID, true
}

// batchStartedBody 旧批量端点响应体：保留 ok/started 旧字段，附加 job_id（可为空）。
func batchStartedBody(jobID string) map[string]any {
	body := map[string]any{"ok": true, "started": true}
	if jobID != "" {
		body["job_id"] = jobID
	}
	return body
}

// ---------------------------------------------------------------------------
// Job API handlers
// ---------------------------------------------------------------------------

// jobsList GET /panel/api/jobs?limit=20：最近 Job 列表（新的在前）。
func (p *Panel) jobsList(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Jobs == nil {
		writeErr(w, http.StatusNotImplemented, "jobs registry not available")
		return
	}
	limit := jobsDefaultListLimit
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > jobsMaxListLimit {
		limit = jobsMaxListLimit
	}
	list := p.cfg.Jobs.List(limit)
	if list == nil {
		list = []*jobs.Job{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "jobs": list})
}

// jobsGet GET /panel/api/jobs/{id}：单个 Job（进度轮询/取消后确认）。
func (p *Panel) jobsGet(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Jobs == nil {
		writeErr(w, http.StatusNotImplemented, "jobs registry not available")
		return
	}
	job, err := p.cfg.Jobs.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": job})
}

// jobsRun POST /panel/api/jobs/{kind}/run：启动一类全量任务 Job。
func (p *Panel) jobsRun(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Jobs == nil {
		writeErr(w, http.StatusNotImplemented, "jobs registry not available")
		return
	}
	kind := jobs.Kind(strings.TrimSpace(r.PathValue("kind")))
	if !kind.Valid() {
		writeErr(w, http.StatusBadRequest, "unknown job kind: "+string(kind)+"（可选 checkin/travel/activity/keepalive/balance）")
		return
	}
	spec, ok := p.jobSpecFor(kind)
	if !ok {
		if p.cfg.Scheduler == nil {
			writeErr(w, http.StatusNotImplemented, "scheduler not available")
			return
		}
		writeErr(w, http.StatusBadRequest, "job kind not runnable: "+string(kind))
		return
	}
	jobID, started := p.triggerJob(spec)
	if !started {
		writeBatchConflict(w, spec.conflict)
		return
	}
	log.Printf("panel: 手动全量任务已触发 kind=%s job=%s（后台执行）", spec.kind, jobID)
	writeJSON(w, http.StatusOK, batchStartedBody(jobID))
}

// jobsCancel POST /panel/api/jobs/{id}/cancel：取消运行中 Job（不存在 404）。
func (p *Panel) jobsCancel(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Jobs == nil {
		writeErr(w, http.StatusNotImplemented, "jobs registry not available")
		return
	}
	canceled, err := p.cfg.Jobs.Cancel(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	log.Printf("panel: job 取消请求 id=%s canceled=%v", r.PathValue("id"), canceled)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "canceled": canceled})
}

// ---------------------------------------------------------------------------
// #9 任务历史接线：单号 tasks/run 同步完成后写一条 jobs 历史（kind=tasks_run）
// ---------------------------------------------------------------------------

// withJobHistory 包装单号「做任务」handler：解响应后向 Job Registry 追加一条
// 已完成记录（成功/失败都记），不改变同步响应契约。
// 只解析 summary/results，不读取/记录任何凭证。
func (p *Panel) withJobHistory(kind jobs.Kind, trigger jobs.Trigger, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p.cfg.Jobs == nil {
			next(w, r)
			return
		}
		ar := &auditResponse{ResponseWriter: w, limit: taskRunBodyMax}
		started := time.Now()
		next(ar, r)
		ok, uid, detail := ar.result()
		var body struct {
			Summary *struct {
				Total   int `json:"total"`
				Done    int `json:"done"`
				Skipped int `json:"skipped"`
				Error   int `json:"error"`
			} `json:"summary"`
			Results []json.RawMessage `json:"results"`
		}
		_ = json.Unmarshal(ar.body.Bytes(), &body)
		total, done, fail, skip := 0, 0, 0, 0
		switch {
		case body.Summary != nil:
			total = body.Summary.Total
			done = body.Summary.Done
			fail = body.Summary.Error
			skip = body.Summary.Skipped
		case body.Results != nil:
			total = len(body.Results)
			done = total
		}
		state := jobs.StateDone
		errMsg := ""
		if !ok {
			state = jobs.StateFailed
			errMsg = truncateRunes(detail, 200)
		}
		logTail := make([]string, 0, 2)
		if uid != "" {
			logTail = append(logTail, "uid="+shortUID(uid))
		}
		if errMsg != "" {
			logTail = append(logTail, errMsg)
		}
		p.cfg.Jobs.Record(jobs.Job{
			Kind:       kind,
			Trigger:    trigger,
			State:      state,
			Total:      total,
			Done:       done,
			Fail:       fail,
			Skip:       skip,
			StartedAt:  started,
			FinishedAt: time.Now(),
			Error:      errMsg,
			LogTail:    logTail,
		})
	}
}

// shortUID 脱敏短 uid（通知/日志用；与 notify 的 uid8 约定一致）。
func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
