# WorkBuddy2API Pro 优化升级建议

> 文档版本：v1.0 · 编写日期：2026-10-24
> 适用代码基线：`workbuddy2api-pro` v2.0.0-pro（上游 `linguo2625469/workbuddy2api-panel@73a7560` + fork 定制，含本次改名 / 删补偿 / 列表分页筛选 / 单号做任务）
> 目标读者：仓库所有者（实际运维 1146 个账号）+ 后续接手维护者
> 性质：**只读分析产出的建议清单，未改动任何代码 / 配置 / README**。文中行号以当前分支为参考；`internal/panel/panel.go`、`app.js`、`index.html` 在本次版本中仍在改动，行号可能漂移，**以函数名/路由名为准**。

> **实施状态（2026-10-24 更新）**：v2.0.1 已采纳 **#2（全部）**、**#4（最小三项）**、**#10（并发闸门 / 启动首刷 / 单飞 / balance_all 异步子项）**、**#11（补跑窗口 + 单飞 / ctx 子项）**；**v2.1.0 已采纳 #1（服务端分页 + 字段裁剪 + ETag + lite overview，账号池数据源已切换）**；#10 / #11 完整版本、#3、#5-#9、#12-#20 仍按路线图排期。本文其余建议状态不变。

---

## 0. TL;DR（一页看完）

**一句话结论：** 2.0.0-pro 已经把「单机自用、几十个号」的上游项目改成了「千人账号池」的运维工具，但性能路径、批量操作、调度器、安全边界四处仍按「个位数账号」的假设在跑。**最优先的不是加功能，而是把这四处假设改成 1146 个账号下安全的形态**；本文 20 条建议按 v2.0.1 热修 → v2.1 运维效能 → v2.2 安全与架构 三档推进。

### 0.1 优先级总表（20 条）

| # | 建议 | 优先级 | 工作量 | 一句话收益 | 建议版本 |
|---|---|---|---|---|---|
| 1 | 总览接口服务端分页 + 字段裁剪 + ETag | **P0** | 中（2-4 天） | 5s 轮询从 689 KB 降到每页 ~10-60 KB | v2.1 |
| 2 | 面板 HTTP 压缩 + 静态资源缓存 + 前端轮询治理 | **P0** | 小（0.5-1 天） | 传输量降一个数量级；页面隐藏时零轮询 | **v2.0.1** |
| 3 | SSE / 增量事件推送替代全量轮询 | P2 | 大（1-2 周） | 推送级实时，彻底告别轮询 | v2.2 |
| 4 | 面板凭据与网关 API key 分离 + 空 key 阻断 + 登录限速 | **P0** | 小（1 天） | 关掉「拿到 API key 即拿到后台」的提权面 | **v2.0.1** |
| 5 | 面板操作审计日志（谁、何时、对哪个号、结果） | P1 | 中（1-2 天） | 误删/误禁用可追溯、可复盘 | v2.1 |
| 6 | 多用户 / 角色 / 会话登录（RBAC） | P2 | 大（3-5 天） | 多人协作与最小权限 | v2.2 |
| 7 | 凭证静态加密 + auths 权限体检 | P2 | 大（3-5 天） | 备份/磁盘泄露不再等于全部账号失守 | v2.2 |
| 8 | 可观测底座：`/metrics` + `/readyz` + 结构化日志 | P1 | 中（2-3 天） | 接 Prometheus/告警；排障不靠翻表格日志 | v2.1 |
| 9 | 任务执行历史持久化（每次批量任务一条账） | P1 | 中（1-2 天） | 「昨晚签到成功几个 / 哪几个失败」一键可查 | v2.1 |
| 10 | 全池批量操作统一 Job Runner（异步 + 进度 + 取消 + 并发闸门） | **P0** | 中（3-5 天） | 误点不再 1146 并发打上游；进度可见、可取消 | v2.1（闸门 v2.0.1） |
| 11 | 签到 1 小时优化 + 调度阻塞丢批修复 | **P0** | 大（4-6 天） | 1h → 10-20 min；不再吞掉 10/22/23 点任务 | v2.1（补跑窗口 v2.0.1） |
| 12 | 账号备注服务端化 + 标签/分组 | P1 | 中（备注 1 天 / 标签 2-3 天） | 1146 个号能命名、筛选、批量操作 | v2.1（备注）/ v2.2（标签） |
| 13 | state.json 可靠性与写放大治理 + 备份恢复 + 数据治理 | P1 | 中（2-3 天） | 损坏可恢复、SSD 写量降一个数量级、磁盘可控 | v2.1 |
| 14 | 优雅停机与滚动升级 | P1 | 小（0.5-1 天） | 重启不断流、任务不脏、升级可预期 | v2.1 |
| 15 | 配置变更安全（备份 / 回滚 / 危险值拦截） | P1 | 小（0.5-1 天） | 改错配置可回滚，不再可能「手滑关鉴权」 | v2.1 |
| 16 | Go 1.22.5 → 1.24 + 依赖清理 + 供应链检查 | P1 | 小（0.5 天） | 工具链一致、漏洞可查、依赖干净 | v2.1 |
| 17 | CI 强化 + 容器运行时加固 | P1 | 中（1-2 天） | PR 门槛（race/vet/扫描）、容器只读、日志不撑爆磁盘 | v2.1 |
| 18 | 前端工程化（模块拆分 + 真实 DOM 测试 + lint） | P2 | 中（3-5 天） | 面板白屏类回归前移到 CI | v2.2 |
| 19 | 事件通知渠道（Webhook / 企业微信 / Telegram / Bark） | P1 | 中（2-3 天） | 无人值守也能知道签到失败、账号阵亡、积分到期 | v2.1 |
| 20 | 移动端收尾（新控件适配、操作收敛） | P2 | 小（1-2 天） | 手机上可完成日常巡检与单号操作 | v2.2 |

### 0.2 P0 清单（立即做）

| # | 为什么是 P0 | 不做的后果 |
|---|---|---|
| 1 | `/panel/api/overview` 每次都把 1146 个账号全量序列化（689 KB / 13 ms），前端 5s 轮询一次（`app.js` `start()` 的 `setInterval(refreshVisible, 5000)`），服务端分页/裁剪/ETag 是唯一能从根上解决的手段 | 每开一个面板页每小时约 484 MB 下行；移动网络/远程隧道下卡顿明显；账号越多越糟 |
| 2 | 689 KB JSON 未压缩、`app.js`/`index.html` 无缓存头、轮询不感知页面可见性——三处都是「半天级」小改动 | 同样的流量白花；后台标签页整夜空转；隧道/流量成本 |
| 4 | 面板与 `/v1` **共用同一个 `api_key`**（`panel.go withAuth` → `p.apiKey()` 与 `server/handler.go withAuth` 同一份配置）：任何持有网关 key 的下游客户端都能调用 `POST /panel/api/accounts/{uid}/remove`、`POST /panel/api/config` 甚至把 key 改成空 | 一次 key 外泄 = 账号池被清空/被接管；且 `api_key` 为空时面板与 API 全部免鉴权 |
| 10 | 全量余额刷新对 1146 个账号**每号起一个 goroutine、无并发上限**（`scheduler.go RunBalanceRefreshNow`，面板 handler 同步等待），启动时还会自动先刷一轮（`StartBalanceRefresh` 启动即 `RunBalanceRefreshNow()`） | 一次「刷新」= 1146 个并发上游请求，极易触发上游风控/429，甚至被 WAF 盯上；HTTP 请求要等几分钟，反代先 504 |
| 11 | 签到串行一轮约 1 小时（`RunCheckinNow` 逐号两次 RPC）；调度器 `runBatch` **阻塞式**等本批跑完再算下一个时点，跨过 10:00/22:00/23:00 时该时点任务当天直接漏跑 | 保活/活跃/夜猫子有概率整天不执行；1146 号下这是「每天在赌」 |

### 0.3 「是否本次采纳」的判定口径

- 本次 v2.0.0-pro 已冻结功能范围（改名 / 删补偿 / 分页筛选 / 单号做任务），**20 条建议全部默认不纳入本次版本**，避免临发布改大改。
- 仅 3 组「半天到一天级、低风险、纯增益」的子项标记为**可选 v2.0.1 补丁**：#2 全量、#4 的最小三项、#10 的并发闸门子项；另有 #11 的补跑窗口子项，若本次补丁窗口还有余量，建议一并带上（理由见 #11：它挡住的是「保活/活跃整天漏跑」）。
- 每条建议正文都标注了「本次采纳」，方便直接勾选。

---

## 一、实测基线与现状画像（1146 账号）

以下数字来自仓库内 `MAINTAINERS.md` 的实测记录、代码静态事实与本次起草时的代码统计，作为每条建议的量化底稿。

### 1.1 实测数字

| 指标 | 实测 / 代码事实 | 出处 |
|---|---|---|
| 账号规模 | 1146 个 auths 文件，容器 CPU 0.04%、内存 16.4 MB | `MAINTAINERS.md` §实测基线 |
| `/panel/api/overview` | **689 KB / 13 ms**，每次全量返回 `Pool.List()`（所有账号全字段） | 实测 + `panel.go overview()` |
| 面板轮询 | **每 5 秒**一次全量 overview（账号池视图），页面隐藏也不停 | `app.js start()` / `refreshVisible()` |
| 签到 | **串行**逐号执行，一轮约 **1 小时**（每号 2 次上游 RPC：daily-checkin + user-resource） | `MAINTAINERS.md` + `scheduler.go RunCheckinNow` |
| 全量余额刷新 | 每号一个 goroutine，**无并发上限**；面板「刷新」同步等待 | `scheduler.go RunBalanceRefreshNow` |
| 重新扫描 auths/ | 1146 文件约 **160 ms**，`scanned=1146, added=0, removed=0` | `MAINTAINERS.md` |
| auths 文件属主 | 52 个文件是 `root:root`，容器内可读不可写 → token 刷新失败隐患 | `MAINTAINERS.md` |
| 面板前端规模 | `app.js` 192.8 KB / 3766 行；`index.html` 84.1 KB / 1399 行；`mobile.js` 12.2 KB | 本次代码统计 |
| 测试规模 | 852 个 Go 测试函数；其中 `internal/panel/frontend_test.go` 1494 行 / 27 个 node harness 测试 | 本次代码统计 |
| 面板鉴权 | 与网关共用 `api_key`；`api_key == ""` = 面板与 `/v1/*` 全放行 | `panel.go withAuth`、`config.go` |
| 面板 API 基建缺口 | 无 gzip、无 ETag、无 `/metrics`、无 `/readyz`、无审计、无限速（grep 为 0） | 本次代码检索 |
| 调度器心跳 | 单 goroutine 主循环 + 阻塞式 `runBatch`，跨时点不补跑 | `scheduler.go Run/runBatch/nextFire` |

> 注：本节为起草时快照（2026-10-24 00:30，HEAD `3bd2f82` + 在途改动）。此后新增 `tabs_test.go`、前端 11 个 node harness 用例并修订少量文档，前端行数与测试数量有轻微漂移，不影响结论与优先级。

### 1.2 痛点 → 建议索引

| 痛点 | 对应建议 |
|---|---|
| 面板卡、流量大、隧道慢 | #1 #2 #3 |
| 网关 key = 后台管理员 | #4 #6 #15 |
| 误操作不可追溯、多人不可分权 | #5 #6 |
| 凭证明文、备份泄露风险 | #7 |
| 不知道任务跑没跑、失败多少 | #8 #9 #19 |
| 批量操作打爆上游、无进度 | #10 #11 |
| 签到 1 小时、任务漏跑 | #10 #11 #19 |
| 1146 个号找不到、记不住 | #12 |
| 状态文件损坏/写放大/磁盘 | #13 #14 #17 |
| 工具链旧、CI 门槛低、容器不够硬 | #16 #17 |
| 前端维护/白屏回归 | #18 |
| 手机上看不清、点不动 | #20 |

---

## 二、本次 v2.0.0-pro 已实现 vs 后续建议（边界声明，避免重复建议）

### 2.1 本次版本已实现（本文不再作为建议）

| 能力 | 说明 | 对本文的意义 |
|---|---|---|
| 更名 pro | Go module `github.com/lei-rr/workbuddy2api-pro`；`appVersion = "2.0.0-pro"`；镜像/容器名/面板标题/`ServiceName` 全量改名；CI tag 断言与产物名带 `-pro`；README/LICENSE 自引用更新，保留上游版权归属（commit `3bd2f82`） | #16 只建议版本号/工具链机制，不重复改名 |
| 彻底删除「官方补偿」 | 前端卡片、`/panel/api/compensation*` 接口、scheduler 补偿执行器、upstream `claim-compensation`/`compensation-status` 调用全部删除；**网关不再调用任何补偿接口**；连登管家保留 `ClaimGift`（礼包），不含补偿 | 本文**不建议也不会建议**重新引入补偿；后续任何「补偿」字样的需求都应先回看本次删除决策 |
| 各列表前端分页 + 筛选 | 账号池（搜索/状态/版本 + 50/100/200/全部）、模型锁池、积分到期、运行日志、请求日志、积分历史、任务中心队列等 | #1 是「把分页从纯前端下沉到服务端」，不是重做分页 UI |
| 账号行「做任务」按钮 | 账号池每行新增「做任务」入口，对接 `POST /panel/api/accounts/{uid}/tasks/run`（同步返回 `results` / `summary`） | #10 讨论的是「长任务 job 化 + 进度」，不重复该按钮 |
| 保留「重新扫描 auths/」 | `POST /panel/api/accounts/rescan`：`auth.LoadDir` + `pool.SyncToDir`，0 文件保护（409）；1146 文件约 160 ms | #12/#13 不重复实现该按钮；只在「自动对账」方向做增强（列为附注） |

### 2.2 已有能力清单（不要当新建议重复提）

账号池三 tab（账号池 / 模型锁池 / 积分到期）、单号积分构成弹窗、账号筛选与分页、任务中心（扫描 / 队列 / 批量自动任务）、运行日志 / 请求日志 / 积分历史、配置热更新、在线 OAuth 添加账号、请求 JSONL 归档（元数据脱敏）、企业版门控、按地区轮询/顺序耗尽调度、手机端适配（≤760px）、明暗主题、`/healthz` 身份标识、`go:embed` 单二进制、Docker 非 root（`USER app`，uid 10001）。

