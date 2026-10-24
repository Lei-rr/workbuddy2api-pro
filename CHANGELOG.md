# Changelog

本文件记录 **WorkBuddy2API Pro**（`workbuddy2api-pro`）的版本变更。

- 原始项目：[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)（已删除）
- 上游最终版：[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) `73a7560`（2026-10-21 停止维护）
- 本仓库：在**上游最终版**基础上继续二次开发（副标题、面板定制、Pro 改动均以下文为准）

---

## v2.6.0-pro（2026-10-24）

生产化审计批次：用户要求「审计全部项目代码、优化升级、允许架构重构、清理垃圾文件、符合线上标准」。四路并行审计（Go 核心 / panel / 运维配置与仓库卫生 / 前端）共发现 **P0 6 项、P1 50+ 项**，本版修复全部 P0 与主要 P1。

### 安全 P0

- **默认密钥风险**：`config.example.json` 不再含 `test_key`（`api_key` 为空）；新增弱密钥启动守卫——`api_key`/`panel.api_key` 命中弱值名单（test_key/changeme/password/123456/example 等）且监听非回环时**拒绝启动**并提示生成随机密钥（回环仅 WARN）；Docker 默认配置因此 fail-fast，不再出现「公网已知密钥网关」
- **`/v1` 请求体无上限**：新增 `server.max_body_mb`（默认 64MiB，env `WB2A_MAX_BODY_MB`，clamp 1–1024）；`/v1/chat/completions`、`/v1/messages` 等用 `http.MaxBytesReader` 限长，超限 413（OpenAI/Anthropic 错误体）；面板 POST 同样限长（普通 1MiB / import 32MiB）
- **面板敏感响应**：`/panel/api/*` 默认 `Cache-Control: no-store`（config 回显含密钥/口令不再被缓存；accounts 的 ETag/304 语义保持）；`meta`/`jobs` 写操作补操作审计（`account_meta_update/delete`、`job_run/cancel`）

### 可靠性与性能 P0

- **session 粘性 O(n)**：pool 新增按 (model, realm) 的可用账号 `AvailabilityView` 缓存（Contains O(1)、权重槽复用、membership 代际 + 500ms TTL、容量 256），session 快路径 O(1)；10k 下每请求 7.6–8.9ms → **108–131ns（0 alloc）**
- **pool 落盘锁内 IO**：改为锁内快照 + 锁外原子写（`persistMu` 串行 + 版本号防旧覆盖，保留 .bak/失败语义）；10k 状态约 76ms 的 marshal+写盘不再阻塞 Pick/chat
- **停机竞态**：scheduler 余额刷新/补领清扫等后台循环纳入统一 WaitGroup（`WaitBackground`），停机顺序改为 cancel → Shutdown → 等后台循环 → **最后** Flush；超时 WARN 仍 Flush
- **账号级无上限扇出**：补领清扫/任务扫描/成长队列/昵称同步改为有界 worker pool（`schedule.task_sweep_concurrency` 默认 8，clamp 1–64，env `WB2A_TASK_SWEEP_CONCURRENCY`），10k 池不再瞬时上万 goroutine；进度聚合、ctx 取消即停

### 其它修复

- panel：`writeJSON` 序列化失败不再 200 空 body（改 500+日志）；import 空 `auth_dir` 拒绝（不写 CWD）；抽 `readJSONBody`/`parseLimit`/`accountForRequest`/`fanOutAccounts` 等公共 helper；删除死代码、修正 8 处失准注释
- 前端：4 类 HTML 注入兜底（未知枚举/数值字段全部转义）、fetch 15s 超时、usage/logs/packages/meta 请求序号防旧响应覆盖、日志页重数据请求降频（5s 轮询请求数 −75%）、mobile observer 按视口增量处理、6 个弹层可访问性（role/aria-modal/焦点陷阱/Esc）、死代码与重复格式化器清理
- 启动/运维：`login.sh` 写 auth 强制 0600；`config.json.bak` 加入 .gitignore；`.dockerignore` 排除文档/tools；热生效清单按代码修正（jobs/notify/prompt/global/dns/session_sticky.enabled/panel.events/upstream 身份/allow_anonymous/max_body_mb 等改为明确「需重启」）；upstash 清空/修改后提示生效条件；scheduler「启动关闭→运行期开启」改为明确提示需重启
- 工程化：全仓 gofmt 清零 + CI **gofmt 门禁**、CI setup-node 24、构建加 `-trimpath`；修正 README 文档漂移（prompt.mode 默认 passthrough、UA 默认 `WorkBuddy/5.5.4 … CLI/2.137.1`、仅 linux/amd64、请求体上限、出处表行号易漂移提示、失效的文件引用）

### 兼容性/行为变更（升级需注意）

- **镜像不带配置直接启动不再可用**：默认 `api_key` 为空 + 非回环监听 → fail-fast；请挂载/生成含随机 `api_key` 的 config.json（首启自动生成配置仍含随机密钥）
- **老配置使用 test_key 等弱密钥且非回环监听时升级会启动失败**，需改随机密钥（预期安全修复）
- 请求体超过 `server.max_body_mb` 返回 413（此前由上游或内存行为决定）
- 响应 JSON 无法序列化时由「200 空 body」改为「500 + 错误体」
- panel.Config 新增 `TaskSweepConcurrency`，config 新增 `schedule.task_sweep_concurrency`；`server.max_body_mb`；无移除字段

### 测试与验证

- 新增测试：弱密钥守卫/body 上限 413/pool AvailabilityView/persist 锁外/停机顺序/scheduler recover/credithist 锁外、panel 有界扇出与 16 项 P1、前端 XSS/超时/序号/日志降频/可访问性等；`go build` / `go vet` / `go test -count=1 ./...` 全绿（21 包）
- 审计报告：`D:\dsh\tmp\audit-core.md`、`audit-panel.md`、`audit-ops.md`、`audit-frontend.md`（不入库）
- QA 独立验证：见交付报告（`D:\dsh\tmp\verify-final-v260.md`）

---

## v2.5.0-pro（2026-10-24）

性能专项：针对 1k–10k+ 账号规模重构热路径（用户要求）。完整数据见 [PERF.md](PERF.md)。

