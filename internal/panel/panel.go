// Package panel 内嵌式 Web 管理面板：账号池总览、单号运维（解冻/禁用/签到/
// 刷新余额/移除）、浏览器内 OAuth 添加账号（免重启热加载进池）、手动批量
// 签到/保活，以及运行日志环形缓冲（镜像 log 包与 chat 表格日志）。
//
// 设计约束：
//   - 前端 go:embed 单文件（index.html），无任何外部构建依赖，与二进制同体部署；
//   - 鉴权复用网关 api_key（Bearer），与 /v1/* 同一口径；api_key 为空 = 不鉴权
//     （仅本机/私网使用）。面板 HTML 本身无秘密，可匿名加载，密钥只发给 /panel/api/*；
//   - 不改写既有池语义：所有运维操作落到 pool 已有入口（Revive/Disable/Remove...），
//     添加账号走 auth.SaveAtomic + pool.Add，重启后与 auths/ 目录天然对齐。
package panel

import (
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/credithist"
	"github.com/lei-rr/workbuddy2api-pro/internal/httpauth"
	"github.com/lei-rr/workbuddy2api-pro/internal/jobs"
	"github.com/lei-rr/workbuddy2api-pro/internal/livecfg"
	"github.com/lei-rr/workbuddy2api-pro/internal/notify"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/reqlog"
	"github.com/lei-rr/workbuddy2api-pro/internal/scheduler"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
	"github.com/lei-rr/workbuddy2api-pro/internal/usage"
)

// Config 面板依赖（main 装配注入）。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	Scheduler *scheduler.Scheduler // 手动触发签到/保活；nil 时对应接口返回 501
	AuthDir   string               // OAuth 登录完成后凭证落盘目录
	APIKey    string               // 网关密钥（空 = 不鉴权）；PanelAPIKey 为空时面板也用它；与 Live 同时给出时 Live 优先
	// PanelAPIKey 面板独立密钥（v2.0.1 #4：与网关 api_key 分离）。非空时 /panel/*
	// 只认它，/v1/* 仍只认网关 api_key；空 = 回落网关密钥（向后兼容）。
	// 装配期读取，变更需重启进程（SaveConfig 的 restart_required 会列出 panel.api_key）。
	PanelAPIKey string
	// PollIntervalSec 前端轮询间隔（秒），经 /panel/api/overview 的 poll_interval_sec
	// 下发；Live 快照有值时以快照为准（保存配置热生效），否则用本静态值，<=0 回落 5。
	PollIntervalSec int
	// TaskSweepConcurrency 账号级扇出并发上限（v2.6.0 P0）：补领清扫/任务扫描/
	// 队列扫描/昵称同步/券码查询共用同一 worker 池大小；<=0 用默认 8，上限 64。
	// main 可接 schedule.task_sweep_concurrency；未接线时默认值即安全兜底。
	TaskSweepConcurrency int
	RedisMode            string // "upstash" / "noop"，仅观测透出
	Version              string // 面板版本号（展示用）

	// Live 运行期可变配置（在线改配置立即生效）。
	Live *livecfg.Holder

	// ConfigPath config.json 路径与加载器（配置页读写用）。
	// LoadConfig 返回解析后的配置对象（前端展示/校验用，具体类型由 main 注入的闭包决定）；
	// nil 时配置页返回 501。
	ConfigPath string
	LoadConfig func() (any, error)
	// SaveConfig 校验并落盘配置，返回需要重启才能生效的字段列表；随后由 main 注入的
	// ApplyConfig 闭包完成热生效（池参数/排程/密钥/脱敏）。error 时配置不写盘。
	SaveConfig func(raw []byte) (restartRequired []string, err error)
	// RollbackConfig 用上一版配置备份（config.json.bak）恢复并热应用，返回需要重启
	// 才能生效的字段列表（v2.2.0 #15：POST /panel/api/config/rollback）。nil 时回滚
	// 接口返回 501；无备份/备份非法等用户可纠正的错误由 handler 映射 400。
	RollbackConfig func() (restartRequired []string, err error)

	// StickyCount 返回粘性会话绑定数；nil 时报告 0。
	StickyCount func() int

	// Usage 逐请求用量记录器（nil = 用量接口返回 501）。
	Usage *usage.Recorder
	// RequestLog 请求指标与归档（nil = 对应接口返回 501）。
	RequestLog *reqlog.Recorder
	// CreditHistory 积分变动账本（nil = 积分历史接口返回 501）。只读展示：
	// 留痕由余额查询路径（upstream.Client 的余额观察者）写入。
	CreditHistory *credithist.Ledger

	// ProbeFile 模型输出上限探测结果文件（scripts/probe_max_tokens.py --panel-out
	// 写入；空或文件不存在 = model_probes 端点返回空集，面板不显示任何实测标注）。
	// 只读展示：网关不解析、不依赖其内容做任何路由/出站决策。
	ProbeFile string

	// StateFile state.json 路径（v2.2.0 #5/#9）：面板侧 JSONL 落盘目录由它推导
	// （审计 panel-audit.jsonl 与任务历史 task-runs.jsonl 与 state 同目录，
	// 与 usage/request-logs 同一口径）。为空时回落 ConfigPath 推导（见 panelDataDir）。
	// main.go 装配一行：StateFile: cfg.StateFile（由 Lead 合入；不改本包之外的代码）。
	StateFile string

	// Jobs 全池任务 Job Runner（v2.3.0 #10）。nil = 未装配：批量端点退回 v2.2 的
	// 异步实现（无 job_id），Job API 返回 501（嵌入式/老测试兼容）。
	// main.go 用 cfg.Jobs 创建（jobs.New），数据落 <state 同目录>/jobs.json。
	Jobs *jobs.Registry
	// Notify 事件通知器（v2.3.0 #19；nil/关闭状态时所有方法 no-op）。面板在
	// accountDisable 成功后 Emit account_disabled；Job 完成通知由 main 的
	// jobs.Config.OnFinish 接线（本包不感知 registry 回调）。
	Notify *notify.Notifier
}