> 如果后续文档/需求里再出现「加分页」「加重新扫描」「加单号做任务」「加补偿」——前三个本次已做，最后一个明确不做。

---

## 三、20 条优化升级建议

> 每条结构：**优先级 · 工作量 · 建议版本 · 本次采纳** → 现状与痛点（代码事实 + 实测） → 建议方案（文件/接口/配置） → 预期收益 → 改动面与风险 → 本次可选子项（如有）。
> 工作量口径：小 ≤1 天；中 1-5 天；大 >5 天（按 1 名熟悉 Go + 原生 JS 的维护者估算）。

---

### #1 总览接口服务端分页 + 字段裁剪 + ETag

> **P0 · 工作量：中（2-4 天）· 建议版本：v2.1 · 本次采纳：✅ v2.1.0 已实现**（`GET /panel/api/accounts` + `overview?lite=1` + ETag/304，前端账号池已切换；详见 CHANGELOG）

**现状与痛点**

- `GET /panel/api/overview` 每次返回 `Pool.List()` 全量账号（`panel.go overview()`，约 255 行），1146 账号实测 **689 KB / 13 ms**；`Status` 结构（`internal/pool/entry.go` 约 71-132 行）每账号还带 `credits_expiry_buckets`、`model_costs`、`rate_limited_models`、`token_usage` 等可变长数组——这些字段只有「积分到期」「账号详情」才用得到，账号池列表页只用到其中十来个标量。
- 前端每 5s 拉一次全量（`app.js start()` → `refreshVisible()` → `loadOverview(true)`），拿到后在浏览器里 `filter` + `slice` 出当前页 100 行（`renderAccounts` 约 451 行）。**分页/筛选是纯前端**，网络与 JSON 解析成本完全没省。
- 无 `ETag` / `If-None-Match`：哪怕数据没变，也每次回 689 KB。
- 换算：689 KB × 12 次/分 × 60 分 ≈ **484 MB/小时/每个打开的面板页**；远程/手机流量下体感明显。
- 附带：`GET /status`（`server/handler.go` 约 235 行）也全量返回同一份 `Pool.List()`，脚本调用同样偏重。

**建议方案**

1. 新增分页接口（不破坏旧接口）：
   - `GET /panel/api/accounts?page=1&page_size=100&q=&state=&realm=&enterprise=&sort=&fields=`
     - `q`：uid / 昵称包含匹配；`state`：`healthy|cooling|disabled|paused`；`realm`：`cn|global`；`sort`：`uid|credits|last_success|cool_remaining`
     - 默认只返回列表投影字段（uid/nickname/realm/enterprise/credits/credits_total/cooling/cool_kind/cool_remaining_sec/until/disabled/disabled_reason/paused/checkin_done/soft_streak/in_flight/last_success/success_count/err_total）；`fields=full` 才带 `model_costs` 等重字段。
     - 响应：`{ "now": <unix>, "total": N, "filtered": M, "page": 1, "page_size": 100, "rows": [...] }`。
   - `GET /panel/api/overview?lite=1`：只返回汇总计数 + `model_locks` + `balance_refresh_sec` 等元信息，**不带 `accounts`**（默认不带 `lite` 时保持旧全量语义，脚本零破坏）。
2. ETag/304 与「冻结倒计时」：
   - 在 `pool` 内加一个变更纪元计数（`statusEpoch`，账号增删/状态变更/积分写入时 +1），`/accounts` 的 ETag = hash(epoch + 查询参数 + 排序口径)。
   - **关键前置**：把 `cool_remaining_sec` 这种「秒级变化字段」从 ETag 输入中排除，前端改为用 `until` 时间戳本地倒计时（后端同时回 `now` 校正时钟）。否则 304 会让倒计时冻结。
3. 前端：
   - 5s 轮询只拉 `overview?lite=1`（约 1-2 KB）；
   - 账号页 10s 或「summary 合计变化」时拉当前页 `/accounts`，筛选/翻页/排序全部走服务端参数；
   - 保留「全部（不分页）」作为显式选项，走 `page_size=0`（服务端返回上限 1000 并提示）。
4. 兼容旧脚本：`overview`、`/status` 的旧字段与默认行为不变，只新增接口与 `lite` 参数；README 面板接口表补一行。

**预期收益**

- 单次下拉 689 KB → 每页约 60 KB（100 行 × 投影字段），page_size=100；配合 #2 的 gzip 后约 8-15 KB（估算，需实测）；轮询流量下降 1-2 个数量级。
- 服务端可只序列化当前页（`O(page_size)` 而非 `O(1146)`），`/panel/api/overview?lite=1` 的 marshal 成本从 13 ms 降到 <1 ms。
- 顺带解决「全部」模式下浏览器一次持有 1146 个对象、每次筛选 O(n) 重渲染的问题。

**改动面与风险**

- 改动：`internal/panel/panel.go`（新 handler + 路由）、`internal/pool`（分页/筛选/状态投影与 epoch，避免在 panel 里重复实现 + 加锁遍历逻辑）、`app.js`（`loadOverview`/`renderAccounts`/筛选状态）、`index.html`（分页控件与服务端参数绑定）、`frontend_test.go`。
- 风险：① 两个数据源（summary 与 page）可能短暂不一致——用「同一次响应里带 `now` + epoch，前端按 epoch 丢弃过期页」可解；② 排序与筛选口径要写进测试（尤其 paused/cooling 与 now 相关字段）；③ 旧脚本依赖全量字段，默认行为必须原样保留。
- 已有 `CountsDetailedWithPaused()` 可直接复用做 summary，注意别在持锁时做排序/JSON。

**本次可选子项**：无（改动面偏大，放 v2.1 做）。

---

### #2 面板 HTTP 压缩 + 静态资源缓存 + 前端轮询治理

> **P0 · 工作量：小（0.5-1 天）· 建议版本：v2.0.1 · 本次采纳：否（v2.0.0-pro 不纳入；建议作为 v2.0.1 补丁优先纳入）**

**现状与痛点**

- `panel.writeJSON`（`panel.go` 约 1087 行）直接 `json.Marshal` + `w.Write`，**不压缩**；689 KB JSON 未压缩，且静态资源 `app.js`（193 KB）、`index.html`（84 KB）、`mobile.*` 也无缓存头（`index.go` 手动 `Write`，无 `ETag`/`Cache-Control`）。
- 轮询三重浪费：① `setInterval(refreshVisible, 5000)` 无条件跑，`document.visibilityState === 'hidden'` 时也跑；② 失败没有退避（断网/401 后仍每 5s 重试）；③ 每次账号操作成功后 `finally { loadOverview(true) }` 与下一次轮询重复。
- 401 时 `api()` 每次轮询都会 `openKey()`（`app.js` 约 52 行），后台标签页会反复抢焦点/弹密钥框。
- 静态资源每次刷新重新下载（无 304），面板首次打开约 280 KB 未压缩。

**建议方案**

1. 压缩中间件（只包面板，不碰 `/v1` SSE）：
   - 在 `panel.ServeHTTP` 里按 `Accept-Encoding` 对 `application/json`、`text/html`、`text/javascript`、`text/css` 且响应 >1 KB 的输出做 gzip（标准库 `compress/gzip` + `sync.Pool`）；设置 `Vary: Accept-Encoding`。
   - 流式/健康检查/已编码响应跳过；`/panel/api/*` 全部受益（overview、request_logs、usage 都是大 JSON）。
2. 静态资源缓存（`internal/panel/index.go`）：
   - 启动时对 `app.js`/`mobile.js`/`mobile.css` 算 SHA-256，输出 `ETag` + `Cache-Control: public, max-age=0, must-revalidate`（或 `max-age=3600` + ETag 兜底）；`If-None-Match` 命中回 304。
   - `index.html` 用弱 ETag + `Cache-Control: no-cache`（保证改版即刷新）。
3. 轮询治理（`app.js`）：
   - `visibilitychange`：隐藏时暂停计时器，恢复时立即刷一次并重启；
   - 失败退避：5s → 10s → 30s（上限 60s），成功恢复 5s；401 时不自动重试、把密钥框只弹一次；
   - 间隔可配：`GET /panel/api/overview?lite=1` 里返回 `poll_interval_sec`（后端配置 `panel.poll_interval_sec`，默认 5），前端按它跑；
   - 多标签页防惊群：±20% 抖动。
4. 顺手：`X-Robots-Tag: noindex`（`setSecurityHeaders`），防止面板被搜索引擎收录。

**预期收益**

- overview 传输 689 KB → 约 40-70 KB（gzip 对高重复 JSON 通常 10-15×；估算，实测为准）；静态资源首屏 280 KB → 约 60-90 KB。
- 页面隐藏/锁屏后流量归零（运维常见场景：面板常开一整天）。
- 失败退避让弱网/重启窗口不再刷屏。

**改动面与风险**

- 改动：`internal/panel/index.go`、`internal/panel/panel.go`（ServeHTTP 压缩包装）、`app.js`、`index.html`（script 标签可不变，缓存靠头）、`frontend_test.go`（新增可见性/退避的纯函数测试）。
- 风险：① 不要在 SSE/流式响应上包 gzip（只包面板）；② 压缩后要 `w.Header().Del("Content-Length")` 或改用 `http.ResponseWriter` 包装，避免长度不符；③ 缓存头写错会导致改版后面板不更新——`index.html` 必须 no-cache。
- 测试：Go 里用 `httptest` + `Accept-Encoding: gzip` 断言 `Content-Encoding`、解压后 JSON 等价、304 分支。

**本次可选子项（推荐）**：全部。这是「一个下午能做完、风险极低、收益立刻可见」的改动，最适合作为 v2.0.1 补丁。

---

### #3 SSE / 增量事件推送替代全量轮询

> **P2 · 工作量：大（1-2 周）· 建议版本：v2.2 · 本次采纳：否**

**现状与痛点**

- 现状是「客户端定时拉全量」的拉模型（`app.js` 5s 轮询），没有服务端推送；任务执行（签到/队列）的真实进度只能靠日志或刷新后看计数变化。
- 只做 #1 + #2 后，轮询成本已可接受；但「实时性」仍受 5s 限制，长任务（#10 的 Job Runner）想要顺滑进度条，轮询需要 ≤1s，成本又上去了。
- 浏览器原生 `EventSource` 不能带 `Authorization` 头，直接上 SSE 会引入「token 拼 URL」的安全退化——这是本条的隐藏成本，必须先设计好鉴权。

**建议方案**

1. 服务端事件源：`GET /panel/api/events`（`text/event-stream`），事件类型：
   - `summary`（计数变化，节流 1/s）、`account`（单号状态增量，带 uid + 变更字段）、`model_lock`、`job`（Job Runner 进度）、`log`（可选，复用 ring）。
2. 推送触发：在 `pool` 加最小订阅者机制（`Subscribe(func(Event))`，内部 buffered channel + 丢弃旧事件），面板 `Panel` 订阅后广播；**不要**在热路径同步推送。
3. 鉴权两种做法（二选一）：
   - **A（推荐）**：`POST /panel/api/events/ticket`（Bearer）返回 60 秒一次性 ticket，`EventSource` 用 `?ticket=`；服务端校验后立即失效；CSP 已限同源，风险可控。
   - B：前端用 `fetch()` + `ReadableStream` 手工解析 SSE（可带 Authorization 头），代价是前端多一层解析代码。
4. 代理注意：Nginx 需 `proxy_buffering off; proxy_read_timeout 长于心跳`；服务端 15s 发一次 `: ping`；客户端断线指数退避重连。
5. 降级：SSE 连不上时自动回落到 #1 的分页轮询（不要出现「SSE 挂了面板空白」）。
6. 若不想引入 SSE 复杂度，可先做**长轮询**版本：`GET /panel/api/overview?lite=1&since=<epoch>&wait=25`，有变化立即返回、无变化 25s 后回 204——改动比 SSE 小，效果接近。

**预期收益**

- 任务进度、账号状态、模型锁变化秒级可见；轮询请求数从「每页每分钟 12 次」降到「空闲时 0 次」。
- 为 #10 的 Job 进度、#9 的任务历史实时化提供通道。

**改动面与风险**

- 改动：`internal/pool`（订阅机制，注意锁与 goroutine 泄漏）、`internal/panel`（事件 hub + handler）、`app.js`（EventSource/断线重连/降级）、Nginx 文档。
- 风险：① 连接泄漏（每个浏览器标签一条长连接，面板常开）；② 反向代理缓冲/超时；③ ticket 泄露窗口；④ 事件风暴（1146 账号批量任务时逐号推送会打爆浏览器）——必须合并/节流为「每 500ms 一批」。
- 建议先做「长轮询」验证价值，再决定是否上 SSE。

**本次可选子项**：无。

---

### #4 面板凭据与网关 API key 分离 + 空 key 阻断 + 登录限速

> **P0 · 工作量：小（1 天）· 建议版本：v2.0.1 · 本次采纳：否（v2.0.0-pro 不纳入；建议作为 v2.0.1 补丁优先纳入）**

**现状与痛点**

- **同一把 key 管一切**：`panel.go withAuth`（约 211 行）用 `httpauth.VerifyBearer(r, p.apiKey())`，`server/handler.go withAuth`（约 201 行）用 `cfg.APIKey` ——两者都是 `config.api_key`。也就是说：**任何拿到网关 API key 的下游客户端（或不小心贴出去的 key），都能直接调面板管理接口**：`POST /panel/api/accounts/{uid}/remove`（删号+删凭证文件）、`/disable`、`/config`（改 `api_key` 甚至清空、改 `auth_dir`）、`/checkin_all`（全池打上游）。
- `api_key` 为空 = **面板与 `/v1/*` 全部免鉴权**（README 明确写「空 = 不鉴权直接放行」）；首次运行会自动生成随机 key（`cmd/server/config.go WriteDefault`，`sk-` + 18 字节随机，强度足够），但老配置/手改配置可能为空。
- 默认 `listen: ":7863"` 绑定所有网卡，`docker-compose.yml` 发布 `7863:7863`（全网卡），无内置 TLS；`POST /panel/api/config` 可以在运行期把 key 改成空——**一次提权后无法自愈**。
- 面板登录接口无失败限速/锁定：`withAuth` 只是常量时间比较，暴力尝试没有成本（key 熵高时实际不可爆破，但空 key 配置下等于没有门）。
- 面板 HTML 可匿名加载（无秘密，但暴露「这里有面板」）；`/healthz` 无鉴权且返回账号总数/健康数（信息量低，可接受）。

