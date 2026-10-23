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
	"bytes"
	"compress/gzip"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
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
	RedisMode       string // "upstash" / "noop"，仅观测透出
	Version         string // 面板版本号（展示用）

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

// loginTTL 授权 URL 的最长有效期：超时的 state 直接回收，
// 防止"开了添加账号弹窗就走开"的会话永久滞留。
const loginTTL = 15 * time.Minute

// loginSession 进行中的 OAuth 会话：创建时刻 + realm（cn/global，用于落盘与端点切换）。
type loginSession struct {
	created time.Time
	realm   string // "cn" / "global"，缺省 cn
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
		dataDir:      dataDir,
		auditLog:     newJSONLLog(joinDataFile(dataDir, auditFileName)),
		taskRunsLog:  newJSONLLog(joinDataFile(dataDir, taskRunsFileName)),
	}
	p.routes()
	return p
}

// Logs 返回日志环形缓冲（main 经 MultiWriter 镜像 log 与 chat 表格日志进来）。
func (p *Panel) Logs() *Ring { return p.logs }

func (p *Panel) routes() {
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)
	p.mux.HandleFunc("GET /panel/mobile.css", p.mobileStyle)
	p.mux.HandleFunc("GET /panel/mobile.js", p.mobileScript)

	// ---- 只读接口 ----
	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
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
	p.mux.HandleFunc("POST /panel/api/jobs/{kind}/run", p.withAuth(p.jobsRun))
	p.mux.HandleFunc("POST /panel/api/jobs/{id}/cancel", p.withAuth(p.jobsCancel))
	// v2.3.0 #12 账号备注/标签写操作（PUT 覆盖保存；DELETE 幂等删除）。
	p.mux.HandleFunc("PUT /panel/api/accounts/{uid}/meta", p.withAuth(p.accountMetaPut))
	p.mux.HandleFunc("DELETE /panel/api/accounts/{uid}/meta", p.withAuth(p.accountMetaDelete))
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

// ServeHTTP 统一入口：先写安全响应头，再按来源 IP 做鉴权失败限速前置，最后
// 用 gzip 包装写面板响应（仅面板——server 包的 /v1/* 不经过这里，SSE 不受影响）。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	// 压缩能力声明对所有面板响应生效（含未压缩的小响应与 304），便于反代/CDN 正确分桶缓存。
	w.Header().Set("Vary", "Accept-Encoding")
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

	// 缓冲后统一决定是否压缩：面板无流式接口，缓冲让 Content-Length/1KB 阈值判断
	// 都能如实进行；HEAD/204/304/已编码响应在 close 里跳过压缩。
	gw := &gzipResponseWriter{ResponseWriter: w, req: r}
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

// expiringSoonWindow 返回调度器当前生效的快过期路由窗口；测试面板无调度器时返回 0。
func (p *Panel) expiringSoonWindow() time.Duration {
	if p.cfg.Scheduler == nil {
		return 0
	}
	return p.cfg.Scheduler.ExpiringSoonWindow()
}

// poolExpiryBuckets 把 upstream 的按日到期批次转换为 pool 结构。两个包字段同构，
// 手工转换是为了不让底层 pool 反向依赖 upstream。
func poolExpiryBuckets(in []upstream.ExpiryBucket) []pool.ExpiryBucket {
	if len(in) == 0 {
		return nil
	}
	out := make([]pool.ExpiryBucket, len(in))
	for i, b := range in {
		out[i] = pool.ExpiryBucket{Date: b.Date, ExpireAt: b.ExpireAt, Remain: b.Remain}
	}
	return out
}

// ---------------------------------------------------------------------------
// 只读接口
// ---------------------------------------------------------------------------

// overview 总览：池计数 + 每账号状态 + 面板元信息。
//
// v2.1 #1：?lite=1 只回汇总（计数/元信息/model_locks/积分合计，约 1-2 KB），供前端
// 5s 轮询；不带 lite（默认）时除新增 credits_sum/credits_total_sum 两个汇总字段外
// 与旧版逐字段等价（含 accounts 全量）——旧脚本零破坏。
func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	// 用 WithPaused 而不是 CountsDetailed：后者把暂停号并进 disabled（/status 的
	// 「不可用」口径），而概况卡片要分开展示（issue #125）。
	total, healthy, cooling, disabled, paused, inFlightFull := p.cfg.Pool.CountsDetailedWithPaused()
	sticky := 0
	if p.cfg.StickyCount != nil {
		sticky = p.cfg.StickyCount()
	}
	// 积分合计（「积分剩余 / 总额」统计条）：v2.1 #1 前端改 lite 轮询后不再拿到
	// accounts 数组，无法本地求和；后端直接透出，lite 与全量都带。用 pool 的只读
	// 求和（O(n)、零分配）而不是 List() 遍历——lite 分支因此仍不构造全量快照。
	creditsSum, creditsTotalSum := p.cfg.Pool.CreditsSums()
	resp := map[string]any{
		"version":       p.cfg.Version,
		"uptime_sec":    int(time.Since(p.started).Seconds()),
		"auth_required": p.apiKey() != "",
		// poll_interval_sec 面板前端轮询间隔（秒，2-60；v2.0.1 #2）：前端据此安排
		// 5s 默认轮询并可在线改配置，后端不感知前端实现。
		"poll_interval_sec": p.pollIntervalSec(),
		"redis_mode":        p.cfg.RedisMode,
		"sticky_sessions":   sticky,
		"total":             total,
		"healthy":           healthy,
		"cooling":           cooling,
		"disabled":          disabled,
		"paused":            paused,
		"in_flight_full":    inFlightFull,
		// credits_sum = 全池 credits 之和；credits_total_sum = 全池总额度之和（未知
		// 0/企业无限量 -1 按 0 计）。与旧前端「从 accounts 数组求和」口径一致，
		// 全量 overview 下也恒有该字段（向后兼容，只增不减）。
		"credits_sum":       creditsSum,
		"credits_total_sum": creditsTotalSum,
		// model_locks 模型级限流全清单（哪些模型不能用、锁了几个号、还要锁多久）：
		// 与 accounts 的账号池视图互补，前端「模型锁池」表直接渲染。无锁时为 null。
		"model_locks": p.cfg.Pool.ModelLockView(),
		// 到期提醒快照的节奏与年龄：界面轮询是 5 秒级，但卡片数据只随余额后台刷新
		// 更新（默认每 5 分钟）——把这两个数透出，前端才能如实标注「x 分钟前更新」。
		"balance_refresh_sec":  p.balanceRefreshSec(),
		"balance_refreshed_at": p.balanceRefreshedAt(),
	}
	if !overviewLite(r) {
		resp["accounts"] = p.cfg.Pool.List()
	}
	writeJSON(w, http.StatusOK, resp)
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