// Panel 管理面板 handler。挂载方式：外层 mux Handle("/panel/", panel)，
// 本 mux 的 pattern 均带 /panel 前缀（外层不做前缀剥离）。
type Panel struct {
	cfg     Config
	mux     *http.ServeMux
	started time.Time
	logs    *Ring

	// authFailures 面板鉴权失败限速（v2.0.1 #4-3）：按来源 IP 的 token bucket，
	// 20 次/分钟，成功后清桶，内存 LRU 上限 1 万 IP。
	authFailures *authFailLimiter

	// logins 进行中的 OAuth 设备授权会话（state → 会话信息）。
	// poll 成功或超时（loginTTL）后剔除；面板常驻进程，容量天然有界。
	loginMu sync.Mutex
	logins  map[string]loginSession

	// taskMu/taskLocks 一键完成任务的 per-account 互斥：同一账号的任务动作
	// （单任务 / 全量）同时只允许一条在跑。重复点击直接返回 409"仍在执行"，
	// 而不是并发跑两遍浪费上游请求（动作虽幂等，expert 系每遍含 8 次真实对话）。
	// 不同账号之间不互斥（并行照旧）。TryLock 语义，锁条目常驻（账号数有界）。
	taskMu    sync.Mutex
	taskLocks map[string]*sync.Mutex

	// 任务中心执行队列（taskcenter.go）。
	queueOnce sync.Once
	q         *queueState

	// apiRequests 面板 API 请求计数（v2.2.0 #8：/panel/api/* 全部计入，含 401/429；
	// 仅作 /metrics 的 workbuddy_panel_requests_total 展示）。
	apiRequests atomic.Int64

	// accountsETag 账号列表 ETag/304 缓存（v2.5.0 Perf-3）：按归一化查询 LRU，
	// 池/元数据版本未变时 5s 轮询可直接 304（不构 rows），显著降低常态开销。
	accountsETag *accountsETagCache

	// dataDir 面板自有 JSONL 落盘目录（审计/任务历史与 state 文件同目录）；空 = 未装配，
	// 两项旁路功能自动关闭（测试与只读装配不产生副作用文件）。
	dataDir string
	// auditLog 操作审计 JSONL（v2.2.0 #5）；taskRunsLog 任务执行历史 JSONL（#9）。
	auditLog    *jsonlLog
	taskRunsLog *jsonlLog
}

