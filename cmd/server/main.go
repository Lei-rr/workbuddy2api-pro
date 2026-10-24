// main.go workbuddy2api-pro 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/credithist"
	"github.com/lei-rr/workbuddy2api-pro/internal/jobs"
	"github.com/lei-rr/workbuddy2api-pro/internal/livecfg"
	"github.com/lei-rr/workbuddy2api-pro/internal/notify"
	"github.com/lei-rr/workbuddy2api-pro/internal/panel"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/redisstore"
	"github.com/lei-rr/workbuddy2api-pro/internal/reqlog"
	"github.com/lei-rr/workbuddy2api-pro/internal/scheduler"
	"github.com/lei-rr/workbuddy2api-pro/internal/server"
	"github.com/lei-rr/workbuddy2api-pro/internal/session"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
	"github.com/lei-rr/workbuddy2api-pro/internal/usage"
)

// appVersion 网关版本（fork 版：面板 + 任务体系），透出到 /panel/api/overview。
const appVersion = "2.6.0-pro"

// usagePathFor 由 state 文件路径推出用量文件路径：同目录、文件名 usage.json。
// 这样 config 里改 state_file 时用量数据跟着走，不需要额外配置项。
func usagePathFor(stateFile string) string { return stateSibling(stateFile, "usage.json") }