// ---------------------------------------------------------------------------
// v2.1 #1：账号池列表服务端分页 / 筛选 / 排序 / 字段裁剪 / ETag
// ---------------------------------------------------------------------------

const (
	// accountsDefaultPageSize 默认页大小（1146 账号全量约 689 KB；100 行 light 约
	// 数十 KB，配合 gzip 后 8-15 KB 量级）。
	accountsDefaultPageSize = 100
	// accountsMaxPageSize 服务端页大小上限：page_size=0（「全部」）与 >上限 的请求
	// 都按它截断，防止单个响应无限大。
	accountsMaxPageSize = 2000
	// accountsFieldsLight / accountsFieldsFull 字段投影档位（默认 light）。
	accountsFieldsLight = "light"
	accountsFieldsFull  = "full"
	// accountsCacheControl 账号列表缓存策略：must-revalidate（每次带 ETag 回源校验，
	// 命中 304 零 body）；private 因为响应依赖面板密钥鉴权，禁止共享缓存跨用户复用。
	accountsCacheControl = "private, no-cache"
)

// accountsQuery 是 /panel/api/accounts 的归一化查询参数。
// 归一化后同时用于筛选/排序/分页与 ETag 输入：语义相同的原始写法（大小写、
// 默认值/显式值、越界值）映射到同一份参数，从而共享缓存条目。
type accountsQuery struct {
	Page       int    `json:"page"`       // >=1；默认 1
	PageSize   int    `json:"page_size"`  // 默认 100；0 = 全部（受 accountsMaxPageSize 截断）
	Q          string `json:"q"`          // 已 TrimSpace + ToLower：匹配 uid/nickname 包含
	State      string `json:"state"`      // "" | healthy | cooling | paused | disabled
	Realm      string `json:"realm"`      // "" | cn | global
	Enterprise string `json:"enterprise"` // "" | "1" | "0"
	Sort       string `json:"sort"`       // uid（默认）| credits | last_success | cool_remaining
	Fields     string `json:"fields"`     // light（默认）| full
}

// parseAccountsQuery 解析并归一化查询参数。非法值一律回落默认（不 400）：
// 列表是只读观测接口，容忍前端/脚本传脏参数比回错更容易排障。
func parseAccountsQuery(r *http.Request) accountsQuery {
	qv := r.URL.Query()
	q := accountsQuery{
		Page:       1,
		PageSize:   accountsDefaultPageSize,
		Q:          strings.ToLower(strings.TrimSpace(qv.Get("q"))),
		State:      normalizeAccountsState(qv.Get("state")),
		Realm:      normalizeAccountsRealm(qv.Get("realm")),
		Enterprise: normalizeAccountsEnterprise(qv.Get("enterprise")),
		Sort:       normalizeAccountsSort(qv.Get("sort")),
		Fields:     normalizeAccountsFields(qv.Get("fields")),
	}
	if raw := strings.TrimSpace(qv.Get("page")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			q.Page = n
		}
	}
	if vals, ok := qv["page_size"]; ok && len(vals) > 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(vals[0])); err == nil && n >= 0 {
			switch {
			case n == 0:
				q.PageSize = 0 // 全部（服务端再按 accountsMaxPageSize 截断）
			case n > accountsMaxPageSize:
				q.PageSize = accountsMaxPageSize
			default:
				q.PageSize = n
			}
		}
	}
	return q
}

// normalizeAccountsState 归一化状态筛选；未知值 = 不筛选（全部）。
func normalizeAccountsState(v string) string {
	switch s := strings.ToLower(strings.TrimSpace(v)); s {
	case "healthy", "cooling", "paused", "disabled":
		return s
	}
	return ""
}

// normalizeAccountsRealm 归一化域筛选；未知值 = 不筛选。
func normalizeAccountsRealm(v string) string {
	switch s := strings.ToLower(strings.TrimSpace(v)); s {
	case "cn", "global":
		return s
	}
	return ""
}

// normalizeAccountsEnterprise 归一化企业版筛选（1/0，兼容 true/false 写法）；
// 空/未知值 = 不筛选。
func normalizeAccountsEnterprise(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "" {
		return ""
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return ""
	}
	if b {
		return "1"
	}
	return "0"
}

// normalizeAccountsSort 归一化排序键；未知值回落默认 uid 升序。
func normalizeAccountsSort(v string) string {
	switch s := strings.ToLower(strings.TrimSpace(v)); s {
	case "credits", "last_success", "cool_remaining":
		return s
	}
	return "uid"
}

// normalizeAccountsFields 归一化字段投影；只有显式 full 才返回全量，其余一律 light。
func normalizeAccountsFields(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), accountsFieldsFull) {
		return accountsFieldsFull
	}
	return accountsFieldsLight
}

// accountsMatch 报告账号是否满足筛选条件。
func accountsMatch(st pool.Status, q accountsQuery, now time.Time) bool {
	if q.Q != "" &&
		!strings.Contains(strings.ToLower(st.UID), q.Q) &&
		!strings.Contains(strings.ToLower(st.Nickname), q.Q) {
		return false
	}
	if q.Realm != "" && st.Realm != q.Realm {
		return false
	}
	if q.Enterprise != "" && (q.Enterprise == "1") != st.Enterprise {
		return false
	}
	if q.State != "" && accountsState(st, now) != q.State {
		return false
	}
	return true
}

// accountsState 单值状态归类，优先级 disabled > paused > cooling > healthy，
// 与 pool.CountsDetailedWithPaused 的四个计数桶口径一致：
//   - cooling = 未禁用未暂停且不可选：账号级冷却（until）/熔断期（breaker_until）/
//     连败降权期（degrade_until），即 pool.healthy 判 false 的全部原因；
//   - healthy = pool.healthy 判 true（注意 in_flight 维度不算，与 CountsDetailed 一致）。
func accountsState(st pool.Status, now time.Time) string {
	switch {
	case st.Disabled:
		return "disabled"
	case st.Paused:
		return "paused"
	case st.Cooling || (!st.DegradeUntil.IsZero() && now.Before(st.DegradeUntil)):
		// Status.Cooling 只覆盖 until/breakerUntil；pool.healthy 还看 degradeUntil，
		// 这里补上降权期，保证 state=healthy 不会把降权号算进来。
		return "cooling"
	default:
		return "healthy"
	}
}