**建议方案（最小三项，均可独立上线）**

1. **面板 key 与网关 key 分离**：
   - 新增配置：`panel.api_key`（环境变量 `WB2A_PANEL_KEY`）；为空时**回落** `api_key`（向后兼容，启动日志提示「面板与网关共用密钥，建议分离」）。
   - `panel.apiKey()` 优先读 `panel.api_key`；`server` 侧 `/v1/*` 只认 `api_key`，不认 panel key。
   - 面板前端登录框逻辑不变（输入的是面板 key）；网关客户端用原 key。
2. **空 key 非回环阻断**：
   - 启动校验：`listen` 的 host 不是 `127.0.0.1/::1/localhost` 且 `api_key == ""` 且 `panel.api_key == ""` → `log.Fatalf`，除非显式配置 `security.allow_anonymous: true`（默认 false），错误信息里给出生成 key 的方法。
   - 面板保存配置时**服务端拒绝**把 `api_key` 写成空（除非 `security.allow_anonymous`），避免运行期自废鉴权。
3. **登录失败限速 + 审计前置**：
   - 在 `panel.ServeHTTP` 加一个按「来源 IP（X-Forwarded-For 首段，可信代理可配）」的 token bucket：鉴权失败 20 次/分钟 → 429 + `Retry-After`；成功一次清桶；内存 LRU 上限（如 1 万 IP）。
   - 失败记录交给 #5 的审计通道（后续一起做）。
4. 可选但建议：`setSecurityHeaders` 增加 `X-Robots-Tag: noindex`；启动日志打印「面板监听地址 + 是否启用鉴权 + 是否 HTTPS」三行，提醒公网部署加反代。

**预期收益**

- 彻底切断「API 使用者 = 后台管理员」的信任越权；key 分发给下游时不再附带删号权限。
- 空 key 导致的「裸奔面板」从「可能发生」变成「启动直接失败」。
- 为 #6 的 RBAC 打地基（先分 key，后分角色）。

**改动面与风险**

- 改动：`cmd/server/config.go`（Config 字段 + env + 校验）、`internal/panel/panel.go/panel.go`（`apiKey()`、限速中间件）、`internal/server`（无需改，保持只认 `api_key`）、`config.example.json`、README/面板配置页（加 panel key 字段）、`app.js` 无需改。
- 风险：① 用户升级后如果只配了 `api_key`，行为不变（回落逻辑）；② 分离后前端要输面板 key，旧浏览器 localStorage 里存的是网关 key → 会 401，需要前端提示「面板密钥已变更」；③ 限速状态在内存，重启清零（可接受）；④ XFF 可伪造，限速键在无反代时应回落 TCP 对端（代码里已有 `clientIP` 逻辑可参考 `server/logging.go`）。
- 测试：`internal/panel/security_test.go` 已有鉴权用例，补「panel key 生效/回落」「空 key 拒绝启动」「429 限速」三组。

**本次可选子项（推荐）**：第 1、2 项（半天），#3 视时间。

---

### #5 面板操作审计日志

> **P1 · 工作量：中（1-2 天）· 建议版本：v2.1 · 本次采纳：否**

**现状与痛点**

- 所有写操作只有零散 `log.Printf("panel: disable uid=...")`（`panel.go accountDisable/Revive/Remove 等`），写入 500 条环形缓冲（`ring.go`）+ docker stdout。**没有操作者身份、来源 IP、结果状态、耗时**，也不落盘。
- 环形缓冲 500 条在 1146 号场景很快被任务日志冲掉：一次签到就是 1000+ 行 `checkin xxx: ...`，一场批量任务跑完，之前的「谁把某个号删了」已经查不到。
- `docker logs` 会随容器重建丢失（compose 没配日志轮转，见 #17），事后复盘只能靠猜。
- 本次删除了补偿接口，减少了一部分写接口；但账号删除/禁用/改配置仍是不可逆操作（删号会同时删 `auths/` 文件）。

**建议方案**

1. 新增 `internal/panelaudit`（或放 `panel` 包内）：
   - 中间件只挂「非 GET」类 `/panel/api/*`：记录 `{ts, request_id, actor, action, target_uid, params_summary, status, ok, duration_ms, client_ip, user_agent}`。
   - `actor` 目前 = 面板 key 指纹（`sha256(panel_key)` 前 8 位，**绝不落 key 明文**）；#6 落地后可换成用户名。
   - `params_summary` 白名单化：账号操作记 uid；`config` 操作只记「哪些键变了」（diff key 列表，不含值）；`remove` 记「是否删了文件」。
   - 落盘 `data/panel-audit.jsonl`（每日滚动、保留 90 天、总容量上限 20 MB，复用 `reqlog` 的 rotate/prune 模式）；同时写一份进面板 ring（live 可见）。
2. 查询接口：`GET /panel/api/audit?limit=200&action=&uid=&from=&to=`（Bearer）。
3. 面板 UI：运行日志页新增第四个频道「运维」，表格列：时间 / 操作 / 对象 / 结果 / 来源 IP；可用现有频道 chips 组件。
4. 覆盖范围：账号 revive/disable/pause/resume/remove、单号 checkin/balance、批量 `*_all`、`tasks/run`、`rescan`、配置保存、OAuth 登录完成、登录失败（来自 #4）。
5. 与 #9 的分工：#9 记「任务批次结果」，#5 记「人的操作」；两者都不记凭证与对话内容。

**预期收益**

- 「谁在什么时候把 300 个号删了 / 把 key 改了 / 触发了全量签到」可回答；误操作可定位到时间点与对象。
- 出现异常消耗/账号被禁用时，能对照操作时间线排除人为因素。
- 满足后续多人协作（#6）的最低合规要求。

**改动面与风险**

- 改动：`internal/panel`（中间件 + handler + ring 频道）、`index.html`/`app.js`（新频道 UI）、`cmd/server/main.go`（审计文件路径随 `state_file` 目录）、README 接口表。
- 风险：① 写盘量小（只有写操作，1146 号批量任务一次也只有 1 条），但需异步写避免阻塞 handler（复用 reqlog 的队列模式）；② IP 属于个人信息，沿用 `logging.request_client_info` 开关决定是否采集；③ `params_summary` 必须白名单，避免把 api_key/webhook token 写进去。
- 测试：httptest 发写操作 → 断言审计文件多一行、字段齐全、GET 不记。

**本次可选子项**：无（依赖 #4 的 actor 定义，放 v2.1 一起）。

---

### #6 多用户 / 角色 / 会话登录（RBAC）

> **P2 · 工作量：大（3-5 天）· 建议版本：v2.2 · 本次采纳：否**

**现状与痛点**

- 面板只有「一个 key 全权限」：无用户概念、无角色、无会话、无操作归属（`panel.go withAuth`）。多人（自己 + 家人/同事 + 只读观察者）共用时无法分权；API 客户端与管理员也无法隔离。
- key 存在浏览器 `localStorage`（`app.js LS_KEY`），无会话过期、无退出登录（只能清缓存）、无 CSRF 概念（Bearer 天然免疫，但改 cookie 后必须补）。
- 移动端/多设备时常需要重新输入 key，体验一般（顺带说，这是 #20 的一部分）。

**建议方案**

1. 配置 `panel.users: [{name, role, password_hash}]`（argon2id 或 bcrypt；首次启动可用 CLI/环境变量生成，避免明文写在 config）：
   - `viewer`：只读（GET 类接口）；
   - `operator`：账号运维 + 任务执行（不含删号/移除、不含配置）；
   - `admin`：全部 + 用户管理 + 配置 + key 轮换。
2. 登录流程：`POST /panel/api/session/login {user, password}` → `Set-Cookie: wb2_session=<opaque>; HttpOnly; SameSite=Strict; Secure(当 TLS 时)`；服务端内存会话（可选 HMAC 无状态 token），滑动过期 12h；`POST /panel/api/session/logout`；失败限速复用 #4。
3. CSRF：cookie 模式下所有非 GET 请求校验 `X-CSRF-Token`（登录时下发，与 session 绑定）。
4. 路由级权限表（集中在一个 map，router 注册时声明所需角色），`withAuth` 升级为 `withRole("operator")`。
5. 兼容：保留 `api_key` 作为「root/脚本」通道（可配置 `panel.legacy_api_key: true` 默认开），脚本与 `curl` 不受影响；面板前端优先走会话，未登录时显示登录页（现在是密钥框）。
6. 与 #4 的关系：#4 先解决「面板 key ≠ 网关 key」；#6 再把「一把面板 key」变成「多用户+角色」。

**预期收益**

- 多人运维下的最小权限；误操作面降低；操作可归属（配合 #5 `actor` 字段变成用户名）。
- 可把只读大屏/监控账号与管理员分开。

**改动面与风险**

- 改动：`internal/panel`（session store、角色路由、登录页/前端）、`cmd/server/config.go`（users 配置与校验）、`index.html`/`app.js`（登录页替换密钥框）、README。
- 风险：① 密码哈希/会话固定/CSRF 任一处理不当反而降低安全性；② 忘记密码需要恢复通道（保留 root api_key）；③ 面板登录态与 `localStorage` 旧 key 的兼容迁移需设计；④ 工作量集中在前端，注意与 #18 的前端模块化排期协同。
- 建议：不在 v2.1 做——单用户场景收益有限，先把 #4/#5 做扎实。

**本次可选子项**：无。

---

### #7 凭证静态加密 + auths 权限体检

> **P2 · 工作量：大（3-5 天）· 建议版本：v2.2 · 本次采纳：否**

**现状与痛点**

- `auths/workbuddy-<uid>.json` 里是**明文** `accessToken` / `refreshToken`（`internal/auth/auth.go Parse`，约 281 行；`SaveAtomic` 以 `0600` 原子写回，约 357-402 行）。README《安全与合规》已明确提示「明文凭证，切勿泄露」。
- 1146 个账号意味着磁盘上就是 1146 组可用的完整凭证：任何一次 `data/` / `auths/` 备份被拷走、任何一次宿主机账号被登录、任何一次误打包上传，都等于全部账号交接。
- 已知的现实问题：本机部署里 52 个 auth 文件属主是 `root:root`，容器（uid 10001）可读不可写 → 某天 token 刷新会失败（`MAINTAINERS.md` 已记录）。当前**没有任何启动自检**能提前发现这类权限问题。
- 外部脚本（`scripts/probe_active.py`、`scripts/task_*.py`）直接 `json.load` 读取 `accessToken` 明文——这决定了「加密」不能只改 Go 侧，必须考虑脚本兼容（这也是本条放 P2 的原因之一）。

**建议方案（分两阶段，先做低风险的 a）**

1. **a) 权限体检（小，建议 v2.1 先做，可独立上线）**：
   - 启动时扫描 `auth_dir`：`mode & 0o077 != 0`（他人可读写）、属主 != 当前容器用户、且 `unix.Access(W_OK)` 失败 → 汇总 WARN 日志（明确列出文件名与原因，参考现有 `auth.LoadDir` 遍历）。
   - 面板新增 `GET /panel/api/health/auths` 返回 `{total, bad_mode: N, not_writable: N, items:[...]}`；账号池页顶部出现一条可折叠警示条（「52 个凭证文件权限/属主异常，点此查看修复命令」）。
   - 提供修复脚本入口（文档 + 容器内 `chmod/chown` 命令），**不自动改权限**（避免误伤）。
2. **b) 静态加密（大，v2.2）**：
   - 主密钥来源：环境变量 `WB2A_MASTER_KEY`（32 字节 base64）优先，其次 `auths/.master.key`（0600）自动生成（首次启动打印指纹，不打印密钥）。
   - 字段级 AES-256-GCM：`accessToken`/`refreshToken`/`device_token` 加密后写 `enc:v1:<base64(nonce+ciphertext)>`，文件结构与文件名不变；`Parse` 遇到 `enc:` 前缀解密，遇到旧明文照常读（自动兼容），`SaveAtomic` 一律写加密。
   - 迁移命令：`wb2api -encrypt-auths -config config.json`（逐文件重写、失败回滚、`--dry-run`）。
   - **脚本兼容**：给 Python 脚本提供两选一：① 读 `WB2A_MASTER_KEY` 用 `cryptography` 解密（需加依赖）；② 新增 `cmd/export-auth`（Go）输出单账号明文 JSON 到 stdout 供脚本管道使用（推荐，脚本改一行 `subprocess`）。
   - 丢失主密钥 = 全部账号不可恢复：文档强制「主密钥单独备份、与 auths 分开存放」，`data/` 备份默认加密。

**预期收益**

- 「备份泄露 / 主机被非 root 用户读取 / 误上传仓库」三种高频事故从「全部账号失守」降为「需要主密钥」。
- 权限体检单独就能消除「52 个文件某天写失败」的静默故障。

**改动面与风险**

- 改动：`internal/auth`（Parse/SaveAtomic/加解密）、`cmd/server/main.go`（主密钥加载、启动体检）、`cmd/` 新增迁移与导出工具、`internal/panel`（体检接口/警示条）、`scripts/*.py`、README 安全章节。
- 风险：① 主密钥管理复杂化，**忘备份 = 灾备事故**；② 密钥轮换需要重加密全目录；③ 脚本兼容是主要工作量；④ Windows 下属主检查语义不同（`os.FileInfo.Sys()` 类型断言要加 build tag）。
- 建议：a 阶段先上；b 阶段等 #13 的备份方案稳定后再做（备份本身也要考虑是否加密）。
- 替代方案（如果不想引入加密）：OS 全盘加密 + `auths/` 单独 0700 目录 + 备份使用 `age`/`7z -p` 加密 + README 明确「备份即凭证」。成本低得多，可作为 b 的「不做」选项。

**本次可选子项**：a 阶段的「启动权限体检 + 面板警示」可考虑 v2.1 先做（约半天）。

---

### #8 可观测底座：`/metrics` + `/readyz` + 结构化日志

> **P1 · 工作量：中（2-3 天）· 建议版本：v2.1 · 本次采纳：否**

**现状与痛点**

