// Command perfbench 端到端性能基准工具（v2.5.0 性能专项，只读：不修改生产代码）。
//
// 用途：生成 1k/5k/10k 合成账号 + 最小配置，启动被测 workbuddy2api-pro 二进制，
// 测量启动耗时、面板各读端点 p50/p95/max/bytes、并发混合吞吐，并采集进程 RSS/CPU，
// 输出 JSON + 人读摘要。配合 internal/pool、internal/panel 下的 Go 微基准定位热点。
//
// 典型用法（Windows/pwsh，仓库根目录）：
//
//	go run ./tools/perfbench -accounts 10000 -clients 10 -duration 20s \
//	    -out D:\dsh\tmp\perf-10000.json
//
// 也可先 `go build -o D:\dsh\tmp\perfbench.exe ./tools/perfbench` 再多次运行（避免
// 每次重新编译工具）。被测二进制默认由本工具自动 `go build ./cmd/server` 到临时目录。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 版本/固定参数。
const (
	toolVersion   = "v1"
	authDirName   = "auths"
	dataDirName   = "data"
	configName    = "config.json"
	serverBinName = "wb2a-perf-server"
	apiKey        = "perfbench-key"
)

func main() {
	var (
		binFlag   = flag.String("bin", "", "被测二进制路径；空 = 自动 go build ./cmd/server 到临时目录")
		repoFlag  = flag.String("repo", "", "仓库根目录；空 = 从当前目录向上查找 go.mod")
		accounts  = flag.Int("accounts", 1000, "合成账号数（1k/5k/10k）")
		clients   = flag.Int("clients", 10, "并发客户端数")
		duration  = flag.Duration("duration", 20*time.Second, "并发混合负载持续时间")
		iters     = flag.Int("iters", 40, "单端点顺序测量次数（>=30）")
		outFlag   = flag.String("out", "", "JSON 报告输出路径；空 = <workdir>/perf-<accounts>.json")
		workdir   = flag.String("workdir", "", "临时工作目录；空 = D:\\dsh\\tmp\\perfbench-<accounts>-<unix>")
		keep      = flag.Bool("keep", false, "保留临时工作目录（含日志/生成配置/凭证）")
		label     = flag.String("label", "v2.4.1", "报告标签（版本号）")
		timeoutEP = flag.Duration("endpoint-timeout", 15*time.Second, "单端点请求硬超时")
	)
	flag.Parse()
	if *iters < 30 {
		*iters = 30
	}
	started := time.Now()

	// ---- 1. 定位仓库 & 工作目录 ----
	repo := *repoFlag
	if repo == "" {
		wd, err := os.Getwd()
		if err != nil {
			fatalf("getwd: %v", err)
		}
		repo, err = findRepoRoot(wd)
		if err != nil {
			fatalf("%v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "cmd", "server")); err != nil {
		fatalf("repo %s 下找不到 cmd/server: %v", repo, err)
	}
	root := *workdir
	if root == "" {
		root = filepath.Join(tempDir(), fmt.Sprintf("perfbench-%d-%d", *accounts, time.Now().Unix()))
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		fatalf("mkdir workdir: %v", err)
	}
	if !*keep {
		defer func() { _ = os.RemoveAll(root) }()
	}

	// ---- 2. 被测二进制 ----
	binPath := *binFlag
	buildMS := 0.0
	if binPath == "" {
		binPath = filepath.Join(root, serverBinName+exeSuffix())
		fmt.Printf("[perfbench] 构建被测二进制: go build -o %s ./cmd/server ...\n", binPath)
		t0 := time.Now()
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd/server")
		cmd.Dir = repo
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		buildMS = float64(time.Since(t0).Microseconds()) / 1000
		if err != nil {
			fatalf("go build 失败: %v\n%s", err, out)
		}
		fmt.Printf("[perfbench] 构建完成 %.0fms\n", buildMS)
	} else {
		if _, err := os.Stat(binPath); err != nil {
			fatalf("-bin 不存在: %v", err)
		}
	}

	// ---- 3. 生成合成账号 + 最小配置 ----
	fmt.Printf("[perfbench] 生成 %d 个合成账号到 %s ...\n", *accounts, root)
	t0 := time.Now()
	if err := generateWorkdir(root, *accounts); err != nil {
		fatalf("生成测试环境: %v", err)
	}
	fmt.Printf("[perfbench] 环境生成完成 %.0fms（%s）\n", float64(time.Since(t0).Microseconds())/1000, root)

	// ---- 4. 启动被测二进制并测量 ----
	rep := &Report{
		Label:       *label,
		Tool:        toolVersion,
		GeneratedAt: started.Format(time.RFC3339),
		Accounts:    *accounts,
		Clients:     *clients,
		Iters:       *iters,
		DurationSec: duration.Seconds(),
		BuildMS:     round2(buildMS),
		Endpoints:   map[string]EndpointStat{},
	}
	rep.Host = machineInfo()
	srv, err := startServer(binPath, root)
	if err != nil {
		fatalf("启动被测服务: %v", err)
	}
	defer srv.stop()
	rep.StartupMS = round2(srv.startupMS)
	rep.StartupInProcMS = round2(srv.startupInProcMS)
	rep.StartupSpawnMS = round2(srv.startupMS - srv.startupInProcMS)
	rep.ServerVersion = srv.serverVersion
	rep.Port = srv.port
	rep.LoadedAccounts = srv.loadedAccounts
	rep.LogPath = srv.logPath
	rep.Goroutines = -1 // 无 pprof/expvar 端点，外部进程不可观测
	if srv.loadedAccounts != *accounts {
		fmt.Printf("[perfbench] WARN: 健康检查报告 total=%d，期望 %d\n", srv.loadedAccounts, *accounts)
	}
	if rss, cpu, err := procStats(srv.pid); err == nil {
		rep.RSSReadyMB = round2(float64(rss) / (1 << 20))
		rep.CPUReadySec = round2(cpu)
	}
	fmt.Printf("[perfbench] 服务就绪: startup=%.1fms pid=%d total=%d rss=%.1fMB\n",
		rep.StartupMS, srv.pid, srv.loadedAccounts, rep.RSSReadyMB)

	// ---- 5. 顺序端点基准 ----
	eps := endpointSpecs(*accounts)
	rep.Endpoints = measureEndpoints(srv, eps, *iters, *timeoutEP)

	// ---- 6. 并发混合吞吐 ----
	rep.Throughput = runLoad(srv, mixPaths, *clients, *duration)
	if rss, cpu, err := procStats(srv.pid); err == nil {
		rep.RSSAfterLoadMB = round2(float64(rss) / (1 << 20))
		rep.CPUTotalSec = round2(cpu)
	}
	_ = context.Background()

	// ---- 7. 输出 ----
	rep.TotalWallSec = round2(time.Since(started).Seconds())
	if *outFlag == "" {
		*outFlag = filepath.Join(root, fmt.Sprintf("perf-%d.json", *accounts))
	}
	if err := writeReport(*outFlag, rep); err != nil {
		fatalf("写报告: %v", err)
	}
	printSummary(rep)
	fmt.Printf("\n[perfbench] JSON 报告: %s\n", *outFlag)
	if *keep {
		fmt.Printf("[perfbench] 工作目录保留: %s\n", root)
	}
}

// ---------------------------------------------------------------------------
// 数据模型
// ---------------------------------------------------------------------------

// EndpointStat 单端点顺序测量的统计。
type EndpointStat struct {
	Path   string  `json:"path"`
	Count  int     `json:"count"`
	P50MS  float64 `json:"p50_ms"`
	P95MS  float64 `json:"p95_ms"`
	MaxMS  float64 `json:"max_ms"`
	MeanMS float64 `json:"mean_ms"`
	Bytes  int     `json:"bytes"`
	Status int     `json:"status"`
	Error  string  `json:"error,omitempty"`
	Note   string  `json:"note,omitempty"`
}

// Throughput 并发混合负载统计。
type Throughput struct {
	Clients     int     `json:"clients"`
	DurationSec float64 `json:"duration_sec"`
	Requests    int     `json:"requests"`
	Errors      int     `json:"errors"`
	RPS         float64 `json:"rps"`
	P50MS       float64 `json:"p50_ms"`
	P95MS       float64 `json:"p95_ms"`
	MaxMS       float64 `json:"max_ms"`
	BytesTotal  int64   `json:"bytes_total"`
}

// HostInfo 机器信息（便于跨机器对照）。
type HostInfo struct {
	Hostname  string  `json:"hostname"`
	OS        string  `json:"os"`
	Arch      string  `json:"arch"`
	NumCPU    int     `json:"num_cpu"`
	CPUModel  string  `json:"cpu_model,omitempty"`
	RAMGB     float64 `json:"ram_gb,omitempty"`
	GoVersion string  `json:"go_version"`
}

// Report 单档完整报告。
type Report struct {
	Label         string   `json:"label"`
	Tool          string   `json:"tool"`
	GeneratedAt   string   `json:"generated_at"`
	Host          HostInfo `json:"host"`
	ServerVersion string   `json:"server_version,omitempty"`
	Accounts      int      `json:"accounts"`
	Clients       int      `json:"clients"`
	Iters         int      `json:"iters"`
	DurationSec   float64  `json:"duration_sec"`
	BuildMS       float64  `json:"build_ms"`
	StartupMS     float64  `json:"startup_ms"`
	// StartupInProcMS = 就绪时刻 - 进程创建时刻（去掉进程创建/杀毒扫描等固定开销，
	// 更接近「服务自身启动」耗时）；StartupSpawnMS = 两者差（Windows 进程创建开销）。
	StartupInProcMS float64                 `json:"startup_inproc_ms"`
	StartupSpawnMS  float64                 `json:"startup_spawn_ms"`
	Port            int                     `json:"port"`
	LoadedAccounts  int                     `json:"loaded_accounts"`
	LogPath         string                  `json:"log_path,omitempty"`
	Endpoints       map[string]EndpointStat `json:"endpoints"`
	Throughput      Throughput              `json:"throughput"`
	RSSReadyMB      float64                 `json:"rss_ready_mb"`
	RSSAfterLoadMB  float64                 `json:"rss_after_load_mb"`
	CPUReadySec     float64                 `json:"cpu_ready_sec"`
	CPUTotalSec     float64                 `json:"cpu_total_sec"`
	TotalWallSec    float64                 `json:"total_wall_sec"`
	// Goroutines 外部进程不可观测（无 pprof/expvar 端点），恒为 -1，README 说明。
	Goroutines int `json:"goroutines"`
}

// ---------------------------------------------------------------------------
// 环境生成
// ---------------------------------------------------------------------------

// generateWorkdir 生成 auths/*.json、data/account-meta.json、config.json。
func generateWorkdir(root string, n int) error {
	authDir := filepath.Join(root, authDirName)
	dataDir := filepath.Join(root, dataDirName)
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	rng := rand.New(rand.NewSource(42)) // 固定种子：同 N 可复现
	// 并行写文件（10k 文件串行写约 1-2s，并行更快且不影响被测启动）。
	var (
		wg     sync.WaitGroup
		sem    = make(chan struct{}, 8)
		errMu  sync.Mutex
		firstE error
	)
	setErr := func(err error) {
		errMu.Lock()
		if firstE == nil {
			firstE = err
		}
		errMu.Unlock()
	}
	for i := 0; i < n; i++ {
		uid := fmt.Sprintf("u%06d", i)
		nick := fmt.Sprintf("bench-%s", uid)
		realm := "cn"
		domain := "copilot.tencent.com"
		if i%5 == 0 {
			realm = "global"
			domain = "workbuddy.ai"
		}
		enterprise := ""
		if i%20 == 0 {
			enterprise = fmt.Sprintf("ent-%06d", i)
		}
		exp := time.Now().Add(72 * time.Hour).Unix()
		doc := map[string]any{
			"accessToken":  fmt.Sprintf("at-%06d-%08x", i, rng.Int63()),
			"refreshToken": fmt.Sprintf("rt-%06d-%08x", i, rng.Int63()),
			"expiresAt":    exp,
			"domain":       domain,
			"realm":        realm,
			"uid":          uid,
			"nickname":     nick,
		}
		if enterprise != "" {
			doc["enterpriseId"] = enterprise
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		file := filepath.Join(authDir, "workbuddy-"+uid+".json")
		wg.Add(1)
		sem <- struct{}{}
		go func(file string, raw []byte) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := os.WriteFile(file, raw, 0o600); err != nil {
				setErr(err)
			}
		}(file, raw)
	}
	wg.Wait()
	if firstE != nil {
		return firstE
	}

	// account-meta.json：约 10% 账号打 vip、5% 打 trial（部分叠加），供 tag= 筛选基准。
	meta := map[string]any{"version": 1, "accounts": map[string]any{}}
	accountsMeta := meta["accounts"].(map[string]any)
	for i := 0; i < n; i++ {
		uid := fmt.Sprintf("u%06d", i)
		var tags []string
		if i%10 == 0 {
			tags = append(tags, "vip")
		}
		if i%20 == 0 {
			tags = append(tags, "trial")
		}
		if len(tags) == 0 {
			continue
		}
		accountsMeta[uid] = map[string]any{
			"tags":       tags,
			"updated_at": time.Now().Unix(),
		}
	}
	metaRaw, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(dataDir, "account-meta.json"), metaRaw, 0o600); err != nil {
		return err
	}

	// state.json：给账号写入「有历史」的运行态（积分/成败计数/冷却/到期批次/模型成本），
	// 让 pool.List()/overview/accounts 基准覆盖真实大池的字段量级，而不是全零新号。
	if err := generateStateFile(dataDir, n); err != nil {
		return err
	}

	// 最小配置由 writeConfig 生成（listen 端口在启动前写入；此处只建目录与账号）。
	_ = apiKey
	if err := writeConfig(filepath.Join(root, configName), 0); err != nil {
		return err
	}
	return nil
}

// generateStateFile 生成与 N 个账号对应的 data/state.json（pool 本地持久化格式：
// {"accounts":{uid: {...}}，"scheduling" 可省略}）。状态确定性可复现（固定规则），
// 覆盖 disabled/paused/cooling 三类非健康账号 + 积分/到期批次/模型成本等运行态。
func generateStateFile(dataDir string, n int) error {
	now := time.Now()
	today := now.Format("2006-01-02")
	mkTime := func(offset time.Duration) string {
		return now.Add(offset).UTC().Format(time.RFC3339Nano)
	}
	bucketDate := func(days int) (string, string) {
		t := now.Add(time.Duration(days) * 24 * time.Hour)
		return t.Format("2006-01-02"), t.UTC().Format(time.RFC3339Nano)
	}
	accounts := make(map[string]any, n)
	for i := 0; i < n; i++ {
		uid := fmt.Sprintf("u%06d", i)
		st := map[string]any{
			// credits：均匀分布 100..10000，保证 credits_sum 非平凡。
			"credits":            int64(100 + (i*37)%9900),
			"credits_total":      int64(1000 + (i*101)%9000),
			"disabled":           false,
			"cool_kind":          0, // pool.CoolKind 是 int 枚举：0=hard、1=soft（不是字符串）
			"soft_streak":        i % 17,
			"session_dead_fails": 0,
			"consecutive_fails":  i % 5,
			"credits_expiring":   0,
		}
		switch {
		case i%50 == 1: // ~2% 禁用（运维剔除/死亡账号）
			st["disabled"] = true
			st["reason"] = "bench disabled"
		case i%37 == 2: // ~2.7% 暂停选号
			st["paused"] = true
		case i%23 == 3: // ~4.3% 软冷却
			st["until"] = mkTime(30 * time.Minute)
			st["cool_kind"] = 1 // CoolSoft
			st["reason"] = "429 rate limit"
		}
		// 历史成败与最近使用（影响 full 字段与软冷却退避展示）。
		st["success_count"] = int64(i % 500)
		st["err_total"] = int64(i % 13)
		st["last_success"] = mkTime(-time.Duration(i%240) * time.Minute)
		if i%7 == 0 {
			st["last_err"] = mkTime(-time.Duration(i%90) * time.Minute)
		}
		// 当日已签（约 70%：面板「签到」按钮状态字段）。
		if i%10 < 7 {
			st["last_checkin_day"] = today
		}
		// token 用量（约 1/3 账号有聊天历史）。
		if i%3 == 0 {
			st["token_usage"] = map[string]any{
				"request_count":     int64(i % 500),
				"usage_count":       int64(i % 400),
				"prompt_tokens":     int64(i * 111),
				"completion_tokens": int64(i * 222),
				"total_tokens":      int64(i * 333),
				"last_latency_ms":   int64(200 + i%2000),
				"last_used_at":      mkTime(-time.Duration(i%180) * time.Minute),
				"last_model":        "claude-sonnet-4-20250514",
			}
		}
		// 快过期积分 + 到期批次（约 1/5 账号：驱动「最早到期优先」路由与首页卡片）。
		if i%5 == 0 {
			date1, at1 := bucketDate(3 + i%10)
			date2, at2 := bucketDate(30 + i%60)
			st["credits_expiring"] = int64(50 + i%300)
			st["credits_earliest_expiry"] = at1
			st["credits_earliest_remaining"] = int64(50 + i%300)
			st["credits_expiry_buckets"] = []map[string]any{
				{"date": date1, "expires_at": at1, "remain": int64(50 + i%300)},
				{"date": date2, "expires_at": at2, "remain": int64(100 + i%500)},
			}
		}
		// 实测扣费账本（约 1/4 账号 × 2 个模型：pool.statusOf 会做惰性过滤）。
		if i%4 == 0 {
			st["model_costs"] = map[string]any{
				"claude-sonnet-4-20250514": map[string]any{
					"cost_per_1k": 0.003, "last_seen": mkTime(-time.Duration(i%60) * time.Minute), "samples": 3,
				},
				"gpt-5": map[string]any{
					"cost_per_1k": 0.01, "last_seen": mkTime(-time.Duration(i%120) * time.Minute), "samples": 1,
				},
			}
		}
		accounts[uid] = st
	}
	raw, err := json.Marshal(map[string]any{"accounts": accounts})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, "state.json"), raw, 0o600)
}