// sortAccounts 按归一化排序键就地排序（SliceStable + uid 兜底，保证同值输出确定，
// ETag 才能稳定）：
//   - uid：升序（默认，与 pool.List() 的自然顺序一致）；
//   - credits：降序（余额多的在前），同额按 uid 升序；
//   - last_success：降序（最近成功在前），从未成功的零值恒排最后，同刻按 uid 升序；
//   - cool_remaining：升序（剩余冷却秒数少的在前；未冷却为 0 排最前），同值按 uid 升序。
func sortAccounts(rows []pool.Status, sortBy string) {
	switch sortBy {
	case "credits":
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Credits != rows[j].Credits {
				return rows[i].Credits > rows[j].Credits
			}
			return rows[i].UID < rows[j].UID
		})
	case "last_success":
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := rows[i].LastSuccessTime, rows[j].LastSuccessTime
			if !a.Equal(b) {
				if a.IsZero() {
					return false
				}
				if b.IsZero() {
					return true
				}
				return a.After(b)
			}
			return rows[i].UID < rows[j].UID
		})
	case "cool_remaining":
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].CoolRemaining != rows[j].CoolRemaining {
				return rows[i].CoolRemaining < rows[j].CoolRemaining
			}
			return rows[i].UID < rows[j].UID
		})
	default: // uid
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].UID < rows[j].UID })
	}
}

// pageAccounts 按 page/page_size 切页，返回当前页、生效页码与总页数。
//
// page_size<=0 表示「全部」模式：返回前 min(len, accountsMaxPageSize) 行，
// page_size 语义保持 0（响应回显 0，前端据此知道不是普通分页），page 规范化为 1，
// pages 为 0（空结果）/1。
//
// 普通模式下超界 page 自动钳到最后一页（响应 page 为钳后值，前端以响应为准）：
// 数据随状态变化减少页数时，轮询不会停在一个永远空白的页码上；空结果 pages=0、
// page=1。空切片保持非 nil，JSON 输出 [] 而不是 null。
func pageAccounts(rows []pool.Status, page, pageSize int) (pageRows []pool.Status, effPage, pages int) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		if len(rows) == 0 {
			return rows[:0], 1, 0
		}
		if len(rows) > accountsMaxPageSize {
			return rows[:accountsMaxPageSize], 1, 1
		}
		return rows, 1, 1
	}
	pages = (len(rows) + pageSize - 1) / pageSize
	if pages == 0 {
		return rows[:0], 1, 0
	}
	if page > pages {
		// 钳到最后一页：此后 (page-1)*pageSize < len(rows)，无溢出风险
		// （page 可能来自任意大的整数，先钳再乘）。
		page = pages
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if end > len(rows) {
		end = len(rows)
	}
	return rows[start:end], page, pages
}

// lightStatus 返回 light 字段投影：把两个「重/易变、列表页用不到」的字段置零
// （两者都是 omitempty，置零即从 JSON 省略）：
//   - credits_expiry_buckets：按日到期批次，可变长数组，只属「积分到期」快照；
//   - cool_remaining_sec：秒级倒计时，前端改用 until / breaker_until / degrade_until
//     绝对时间 + 响应里的 now 在本地倒计时（否则 304 会让倒计时冻结）。
//
// 其余字段与 /overview 的 accounts 逐字段同构（含 token_usage / model_costs /
// rate_limited_models），前端渲染列表无需字段回退。
func lightStatus(st pool.Status) pool.Status {
	st.CreditsExpiryBuckets = nil
	st.CoolRemaining = 0
	return st
}

// stableStatusForETag 返回 ETag 输入用的稳定副本：把「随秒/随请求瞬变，但不足以
// 让列表内容发生有意义变化」的字段规范化为零值，避免 5s 轮询在无运维状态变化时
// ETag 抖动（这些字段变化时若换 ETag，缓存永远命不中）：
//   - cool_remaining_sec：按 now 计算的秒级倒计时；
//   - in_flight：在途并发瞬时值；
//   - breaker_fails：熔断器运行期失败计数（每次请求都可能 +1/清零）；
//   - token_usage.last_used_at：最近一次请求时刻。
//
// 其它字段（credits/until/token 计数/model_costs/...）是有意义的内容变化，照常
// 参与 hash。代价：命中 304 时浏览器复用旧 body，其中倒计时/在途可能是旧值——
// 倒计时本就应由 until 本地重算，在途是瞬时观测，口径可接受（契约见任务 #1）。
func stableStatusForETag(st pool.Status) pool.Status {
	st.CoolRemaining = 0
	st.InFlight = 0
	st.BreakerFails = 0
	st.TokenUsage.LastUsedAt = time.Time{}
	return st
}

// accountsProjection 返回响应体行（light 省略两个字段；full 完整 Status）。
// 始终返回非 nil 切片：空页 JSON 输出 [] 而不是 null。
func accountsProjection(pageRows []pool.Status, fields string) []pool.Status {
	rows := make([]pool.Status, len(pageRows))
	if fields == accountsFieldsFull {
		copy(rows, pageRows)
		return rows
	}
	for i, st := range pageRows {
		rows[i] = lightStatus(st)
	}
	return rows
}

// accountsStableProjection 返回 ETag hash 输入行：在响应投影基础上再把易变字段
// 归零（见 stableStatusForETag）。
func accountsStableProjection(pageRows []pool.Status, fields string) []pool.Status {
	rows := make([]pool.Status, len(pageRows))
	for i, st := range pageRows {
		if fields != accountsFieldsFull {
			st = lightStatus(st)
		}
		rows[i] = stableStatusForETag(st)
	}
	return rows
}

