// audit.go 面板操作审计（v2.2.0 #5）：所有写操作落 data/panel-audit.jsonl。
//
// 为什么需要：写操作（删号/禁用/改配置/全量任务）此前只有零散 log.Printf，进 500 条
// 环形缓冲又会被批量任务日志瞬间冲掉，docker logs 重建即丢；出问题时无法回答
// 「谁在什么时候把号删了 / 把配置改了」。审计是旁路：写盘失败只 WARN，绝不影响
// 操作结果；不记录凭证/密钥/请求体，detail 只取响应里的错误/提示文案。
//
// 埋点方式：路由注册时用 withAudit 包一层（panel.go routes()），统一捕获
// {ts, ip, action, uid, ok, detail}：
//   - ok：HTTP 2xx 且响应 JSON 未显式声明 ok:false（部分 handler 用 200+ok:false
//     表达业务拒绝）；
//   - uid：优先路径参数 {uid}，否则响应 JSON 的 uid（登录/做任务等）；
//   - detail：响应 JSON 的 error/message 等可读文案（截断 240 字符）。
//
// 只审计「写操作」；GET 只读接口不落盘。唯一的例外是 OAuth 登录轮询（GET，但完成时
// 会创建账号）：用 withAuditConditional 只在「完成/失败」时记一条，pending 轮询不记。
package panel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"
)

const (
	// auditFileName 审计文件名（与 state 文件同目录）。
	auditFileName = "panel-audit.jsonl"
	// auditDefaultLimit / auditMaxLimit 查询默认与上限条数。
	auditDefaultLimit = 200
	auditMaxLimit     = 2000
	// auditBodyMax 审计中间件捕获的响应体上限（只用于解析 ok/error/uid，不落盘）。
	auditBodyMax = 4096
)

// 审计 action 枚举（稳定契约：前端 AUDIT_ACTION_LABEL 按这些值做展示映射，
// 新增只加不改名；未映射的新 action 前端会原样显示 code）。
const (
	auditActionLoginStart     = "login_start"
	auditActionLogin          = "login"
	auditActionImport         = "import"
	auditActionAccountRevive  = "account_revive"
	auditActionAccountDisable = "account_disable"
	auditActionAccountPause   = "account_pause"
	auditActionAccountResume  = "account_resume"
	auditActionAccountsRescan = "accounts_rescan"
	auditActionAccountCheckin = "account_checkin"
	auditActionAccountBalance = "account_balance"
	auditActionAccountRemove  = "account_remove"
	auditActionTaskAccept     = "task_accept"
	auditActionTaskAcceptAll  = "task_accept_all"
	auditActionTaskClaim      = "task_claim"
	auditActionTaskAuto       = "task_auto"
	auditActionTaskAutoAll    = "task_auto_all"
	auditActionTasksRun       = "tasks_run"
	auditActionTasksRunQueue  = "tasks_run_queue"
	auditActionCheckinAll     = "checkin_all"
	auditActionTravelAll      = "travel_all"
	auditActionActivityAll    = "activity_all"
	auditActionKeepaliveAll   = "keepalive_all"
	auditActionBalanceAll     = "balance_all"
	auditActionUsageSave      = "usage_save"
	auditActionConfigSave     = "config_save"
	// auditActionConfigRollback 配置回滚端点（panel.go routes() 已接线，v2.2.0 #15）：
	// 与 config_save 同一审计通道，动作名稳定。
	auditActionConfigRollback = "config_rollback"
	// v2.6.0 审计补覆盖：meta 写与 Job 触发/取消此前是缺口（旧批量端点已审计）。
	auditActionAccountMetaUpdate = "account_meta_update"
	auditActionAccountMetaDelete = "account_meta_delete"
	auditActionJobRun            = "job_run"
	auditActionJobCancel         = "job_cancel"
)

// AuditEntry 一条面板操作审计记录（JSONL 单行；GET /panel/api/audit 原样返回）。
type AuditEntry struct {
	TS     int64  `json:"ts"`               // unix 秒
	IP     string `json:"ip,omitempty"`     // 来源 IP（X-Forwarded-For 首段 → X-Real-IP → 对端）
	Action string `json:"action"`           // 动作枚举，见 auditAction* 常量
	UID    string `json:"uid,omitempty"`    // 目标账号（非账号操作为空）
	OK     bool   `json:"ok"`               // 操作是否成功
	Detail string `json:"detail,omitempty"` // 失败原因/提示（≤240 字符；不含凭证）
}