// writeConfig 把最小配置写入 config.json（listen 端口显式指定，0 = 随机选空闲端口）。
func writeConfig(path string, port int) error {
	if port == 0 {
		p, err := freePort()
		if err != nil {
			return err
		}
		port = p
	}
	cfg := map[string]any{
		"listen":     fmt.Sprintf("127.0.0.1:%d", port),
		"api_key":    apiKey,
		"auth_dir":   filepath.Join(filepath.Dir(path), authDirName),
		"state_file": filepath.Join(filepath.Dir(path), dataDirName, "state.json"),
		"panel": map[string]any{
			"api_key":           apiKey,
			"poll_interval_sec": 5,
		},
		"security": map[string]any{"allow_anonymous": false},
		"logging": map[string]any{
			"request_archive_enabled": false,
			"request_client_info":     false,
		},
		"jobs": map[string]any{"max_concurrency": 4, "retain": 50},
		"schedule": map[string]any{
			"checkin_enabled":         false,
			"growth_enabled":          false,
			"travel_enabled":          false,
			"activity_enabled":        false,
			"keepalive_enabled":       false,
			"blackcat_enabled":        false,
			"balance_refresh_enabled": false,
			"task_sweep_enabled":      false,
		},
		"session_sticky": map[string]any{"enabled": false},
		"upstash":        map[string]any{"url": "", "token": ""},
		// 网络隔离：启动预热（warmModelRates）会打上游域名，若本机无外网/被墙会让
		// 启动耗时抖动。把两域 base 指向本地不可达端口 → 预热立即失败回退内置种子，
		// 基准完全离线、可复现（不影响面板读端点，本基准不发起真实上游请求）。
		"global": map[string]any{"enabled": true, "chat_base": "http://127.0.0.1:1", "billing_base": "http://127.0.0.1:1"},
		"cn":     map[string]any{"chat_base": "http://127.0.0.1:1"},
		"upstream": map[string]any{
			"timeout_seconds":        5,
			"header_timeout_seconds": 5,
			"idle_timeout_seconds":   10,
		},
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// freePort 选一个当前空闲的本地端口（监听后立即释放；极小概率被抢占，失败时重试）。
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// findRepoRoot 从 start 向上找包含 go.mod 的目录。
func findRepoRoot(start string) (string, error) {
	dir := start
	for {
		if fi, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !fi.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("从 %s 向上未找到 go.mod（请在仓库内运行，或使用 -repo）", start)
		}
		dir = parent
	}
}

// tempDir 基准临时目录（Windows 任务约定统一落 D:\dsh\tmp）。
func tempDir() string {
	if runtime.GOOS == "windows" {
		return `D:\dsh\tmp`
	}
	return os.TempDir()
}

// exeSuffix Windows 二进制后缀。
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// ---------------------------------------------------------------------------
// 启动/停止被测进程
// ---------------------------------------------------------------------------

type serverProc struct {
	cmd             *exec.Cmd
	pid             int
	port            int
	baseURL         string
	startupMS       float64
	startupInProcMS float64
	serverVersion   string
	loadedAccounts  int
	logPath         string
	logFile         *os.File
	exited          chan struct{}
	waitErr         error
	mu              sync.Mutex
}

// startServer 写配置（随机端口）→ 启动二进制 → 轮询 /healthz 200 并计时。
func startServer(binPath, root string) (*serverProc, error) {
	cfgPath := filepath.Join(root, configName)
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("选端口: %w", err)
	}
	if err := writeConfig(cfgPath, port); err != nil {
		return nil, fmt.Errorf("写配置: %w", err)
	}
	logPath := filepath.Join(root, "server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("建日志: %w", err)
	}
	cmd := exec.Command(binPath, "-config", cfgPath)
	cmd.Dir = root
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("Start: %w", err)
	}
	sp := &serverProc{
		cmd:     cmd,
		pid:     cmd.Process.Pid,
		port:    port,
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		logPath: logPath,
		logFile: logFile,
		exited:  make(chan struct{}),
	}
	go func() {
		sp.waitErr = cmd.Wait()
		close(sp.exited)
	}()

	client := &http.Client{Timeout: 1500 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}
	deadline := time.Now().Add(180 * time.Second)
	var lastBody []byte
	for time.Now().Before(deadline) {
		select {
		case <-sp.exited:
			sp.logFile.Close()
			return nil, fmt.Errorf("进程提前退出: %v（日志 %s）", sp.waitErr, logPath)
		default:
		}
		resp, err := client.Get(sp.baseURL + "/healthz")
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			lastBody = body
			if resp.StatusCode == http.StatusOK {
				readyAt := time.Now()
				sp.startupMS = float64(readyAt.Sub(t0).Microseconds()) / 1000
				sp.loadedAccounts = parseHealthzTotal(body)
				// 进程创建时刻（去掉 Windows 进程创建/AV 扫描的固定开销；失败则记 0）。
				if ct, err := processCreateTime(sp.pid); err == nil && ct.Before(readyAt) {
					sp.startupInProcMS = float64(readyAt.Sub(ct).Microseconds()) / 1000
				}
				sp.serverVersion = fetchServerVersion(sp.baseURL)
				return sp, nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	sp.stop()
	return nil, fmt.Errorf("等待 /healthz 超时（最后响应: %s，日志 %s）", truncate(string(lastBody), 200), logPath)
}

// parseHealthzTotal 从 /healthz JSON 取 total。
func parseHealthzTotal(body []byte) int {
	var v struct {
		Total int `json:"total"`
	}
	if json.Unmarshal(body, &v) != nil {
		return -1
	}
	return v.Total
}

// processCreateTime 读进程创建时刻（Windows 用 .NET StartTime；Linux 用 /proc/pid/stat
// 的 starttime 不可靠，这里仅在 Windows 提供，其他平台返回错误）。
func processCreateTime(pid int) (time.Time, error) {
	if runtime.GOOS != "windows" {
		return time.Time{}, fmt.Errorf("unsupported on %s", runtime.GOOS)
	}
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		fmt.Sprintf(`(Get-Process -Id %d -ErrorAction Stop).StartTime.ToUniversalTime().ToString("o")`, pid)).Output()
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, strings.TrimSpace(string(out)))
}