// accountsETagInput 是 ETag 的完整 hash 输入：归一化查询参数 + 计数/分页元信息 +
// 稳定投影行。rows 参与 hash 保证「内容变了 ETag 必变」；查询参数参与保证不同
// 页/筛选/排序/投影各有独立缓存条目。
type accountsETagInput struct {
	Query    accountsQuery `json:"query"`
	Total    int           `json:"total"`
	Filtered int           `json:"filtered"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
	Pages    int           `json:"pages"`
	Rows     []pool.Status `json:"rows"`
}

// accountsETag 计算弱 ETag `W/"acct-<sha256 hex>"`。
// 弱校验适合「语义等价即可复用」的列表响应；hex 输入是无 key 的 JSON
// （encoding/json 对结构体字段顺序确定，无 map 乱序问题）。
//
// ok=false 表示稳定投影无法 JSON 序列化（例如极端脏数据里的 NaN/Inf 浮点）：
// 此时调用方必须跳过 ETag/304——绝不能返回一个恒定 ETag，否则后续所有轮询都会
// 命中 304 拿到过期数据，比没有缓存更糟。
func accountsETag(in accountsETagInput) (etag string, ok bool) {
	raw, err := json.Marshal(in)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(raw)
	return `W/"acct-` + hex.EncodeToString(sum[:]) + `"`, true
}

// accounts 账号池列表（v2.1 #1）：服务端筛选/排序/分页/字段裁剪 + ETag/304。
//
// 数据源与 /overview 的 accounts 相同（pool.List() 快照，锁外筛选/排序/序列化），
// 但只序列化当前页——旧接口默认行为完全不变，本接口供账号页按需拉取：
//
//	GET /panel/api/accounts?page=1&page_size=100&q=&state=&realm=&enterprise=&sort=&fields=
//
// 响应：{"ok":true,"now":<unix秒>,"total":N,"filtered":M,"page":1,"page_size":100,
// "pages":P,"rows":[Status...]}。now 供前端校正时钟后在本地按 until 倒计时。
// 超界 page 钳到最后一页、page_size=0 时 page 规范化 1（均以响应 page 为准）。
// 带 If-None-Match 且命中稳定投影 ETag 时返回 304 空 body（保留 ETag/
// Cache-Control/Vary），5s 轮询在无状态变化时零 body。
func (p *Panel) accounts(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	q := parseAccountsQuery(r)

	var snapshot []pool.Status
	if p.cfg.Pool != nil {
		snapshot = p.cfg.Pool.List()
	}
	total := len(snapshot)
	filtered := make([]pool.Status, 0, len(snapshot))
	for _, st := range snapshot {
		if accountsMatch(st, q, now) {
			filtered = append(filtered, st)
		}
	}
	sortAccounts(filtered, q.Sort)
	pageRows, effPage, pages := pageAccounts(filtered, q.Page, q.PageSize)
	q.Page = effPage // 生效页码（越界钳到末页 / 全部模式规范化 1）

	// 先算 ETag（只序列化稳定投影）：304 命中时连响应 body 都省掉，是 5s 轮询常态。
	// q.Page 已替换为生效页码，语义等价的越界请求（如 page=99 钳到末页）共享 ETag。
	h := w.Header()
	h.Set("Cache-Control", accountsCacheControl)
	if etag, ok := accountsETag(accountsETagInput{
		Query:    q,
		Total:    total,
		Filtered: len(filtered),
		Page:     q.Page,
		PageSize: q.PageSize,
		Pages:    pages,
		Rows:     accountsStableProjection(pageRows, q.Fields),
	}); ok {
		h.Set("ETag", etag)
		if ifNoneMatch(r, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"now":       now.Unix(),
		"total":     total,
		"filtered":  len(filtered),
		"page":      q.Page,
		"page_size": q.PageSize,
		"pages":     pages,
		"rows":      accountsProjection(pageRows, q.Fields),
	})
}

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
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
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
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
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

// models 实时查询上游模型列表与 reasoning 实际档位（直连上游，不读路由层 1h 缓存）：
// 回答"该模型到底支持哪几档思考"。顺带刷新 client 的 effort 降级能力缓存。
// 与 /v1/models 同口径的双域输出：CN 域模型加 "cn:" 前缀、global 域加 "global:" 前缀
// （gateway 路由协议，前端显示的 id 就是调用时要填的完整 model 值）。
// 各域独立探测、独立容错：某域无可用账号则整域跳过；两域全空时才报错
// （有错误明细回 502，一个账号都没有回 503）。
func (p *Panel) models(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0)
	var fetchErrs []string

	// CN 域：有可用 CN 账号才查（此前无条件 Pool.Pick()+FetchModels——选中 global
	// 账号时打 CN 端点必然失败，混合池表现为偶发 502，纯 global 池必炸）。
	if uids := p.cfg.Pool.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if acct := p.cfg.Pool.AuthByUID(uids[0]); acct != nil {
			infos, err := p.cfg.Upstream.FetchModels(acct)
			if err != nil {
				fetchErrs = append(fetchErrs, "cn: "+err.Error())
			} else {
				for _, mi := range infos {
					out = append(out, panelModelEntry("cn", mi, mi.Efforts, mi.DefaultEffort, p.cfg.Upstream.HTTP))
				}
			}
		}
	}

	// global 域：路由开关开且有可用 global 账号才查（独立目录端点，FetchGlobalModelInfos；
	// Upstream.GlobalEnabled 是探测侧同一道闸，与 main 装配的 config global.enabled 一致）。
	if p.cfg.Upstream.GlobalEnabled {
		if uids := p.cfg.Pool.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if acct := p.cfg.Pool.AuthByUID(uids[0]); acct != nil {
				infos := p.cfg.Upstream.FetchGlobalModelInfos(acct)
				if len(infos) == 0 {
					fetchErrs = append(fetchErrs, "global: 上游未返回可用模型")
				} else {
					efforts, defaults := p.cfg.Upstream.GlobalEffortSnapshot()
					for _, mi := range infos {
						out = append(out, panelModelEntry("global", mi, efforts[mi.ID], defaults[mi.ID], p.cfg.Upstream.HTTP))
					}
				}
			}
		}
	}

	if len(out) == 0 {
		if len(fetchErrs) > 0 {
			writeErr(w, http.StatusBadGateway, "fetch models: "+strings.Join(fetchErrs, "; "))
			return
		}
		writeErr(w, http.StatusServiceUnavailable, "没有可用账号：请先在面板添加账号再查询")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": out})
}

// panelModelEntry 构造单个模型条目（两域共用）：id 带 realm 前缀（调用值即显示值），
// context_length / max_output_tokens 走四级查找链，effort 档位按 realm 域取
// EffortListing（远端权威 ∪ 静态兜底表）——与 /v1/models 同一口径，两侧不再漂移。
func panelModelEntry(realm string, mi upstream.ModelInfo, remoteEfforts []string, remoteDefault string, httpc *http.Client) map[string]any {
	entry := map[string]any{
		"id":                   realm + ":" + mi.ID,
		"name":                 mi.Name,
		"default_effort":       mi.DefaultEffort,
		"supported_efforts":    mi.Efforts,
		"can_disable_thinking": mi.CanDisableThinking,
		"supports_reasoning":   mi.SupportsReasoning,
		"supports_images":      mi.SupportsImages,
		"credits":              mi.Credits,
		"description":          mi.Description,
		"tags":                 mi.Tags,
		"vendor":               mi.Vendor,
		"is_default":           mi.IsDefault,
		"supports_tool_call":   mi.SupportsToolCall,
		"only_reasoning":       mi.OnlyReasoning,
		"reasoning_effort":     mi.ReasoningEffort,
		"reasoning_summary":    mi.ReasoningSummary,
	}
	// 限时优惠（modelPromotions）：credits 是牌价，promo_* 是当前生效折扣
	//（WorkBuddy 客户端显示的就是这个生效价）。前端据此显示「生效价+标签+划线牌价」。
	if mi.PromoFactor != nil {
		entry["promo_factor"] = *mi.PromoFactor
		entry["promo_credits"] = mi.PromoCredits
	}
	if mi.PromoLabel != "" {
		entry["promo_label"] = mi.PromoLabel
	}
	if mi.PromoNote != "" {
		entry["promo_note"] = mi.PromoNote
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, httpc)
	if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, httpc); ok {
		entry["max_output_tokens"] = mo
	}
	if efforts, def := upstream.EffortListing(realm, mi.ID, remoteEfforts, remoteDefault); efforts != nil {
		entry["supported_efforts"] = efforts
		if def != "" {
			entry["default_effort"] = def
		}
	}
	return entry
}

// modelProbes 返回模型输出上限的探测结果（scripts/probe_max_tokens.py --panel-out
// 写入的契约文件），供前端在「模型与档位」的实测列做风险标注。
//
// 设计边界：纯只读透传——文件缺失/未配置返回空集（面板退化为无标注，与历史行为
// 一致），网关自身不解析字段语义、不据此做任何路由或出站决策；上游改了限制后
// 重跑一次工具、下次查询即刷新，无需重启网关。
func (p *Panel) modelProbes(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"probes": map[string]json.RawMessage{}, "exists": false}
	if p.cfg.ProbeFile == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	raw, err := os.ReadFile(p.cfg.ProbeFile)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, out)
			return
		}
		writeErr(w, http.StatusInternalServerError, "read probes: "+err.Error())
		return
	}
	var f struct {
		Version int                        `json:"version"`
		Probes  map[string]json.RawMessage `json:"probes"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		writeErr(w, http.StatusBadGateway, "parse probes: "+err.Error())
		return
	}
	if f.Probes == nil {
		f.Probes = map[string]json.RawMessage{}
	}
	out["probes"] = f.Probes
	out["exists"] = true
	if fi, err := os.Stat(p.cfg.ProbeFile); err == nil {
		out["updated_at"] = fi.ModTime().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// 账号运维
// ---------------------------------------------------------------------------

// accountRevive 手动复活：清禁用 + 冷却 + 熔断（运维口径无条件恢复）。
func (p *Panel) accountRevive(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Revive(uid)
	log.Printf("panel: revive uid=%s（人工清除禁用/冷却/熔断）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountDisable 人工禁用（不再参与选号，需面板 revive 或重登恢复）。
func (p *Panel) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Disable(uid, "manual disable (panel)")
	// v2.3.0 #19：账号被禁用 → Emit account_disabled（Notifier 关闭时 no-op；uid 脱敏）。
	if p.cfg.Notify != nil {
		p.cfg.Notify.AccountDisabled(shortUID(uid), "manual disable (panel)")
	}
	log.Printf("panel: disable uid=%s（人工禁用）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountPause 暂停选号：账号退出选号候选，但**照常参与**签到 / 活跃上报 / 保活 /
// 余额刷新。与 disable 的区别：不写 reason、不清冷却域、不重置计数——账号是「临时
// 让位」而非「判死」，点「恢复选号」即可立刻回到池子（无需重登或解冻）。
func (p *Panel) accountPause(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !p.cfg.Pool.Pause(uid) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	log.Printf("panel: pause uid=%s（暂停选号，保号任务照常）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountResume 解除暂停选号（幂等，对未暂停账号为空操作）。
func (p *Panel) accountResume(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !p.cfg.Pool.Resume(uid) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	log.Printf("panel: resume uid=%s（恢复参与选号）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountsRescan 按 auths/ 目录重新对齐账号池：外部脚本批量导入/删除凭证文件后
// 免重启生效。只增删账号，已存在账号的积分/冷却/计数等状态保留（pool.SyncToDir 语义）。
func (p *Panel) accountsRescan(w http.ResponseWriter, r *http.Request) {
	if p.cfg.AuthDir == "" {
		writeErr(w, http.StatusNotImplemented, "auth_dir not configured")
		return
	}
	before := make(map[string]bool, 1024)
	for _, s := range p.cfg.Pool.List() {
		before[s.UID] = true
	}
	auths, err := auth.LoadDir(p.cfg.AuthDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "scan auth dir: "+err.Error())
		return
	}
	// 扫到 0 个文件而池非空 = 目录挂载丢失或路径配错。此时 SyncToDir 会清空整池
	// 并把结果落盘（不可逆），必须拒绝，交给人去确认挂载。
	if len(auths) == 0 && len(before) > 0 {
		writeErr(w, http.StatusConflict, "扫描到 0 个凭证文件（疑似 auth_dir 挂载异常），已拒绝执行")
		return
	}
	p.cfg.Pool.SyncToDir(auths)
	after := make(map[string]bool, len(auths))
	added := 0
	for _, s := range p.cfg.Pool.List() {
		after[s.UID] = true
		if !before[s.UID] {
			added++
		}
	}
	removed := 0
	for uid := range before {
		if !after[uid] {
			removed++
		}
	}
	log.Printf("panel: rescan auths 目录 文件=%d 新增=%d 剔除=%d", len(auths), added, removed)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "scanned": len(auths), "added": added, "removed": removed,
	})
}

// accountCheckin 单号签到：DailyCheckin + 余额查询解冻（已签到等业务错误不阻塞余额刷新），
// 与 scheduler.RunCheckinNow 的单号语义一致。
//
// v2.2.0 #9：收尾写一条任务执行历史（kind=checkin，trigger=manual，total=1）——
// 企业版（无签到体系）记 skip，上游拒绝记 fail，成功/幂等已签记 done。
func (p *Panel) accountCheckin(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	checkinMsg := ""
	checkinDone := false
	started := time.Now()
	fail, skip := 0, 0
	defer func() {
		done := 0
		if fail == 0 && skip == 0 {
			done = 1
		}
		errMsg := ""
		if fail > 0 {
			errMsg = checkinMsg
		}
		p.recordTaskRun(TaskRun{
			Kind: taskKindCheckin, Trigger: taskTriggerManual, UID: uid,
			StartedAt: started.Unix(), FinishedAt: time.Now().Unix(),
			Total: 1, Done: done, Fail: fail, Skip: skip, Error: errMsg,
		})
	}()
	// 企业版无签到体系（上游 400 code 10001「企业账号不支持该操作」）：不发起该调用，
	// 端点退化为「刷新额度」——企业额度走 get-enterprise-user-usage 口径（见 upstream）。
	// 前端对企业号不渲染「签到」按钮；此处是 API 侧防御（外部脚本/旧缓存前端仍可能调用）。
	if a.IsEnterprise() {
		skip = 1
		checkinMsg = "企业账号无签到体系（已跳过签到，仅刷新额度）"
	} else if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
		checkinMsg = err.Error() // "今天已签到"等业务错误照常查余额
		// 幂等拒绝同样是「今日已签」，标记后按钮显示「已签」。
		if upstream.IsAlreadyCheckin(err) {
			p.cfg.Pool.NoteCheckinDone(uid)
			checkinDone = true
		} else {
			fail = 1
		}
	} else {
		p.cfg.Pool.NoteCheckinDone(uid)
		checkinDone = true
	}
	resp := map[string]any{"ok": true, "checkin_done": checkinDone}
	if a.IsEnterprise() {
		resp["enterprise"] = true
	}
	if checkinMsg != "" {
		resp["checkin_message"] = checkinMsg
	}
	remain, total, expiring, earliestAt, earliestRemaining, buckets, err := p.cfg.Upstream.UserResourceSnapshot(a, p.expiringSoonWindow())
	if err != nil {
		resp["balance_error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	p.cfg.Pool.ReenableIfCredits(uid, remain, total)
	p.cfg.Pool.SetCreditsSnapshot(uid, pool.CreditsSnapshot{
		Remain:            remain,
		Total:             total,
		Expiring:          expiring,
		EarliestAt:        earliestAt,
		EarliestRemaining: earliestRemaining,
		ExpiryBuckets:     poolExpiryBuckets(buckets),
	})
	resp["credits"] = remain
	resp["credits_total"] = total
	log.Printf("panel: checkin uid=%s msg=%q credits=%d/%d", uid, checkinMsg, remain, total)
	writeJSON(w, http.StatusOK, resp)
}

// accountBalance 单号余额刷新：更新余额与到期快照，不触碰冷却状态。
// v2.2.0 #9：收尾写一条任务执行历史（kind=balance，trigger=manual，total=1）。
func (p *Panel) accountBalance(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	started := time.Now()
	refreshErr := ""
	defer func() {
		done, fail := 0, 0
		if refreshErr == "" {
			done = 1
		} else {
			fail = 1
		}
		p.recordTaskRun(TaskRun{
			Kind: taskKindBalance, Trigger: taskTriggerManual, UID: uid,
			StartedAt: started.Unix(), FinishedAt: time.Now().Unix(),
			Total: 1, Done: done, Fail: fail, Error: refreshErr,
		})
	}()
	remain, total, expiring, earliestAt, earliestRemaining, buckets, err := p.cfg.Upstream.UserResourceSnapshot(a, p.expiringSoonWindow())
	if err != nil {
		refreshErr = err.Error()
		writeErr(w, http.StatusBadGateway, "user resource: "+err.Error())
		return
	}
	p.cfg.Pool.SetCreditsSnapshot(uid, pool.CreditsSnapshot{
		Remain:            remain,
		Total:             total,
		Expiring:          expiring,
		EarliestAt:        earliestAt,
		EarliestRemaining: earliestRemaining,
		ExpiryBuckets:     poolExpiryBuckets(buckets),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credits": remain, "credits_total": total})
}

// accountRemove 移除账号：先出池（立即落盘 state），再删 auth 文件。
func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.Remove(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	// v2.3.0 #12：账号已出池 → 同步清理其备注/标签（accountmeta.go）。
	// 放在 Remove 之后、两个成功响应分支之前，保证「删号但备注残留」不会发生。
	p.accountMetaForget(uid)
	fileMsg := ""
	if a.FilePath != "" {
		if err := os.Remove(a.FilePath); err != nil && !os.IsNotExist(err) {
			fileMsg = err.Error()
		}
	}
	if fileMsg != "" {
		log.Printf("panel: remove uid=%s（auth 文件删除失败: %s）", uid, fileMsg)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "file_error": fileMsg})
		return
	}
	log.Printf("panel: remove uid=%s（已出池并删除凭证文件）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 批量任务（v2.0.1 #10：同 kind 单飞 + 异步 + 409）
//
// 五个 *_all 端点共用调度器的任务单飞 gate（scheduler.BeginTask/EndTask）：
// 置位成功 → 起后台 goroutine 执行并立即返回 {ok:true,started:true}；置位失败
// （同类全池任务正在执行，手动双击或定时批次撞车）→ 409 + {"error":"…正在执行中"}。
// 前端 api() 会把非 2xx 的 error 文案原样 toast（frontend2 已接 409 文案）。
//
// balance_all 过去是「同步等全池刷完再返回 + 随响应带回 accounts」——1146 号池
// 会让反代 60s 超时（504），且与 #10 的并发闸门叠加时页面长时间无反馈。现改为
// 异步：先占 gate → 立即返回 → 后台按有界并发跑完（含昵称同步），前端 2.5s 后
// 补拉 overview 看到首批余额，其余交给常规轮询。旧响应字段 ok/started 保留。
// ---------------------------------------------------------------------------

// writeBatchConflict 已是同类任务执行中：409 + 与其它面板冲突接口同口径的 error 文案。
func writeBatchConflict(w http.ResponseWriter, msg string) {
	writeErr(w, http.StatusConflict, msg)
}

// checkinAll 手动触发全量签到（异步执行，进度看日志区/账号状态变化）。
//
// v2.3.0 #10：统一经 Job Runner 起 Job——先占 scheduler 同 kind 单飞 gate（409
// 语义不变），再注册 Job（返回 job_id，可经 GET /jobs/{id} 看进度/取消）。
// 旧响应字段 ok/started 保留；Registry 未装配时退回 v2.2 异步实现（无 job_id）。
func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	spec, ok := p.jobSpecFor(jobs.KindCheckin)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	jobID, started := p.triggerJob(spec)
	if !started {
		writeBatchConflict(w, spec.conflict)
		return
	}
	log.Printf("panel: 手动全量签到已触发（含连登管家；job=%s）", jobID)
	writeJSON(w, http.StatusOK, batchStartedBody(jobID))
}

// travelAll 手动触发全量猫猫旅行巡检（v2.3.0 #10 Job Runner，响应附 job_id）。
func (p *Panel) travelAll(w http.ResponseWriter, r *http.Request) {
	spec, ok := p.jobSpecFor(jobs.KindTravel)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	jobID, started := p.triggerJob(spec)
	if !started {
		writeBatchConflict(w, spec.conflict)
		return
	}
	log.Printf("panel: 手动全量旅行巡检已触发（job=%s）", jobID)
	writeJSON(w, http.StatusOK, batchStartedBody(jobID))
}

// activityAll 手动触发全量活跃上报（v2.3.0 #10 Job Runner，响应附 job_id）。
func (p *Panel) activityAll(w http.ResponseWriter, r *http.Request) {
	spec, ok := p.jobSpecFor(jobs.KindActivity)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	jobID, started := p.triggerJob(spec)
	if !started {
		writeBatchConflict(w, spec.conflict)
		return
	}
	log.Printf("panel: 手动全量活跃上报已触发（job=%s）", jobID)
	writeJSON(w, http.StatusOK, batchStartedBody(jobID))
}

// keepaliveAll 手动触发全量 token 保活（v2.3.0 #10 Job Runner，响应附 job_id）。
func (p *Panel) keepaliveAll(w http.ResponseWriter, r *http.Request) {
	spec, ok := p.jobSpecFor(jobs.KindKeepalive)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	jobID, started := p.triggerJob(spec)
	if !started {
		writeBatchConflict(w, spec.conflict)
		return
	}
	log.Printf("panel: 手动全量保活已触发（job=%s）", jobID)
	writeJSON(w, http.StatusOK, batchStartedBody(jobID))
}

// balanceAll 手动全量刷新余额（v2.3.0 #10 Job Runner，响应附 job_id）。
// 后台按 schedule.balance_refresh_concurrency 有界并发查上游、写回池内 credits
// （含解冻语义）并同步昵称；重复点击/周期刷新撞车 → 409。
func (p *Panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	spec, ok := p.jobSpecFor(jobs.KindBalance)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	jobID, started := p.triggerJob(spec)
	if !started {
		writeBatchConflict(w, spec.conflict)
		return
	}
	log.Printf("panel: 手动全量余额刷新已触发（job=%s）", jobID)
	writeJSON(w, http.StatusOK, batchStartedBody(jobID))
}

// syncNicknames 手动刷新时的昵称同步（issue #94：上游改名免重登）。
// 只在面板手动「刷新」路径调用——后台余额定时器不触发（用户明确要求资料接口
// 仅手动触达）。逐号拉 /console/account，只取 nickname（手机号等敏感字段在
// upstream.FetchAccountProfile 内即被丢弃）；单号失败静默跳过，不打断余额刷新
// 的既有结果。
func (p *Panel) syncNicknames() {
	type job struct {
		uid string
		a   *auth.Auth
	}
	var jobs []job
	for _, st := range p.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		if a := p.cfg.Pool.AuthByUID(st.UID); a != nil && a.AccessTokenValue() != "" {
			jobs = append(jobs, job{uid: st.UID, a: a})
		}
	}
	if len(jobs) == 0 {
		return
	}
	var (
		mu      sync.Mutex
		updated int
		failed  int
		sem     = make(chan struct{}, 3)
		wg      sync.WaitGroup
	)
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			nick, err := p.cfg.Upstream.FetchAccountProfile(j.a)
			if err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			if p.cfg.Pool.SetNickname(j.uid, nick) {
				mu.Lock()
				updated++
				mu.Unlock()
			}
		}(j)
	}
	wg.Wait()
	if updated > 0 || failed > 0 {
		log.Printf("panel: 昵称同步：更新 %d 个，失败 %d 个（未变化不计数）", updated, failed)
	}
}

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

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

// ---------------------------------------------------------------------------
// HTTP 中间件：gzip 压缩 + 鉴权失败限速（v2.0.1 #2 / #4）
// ---------------------------------------------------------------------------

// defaultPollIntervalSec 面板前端默认轮询间隔（秒），与 cmd/server 的配置默认一致。
const defaultPollIntervalSec = 5

// gzipMinSize 只压缩超过该字节数的响应：小 JSON/错误页压缩收益为负（头部开销
// 可能超过节省的字节），且会让 401/429 这类响应更难在抓包里直接读。
const gzipMinSize = 1024

// gzipWriterPool 复用 gzip.Writer（每次 Reset 到目标 writer），避免每次请求分配
// ~1.2MB 的压缩窗口/哈希表。Close 不归还底层 writer（Reset 已切断引用）。
var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(nil) },
}

// acceptsGzip 解析 Accept-Encoding，是否接受 gzip（含 q=0 显式拒绝）。
func acceptsGzip(r *http.Request) bool {
	if r == nil {
		return false
	}
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(enc), "gzip") {
			continue
		}
		// gzip;q=0 / q=0.0 表示显式拒绝；其余参数（无 q / q=0.5 / q=1）都可压缩。
		return !gzipQZero(params)
	}
	return false
}

