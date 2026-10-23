// metrics.go 面板可观测底座（v2.2.0 #8 部分）：/panel/api/readyz + /panel/api/metrics。
//
// 与 /healthz 的分工（/healthz 在 internal/server，语义是「业务可用」= 有可服务账号）：
//   - /readyz：进程「就绪性」——配置可读、auth_dir 可写、state 目录可写、池已加载；
//     容器编排启动探针用（刚启动/挂载丢失/磁盘只读时该 503），不掺业务流量判断；
//   - /metrics：手写 Prometheus text exposition v0.0.4（零第三方依赖）——up/uptime、
//     池状态计数、在途请求、积分合计、模型锁数、面板 API 请求计数。
//
// 两项都走面板 Bearer 鉴权（withAuth）：面板只在内网/本机暴露，指标不含凭证；
// readyz/metrics 响应很小（<1KB），gzip 中间件按其阈值自然跳过压缩。
package panel

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// readyProbePrefix 可写性探测临时文件前缀（建完立即删除，无副作用）。
	readyProbePrefix = ".readyz-probe-"
	// metricsContentType Prometheus 文本格式 Content-Type（exposition v0.0.4）。
	metricsContentType = "text/plain; version=0.0.4; charset=utf-8"
)

// redisReady Redis 依赖的只读观测（panel 不依赖 Redis 判就绪，仅透出模式供排查）。
type redisReady struct {
	Mode string `json:"mode"` // "upstash"
}

// readyChecks readyz 的逐项检查结果（JSON 形状稳定：探针/脚本可逐键消费）。
type readyChecks struct {
	Config           bool        `json:"config"`             // 配置文件存在且可读
	AuthDirWritable  bool        `json:"auth_dir_writable"`  // auth_dir 目录可写（添加账号/续期落盘）
	StateDirWritable bool        `json:"state_dir_writable"` // state 同目录可写（审计/历史/状态落盘）
	PoolLoaded       bool        `json:"pool_loaded"`        // 账号池已装配（*pool.Pool 非 nil）
	Redis            *redisReady `json:"redis"`              // null = 未使用 Redis（noop）
	UptimeSec        int64       `json:"uptime_sec"`
}

// readyzResponse /panel/api/readyz 响应：ok=false 时 HTTP 503；errors 列出失败项便于排查。
type readyzResponse struct {
	OK     bool        `json:"ok"`
	Checks readyChecks `json:"checks"`
	Errors []string    `json:"errors,omitempty"`
}

// readyz 就绪探针：关键项全通过 → 200 {ok:true}；任一失败 → 503 {ok:false, errors:[...]}。
// 就绪 = 进程能正常承担面板职责（配置可读、目录可写、池已装配）；空池不算不就绪
// （全新部署允许 0 账号，业务可用性由 /healthz 判断）。
func (p *Panel) readyz(w http.ResponseWriter, r *http.Request) {
	checks, failures := p.readyChecks()
	resp := readyzResponse{OK: len(failures) == 0, Checks: checks, Errors: failures}
	status := http.StatusOK
	if !resp.OK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, resp)
}

// readyChecks 计算各项检查并返回失败项名称列表。
func (p *Panel) readyChecks() (readyChecks, []string) {
	var failures []string

	// config：路径已配置且文件存在（LoadConfig/SaveConfig 都围绕该文件）。
	configOK := false
	if p.cfg.ConfigPath != "" {
		if fi, err := os.Stat(p.cfg.ConfigPath); err == nil && fi.Mode().IsRegular() {
			configOK = true
		}
	}
	if !configOK {
		failures = append(failures, "config")
	}

	// auth_dir：添加账号/续期要落盘凭证，目录不可写时面板运维能力已受损。
	authOK := dirWritable(p.cfg.AuthDir)
	if !authOK {
		failures = append(failures, "auth_dir_writable")
	}

	// state 同目录：审计/任务历史/用量等 JSONL 都落这里；不可写说明挂载/磁盘异常。
	stateOK := dirWritable(p.dataDir)
	if !stateOK {
		failures = append(failures, "state_dir_writable")
	}

	// pool：nil 表示装配缺失（面板大量接口不可用）。
	poolOK := p.cfg.Pool != nil
	if !poolOK {
		failures = append(failures, "pool_loaded")
	}

	var redis *redisReady
	if p.cfg.RedisMode != "" && p.cfg.RedisMode != "noop" {
		redis = &redisReady{Mode: p.cfg.RedisMode}
	}

	return readyChecks{
		Config:           configOK,
		AuthDirWritable:  authOK,
		StateDirWritable: stateOK,
		PoolLoaded:       poolOK,
		Redis:            redis,
		UptimeSec:        int64(time.Since(p.started).Seconds()),
	}, failures
}

