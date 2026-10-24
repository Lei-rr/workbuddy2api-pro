// overview.go 总览接口与池摘要封装（v2.6.1 拆分自 panel.go，纯移动）。
//
//   - overview/lite/accounts=0：计数/元信息/可选 accounts；数据源 pool.Summary/Snapshot；
//   - poolSnapshot/poolVersion/poolSummary：pool 只读 API 的薄封装（v2.5.0 Perf-3）。
package panel

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
)

// ---------------------------------------------------------------------------
// 只读接口
// ---------------------------------------------------------------------------

// overview 总览：池计数 + 面板元信息（可选 accounts 全量）。
//
// v2.1 #1：?lite=1 只回汇总（计数/元信息/model_locks/积分合计，约 1-2 KB），供前端
// 5s 轮询；不带 lite（默认）时除新增 credits_sum/credits_total_sum 两个汇总字段外
// 与旧版逐字段等价（含 accounts 全量）——旧脚本零破坏。
//
// v2.5.0 Perf-3：汇总数据改由 pool.Summary()（O(1) 缓存）一次取齐，不再做
// CountsDetailedWithPaused / CreditsSums / ModelLockView 三次全池扫描；10k 账号下
// lite 从 ~12ms 降到亚毫秒。另新增 ?accounts=0（轻量模式，响应与 lite 等价）：全量
// overview 的调用方可显式关闭 accounts 数组；默认仍带 accounts 保持旧契约。
func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	sum := p.poolSummary()
	sticky := 0
	if p.cfg.StickyCount != nil {
		sticky = p.cfg.StickyCount()
	}
	// model_locks 模型级限流全清单（哪些模型不能用、锁了几个号、还要锁多久）：
	// Summary.ModelLocks 只给条目数（O(1)）；无锁时直接 null（与原行为一致），
	// 有锁时才取一次明细（低频，避免常态全池遍历 + 逐号 Realm()）。
	var modelLocks any
	if sum.ModelLocks > 0 && p.cfg.Pool != nil {
		modelLocks = p.cfg.Pool.ModelLockView()
	}
	resp := map[string]any{
		"version":    p.cfg.Version,
		"uptime_sec": int(time.Since(p.started).Seconds()),
		// auth_required 面板密钥是否启用（前端据此决定是否弹密钥输入）。
		"auth_required": p.apiKey() != "",
		// poll_interval_sec 面板前端轮询间隔（秒，2-60；v2.0.1 #2）：前端据此安排
		// 5s 默认轮询并可在线改配置，后端不感知前端实现。
		"poll_interval_sec": p.pollIntervalSec(),
		"redis_mode":        p.cfg.RedisMode,
		"sticky_sessions":   sticky,
		// 计数口径 = WithPaused：paused 独立于 disabled 展示（issue #125）。
		"total":    sum.Total,
		"healthy":  sum.Healthy,
		"cooling":  sum.Cooling,
		"disabled": sum.Disabled,
		"paused":   sum.Paused,
		// in_flight_full 全池在途请求数（Summary 内部 atomic 维护，不再 List 累加）。
		"in_flight_full": sum.InFlight,
		// credits_sum = 全池 credits 之和；credits_total_sum = 全池总额度之和（未知
		// 0/企业无限量 -1 按 0 计）。与旧前端「从 accounts 数组求和」口径一致，
		// 全量 overview 下也恒有该字段（向后兼容，只增不减）。
		"credits_sum":       sum.CreditsSum,
		"credits_total_sum": sum.CreditsTotalSum,
		// model_locks 模型级限流全清单（无锁时 null）。
		"model_locks": modelLocks,
		// 到期提醒快照的节奏与年龄：界面轮询是 5 秒级，但卡片数据只随余额后台刷新
		// 更新（默认每 5 分钟）——把这两个数透出，前端才能如实标注「x 分钟前更新」。
		"balance_refresh_sec":  p.balanceRefreshSec(),
		"balance_refreshed_at": p.balanceRefreshedAt(),
	}
	if !overviewLite(r) && !overviewAccountsDisabled(r) {
		// accounts 与 /panel/api/accounts 的数据源同一份缓存快照（只读，不排序）。
		resp["accounts"] = p.poolSnapshot()
	}
	writeJSON(w, http.StatusOK, resp)
}

// overviewAccountsDisabled 解析 ?accounts=0（v2.5.0 Perf-3）：显式关闭 accounts 数组的
// 轻量模式（响应等价于 ?lite=1）。空值/非法值一律按默认（带 accounts）处理，
// 旧调用方零破坏；默认 overview 契约仍包含 accounts。
func overviewAccountsDisabled(r *http.Request) bool {
	v := strings.TrimSpace(r.URL.Query().Get("accounts"))
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	return err == nil && !b
}

// poolSnapshot 返回池的缓存只读快照（按 uid 升序；调用方不得排序/修改）。
// nil 池（嵌入式/测试装配）返回 nil，与旧 List() 语义一致。
func (p *Panel) poolSnapshot() []pool.Status {
	if p.cfg.Pool == nil {
		return nil
	}
	return p.cfg.Pool.Snapshot()
}

// poolVersion 返回池内容代际号（v2.5.0 Perf-3 ETag 缓存失效判断用）。
func (p *Panel) poolVersion() uint64 {
	if p.cfg.Pool == nil {
		return 0
	}
	return p.cfg.Pool.Version()
}

// poolSummary 返回池一次性摘要（计数/积分合计/在途/模型锁数；O(1) 缓存）。
func (p *Panel) poolSummary() pool.Summary {
	if p.cfg.Pool == nil {
		return pool.Summary{}
	}
	return p.cfg.Pool.Summary()
}

// overviewLite 解析 ?lite=1（兼容 1/true 等布尔写法，大小写不敏感）。
// 空值/非法值一律按默认全量处理：旧调用方（不带该参数）零破坏。
func overviewLite(r *http.Request) bool {
	v := strings.TrimSpace(r.URL.Query().Get("lite"))
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	return err == nil && b
}

// balanceRefreshSec 余额后台刷新间隔（秒）；无调度器/未开启时为 0。
func (p *Panel) balanceRefreshSec() int {
	if p.cfg.Scheduler == nil {
		return 0
	}
	return int(p.cfg.Scheduler.BalanceInterval() / time.Second)
}

// balanceRefreshedAt 最近一次后台余额刷新完成时刻；无调度器或尚未跑过为零值。
func (p *Panel) balanceRefreshedAt() time.Time {
	if p.cfg.Scheduler == nil {
		return time.Time{}
	}
	return p.cfg.Scheduler.BalanceRefreshedAt()
}