- **没有 `/metrics`**：全仓库 grep `prometheus|/metrics|pprof` 为 0；只有 `/healthz`（`server/handler.go` 约 211-233 行，返回 healthy/total/realm_servable）和面板里的内存指标（`reqlog.Recorder.Snapshot`）。
- **没有 `/readyz`**：`/healthz` 用 `ServableNow()` 判活（无可用账号返回 503）——这是「业务可用性」，不是「进程就绪性」。启动期 `auth.LoadDir` 还没完成、state 还没恢复时，容器探活与业务探活混在一起；也没有「state 可写 / auth_dir 可写 / 调度器存活」检查（而这三项才是 1146 号部署最容易坏的）。
- **日志是给人看的文本**：调度日志 `checkin uid8: ...`、chat 表格行、`panel:` 前缀行，级别只有靠 `WARN:` 字符串（`internal/logfmt`）；没有 JSON 输出、没有统一 `request_id` 关联（chat 有 `X-Request-Id`，任务日志没有）、没有日志采样。
- 容器只保留 `docker logs`（无轮转，见 #17），日志一多就丢；无法接 Loki/ELK/云日志。
- 1146 号每天的日志量：签到 2×1146 行 + 各任务 + chat 流水，一次签到峰值就是数千行，500 条面板 ring 瞬间被冲掉。

**建议方案**

1. 新增 `internal/metrics`（零第三方依赖，原子计数 + 固定标签）：
   - 网关：`requests_total{path,outcome}`、`request_duration_seconds`（桶）、`tokens_total{kind}`、`credits_total`、`inflight_requests`；
   - 池：`pool_accounts{state}`（healthy/cooling/disabled/paused）、`pool_pick_failures_total`、`pool_breaker_open`；
   - 调度：`task_runs_total{task,result}`、`task_duration_seconds{task}`、`task_last_success_timestamp{task}`、`task_accounts_done_total{task}`；
   - 面板：`panel_api_requests_total{endpoint}`、`panel_response_bytes_total`、`jobs_running`。
   - 暴露：`GET /metrics`（Prometheus 文本格式，手写序列化即可，不必引 prometheus/client_golang——保持零依赖），配置 `metrics.enabled` + `metrics.listen`（**建议独立端口 `127.0.0.1:7864`**，不暴露公网）+ 可选 `metrics.token`。
2. 新增 `GET /readyz`：检查 ① 至少加载到账号（或显式允许空池）、② `auth_dir` 可读、③ `data/` 可写（写 `.ready` 探测文件）、④ 调度器/余额循环心跳（每类任务记 `last_run` 时间戳，超过 2× 周期未更新即 unhealthy）、⑤ state 最近写入成功；返回 `{ready, checks:{...}}`，不健康 503。
3. 结构化日志（`cmd/server` + `internal/logfmt` 升级）：
   - 配置 `logging.format: "text" | "json"`（默认 text，保持现有测试与 README 契约），JSON 行形如 `{"ts","level","mod","msg","uid8","task","attempt","err"}`；
   - `log.SetOutput` 继续镜像到面板 ring（ring 里存原始 text），JSON 只影响 stdout；
   - 任务类日志带 `run_id`（配合 #9），可用 `journalctl`/Loki 按 run_id 聚合；
   - 级别统一：INFO 默认、WARN/ERR 显式；`logfmt` 已有约定，补一个 `logfmt.Level` helper 即可。
4. 顺手：`/healthz` 保持现状（负载均衡语义不变），README 区分 healthz/readyz。

**预期收益**

- 接 Prometheus/Grafana 后，可做「签到成功率 / 任务耗时 / 冷却账号数 / 429 率」面板与告警；不接也能用 `/readyz` 让编排器判断「真的可以接流量」。
- JSON 日志让 1146 号场景的排障从「grep 文本」变成「按字段过滤」；`run_id` 把一次任务的 1146 行串起来。
- 为 #9/#19 提供数据源（任务历史与通知直接复用 metrics 的计数）。

**改动面与风险**

- 改动：新增 `internal/metrics`、`internal/panel` 或 `internal/server` 挂 `/metrics`、`cmd/server/main.go`（装配、独立 listener）、`cmd/server/config.go`（metrics/日志格式配置）、`internal/logfmt`、README。
- 风险：① 标签基数（**不要**把 uid/model 当标签，用汇总或 Top-N）；② 独立端口要写防火墙说明，别把 7864 暴露公网；③ 日志格式切换要保证不破坏依赖文本格式的测试与脚本（默认 text，json 仅 opt-in）；④ 手写 Prometheus 格式注意转义。
- 测试：`internal/metrics` 纯单元测试 + `/metrics` 输出格式快照；`/readyz` 用 fake 检查器。

**本次可选子项**：`/readyz` + 「auth_dir/data 可写」两项可先做（小，v2.1 首批），其余随 #9/#10 一起。

---

### #9 任务执行历史持久化

> **P1 · 工作量：中（1-2 天）· 建议版本：v2.1 · 本次采纳：否**

**现状与痛点**

- 定时任务（签到/旅行/活跃/保活/夜猫子/连登管家/成长队列/余额刷新）**只打日志，不落结构化结果**：`scheduler.go RunCheckinNow` 逐号 `log.Printf`，失败行也只有一句错误；没有「本轮开始/结束、成功数/失败数/耗时」的汇总。
- 面板上的手动批量按钮只回 `{ok:true, started:true}`（`panel.go checkinAll/travelAll/...`），前端 toast「结果见日志」；而日志在 500 条 ring + docker stdout，一次 1146 号签到就能把 ring 冲满，`docker logs` 重建即丢。
- 因此「昨晚 21 点签到成功了多少个？哪 37 个失败了？为什么？」目前**无法回答**，只能人工翻日志；而这恰恰是 1146 号运维每天最需要的信息。
- `taskcenter` 的队列进度只存在内存 `queueState`（`internal/panel/taskcenter.go` 约 135-142 行），进程重启即丢，也没有历史。

**建议方案**

1. 新增 `internal/taskhist`（JSONL，复用 `reqlog` 的 rotate/prune 思路）：
   - 文件 `data/task-runs.jsonl`，一次任务批次一行：`{run_id, task, trigger(cron|manual|queue), started_at, finished_at, duration_ms, total, ok, fail, skip, aborted, error_samples:[{uid8, err} 最多 20 条], host}`。
   - 同时按账号记录失败明细（可选文件 `data/task-failures.jsonl`，只记失败，保留 30 天，容量上限 10 MB）。
   - 保留 30 天、总上限 20 MB；写失败不影响任务执行（异步队列 + 丢弃计数）。
2. 埋点位置：`RunCheckinNow`、`RunTravelNow`、`runActivity`、`RunKeepaliveNow`、`RunBlackcatNow`、`RunStreakBonusNow`、`RunBalanceRefreshNow`、`RunGrowthQueueOnce`、面板单号操作（checkin/balance/tasks/run）。
3. 接口：
   - `GET /panel/api/task_runs?limit=20&task=&from=&to=`（倒序）；
   - `GET /panel/api/task_runs/{run_id}`（含失败样本）；
   - 复用 `run_id` 贯穿日志（#8），点击历史行可跳到该时间段的日志。
4. 面板 UI：运行日志页或任务中心新增「执行历史」表：任务 / 触发方式 / 开始时间 / 耗时 / 成功·失败·跳过 / 状态，失败行标红并可展开；在 1146 号场景这比翻日志有用得多。
5. 与 #19 联动：失败数 >0 或耗时异常时触发通知渠道。

**预期收益**

- 每天的任务可对账：「签到成功 1109 / 失败 37」一目了然，失败样本直接给出原因（429/12153/网络）。
- 进程重启/升级后仍能看历史，不再依赖 docker logs。
- 为 #10 的 Job 进度、#19 的通知提供统一数据模型。

**改动面与风险**

- 改动：新增 `internal/taskhist`、`internal/scheduler/*`（各 Run* 函数加统计与 defer 收尾）、`internal/panel`（handler/UI）、`cmd/server/main.go`（初始化与路径推导，沿用 `stateSibling`）。
- 风险：① 统计代码要并发安全（各任务函数内部是串行的，但同一任务可能被手动与 cron 并发触发——先由 #10/#11 的单飞兜底，否则统计会互相覆盖）；② 失败明细不要记凭证/响应正文；③ 写盘量与 rotate（每次任务一行，可忽略）。
- 测试：fake upstream 跑 3 个账号 → 断言 JSONL 行数/计数/失败样本；rotate/prune 单测。

**本次可选子项**：无。

---

### #10 全池批量操作统一 Job Runner（异步 + 进度 + 取消 + 并发闸门）

> **P0 · 工作量：中（3-5 天）· 建议版本：v2.1 · 本次采纳：否（仅「并发闸门」子项建议 v2.0.1）**

**现状与痛点**

- **`balance_all` 是同步阻塞 + 无并发上限**：`panel.go balanceAll`（约 809 行）直接调 `scheduler.RunBalanceRefreshNow()`，而后者对每个账号 `go func(){...}()`（`scheduler.go` 约 653-686 行）——1146 个账号 = **1146 个并发上游请求**，HTTP handler 要等全部完成后再 `syncNicknames()`（另有 3 并发）。`MAINTAINERS.md` 已实测记录「对全池无并发上限地打上游（1146 个并发请求）」。面板代码注释里还写着「账号量小（个位数），同步等待」——这个假设早已不成立。
- **启动即触发一次全量余额刷新**：`StartBalanceRefresh` 启动时先 `RunBalanceRefreshNow()`（`scheduler.go` 约 697-701 行），默认 `balance_refresh_enabled: true`。也就是说，只要用户没手动关，**每次重启/升级后立即对全池发起一次无上限并发扫描**。当前部署把该开关关了，但这是「配置绕过问题」，不是修复。
- **手动批量任务没有单飞/进度/取消**：`checkin_all`/`travel_all`/`activity_all`/`keepalive_all` 都是 `go p.cfg.Scheduler.RunXxxNow()`，返回 `{ok:true,started:true}` 就结束。双击按钮、或定时任务与手动同时触发，就会**两轮全池任务并发打上游**（scheduler 的 Run* 函数没有互斥）。
- **单号「做任务」同步等待**：本次新增的 `POST /panel/api/accounts/{uid}/tasks/run` 按 task-2 契约为同步返回（一个号 1-2 分钟），HTTP 连接一直挂着；反代 60s 超时会 504，前端只能看到失败。
- 已有 `taskcenter` 队列（`internal/panel/taskcenter.go`）有进度条与并发控制（1-4），但只服务成长任务，不能复用到签到/余额；且状态在内存，重启丢失。

**建议方案**

1. 新增 `internal/jobs`（或 `internal/panel/job.go`）统一 Job 模型：
   ```
   type Job struct {
     ID, Kind, Trigger string       // kind: checkin|balance|travel|activity|keepalive|blackcat|growth|tasks_run
     State string                   // pending|running|done|failed|canceled
     Total, Done, Fail, Skip int
     StartedAt, FinishedAt time.Time
     Error string
     LogTail []string               // 最近 50 行，供面板展示
   }
   ```
   - 进程内注册表 + `data/jobs.json` 持久化最近 50 条（重启后 running → interrupted，供面板显示「上次中断」）；
   - **同 kind 单飞**：已有 running 的同类任务返回 409（沿用 taskcenter 的占位模式）；
   - `context.CancelFunc` 支持取消（任务循环里检查 ctx，签到/旅行/活跃循环改为接受 ctx）。
2. 并发闸门（所有打上游的批量路径共用）：
   - `jobs.max_concurrency`（默认 4，可热配）：信号量限制同时在途；
   - `jobs.upstream_rps`（默认 2，令牌桶）：限制全池任务对上游的总请求速率——1146 号不该按「越快越好」跑，风控优先；
   - **`RunBalanceRefreshNow` 必须改成有界并发**（不复用无上限 goroutine），并让后台周期刷新也用同一闸门；启动首刷改为「延迟 30s + 有界」或在账号数 > 200 时默认跳过首刷（配置可恢复）。
3. 接口（保留旧端点兼容）：
   - `POST /panel/api/jobs/{kind}/run`（`{params}`）→ `{ok, job_id}`；旧端点 `/checkin_all` 等内部改为起 Job 并返回 `{ok, started, job_id}`（旧字段保留）；
   - `GET /panel/api/jobs?limit=20`、`GET /panel/api/jobs/{id}`（含 log_tail）；
   - `POST /panel/api/jobs/{id}/cancel`。
4. 前端：复用任务中心的进度条组件（`.qprog`），账号池顶部显示「当前任务：签到 623/1146 · 失败 4 · 取消」；单号 `tasks/run` 前端轮询 job（或先保留同步、后续切换为 `job_id`，契约上加字段不破坏旧调用）。
5. 与 #11 合并实施：任务循环加 ctx + 统计（#9）+ 闸门（本条）+ 调度解耦（#11）是同一处代码，建议一个 PR 做完。

**预期收益**

- 消除「一次点击 1146 并发」的上游风控风险；余额刷新从「分钟级阻塞 + 504」变成「可控速率后台跑 + 进度」。
- 批量任务可重复点击、可见、可取消；重启后能看到「上次跑了一半」。
- 为 #9 的历史、#19 的通知、#11 的调度统一提供基础设施。

**改动面与风险**

- 改动：新增 `internal/jobs`、`internal/scheduler/*`（ctx + 统计 + 闸门）、`internal/panel/panel.go`（handler 映射）、`app.js`/`index.html`（进度/取消 UI）、`cmd/server/config.go`（jobs 配置）。
- 风险：① 旧端点响应契约（`ok/started`）要保留，避免脚本/前端缓存失效；② 任务取消 = 中途放弃剩余账号，可能造成「一半号签到、一半没签」——取消文案要说明，且取消只停后续、不中断当前账号；③ 闸门配置太小会让 1146 号签到更慢（默认值与 #11 的并发一起调）；④ jobs.json 与 state 一致性问题（只做展示，不参与选号，风险低）。
- 测试：fake upstream + 200 个假账号 → 断言最大并发 ≤ 配置、重复触发 409、取消生效、进度计数正确。

**本次可选子项（强烈建议 v2.0.1 先做）**：
- 给 `RunBalanceRefreshNow` 加并发信号量（默认 8-16），启动首刷延后或跳过；
- `balance_all` 改为「起 goroutine + 立刻返回 job 状态」或至少给 handler 加超时返回（不要让反代 504）；
- 三个 `*_all` 端点加同 kind 单飞（重复点击返回 409）。