// gzipQZero 判断 Accept-Encoding 编码参数里的 q 值是否为 0（= 显式拒绝）。
func gzipQZero(params string) bool {
	for _, p := range strings.Split(params, ";") {
		p = strings.TrimSpace(p)
		if !strings.HasPrefix(p, "q=") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(p[2:]), 64)
		return err == nil && q == 0
	}
	return false
}

// isCompressibleContentType 面板可压缩的内容类型（JSON 接口 + 页面/脚本/样式）。
// 不碰 text/event-stream、图片等（面板目前没有，但显式白名单能防未来新增接口时误压）。
func isCompressibleContentType(ct string) bool {
	mt, _, _ := strings.Cut(ct, ";")
	switch strings.ToLower(strings.TrimSpace(mt)) {
	case "application/json",
		"text/html",
		"text/javascript",
		"application/javascript",
		"text/css":
		return true
	}
	return false
}

// gzipResponseWriter 缓冲面板响应，在 close 时按内容类型/大小/Accept-Encoding
// 决定是否 gzip 后一次性写给真实 ResponseWriter。
//
// 为什么缓冲而不是边写边压：面板没有流式接口，缓冲后才能在写响应头前知道
// 完整长度（>1KB 阈值）并准确维护 Content-Length；流式响应（SSE）在 server 包、
// 不经过本包装，故不受影响。
type gzipResponseWriter struct {
	http.ResponseWriter
	req    *http.Request
	buf    bytes.Buffer
	status int
	wrote  bool
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	if g.wrote {
		return
	}
	g.status = status
	g.wrote = true
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	if !g.wrote {
		g.status = http.StatusOK
		g.wrote = true
	}
	return g.buf.Write(p)
}

