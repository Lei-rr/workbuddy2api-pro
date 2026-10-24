// autotask_http.go 任务相关 HTTP handler（v2.6.1 拆分自 autotask.go，纯移动；响应契约不变）。
package panel

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// accountTaskAuto 一键完成单个任务：执行对应动作 → 回读进度 → 汇报结果。
func (p *Panel) accountTaskAuto(w http.ResponseWriter, r *http.Request) {
	a, ok := p.accountForRequest(w, r)
	if !ok {
		return
	}
	uid := a.UID
	// 企业上下文门控（企业版 / 企业切换会话）：行为事件会被上游静默丢弃或
	// 直接 403，执行任何成长任务都是零收益白跑（含真实对话配额）——直接拒绝
	// 并给出修复路径，不再让用户对着「已上报但进度 0/x」猜原因
	// （见 auth.IsEnterpriseSession）。
	if a.IsEnterpriseSession() {
		writeErr(w, http.StatusUnprocessableEntity,
			"该账号为企业版/企业切换会话（token_source=enterprise_switch），无个人成长体系，成长任务行为事件会被上游忽略；请移除账号后以个人身份重新登录")
		return
	}
	var body struct {
		TaskCode string `json:"task_code"`
	}
	if !readJSONBody(w, r, panelJSONBodyMax, &body) {
		return
	}
	if body.TaskCode == "" {
		writeErr(w, http.StatusBadRequest, "task_code required")
		return
	}
	act := autoActionFor(body.TaskCode)
	if act == nil {
		writeErr(w, http.StatusNotImplemented,
			"该任务需要客户端内交互（无对应接口），无法自动完成；请按任务说明在官方客户端操作")
		return
	}
	// per-account 互斥：同账号的任务动作正在跑（单任务或全量）时直接 409，
	// 不并发重跑（动作幂等但 expert/skill 系含真实对话，重跑浪费配额）。
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "该账号有任务动作正在执行中，请等本轮结束后再试")
		return
	}
	defer p.unlockAccount(uid)
	// 前置读取：已完成的任务直接跳过（幂等，不浪费上游调用）。
	// taskByCode 已双口径（mp 专属码自动回落 mp 列表）。
	before, err := p.taskByCode(a, act.TaskCode)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list tasks: "+err.Error())
		return
	}
	if before == nil {
		writeErr(w, http.StatusNotFound, "该账号没有此任务")
		return
	}
	isMP := isMPTaskCode(act.TaskCode)
	if before.Claimed {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "skipped": true, "message": "该任务已领取过奖励"})
		return
	}
	msg, err := act.run(p, a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "执行失败: "+err.Error())
		return
	}
	// 回读验证：上报 200 ≠ 计分（上游可能静默丢弃 + 计分异步），
	// 用有界轮询等异步计时落定，再决定是否自动领奖（mp 任务同口径回读）。
	var after *upstream.Task
	var aerr error
	if isMP {
		after, aerr = p.taskByCodeMP(a, act.TaskCode)
	} else {
		after, aerr = p.taskByCodeWaiting(a, act.TaskCode)
	}
	progressBefore, progressAfter := taskProgressText(before), ""
	claimable := false
	if aerr == nil && after != nil {
		progressAfter = taskProgressText(after)
		claimable = after.Claimable
	}
	resp := map[string]any{
		"ok":               true,
		"message":          msg,
		"progress_before":  progressBefore,
		"progress_after":   progressAfter,
		"claimable":        claimable,
		"attempt":          act.Attempt,
		"verify_supported": true,
	}
	// 达标即自动领奖（Web 端 claim）：把"完成→领奖"收敛成一步，无需用户再点一次。
	if claimable {
		var credit, energy int64
		var cerr error
		if isMP {
			credit, energy, cerr = p.cfg.Upstream.ClaimRewardMP(a, act.TaskCode)
		} else {
			credit, energy, cerr = p.cfg.Upstream.ClaimReward(a, act.TaskCode)
		}
		if cerr == nil {
			resp["claimed"] = true
			resp["credit"] = credit
			resp["energy"] = energy
			if credit > 0 || energy > 0 {
				resp["message"] = msg + fmt.Sprintf("；已自动领奖 +%d 分 +%d 能", credit, energy)
			} else {
				resp["message"] = msg + "；奖励此前已领取"
			}
		} else {
			resp["claim_error"] = cerr.Error()
			resp["message"] = msg + "；达标但领奖失败，可在任务列表手动点「领取」重试"
		}
	}
	log.Printf("panel: 任务动作 uid=%s code=%s progress %s -> %s claimable=%v claimed=%v",
		uid, act.TaskCode, progressBefore, progressAfter, claimable, resp["claimed"])
	writeJSON(w, http.StatusOK, resp)
}