### 10k 账号实测（同机，详见 PERF.md）

- 进程启动 **1,600ms → 600ms**；负载后 RSS **214MB → 92MB**；混合读吞吐 **327 → 4,132 req/s**；p95 **69ms → 5.9ms**
- `overview?lite=1` **12.5ms → 0.54ms**；`metrics` **38ms → 0.57ms**；`accounts` 分页 100 行 **31ms → 2.9ms**；ETag 304 **31ms → 0.57ms**；`accounts` 全部（2000 上限）**73ms → 24ms**
- 1k/5k：启动 296→~300ms / 881→416ms；吞吐 1,637→4,635 / 583→4,826 req/s

### 改动

- **internal/pool**：新增 `Snapshot() []Status`（缓存只读快照，uid 升序，命中 0 alloc）、`Summary()`（一次遍历聚合计数/积分/在途/模型锁，version+TTL 500ms 缓存）、`Version()`（内容代际，InFlight 例外）、`AvailableUIDsSorted()`；`List()` 保持旧拷贝语义；`ModelLockView` 空锁原子计数 O(1)；`statusOf` 共用 now/today 与模型台账单条目快路径
- **internal/panel**：overview/metrics/SSE 改用 `Summary()`（删除多次全池扫描）；accounts 改 `Snapshot()` + 索引过滤/排序/分页（默认 uid 零排序）；**ETag/304 LRU 缓存**（归一化 query→etag，pool/meta 代际失效 + 10s TTL 兜底，304 不构 rows）；gzip 缓冲 `sync.Pool`；新增 `overview?accounts=0` 轻量模式
- **internal/auth**：`LoadDir` worker pool 并行加载（结果稳定排序，语义不变）；`Realm()` 记忆化（atomic 快路径 + 写回自动失效）；auth 文件 Parse 去掉双重 Unmarshal
- **cmd/server**：启动分段计时日志（load auths / pool load+sync / ready）
- **工具**：新增 `tools/perfbench`（1k/5k/10k 可复现基准）+ pool/panel/auth 微基准

### 兼容性

- API 契约不变：accounts/overview/metrics 响应字段与 ETag 值保持兼容（ETag 值与优化前逐字节一致）；新增可选 `overview?accounts=0`
- 无配置变更；启动日志新增 3 行 `startup: …` 便于观测
- 已知剩余瓶颈（PERF.md 第 5 节）：全量 overview（默认含 accounts，10k 123ms/7.1MB，兼容保留）、`page_size=0` 上限 2000、`List()` 低频调用点、ModelLockView 有锁明细

### 测试与验证

- 新增测试：pool 缓存/版本语义 8 用例 + panel 优化 13 用例 + auth 并行/Realm/Parse 6 用例 + 3 组基准；`go build` / `go vet` / `go test -count=1 ./...` 全绿（21 包）
- 独立性能对比：tools/perfbench 1k/5k/10k（前后数据见 PERF.md）；QA 回归见交付报告（`D:\\dsh\\tmp\\verify-final-v250.md`）

---

## v2.4.1-pro（2026-10-24）

按用户要求移除 v2.4.0 试做的两个功能，保留 SSE 实时事件。

### 移除：多用户 / RBAC（#6）

- 删除 `POST /panel/api/auth/login`、`POST auth/logout`、`GET auth/info` 端点与全部角色门禁/session store/密码校验/`panel_login` 审计
- 删除 `panel.users`、`panel.session_ttl` 配置、`WB2A_PANEL_SESSION_TTL` env、前端「用户名/密码」登录/模式切换/角色禁用/退出按钮/用户名显示
- `withAuth` 恢复 v2.3.1 单密钥逻辑（与 `git show e7821c5:internal/panel/panel.go` 逐字节一致，仅保留 SSE 的 events 路由与 gzip 绕过差异）
- 旧 `config.json` 若含 `panel.users`/`panel.session_ttl`：JSON 解码忽略未知键，无需迁移；建议顺手删除

### 移除：凭证静态加密（#7）

- 删除 `internal/auth` 加密层（crypto.go/加密写回/密文解析）、`cmd/authcrypt` 命令、`security.encrypt_auths` 配置、`WB2A_MASTER_KEY`/`WB2A_ENCRYPT_AUTHS` env、启动加密装配与日志、compose 的 `WB2A_MASTER_KEY` 示例
- `internal/auth` 恢复 v2.3.1 明文行为；存量明文 auths 零影响
- 若曾按 v2.4.0 文档执行过 `authcrypt -encrypt`：请先恢复到明文再升级（用 v2.4.0 的 authcrypt `-decrypt`，或换回旧版镜像处理），否则 v2.4.1 无法读取密文 auth 文件；`.plain.bak`/`.enc.bak` 视需要清理

### 保留

- **SSE 事件推送（#3）**：`GET /panel/api/events`、`panel.events_enabled`、前端实时徽标/断线回落轮询全部保留，行为与 v2.4.0 一致
- 其余 v2.0-v2.3 功能零改动；`go build` / `go vet` / `go test -count=1 ./...` 全绿（19 包）

---

## v2.4.0-pro（2026-10-24）

安全与实时批次（UPGRADE_PROPOSALS #3 / #6 / #7 / #20 收尾）。

### #3 SSE 事件推送（替代轮询）

- 新增 `GET /panel/api/events`（withAuth）：`event: snapshot`（首帧立即 + 每 2s：now/now_unix/version/poll_interval_sec/counts/credits_sum/credits_total_sum/jobs 最近 10 条/events_enabled）、`event: jobs`/`event: accounts`（变化时推送）、15s 心跳 `: ping`
- Hub：包级懒创建、单实例最多 64 客户端、每客户端缓冲 16、慢客户端断开由重连 snapshot 自愈；无客户端时 hub 自动停止并摘除；`panel.events_enabled=false` → 404
- ServeHTTP 对 /panel/api/events 绕过 gzip 缓冲（事件立即 flush）
- 前端：fetch + ReadableStream 带 Bearer 解析 SSE（EventSource 不支持 header）；连接期间暂停 jobs/accounts 轮询、断线指数退避 1→30s、404/501 永久回落轮询；顶栏「实时/轮询」徽标