**注**：这三项合计约 0.5-1 天，能立刻消除最危险的上游风暴路径。

---

### #11 签到 1 小时优化 + 调度阻塞丢批修复

> **P0 · 工作量：大（4-6 天）· 建议版本：v2.1 · 本次采纳：否（「补跑窗口 + 单飞」子项建议 v2.0.1）**

**现状与痛点（两个独立问题，必须一起看）**

1. **签到串行、一轮约 1 小时**：`RunCheckinNow`（`scheduler.go` 约 466-522 行）逐号执行「daily-checkin + user-resource 两次上游 RPC」，无并发、无不必要的跳过、无重试；1146 号 × 约 3 秒 ≈ 57 分钟（与 `MAINTAINERS.md` 实测吻合）。21:00 那轮还对当天已签到的号再发一次 checkin（上游回「今天已签到」），白付 1146 次 RPC。
2. **调度器阻塞式 runBatch 会吞掉后续时点**：`Run()` 主循环（约 377-409 行）在 `runBatch()` 里 `wg.Wait()` 等所有任务族跑完，返回后用 `nextWake(time.Now())` 重新计算下一个时点；而 `nextFire()`（约 218-230 行）对「已经过去的时点」一律返回**明天**。于是：
   - 09:00 批次（签到+旅行）在 1146 号下约 10:00 结束——正好压在 `activity_hours: [10]` 上，网络稍慢就**当天活跃上报直接漏跑**；
   - 21:00 批次约 22:00 结束——压在 `keepalive_hours: [22]` 上，漏跑意味着**当天 token 保活丢失**；
   - 23:00 夜猫子同理。
   - 这不是理论风险：代码路径上没有任何「迟到补跑」逻辑（只有同一批次内迟到唤醒的 `wakeupGraceDelay`），漏了就是漏一整天。
3. 手动 `checkin_all` 与定时签到之间无单飞（`scheduler` 的 Run* 函数没有互斥），可能并发两轮。
4. 任务函数多为无 ctx 的同步循环（`RunCheckinNow`/`RunKeepaliveNow`/`RunTravelNow` 用 `time.Sleep` 而非 `sleepCtx`），优雅停机/取消（#14）无从谈起。

**建议方案**

1. **调度解耦（优先）**：
   - 每个任务族（checkin/travel/activity/keepalive/blackcat/growth）独立 goroutine + 自己的下次执行时间；主循环只做「到点派发」，**不再 wg.Wait 整批**；
   - 每族单飞：上一轮没跑完则本轮跳过并记 skip（不要叠加）；
   - **补跑窗口**：`schedule.catch_up_minutes`（默认 120，0=关闭）。若「本该执行的时点」已过且在窗口内、且今天该任务未成功跑过 → 立即补跑一轮；
   - 每个任务族记录 `last_run_day`/`last_run_at`（内存 + state 持久化），避免重复补跑。
2. **签到提速与省流**：
   - `schedule.checkin_concurrency`（默认 2，范围 1-8）+ 0-300ms 抖动；配合 #10 的全局 RPS 限速；
   - **当天已签到则跳过 checkin RPC**（`Status.CheckinDone` 已有此字段，来自 `last_checkin_day`），仍照常查余额/解冻——21:00 那轮直接省掉 1146 次 checkin；
   - 分类重试：只对「网络错误 / 超时 / 5xx / 429」重试 1-2 次（指数退避）；402、12153、11101、内容拦截按既有分类不重试；
   - 每号结果进 #9 历史，失败样本可查。
3. **进度与可观测**：签到循环用原子计数写 `done/total/fail/skip`，接到 #10 的 Job 进度；日志行加 `run_id`（#8）。
4. **风控优先的默认值**：默认并发 2（不是 8），并在配置注释里写明「上游有 WAF/风控，调高前先小批量观察 429 率」；保留「串行 800ms」作为 `concurrency=1` 的兼容档。
5. **单号/批量任务统一走同一执行器**：面板单号签到、`tasks/run`、定时签到共用「一次任务执行」封装，避免三套逻辑漂移。

**预期收益**

- 签到 1 小时 → 并发 2-3 + 跳过已签后约 **10-20 分钟**；
- 每天 10:00 活跃上报、22:00 保活、23:00 夜猫子不再随机漏跑（当前是概率性事故）；
- 手动/定时不再叠加打上游；停机可取消，不再「杀进程时签到跑一半」。

**改动面与风险**

- 改动：`internal/scheduler/scheduler.go`（主循环 + 各 Run* 改 ctx + 并发/节流 + 统计）、`internal/scheduler/travel.go`/`streak.go`/`blackcat.go`（ctx 化）、`cmd/server/config.go`（新配置）、`internal/pool`（ReadOnly 快照遍历避免长任务持锁）、`internal/panel`（进度）、测试（`scheduler_test.go` 26 个用例需适配）。
- 风险：① **并发调高会触发上游风控**——默认保守、可回退到 1；② 调度器重构是本次最大的风险点之一，必须补「时点跨越/补跑/单飞」的确定性测试（可用可注入的 clock）；③ 「跳过已签到」要确认 `CheckinDone` 的语义（global/enterprise 门控、跨时区边界），否则会误跳；④ 任务在并发下对 `pool` 的读写与自检逻辑要重新 race 验证（`go test -race`）。
- 实施顺序建议：先做「补跑窗口 + 单飞 + ctx」（小、风险低、立刻止血），再做「并发/跳过/重试」（中），最后做「调度器按任务族解耦」（大）。

**本次可选子项（建议 v2.0.1）**：
- 补跑窗口（`catch_up_minutes` 默认 120）+ 手动/定时单飞：约 0.5-1 天，**能直接消灭「保活/活跃整天漏跑」这个 P0 风险**；
- `RunTravelNow`/`RunCheckinNow` 的 `time.Sleep` 换成 ctx 感知（为 #14 铺垫）。

---

### #12 账号备注服务端化 + 标签/分组

> **P1 · 工作量：中（备注 1 天 / 标签 2-3 天）· 建议版本：v2.1（备注）/ v2.2（标签）· 本次采纳：否**

**现状与痛点**

- 账号备注只存在**浏览器 localStorage**（`app.js` 的 `LS_REMARKS` / `remarksAll()`，约 3684 行起）：换浏览器/清缓存/换设备即丢；手机上看不到桌面端备注。
- 备注键取 **uid 前 8 位**（`remarkKey()`）：1146 个账号下与前缀碰撞概率虽低（≈1.5e-4），但一旦碰撞就是「两个号共用一条备注」的静默错误；且全部逻辑在展示层，服务端无感知。
- 只有「搜索/状态/版本」三个筛选（`accFilters`），1146 个账号无法按「用途/地区/批次/负责人」等业务维度找号；批量按钮只有「全部签到 / 全部保活」这类全池操作，无法「只对某一批号做」。`MAINTAINERS.md` 明确记录：**自定义分组 + 按分组/交集批量暂停选号曾经做过又按最小化撤掉了**——所以本条必须解释「这次为什么不一样」，否则会第三次回退。
- 任务中心/请求日志/积分历史里的账号标识只有 uid8 或昵称，没有业务备注贯穿。

**建议方案**

1. 服务端元数据存储：`data/account-meta.json`（0600、原子写、`schema_version`）：
   ```json
   { "version": 1, "accounts": { "<full-uid>": { "note": "采购A-备用", "tags": ["采购A","待补签"], "group": "主力池", "updated_at": "..." } } }
   ```
   - 键必须是**完整 uid**（不是 uid8），与 `auths/`、`state.json` 同源，不参与选号/计费/鉴权。
2. 接口：
   - `GET /panel/api/account_meta`（全量，约 1146 条 × 100B ≈ 100 KB，可随分页接口一起返回）；
   - `GET/PUT/DELETE /panel/api/accounts/{uid}/meta`；
   - `POST /panel/api/account_meta/import`（从 localStorage 迁移：前端先用当前 overview 把 uid8→uid 解析好再提交，服务端只存全 uid）。
3. 面板：
   - 账号池备注列/编辑沿用现有弹窗，改成「读服务端 + 写服务端」；
   - 筛选栏增加 `tag` / `group` 下拉（数据来自 meta 聚合），与 #1 的服务端筛选参数打通；
   - 批量操作增加「对当前筛选结果执行」：把筛选命中的 uid 列表传给 #10 的 Job Runner（**不做持久化分组树、不做交集选号**——这正是上次回退的复杂度来源；只做「筛选结果 → 批量动作」的一次性传递）。
4. 迁移与兼容：首次打开若服务端 meta 为空而 localStorage 有备注 → 提示「检测到本机备注，是否导入服务端（覆盖/合并）」；localStorage 降级为缓存，服务端权威。
5. 备注/标签的展示贯穿：账号池、任务弹窗、请求日志筛选、积分历史（后端在响应里带 `account` 昵称已有，可加 `note`）。

**预期收益**

- 1146 个号可命名、可分类、可跨设备；「这批号是干嘛的」不再靠记忆。
- 批量操作从「全池」细化到「某个子集」，减少误伤。
- 消除 localStorage 丢失与 uid8 碰撞隐患。

**改动面与风险**

- 改动：`internal/panel`（meta store + handler + overview/分页响应）、`app.js`（备注读写、筛选、导入）、`index.html`（筛选控件）、`cmd/server/main.go`（文件路径）、测试。
- 风险：① 上次分组回退的教训——**不要做复杂分组模型**，先「备注 + 单层 tags」，且必须服务端持久化；② meta 与 auths 删除的联动（删号时清 meta，避免残留；但保留「已删除账号的备注」也有价值——建议保留并标注孤儿）；③ 导入覆盖策略要二次确认；④ 备注内容可能含敏感信息（用户自填），落盘 0600 且不写日志。
- 备选：如果只想先解决「换设备丢失」，备注可只做服务端单字段（不做 tags/group），1 天内完成。

**本次可选子项**：备注服务端化（最小版）可进 v2.1 首批；标签/分组放 v2.2，并明确「不做分组树/交集」。

---

### #13 state.json 可靠性与写放大治理 + 备份恢复 + 数据治理

> **P1 · 工作量：中（2-3 天）· 建议版本：v2.1 · 本次采纳：否**

**现状与痛点**

- **写放大**：`internal/pool/persist.go` 的 flusher 每 `flushInterval = 5s` 检查一次 `dirty`，有变更就把**全部 1146 个账号**序列化并整文件重写（`saveLocked`，约 240-274 行，tmp + rename，`json.MarshalIndent` 缩进）。而 `NoteSuccess`/`NoteError`/`NoteModelCost`/积分写入等热路径都会置 dirty——**只要有聊天/任务流量，基本每 5s 都在整文件重写**。按 1146 账号 state 约 0.5-1 MB 估算：约 6-12 MB/分钟、0.4-0.7 GB/小时、8-17 GB/天（估算值，需在真实 state 上实测）；且 `json.MarshalIndent` 比紧凑序列化更慢更大（`internal/pool/bench_test.go` 已量化 46 账号版本）。
- **损坏即静默清零**：`load()`（约 120-131 行）`json.Unmarshal` 失败直接 `return`——不记日志、不隔离坏文件；随后一次 flush 会用「空状态」覆盖掉那个损坏但仍可能人工抢救的文件。**没有任何备份**（README 把备份责任交给用户）。
- **无 schema 版本**：`stateFile` 只有 `accounts`/`scheduling`，后续字段演进没有迁移钩子。
- **无单实例锁**：两个容器/两个进程指向同一 `data/` 会互相覆盖 state（NAS/误操作常见）。
- **无数据治理视图**：state.json、usage.json（`maxBuckets = 400_000`，`internal/usage/usage.go` 约 36 行）、`request-logs/`（默认保留 7 天、上限 100 MB）、`credit-history.json`（上限 2000 条）各自有保留策略但没有统一面板展示；磁盘写满时只有 `notePersistFail` 节流日志（每 12 次失败一条），面板无感知。
- `saveLocked` 的 tmp 写入**没有 fsync**（`os.WriteFile` + `os.Rename`），断电场景可能丢最后一次写（rename 原子性保护了「不出现半个文件」，但不保证内容落盘）。

**建议方案**

1. **损坏恢复**：`load()` 失败时 ① 记 ERROR 日志；② 把坏文件改名为 `state.json.corrupt-<timestamp>`（保留抢救价值）；③ 尝试读 `state.json.bak`；④ 都失败才空池启动，并在面板/日志醒目提示。
2. **备份**：
   - 每次成功 rename 前，把旧文件保留为 `state.json.bak`（一次写盘多一次 copy，成本可接受）；
   - 每日首次成功写盘后（或按 `state.backup_keep_days`）生成 `data/backups/state-YYYY-MM-DD.json`，保留 7 份；
   - 提供 `scripts/backup.sh`/`backup.ps1`：打包 `auths/ data/ config.json`（0600）+ 校验和 + 恢复步骤文档；面板「数据」卡片显示「最近备份时间/大小」（从文件 stat 读，不额外维护状态）。
3. **写放大**：
   - flusher 默认间隔 5s → **30s**（配置 `state.flush_interval`），并在账号生命周期操作（add/remove/disable/pause/resume）后**强制立即 flush**（`p.FlushNow()`），保证「删号后立刻重启」不复活；
   - `stateOverviewLocked` 产物用 `json.Marshal`（紧凑）落盘、面板/备份可读性用 `jq` 解决（缩进对机器无意义，体积可省 ~20-30%）；
   - 可选进阶：把「高频计数」（success/err/last_used/token_usage）与「生命周期状态」（credits/disabled/cooling）分文件，高频文件用更长间隔；改动大，先不做。
4. **schema 版本 + 单实例锁**：`stateFile` 加 `"version": 1`；启动用 `data/.lock`（`O_CREATE|O_EXCL` + 进程退出清理；Windows 用独占打开）检测多实例，冲突时 fail fast 并提示。
5. **数据治理**：面板新增「存储」卡片/接口 `GET /panel/api/storage`：各文件/目录大小、最后修改时间、保留策略、磁盘剩余（`syscall.Statfs`，Windows 用 `GetDiskFreeSpaceEx`）；低于 10% 或 state 写失败时置顶告警（配合 #19 通知）。
6. fsync：tmp 文件 `f.Sync()` 后再 rename（与 `saveConfig` 的 EBUSY 分支保持类似风格），rename 后可选 `dir.Sync()`（Linux；Windows 忽略）。