// tryLockAccount 尝试锁定账号的任务执行；已在执行返回 false。
func (p *Panel) tryLockAccount(uid string) bool {
	p.taskMu.Lock()
	if p.taskLocks == nil {
		p.taskLocks = make(map[string]*sync.Mutex)
	}
	mu := p.taskLocks[uid]
	if mu == nil {
		mu = &sync.Mutex{}
		p.taskLocks[uid] = mu
	}
	p.taskMu.Unlock()
	return mu.TryLock()
}

// unlockAccount 释放账号任务锁（与 tryLockAccount 配对）。
func (p *Panel) unlockAccount(uid string) {
	p.taskMu.Lock()
	mu := p.taskLocks[uid]
	p.taskMu.Unlock()
	if mu != nil {
		mu.Unlock()
	}
}

// New 构建面板。
func New(cfg Config) *Panel {
	if cfg.RedisMode == "" {
		cfg.RedisMode = "noop"
	}
	// 面板侧 JSONL（审计/任务历史）落盘目录：与 state 文件同目录；未装配 StateFile 时
	// 回落 ConfigPath 推导；两者都空 = 功能关闭（不落盘，测试无副作用）。
	dataDir := panelDataDir(cfg.StateFile, cfg.ConfigPath)
	p := &Panel{
		cfg:          cfg,
		mux:          http.NewServeMux(),
		started:      time.Now(),
		logs:         NewRing(500),
		logins:       map[string]loginSession{},
		authFailures: newAuthFailLimiter(authFailLimiterMaxIPs),
		accountsETag: newAccountsETagCache(),
		dataDir:      dataDir,
		auditLog:     newJSONLLog(joinDataFile(dataDir, auditFileName)),
		taskRunsLog:  newJSONLLog(joinDataFile(dataDir, taskRunsFileName)),
	}
	p.routes()
	return p
}

// Logs 返回日志环形缓冲（main 经 MultiWriter 镜像 log 与 chat 表格日志进来）。
func (p *Panel) Logs() *Ring { return p.logs }