### #6 多用户 / RBAC（阶段一）

- 配置 `panel.users`（`{name,password|password_hash,role}`）+ `panel.session_ttl`（默认 12h）；空 users = 单密钥模式零变化
- 新增 `POST /panel/api/auth/login`（成功下发 32B token + HttpOnly `wb2_session` Cookie；失败统一 401 并沿用 20 次/分钟/IP 限速）、`POST auth/logout`、`GET auth/info`（mode/user/role/events_enabled）
- 集中式角色门禁：viewer=只读 GET（+logout）；operator=任务/账号运维写（checkin/balance/revive/disable/pause/resume、tasks、jobs、meta、usage）；admin=全部（config 读写、删号、重扫、导入、添加账号仅 admin）；`api_key` Bearer 始终 admin（脚本兼容）
- 安全加严：`GET /panel/api/config` 与 `POST accounts/rescan` 限 admin（前者回显含密钥/口令哈希，后者等价删号危险操作）
- 前端：启动读 auth/info 自动切换「密钥/账号密码」登录；顶栏显示用户名+角色与退出；按角色禁用/隐藏写操作与配置导航（渲染期 + 命令式双保险）

### #7 凭证静态加密（可选）

- `security.encrypt_auths` + `WB2A_MASTER_KEY`（32 字节 base64/hex）：AES-256-GCM 整文件包装（随机 nonce、0600 原子写）
- 存量明文兼容、混合目录可读；开启后所有写回（登录/刷新/realm 回填/导入）自动加密；密钥不可用时拒绝写回（不降级明文）；启动缺/非法密钥 fail-fast 并提示迁移
- 新增 `cmd/authcrypt`：`-encrypt|-decrypt [-config] [-dir] [-dry-run]`，保留 `.plain.bak`/`.enc.bak`，输出 summary、退出码 0/1
- LoadDir：密文解密失败逐文件 WARN（点名文件）；开启加密但仍有明文时汇总 WARN 提示迁移
- 启动日志输出主密钥指纹（sha256 前 8 字节）便于核对同一把密钥；docker-compose 增加 `WB2A_MASTER_KEY` 注释示例

### #20 移动端收尾

- 320px/375px 回归：多用户登录表单、实时徽标、用户名、标签筛选行、Job 进度条、审计/任务历史表格均无横向溢出

### 兼容性

- 默认全部关闭/不启用：不配 `panel.users` 不启用登录；`security.encrypt_auths=false` 维持明文；`panel.events_enabled=true` 仅新增 SSE（旧客户端不受影响）
- 旧配置零影响：新字段均有默认值；`api_key` 单密钥模式行为与 v2.3.1 一致
- 取舍（文档已注明）：Cookie 会话暂无 CSRF token；用户管理仍改配置重启；会话仅内存；SSE 单实例进程内推送；`scripts/*.py` 在加密后需先解密

### 测试与验证

- 新增/扩展测试：SSE 12 + RBAC 13 测试函数 + auth 加密 16 + authcrypt + 前端 4 组；`go build` / `go vet` / `go test -count=1 ./...` 全绿（21 包）
- Lead E2E 冒烟 29/29：加密启动/缺密钥 fail-fast/authcrypt 往返、多用户 login 三角色矩阵/403/logout/限速、SSE snapshot、单密钥兼容、version/frontend 标记
- QA 独立验证（HEAD `c7be4bd`）：**287 项检查全 PASS，P0=0 / P1=0**——verify-pro 28/28、v2.0.1/v2.1/v2.2/v2.3/v2.3.1 回归 28/42/25/24/28 全 PASS、新增 verify-v240 35/35（SSE headers/gzip 绕过/snapshot 字段/jobs 增量/心跳/双客户端/401/disabled 404；RBAC 单密钥零破坏+三角色矩阵+logout 双通道+失败 429+audit；加密 authcrypt 五场景+缺/错密钥 fail-fast+密文加载）、单测 auth 13/panel 30/前端 73 PASS、前端 harness 53/53（0 warning）、真实浏览器 11/11、320px/375px 各 13/13；详见 `D:\dsh\tmp\verify-final-v240.md`

---

## v2.3.1-pro（2026-10-24）

调度与体验批次（UPGRADE_PROPOSALS #11 完整优化、#18 阶段一、#12/#19 扩展）。

### #11 调度器完整优化

- **签到并发**：`schedule.checkin_concurrency`（默认 3，clamp 1–16，热生效）——签到批次改为账号级 worker pool，进度/取消线程安全；1146 号池串行约 1 小时 → 并发 3 后约 15–20 分钟
- **跳过当日已签**：进程内 uid→本地日期记录（成功或上游「已签到」幂等即标记）；同日手动/定时/补跑再跑跳过 checkin RPC（仍刷新余额/解冻）并计 skip；重启后内存清零（上游幂等，文档注明）
- **失败重试分层**：429 软限流由 scheduler 重试 1 次（防重试风暴，5xx/网络由 upstream 客户端既有有界重试，不叠加）；4xx 鉴权/凭证/业务直接 fail
- **按族解耦**：到点批次每族先抢单飞 gate 再独立 goroutine 执行，慢签到不再阻塞旅行/活跃/保活；补跑窗口/lastRun/单飞 409 语义保持
- **定时/补跑批次进入 Job 历史**：`trigger=schedule/catch_up` 的 Job 出现在 `GET /panel/api/jobs`、可取消（ctx 取消早停）、job_done Webhook 正常；失败不阻断调度

### #18 前端工程化（阶段一）

- 新增 `scripts/panel_frontend_check.mjs`（零依赖）：JS 语法解析、DOM id 双向契约、API 路径白名单（禁硬编码/绝对 URL/疑似 key）、体积预算（app.js<350KB、mobile.js<25KB、index.html<130KB）
- CI `go-binaries` test job 增加 `node scripts/panel_frontend_check.mjs` 门禁

### #12 扩展：标签服务端筛选