// stateSibling 返回与 state 文件同目录的指定文件名路径（相对路径场景回落当前目录）。
// usage.json（用量记录）与 output_probes.json（模型上限探测）共用本规则。
func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（默认当前目录 config.json；不存在时自动生成推荐配置）")
	flag.Parse()

	// v2.5.0 Perf-4：启动分段计时（进程内视角，不含 CreateProcess 开销）。
	// 便于 perfbench / 线上日志定位「10k 账号启动慢」卡在配置、auth 解析还是池对齐。
	startupAt := time.Now()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// errors.Is 才能看穿 Load 里 fmt.Errorf("%w") 的包装；os.IsNotExist 不行。
		if errors.Is(err, fs.ErrNotExist) {
			// 首次运行：目录下没有配置 → 自动落一份推荐配置（含随机 api_key）再加载。
			// 双击 exe / 裸跑 docker 即开，无需先手工复制样例。
			if key, werr := WriteDefault(*cfgPath); werr == nil {
				log.Printf("config %s 不存在，已生成推荐配置（api_key=%s，记录在该文件里，可自行修改）", *cfgPath, key)
				cfg, err = Load(*cfgPath)
			}
			if err != nil {
				// 生成失败（目录只读等）：退回纯默认 + env（旧行为兜底），不阻塞启动。
				log.Printf("config %s not found (auto-generate failed), using defaults+env: %v", *cfgPath, err)
				cfg, err = Load("")
			}
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	// 安全前置（v2.0.1 #4-2）：非回环监听 + 网关/面板均无密钥 + 未显式允许匿名 →
	// 直接拒绝启动。必须在建立任何监听/加载账号之前执行，错误信息给出可操作的修复路径。
	if err := validateListenSecurity(cfg); err != nil {
		log.Fatalf("startup security check: %v", err)
	}
	// 面板密钥分离提示（v2.0.1 #4-1）：只配了 api_key 时面板与 /v1 共用同一把密钥，
	// 任何拿到网关 key 的下游调用方都能调面板管理接口——提示但不改变行为（向后兼容）。
	switch {
	case cfg.Panel.APIKey != "":
		log.Printf("面板密钥已与网关密钥分离（panel.api_key 生效；/v1/* 只认 api_key）")
	case cfg.APIKey != "":
		log.Printf("WARN: panel.api_key 未配置，面板与网关共用同一把 api_key（建议分离；panel.api_key 变更需重启）")
	case cfg.Security.AllowAnonymous:
		log.Printf("WARN: api_key 与 panel.api_key 均为空且 security.allow_anonymous=true：网关与面板均无鉴权（仅限可信网络）")
	}

	// v2.2 #13 启动自检（warn 而非 fatal）：auth_dir / state 目录可写性 + 磁盘水位。
	// 容器/宿主挂载与权限问题很常见，直接拒绝启动会让用户失去管理面板这个排查入口；
	// 逐条 WARN + 修复提示，把是否继续的决定权留给用户。
	for _, warning := range startupSelfCheck(cfg.AuthDir, cfg.StateFile) {
		log.Printf("WARN: %s", warning)
	}

	// v2.3.0 #10/#19：后台根 ctx 提前创建——Job Registry 与通知器在 panel/调度器
	// 装配前就要拿到它；优雅停机在同一处 cancel（运行中 Job → canceled、通知器
	// 仍可排空已入队事件，见 main 尾部收尾顺序）。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// v2.3.0 #19 事件通知（Webhook；空/非法 URL = 关闭，Notifier 为 no-op 实例）。
	// v2.3.1 #19：渠道配置（webhook/wecom/telegram/bark）统一由 notify.NewFromConfig 解析。
	notifier := notify.NewFromConfig(notify.Config{
		WebhookURL:     cfg.Notify.WebhookURL,
		Timeout:        cfg.NotifyTimeoutDur,
		Events:         cfg.Notify.Events,
		Channel:        cfg.Notify.Channel,
		TelegramChatID: cfg.Notify.TelegramChatID,
		Title:          cfg.Notify.Title,
	})
	// 兜底关闭（幂等）：正常路径在 main 尾部显式带超时排空后再退出。
	defer func() { _ = notifier.Close(context.Background()) }()

	// v2.3.0 #10 全池任务 Job Runner：jobs.json 与 state 同目录；并发闸门/保留条数
	// 来自 config.jobs。BaseContext=主 ctx（SIGTERM → 运行中 Job 取消为 canceled），
	// OnFinish 把终态 Job 转成 job_done 通知（Notifier 关闭时 no-op）。
	jobReg := jobs.New(jobs.Config{
		Path:           stateSibling(cfg.StateFile, "jobs.json"),
		Retain:         cfg.Jobs.Retain,
		MaxConcurrency: cfg.Jobs.MaxConcurrency,
		BaseContext:    ctx,
		OnFinish:       func(j *jobs.Job) { notifier.JobDone(jobDonePayload(j)) },
	})
	log.Printf("[jobs] 全池任务 Job Runner 已启用: %s（并发上限 %d，保留 %d 条）",
		stateSibling(cfg.StateFile, "jobs.json"), jobReg.MaxConcurrency(), cfg.Jobs.Retain)

	authLoadAt := time.Now()
	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)
	log.Printf("startup: load auths took %dms (%d accounts)", time.Since(authLoadAt).Milliseconds(), len(auths))

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	// 停机序：先 pool.Close()（最后一次 Flush → SaveState 已提交到 store），
	// 再 store.Close() 排空在途异步写（最后一笔 Redis 镜像必须写完才关连接）。
	defer func() {
		p.Close()
		_ = store.Close()
	}()
	p.SetStore(store)
	poolLoadAt := time.Now()
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）
	log.Printf("startup: pool load+sync took %dms", time.Since(poolLoadAt).Milliseconds())

	// 熔断器 + 在途上限（含 global 分档）+ 连败降权 + 闲置补偿调优（从 config 注入，
	// 非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域 WAF 风控分档（P1-1）
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(cfg.SoftRateMaxDur)                 // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur) // costTier 探索窗口（issue #136，默认 30m；0 关停）
	p.SetCreditFloor(cfg.Pool.CreditFloor)               // 积分保底（默认 0 = 关闭）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(cfg.Pool.PreferExpiring)
	p.SetCrossDomainFallback(cfg.Pool.CrossDomainFallback) // 裸名跨域回退（issue #135，默认关闭）
	_ = p.SetScheduling(cfg.Pool.Strategy, cfg.Pool.AccountOrder)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			// v2.6.0 P0-2：按 (model, realm) 的可用视图由 pool 缓存（Contains O(1)、
			// UIDs 复用权重槽），session 热路径不再每请求全池遍历/排序/建 map。
			AvailableViewForModel: realmAwareAvailableViewForModel(p),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()

	// 积分保底的「收费」兜底判据：接上游模型目录的积分倍率表。本地实测台账无观测
	// 时用它判收费——否则「没学过」恒等于「放行」，高价新模型会把触底号一笔打穿
	// （kimi-k3-1 实案：全池无观测 → 保底全放行 → 两笔打穿并硬冷却到次日 04:00）。
	// 位于 up 装配之后：倍率表由探测下发，闭包每次调用读实时快照。
	p.SetModelRateOf(func(realm, model string) string { return up.ModelRate(realm, model) })

	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints.Store(cfg.Features.SanitizeBlacklistFingerprints)
	// 出站 UA 与归属头（issue #42 + 上游同步）：
	// UserAgent 非空则完全覆盖；ClientVersion/CliVersion 缺省对齐官方形态；
	// ClientName 非空时 chat 路径注入 X-IDE-* 四头（用量归因对齐官方桌面端）。
	up.UserAgent = cfg.Upstream.UserAgent
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	up.ClientName = cfg.Upstream.ClientName
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 路由（config global 段）：上游侧开关（第一道闸）+ base 覆盖；
	// auth 侧开关（auth.SetGlobalEnabled）是第二道闸，两者同 config global.enabled。
	up.GlobalEnabled = cfg.Global.Enabled
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	applyCNChatBase(up, cfg)
	auth.SetGlobalEnabled(cfg.Global.Enabled)
	// DNS：Android 没有 /etc/resolv.conf，Go 的纯 Go 解析器会退回 127.0.0.1/::1，
	// 于是查任何域名都变成"连本机 53 端口"（dial tcp: lookup copilot.tencent.com
	// on [::1]:53: connection refused）。配了 dns_servers / WB2A_DNS 就显式指定。
	if len(cfg.DNSServers) > 0 {
		d := upstream.NewDNSDialer(cfg.DNSServers)
		up.SetDNSDialer(d)
		// 推送 webhook / Upstash 镜像等走 http.DefaultTransport，一并换掉
		if t, ok := http.DefaultTransport.(*http.Transport); ok {
			t.DialContext = d.DialContext
		}
		log.Printf("DNS: 出站解析用 %v（不走系统 /etc/resolv.conf）", cfg.DNSServers)
	}
	// model.json 本地缓存接线（context_length/max_output_tokens 四级查找链第 3 级）：
	// 数据目录与 state.json 同风格（Docker volume 持久化路径）。首次缺失/损坏自动
	// 回落仓库内嵌种子；models.dev 按需拉取成功后原子写回。
	upstream.SetModelCatalogPath(stateSibling(cfg.StateFile, "model.json"))

	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   cfg.Schedule.CheckinHours,
		TravelHours:    cfg.Schedule.TravelHours,
		ActivityHours:  cfg.Schedule.ActivityHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		BlackcatHours:  cfg.Schedule.BlackcatHours,
		GrowthHours:    cfg.Schedule.GrowthHours,
		// 快过期积分优先消耗：签到/余额刷新按此窗口分桶（issue:积分过期）。
		ExpiringSoonWindow: cfg.ExpiringSoonDur,
		CheckinDisabled:    !cfg.Schedule.CheckinEnabled,
		TravelDisabled:     !cfg.Schedule.TravelEnabled,
		ActivityDisabled:   !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:  !cfg.Schedule.KeepaliveEnabled,
		BlackcatDisabled:   !cfg.Schedule.BlackcatEnabled,
		GrowthDisabled:     !cfg.Schedule.GrowthEnabled,
		// 保号类四任务是否覆盖禁用账号（缺省 false = 禁用即跳过，保持既有行为）。
		IncludeDisabledInTasks: cfg.Schedule.IncludeDisabledInTasks,
		// v2.0.1 #10 子项：余额全量刷新有界并发（默认 8，上限 32）与启动首刷保护
		// （延迟 + 大池跳过）。后台周期刷新与面板手动刷新共用该并发闸门/单飞 gate。
		BalanceRefreshConcurrency:  cfg.Schedule.BalanceRefreshConcurrency,
		BalanceRefreshStartupDelay: cfg.BalanceRefreshStartupDelayDur,
		BalanceRefreshStartupForce: cfg.Schedule.BalanceRefreshStartupForce,
		// v2.0.1 #11 子项：补跑窗口（默认 120 分钟，0=关闭）。批次跨过后续时点
		// 或进程重启后，把「今天尚未跑过且在窗口内」的任务族立即补跑一轮。
		CatchUpMinutes: cfg.Schedule.CatchUpMinutes,
		// v2.3.1 #11：签到 worker pool 账号级并发（默认 3，clamp 1..16；面板保存热生效）。
		CheckinConcurrency: cfg.Schedule.CheckinConcurrency,
		// v2.3.1 #11：定时/补跑批次进 Job 历史（trigger=schedule/catch_up），
		// 面板 GET /panel/api/jobs 可见、cancel 可让批次提前停止；JobDone 走 webhook。
		Jobs: jobReg,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻；账号级并发 %d）",
			cfg.Schedule.CheckinHours, cfg.Schedule.CheckinConcurrency)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每日 1 次，点亮连登 + 解锁 first_buddy）", cfg.Schedule.ActivityHours)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	switch {
	case !cfg.Schedule.BlackcatEnabled:
		log.Printf("夜猫子已禁用（schedule.blackcat_enabled=false）")
	default:
		log.Printf("夜猫子已启用：%v 点（23:00–08:00 窗口 glm-5.2 对话补足）", cfg.Schedule.BlackcatHours)
	}
	switch {
	case !cfg.Schedule.BalanceRefreshEnabled:
		log.Printf("余额后台刷新已禁用（schedule.balance_refresh_enabled=false）")
	case cfg.BalanceRefreshInterval > 0:
		log.Printf("余额后台刷新：每 %s（签到时点照常额外刷新；并发上限 %d）",
			cfg.BalanceRefreshInterval, cfg.Schedule.BalanceRefreshConcurrency)
	}
	if cfg.Schedule.CatchUpMinutes > 0 {
		log.Printf("调度补跑窗口：%d 分钟（批次跨过后续时点/重启后补跑当天未跑的任务族）",
			cfg.Schedule.CatchUpMinutes)
	} else {
		log.Printf("调度补跑窗口已关闭（schedule.catch_up_minutes=0：错过的时点等下一轮）")
	}
	switch {
	case !cfg.Schedule.TaskSweepEnabled:
		log.Printf("任务补领清扫已禁用（schedule.task_sweep_enabled=false）")
	case cfg.TaskSweepInterval > 0:
		log.Printf("任务补领清扫：每 %s（补领已达标未领的成长任务奖励）", cfg.TaskSweepInterval)
	}
	if cfg.Schedule.IncludeDisabledInTasks {
		log.Printf("保号任务覆盖禁用账号（schedule.include_disabled_in_tasks=true）：禁用号仍签到 / 活跃 / 保活 / 刷新余额，但不参与选号")
	}

	// 管理面板日志镜像：标准 log（stderr）与 chat 表格日志（stdout）双路复制进
	// 面板环形缓冲，供 /panel/api/logs 读取；控制台输出行为完全不变。
	// live 承载可热改字段（api_key/soft_rate/脱敏开关/面板轮询间隔），面板保存配置时在线替换。
	live := livecfg.New(livecfg.Snapshot{
		APIKey:               cfg.APIKey,
		SoftCooldown:         cfg.SoftRateDur,
		SanitizeFingerprints: cfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     cfg.Logging.RequestClientInfo,
		HiddenModels:         livecfg.NormalizeHiddenModels(cfg.Models.Hidden),
		PollIntervalSec:      cfg.Panel.PollIntervalSec,
	})
	// 用量记录器：与 state 文件同目录，随 state_file 配置一起搬移。
	// datapath 由 state 文件路径推出，避免再加一个配置项。
	usagePath := usagePathFor(cfg.StateFile)
	rec := usage.New(usagePath)
	rec.Start()
	defer rec.Stop()
	log.Printf("[usage] 逐请求用量记录已启用: %s (%s)", usagePath, rec.Describe())

	// 请求指标始终启用；JSONL 归档只写脱敏元数据，写盘失败不影响聊天请求。
	requestLog := reqlog.New(reqlog.Config{
		Dir:           stateSibling(cfg.StateFile, "request-logs"),
		Enabled:       cfg.Logging.RequestArchiveEnabled,
		RetentionDays: cfg.Logging.RequestRetentionDays,
		MaxBytes:      int64(cfg.Logging.RequestArchiveMaxMB) << 20,
	})
	defer requestLog.Close()
	rs := requestLog.Snapshot().Archive
	if rs.Enabled {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档 %s（保留 %d 天，上限 %d MiB）",
			rs.Dir, cfg.Logging.RequestRetentionDays, cfg.Logging.RequestArchiveMaxMB)
	} else {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档已关闭")
	}

	// 积分历史：把每次真实查到的余额与上一次比对，变动即留痕（新账号首次只建
	// 基线）。挂载在余额查询路径上（upstream 的余额观察者），签到 / 活跃上报 /
	// 旅行 / 保活 / 面板手动刷新全覆盖——上游只在少数渠道落金额日志，比对余额
	// 是唯一可靠的口径。与 state/usage/request-logs 同目录，随 state_file 搬移。
	creditHist := credithist.New(stateSibling(cfg.StateFile, "credit-history.json"), 2000)
	up.SetCreditObserver(creditHist.Observe)
	log.Printf("[credithist] 积分历史已启用: %s", stateSibling(cfg.StateFile, "credit-history.json"))

	pn := panel.New(panel.Config{
		Pool:        p,
		Usage:       rec,
		RequestLog:  requestLog,
		Upstream:    up,
		Scheduler:   sch,
		AuthDir:     cfg.AuthDir,
		APIKey:      cfg.APIKey,
		RedisMode:   redisMode,
		StickyCount: sessCount,
		Version:     appVersion,
		Live:        live,
		// 面板安全（v2.0.1 #4）：独立面板密钥（空 = 回落网关 api_key，向后兼容）与
		// 前端轮询间隔（overview 下发 poll_interval_sec）。两者都在装配期进入面板，
		// panel.api_key 变更需重启；轮询间隔经上面的 live 快照热生效。
		PanelAPIKey:     cfg.Panel.APIKey,
		PollIntervalSec: cfg.Panel.PollIntervalSec,
		// 模型上限探测数据（scripts/probe_max_tokens.py --panel-out 写入）：
		// 与 state 文件同目录，缺省 data/output_probes.json。
		ProbeFile: stateSibling(cfg.StateFile, "output_probes.json"),
		// v2.2.0 #5/#9：面板操作审计与任务执行历史 JSONL 与 state 同目录
		// （data/panel-audit.jsonl、data/task-runs.jsonl）。
		StateFile:  cfg.StateFile,
		ConfigPath: *cfgPath,
		LoadConfig: func() (any, error) {
			return Load(*cfgPath)
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			return saveConfig(raw, *cfgPath, live, p, up, sch)
		},
		// v2.2.0 #15：一键回滚（POST /panel/api/config/rollback）——用 config.json.bak
		// 恢复并热应用；与保存共用 applyConfigRuntime，热生效范围一致。
		RollbackConfig: func() ([]string, error) {
			return rollbackConfig(*cfgPath, live, p, up, sch)
		},
		// 积分历史账本（只读展示；写入由上面的余额观察者完成）。
		CreditHistory: creditHist,
		// v2.3.0 #10 Job Runner：批量端点起 Job（job_id/进度/取消/jobs.json 持久化）。
		Jobs: jobReg,
		// v2.6.0 审计 P0：账号级扇出并发上限（补领清扫/任务扫描/成长队列/昵称同步等）。
		TaskSweepConcurrency: cfg.Schedule.TaskSweepConcurrency,
		// v2.3.0 #19 通知器：accountDisable 成功时 Emit account_disabled（关闭时 no-op）。
		Notify: notifier,
	})
	// v2.4.0 #3 SSE 事件推送开关（config panel.events_enabled；关闭时 /panel/api/events 404，
	// 前端自动回落轮询）。handler 运行期读包级原子量，先后顺序不敏感。
	panel.SetEventsEnabled(cfg.Panel.EventsEnabled)
	// 成长任务队列每日自动执行（与「执行全部待办」同管线）：Sequential 族零点解锁后
	// 无需手动扫描；hook 返回即启动（异步执行），已在跑时内部跳过。
	sch.SetGrowthHook(pn.RunGrowthQueueOnce)
	// 清扫回调返回统计汇总（测试/日志可观测），调度器 hook 只要求 func()：
	// 这里收掉返回值——汇总已由 panel 侧内部日志输出。
	sch.SetTaskSweepHook(func() { pn.RunTaskSweepOnce() })
	log.SetOutput(io.MultiWriter(os.Stderr, pn.Logs()))
	server.SetChatLogOutput(io.MultiWriter(os.Stdout, pn.Logs()))

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		Panel:        pn,
		Live:         live,
		Usage:        rec,
		RequestLog:   requestLog,
		PromptMode:   cfg.Prompt.Mode,
		PromptText:   cfg.PromptText,
		// 来源记录开关经 livecfg 热生效；此处同时填静态字段，供 Live 为 nil 的
		// 裸用/测试路径拿到同一缺省值。
		RecordClientInfo: cfg.Logging.RequestClientInfo,
		// handler 侧第三道闸（global realm）：false（显式逃生门）时不列 global: 模型名。
		GlobalEnabled: cfg.Global.Enabled,
		// 模型黑名单原始条目；运行期以 livecfg 快照为准（热生效），此处供
		// Live 为 nil 的裸用/测试路径。
		ModelsHidden: cfg.Models.Hidden,
		// v2.6.0 P0-1：/v1 请求体上限（config server.max_body_mb，默认 64MiB）。
		MaxBodyMB: cfg.Server.MaxBodyMB,
	})

	// v2.2 #14 优雅停机：显式信号通道（不用 signal.NotifyContext——需要两级语义：
	// 第一次信号走优雅停机，停机期间再按一次立即强制退出）。ctx/cancel 已在 main
	// 前部创建（Job Registry/通知器共享），这里只挂后台任务与 HTTP 服务。
	// 后台任务登记：停机时先 cancel 让它们不再取新任务，再等待收敛
	//（收敛上限由 server.shutdown_timeout 的同一时间预算兜底）。
	// v2.6.0 Audit-1 P0-4：scheduler 的余额刷新/任务补领循环不再游离于停机顺序之外——
	// 它们由 sch.WaitBackground 收敛，并作为 shutdownDeps.WaitExtra 在 Flush 之前等待，
	// 保证「停新任务 → 等所有会写池的后台 goroutine 退出 → Flush」。
	// Job Registry 的运行中 Job 由 main 尾部 jobReg.Close()+Wait 单独等待收敛。
	var bgWG sync.WaitGroup
	bgWG.Add(2)
	go func() { defer bgWG.Done(); sch.Run(ctx) }()
	go func() { defer bgWG.Done(); warmModelRates(ctx, up, p) }()
	sch.StartBalanceRefresh(ctx, cfg.BalanceRefreshInterval)
	sch.StartTaskSweep(ctx, cfg.TaskSweepInterval)
	waitSchedBackground := func(wctx context.Context) bool { return sch.WaitBackground(wctx) }

	// 启动即预热模型积分倍率表：倍率只在 FetchModels/FetchGlobalModelInfos 成功时
	// 填充（两者均懒触发），重启后到首次 /v1/models 或面板模型页被访问之前，
	// ModelRate 恒返回空串——积分保底的目录兜底在这段空窗期内形同虚设，触底号
	// 会被当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费
	// 模型归零；倍率表当时尚未建立）。
	// 已在上面以「可收敛的后台任务」登记（bgWG）：优雅停机时随 ctx 取消，不得拖住退出。
	log.Printf("startup: ready took %dms", time.Since(startupAt).Milliseconds())
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body 上传）：防慢速 body 拖死连接。
		// 请求体大小由 server.max_body_mb（默认 64MiB）在 handler 层用 MaxBytesReader
		// 限制（v2.6.0 P0-1：超限 413，不把超大 body 读进内存）。缺省 300s
		//（issue #100：旧固定 60s 会掐掉大上下文/文件块经反代链的慢速上传，客户端
		// 收到 400 "read body: ... i/o timeout"）；server.read_timeout="0" 显式关闭。
		// 改动需重启进程。
		ReadTimeout: cfg.ServerReadTimeoutDur,
		// IdleTimeout keep-alive 空闲连接回收：配合 chat 出站 ctx 传播防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	// 显式 goroutine Serve：主 goroutine 负责信号编排（Shutdown 会让 Serve 返回
	// http.ErrServerClosed，由主 goroutine 按停机流程处理）。
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	// 缓冲 2：第一次信号触发优雅停机；停机等待期间再收到一次信号立即强退。
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	log.Printf("workbuddy2api-pro listening on %s (api_key=%v)，管理面板 http://127.0.0.1%s/panel/", cfg.Listen, cfg.APIKey != "", panelListenPath(cfg.Listen))
	log.Printf("优雅停机：SIGINT/SIGTERM 后等待在途请求至多 %s（server.shutdown_timeout；期间再次收到信号立即强退）", cfg.ShutdownTimeoutDur)

	var (
		shutdownStart time.Time
		shutdownRes   shutdownResult
	)
	select {
	case err := <-serveErr:
		// Serve 自身返回（非本进程 Shutdown 触发）→ 监听失败等致命错误，直接退出。
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			cancel()
			log.Fatalf("http: %v", err)
		}
		// 理论上不可达（Shutdown 只由下方信号分支触发）：保守按停机流程收尾。
		shutdownStart = time.Now()
		shutdownRes = gracefulShutdown(shutdownDeps{
			Shutter:    srv,
			Cancel:     cancel,
			Background: &bgWG,
			WaitExtra:  waitSchedBackground,
			Flush:      p.Flush,
			Timeout:    cfg.ShutdownTimeoutDur,
		})
	case sig := <-sigCh:
		shutdownStart = time.Now()
		log.Printf("收到退出信号 %v：开始优雅停机（上限 %s）", sig, cfg.ShutdownTimeoutDur)
		// 二次信号：不再等待，立即强制退出。130 = 128+SIGINT 的约定俗成退出码。
		go func() {
			sig2 := <-sigCh
			log.Printf("再次收到退出信号 %v：立即强制退出（在途请求/未落盘计数可能丢失）", sig2)
			os.Exit(130)
		}()
		shutdownRes = gracefulShutdown(shutdownDeps{
			Shutter:    srv,
			Cancel:     cancel,
			Background: &bgWG,
			WaitExtra:  waitSchedBackground,
			Flush:      p.Flush,
			Timeout:    cfg.ShutdownTimeoutDur,
		})
	}

	// v2.3.0 #10 Job Registry 收尾：gracefulShutdown 已 cancel 根 ctx（运行中 Job
	// 收到取消），这里显式 Close 拒绝新 Job 并有上限等待 goroutine 收敛——避免
	// 「进程退出时 jobs.json 正被半写」与「Job 状态卡在 running」。
	jobReg.Close()
	jobWaitCtx, jobWaitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	jobConverged := jobReg.Wait(jobWaitCtx)
	jobWaitCancel()
	if !jobConverged {
		log.Printf("WARN: jobs: 运行中 Job 未在 5s 内全部收敛（继续退出；jobs.json 保留最后快照）")
	}
	// v2.3.0 #19 通知器收尾：排空已入队事件（有上限；webhook 故障不得拖住退出）。
	notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := notifier.Close(notifyCtx); err != nil {
		log.Printf("WARN: notify: 关闭时未排空全部事件: %v", err)
	}
	notifyCancel()

	// 最终资源收尾：观测账本（usage/reqlog）→ 会话 GC → 池状态/Redis 异步写。
	// 与 main 顶部 defer 的兜底重复调用是安全的（各处 Stop/Close 均幂等）。
	rec.Stop()
	requestLog.Close()
	if sessRouter != nil {
		sessRouter.StopGC()
	}
	p.Close()
	_ = store.Close()

	log.Printf("graceful shutdown complete in %dms（在途请求超时=%v，后台任务已收敛=%v）",
		time.Since(shutdownStart).Milliseconds(), shutdownRes.TimedOut, shutdownRes.BackgroundDone)
	log.Printf("bye")
}