**预期收益**

- 崩溃/断电/磁盘满后**状态可恢复**，不再「静默清零 + 覆盖证据」；
- SSD 写量从约 0.4-0.7 GB/小时降到约 0.06-0.12 GB/小时（30s 间隔 + 紧凑序列化，估算）；1146 号场景下这是实打实的硬件寿命与 IO 争用改善；
- 多实例误部署从「互相踩」变成「启动即报错」；
- 磁盘水位/备份状态一目了然。

**改动面与风险**

- 改动：`internal/pool/persist.go`（load/saveLocked/flusher/backup/version/lock）、`internal/pool/pool.go`（暴露 FlushNow）、`cmd/server/main.go`（启动自检、备份路径）、`internal/panel`（storage 接口/卡片）、`scripts/`。
- 风险：① flush 间隔变长后崩溃最多丢 30s 的计数（可接受，且生命周期操作强制 flush）；② `.bak` 让写盘 IO 翻倍——可只对「每日首写」保留；③ Windows 下锁/Statfs 需要 build tag；④ 测试大量依赖 `flushInterval` 变量（可直接改小保持测试速度）。
- 验收：`go test ./internal/pool` 已有 95 个用例；新增「坏文件隔离」「.bak 恢复」「版本不符拒绝」「双实例锁」四组。

**本次可选子项**：坏文件隔离 + `.bak` + 每日备份 + 启动磁盘/权限自检（约 1 天）可进 v2.1 首批；写间隔调整与存储面板随后。

---

### #14 优雅停机与滚动升级

> **P1 · 工作量：小（0.5-1 天）· 建议版本：v2.1 · 本次采纳：否**

**现状与痛点**

- `cmd/server/main.go`（约 383-395 行）：收到 SIGINT/SIGTERM 后 `p.Flush()` 再 `srv.Shutdown`，超时**硬编码 5s**：
  - 长 SSE 聊天流（README 明确「聊天流没有总时长上限」）超过 5s 会被直接掐断，客户端表现为中途断流；
  - 没有等调度器/任务 goroutine 收尾（`Scheduler.Run` 的 goroutine 不在 WaitGroup 里），签到跑一半就被进程退出；
  - `p.Flush()` 在 Shutdown 之前执行，之后若有请求写池状态（成功计数等）可能再置 dirty，而 flusher 已被 `Close` 停掉——最后一次变更可能丢。
- `docker-compose.yml` 没有 `stop_grace_period`，Docker 默认 10s 后 SIGKILL——即使服务端想等，容器也会被杀。
- 没有「draining」状态：停机期间 `/healthz` 仍返回 200，负载均衡/编排器继续把新请求打进来（单机 docker 影响小，但升级/多实例时会有）。
- 没有升级流程文档：「改了镜像 → compose up -d → 容器被替换」期间正在进行的长请求与批量任务无提示。

**建议方案**

1. 可配置停机超时：`server.shutdown_timeout`（默认 30s，范围 5-120s）替换硬编码 5s。
2. 停机顺序（在 `main.go` 里显式编排）：
   1) 标记 draining（`/readyz` 立即 503，`/healthz` 可选保持 200 到退出）；
   2) cancel 调度 ctx → 各任务族感知 ctx 停止取新账号（配合 #11）；
   3) `srv.Shutdown(ctx)` 等在途 HTTP（含 SSE）结束；
   4) 等待任务 WaitGroup（新加）收尾；
   5) 最后 `p.Flush()` + `p.Close()`（放到 Shutdown 之后，避免「flush 后又被写脏」）；
   6) 日志输出「in-flight=N tasks=R 已完成停机」。
3. Docker：`docker-compose.yml` 加 `stop_grace_period: 60s`；Dockerfile 显式 `STOPSIGNAL SIGTERM`（默认即 TERM，写明便于 review）；`HEALTHCHECK` 保持。
4. 升级文档：`docker compose pull && docker compose up -d` 前先看面板是否有 running job（#10），有则取消/等待；升级后检查 `/panel/api/task_runs` 的 interrupted 记录。
5. 重启后：`jobs.json` 中 running 状态改为 `interrupted` 并在面板提示（#10）。

**预期收益**

- 重启/升级不再「正在生成的长回答断流」「签到跑一半被吞」；
- 状态落盘顺序正确，最后一批成功计数不丢；
- 升级操作有据可依。

**改动面与风险**

- 改动：`cmd/server/main.go`、`cmd/server/config.go`（shutdown_timeout）、`docker-compose.yml`、`internal/scheduler`（WaitGroup）、文档。
- 风险：停机时间变长（默认 30s 会让 `docker compose restart` 变慢）——通常可接受；SSE 客户端需自动重连（OpenAI 客户端一般会）。
- 测试：进程级难以单测，可用「信号 + fake handler 睡眠」的集成测试或脚本验证（verifier 可做）。

**本次可选子项**：无（0.5 天，v2.1 初期做）。

---

### #15 配置变更安全（备份 / 回滚 / 危险值拦截）

> **P1 · 工作量：小（0.5-1 天）· 建议版本：v2.1 · 本次采纳：否**

**现状与痛点**

- 面板「配置」页保存走 `cmd/server/main.go saveConfig`（约 462 行）：校验 → 深合并 → 临时文件 + rename（bind mount EBUSY 回退）→ 热应用 → 返回需重启字段。实现本身相当严谨（保留未知键、原子替换、失败保留 tmp），但**没有「上一版配置」备份**：一次错误改动（比如把 `auth_dir` 指错、把 `api_key` 清空、把 `soft_rate` 调成 1s）只能手工回忆恢复。
- **危险值不拦截**：`api_key: ""` 在语义上 = 关闭鉴权（README 明确），但面板保存时没有二次确认/拒绝；`listen` 改成 `0.0.0.0:xxxx` 时也不提醒「公网暴露且无 TLS」。
- 面板保存时返回 `restart_required` 字段清单（如 `listen/auth_dir/state_file/upstream.*`），但用户很容易忽略——部分字段「已保存但未生效」会让人误以为生效。
- 配置里未来会增加通知 token、用户口令哈希等敏感项（#6/#19），`GET /panel/api/config` 目前返回整份配置（面板展示用），需要逐项考虑掩码。

**建议方案**

1. 保存前自动备份：`config.json` → `config.json.bak`（保留 3 个带时间戳的 `config.json.bak-<ts>`，只在「内容有变化」时生成；文件 0600）。
2. 保存响应增强：返回 `{changed: ["pool.credit_floor","api_key"], restart_required: [...], backup: "config.json.bak-..."}`；前端弹出「本次改动 N 项，其中 3 项需重启」的确认框（现在可能已有部分）。
3. 危险值拦截（服务端）：
   - `api_key == ""` 且 `security.allow_anonymous != true` → 400 拒绝（与 #4 的空 key 阻断同一判据）；
   - `listen` host 从回环改为非回环且无 TLS/无 `panel.api_key` → 返回 warning 字段，前端强提示；
   - `auth_dir`/`state_file` 父目录不可写 → 拒绝（避免「保存成功但重启后起不来」）。
4. 一键回滚：`POST /panel/api/config/rollback`（把最近一个 `.bak-*` 恢复并重新热应用，需重启字段照旧提示）。
5. 敏感字段掩码：`GET /panel/api/config` 对 `api_key`、通知 token 等返回 `"sk-****"` 占位（前端未修改则提交原值，需要后端支持「掩码值 = 不修改」的合并语义）；防止面板被肩窥/截图泄露。
6. 与 #5 联动：配置保存动作进审计（只记变更键名，不记值）。

**预期收益**

- 改错配置可回滚；「手滑把鉴权关掉」「把目录指错」在保存时就被拦；
- 敏感配置不再以明文出现在面板 DOM/截图里。

**改动面与风险**

- 改动：`cmd/server/main.go saveConfig`、`internal/panel/panel.go saveConfig handler`、`app.js`/`index.html`（确认框/回滚按钮/掩码）、`config.example.json`。
- 风险：① 掩码合并语义要和「深合并保留未知键」兼容（掩码值不能真写盘）；② `.bak` 与 bind mount 场景（单文件挂载无法 rename，见现有 EBUSY 回退）——备份 copy 放同目录即可；③ 校验过严可能挡住合理操作（提供 `security.allow_*` 逃生门）。

**本次可选子项**：备份 + 空 key 拦截（半天，可与 #4 同 PR）。

---

### #16 Go 1.22.5 → 1.24 + 依赖清理 + 供应链检查

> **P1 · 工作量：小（0.5 天）· 建议版本：v2.1 · 本次采纳：否**

**现状与痛点**

- `go.mod` 声明 `go 1.22.5`，但 `Dockerfile` 构建阶段用 `golang:1.23-alpine`——**发布二进制与 CI 测试的 Go 版本不一致**（CI 的 `setup-go` 用 `go-version-file: go.mod`，实际按 1.22.5 跑测试）。这种「测试一套、发布另一套」是典型的隐蔽风险。
- 依赖卫生：`go.sum` 里同时存在 `redis/go-redis/v9 v9.18.0` 与 **v9.22.0** 两条记录（后者未被 go.mod 引用，疑似历史 `go get` 残留）；`go.uber.org/atomic v1.11.0` 以 indirect 形式存在，但代码里 grep 不到任何 import（Go 1.19+ 标准库 `sync/atomic` 已覆盖其用途）——说明很久没跑过 `go mod tidy`。
- 依赖只有 `go-redis v9.18.0` + `xxhash` + `rendezvous` + `x/sys` + `go.uber.org/atomic`（非常轻，这是优点）；但没有 `govulncheck`、没有 Dependabot/Renovate，上游依赖的安全公告不会自动可见。
- Go 1.23/1.24 的收益：标准库 HTTP/TLS/timer 修复、`sync` 改进、`os.Root` 等（本项目直接收益有限，但**保持受支持的工具链**本身就是安全项）；1.22 已不在官方安全支持窗口内。

**建议方案**

1. `go.mod`：`go 1.24` + `toolchain go1.24.x`（固定补丁版本，避免不同机器用不同 minor）；`docker/Dockerfile` 改 `golang:1.24-alpine`；README badge、开发环境要求同步。
2. 依赖清理：`go mod tidy` 后确认 `go.uber.org/atomic` 是否消失、`go.sum` 是否只剩 v9.18.0；把 go-redis 升到该 major 的最新 v9.x（先跑全量测试；`redisstore` 只用了基础 API，风险低）、`golang.org/x/sys` 升到最新。
3. 供应链：CI 加 `govulncheck ./...`（Go 官方工具，失败即红）；GitHub 开 Dependabot（gomod / github-actions / docker 三类，月度），PR 自动跑测试。
4. 验证：`go build ./... && go vet ./... && go test ./...`；CI 的 go-version-file 自动跟随新 go.mod；发一版 `v2.1.0` 验证 GHCR 镜像正常。

**预期收益**

- 测试与发布用同一 Go 版本；依赖干净、漏洞可查；升级是后续所有改动的「地基」。

**改动面与风险**

- 改动：`go.mod`/`go.sum`、`Dockerfile`、README、CI workflow。
- 风险：① go-redis 升版可能有 API 变化（本项目只用 Set/Get/Expire 级别，低风险）；② `toolchain` 与老构建机（本地 Go 1.22）不兼容——README 说明「本机需 Go 1.24+」；③ 1146 号规模下内存/GC 行为几乎不变。
- 验收：`go test ./...` 全绿 + 镜像构建 + 启动冒烟（`/healthz`）。

**本次可选子项**：`go mod tidy` + `COPY go.mod go.sum`（Dockerfile）可顺手在 v2.0.1 做，零风险清理。

---

### #17 CI 强化 + 容器运行时加固

> **P1 · 工作量：中（1-2 天）· 建议版本：v2.1 · 本次采纳：否**

**现状与痛点**

- **CI 门槛低**：
  - `.github/workflows/docker-ghcr.yml` 只做 build/push，**不跑测试**（PR 也只构建镜像）；
  - `.github/workflows/go-binaries.yml` 的 `test` job 只有 `go test ./...`（Go 会自动跑 vet 子集，但没有显式 `go vet`、没有 `-race`、没有 `gofmt`/`staticcheck`、没有覆盖率、没有 `govulncheck`）；
  - 两个 workflow 都没有镜像扫描/SBOM（`provenance: false` 还主动关了 attestation）。
- **Dockerfile**：`COPY go.mod ./`（第 4 行）没有一起 COPY `go.sum`，随后的 `go mod download` 依赖「自动补 go.sum」的隐式行为，缓存/校验语义不清晰；基础镜像未按 digest 固定；没有 OCI labels（version/revision）。
- **CI 缓存**：Docker workflow 已用 `cache-from/cache-to: type=gha`（这点是好的）；Go 构建靠 setup-go 默认缓存；但依赖层因缺 `go.sum` 命中不稳定，也没有显式的 buildx mount cache，构建耗时随依赖变化波动。
- **容器运行时**：已经是非 root（`USER app`，uid 10001）——这点做得好，是现状而非待办；但 compose 未加 `read_only`、`cap_drop`、`no-new-privileges`；**没有配置 docker 日志轮转**（`json-file` 默认无限增长）——1146 号签到一次就是上千行 stdout，长期跑会把宿主机磁盘写满；没有 `stop_grace_period`（见 #14）。
- 多架构：Docker 镜像固定 `linux/amd64`（本地部署是 x86，这是有意的性能取舍，**不建议**为了多架构加回 QEMU）；二进制矩阵已覆盖 5 平台，保持即可。

**建议方案**

1. CI：
   - test job 增加：`gofmt -l` 非空即失败、`go vet ./...`、`go test -race ./...`（`internal/pool`、`internal/scheduler`、`internal/server`、`internal/panel` 优先；全量 race 太慢可只跑核心包）、`govulncheck ./...`、覆盖率 artifact（不强制阈值）；
   - Docker workflow 增加 `needs: test`（或复用 go-binaries 的 test job），保证「镜像能构建」不等于「代码正确」；
   - 显式声明 setup-go 缓存（`cache: true`，依赖 `go.sum`）+ 保留 `cache-from/to: type=gha`；`COPY go.sum` 修好后依赖层可稳定复用；
   - 加 `trivy`/`grype` 镜像扫描（失败仅告警，高危失败）+ `anchore/sbom-action` 生成 SBOM 挂到 Release；
   - Dependabot 自动 PR（#16）。
