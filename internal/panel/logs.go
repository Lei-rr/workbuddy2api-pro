// logs.go 运行日志/请求指标/请求归档/积分历史/昵称映射（v2.6.1 拆分自 panel.go，纯移动）。
package panel

import (
	"net/http"

	"github.com/lei-rr/workbuddy2api-pro/internal/reqlog"
)

// logsHandler 返回日志环形缓冲快照（时间升序，含频道标记 chat/task/sys）。
func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": p.logs.Snapshot()})
}

// requestMetrics 返回进程内请求指标、最近 100 条与归档状态。
func (p *Panel) requestMetrics(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	writeJSON(w, http.StatusOK, p.cfg.RequestLog.Snapshot())
}

// requestLogs 从 JSONL 归档读取最近请求；limit 默认 200、最大 1000。
// 支持按 outcome/account/model/client_ip/user_agent 过滤（字符串字段为包含匹配）
// 与 from/to 时间区间（闭区间，unix 秒或 RFC3339）——面板「运行日志」的筛选框、
// 来源查询与「今天 / 自定义区间」都走这里。
func (p *Panel) requestLogs(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	limit := parseLimit(r, 200, 1000)
	q := r.URL.Query()
	rows, err := p.cfg.RequestLog.ReadArchive(limit, reqlog.Filter{
		Outcome:   q.Get("outcome"),
		Account:   q.Get("account"),
		Model:     q.Get("model"),
		ClientIP:  q.Get("client_ip"),
		UserAgent: q.Get("user_agent"),
		From:      parseTimeParam(q.Get("from")),
		To:        parseTimeParam(q.Get("to")),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 空结果回 []（而不是 JSON null）：前端把 null 与"归档关闭"混在一起会走错分支，
	// 显示成不满足筛选条件的最近请求。
	if rows == nil {
		rows = []reqlog.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows, "limit": limit})
}

// creditHistory 返回积分变动流水（新的在前），供「运行日志 → 积分历史」展示。
// limit 默认 200、上限 1000（与 requestLogs 同口径）；可带 uid 精确过滤。
// 账号昵称在读取时用池快照填充——账本只存 uid，上游改名后旧流水也跟着更新，
// 且留痕路径不必碰 auth 的昵称锁。
func (p *Panel) creditHistory(w http.ResponseWriter, r *http.Request) {
	if p.cfg.CreditHistory == nil {
		writeErr(w, http.StatusNotImplemented, "credit history not available")
		return
	}
	limit := parseLimit(r, 200, 1000)
	uid := r.URL.Query().Get("uid")
	nicks := p.nicknames()
	// 有 uid 过滤时必须先全量取回（账本上限默认 2000 条）：先截断再过滤会让
	// 筛选后的条数看起来像历史缺失。无过滤时只取 limit 条，不为一次展示拷贝整个账本。
	readLimit := limit
	if uid != "" {
		readLimit = 0
	}
	all := p.cfg.CreditHistory.Read(readLimit)
	out := make([]map[string]any, 0, limit)
	for _, e := range all {
		if uid != "" && e.UID != uid {
			continue
		}
		out = append(out, map[string]any{
			"time":    e.Time,
			"uid":     e.UID,
			"account": nicks[e.UID],
			"delta":   e.Delta,
			"before":  e.Before,
			"after":   e.After,
		})
		if len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "limit": limit})
}

// nicknames 池内 uid → 昵称映射（仅展示用，不含任何凭证）。
func (p *Panel) nicknames() map[string]string {
	nicks := map[string]string{}
	if p.cfg.Pool == nil {
		return nicks
	}
	for _, s := range p.cfg.Pool.List() {
		if s.Nickname != "" {
			nicks[s.UID] = s.Nickname
		}
	}
	return nicks
}