- `GET /panel/api/accounts?...&tag=<标签>`：按 account-meta 的 tags 大小写不敏感筛选，与 q/state/realm/enterprise AND；`tag` 已纳入 ETag（304 语义不变）
- `GET /panel/api/meta/tags`：`{tag,count}` 按 count 降序、同 count 字典序；内存索引（加载后 O(1) 查表），meta 变更热更新
- 账号池筛选栏新增「标签」下拉（最多 50 个、count 降序、无匹配空态提示），与分页/其它筛选组合

### #19 扩展：通知渠道

- `notify.channel`：`webhook`（默认，payload 不变）/ `wecom`（企业微信群机器人 markdown）/ `telegram`（Bot sendMessage，需 `notify.telegram_chat_id`）/ `bark`（iOS Bark）
- 纯文本概要（如 `[job_done] checkin 完成 100/120 失败 3 跳过 5`，≤2KB）；成功判定 webhook 2xx / wecom errcode==0 / telegram ok:true / bark 2xx；日志按渠道脱敏

### 兼容性

- 旧配置零影响：`schedule.checkin_concurrency` 默认 3、`notify.channel` 默认 webhook；telegram 渠道缺 chat_id 会启动报错（提示配置）
- Job API/旧批量端点/备注接口/ETag 语义只增不改
- 跳过当日已签仅进程内（重启后重打，上游幂等）；定时 Job 的 trigger 字段可选展示

### 测试与验证

- 新增测试：scheduler 11 + cmd/server 1 + panel tag 5 + notify 12 + 前端 4/扩展 3；`go build` / `go vet` / `go test -count=1 ./...` 全绿（21 包）
- 前端工程化检查：`panel frontend check: OK (app.js=269.4KB, mobile.js=12.6KB, index.html=94.8KB)`
- QA 独立验证（HEAD `0bd0826`）：**252 项检查全 PASS，P0=0 / P1=0**——verify-pro 28/28、v2.0.1 回归 28/28、v2.1 回归 42/42、v2.2 回归 25/25、v2.3 回归 24/24、新增 verify-v231 28/28（前端检查脚本/CI/标签筛选 E2E/meta tags/ETag/通知渠道 wecom+telegram+bark/缺 chat_id 启动失败/checkin_concurrency 热更新）、独立前端 harness 53/53（0 warning）、真实浏览器 11/11、320px/375px 各 13/13；调度器 #11 单测 23 PASS；详见 `D:\dsh\tmp\verify-final-v231.md`

---

## v2.3.0-pro（2026-10-24）

运维效能批次（UPGRADE_PROPOSALS #10 / #12 / #19，Wave 2A）。

### #10 全池批量任务 Job Runner（异步 / 进度 / 取消 / 持久化）

- 新增 `internal/jobs`：Job 状态机（pending/running/done/failed/canceled/interrupted）、**同 kind 单飞**、**Job 级并发闸门**（`jobs.max_concurrency`，默认 4）、取消、`data/jobs.json` 原子持久化（`jobs.retain` 默认 50，重启时上次 running → interrupted）
- 新增 API：`GET /panel/api/jobs`、`GET /panel/api/jobs/{id}`、`POST /panel/api/jobs/{kind}/run`、`POST /panel/api/jobs/{id}/cancel`
- 旧批量端点（checkin_all/travel_all/activity_all/keepalive_all/balance_all）改为**异步 Job**：保留 `ok/started` 并新增 `job_id`，同类在跑 409 语义不变
- scheduler 新增 `Run{Checkin,Travel,Activity,Keepalive,BalanceRefresh}Ctx(ctx, ProgressFunc)`：逐号进度上报、ctx 取消早停；旧 `Run*Now()` 行为不变；#11 补跑窗口/单飞 gate 语义保留
- 面板账号池顶部新增 **Job 进度条**：类型/进度/失败/跳过/取消（只停后续账号），无 running 时隐藏；取消/进度轮询与旧 API 兜底
- 单号 `tasks/run` 额外写一条 kind=tasks_run 的 Job 历史（同步响应契约不变）

### #19 Webhook 事件通知（最小闭环）

- 新增 `internal/notify`：异步单 worker 发送 `{"event","ts","data"}`（JSON POST），失败重试 1 次、队列满丢弃计数、Close 排空、URL 日志脱敏、64KB 摘要降级
- 事件：`job_done`（Job 终态）、`account_disabled`（面板人工禁用）、`checkin_failed`（单号签到失败）；空 `notify.webhook_url` = 关闭且所有调用 no-op
- 新增配置 `notify.webhook_url` / `notify.timeout`（默认 10s）/ `notify.events`（默认三件套），env `WB2A_NOTIFY_*`；停机时 notifier.Close(5s) 排空

### #12 账号备注/标签服务端化（部分）

- 新增 `data/account-meta.json`（0600 原子写、损坏隔离 .corrupt-*、懒加载）存储 `{note,tags,updated_at}`
- 新增 API：`GET /panel/api/meta`、`GET|PUT|DELETE /panel/api/accounts/{uid}/meta`；校验 note≤500 字符、tags≤10 个×32 字符、uid 白名单；账号删除时自动清理
- 面板备注弹窗改为服务端读写（localStorage 降级缓存保留）：保存按 diff 逐账号 PUT/DELETE、部分失败保留本地并提示；账号行显示 tags（最多 3 个 + N）
- **一次性迁移**：服务端 meta 为空且本机有旧备注 → 自动导入服务端（成功后清本地键、toast 提示）；服务端已有数据不覆盖
- 新增配置 `jobs.*`、`notify.*`（见 README 字段速查）

### 兼容性

- 旧批量端点响应字段只增不改（新增 job_id）；`/panel/api/tasks/run`、`/task_runs`、`/status` 等行为不变
- 备注迁移只发生在服务端 meta 为空的首次升级；迁移失败自动保留本机缓存（可重试）
- 定时/补跑批次暂不写入 jobs.json（手动批量 + 单号任务历史已覆盖）；账号级 worker pool 与 #11 调度器完整优化在 v2.3.1

### 测试与验证