// jobDonePayload 把 Job 终态转成 notify job_done 事件的 data：
// 字段与 GET /panel/api/jobs 对齐（接收端可区分 kind/state/计数），时间用 unix 秒；
// 不含任何凭证，Error 仅执行级摘要（上游/任务体返回的公开错误文本）。
func jobDonePayload(j *jobs.Job) map[string]any {
	if j == nil {
		return map[string]any{}
	}
	data := map[string]any{
		"id":      j.ID,
		"kind":    string(j.Kind),
		"trigger": string(j.Trigger),
		"state":   string(j.State),
		"total":   j.Total,
		"done":    j.Done,
		"fail":    j.Fail,
		"skip":    j.Skip,
	}
	if !j.StartedAt.IsZero() {
		data["started_at"] = j.StartedAt.Unix()
	}
	if !j.FinishedAt.IsZero() {
		data["finished_at"] = j.FinishedAt.Unix()
	}
	if j.Error != "" {
		data["error"] = j.Error
	}
	return data
}

// warmModelRates 启动预热各域模型积分倍率表（供积分保底的目录兜底判定）。
//
// 为什么需要：倍率表只在 FetchModels（CN）/ FetchGlobalModelInfos（global）成功时
// 填充，两者都是懒触发（被 /v1/models 或面板模型页访问才跑）。重启后到首次触发
// 之间的空窗期里 ModelRate 恒返回空串，保底的目录兜底判不出收费，触底号会被
// 当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费模型归零）。
//
// 失败处理：单域失败只记 WARN（不阻塞、不致命——后续懒触发仍会补上）；global 域
// 仅在其路由开关开启时预热（逃生门关锁时按 CN 处理，无需探测）。
func warmModelRates(ctx context.Context, up *upstream.Client, p *pool.Pool) {
	// 预热不得拖住进程退出：ctx 取消（SIGINT/SIGTERM）时立刻放弃剩余域。
	if ctx.Err() != nil {
		return
	}
	// CN：有可用 CN 账号才拉（与面板 models 同口径，避免无谓上游调用）。
	if uids := p.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if a := p.AuthByUID(uids[0]); a != nil {
			if _, err := up.FetchModels(a); err != nil {
				log.Printf("WARN: [upstream] warm model rates (cn): %v", err)
			} else {
				log.Printf("[upstream] warm model rates: cn ok")
			}
		}
	}
	// global：独立目录端点（workbuddy.ai），倍率按 "global" 域键存储。
	if up.GlobalEnabled && ctx.Err() == nil {
		if uids := p.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if a := p.AuthByUID(uids[0]); a != nil {
				// FetchGlobalModelInfos 无错误返回（内部负缓存自行节流），
				// 仅按结果条数判断是否拿到目录。
				if infos := up.FetchGlobalModelInfos(a); len(infos) == 0 {
					log.Printf("WARN: [upstream] warm model rates (global): empty model list")
				} else {
					log.Printf("[upstream] warm model rates: global ok (%d models)", len(infos))
				}
			}
		}
	}
}