// dirWritable 用「建临时文件 → 立即删除」探测目录可写（无持久副作用）。
// 空路径 / 不存在 / 指向普通文件 / 权限不足一律 false；不创建缺失目录（那是装配错误，
// 应在启动日志暴露而不是被探针悄悄修好）。
func dirWritable(dir string) bool {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return false
	}
	f, err := os.CreateTemp(dir, readyProbePrefix+"*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// metrics 输出 Prometheus 文本指标（每次抓取实时计算；1146 账号池 O(n) 毫秒级）。
// 标签只出现固定低基数（state）；uid/model 等不建标签，避免标签基数爆炸。
func (p *Panel) metrics(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", metricsContentType)
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(p.metricsText()))
}

// metricsText 渲染 Prometheus text exposition v0.0.4（手写，零第三方依赖）。
func (p *Panel) metricsText() string {
	var (
		healthy, cooling, disabled, paused int
		creditsSum, creditsTotalSum        int64
		inFlight, modelLocks               int
	)
	if p.cfg.Pool != nil {
		// 池总数不单独出指标：accounts_total 的四个 state 已能表达；逐号在途数需要
		// List() 快照累加（metrics 抓取频率低，O(n) 可接受；不为此新增 pool 接口，
		// 避免跨任务改动）。
		_, healthy, cooling, disabled, paused, _ = p.cfg.Pool.CountsDetailedWithPaused()
		creditsSum, creditsTotalSum = p.cfg.Pool.CreditsSums()
		modelLocks = len(p.cfg.Pool.ModelLockView())
		for _, st := range p.cfg.Pool.List() {
			inFlight += st.InFlight
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# HELP workbuddy_up Gateway/panel process is running (always 1 when scraped).\n")
	fmt.Fprintf(&b, "# TYPE workbuddy_up gauge\n")
	fmt.Fprintf(&b, "workbuddy_up 1\n")
	fmt.Fprintf(&b, "# HELP workbuddy_uptime_seconds Process uptime in seconds.\n")
	fmt.Fprintf(&b, "# TYPE workbuddy_uptime_seconds gauge\n")
	fmt.Fprintf(&b, "workbuddy_uptime_seconds %.0f\n", time.Since(p.started).Seconds())
	fmt.Fprintf(&b, "# HELP workbuddy_accounts_total Account pool size by state.\n")
	fmt.Fprintf(&b, "# TYPE workbuddy_accounts_total gauge\n")
	for _, st := range []struct {
		state string
		n     int
	}{
		{"healthy", healthy},
		{"cooling", cooling},
		{"disabled", disabled},
		{"paused", paused},
	} {
		fmt.Fprintf(&b, "workbuddy_accounts_total{state=%q} %d\n", st.state, st.n)
	}
	fmt.Fprintf(&b, "# HELP workbuddy_in_flight In-flight chat requests across the pool.\n")
	fmt.Fprintf(&b, "# TYPE workbuddy_in_flight gauge\n")
	fmt.Fprintf(&b, "workbuddy_in_flight %d\n", inFlight)
	fmt.Fprintf(&b, "# HELP workbuddy_credits_sum Sum of remaining credits across the pool.\n")
	fmt.Fprintf(&b, "# TYPE workbuddy_credits_sum gauge\n")
	fmt.Fprintf(&b, "workbuddy_credits_sum %d\n", creditsSum)
	fmt.Fprintf(&b, "# HELP workbuddy_credits_total_sum Sum of credit quotas across the pool (unknown/unlimited counted as 0).\n")
	fmt.Fprintf(&b, "# TYPE workbuddy_credits_total_sum gauge\n")
	fmt.Fprintf(&b, "workbuddy_credits_total_sum %d\n", creditsTotalSum)
	fmt.Fprintf(&b, "# HELP workbuddy_model_locks Number of model-level lock rows (models unusable on some accounts).\n")
	fmt.Fprintf(&b, "# TYPE workbuddy_model_locks gauge\n")
	fmt.Fprintf(&b, "workbuddy_model_locks %d\n", modelLocks)
	fmt.Fprintf(&b, "# HELP workbuddy_panel_requests_total Panel API requests since process start.\n")
	fmt.Fprintf(&b, "# TYPE workbuddy_panel_requests_total counter\n")
	fmt.Fprintf(&b, "workbuddy_panel_requests_total %d\n", p.apiRequests.Load())
	return b.String()
}
