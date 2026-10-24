# 性能与容量（v2.5.0）

> 测量口径：Windows 10/11 VM，8 vCPU（Intel Xeon Skylake-class）/ 16GB，Go 1.24.5；
> 客户端与服务端**同机** loopback，因此绝对吞吐/延迟仅适合同机版本间对比。
> 账号为合成假凭证（含真实运行态：积分/冷却/到期批次/模型成本），排程全关、上游指向本机不可达端口。

## 1. 结论摘要（10,000 账号）

| 指标 | v2.4.1（优化前） | v2.5.0（优化后） | 变化 |
|---|---|---|---|
| 进程启动（warm 页缓存） | 1,600 ms | **600 ms** | **-62%** |
| 负载后 RSS | 214 MB | **92 MB** | **-57%** |
| 并发吞吐（10 客户端，混合读端点） | 327 req/s | **4,132 req/s** | **+1,164%** |
| 混合负载 p95 | 69 ms | **5.9 ms** | **-91%** |
| `overview?lite=1`（344B） | 12.5 ms | **0.54 ms** | ~23× |
| `metrics`（1.3KB） | 38 ms | **0.57 ms** | ~67× |
| `accounts` 分页 100 行 light | 31 ms | **2.9 ms** | ~11× |
| `accounts` ETag 命中 304 | 31 ms | **0.57 ms** | ~54× |
| `accounts` 全部（上限 2000） | 73 ms | **24 ms** | ~3× |
| 全量 `overview`（含 10k accounts，7.1MB） | 178 ms | 123 ms | ~1.4× |

1k / 5k 账号同口径：

| 账号数 | 启动（warm） | 负载后 RSS | 吞吐 | p95 |
|---|---|---|---|---|
| 1,000 | 296 → **~300 ms**（波动小） | 48 → **61 MB** | 1,637 → **4,635 req/s** | 14 → **5.2 ms** |
| 5,000 | 881 → **416 ms** | 120 → **69 MB** | 583 → **4,826 req/s** | 40 → **4.9 ms** |
| 10,000 | 1,600 → **600 ms** | 214 → **92 MB** | 327 → **4,132 req/s** | 69 → **5.9 ms** |

> 说明：1k 启动数字受 Windows Defender 首次扫描新 exe 影响（重建二进制首启固定 +~1s，缓存后消失）；
> 表中 1k 启动取 warm 口径。

## 2. 主要优化点

| 层 | 改动 | 效果（10k） |
|---|---|---|
| `internal/pool` | `Snapshot()` 缓存只读快照（uid 有序、命中 0 alloc）；`Summary()` 一次遍历聚合面板全部指标（version+TTL 500ms 缓存）；`Version()` 代际号；`ModelLockView` 空锁 O(1)（模型锁条目原子计数）；`AvailableUIDsSorted()` 缓存；`statusOf` 共用 now/today、单条目模型台账快路径 | List 16.3ms→Snapshot 命中 19ns；Summary 25.6ms→34ns；ModelLockView 无锁 3.69ms→10ns |
| `internal/panel` | overview/metrics/SSE 全部改用 `Summary()`（不再多次全池扫描）；accounts 改 `Snapshot()` + **索引过滤/排序/分页**（不拷贝 527B/行 Status，默认 uid 零排序）；**ETag/304 LRU 缓存**（归一化 query→etag，pool/meta 代际失效，TTL 10s 兜底）；gzip 缓冲 `sync.Pool`；新增 `overview?accounts=0` 轻量模式 | lite 9.3ms→37µs；metrics 19.5ms→61µs；accounts 29.7ms→2.85ms；ETag304 28.3ms→16µs；分配 accounts 11.6MB→357KB |
| `internal/auth` | `LoadDir` worker pool 并行加载（min(16,NumCPU*2)，结果稳定排序）；`Realm()` 记忆化（atomic 快路径，globalEnabled 与写回自动失效）；auth 文件 Parse 单次解析（去掉双重 Unmarshal） | 10k LoadDir ~1.1–2.2s→251–375ms（warm）；Realm 168.5ns→5.1ns |
| `cmd/server` | 启动分段计时日志：`startup: load auths took Xms` / `startup: pool load+sync took Xms` / `startup: ready took Xms` | 便于定位启动瓶颈 |

## 3. 容量建议

- **1k–10k 账号**：默认配置即可；面板建议使用服务端分页（`page_size` 100/200），账号池视图轮询 = `overview?lite=1` + 当前页 accounts（ETag 304）。
- **脚本/监控**：优先 `GET /panel/api/overview?accounts=0`（或 `?lite=1`）获取汇总；需要全量账号时用 `/panel/api/accounts` 分页。
- **>10k**：建议开启 `panel.events_enabled`（SSE）替代高频轮询；`accounts?page_size=0`（全部）上限 2000，仅用于导出场景。
- **启动**：auths 目录放本地盘（冷盘/网络盘/Defender 实时扫描会显著拖慢 10k 小文件读取）；启动日志可核对三段耗时。
- **全量 overview**（含 accounts 数组）在 5k 时约 3.5MB、10k 时约 7MB：属兼容接口，面板不再使用；新脚本请改用 lite/分页。

## 4. 可复现基准

```powershell
# 构建
go build -o D:\tmp\wb2a-perf.exe ./cmd/server
go build -o D:\tmp\perfbench.exe ./tools/perfbench

# 1k / 5k / 10k（-duration 控制并发段时长，-out 输出 JSON）
D:\tmp\perfbench.exe -bin D:\tmp\wb2a-perf.exe -accounts 10000 -clients 10 -duration 20s -out D:\tmp\perf-10000.json
```

微基准（同机）：

```powershell
go test -run '^$' -bench 'BenchmarkPool...' -benchmem ./internal/pool/
go test -run '^$' -bench 'BenchmarkPerfOptim...' -benchmem ./internal/panel/
go test -run '^$' -bench 'BenchmarkLoadDir10k' -benchmem ./internal/auth/
```

## 5. 已知剩余瓶颈

1. **全量 `overview`**（默认含 accounts）：10k 约 123ms/7.1MB，属旧兼容契约；轻量场景用 `?lite=1` 或 `?accounts=0`。
2. **`accounts?page_size=0`（全部，上限 2000）**：10k 时约 24ms/1.3MB，建议配合筛选/分页使用。
3. **`List()` 旧语义**（每次拷贝）：panel 热路径已迁移到 `Snapshot()`，仍有 scheduler/jobs 等低频调用点。
4. **ModelLockView 有活跃锁时**仍 O(n×行数)（面板仅在 `ModelLocks>0` 时低频取明细）。
5. **冷盘/Defender 扫描**下 10k auths 小文件读取受 I/O 上限约束（算法已并行，环境因素为主）。
6. **ETag 缓存 TTL 10s**：纯时间流逝状态（如冷却到期）最多滞后 10s；写操作会立即失效。