- 新增测试：jobs 12 + scheduler 6 + panel jobapi/accountmeta 20 + notify 17 + cmd/server 1；`go build` / `go vet` / `go test -count=1 ./...` 全绿（21 包）
- QA 独立验证（HEAD `1e88282`）：**218 项检查全 PASS，P0=0 / P1=0**——verify-pro 28/28、v2.0.1 回归 28/28、v2.1 回归 42/42、v2.2 回归 25/25、新增 verify-v23 24/24（jobs API/409/404/cancel 语义/RFC3339/旧端点 job_id/jobs.json 落盘/重启 running→interrupted/meta CRUD 与校验/Webhook 收到 job_done 且无凭证/空 URL 关闭）、独立前端 harness 48/48（0 warning）、真实浏览器 11/11、320px/375px 各 12/12；详见 `D:\dsh\tmp\verify-final-v23.md`

---

## v2.2.0-pro（2026-10-24）

工程与可靠性批次（UPGRADE_PROPOSALS #5 / #8 / #9 / #13 子项 / #14 / #15 / #16 / #17 + v2.1 P2 修复）。

### #14 优雅停机（滚动升级不再断流）

- 收到 SIGINT/SIGTERM：停止接新请求 → `http.Server.Shutdown`（上限 `server.shutdown_timeout`，默认 30s，可配 5–120s）→ 后台任务收敛 → **最后** `pool.Flush`（修复旧实现 flush 在 Shutdown 前的顺序问题）→ usage/reqlog/session 收尾 → 退出码 0
- 二次信号立即强制退出（130）；日志含各阶段与 `graceful shutdown complete in Xms`
- compose `stop_grace_period: 35s`（30s 上限 + 5s 余量）

### #13 state.json 可靠性（子项）

- 主文件写成功后把上一版滚动为 `state.json.bak`（tmp+rename、0600；写失败不覆盖备份）
- 启动时主文件损坏 → 隔离为 `state.json.corrupt-<ts>` → 自动从 .bak 恢复并回写；无/坏备份时空池启动（不 fatal、不静默覆盖证据）
- 启动自检（WARN 非 fatal）：auth_dir/state 目录可写性（临时文件探测）、磁盘 <100MiB、chown/挂载修复提示（Windows kernel32 / Unix Statfs 双实现）

### #16 / #17 工具链与运行时

- **Go 1.22.5 → 1.24**（go.mod/Dockerfile/CI 跟随，`go mod tidy` 清理，五平台交叉编译通过）
- Dockerfile：`golang:1.24-alpine`、`COPY go.mod go.sum`（依赖层缓存稳定）；运行用户/HEALTHCHECK 保留
- CI：test job 增加 `go vet ./...`，setup-go 显式 cache
- compose：json-file 日志轮转（20MB × 3）+ `stop_grace_period: 35s`

### #15 配置安全（备份 / 回滚 / 危险值）

- 保存配置：校验 → 备份 `config.json.bak` → 原子写盘 → 热应用（校验失败不触碰备份；写失败备份仍是当前生效版）
- 新增 `POST /panel/api/config/rollback`：用 .bak 恢复并热应用，返回 `restart_required`；无备份/非法备份/危险值 400 且不改动当前配置
- 新增危险值拦截（空 auth_dir/state_file）、监听面变化审计日志

### #8 可观测（面板侧）

- `GET /panel/api/readyz`：config / auth_dir_writable / state_dir_writable / pool_loaded（失败 503）
- `GET /panel/api/metrics`：Prometheus text v0.0.4（手写零依赖）——`workbuddy_up`、`uptime_seconds`、`accounts_total{state=...}`、`in_flight`、`credits_sum`、`credits_total_sum`、`model_locks`、`panel_requests_total`

### #5 面板操作审计

- 路由级 `withAudit` 中间件统一埋点（26 个写操作动作，成功/失败都记），落 `data/panel-audit.jsonl`（0600、追加、mutex、写失败仅 WARN）
- `GET /panel/api/audit?limit=200`；面板「运行日志」页新增只读「操作审计」区块（点击拉取，不轮询）

### #9 任务执行历史

- 落 `data/task-runs.jsonl`：单号做任务/签到/余额、手动 `*_all`×5、成长队列、补领清扫（kind/trigger/起止/总数/完成/失败/跳过/错误）
- `GET /panel/api/task_runs?limit=100`；面板「运行日志」页新增只读「任务执行历史」区块

### 前端（含 v2.1 P2 修复）

- 账号增删/重扫/登录/导入后**后台补拉完整 overview**（积分历史下拉、积分条基准、到期快照即时更新）
- `page_size=0` 超 2000 行时显示截断提示（建议筛选/分页）
- 新增审计/任务历史只读 UI（移动端自动卡片化）

### 兼容性

- `/panel/api/config` 保存响应字段不变（备份/回滚为新增端点）；`/panel/api/overview`、`/status` 行为不变
- 新增配置键 `server.shutdown_timeout`（默认 `30s`，env `WB2A_SHUTDOWN_TIMEOUT`），老配置零影响
- 审计/任务历史/备份文件均为新增数据文件（data/ 下），不影响既有运行数据

### 测试与验证

- 新增测试：优雅停机 9 + 自检 11 + state 备份 5 + 配置备份 8 + 审计/JSONL/可观测/任务历史（5 文件 1000+ 行）+ 前端 3 组
- `go build` / `go vet` / `go test -count=1 ./...` 全绿（20 包）；Lead E2E 冒烟 14/14（readyz/metrics/配置保存 .bak/回滚恢复/审计动作/401）
- QA 独立验证（HEAD `0389299`）：**189 项检查全 PASS，P0=0 / P1=0**——verify-pro 28/28、v2.0.1 回归 28/28、v2.1 回归 42/42、新增 verify-v22 25/25（readyz 正常/失败、metrics 8 指标、审计留痕、config .bak/rollback/无备份 400、损坏 state+.bak 恢复与隔离、Go1.24/compose/CI 静态）、独立前端 harness 43/43（0 warning）、真实浏览器 11/11、320px/375px 各 12/12；详见 `D:\dsh\tmp\verify-final-v22.md`

---

## v2.1.0-pro（2026-10-24）

### #1 面板数据面重构（P0，v2.1 第一项）