// accountTaskAutoAll 一键完成该账号全部可自动任务。
func (p *Panel) accountTaskAutoAll(w http.ResponseWriter, r *http.Request) {
	a, ok := p.accountForRequest(w, r)
	if !ok {
		return
	}
	uid := a.UID
	// 企业上下文门控：与 accountTaskAuto 同口径（企业版 / 企业切换会话下成长任务
	// 行为事件被上游静默丢弃，跑全量只会白耗真实对话配额）。「一键完成全部」与
	// 单任务入口共用同一套动作，必须一并挡上（下游 fork 吸收本 PR 时发现的缺口）。
	if a.IsEnterpriseSession() {
		writeErr(w, http.StatusUnprocessableEntity,
			"该账号为企业版/企业切换会话（token_source=enterprise_switch），无个人成长体系，成长任务行为事件会被上游忽略；请移除账号后以个人身份重新登录")
		return
	}
	// per-account 互斥（与单任务动作共用一把锁）：重复点击 409。
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "该账号有任务动作正在执行中，请等本轮结束后再试")
		return
	}
	// 用 context 兜底超时（多项任务串联 + 每项含真实对话，可能耗时较长）。
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	done := make(chan []map[string]any, 1)
	go func() {
		defer p.unlockAccount(uid) // 流水线真正结束（而非 HTTP 超时返回）才放锁
		done <- p.runAutoAll(a)
	}()
	select {
	case results := <-done:
		log.Printf("panel: 一键完成可自动任务 uid=%s 共 %d 项", uid, len(results))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
	case <-ctx.Done():
		// HTTP 侧超时返回，但后台流水线仍在跑——锁在流水线 goroutine 内释放，
		// 期间重复点击会被 409 挡住，不会出现两轮并发。
		writeErr(w, http.StatusGatewayTimeout, "执行超时（任务仍在后台继续）")
	}
}

// accountTasksRun 单账号「做任务」：对该账号执行全部可自动任务，返回逐项结果 + 汇总。
//
// 路由：POST /panel/api/accounts/{uid}/tasks/run（无请求体）。
//
// 语义：与 auto_all 共用同一个执行器（runAutoAll：批量 accept → 逐项行为链路 →
// 达标自动领奖，见本文件），不另造一套执行逻辑；区别只在响应契约——前端「做任务」
// 按钮直接拿 summary 展示计数，不必自己遍历 results。同步返回（可能耗时数分钟）。
//
// 成功响应（200）：
//
//	{"ok":true,"uid":"<uid>",
//	 "results":[{"task_code":"...","status":"done|skipped|error","message":"..."}, ...],
//	 "summary":{"total":N,"done":N,"skipped":N,"error":N,"claimed":N}}
//
// results 原样透传 runAutoAll 的条目（附 desc/claimed/credit/energy/progress_after
// 等扩展字段；前端只依赖 task_code/status/message 三字段）。summary 口径见
// taskRunSummary 注释。
//
// 门控与状态码（tasks_run_test.go 钉住）：
//   - 未知 uid → 404 {"ok":false,"error":"account not found"}；
//   - 企业版 / 企业切换会话（无个人成长体系，行为事件会被上游忽略）→ **200 + ok:false
//   - message**，不发起任何上游请求（与前端「跳过并提示」的交互约定一致）；
//   - 无凭证 → 同 200 + ok:false + message；
//   - 同账号已有任务动作在跑（per-account 锁被占）→ 409；
//   - 执行超时 → 504（后台流水线继续跑，锁由 goroutine 释放）。
//
// 两个 ok:false 分支也返回 results:[] 与 summary 零值，保持响应形状稳定，前端可以
// 无条件读 .results/.summary，只看 .ok 决定成功还是展示 .message。
func (p *Panel) accountTasksRun(w http.ResponseWriter, r *http.Request) {
	a, ok := p.accountForRequest(w, r)
	if !ok {
		return
	}
	uid := a.UID
	// 门控拒绝统一回 200 + ok:false（不是 4xx）：前端 api() 只看 HTTP 状态码，
	// 4xx 会被当成请求失败弹通用错误，拿不到这里的可读原因。
	reject := func(msg string) {
		log.Printf("panel: 做任务 uid=%s 跳过: %s", uid, msg)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"uid":     uid,
			"message": msg,
			"results": []map[string]any{},
			"summary": taskRunSummary{},
		})
	}
	if a.IsEnterpriseSession() {
		reject("该账号为企业版/企业切换会话，无个人成长任务体系（成长任务行为事件会被上游忽略），无法执行做任务")
		return
	}
	if a.AccessTokenValue() == "" {
		reject("该账号无有效凭证（未登录或 token 已清空），无法执行做任务")
		return
	}
	// per-account 互斥与单任务/auto_all 共用一把锁：正在跑就 409，不并发重跑。
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "该账号有任务动作正在执行中，请等本轮结束后再试")
		return
	}
	// 与 auto_all 同口径的 context 兜底超时（多项任务串联 + 每项可能含真实对话）。
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	done := make(chan []map[string]any, 1)
	go func() {
		defer p.unlockAccount(uid) // 流水线真正结束才放锁（HTTP 超时不影响）
		done <- p.runAutoAll(a)
	}()
	select {
	case results := <-done:
		if results == nil {
			results = []map[string]any{} // JSON 输出 []（而不是 null），前端可直接 length
		}
		summary := summarizeTaskRun(results)
		log.Printf("panel: 单号做任务 uid=%s 共 %d 项（完成 %d 跳过 %d 失败 %d 领奖 %d）",
			uid, summary.Total, summary.Done, summary.Skipped, summary.Error, summary.Claimed)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"uid":     uid,
			"results": results,
			"summary": summary,
		})
	case <-ctx.Done():
		writeErr(w, http.StatusGatewayTimeout, "执行超时（任务仍在后台继续）")
	}
}