// close 把缓冲内容按需压缩并写出（ServeHTTP defer 调用；幂等）。
func (g *gzipResponseWriter) close(compressOK bool) {
	status := g.status
	if status == 0 {
		status = http.StatusOK
	}
	h := g.Header()
	body := g.buf.Bytes()

	noBody := g.req != nil && g.req.Method == http.MethodHead
	// 304/204 无实体；已编码（如上游/未来中间件自己压过）不二次压缩。
	compressible := compressOK &&
		!noBody &&
		status != http.StatusNoContent &&
		status != http.StatusNotModified &&
		status != http.StatusPartialContent &&
		h.Get("Content-Encoding") == "" &&
		len(body) > gzipMinSize &&
		isCompressibleContentType(h.Get("Content-Type"))

	if compressible {
		// 压缩后长度不可预知：删掉可能存在的 Content-Length，由 net/http 决定
		// （小响应会自动计算，大响应走 chunked）。
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		g.ResponseWriter.WriteHeader(status)
		gz := gzipWriterPool.Get().(*gzip.Writer)
		gz.Reset(g.ResponseWriter)
		_, _ = gz.Write(body)
		_ = gz.Close()
		gzipWriterPool.Put(gz)
		return
	}

	g.ResponseWriter.WriteHeader(status)
	if noBody {
		return
	}
	_, _ = g.ResponseWriter.Write(body)
}

