# perfbench —— 1k/5k/10k 账号端到端性能基准（v2.5.0 性能专项）

只读基准工具：**不修改生产代码**。生成合成账号池 + 最小配置，启动真实
`cmd/server` 二进制，测量启动耗时、面板读端点延迟/响应体大小、并发混合吞吐，
并采集进程 RSS/CPU；输出 JSON + 人读摘要。与 `internal/pool`、`internal/panel`
的 Go 微基准（`go test -bench`）配合定位热点。

## 用法

仓库根目录（Windows/pwsh 示例，环境变量按需）：

```powershell
$env:PATH="D:\dsh\tmp\go\go\bin;$env:PATH"
$env:GOPATH="D:\dsh\tmp\gopath"; $env:GOCACHE="D:\dsh\tmp\gocache"; $env:GOFLAGS="-mod=mod"

# 1k / 5k / 10k 三档（默认自动 go build ./cmd/server 到临时目录）
go run ./tools/perfbench -accounts 1000  -out D:\dsh\tmp\perf-1000.json
go run ./tools/perfbench -accounts 5000  -out D:\dsh\tmp\perf-5000.json
go run ./tools/perfbench -accounts 10000 -out D:\dsh\tmp\perf-10000.json
```

建议先构建工具再多次运行（避免每次重新编译）：

```powershell
go build -o D:\dsh\tmp\perfbench.exe ./tools/perfbench
D:\dsh\tmp\perfbench.exe -accounts 10000 -clients 10 -duration 20s `
    -out D:\dsh\tmp\perf-10000.json
```

常用参数：

| flag | 默认 | 说明 |
|---|---|---|
| `-bin` | 空 | 被测二进制路径；空 = 自动 `go build ./cmd/server` 到临时目录 |
| `-repo` | 空 | 仓库根；空 = 从当前目录向上找 `go.mod` |
| `-accounts` | 1000 | 合成账号数（1k/5k/10k） |
| `-clients` | 10 | 并发客户端数 |
| `-duration` | 20s | 并发混合负载持续时间 |
| `-iters` | 40 | 单端点顺序测量次数（强制 ≥30） |
| `-out` | 空 | JSON 报告路径；空 = `<workdir>/perf-<accounts>.json` |
| `-workdir` | 空 | 临时工作目录；空 = `D:\dsh\tmp\perfbench-<accounts>-<unix>` |
| `-keep` | false | 保留工作目录（配置/凭证/server.log/state.json 可复盘） |
| `-label` | v2.4.1 | 报告标签 |

## 基准环境（自动生成，可复现）

- **账号**：`workbuddy-u%06d.json` 平铺形，80% cn + 20% global，5% 企业版；token 为
  伪造值（不过真实上游）。
- **运行态**：`data/state.json` 注入确定性历史（约 2% 禁用、2.7% 暂停、4.3% 软冷却；
  积分/总额度分布；成功/失败计数；70% 当日已签；1/3 有 token 用量；1/5 有快过期积分
  与到期批次；1/4 有模型成本账本），避免"全零新号"低估 `List/statusOf` 成本。
- **标签**：`data/account-meta.json` 给 ~10% 账号打 `vip`、~5% 打 `trial`，供 `tag=` 基准。
- **最小配置**：`127.0.0.1:<随机端口>`、固定 api_key（`perfbench-key`）、**所有定时任务
  关闭**、request archive 关闭、Redis/粘性会话关闭；`global/cn` base 指向
  `http://127.0.0.1:1` 使启动预热立即失败回退内置种子 —— **完全离线、无外网依赖**。
- **随机种子固定（42）**：同 `-accounts` 下账号/状态/端口可复现（端口随机选空闲）。

## 测量口径

- **启动耗时**：进程 `Start()` → `/healthz` 首次 200。另报
  `startup_inproc_ms`（就绪时刻 − 进程创建时刻，去掉 Windows 进程创建/杀毒扫描
  固定开销）与 `startup_spawn_ms`（两者差）。
- **端点**：每端点 warmup 3 次后测 `-iters` 次（默认 40）；记录 p50/p95/max/mean/
  **响应体字节数**（Identity，不压缩）与状态码；另测 `Accept-Encoding: gzip` 的
  线上压缩字节（`*_gzip`）。`accounts_etag_304` 先取 ETag 再带 `If-None-Match` 循环
  测 304 路径。10k 档的**全量 overview**（含 accounts 数组）只单次尝试并记录
  耗时/字节（防重复压测打爆内存）。
- **并发吞吐**：`-clients` 个 goroutine 在 `-duration` 内混合打轻量端点
  （overview lite / accounts light / accounts tag / jobs / metrics / readyz，均 gzip），
  统计 req/s、错误数、p50/p95/max、总字节。
- **资源**：进程 RSS 与累计 CPU 秒（Windows `Get-Process`；Linux `/proc/<pid>`），
  在"就绪后"与"负载后"各取一次。**goroutines 外部进程不可观测**（无 pprof/expvar），
  报告恒为 `-1`。
- **热点定位**：本工具只给端到端数字；函数级热点用
  `internal/pool/perf_bench_test.go`、`internal/panel/perf_bench_test.go` 的
  `go test -bench ... -cpuprofile` + `go tool pprof -top`。

## 已知限制

- **客户端与服务端同机**：吞吐/延迟包含同机 CPU 竞争与 loopback 开销，绝对值仅用于
  **同机版本间对比**，不代表跨机生产数字。
- **无真实上游**：所有上游调用关闭（排程全关、base 指向不可达端口），不测
  chat 转发、token 刷新、签到等网络路径。
- **进程被直接 Kill**：Windows 无 SIGTERM，结束时直接 `Kill()`；不影响只读基准，
  但不会触发 `state.json` 优雅落盘（一次性临时目录）。
- **全量 overview 内存**：10k 档只单次尝试（JSON 约数 MB，重复压测无意义）；
  该端点生产实际很少用（v2.1 起前端走 lite/accounts 分页）。
- 端口用"监听后释放"方式选取，极小概率被其它进程抢占导致启动失败（重跑即可）。
- `data/state.json` 由基准工具生成（非服务真实落盘格式的完整字段）；服务启动会加载
  它并回写自己的完整格式（不影响本次运行测量）。