// panelListenPath 从 listen 地址提取 ":port" 形式，用于启动日志拼面板 URL
// （":7863" 或 "0.0.0.0:7863" → ":7863"；异常输入原样返回）。
func panelListenPath(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}
	return listen
}

// saveConfig 面板保存配置：校验 → 落盘 → 热应用 → 返回需重启的字段列表。
//
// 热生效范围（设计取舍）：
//   - api_key / cooldown.soft_rate / features.sanitize_blacklist_fingerprints → livecfg 快照
//   - panel.poll_interval_sec → livecfg 快照（overview 下发的 poll_interval_sec 立即变）
//   - pool.* → pool.SetBreaker/SetMaxInFlight/SetSoftRateMax/SetWeights/SetCostExploreInterval/SetPreferExpiring/SetCreditFloor
//   - schedule.* → scheduler.Reconfigure/SetBalanceInterval/SetExpiringSoonWindow
//
// 需重启（涉及监听地址、HTTP client 超时、auth_dir 等装配期依赖）：
//   - listen / auth_dir / state_file / upstream.* / upstash.* / session_sticky.*（TTL 类）
//   - panel.api_key：装配期进入 panel.Config，热改会让面板与网关密钥状态错位
//
// 危险值拦截：把 api_key 保存为空（除非 security.allow_anonymous=true）会被拒绝并
// 不落盘——避免运行期自废鉴权（v2.0.1 #4-2）。
//
// 落盘用"先写 tmp 再 rename"原子替换，且优先保留磁盘上的原始 JSON 结构（只改
// 面板表单覆盖到的键），避免把用户手写的注释性字段/未知键洗掉——这里直接整体
// 序列化校验后的配置，未知键在 json.Unmarshal 时已丢失，故先合并原始 map。
func saveConfig(raw []byte, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler) ([]string, error) {
	// 1) 解析原始 JSON 为 map（保留用户手写的未知键），再叠加面板提交的键。
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	var cur, incoming map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		cur = map[string]any{}
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	merged := mergeConfigMaps(cur, incoming)

	// 2) 校验（与启动同一套 Default+normalize），失败直接返回、不落盘。
	newCfg, err := ParseConfig(mergedJSON(merged))
	if err != nil {
		return nil, err
	}
	// 2.1) 危险值拦截（v2.0.1 #4-2）：拒绝把网关 api_key 保存为空（除非显式
	// security.allow_anonymous=true）——否则一次手滑/一次提权就能在运行期自废鉴权，
	// 且重启后无法自愈（见 UPGRADE_PROPOSALS.md #4/#15）。
	if err := rejectClearingAPIKey(cur, incoming, newCfg); err != nil {
		return nil, err
	}
	// 2.2) 危险值拦截（v2.2.0 #15，逻辑见 config_backup.go）：auth_dir/state_file 不
	// 允许保存为空；listen 的监听面（回环 ↔ 非回环）允许改但写审计日志，便于事后
	// 追溯公网暴露操作。oldRaw 是保存前原文（merge 已原地改写 cur，不能拿 cur 判旧值）。
	if err := verifyDangerousConfigValues(newCfg); err != nil {
		return nil, err
	}
	if note := listenChangeAuditNote(oldRaw, newCfg.Listen); note != "" {
		log.Printf("panel: 配置保存 %s", note)
	}

	// 3) 落盘：先备份当前生效版本（config.json.bak，v2.2.0 #15 回滚点），再原子替换。
	//    校验失败已在上面的 2.x 提前返回，不会触碰 .bak；写盘失败时 .bak 仍是当前生效版本。
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	if _, err := backupConfigFile(path); err != nil {
		return nil, err
	}
	if err := writeConfigAtomic(path, out); err != nil {
		return nil, err
	}

	// 4) 热应用（与 rollbackConfig 共用 config_backup.go 的 applyConfigRuntime，
	//    保存/回滚的热生效范围保证一致）。
	restart := applyConfigRuntime(newCfg, live, p, up, sch)
	// v2.6.0 audit P1: upstash connection / scheduler ticker startup condition are assembly-time
	// state; compare against the pre-save raw config and append fields that need a restart.
	return append(restart, startupOnlyChanges(oldRaw, newCfg)...), nil
}