// ServeHTTP 统一入口：先写安全响应头，再按来源 IP 做鉴权失败限速前置，最后
// 用 gzip 包装写面板响应（仅面板——server 包的 /v1/* 不经过这里，SSE 不受影响）。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	// 压缩能力声明对所有面板响应生效（含未压缩的小响应与 304），便于反代/CDN 正确分桶缓存。
	w.Header().Set("Vary", "Accept-Encoding")
	// v2.6.0 P1：面板 API 默认 no-store。部分响应含密钥/口令/审计等敏感信息
	// （如 GET /panel/api/config 回显 api_key、panel.api_key、webhook/device_token），
	// 不应进入浏览器或中间缓存。accounts（private, no-cache + ETag）与 metrics/
	// events 会在各自 handler 内覆盖为更精确的值，304/ETag 缓存语义不变。
	if isPanelAPIPath(r.URL.Path) {
		w.Header().Set("Cache-Control", "no-store")
	}
	// v2.2.0 #8：面板 API 请求计数（/metrics 展示；含 401/429，反映真实到达量）。
	if isPanelAPIPath(r.URL.Path) {
		p.apiRequests.Add(1)
	}

	// 鉴权失败限速前置：被限的 IP 暂时连正确 key 也回 429，客户端按 Retry-After 退避。
	// 只作用于 /panel/api/*：页面本身无秘密、可匿名加载，不必一并掐掉（否则被限 IP
	// 连页面都刷新不了，体验上与"面板挂了"无异）。
	if isPanelAPIPath(r.URL.Path) && p.authFailures != nil {
		if blocked, retryAfter := p.authFailures.blocked(rateLimitIP(r)); blocked {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeErr(w, http.StatusTooManyRequests, "too many failed authentication attempts; retry after "+strconv.Itoa(retryAfter)+"s")
			return
		}
	}

	// SSE（v2.4.0 #3 /panel/api/events）必须绕过 gzip 缓冲：事件流需要立即 flush，
	// 不能等 close 时统一压缩（缓冲会让客户端收不到增量事件）。
	if r.URL.Path == "/panel/api/events" {
		p.mux.ServeHTTP(w, r)
		return
	}

	// 缓冲后统一决定是否压缩：面板无流式接口，缓冲让 Content-Length/1KB 阈值判断
	// 都能如实进行；HEAD/204/304/已编码响应在 close 里跳过压缩。
	// 缓冲 bytes.Buffer 来自 sync.Pool（v2.5.0 Perf-3）：大响应（10k 账号页/全量
	// overview）每次请求数 MB 的缓冲复用，避免反复扩容分配。
	gw := &gzipResponseWriter{ResponseWriter: w, req: r, buf: acquireGzipBuffer()}
	defer gw.close(acceptsGzip(r))
	p.mux.ServeHTTP(gw, r)
}

// withAuth 面板 Bearer 鉴权（经 httpauth 常量时间比较）：
//   - 密钥取 apiKey()（PanelAPIKey 优先，空则回落到网关 api_key / Live 快照）；
//   - 成功一次清空该 IP 的失败计数（v2.0.1 #4-3：正常用户不会因为隔壁暴力尝试被拖累）；
//   - 失败一次计入限速桶（容量 20/分钟，超限由 ServeHTTP 前置返回 429+Retry-After）。
func (p *Panel) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := rateLimitIP(r)
		if !httpauth.VerifyBearer(r, p.apiKey()) {
			if p.authFailures != nil {
				p.authFailures.fail(ip)
			}
			writeErr(w, http.StatusUnauthorized, "invalid_api_key")
			return
		}
		if p.authFailures != nil {
			p.authFailures.success(ip)
		}
		next(w, r)
	}
}

// apiKey 当前生效的面板密钥：PanelAPIKey（独立面板密钥，需重启）优先；
// 未配置时回落到网关 api_key（Live 快照优先于静态字段，保持既有热改语义）。
func (p *Panel) apiKey() string {
	if p.cfg.PanelAPIKey != "" {
		return p.cfg.PanelAPIKey
	}
	if p.cfg.Live != nil {
		return p.cfg.Live.Load().APIKey
	}
	return p.cfg.APIKey
}

// pollIntervalSec 面板前端轮询间隔（秒）：livecfg 快照优先（面板保存后热生效），
// 否则用装配期静态值；非法值回落默认 5。
func (p *Panel) pollIntervalSec() int {
	if p.cfg.Live != nil {
		if v := p.cfg.Live.Load().PollIntervalSec; v > 0 {
			return v
		}
	}
	if p.cfg.PollIntervalSec > 0 {
		return p.cfg.PollIntervalSec
	}
	return defaultPollIntervalSec
}

// ---------------------------------------------------------------------------
// HTTP 中间件：gzip 压缩 + 鉴权失败限速（v2.0.1 #2 / #4）
// ---------------------------------------------------------------------------

// defaultPollIntervalSec 面板前端默认轮询间隔（秒），与 cmd/server 的配置默认一致。
const defaultPollIntervalSec = 5