2. Dockerfile：
   - `COPY go.mod go.sum ./`；
   - 基础镜像固定 digest（注释里写对应 tag，Dependabot 更新）；
   - 加 OCI labels：`org.opencontainers.image.version=$APP_VERSION`（可用 build-arg 传入 appVersion）、`revision`、`source`；
   - 保留 `USER app`、`HEALTHCHECK`；可选加 `STOPSIGNAL SIGTERM`。
3. docker-compose.yml：
   - `read_only: true` + `tmpfs: ["/tmp"]`（应用只写挂载的 `./auths`、`./data`、`config.json`；Python 脚本需要 /tmp 可写）；
   - `security_opt: ["no-new-privileges:true"]`、`cap_drop: ["ALL"]`；
   - `stop_grace_period: 60s`；
   - `logging: { driver: "json-file", options: { max-size: "20m", max-file: "5" } }` ← **这条对 1146 号部署最实用**；
   - healthcheck 显式写出（compose 里目前依赖镜像 HEALTHCHECK，可保留）。
4. 发布：`v*` tag 流程已有「tag == appVersion」断言，保留；Release 附件加 SBOM/checksums（已有 checksums）。

**预期收益**

- 数据竞争、格式、vet 问题在 PR 阶段拦截（`internal/auth` 历史上就出过并发读写的 race，见 `auth.go AccessTokenValue` 注释）；
- 容器只读 + 去能力 + 日志轮转：被入侵后的破坏面更小，宿主机磁盘不会被日志写满；
- 镜像可追溯（digest + labels + SBOM）。

**改动面与风险**

- 改动：`.github/workflows/*`、`Dockerfile`、`docker-compose.yml`、可能新增 `.github/dependabot.yml`。
- 风险：① `read_only` 可能挡住未发现的写路径（先 dry-run 跑一轮完整功能再合入）；② race 测试可能暴露既有并发缺陷，需要留时间修；③ trivy 扫描基础镜像（alpine）漏洞噪音多，先设「仅 high/critical 告警」。
- 验收：PR 上 CI 全绿；`docker compose up -d` 后面板/签到/配置保存/登录均正常；日志轮转生效（`docker inspect` 看 LogConfig）。

**本次可选子项**：compose 的日志轮转 + `stop_grace_period`（10 分钟、零风险）建议 v2.0.1 顺手带上。

---

### #18 前端工程化（模块拆分 + 真实 DOM 测试 + lint）

> **P2 · 工作量：中（3-5 天）· 建议版本：v2.2 · 本次采纳：否**

**现状与痛点**

- `internal/panel/app.js` 192.8 KB / 3766 行，单文件全局脚本；`index.html` 84 KB / 1399 行（内联 CSS 也在里面）；`mobile.js` 215 行。没有 `package.json`、没有 lint/format、没有构建步骤（这是**有意取舍**：`go:embed` 单二进制、零外部依赖，不建议轻易推翻）。
- 测试是「Go 驱动 node」的字符串切片 harness（`frontend_test.go` 1494 行 / 27 个用例）：用 `src.indexOf('function X')` 截取函数源码、用 Proxy DOM 桩执行。它确实抓到过真实白屏（v1.11.3/1.11.4 两连炸后加的冒烟），但**对重构极度脆弱**：改个函数名/顺序/加个 `async`，切片测试就失效或误报；且只能测纯函数，测不了真实 DOM 交互（筛选、分页、弹窗状态）。
- 本次前端改动（删补偿 + 各列表分页筛选 + 做任务按钮）是一次大范围重渲染，正是「字符串切片测试最脆弱」的场景。
- 没有 CI 级 lint，1146 号场景下的前端 bug（如 `Math.max(...1146 项)` 的栈限制、筛选状态在轮询后被重置）只能靠人肉发现。

**建议方案（分两阶段，保持零构建原则）**

1. **阶段一（小，可先做）**：
   - 在仓库根加 `package.json`（仅 devDependencies，不参与 embed/镜像）：`eslint`（`no-undef`、`no-unused-vars`、`eqeqeq`、`no-eval`）、`prettier`（只做 check，不强制重排以免 diff 爆炸）；
   - `app.js` 把纯函数（分页切片、筛选匹配、格式化、时间范围）挂到一个显式测试出口（如 `window.__WB_TEST__ = {slicePage, accMatch, ...}`），Go 测试直接 `vm.runInContext` 调函数，**不再字符串切片**；
   - CI 加 `npx eslint internal/panel/*.js`（或 Go 测试里调 node 执行）。
2. **阶段二（中）**：
   - 拆 ES Modules：`internal/panel/app.js` 变为入口，引入 `lib/api.js`（fetch 封装）、`lib/state.js`、`views/accounts.js`、`views/logs.js` 等；`index.html` 改 `<script type="module" src="/panel/app.js">`（CSP `script-src 'self'` 对同源 module 放行，无需改 CSP）；
   - `go:embed` 用 `embed.FS` + `fs.Sub` 提供 `/panel/js/*` 静态资源（注意路径与缓存头，#2）；
   - 测试改用 `node:test` + `happy-dom`/`jsdom`（devDependency），对 render 函数做真实 DOM 断言（如「渲染 1146 行时只创建 100 个 tr」）；
   - **不引入 React/Vue/Vite**：对自托管单二进制是负收益；用原生 + 小工具函数即可。
3. 保留现有冒烟测试（`TestAppJSTopLevelSmoke`）作为最后防线；新增「renderAccounts 分页边界」「筛选后计数」「做任务按钮存在」等真实 DOM 用例。

**预期收益**

- 前端回归从「字符串切片碰运气」变成「行为测试」；后续改 UI（分页、筛选、移动端）有安全网。
- 拆模块后 app.js 可读性/协作性提升，改一个视图不再 grep 4000 行。
- lint 拦截未定义变量类白屏事故（历史两次事故的根因类型）。

**改动面与风险**

- 改动：`internal/panel/*.js`（拆文件）、`internal/panel/index.go`（embed FS + 静态路由）、`index.html`（script 标签）、`frontend_test.go`（重写 harness）、`package.json`、CI。
- 风险：① 拆分期间功能 regression——必须先在分支拆完并让现有 27 个用例全绿（或逐个替换）；② 多文件 embed 与移动端加载顺序（mobile.js 依赖 app.js 全局）要理清；③ 上游已停更，不再有「合并上游冲突」约束，这是做拆分的窗口期。
- 建议：阶段一（测试出口 + lint）可随任意前端 PR 顺手做，半天；阶段二单独排 v2.2。

**本次可选子项**：阶段一的「测试出口替换字符串切片」可在 v2.1 做（约半天），能立刻让后续前端改动安全。

---

### #19 事件通知渠道（Webhook / 企业微信 / Telegram / Bark）

> **P1 · 工作量：中（2-3 天）· 建议版本：v2.1 · 本次采纳：否**

**现状与痛点**

- 全仓库 grep `webhook|notify|telegram|bark|通知`（除 Upstash 注释外）为 0：**没有任何主动通知能力**。所有异常只能「打开面板看」或「翻 docker logs」。
- 1146 号场景下最需要被通知的事件：
  1. 签到/任务批次失败数 > 阈值（#9 有历史后可精确触发）；
  2. 账号连续 12153 被判禁用（`NoteSessionDead` 连续 3 次，scheduler.go keepalive）；
  3. 余额不足/402 硬冷却账号数突增（可能意味着某批号被打空）；
  4. 积分即将到期（到期提醒卡片已有数据，但不会主动推送）；
  5. 健康账号数 = 0 / 网关探活失败（现在只有 `/healthz` 503，没人看）；
  6. 磁盘水位低、state 落盘连续失败（`notePersistFail` 只打日志）；
  7. 配置变更/登录失败（#5 审计后）。
- 「积分到期提醒」目前只是面板卡片；用户不可能每天盯着面板。

**建议方案**

1. 新增 `internal/notify`：
   - 配置 `notify.enabled`、`notify.channels: [{type: "webhook"|"wecom"|"telegram"|"bark", url, token, chat_id, ...}]`、`notify.events: [...]`、`notify.quiet_hours: ["23:00-08:00"]`、`notify.min_interval`（去重/聚合窗口，默认 5 分钟）。
   - 渠道实现：通用 Webhook（POST JSON，可自定义模板）、企业微信群机器人（`msgtype: text`）、Telegram `sendMessage`、Bark `/{token}/标题/内容`；全部走 `http.DefaultTransport`（`dns_servers` 已替换该 Transport，能解析域名）。
2. 事件钩子（与 #9/#8 共用数据）：
   - `task_run_finished`（fail>0 或 duration 异常）、`account_disabled`、`credit_low`（阈值配置）、`credits_expiring_digest`（每日汇总）、`health_degraded`（healthy=0 持续 >2 min）、`state_persist_failed`、`disk_low`、`config_changed`、`login_failed`（#4）。
   - 每条通知带面板链接（`panel.public_url` 配置）与时间/账号数等摘要，**不带凭证/uid 全量**（uid8 即可）。
3. 可靠性：发送异步队列 + 3 次指数退避重试；失败仅记日志/计数（`notify_send_failed_total`，进 #8 metrics）；持久化「最近发送状态」到 `data/notify-state.json`，避免同事件刷屏；静默时段内的事件合并成一条早报。
4. 测试入口：`POST /panel/api/notify/test`（发送一条测试消息）→ 面板配置页「发送测试」按钮，避免配完不知道通不通。
5. 安全：webhook URL/token 在 `config.json`（0600），面板配置页掩码显示（#15）；日志不打印完整 URL。

**预期收益**

- 无人值守：签到批量失败、账号被判死、积分快到期、服务不可用时第一时间在手机/群里收到；
- 把「每天开面板巡检」变成「有事才看」，这是千人号池运维的体验质变；
- 与 #9 合起来回答「昨天到底跑成什么样」。

**改动面与风险**

- 改动：新增 `internal/notify`、`cmd/server/main.go`（装配）、`internal/scheduler`/`internal/pool`（钩子）、`internal/panel`（配置页/测试接口）、`config.example.json`、README。
- 风险：① SSRF（管理员可配任意 URL，属于 admin 能力，可接受，但不要在普通用户可控处触发）；② 通知风暴（任务失败 1146 条 → 必须聚合为一条摘要，`min_interval` + 去重）；③ 渠道 API 变动（Telegram/企微偶有改版）——错误只记日志不阻塞；④ 时区（quiet_hours 用容器本地时区，与 `TZ` 一致）。
- 测试：fake HTTP server 断言请求体/重试/去重；通知内容快照。

**本次可选子项**：先做「通用 Webhook 单渠道 + 签到批次结果 + 账号禁用」最小闭环（约 1 天），其余渠道/事件后补。

---

### #20 移动端收尾（新控件适配、操作收敛）

> **P2 · 工作量：小（1-2 天）· 建议版本：v2.2 · 本次采纳：否**

**现状与痛点**

- 上游已有成熟的 `mobile.css` / `mobile.js`（≤760px 侧栏抽屉 + 宽表卡片化，`mobile.js` 用 MutationObserver 把 `<th>` 文本抄进 `td[data-l]`），基础体验不错。
- 但本次新增/改动的控件没有专门的移动端验收：
  - 账号池筛选栏（搜索/状态/版本 + 每页条数）在 360px 下可能换行拥挤；
  - 账号行操作列新增「做任务」后按钮更多，卡片模式下操作区会很长；
  - 分页控件、任务中心队列、运行日志的筛选/分页在手机上是否好点未知（`mobile.js` 只处理表头标签与抽屉）；
  - 本次删补偿 + 新分页后，`frontend_test.go` 的切片测试覆盖不到「窄屏渲染」。
- 暗色/明暗主题已经完善（跟随系统 + 手动两态切换，`app.js applyTheme`），无需重复建设；本条只做移动端布局与操作层面的收尾。

**建议方案**

1. 移动端专项走查（用 Chrome DevTools 390×844 / iPhone SE）：账号池、任务中心、运行日志、配置页各一遍，记录溢出/遮挡/点不中问题。
2. 操作收敛：
   - 账号行操作按钮在 ≤760px 下折叠为「签到 / 更多（弹层）」——「更多」里放任务、积分、解冻、禁用、暂停、移除；
   - 批量操作区（全部签到/保活/旅行/活跃 + 刷新 + 重新扫描）收纳进一个「批量」下拉；
   - 分页条吸底（sticky bottom）方便长列表翻页；筛选栏横向滚动（`overflow-x: auto` + 隐藏滚动条）。
3. 触控细节：按钮最小点击区 40×40px；危险操作（移除/禁用）保持 `confirm`；表格卡片化时把「备注/做任务」放在首行右侧。
4. 测试：`mobile.js`/`mobile.css` 与新增控件不冲突；在 `frontend_test.go` 加一条「≤760px 时操作按钮使用 data-a 折叠容器」的 DOM 断言（视 #18 进度）。

**预期收益**

- 手机上能完成「看状态、单号签到/做任务、看任务进度、查日志」四件日常事；1146 号场景下很多巡检是碎片时间做的。

**改动面与风险**

- 改动：`internal/panel/mobile.css`、`mobile.js`、`app.js`（操作折叠的少量逻辑）、`index.html`（容器结构）。
- 风险：① 移动端改动容易与桌面端 CSS 互相影响——所有新样式限定在 `@media (max-width: 760px)`；② 折叠后功能可发现性下降（用「更多」明确文案）；③ 无自动化视觉回归，靠人工走查。

**本次可选子项**：无（本次已有上游移动端实现，先不扩）。

---

## 四、明确不建议做的（反向清单，防止走回头路）