- 新增 **`GET /panel/api/accounts`**：服务端筛选（`q` / `state` / `realm` / `enterprise`）+ 排序（`uid` / `credits` / `last_success` / `cool_remaining`）+ 分页（`page` / `page_size`，0=全部、上限 2000、超界钳末页）+ 字段投影（`fields=light` 默认，省略 `credits_expiry_buckets` / `cool_remaining_sec`；`full` 完整）；响应 `{ok,now,total,filtered,page,page_size,pages,rows}`
- **ETag/304**：弱 ETag（hash 覆盖归一化查询 + 稳定投影，`cool_remaining_sec` / `in_flight` / `breaker_fails` / `token_usage.last_used_at` 归零）；`If-None-Match` 命中 → `304` 空 body —— 无状态变化时轮询零解析/零渲染
- **`GET /panel/api/overview?lite=1`**：无 accounts 的轻量汇总（计数/元信息/model_locks/poll_interval_sec + 新增 `credits_sum` / `credits_total_sum`）；默认 overview 行为完全不变（只新增两个汇总字段，脚本零破坏）
- **前端**：账号池改服务端分页/筛选/排序（防抖 250ms、条件变化回第 1 页、请求序号丢弃过期响应、以响应 `page`/`pages` 为准）；轮询 = lite overview + 当前页 accounts（304 复用缓存）；「积分到期」tab 懒加载完整 overview（60s 新鲜窗口）；冷却倒计时改为绝对 `until` 本地计算 + 200 时钟偏差校准（**304 后倒计时继续走**）；统计条改用 `credits_sum`
- **性能**（估算；QA 实测见交付报告）：1146 账号下原「每 5s 689 KB 全量 overview」→「lite ~1–2 KB + light 单页 ~60 KB（gzip 后 ~8–15 KB），无变化时 304」

### 兼容性

- `/panel/api/overview`（不带 lite）与 `/status` 默认行为不变；新增字段只增不改
- 外部脚本如需全量账号仍可拉 `overview`；新脚本建议 `GET /panel/api/accounts`
- 前端账号池「全部」模式上限 2000 行（更多请配合筛选）；其它列表分页仍为纯前端

### 测试与验证

- 新增 `internal/panel/accounts_page_test.go`（10 个测试函数：分页边界/筛选 18 组/排序/light-full 投影/ETag 稳定与变化/304/易变字段规范化/lite/401/积分合计）
- 前端新增/调整 5+ node harness 测试（服务端参数映射、304 跨轮询倒计时、lite 数据流与懒加载、控件接线、credits_sum）
- `go build` / `go vet` / `go test -count=1 ./...` 全绿（20 包）
- QA 独立验证（HEAD `544f7cc`）：**157 项检查全 PASS，P0=0 / P1=0**——verify-pro.ps1 28/28、verify-v21.ps1 42/42（25/300 伪造账号池的分页/筛选/排序/投影/ETag 304/lite/量化）、v2.0.1 回归 28/28、独立前端 harness 38/38（0 warning）、真实浏览器 10/10、320px/375px 各 11/11
- **量化收益**：#1 实测——25 账号 full overview 12,192 B vs lite 335 B（2.75%）；300 账号 full 139,444 B vs lite 341 B（0.24%）、light 100 行页 46,312 B（gzip 1,454 B）；按 1146 账号投影，旧版每 5s 全量轮询约 532 KB（gzip 约 14 KB）→ 新版 lite 约 341 B + 页 gzip 约 1.5 KB，**约 99.7% 流量下降**，且无状态变化时 304 零解析

---

## v2.0.1-pro（2026-10-24）

P0 优化批次（对应 `UPGRADE_PROPOSALS.md` 的 #2 / #4 / #10 子项 / #11 子项）。

### ⚠️ 升级注意（行为变更）

- **非回环监听 + 空密钥将拒绝启动**：`listen` 不是 127.0.0.1/::1/localhost 且 `api_key`、`panel.api_key` 都为空 → 启动失败（`security.allow_anonymous: true` 可显式放行）。Docker / 公网部署请务必配置 key；本机回环调试不受影响。
- **`balance_all` 改为异步**：立即返回 `{ok:true,started:true}`，不再同步等待全池刷新完成，也不再返回 `accounts`；外部脚本需要全量账号请改拉 `GET /panel/api/overview`。
- **批量端点单飞**：`checkin_all` / `travel_all` / `activity_all` / `keepalive_all` / `balance_all` 同类正在执行时返回 `409`（旧版会并发叠跑两轮）。
- 建议分离「面板密钥」与「网关密钥」：设置 `panel.api_key` 后，下游 API 客户端只拿 `api_key` 无法调用面板管理接口；不设置时保持旧行为（回落共用）。

### #2 面板性能（P0）

- 面板响应 **gzip**（JSON/HTML/JS/CSS，body >1KB；`Vary: Accept-Encoding`；`/v1/*` 与 SSE 不受影响）——overview 689 KB 的传输量预计降一个数量级
- 静态资源 **SHA-256 ETag + Cache-Control**：`app.js`/`mobile.js`/`mobile.css` 强 ETag 可 304；`index.html` 弱 ETag + `no-cache` 保证改版即更新
- 新增 `X-Robots-Tag: noindex`
- 压缩阈值 1KB：小响应（空池 overview、401/429）有意不压缩；大池 overview 与 app.js 等静态资源才明显受益（实测 app.js 222 KB → 74 KB wire）
- 前端**轮询治理**：间隔由 `overview.poll_interval_sec` 下发（默认 5s，2–60，可配置）、页面隐藏暂停 / 恢复立即刷新、失败退避 5→10→30→60s、±20% 抖动、401 停止自动重试且密钥框只自动弹一次

### #4 安全（P0）

- `panel.api_key` 与网关 `api_key` **分离**（空 = 回落共用；改动需重启；面板配置页可编辑）
- **空密钥非回环启动阻断** + 保存配置拒绝清空 `api_key`（`security.allow_anonymous` 逃生门）
- 面板鉴权失败按 IP **限速**：20 次/分钟 → `429 + Retry-After`（成功一次清桶，内存 LRU 1 万 IP）
- 面板配置页新增 7 个字段：`panel.api_key` / `panel.poll_interval_sec` / `security.allow_anonymous` / `schedule.catch_up_minutes` / `schedule.balance_refresh_concurrency` / `schedule.balance_refresh_startup_delay` / `schedule.balance_refresh_startup_force`

