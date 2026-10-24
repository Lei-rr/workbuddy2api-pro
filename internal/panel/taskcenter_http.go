// taskcenter_http.go 任务中心 HTTP handler（v2.6.1 拆分自 taskcenter.go，纯移动；响应契约不变）。
package panel

import (
	"context"
	"log"
	"net/http"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// tasksScanAll 扫描全部账号：成长任务（未完成+可自动化，含 mp 口径合并）。
// 只读操作；v2.6.0 P0：账号级拉取改为固定 worker 池（默认 8、可配），
// 10k 账号不再瞬时起 1 万 goroutine；请求取消（ctx）后不再派发新账号。
func (p *Panel) tasksScanAll(w http.ResponseWriter, r *http.Request) {
	states := p.poolSnapshot()
	items := make([]scanAccountItem, len(states))
	concurrency := p.fanOutConcurrency()
	logFanOut("任务扫描", len(states), concurrency)
	fanOutAccounts(r.Context(), len(states), concurrency, func(_ context.Context, i int) {
		st := &states[i]
		if st.Disabled {
			return
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			return
		}
		it := &items[i]
		it.UID, it.Nickname = st.UID, a.Nickname
		// D4 门控：global 账号无 CN 成长任务体系，不发起任何上游调用。
		if a.IsGlobal() {
			return
		}
		// 企业版门控：同上——企业版无个人成长体系（GET /v2/activity/growth/tasks
		// 上游 403「growth system is only available for personal users」）。
		if a.IsEnterprise() {
			return
		}
		if tasks, err := p.cfg.Upstream.ListTasks(a); err != nil {
			it.GrowthErr = err.Error()
		} else {
			for _, t := range tasks {
				if growthPending(t) {
					it.Growth = append(it.Growth, t)
				}
			}
		}
		// 小程序口径任务（school_season 校园日 / Sequential_Tasks_1 小程序首对话）
		// 仅在 mp 头列表下发，与默认口径不重叠——合并进待办列表；mp 列表失败
		// 静默（无 mp 任务的部署/活动结束时零影响）。
		if mpTasks, err := p.cfg.Upstream.ListTasksMP(a); err == nil {
			seen := map[string]bool{}
			for _, t := range it.Growth {
				seen[t.TaskCode] = true
			}
			for _, t := range mpTasks {
				if growthPending(t) && !seen[t.TaskCode] {
					it.Growth = append(it.Growth, t)
				}
			}
		}
	})
	pending := 0
	for _, it := range items {
		pending += len(it.Growth)
	}
	log.Printf("panel: 队列扫描完成：账号 %d（并发上限 %d）待办 %d 项", len(states), concurrency, pending)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": items, "pending_count": pending})
}

// tasksRunQueue 启动执行队列：{concurrency:1-4, growth:bool, school:bool}。
// 先做一次扫描，把全部待办项排队（growth 按账号内 autoActions 顺序执行，
// school 逐账号跑闭环），账号内串行、账号间受并发信号量约束。
// v2.6.0：body 限 1MB（readJSONBody）；扫描阶段有界扇出且受请求 ctx 取消。
func (p *Panel) tasksRunQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Concurrency int  `json:"concurrency"`
		Growth      bool `json:"growth"`
	}
	// body 可省略（旧前端/脚本不带 body 时按默认 Growth=true 处理）：仅在
	// ContentLength>0 时解码，解析失败回 400（不再静默忽略脏 body）。
	if r.ContentLength > 0 && !readJSONBody(w, r, panelJSONBodyMax, &body) {
		return
	}
	if !body.Growth {
		body.Growth = true
	}
	if body.Concurrency < 1 {
		body.Concurrency = 1
	}
	if body.Concurrency > 4 {
		body.Concurrency = 4
	}
	started, total, seq, msg := p.startGrowthQueueCtx(r.Context(), body.Concurrency, body.Growth, taskTriggerManual)
	switch {
	case seq == -1:
		writeErr(w, http.StatusConflict, msg)
	case !started:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": false, "message": msg})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "total": total, "seq": seq})
	}
}

// tasksQueueStatus 队列状态（轮询用）。
func (p *Panel) tasksQueueStatus(w http.ResponseWriter, r *http.Request) {
	q := p.queue()
	q.mu.Lock()
	defer q.mu.Unlock()
	items := make([]queueItem, len(q.items))
	copy(items, q.items)
	writeJSON(w, http.StatusOK, map[string]any{
		"running":    q.running,
		"total":      len(items),
		"conc":       q.conc,
		"started":    !q.startedAt.IsZero(),
		"started_at": q.startedAt,
		"seq":        q.seq,
		"items":      items,
	})
}

// schoolVouchers 我的券码：逐 CN 账号查开学季 /vouchers（固定 3 并发，与
// packages 同款限流），失败只在对应账号标 error。global 账号无开学季，不发上游。
// v2.6.0 P0：账号级扇出改固定 worker 池（不再每账号一个 goroutine）。
func (p *Panel) schoolVouchers(w http.ResponseWriter, r *http.Request) {
	accts := p.poolSnapshot()
	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Vouchers []upstream.SchoolVoucher `json:"vouchers"`
		Err      string                   `json:"error,omitempty"`
	}
	out := make([]row, len(accts))
	type cand struct{ a *auth.Auth }
	cands := make([]cand, 0, len(accts))
	for i := range accts {
		st := &accts[i]
		if st.Disabled {
			continue // 未占位，行末统一压掉
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		cands = append(cands, cand{a: a})
	}
	fanOutAccounts(r.Context(), len(cands), 3, func(_ context.Context, i int) {
		a := cands[i].a
		it := row{UID: a.UID, Nickname: a.Nickname}
		switch {
		case a.IsGlobal():
			it.Err = "global realm（无开学季活动）"
		default:
			vs, err := p.cfg.Upstream.SchoolVouchers(a)
			if err != nil {
				it.Err = err.Error()
			} else {
				it.Vouchers = vs
			}
		}
		out[i] = it
	})
	res := make([]row, 0, len(out))
	for _, it := range out {
		if it.UID != "" {
			res = append(res, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": res})
}