// fetchServerVersion 从 /panel/api/overview?lite=1 取服务版本号（失败返回空）。
func fetchServerVersion(baseURL string) string {
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/panel/api/overview?lite=1", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var v struct {
		Version string `json:"version"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v) != nil {
		return ""
	}
	return v.Version
}

// stop 终止被测进程并等待退出（Windows 无 SIGTERM，直接 Kill；进程内数据为一次性基准）。
func (sp *serverProc) stop() {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.cmd == nil || sp.cmd.Process == nil {
		return
	}
	_ = sp.cmd.Process.Kill()
	select {
	case <-sp.exited:
	case <-time.After(10 * time.Second):
	}
	if sp.logFile != nil {
		_ = sp.logFile.Close()
	}
}

// procStats 读取进程 RSS（字节）与累计 CPU 秒。
func procStats(pid int) (rssBytes int64, cpuSec float64, err error) {
	if runtime.GOOS == "windows" {
		out, e := exec.Command("powershell", "-NoProfile", "-Command",
			fmt.Sprintf(`$p=Get-Process -Id %d -ErrorAction Stop; "$($p.WorkingSet64) $($p.CPU)"`, pid)).Output()
		if e != nil {
			return 0, 0, e
		}
		fields := strings.Fields(strings.TrimSpace(string(out)))
		if len(fields) < 2 {
			return 0, 0, fmt.Errorf("powershell 输出异常: %q", string(out))
		}
		rss, e1 := strconv.ParseInt(fields[0], 10, 64)
		cpu, e2 := strconv.ParseFloat(fields[1], 64)
		if e1 != nil || e2 != nil {
			return 0, 0, fmt.Errorf("解析进程指标失败: %q", string(out))
		}
		return rss, cpu, nil
	}
	status, e := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if e != nil {
		return 0, 0, e
	}
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseInt(fields[1], 10, 64)
				rssBytes = kb * 1024
			}
		}
	}
	stat, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if e == nil {
		fields := strings.Fields(string(stat))
		if len(fields) > 15 {
			ut, _ := strconv.ParseFloat(fields[13], 64)
			st, _ := strconv.ParseFloat(fields[14], 64)
			cpuSec = (ut + st) / 100 // Linux ticks（通常 100Hz）
		}
	}
	if rssBytes == 0 {
		return 0, 0, fmt.Errorf("读不到 VmRSS")
	}
	return rssBytes, cpuSec, nil
}

// machineInfo 机器信息（Windows 额外取 CPU 型号/内存；失败不致命）。
func machineInfo() HostInfo {
	h, _ := os.Hostname()
	hi := HostInfo{Hostname: h, OS: runtime.GOOS, Arch: runtime.GOARCH, NumCPU: runtime.NumCPU(), GoVersion: runtime.Version()}
	if runtime.GOOS == "windows" {
		out, err := exec.Command("powershell", "-NoProfile", "-Command",
			`$c=Get-CimInstance Win32_Processor | Select-Object -First 1; $s=Get-CimInstance Win32_ComputerSystem; "$($c.Name)|$([math]::Round($s.TotalPhysicalMemory/1GB,1))"`).Output()
		if err == nil {
			parts := strings.Split(strings.TrimSpace(string(out)), "|")
			if len(parts) == 2 {
				hi.CPUModel = strings.TrimSpace(parts[0])
				if gb, e := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); e == nil {
					hi.RAMGB = gb
				}
			}
		}
	}
	return hi
}

// ---------------------------------------------------------------------------
// 端点定义与顺序测量
// ---------------------------------------------------------------------------

// endpointSpec 单端点定义；Gzip=true 时带 Accept-Encoding: gzip 且记录线上压缩字节。
type endpointSpec struct {
	name    string
	path    string
	gzip    bool
	etag    bool
	timeout time.Duration
	// skipIfAccounts 账号数超过该值时跳过（如 full overview 只在 1k/5k 跑）。
	skipIfAccounts int
	note           string
}

// endpointSpecs 返回该账号规模下要跑的全部端点。
func endpointSpecs(accounts int) []endpointSpec {
	eps := []endpointSpec{
		{name: "overview_lite", path: "/panel/api/overview?lite=1", note: "面板 5s 轮询轻量概况"},
		{name: "overview_lite_gzip", path: "/panel/api/overview?lite=1", gzip: true},
		{name: "accounts_light_p100", path: "/panel/api/accounts?page=1&page_size=100&fields=light"},
		{name: "accounts_light_p100_gzip", path: "/panel/api/accounts?page=1&page_size=100&fields=light", gzip: true},
		{name: "accounts_full_p100", path: "/panel/api/accounts?page=1&page_size=100&fields=full"},
		{name: "accounts_q", path: "/panel/api/accounts?page=1&page_size=100&q=bench",
			note: "q 命中全部昵称（全量过滤扫描后再分页）"},
		{name: "accounts_sort_credits", path: "/panel/api/accounts?page=1&page_size=100&sort=credits"},
		{name: "accounts_tag", path: "/panel/api/accounts?page=1&page_size=100&tag=vip",
			note: "标签反向索引命中（约 10% 账号）"},
		{name: "accounts_etag_304", path: "/panel/api/accounts?page=1&page_size=100&fields=light", etag: true,
			note: "命中 ETag → 304 零 body（注意：服务端仍做全量 List+filter+sort+hash）"},
		{name: "jobs_list", path: "/panel/api/jobs?limit=20"},
		{name: "metrics", path: "/panel/api/metrics", note: "Prometheus 文本（含全池 O(n) 统计）"},
		{name: "readyz", path: "/panel/api/readyz", note: "含目录可写性探测（建/删临时文件）"},
		{name: "accounts_all_light", path: "/panel/api/accounts?page=1&page_size=0&fields=light", timeout: 20 * time.Second,
			note: "全部（服务端上限 2000 条）；大池受 2000 上限保护"},
	}
	// 全量 overview（含 accounts 数组）只在 1k/5k 跑；10k 用单次尝试记录超大响应行为。
	eps = append(eps, endpointSpec{
		name: "overview_full", path: "/panel/api/overview",
		timeout: 25 * time.Second, skipIfAccounts: 5000,
		note: "含 accounts 数组的全量 JSON（10k 跳过，另行单次尝试）",
	})
	return eps
}

// measureEndpoints 顺序测量端点（每端点 warmup 3 次，然后 iters 次）。
func measureEndpoints(sp *serverProc, specs []endpointSpec, iters int, epTimeout time.Duration) map[string]EndpointStat {
	out := make(map[string]EndpointStat, len(specs))
	client := &http.Client{
		Transport: &http.Transport{
			DisableCompression:  true, // 自行控制 Accept-Encoding，准确统计 bytes
			MaxIdleConns:        32,
			MaxIdleConnsPerHost: 32,
		},
	}
	for _, spec := range specs {
		if spec.skipIfAccounts > 0 && sp.loadedAccounts > spec.skipIfAccounts {
			out[spec.name] = EndpointStat{Path: spec.path, Error: "skipped", Note: spec.note}
			continue
		}
		timeout := spec.timeout
		if timeout == 0 {
			timeout = epTimeout
		}
		name := spec.name
		path := spec.path
		// warmup：首个请求可能触发懒初始化（account-meta 载入、ETag 首次构建等）。
		var ifNoneMatch string
		if spec.etag {
			// ETag 端点：先打一次拿 ETag，再做 304 循环。
			_, _, _, etag, err := doRequestWith(client, sp.baseURL, path, spec.gzip, timeout, "")
			if err != nil {
				out[name] = EndpointStat{Path: path, Error: "etag warmup: " + err.Error(), Note: spec.note}
				continue
			}
			ifNoneMatch = etag
			if etag == "" {
				out[name] = EndpointStat{Path: path, Error: "no etag header", Note: spec.note}
				continue
			}
		} else {
			for i := 0; i < 3; i++ {
				_, _, _, _, _ = doRequestWith(client, sp.baseURL, path, spec.gzip, timeout, "")
			}
		}
		stats := EndpointStat{Path: spec.path, Note: spec.note}
		lat := make([]float64, 0, iters)
		var lastBytes int
		var lastStatus int
		var lastErr error
		for i := 0; i < iters; i++ {
			status, n, ms, _, err := doRequestWith(client, sp.baseURL, path, spec.gzip, timeout, ifNoneMatch)
			lastStatus, lastBytes, lastErr = status, n, err
			if err != nil {
				continue
			}
			lat = append(lat, ms)
		}
		if len(lat) == 0 {
			stats.Error = fmt.Sprintf("all failed: %v", lastErr)
			out[name] = stats
			continue
		}
		sort.Float64s(lat)
		stats.Count = len(lat)
		stats.P50MS = round2(percentile(lat, 0.50))
		stats.P95MS = round2(percentile(lat, 0.95))
		stats.MaxMS = round2(lat[len(lat)-1])
		stats.MeanMS = round2(mean(lat))
		stats.Bytes = lastBytes
		stats.Status = lastStatus
		if lastErr != nil {
			stats.Error = lastErr.Error()
		}
		out[name] = stats
	}
	// 10k：full overview 单次尝试（记录耗时/是否超时/响应字节），不做 30 次（会拖爆内存）。
	if sp.loadedAccounts > 5000 {
		clientLong := &http.Client{Transport: &http.Transport{DisableCompression: true}}
		t0 := time.Now()
		status, n, _, _, err := doRequestWith(clientLong, sp.baseURL, "/panel/api/overview", false, 60*time.Second, "")
		el := float64(time.Since(t0).Microseconds()) / 1000
		st := EndpointStat{Path: "/panel/api/overview", Count: 1, P50MS: round2(el), P95MS: round2(el),
			MaxMS: round2(el), MeanMS: round2(el), Bytes: n, Status: status,
			Note: "10k 账号全量 overview 单次尝试（不重复压测，防 OOM）"}
		if err != nil {
			st.Error = err.Error()
		}
		out["overview_full"] = st
	}
	return out
}

// doRequest 发一次 GET 并读完 body；返回状态码/字节数/耗时(ms)/错误。
func doRequest(c *http.Client, baseURL, path string, gzip bool, timeout time.Duration) (int, int, float64, error) {
	status, n, ms, _, err := doRequestWith(c, baseURL, path, gzip, timeout, "")
	return status, n, ms, err
}

// doRequestWith 同 doRequest，额外支持 If-None-Match 并返回响应 ETag 头。
func doRequestWith(c *http.Client, baseURL, path string, gzip bool, timeout time.Duration, ifNoneMatch string) (int, int, float64, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return 0, 0, 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if gzip {
		req.Header.Set("Accept-Encoding", "gzip")
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	t0 := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return 0, 0, float64(time.Since(t0).Microseconds()) / 1000, "", err
	}
	etag := resp.Header.Get("ETag")
	n, readErr := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	ms := float64(time.Since(t0).Microseconds()) / 1000
	if readErr != nil {
		return resp.StatusCode, int(n), ms, etag, readErr
	}
	return resp.StatusCode, int(n), ms, etag, nil
}

// ---------------------------------------------------------------------------
// 并发混合吞吐
// ---------------------------------------------------------------------------

// mixPaths 并发负载混合的轻量端点（与前端 5s 轮询的调用面一致）。
var mixPaths = []struct {
	path string
	gzip bool
}{
	{"/panel/api/overview?lite=1", true},
	{"/panel/api/accounts?page=1&page_size=100&fields=light", true},
	{"/panel/api/accounts?page=1&page_size=100&fields=light", true},
	{"/panel/api/accounts?page=1&page_size=100&tag=vip", true},
	{"/panel/api/jobs?limit=20", false},
	{"/panel/api/metrics", false},
	{"/panel/api/readyz", false},
}

// runLoad clients 个 goroutine 持续 duration 混合打轻量端点，统计 RPS/p95/错误/字节。
func runLoad(sp *serverProc, mix []struct {
	path string
	gzip bool
}, clients int, duration time.Duration) Throughput {
	if clients <= 0 {
		clients = 1
	}
	tr := &Throughput{Clients: clients, DurationSec: duration.Seconds()}
	client := &http.Client{
		Transport: &http.Transport{
			DisableCompression:  true,
			MaxIdleConns:        clients * 4,
			MaxIdleConnsPerHost: clients * 2,
		},
	}
	deadline := time.Now().Add(duration)
	var (
		mu       sync.Mutex
		allLat   []float64
		requests int64
		errors   int64
		bytes    int64
		wg       sync.WaitGroup
	)
	start := time.Now()
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			local := make([]float64, 0, 1024)
			i := seed
			for time.Now().Before(deadline) {
				m := mix[i%len(mix)]
				i++
				status, n, ms, err := doRequest(client, sp.baseURL, m.path, m.gzip, 30*time.Second)
				atomic.AddInt64(&requests, 1)
				atomic.AddInt64(&bytes, int64(n))
				if err != nil || status != http.StatusOK {
					atomic.AddInt64(&errors, 1)
					continue
				}
				local = append(local, ms)
			}
			mu.Lock()
			allLat = append(allLat, local...)
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	mu.Lock()
	sort.Float64s(allLat)
	tr.Requests = int(requests)
	tr.Errors = int(errors)
	tr.BytesTotal = atomic.LoadInt64(&bytes)
	tr.RPS = round2(float64(requests) / elapsed)
	if len(allLat) > 0 {
		tr.P50MS = round2(percentile(allLat, 0.50))
		tr.P95MS = round2(percentile(allLat, 0.95))
		tr.MaxMS = round2(allLat[len(allLat)-1])
	}
	mu.Unlock()
	tr.DurationSec = round2(elapsed)
	return *tr
}

// ---------------------------------------------------------------------------
// 输出
// ---------------------------------------------------------------------------

// writeReport 写 JSON 报告（含目录自动创建）。
func writeReport(path string, rep *Report) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

// printSummary 打印人读摘要。
func printSummary(rep *Report) {
	fmt.Printf("\n================ perfbench %s · accounts=%d ================\n", rep.Label, rep.Accounts)
	fmt.Printf("host: %s %s/%s  cpu=%d  ram=%.1fGB  go=%s\n",
		rep.Host.Hostname, rep.Host.OS, rep.Host.Arch, rep.Host.NumCPU, rep.Host.RAMGB, rep.Host.GoVersion)
	fmt.Printf("server: %s  pid/port=%d  accounts_loaded=%d\n", rep.ServerVersion, rep.Port, rep.LoadedAccounts)
	fmt.Printf("启动: %.1fms  (进程创建后 %.1fms + 进程创建/扫描 %.1fms; 构建 %.0fms; RSS %.1fMB, CPU %.2fs)\n",
		rep.StartupMS, rep.StartupInProcMS, rep.StartupSpawnMS, rep.BuildMS, rep.RSSReadyMB, rep.CPUReadySec)
	fmt.Printf("\n%-24s %7s %8s %8s %8s %10s %6s\n", "endpoint", "n", "p50ms", "p95ms", "maxms", "bytes", "status")
	names := make([]string, 0, len(rep.Endpoints))
	for n := range rep.Endpoints {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		st := rep.Endpoints[n]
		if st.Error == "skipped" {
			fmt.Printf("%-24s %7s %8s %8s %8s %10s %6s  (skipped)\n", n, "-", "-", "-", "-", "-", "-")
			continue
		}
		if st.Error != "" {
			fmt.Printf("%-24s %7d %8.2f %8.2f %8.2f %10d %6d  ERR %s\n",
				n, st.Count, st.P50MS, st.P95MS, st.MaxMS, st.Bytes, st.Status, truncate(st.Error, 80))
			continue
		}
		fmt.Printf("%-24s %7d %8.2f %8.2f %8.2f %10d %6d\n",
			n, st.Count, st.P50MS, st.P95MS, st.MaxMS, st.Bytes, st.Status)
	}
	tp := rep.Throughput
	fmt.Printf("\n并发混合: clients=%d  %.1fs  requests=%d  errors=%d  rps=%.1f  p50=%.1fms p95=%.1fms max=%.1fms  bytes=%d\n",
		tp.Clients, tp.DurationSec, tp.Requests, tp.Errors, tp.RPS, tp.P50MS, tp.P95MS, tp.MaxMS, tp.BytesTotal)
	fmt.Printf("负载后 RSS: %.1fMB（就绪时 %.1fMB）  进程累计 CPU %.2fs\n",
		rep.RSSAfterLoadMB, rep.RSSReadyMB, rep.CPUTotalSec)
	fmt.Printf("总墙钟: %.1fs  goroutines=外部进程不可观测(-1)\n", rep.TotalWallSec)
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// percentile 已排序切片的分位数（最近秩法）。
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[perfbench] FATAL: "+format+"\n", args...)
	os.Exit(1)
}