// rejectClearingAPIKey 拦截「把网关 api_key 保存为空」（v2.0.1 #4-2）。
//
// 判据：本次提交显式写了空的 api_key，或磁盘上原本有 key 而保存后变空；且
// security.allow_anonymous 未显式打开。例外只放行两类合法场景：
//   - 本来就没有 key 且本次没动它（老配置/无鉴权测试环境；启动校验会挡住非回环监听）；
//   - 提交里同时把 security.allow_anonymous 设为 true（用户明确接受匿名）。
//
// 注意不拦 panel.api_key 清空：空 = 回落网关 key，是既有的向后兼容语义。
func rejectClearingAPIKey(cur, incoming map[string]any, newCfg *Config) error {
	if newCfg.Security.AllowAnonymous || newCfg.APIKey != "" {
		return nil
	}
	curKey, _ := cur["api_key"].(string)
	clearedExplicitly := false
	if v, ok := incoming["api_key"]; ok {
		s, _ := v.(string)
		clearedExplicitly = strings.TrimSpace(s) == ""
	}
	if curKey == "" && !clearedExplicitly {
		return nil
	}
	return fmt.Errorf("api_key 不能保存为空：网关 /v1/* 与面板将失去鉴权，且重启后无法自愈。" +
		"如确需匿名访问，请同时设置 security.allow_anonymous=true；" +
		"否则请填入一个随机密钥（例如 `openssl rand -hex 24` 的输出）")
}

