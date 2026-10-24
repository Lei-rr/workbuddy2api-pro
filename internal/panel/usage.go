// usage.go 用量聚合与积分包查询（v2.6.1 拆分自 panel.go，纯移动）。
package panel

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
	"github.com/lei-rr/workbuddy2api-pro/internal/usage"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// usage 返回逐请求用量聚合。统计窗口三选一：
//   - from/to（unix 秒）：显式区间，用于「今天」与「自定义」——区间由浏览器按
//     本地时区算好再发，服务端时区与浏览器不一致时「今天」才不会被算错；
//   - hours：滚动窗口（默认 72，上限 1440=60 天），卡片汇总/按域/按账号/按模型/
//     时序**全部**按该窗口统计；显式 hours=0 表示全部历史（含 90 天前折叠出的日桶）；
//   - 都不给：等同于 hours=72（保持旧调用方行为）。
func (p *Panel) usage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	q := r.URL.Query()
	win := usage.Window{}
	win.From = parseTimeParam(q.Get("from"))
	win.To = parseTimeParam(q.Get("to"))
	if win.From.IsZero() && win.To.IsZero() {
		win.Hours = 72
		if v := q.Get("hours"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				win.Hours = n
			}
		}
		if win.Hours > 1440 {
			win.Hours = 1440
		}
	}
	// 昵称仅用于展示，取自池快照（不含任何凭证）。
	nicks := p.nicknames()
	var currentRate func(realm, model string) string
	if p.cfg.Upstream != nil {
		currentRate = p.cfg.Upstream.ModelRate
	}
	writeJSON(w, http.StatusOK, p.cfg.Usage.SnapshotWindow(win, nicks, currentRate))
}

// parseTimeParam 解析时间查询参数：unix 秒（前端默认）或 RFC3339（便于手工调
// 接口/写脚本）。空串与非法值都返回零值 = 该侧不设界，不报错——区间参数是可选
// 增强，拼错一个 from 不该让整页用量打不开。
func parseTimeParam(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return time.Time{}
		}
		// 兼容秒与毫秒（前端可能直接把 Date.now() 传上来）。
		if n > 1e12 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", v, time.Local); err == nil {
		return t
	}
	return time.Time{}
}

// usageSave 立即把内存中的用量桶落盘（正常由后台 30s 防抖刷新负责）。
func (p *Panel) usageSave(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	p.cfg.Usage.Save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// packagesRow 是 packages 接口的单账号返回行。
type packagesRow struct {
	UID      string                   `json:"uid"`
	Nickname string                   `json:"nickname"`
	Realm    string                   `json:"realm"`
	Remain   int64                    `json:"remain"`
	Size     int64                    `json:"size"`
	Packages []upstream.CreditPackage `json:"packages"`
	Error    string                   `json:"error,omitempty"`
}

// packages 返回单个账号的积分包构成（?uid= 必填）。
// 只查 1 个账号、只打 1 次上游——面板账号池的「积分」按钮走这条。
// 整池逐号查询的批量特征有风控风险，已移除全量模式：无 uid 直接 400。
func (p *Panel) packages(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	if uid == "" {
		writeErr(w, http.StatusBadRequest, "uid is required")
		return
	}
	p.packagesOne(w, uid)
}

// packagesOne 单号模式：只查一个账号（1 次上游请求），供面板「积分」按钮。
// 上游查询失败仍返回 200 + row.Error——前端与其他接口的 error 行语义一致。
func (p *Panel) packagesOne(w http.ResponseWriter, uid string) {
	st, ok := p.cfg.Pool.Status(uid)
	if !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	it := packagesRow{UID: st.UID, Nickname: st.Nickname, Realm: st.Realm}
	a := p.cfg.Pool.AuthByUID(st.UID)
	if a == nil {
		it.Error = "account not loaded"
		writeJSON(w, http.StatusOK, map[string]any{"accounts": []packagesRow{it}})
		return
	}
	packs, remain, size, err := p.cfg.Upstream.CreditPackages(a)
	if err != nil {
		it.Error = err.Error()
		writeJSON(w, http.StatusOK, map[string]any{"accounts": []packagesRow{it}})
		return
	}
	it.Packages = packs
	it.Remain = remain
	it.Size = size
	writeJSON(w, http.StatusOK, map[string]any{"accounts": []packagesRow{it}})
}
