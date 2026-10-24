// routes.go 面板路由表（v2.6.1 拆分自 panel.go，纯移动；60 条路由路径/方法不变）。
// 路由与 handler 的鉴权/审计包装方式保持原样：withAuth 必选、写操作 withAudit。
package panel

import (
	"github.com/lei-rr/workbuddy2api-pro/internal/jobs"
)

func (p *Panel) routes() {
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)
	p.mux.HandleFunc("GET /panel/mobile.css", p.mobileStyle)
	p.mux.HandleFunc("GET /panel/mobile.js", p.mobileScript)

	// ---- 只读接口 ----
	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
	// v2.4.0 #3：SSE 事件推送（Job/账号/日志增量；断流或不支持时前端自动回落轮询）。
	p.mux.HandleFunc("GET /panel/api/events", p.withAuth(p.events))
	// v2.1 #1：账号池列表服务端分页/筛选/排序/字段裁剪/ETag（与 /overview 旧接口并存，
	// 旧接口语义零变化）。放在 /accounts/{uid}/... 之前只是可读性，ServeMux 按路径匹配。
	p.mux.HandleFunc("GET /panel/api/accounts", p.withAuth(p.accounts))
	p.mux.HandleFunc("GET /panel/api/logs", p.withAuth(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/request_metrics", p.withAuth(p.requestMetrics))
	p.mux.HandleFunc("GET /panel/api/request_logs", p.withAuth(p.requestLogs))
	p.mux.HandleFunc("GET /panel/api/credit_history", p.withAuth(p.creditHistory))
	p.mux.HandleFunc("GET /panel/api/models", p.withAuth(p.models))
	// v2.2.0 #8 可观测（readyz/metrics）+ #5/#9 查询接口（审计/任务历史，新在前）。
	p.mux.HandleFunc("GET /panel/api/readyz", p.withAuth(p.readyz))
	p.mux.HandleFunc("GET /panel/api/metrics", p.withAuth(p.metrics))
	p.mux.HandleFunc("GET /panel/api/audit", p.withAuth(p.auditHandler))
	p.mux.HandleFunc("GET /panel/api/task_runs", p.withAuth(p.taskRunsHandler))
	// v2.3.0 #10 Job Runner：任务进度/历史查询（只读；Job 持久化在 data/jobs.json）。
	p.mux.HandleFunc("GET /panel/api/jobs", p.withAuth(p.jobsList))
	p.mux.HandleFunc("GET /panel/api/jobs/{id}", p.withAuth(p.jobsGet))
	// v2.3.0 #12 账号备注/标签（handler 由 backend-c 的 accountmeta.go 提供）：
	// 列表与单号读取为只读，写操作在下方写分区。数据落 <data>/account-meta.json。
	p.mux.HandleFunc("GET /panel/api/meta", p.withAuth(p.accountMetaList))
	// v2.3.1 #12 扩展：标签索引（accounts?tag= 下拉数据源；count 降序、同 count 字典序）。
	p.mux.HandleFunc("GET /panel/api/meta/tags", p.withAuth(p.accountMetaTags))
	p.mux.HandleFunc("GET /panel/api/accounts/{uid}/meta", p.withAuth(p.accountMetaGet))

	// ---- 写操作 ----
	// v2.2.0 #5：统一经 withAudit 包一层落操作审计（成功/失败都记，detail 取响应
	// 错误/提示文案）。action 枚举见 audit.go；只有写操作审计，GET 只读不记。
	p.mux.HandleFunc("POST /panel/api/login/start", p.withAuth(p.withAudit(auditActionLoginStart, p.loginStart)))
	// OAuth 登录轮询是 GET，但完成时会创建账号：只在「完成/失败」时记一条，pending
	// 每 3s 的轮询不记，避免审计文件被轮询噪音淹没。
	p.mux.HandleFunc("GET /panel/api/login/poll", p.withAuth(p.withAuditConditional(auditActionLogin,
		func(ar *auditResponse) bool {
			if ar.status >= 400 {
				return true
			}
			done, _ := responseJSONBool(ar.body.Bytes(), "done")
			return done
		}, p.loginPoll)))
	p.mux.HandleFunc("GET /panel/api/login/regions", p.withAuth(p.loginRegions))
	p.mux.HandleFunc("POST /panel/api/import/cockpit", p.withAuth(p.withAudit(auditActionImport, p.importCockpit)))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/revive", p.withAuth(p.withAudit(auditActionAccountRevive, p.accountRevive)))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/disable", p.withAuth(p.withAudit(auditActionAccountDisable, p.accountDisable)))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/pause", p.withAuth(p.withAudit(auditActionAccountPause, p.accountPause)))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/resume", p.withAuth(p.withAudit(auditActionAccountResume, p.accountResume)))
	p.mux.HandleFunc("POST /panel/api/accounts/rescan", p.withAuth(p.withAudit(auditActionAccountsRescan, p.accountsRescan)))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/checkin", p.withAuth(p.withAudit(auditActionAccountCheckin, p.accountCheckin)))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/balance", p.withAuth(p.withAudit(auditActionAccountBalance, p.accountBalance)))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/remove", p.withAuth(p.withAudit(auditActionAccountRemove, p.accountRemove)))
	p.mux.HandleFunc("GET /panel/api/accounts/{uid}/tasks", p.withAuth(p.accountTasks))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept", p.withAuth(p.withAudit(auditActionTaskAccept, p.accountTaskAccept)))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept_all", p.withAuth(p.withAudit(auditActionTaskAcceptAll, p.taskAcceptAll)))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/claim", p.withAuth(p.withAudit(auditActionTaskClaim, p.accountTaskClaim)))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto", p.withAuth(p.withAudit(auditActionTaskAuto, p.accountTaskAuto)))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto_all", p.withAuth(p.withAudit(auditActionTaskAutoAll, p.accountTaskAutoAll)))
	// 单账号「做任务」= 审计（人的操作）+ 任务历史（执行结果，v2.2.0 #9）+ Job 历史
	//（v2.3.0 #10：kind=tasks_run 的已完成记录写入 data/jobs.json，供 /jobs 查询）。
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/run", p.withAuth(
		p.withAudit(auditActionTasksRun,
			p.withJobHistory(jobs.KindTasksRun, jobs.TriggerManual,
				p.withTaskRunCapture(taskKindTasksRun, taskTriggerManual, p.accountTasksRun)))))
	// 扫描只读、不产生副作用：不审计（生产上由任务中心按钮触发，只读上游）。
	p.mux.HandleFunc("POST /panel/api/tasks/scan_all", p.withAuth(p.tasksScanAll))
	p.mux.HandleFunc("POST /panel/api/tasks/run_queue", p.withAuth(p.withAudit(auditActionTasksRunQueue, p.tasksRunQueue)))
	p.mux.HandleFunc("GET /panel/api/tasks/queue", p.withAuth(p.tasksQueueStatus))
	p.mux.HandleFunc("GET /panel/api/school/vouchers", p.withAuth(p.schoolVouchers))
	p.mux.HandleFunc("POST /panel/api/checkin_all", p.withAuth(p.withAudit(auditActionCheckinAll, p.checkinAll)))
	p.mux.HandleFunc("POST /panel/api/travel_all", p.withAuth(p.withAudit(auditActionTravelAll, p.travelAll)))
	p.mux.HandleFunc("POST /panel/api/activity_all", p.withAuth(p.withAudit(auditActionActivityAll, p.activityAll)))
	p.mux.HandleFunc("POST /panel/api/keepalive_all", p.withAuth(p.withAudit(auditActionKeepaliveAll, p.keepaliveAll)))
	p.mux.HandleFunc("POST /panel/api/balance_all", p.withAuth(p.withAudit(auditActionBalanceAll, p.balanceAll)))
	// v2.3.0 #10 Job Runner：触发（/jobs/{kind}/run，返回 job_id）与取消。
	// 触发走与旧端点相同的同 kind 单飞 gate：409 语义不变。
	// v2.6.0：与旧批量端点（checkin_all 等）对齐补 withAudit（job_run/job_cancel）。
	p.mux.HandleFunc("POST /panel/api/jobs/{kind}/run", p.withAuth(p.withAudit(auditActionJobRun, p.jobsRun)))
	p.mux.HandleFunc("POST /panel/api/jobs/{id}/cancel", p.withAuth(p.withAudit(auditActionJobCancel, p.jobsCancel)))
	// v2.3.0 #12 账号备注/标签写操作（PUT 覆盖保存；DELETE 幂等删除）。
	// v2.6.0：补 withAudit（account_meta_update/account_meta_delete）。
	p.mux.HandleFunc("PUT /panel/api/accounts/{uid}/meta", p.withAuth(p.withAudit(auditActionAccountMetaUpdate, p.accountMetaPut)))
	p.mux.HandleFunc("DELETE /panel/api/accounts/{uid}/meta", p.withAuth(p.withAudit(auditActionAccountMetaDelete, p.accountMetaDelete)))
	p.mux.HandleFunc("GET /panel/api/packages", p.withAuth(p.packages))
	p.mux.HandleFunc("GET /panel/api/usage", p.withAuth(p.usage))
	p.mux.HandleFunc("POST /panel/api/usage/save", p.withAuth(p.withAudit(auditActionUsageSave, p.usageSave)))
	p.mux.HandleFunc("GET /panel/api/model_probes", p.withAuth(p.modelProbes))
	p.mux.HandleFunc("GET /panel/api/config", p.withAuth(p.getConfig))
	// v2.2.0 #15：一键回滚（恢复 config.json.bak 并热应用）。走 config_save 同一
	// 审计通道（动作名 config_rollback），成功/失败都留痕。
	p.mux.HandleFunc("POST /panel/api/config/rollback", p.withAuth(p.withAudit(auditActionConfigRollback, p.rollbackConfig)))
	p.mux.HandleFunc("POST /panel/api/config", p.withAuth(p.withAudit(auditActionConfigSave, p.saveConfig)))
}