// applyCNChatBase 把 config cn.chat_base 装配进上游 client；空则保留内置默认，
// 老配置（无 cn 段）行为不变。
func applyCNChatBase(up *upstream.Client, cfg *Config) {
	if cfg.CN.ChatBase != "" {
		up.ChatBaseCN = cfg.CN.ChatBase
	}
}

// restartRequiredFields 返回本次改动中无法热生效、需要重启进程的字段名。
// 恒返回完整清单中的"与当前进程装配期依赖相关"的项——面板据此提示用户。
func restartRequiredFields(c *Config) []string {
	var out []string
	// 这些字段在进程内被监听地址/HTTP client/目录句柄等装配期对象捕获。
	if c.Listen != "" {
		out = append(out, "listen")
	}
	if c.AuthDir != "" {
		out = append(out, "auth_dir")
	}
	if c.StateFile != "" {
		out = append(out, "state_file")
	}
	out = append(out, "upstream.timeout_seconds", "upstream.header_timeout_seconds", "upstream.idle_timeout_seconds")
	// upstream.user_agent 在装配期被写进出站 client（main.go 的 up.UserAgent = ...），
	// 之后不再读取——不在 livecfg 热快照里，也无法热改。此前漏列，导致面板改完
	// 显示"已保存"却不提示需要重启，用户以为没生效（issue #102 附带发现 2）。
	out = append(out, "upstream.user_agent")
	// cn.chat_base 同样只在装配期写进 up.ChatBaseCN（请求路径并发读取，不做热改）。
	out = append(out, "cn.chat_base")
	// v2.6.0 audit P1: upstash connection object is created at assembly time; any url/token
	// set/modify/clear requires restart. saveConfig/rollbackConfig append "upstash" by comparing
	// the pre-save config raw via startupOnlyChanges() (covers the clearing case).
	out = append(out, "session_sticky.ttl", "session_sticky.gc_interval")
	out = append(out, "logging.request_archive_enabled", "logging.request_retention_days", "logging.request_archive_max_mb")
	out = append(out, "server.read_timeout")
	// v2.2 #14：优雅停机时限只在启动时读取一次（进入 http.Server 停机编排），
	// 运行期热改无效，改后需重启 → 列入重启清单。
	out = append(out, "server.shutdown_timeout")
	// panel.api_key 在装配期进入 panel.Config（面板鉴权读取的静态字段），运行期
	// 不再读配置——热改会让「面板认新 key、进程内旧实例认旧 key」错位，故要求重启。
	// panel.poll_interval_sec 走 livecfg 快照热生效，不在此列。
	out = append(out, "panel.api_key")
	// v2.0.1 #10：启动首刷的延迟/强制开关只在 StartBalanceRefresh 启动时读一次，
	// 运行期热改无效（改后需重启才按新策略决定首刷），故列入重启清单。
	// schedule.balance_refresh_concurrency / catch_up_minutes 走 setter 热生效，不在此列。
	out = append(out, "schedule.balance_refresh_startup_delay", "schedule.balance_refresh_startup_force")
	// v2.6.0 audit P1: all fields below are captured by assembly-time objects (handler/notifier/
	// jobs/upstream client/auth switch). Panel saves would silently not take effect -> list them.
	out = append(out, "upstream.client_version", "upstream.cli_version", "upstream.client_name",
		"upstream.device_token", "upstream.device_token_file", "upstream.passthrough_ip")
	out = append(out, "global.enabled", "global.chat_base", "global.billing_base")
	out = append(out, "dns_servers")
	out = append(out, "session_sticky.enabled")
	out = append(out, "panel.events_enabled")
	out = append(out, "jobs.max_concurrency", "jobs.retain")
	out = append(out, "notify.webhook_url", "notify.timeout", "notify.events", "notify.channel",
		"notify.telegram_chat_id", "notify.title")
	out = append(out, "prompt.mode", "prompt.file")
	out = append(out, "security.allow_anonymous")
	out = append(out, "server.max_body_mb")
	// schedule.balance_refresh_enabled / task_sweep_enabled: if disabled at startup no ticker
	// goroutine is created; enabling at runtime needs a restart (disable / interval changes are hot).
	out = append(out, "schedule.balance_refresh_enabled", "schedule.task_sweep_enabled")
	return out
}

// mergeConfigMaps 把 incoming 深合并进 cur（原地），返回 cur。
// 对嵌套对象逐键覆盖而不是整体替换：面板表单只提交它管理的键，
// 未提交的兄弟键（含用户手写的未知键）保持原样。
func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if inMap, ok := v.(map[string]any); ok {
			if curMap, ok := cur[k].(map[string]any); ok {
				cur[k] = mergeConfigMaps(curMap, inMap)
				continue
			}
		}
		cur[k] = v
	}
	return cur
}

// mergedJSON 把合并后的 map 序列化回 JSON（供 ParseConfig 校验）。
func mergedJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}