### #10 子项：批量操作闸门（P0）

- `RunBalanceRefreshNow` 改为**有界并发**（`schedule.balance_refresh_concurrency`，默认 8，1–32，热生效），彻底移除 1146 无上限 goroutine
- **启动首刷保护**：延迟 `schedule.balance_refresh_startup_delay`（默认 30s）后执行；账号数 > 200 且 `balance_refresh_startup_force=false` 时跳过启动首刷（周期刷新照常）
- 5 个 `*_all` 批量端点**同类单飞**（手动/定时共用 gate）：执行中重复触发 → 409
- `balance_all` **异步化**（立即返回，后台跑）

### #11 子项：调度补跑（P0）

- 新增 `schedule.catch_up_minutes`（默认 120，0=关）：批次跨过后续时点 / 重启错过时点后，若窗口内存在今天错过的计划时点且该族今天未成功执行 → 自动补跑，只补一次/天/族（修复「签到批次跑 1 小时吞掉 10:00 活跃 / 22:00 保活 / 23:00 夜猫子」）
- 手动/定时任务共用单飞 gate，不再叠加两轮全池任务
- `RunCheckin` / `RunTravel` 内部等待改为 ctx 感知（为优雅停机铺垫）

### 测试与验证

- 新增测试函数 54 个（后端 A 28 + 后端 B 18 + 前端 8）；`go build` / `go vet` / `go test -count=1 ./...` 全绿（20 包）
- QA 独立验证（HEAD `55963fb`）：**107 项检查全 PASS，P0=0 / P1=0**——verify-pro.ps1 28/28、verify-v201.ps1 28/28（gzip/ETag/304、安全三件套、429 限速、异步 balance_all、改名/补偿残留 0）、独立前端 harness 30/30（0 warning）、真实浏览器 10/10、真实 320px/375px 各 11/11；详见 `D:\dsh\tmp\verify-final-v201.md`

---

## v2.0.0-pro（2026-10-24）

首个 Pro 版本。基数 = 上游最终版 `73a7560` + 原 fork 面板定制（合并提交 `f9e654c`）。

### 更名（breaking，部署需同步调整）

| 项目 | 旧 | 新 |
|---|---|---|
| 仓库 | `Lei-rr/workbuddy2api-panel` / `linguo2625469/workbuddy2api-panel` | `Lei-rr/workbuddy2api-pro` |
| Go module | `github.com/linguo2625469/workbuddy2api-panel` | `github.com/lei-rr/workbuddy2api-pro` |
| GHCR 镜像 | `ghcr.io/linguo2625469/workbuddy2api-panel` | `ghcr.io/lei-rr/workbuddy2api-pro` |
| 容器名 | `workbuddy2api` | `workbuddy2api-pro` |
| 网关身份（`/healthz` 的 `service` 字段 / `X-Service` 头） | `workbuddy2api` | `workbuddy2api-pro` |
| 版本号 | `1.13.0-panel` | `2.0.0-pro` |
| 发布产物 | `wb2api-panel-v*.zip/tar.gz` | `wb2api-pro-v*.zip/tar.gz` |
| 面板标题 | `WorkBuddy2API · 控制台` | `WorkBuddy2API Pro · 控制台` |

> 迁移：新镜像挂载原有 `auths/`、`data/`、`config.json` 即可，配置 schema 未变；宿主机若有健康探测/运维脚本按旧 `service` / 容器名判据，需要同步改名。

### 删除：官方补偿（`compensation`）整个功能

按需求「删除最后一版新增的领取补偿 500 积分相关前后端」，且**连后台签到时的静默领取一起删**——网关不再调用 `claim-compensation`：

- `internal/upstream/compensation.go`（`GetCompensationStatus` 等）删除
- `internal/upstream/blackcat.go` 的 `ClaimCompensation` 删除（`ClaimGift` 新手礼包保留）
- `internal/scheduler/compensation.go` 删除；`streak.go` 中补偿调用移除（礼包调用保留）
- `internal/panel/panel.go`：`GET /panel/api/compensation`、`POST /panel/api/accounts/{uid}/compensation`、`POST /panel/api/compensation_all` 三条路由及 handler 删除
- 前端：面板「官方补偿」卡片、领取按钮、倒计时渲染、`compCache` 等在途刷新全部删除
- 收益：打开面板不再逐号请求上游（旧版在 1000+ 账号池下会显著拖慢首屏并触发上游风控）

### 新增：账号列表「做任务」按钮 + 接口

- 账号池每行新增「**做任务**」按钮（企业版账号不渲染，与「签到/任务」一致）
- 新接口：`POST /panel/api/accounts/{uid}/tasks/run`
  - 对该账号执行全部可自动任务（复用既有 `auto_all` 执行器：接受 → 执行 → 回读 → 自动领奖）
  - 响应：`{ok, uid, results:[{task_code,status,message}], summary:{total,done,skipped,error,claimed}}`
  - 状态码：`200` 正常（`ok:true`）；未知 uid `404`；企业版 / 企业切换会话 / 无凭证 → `200 + ok:false + message`（不发起上游请求）；同账号已有任务动作在跑 `409`；执行超时 `504`（后台继续跑）
  - 同步返回，可能耗时数分钟（与既有 auto_all 一致）；响应形状恒定，前端可无条件读 `results`/`summary`
- 原「任务」按钮的详情弹窗（全部接受 / 一键完成可自动任务）保留不变

### 新增：列表分页 + 筛选补齐（纯前端，不改变接口协议）

1000+ 账号池下所有长列表默认只渲染当前页，5s 轮询刷新后筛选与页码保持不变：

| 列表 | 筛选 | 分页 |
|---|---|---|
| 账号池 | 关键词（昵称/UID）、状态、版本 | 50 / 100 / 200 / 全部 |
| 积分到期 | — | 50 / 100 / 200 / 全部（独立分页） |
| 运行日志 | 频道 + 关键词 | 50 / 100 / 200 / 全部 |
| 请求日志 | 关键词（IP/UA/模型/账号/请求 ID）、结果、时间范围 | 50 / 100 / 200 / 全部 |
| 积分历史 | 账号 | 50 / 100 / 200 / 全部 |
| 模型锁池 | 模型关键词、状态、地区 | 分页 |
| 任务中心队列 | 状态、账号关键词 | 分页 / 分批渲染 |