// ---------------------------------------------------------------------------
// 面板鉴权失败限速（v2.0.1 #4-3）
// ---------------------------------------------------------------------------

const (
	// authFailLimit 每个来源 IP 每分钟允许的鉴权失败次数（token bucket 容量）：
	// 超过后新请求回 429 + Retry-After；正常用户每分钟输错 20 次已远超现实。
	authFailLimit = 20
	// authFailWindow token bucket 回满周期 = 每分钟 20 个令牌。
	authFailWindow = time.Minute
	// authFailLimiterMaxIPs 内存 LRU 上限：1 万 IP，防止被伪造 XFF 刷爆内存。
	authFailLimiterMaxIPs = 10000
)

// authFailBucket 单个 IP 的令牌桶。
type authFailBucket struct {
	tokens float64
	last   time.Time
	elem   *list.Element // LRU 节点（front = 最近使用）
}

// authFailLimiter 按来源 IP 的鉴权失败 token bucket + LRU 上限，并发安全。
// 语义：
//   - blocked：该 IP 令牌耗尽 → 拒绝新请求（不消耗令牌），并给出 Retry-After 秒数；
//   - fail：鉴权失败消耗 1 个令牌（首次失败建桶，容量 authFailLimit）；
//   - success：鉴权成功清桶（正常客户端不被同 IP 的异常尝试拖累）。
//
// 状态全在内存：重启清零（可接受，攻击者也要重来）。
type authFailLimiter struct {
	mu      sync.Mutex
	maxIPs  int
	buckets map[string]*authFailBucket
	lru     *list.List
	now     func() time.Time // 测试可注入时钟
}