// audit 记录一条面板操作审计（写失败只 WARN，不阻断 handler）。
// uid 为空时自动从路径参数取；detail 截断后落盘。
func (p *Panel) audit(r *http.Request, action, uid string, ok bool, detail string) {
	if p.auditLog == nil {
		return
	}
	if uid == "" && r != nil {
		uid = r.PathValue("uid")
	}
	entry := AuditEntry{
		TS:     time.Now().Unix(),
		Action: action,
		UID:    uid,
		OK:     ok,
		Detail: truncateRunes(detail, jsonlDetailMax),
	}
	if r != nil {
		entry.IP = rateLimitIP(r)
	}
	p.auditLog.append(entry)
}

// auditResponse 包装 ResponseWriter：记录状态码与响应体前 auditBodyMax 字节，
// 供审计中间件判定 ok/uid/detail；handler 输出字节不受影响。
type auditResponse struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
	limit  int
}

func (a *auditResponse) WriteHeader(code int) {
	if a.status == 0 {
		a.status = code
	}
	a.ResponseWriter.WriteHeader(code)
}

func (a *auditResponse) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	if room := a.limit - a.body.Len(); room > 0 {
		if len(b) > room {
			a.body.Write(b[:room])
		} else {
			a.body.Write(b)
		}
	}
	return a.ResponseWriter.Write(b)
}

// result 解析本次响应：ok 默认按 HTTP 2xx；响应 JSON 显式带 ok:false 时以 body 为准。
// detail 依次取 error/message/checkin_message/file_error/balance_error 中第一个非空串。
func (a *auditResponse) result() (ok bool, uid, detail string) {
	status := a.status
	if status == 0 {
		status = http.StatusOK
	}
	ok = status >= 200 && status < 300

	var m map[string]json.RawMessage
	if a.body.Len() > 0 && json.Unmarshal(a.body.Bytes(), &m) == nil {
		if raw, has := m["ok"]; has {
			var b bool
			if json.Unmarshal(raw, &b) == nil {
				ok = ok && b
			}
		}
		if raw, has := m["uid"]; has {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				uid = s
			}
		}
		for _, key := range []string{"error", "message", "checkin_message", "file_error", "balance_error"} {
			if raw, has := m[key]; has {
				var s string
				if json.Unmarshal(raw, &s) == nil && s != "" {
					detail = s
					break
				}
			}
		}
	}
	if !ok && detail == "" {
		detail = http.StatusText(status)
	}
	return ok, uid, detail
}

// withAudit 包装写操作 handler：执行完自动落一条审计（成功与失败都记）。
func (p *Panel) withAudit(action string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ar := &auditResponse{ResponseWriter: w, limit: auditBodyMax}
		next(ar, r)
		ok, uid, detail := ar.result()
		p.audit(r, action, uid, ok, detail)
	}
}

// withAuditConditional 同 withAudit，但由 should 决定是否记录：用于 GET 轮询类端点
// （OAuth 登录轮询），只在完成/失败时留痕，避免每 3s 一条 pending 噪音。
func (p *Panel) withAuditConditional(action string, should func(*auditResponse) bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ar := &auditResponse{ResponseWriter: w, limit: auditBodyMax}
		next(ar, r)
		if should != nil && !should(ar) {
			return
		}
		ok, uid, detail := ar.result()
		p.audit(r, action, uid, ok, detail)
	}
}

// responseJSONBool 解析响应体 JSON 的布尔字段（不存在/解析失败返回 false,false）。
func responseJSONBool(body []byte, key string) (value, found bool) {
	var m map[string]json.RawMessage
	if len(body) == 0 || json.Unmarshal(body, &m) != nil {
		return false, false
	}
	raw, ok := m[key]
	if !ok {
		return false, false
	}
	var b bool
	if json.Unmarshal(raw, &b) != nil {
		return false, false
	}
	return b, true
}

// auditHandler 返回最近 N 条面板操作审计（新的在前）；文件不存在 → []。
func (p *Panel) auditHandler(w http.ResponseWriter, r *http.Request) {
	limit := parseLimit(r, auditDefaultLimit, auditMaxLimit)
	entries, err := p.auditLog.readLast(limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read audit: "+err.Error())
		return
	}
	if entries == nil {
		entries = []json.RawMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "entries": entries, "limit": limit})
}