### 保留的原 fork 定制（自 `6b31db6` 基线起）

- 账号池客户端筛选 + 分页、三 tab（账号池 / 模型锁池 / 积分到期）、到期列表独立分页
- 单号「积分」按钮 + 单号积分构成弹窗（整池全量查询入口保持封闭，降低上游风控）
- 账号池「重新扫描」按钮（`auths/` 目录热对齐，扫到 0 文件时拒绝执行防误清池）
- `/panel/api/packages` 收紧为单号必填（无 uid → 400）
- 用量页时间范围默认「今天」；CI 仅构建 linux/amd64

### 文档与工程

- README 改为 Pro 版说明；新增本 CHANGELOG 与 `MAINTAINERS.md`（接手/部署/排障手册）
- 新增 `UPGRADE_PROPOSALS.md`：15-20 条后续优化建议（性能 / 安全 / 可观测 / 账号运维 / 可靠性 / 工程质量 / 体验），按 P0/P1/P2 分级
- 质量门禁（2026-10-24 冻结验证）：`go build ./...` · `go vet ./...` · `go test -count=1 ./...`（20 个包全绿）通过；`node --check app.js/mobile.js` 通过
- 新增测试：Go 7 个（`tasks/run` 路由/未知 uid/企业版/无凭证/账号忙 409/summary 计数 + 账号池三 tab 结构契约）+ 前端 node harness 11 个（分页边界、筛选计数、补偿移除反向守卫、做任务按钮与交互 5 分支、任务队列筛选、控件接线、渲染器冒烟）；接线测试当场抓到并修复「队列分页控件 id 与 HTML 不一致」的真 bug
- QA 独立验证 `verify-pro.ps1`：26 项全 PASS（本地起服务：`/healthz` 身份与 X-Service、面板标题、overview version=2.0.0-pro、无 token 401、补偿路由 404、tasks/run 未知 uid 404、安全响应头、日志无 panic）
- 保留并锁定账号池三个二级 tab（账号池 / 模型锁池 / 积分到期）：`tabs_test.go` 钉住结构与顺序，QA 冒烟 C11 + 真实浏览器点击双重验证
- QA 复核后修补：窄屏移动端「做任务」按钮文案裁切（`ACT_SHORT` 增加 3 字映射 + 映射优先于长度阈值）、进行中按钮被 5s 轮询重渲染后误恢复可点击（`dotaskBusy` 集合）、README 请求体上限文档自相矛盾（8 MiB vs 不限 / WB2A_MAX_BODY_MB 已于上游移除）
- 修复后复验（HEAD `2c5e012`）：`verify-pro.ps1` **26/26 PASS**；独立前端 harness **22/22 PASS、0 warning**（含「轮询后执行态保持」）；真实浏览器 DOM/CSP **9/9 PASS**；真实 320px 移动端 **10/10 PASS**（含按钮不裁切、执行中 disabled + 短标签、三 tab 真实点击）；GitHub Actions `docker-ghcr` / `go-binaries` 均 success，GHCR 镜像可匿名拉取（tags: latest / main / sha-*）
- 旧 fork 的 `LOCAL_CHANGES.md` 内容并入 [MAINTAINERS.md](MAINTAINERS.md) 并删除该文件

---

## 上游最终版吸收内容（旧基线 `6b31db6` → `73a7560`，60 个提交）

本仓库起点已完整包含上游最终版全部提交；相对 6b31db6 的主要更新：

### 新能力

- **联网搜索端点** `POST /v1/search/messages`（Anthropic Messages 兼容形态，走上游 `/agenttool/v1/search`；含请求日志记账与 review 修复）
- **手机端适配**：抽屉侧栏、宽表卡片化、窄屏时序图重画（`mobile.css` / `mobile.js` 独立文件）
- **运行日志「积分历史」**：新包 `internal/credithist` + `cmd/credit`，逐账号记录余额变动
- **到期提醒快照与「检查」同口径**：`credits_expiry_buckets` 按自然日聚合写入 state.json
- **账号备注**（本机 localStorage，账号池与用量页同步）+ 到期卡「收起」折叠
- **企业切换会话门控**：`token_source=enterprise_switch` 识别，行为类成长任务跳过并提示重登
- **按地区独立配置轮询 / 顺序耗尽调度**（`pool.strategy.*` / `pool.account_order.*`）
- **`models.hidden` 模型黑名单**（列表过滤 + 对话门控）、裸模型名跨域回退、Global 模型可用性展示
- **配置新增**：`cn.chat_base`、`dns_servers`（`WB2A_DNS`）、出站代理走 `ProxyFromEnvironment`

### 修复 / 稳定性

- 同账号 token refresh 单飞（避免 RT 复用撞 12153 掉登录）
- 6021 `queue.full` 归为通道级忙、不再整池冷却；11158 模型权限错误识别；国际版 11-128 通道校验分流、始终发送 `X-Conversation-ID`
- `reasoning_text` 片段从 content 剥离（修 Codex 转换后 11101）
- 任务补领清扫（issue #174：周期幂等补领「已达标未领」奖励）；「一键完成」遇已达标未领先补领
- 模型级 6004 双时钟展示；软冷却到期不再透出过期 reason；缓存告警假阳性修复
- Dockerfile Go 下载源改为 `goproxy.cn`

### 上游最终版新增、但本 Pro 版已删除

- **官方补偿可观测（issue #189）**：面板「官方补偿」卡片 + `compensation-status` 查询 + 逐号/全量领取 + 签到排程「先查后领」——见上文「删除：官方补偿」。

---

## 版本路线图（建议，见 UPGRADE_PROPOSALS.md）

- **v2.1**：`/overview` 增量/ETag 或服务端分页、任务执行历史与审计、后台任务异步队列 + 进度（避免 1-2 分钟同步请求）
- **v2.2**：面板多用户/RBAC 与操作审计、凭据加密落盘、Prometheus `/metrics`、备份恢复命令、通知渠道（到期/失败告警）