func newAuthFailLimiter(maxIPs int) *authFailLimiter {
	if maxIPs <= 0 {
		maxIPs = authFailLimiterMaxIPs
	}
	return &authFailLimiter{
		maxIPs:  maxIPs,
		buckets: make(map[string]*authFailBucket),
		lru:     list.New(),
		now:     time.Now,
	}
}

// refill 按经过时间补充令牌（上限 authFailLimit）；now 为零值时用当前时间。
func (l *authFailLimiter) refill(b *authFailBucket, now time.Time) {
	rate := float64(authFailLimit) / authFailWindow.Seconds() // 令牌/秒
	b.tokens += now.Sub(b.last).Seconds() * rate
	if b.tokens > authFailLimit {
		b.tokens = authFailLimit
	}
	if b.tokens < 0 {
		b.tokens = 0
	}
	b.last = now
}

// touch 把桶移到 LRU 头部（最近使用）。
func (l *authFailLimiter) touch(b *authFailBucket) {
	if b.elem != nil {
		l.lru.MoveToFront(b.elem)
	}
}

// lookup 取桶并刷新 LRU（不存在返回 nil，调用方持锁）。
func (l *authFailLimiter) lookup(ip string) *authFailBucket {
	b := l.buckets[ip]
	if b != nil {
		l.touch(b)
	}
	return b
}

// getOrCreate 取桶，必要时新建并按 LRU 上限淘汰最旧 IP（调用方持锁）。
func (l *authFailLimiter) getOrCreate(ip string, now time.Time) *authFailBucket {
	if b := l.buckets[ip]; b != nil {
		l.touch(b)
		return b
	}
	b := &authFailBucket{tokens: authFailLimit, last: now}
	b.elem = l.lru.PushFront(ip)
	l.buckets[ip] = b
	for len(l.buckets) > l.maxIPs {
		oldest := l.lru.Back()
		if oldest == nil {
			break
		}
		oldIP, _ := oldest.Value.(string)
		l.lru.Remove(oldest)
		delete(l.buckets, oldIP)
	}
	return b
}

// blocked 该 IP 当前是否被限速；返回建议的 Retry-After 秒数（最小 1）。
func (l *authFailLimiter) blocked(ip string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.lookup(ip)
	if b == nil {
		return false, 0
	}
	now := l.now()
	l.refill(b, now)
	if b.tokens >= 1 {
		return false, 0
	}
	// 距离攒够 1 个令牌的时间 = (1 - tokens) / rate。
	rate := float64(authFailLimit) / authFailWindow.Seconds()
	wait := time.Duration((1 - b.tokens) / rate * float64(time.Second))
	if wait < time.Second {
		wait = time.Second
	}
	secs := int((wait + time.Second - 1) / time.Second) // 向上取整
	if secs < 1 {
		secs = 1
	}
	return true, secs
}

// fail 记录一次鉴权失败（消耗 1 个令牌；耗尽后保持 0，由 blocked 拒绝）。
func (l *authFailLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.getOrCreate(ip, now)
	l.refill(b, now)
	if b.tokens >= 1 {
		b.tokens--
	} else {
		b.tokens = 0
	}
}

// success 鉴权成功：清空该 IP 的失败记录（下次失败从满桶重新计数）。
func (l *authFailLimiter) success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[ip]
	if b == nil {
		return
	}
	if b.elem != nil {
		l.lru.Remove(b.elem)
	}
	delete(l.buckets, ip)
}

// isPanelAPIPath 是否面板数据接口路径（限速只作用于这些路径；页面/静态资源可匿名加载）。
func isPanelAPIPath(path string) bool {
	return strings.HasPrefix(path, "/panel/api/")
}

// rateLimitIP 提取限速用的来源 IP：X-Forwarded-For 首段 → X-Real-IP → TCP 对端。
// 口径与 server/logging.go 的 clientIPForLog 一致（反代后拿真实客户端；直连回落
// RemoteAddr，避免所有直连请求共用一个空 key 被一起限速）。
func rateLimitIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if ip := upstream.ExtractClientIP(r); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}