| 不建议 | 原因 |
|---|---|
| 重新引入「官方补偿」任何形态 | 本次已明确删除（用户决策）；上游接口本身也在收紧，补偿状态查询/领取带来的请求量与风控暴露不值得 |
| 恢复「积分构成」整池全量查询 / 到期卡「检查」按钮 | 本地已刻意封死（`packages` 无 uid 直接 400）：整池逐号查上游有风控风险；单号「积分」按钮已覆盖需求 |
| 把签到并发调到 8-16「越快越好」 | 1146 号同时打上游的 429/WAF 风险远大于节省的几十分钟；默认并发 2-3 + RPS 限速，宁慢求稳 |
| 用 React/Vue/Vite 重写面板 | 自托管单二进制、零构建是核心优势；引入前端构建链会让「拷一个 exe 就能跑」失效。要工程化走 #18 的「原生模块 + node 测试」 |
| 把 state.json 换成 SQLite/Postgres（除非要多实例） | 当前单机单进程 + 原子 rename 已够用；问题在写放大与损坏恢复（#13），换库不解决且引入迁移风险。若将来要多实例，先做单实例锁 + 备份，再评估共享存储 |
| 为多架构 Docker 加回 QEMU/arm64 | 部署机是 x86_64，多架构构建每次多 10 分钟且无收益（二进制 release 已覆盖 5 平台）；保持 `linux/amd64` |
| 用 WebSocket 替代 SSE/长轮询 | 面板是单向数据流，SSE/长轮询足够；WebSocket 需要额外依赖与鉴权设计 |
| 把 api_key 放在 URL query（为了 SSE 方便） | 会进 access log/浏览器历史/截图；走 ticket 或 fetch 流（#3） |
| 给面板加「一键让全账号发对话/做任务」类按钮 | 1146 个号同时发真实对话是上游风控红线，现有任务中心队列（账号间并发 1-4）已是合理上限 |
| 把「自定义分组 + 分组/交集批量暂停选号」原样加回来 | `MAINTAINERS.md` 已记录它被按最小化撤掉；本次 #12 用「服务端备注/标签 + 对筛选结果批量动作」的轻量形态替代，不做分组树/交集 |

---

## 五、版本路线图（建议）

> 工作量按 1 名熟悉本项目的维护者估算；有队友并行可压缩。每个版本都建议「功能 + 测试 + 文档」一起交付。

### v2.0.1（热修补丁，1-2 天，建议尽快）

**目标：止血，不加新功能。**

| 内容 | 对应建议 | 说明 |
|---|---|---|
| 面板 gzip + 静态缓存 + 轮询可见性/退避 | #2 | 约半天，立刻降低面板流量 |
| `panel.api_key` 与网关 key 分离 + 空 key 非回环阻断 + 提交空 key 拒绝 | #4 | 约半天，堵住提权面 |
| `RunBalanceRefreshNow` 并发闸门 + 启动首刷延后/跳过 + `*_all` 单飞 | #10 子项 | 约半天，消除 1146 并发上游风暴 |
| 调度器补跑窗口（`catch_up_minutes`） | #11 子项 | 约半天，阻止「保活/活跃整天漏跑」 |
| compose 日志轮转 + `stop_grace_period` | #17 子项 | 10 分钟 |
| `go mod tidy` + Dockerfile `COPY go.mod go.sum` | #16 子项 | 10 分钟 |

**验收**：`go build ./... && go vet ./... && go test ./...` 全绿；`docker compose up -d` 后面板/登录/签到/配置保存冒烟通过；确认不再有「单次全池无上限并发」代码路径。

### v2.1「千人池运维效能」（2-3 周）

**主题：把「能跑」变成「可安全运维」。**

| 批次 | 内容 | 交付标准 |
|---|---|---|
| Sprint A（第 1 周） | #1 overview 服务端分页 + 字段裁剪 + ETag；#11 调度器按任务族解耦 + 签到跳过已签/分类重试 | 面板首屏 <1s、5s 轮询响应 <50 KB；1146 号模拟：签到耗时下降 ≥50%；`go test -race` 核心包通过 |
| Sprint B（第 2 周） | #10 Job Runner（进度/取消/单飞/持久化）+ 面板进度 UI；#9 任务执行历史；#14 优雅停机 | 批量操作可重复点击、进度可见、取消生效；重启后历史可见；长 SSE 不被 5s 掐断 |
| Sprint C（第 3 周） | #13 state 可靠性 + 备份 + 存储面板；#15 配置备份/回滚/危险值拦截；#5 审计日志；#19 通知最小闭环；#16 Go 1.24；#17 CI/容器加固；#12 备注服务端化 | 坏 state 可恢复；配置可回滚；审计可查；签到失败/账号禁用能收到通知；CI 全绿含 race/vet/govulncheck |

**v2.1 完成后的量化目标**：面板 5s 轮询流量下降 ≥90%；签到一轮 ≤20 分钟；批量操作无「1146 并发」路径；每次任务有可查历史；密钥泄露不再等于后台失守。

### v2.2「安全与架构」（3-6 周）

**主题：多用户/多设备/长期可信。**

| 内容 | 对应建议 | 说明 |
|---|---|---|
| SSE/长轮询事件推送（含断线降级） | #3 | 在 #1/#10 稳定后做，进度实时化 |
| 多用户/角色/会话登录（RBAC） | #6 | 依赖 #4/#5 已完成 |
| 凭证静态加密 + 脚本兼容方案 | #7 | 依赖 #13 备份方案；先做权限体检 |
| 前端模块化 + 真实 DOM 测试 | #18 | 与后续所有前端改动协同 |
| 账号标签/分组（轻量版） | #12 后半 | 明确不做分组树/交集 |
| 移动端收尾 | #20 | 视用户实际使用反馈 |

**v2.2 完成后**：面板可多人分权、凭证不落明文、前端有行为测试安全网；v2.3 若仍单机，建议只做维护性更新 + 上游接口变化适配（上游已停更，协议收紧随时可能发生）。

---

## 六、建议的推进节奏（给维护者的操作清单）

1. **今天/本周**：按 v2.0.1 表逐项做（都是小改动）；每项独立 commit，方便回滚。
2. **第 1 个迭代**：先做 #1 + #11（最大收益），再做 #10 + #9（基础设施）；这四条之间有依赖（#10 依赖 #11 的 ctx/统计，#9 依赖 #10 的单飞），建议同一分支按顺序提交。
3. **每个版本上线前**：用「1146 账号合成数据」做一次性能回归（见附录 D）；给面板留一天观察期（尤其并发/节流类改动，观察 429 率与签到成功率）。
4. **风险控制**：并发/调度类改动（#10/#11）先以「配置默认保守 + 可随时调回串行」的方式上线；出问题能通过配置回退，不必回滚版本。

---

## 附录 A：按工作量排序的快速清单（挑着做）

| 时间预算 | 可做的建议 | 说明 |
|---|---|---|
| 一个下午 | #2（压缩+缓存+轮询治理）、#17 子项（compose 日志轮转/stop_grace）、#16 子项（go mod tidy） | 纯增益、零功能风险 |
| 一天 | #4（密钥分离+空 key 阻断）、#10 子项（并发闸门+首刷延后+单飞）、#15（配置备份+危险值拦截） | 安全/稳定性止血 |
| 两三天 | #11 补跑窗口+ctx、#9 任务历史、#14 优雅停机、#12 备注服务端化 | 先把「每天要用的信息」补齐 |
| 一周 | #1 服务端分页+ETag、#10 完整 Job Runner、#13 state 可靠性、#8 可观测底座 | 需要设计+测试 |
| 两周+ | #11 完整调度解耦、#5 审计、#19 通知、#16/#17 全量、#3 SSE、#6 RBAC、#7 加密、#18 前端工程化、#20 移动端 | 按路线图分批 |

## 附录 B：关键代码出处索引（便于落地时定位）

| 主题 | 文件 / 位置 |
|---|---|
| 面板路由与 handlers | `internal/panel/panel.go` `routes()`、`overview()`、`withAuth()`、`checkinAll()`、`balanceAll()`、`writeJSON()` |
| 面板鉴权/安全头/CSP | `internal/panel/panel.go` `withAuth()`；`internal/panel/index.go` `csp`/`setSecurityHeaders` |
| 面板前端 | `internal/panel/app.js`（`start()`、`refreshVisible()`、`loadOverview()`、`renderAccounts()`、`remarksAll()`）；`internal/panel/index.html`；`internal/panel/mobile.*` |
| 网关鉴权/健康检查 | `internal/server/handler.go` `withAuth()`、`healthz()`、`status()`、`ServeHTTP()` |
| 调度器 | `internal/scheduler/scheduler.go` `Run()`、`runBatch()`、`nextFire()`、`RunCheckinNow()`、`RunBalanceRefreshNow()`、`StartBalanceRefresh()`；`travel.go`、`streak.go`、`blackcat.go` |
| 账号池状态/持久化 | `internal/pool/persist.go`（`flushInterval`、`load()`、`saveLocked()`）；`internal/pool/state.go`（`List()`、`statusOf()`、`CountsDetailedWithPaused()`）；`internal/pool/entry.go`（`Status` 结构） |
| 凭证 | `internal/auth/auth.go` `Parse()`、`SaveAtomic()`、`LoadDir()` |
| 配置 | `cmd/server/config.go`（`Config`、`WriteDefault()`、`applyEnv()`）；`cmd/server/main.go`（`appVersion`、signal/Shutdown、`saveConfig()`） |
| 请求日志/用量 | `internal/reqlog/reqlog.go`（`Event`、`Snapshot`、`prune`）；`internal/usage/usage.go`（`flushInterval`、`maxBuckets`、`load/flush`）；`internal/server/logging.go` |
| 任务中心队列 | `internal/panel/taskcenter.go`（`queueState`、`startGrowthQueue`、`tasksRunQueue`） |
| 前端测试 harness | `internal/panel/frontend_test.go`（`TestAppJSSyntax`、`TestAppJSTopLevelSmoke`、切片 harness） |
| CI/容器 | `Dockerfile`、`docker-compose.yml`、`.github/workflows/go-binaries.yml`、`.github/workflows/docker-ghcr.yml` |
| 性能基准 | `internal/pool/bench_test.go`（当前 46 账号基准，建议扩到 1146） |
| 本地运维事实 | `MAINTAINERS.md`（1146 实测基线、已知坑、部署链路） |

## 附录 C：未单列的小改进（不计入 20 条，可顺手做）

1. **auths 自动对账**：在「重新扫描」按钮之外，可加「每 N 分钟（或 fsnotify）自动 `LoadDir + SyncToDir`」——注意与手动按钮共存（手动仍保留），默认关闭，避免与外部脚本写文件竞争。
2. **`/healthz` 信息收敛**：公网/反代场景下 `healthy/total` 会暴露账号规模；可提供 `?verbose=0` 默认只回 `{ok, service}`，详细计数只在 `/status`（需 key）。
3. **面板页脚显示版本/commit/构建时间**：当前只显示 `v2.0.0-pro`（`overview.version`）；把 git short sha 打进 ldflags，排查「线上跑的是哪版」更快。
4. **`overview`/`/status` 的 `accounts` 字段加 `?accounts=0` 开关**：给只需要计数的脚本减负（与 #1 的 `lite` 统一）。
5. **任务中心队列持久化**：`queueState` 在内存，重启丢进度（#9/#10 会覆盖，短期可先写一条日志「队列中断」）。
6. **日志里的 uid 统一 uid8**：`logfmt` 已有约定，但部分 panel 日志仍打全 uid（`panel: revive uid=<full>`），与「日志不落全量 uid」的口径不一致，可统一（不影响功能）。
7. **`request_logs` 读取性能**：现在每次从 JSONL 倒序扫描（`reqlog.ReadArchive`），7 天 × 大流量后翻页会慢；可加内存索引或按天文件选择（P2，量大再做）。
8. **`bench_test.go` 扩到 1146 账号**：当前 46 账号基准的注释还写「对齐生产规模」，已过时；扩成 1146 + 加 `/overview` marshaling 基准，作为 #1/#13 的验收工具。

## 附录 D：如何验证这些建议真的生效

1. **性能类（#1/#2/#3）**：
   - `go test -bench ./internal/pool -benchmem`（先按附录 C-8 扩到 1146 账号）记录 baseline；
   - 用 `curl -s -o /dev/null -w '%{size_download} %{time_total}\n' -H "Authorization: Bearer $KEY" -H 'Accept-Encoding: gzip' http://127.0.0.1:7863/panel/api/overview?lite=1` 对比改造前后；
   - 浏览器 DevTools Network 面板看 5s 轮询请求体积与「页面隐藏时是否停止」。
2. **调度类（#10/#11）**：
   - 用 1146 个合成账号（`auths/workbuddy-synth-<n>.json` 指向假的 refresh token）启动测试实例，配合 fake upstream（httptest）统计「最大并发数 / 每分钟请求数 / 任务总耗时」；
   - 用可注入时钟（或把时点改到 1 分钟后）验证「批次跨过下个时点时会补跑而不是漏跑」；
   - `go test -race ./internal/scheduler ./internal/pool`。
3. **状态/备份（#13/#14）**：手工把 `state.json` 写坏 → 启动应隔离坏文件并从 `.bak` 恢复；`kill -TERM` 一个正在跑签到的进程 → 重启后无半写文件、任务历史显示 interrupted。
4. **安全（#4/#5/#7）**：用错误 key 连打 30 次 → 应出现 429；`POST /panel/api/config` 提交空 api_key → 应 400；审计文件应记录这次尝试（#5 完成后）。
5. **可观测（#8/#9）**：`curl /metrics | grep wb` 有数据；`GET /panel/api/task_runs?limit=5` 能列出最近任务；`/readyz` 在 `auth_dir` 不可读时返回 503。
6. **回归底线**：每个版本 `go build ./... && go vet ./... && go test ./...` 全绿 + `docker compose up -d` 后 `login → 面板 → 签到 → 配置保存` 四条冒烟路径通过（verifier 的脚本可直接复用）。

---

## 结语

20 条建议里，**真正决定这套系统能不能长期运维 1000+ 账号的只有 5 条 P0**：把面板轮询瘦下来（#1/#2）、把后台凭据边界切开（#4）、把「1146 并发 + 1 小时串行」这两个批量操作隐患修掉（#10/#11）。其余 15 条是「更安全、更可观测、更好维护」的渐进增强，可以按路线图从容推进。

最后再强调一次边界：**本次 v2.0.0-pro 已删除的「官方补偿」不建议以任何形式回归**；本文所有建议都在「不重新引入补偿、不恢复整池积分构成、不做激进风控操作」的前提下成立。


