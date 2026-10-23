# WorkBuddy2API Pro 维护手册（接手先读这页）

> 本文件替代旧 fork 的 `LOCAL_CHANGES.md`：记录 Pro 版的基线、决策、部署链路与排障要点。
> 新会话接手时先读这一页，**不要推翻已有决策**。

## 基线

- 原始项目：[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)（已删库）
- 上游最终版：[`linguo2625469/workbuddy2api-panel`](https://github.com/linguo2625469/workbuddy2api-panel)，基线 commit **`73a7560`**（2026-10-21 发布「停止维护声明」后停更）
- 本仓库起点 = 上游最终版 + 原 fork 面板定制合并（合并提交 `f9e654c`），已验证：`git diff 73a7560 f9e654c` 只差 8 个文件，**上游最终版 60 个提交零丢失**
- 本仓库：**`Lei-rr/workbuddy2api-pro`**，版本 **`2.3.1-pro`**
- 原则：**只做增量**；上游停更后演进以本仓库为准

## 目录速览

```
cmd/server          网关入口（appVersion、装配、配置加载）
cmd/{login,signin,credit,trial}  账号/签到/积分历史/试用等 CLI
internal/server     转发核心：handler、日志、模型解析、搜索端点
internal/upstream   上游协议客户端（chat/billing/growth/tasks/desktop…）
internal/pool       账号池：选号、权重、冷却、熔断、持久化
internal/scheduler  定时任务：签到 / 旅行 / 夜猫子 / 连登 / 成长任务
internal/panel      管理面板（Go 后端 + go:embed 前端 app.js/index.html/mobile.*）
internal/reqlog     请求 JSONL 归档
internal/credithist 积分历史
internal/httpauth   Bearer 鉴权（常量时间比较）
scripts/            运维脚本（探测/任务 CLI 辅助）
.github/workflows   CI：go-binaries（测试+五平台产物）、docker-ghcr（镜像）
```

## 本地开发环境（Windows 参考）

- Go：本机在 `D:\dsh\tmp\go\go\bin`（`go version` = 1.24.5；go.mod 仍声明 1.22.5）
- Node：24（`internal/panel` 的前端测试由 Go 调 `node` 执行，需要 node 在 PATH）
- 常用命令（PowerShell）：

```powershell
$env:PATH="D:\dsh\tmp\go\go\bin;$env:PATH"
$env:GOPATH="D:\dsh\tmp\gopath"; $env:GOCACHE="D:\dsh\tmp\gocache"; $env:GOFLAGS="-mod=mod"
go build ./...
go vet ./...
go test -count=1 ./...          # 含前端 node harness（panel 包约 30s）
node --check internal/panel/app.js
node --check internal/panel/mobile.js
```

质量门禁：以上全部通过才算可提交。

## 版本关键决策

1. **更名**：Go module `github.com/lei-rr/workbuddy2api-pro`；镜像 `ghcr.io/lei-rr/workbuddy2api-pro`；容器名 `workbuddy2api-pro`；`/healthz` 的 `service` / `X-Service` 为 `workbuddy2api-pro`；面板标题 `WorkBuddy2API Pro`；版本 `2.0.0-pro`；发布产物 `wb2api-pro-v*`。
2. **官方补偿整个删除**（用户明确要求，含后台静默领取）：网关不再调用 `claim-compensation`。不要重新引入（`ClaimGift` 新手礼包属另一功能，保留）。
3. **分页/筛选为纯前端**：`overview` / `request_logs` 等接口协议未改，5s 轮询仍全量取数；服务端分页/增量推送列入 `UPGRADE_PROPOSALS.md`。
4. **单账号「做任务」**：`POST /panel/api/accounts/{uid}/tasks/run` = 复用 `auto_all` 执行器，返回 `results + summary{total,done,skipped,error,claimed}`。
5. **保留原 fork 定制**：账号池筛选/分页、三 tab（账号池/模型锁池/积分到期）、到期独立分页、单号积分弹窗、重新扫描按钮、`packages?uid=` 单号必填。
6. **v2.0.1 P0 优化**（2026-10-24）：面板 gzip/ETag/轮询治理；`panel.api_key` 与网关 key 分离 + 空密钥非回环阻断 + 鉴权失败限速；余额刷新有界并发（默认 8）+ 启动首刷保护（>200 号默认跳过）；5 个 `*_all` 单飞（409）且 `balance_all` 异步；调度补跑窗口 `schedule.catch_up_minutes`（默认 120）。
7. **v2.1.0 账号池服务端分页**（2026-10-24）：新增 `GET /panel/api/accounts`（筛选/排序/分页/light 投影 + ETag/304）与 `overview?lite=1`；前端账号池轮询改为 lite + 当前页，完整 overview 仅页面加载/到期 tab 懒加载；默认 overview 与 `/status` 保持兼容。**后续改前端分页逻辑必须走服务端参数，不要再恢复全量 overview 轮询**。
8. **v2.2.0 工程与可靠性**（2026-10-24）：优雅停机 + `server.shutdown_timeout`；state.json 滚动 .bak/损坏回退/启动自检；Go 1.24；CI vet + compose 日志轮转/stop_grace_period；配置保存 .bak + `POST /panel/api/config/rollback`；`/panel/api/readyz`、`/panel/api/metrics`；操作审计 `data/panel-audit.jsonl`、任务历史 `data/task-runs.jsonl`。**新增数据文件会随运行增长（当前未做自动 rotate/prune，见已知注意点）**。
9. **v2.3.0 运维效能**（2026-10-24）：`internal/jobs` Job Runner（异步/进度/取消/data/jobs.json/单飞/Job 级并发闸门）；`internal/notify` Webhook 通知（job_done/account_disabled/checkin_failed）；备注/标签服务端化 `data/account-meta.json` + 前端迁移；旧批量端点新增 job_id 兼容。**Job Runner 的并发闸门是「同时执行的 Job 数」，账号级并行度仍由各任务控制**。
10. **v2.3.1 调度器完整优化**（2026-10-24）：签到 worker pool 并发（`schedule.checkin_concurrency` 默认 3）+ 跳过当日已签（进程内，重启重打）+ 429 重试分层 + 按族解耦（慢签到不阻塞其它族）；定时/补跑批次进入 Job 历史（trigger=schedule/catch_up，可取消）；标签服务端筛选（accounts tag + meta/tags 索引）；通知渠道 wecom/telegram/bark；`scripts/panel_frontend_check.mjs` 入 CI。**改调度语义时先看 checkin11_test.go 的分层/跳过/并发断言**。

## 部署链路

- 镜像：`ghcr.io/lei-rr/workbuddy2api-pro:latest`（GitHub Actions `docker-ghcr` 自动构建，仅 linux/amd64）
- 容器名：`workbuddy2api-pro`；端口默认 `7863`（对外常见映射如 `7800:7863`）
- 运行数据（**不在仓库里，升级时必须保留**）：`auths/`、`data/`、`config.json`
- 从旧 `workbuddy2api-panel` 迁移：停旧容器 → 部署目录改 `image:` / `container_name:` → `docker pull ghcr.io/lei-rr/workbuddy2api-pro:latest` → `docker compose up -d`。配置 schema 未变，无需改 `config.json`；宿主健康探测若按旧 `service == "workbuddy2api"` 判据需同步改名。
- **v2.0.1 升级注意**：① 若 `listen` 为非回环（如 `:7863`）且 `api_key` / `panel.api_key` 均为空，新版本会**拒绝启动**——先配置 key（或 `security.allow_anonymous: true`，或改回环监听）；② 建议设置 `panel.api_key` 把面板密钥与网关 API key 分开；③ `balance_all` 已异步化、批量端点会返回 409，外部脚本/监控需适配；④ 新增 7 个配置键（见 `config.example.json` 与 README「字段速查」）。
- **v2.2.0 升级注意**：① 停机流程改为 SIGTERM 优雅收尾（compose `stop_grace_period: 35s`，`server.shutdown_timeout` 默认 30s）——`docker restart`/`compose up -d` 会先等在途请求；② 配置保存会生成 `config.json.bak`，可用 `POST /panel/api/config/rollback` 回滚；③ 新增 `data/panel-audit.jsonl` / `data/task-runs.jsonl`（0600）与 `state.json.bak`/`state.json.corrupt-*`，备份时把整个 `data/` 一起打包即可；④ Go 工具链升至 1.24（二进制/镜像重新构建）。
- **v2.3.0 升级注意**：① 批量按钮/脚本响应新增 `job_id`（旧字段保留），UI 顶部出现 Job 进度条；② 备注/标签迁到服务端 `data/account-meta.json`（本机旧 localStorage 备注首次打开自动迁移，失败保留本地）；③ 新增 `data/jobs.json`；④ 如配置 `notify.webhook_url`，会收到 job_done/account_disabled/checkin_failed 事件（空=关闭）。
- **v2.3.1 升级注意**：① 签到默认并发 3（可配 1–16），大池单轮显著变快；同日重复跑会跳过已签（重启后重打，上游幂等）；② `notify.channel=telegram` 必须配 `notify.telegram_chat_id`（否则启动报错）；③ 定时/补跑批次现在也会出现在任务进度/历史里（trigger=schedule/catch_up），面板可取消；④ CI 新增前端检查脚本，本地可跑 `node scripts/panel_frontend_check.mjs`。

参考 `docker-compose.yml`（仓库内已改名）：

```yaml
services:
  wb2api:
    build: .
    container_name: workbuddy2api-pro
    restart: unless-stopped
    user: "${PUID:-10001}:${PGID:-10001}"
    environment: [TZ=Asia/Shanghai]
    ports: ["7863:7863"]
    volumes:
      - ./auths:/app/auths
      - ./data:/app/data
      - ./config.json:/app/config.json
    stop_grace_period: 35s
    logging:
      driver: json-file
      options: { max-size: "20m", max-file: "3" }
```

**发布流程**：`git push origin main` → GitHub Actions 构建并推送 GHCR（约 2-3 分钟）→ `docker pull` → `docker compose up -d`。
新仓库注意：① 首次推送后到 Actions 确认 `docker-ghcr` workflow 已运行（新仓库 Actions 默认启用）；② 首次发布后需把 GHCR package 可见性设为 Public（否则拉取要 `docker login ghcr.io`）。

**打 tag 发布二进制**：`v2.0.0`（与 `appVersion` 去掉 `-pro` 后一致）→ `go-binaries` workflow 出五平台产物 + Release。

## 账号热更新

面板「重新扫描」按钮 = `auth.LoadDir` + `pool.SyncToDir`：外部脚本往 `auths/` 丢/删凭证后点一次即热对齐（只增删账号，已有账号积分/冷却状态保留）。后端对「扫到 0 个文件而池非空」返回 409 拒绝，防挂载丢失误清池。

## 实测基线（原 fork，1146 账号，供回归对照）

- `/panel/api/overview`：约 689 KB / 13 ms（v2.0.x 旧轮询口径；v2.1.0 起账号池不再每 5s 拉它）
- v2.1.0 轮询口径：`overview?lite=1`（实测约 330–340 B）+ `accounts?page=..&page_size=100&fields=light`（实测约 46 KB / gzip 约 1.1–1.5 KB；无状态变化时 304 空 body）
- v2.1.0 量化：25 账号 full overview 12,192 B → lite 335 B；300 账号 full 139,444 B → lite 341 B；1146 账号投影下旧版 5s 全量轮询约 532 KB（gzip ~14 KB）→ 新版约 1.8 KB/轮询（约 -99.7%）
- 容器：CPU 约 0.04%，内存约 16.4 MB
- `accounts/rescan`：`scanned=1146 added=0 removed=0`，约 160 ms
- `packages?uid=<真实>` 200 / 未知 404 / 无 uid 400

## 已知注意点 / 坑

- 页面顶部「刷新」按钮 = `balance_all`（v2.0.1 起**异步 + 有界并发**，默认 8，可配 `schedule.balance_refresh_concurrency`）：大池下一次点击仍会向每个账号发起一次余额 RPC（约 1146 个，受并发上限约束），非必要不点；重复点击会 409。启动首刷延迟 30s，账号数 > 200 时默认跳过（`schedule.balance_refresh_startup_force` 可强制）。
- 签到是**串行**执行，1146 号一轮约 1 小时；新增账号较多时注意排程窗口。
- `state.json` 会落后于池（官方设计：新增账号不置 dirty，只有剔除才落盘）；重启后按零值重建，不影响使用。
- `auths/` 若出现属主混杂（如部分 `root:root`，容器以 10001 运行），可能 token 刷新失败：`chown -R 10001:10001 auths`。
- 面板只有单 `api_key` 鉴权，**公网部署必须置于 HTTPS 反代之后**并加访问限流；`auths/` 内是明文 access/refresh token，勿泄露。
- 单号积分构成弹窗为按需 1 次上游请求；整池全量查询入口已刻意封闭，不要恢复成页面自动全量查询（1000+ 账号时会触发风控）。
- 账号池已是**服务端分页/筛选**（v2.1.0）：改 `renderAccounts`/`accQueryString` 时保持「以响应 page/pages 为准」与 ETag/304 缓存语义；其它列表（运行日志/请求日志/积分历史/模型锁池/任务队列）仍为纯前端分页。
- 账号池 `light` 投影没有 `cool_remaining_sec`（秒级易变字段不参与 ETag）：前端倒计时用 `until`/`breaker_until`/`degrade_until` 本地算，不要依赖该字段。
- 完整 overview 仅页面加载后台补一次 + 「积分到期」tab 按 60s 新鲜窗口拉取；列表 tab 不要再拉全量。
- **已知 P2（v2.2 待修）**：`poolAccountList()` 优先用完整 overview 快照，账号增删后积分历史下拉/积分条基准可能滞后（建议增删号后后台补拉一次全量）；`page_size=0` 在 >2000 账号池会静默截断 2000（前端应提示改用分页）。
- v2.0.1 面板 gzip 有 **1KB 阈值**：空池 overview / 401 / 429 等小响应不压缩属预期；大池 overview 与静态资源才受益（实测 app.js 222 KB → 74 KB wire）。
- v2.0.1 鉴权失败限速是**进程内 best-effort**（重启清零；XFF 可伪造分散计数），依赖可信反代；如需强防护需外置 WAF/反代限流。
- v2.2.0 `data/panel-audit.jsonl` / `data/task-runs.jsonl` **目前只追加不轮转**：写操作/任务批次每行约 100-300B，按日任务量评估（当前量级年增长 <10MB），后续 wave 可接 reqlog 的 rotate/prune 模式；磁盘紧张时手动清理旧文件即可（运行日志 ring 不受影响）。
- v2.2.0 state 备份：`state.json.bak` 保留 1 份；损坏主文件会被隔离为 `state.json.corrupt-<ts>`，确认无需抢救后可删除这些文件。
- v2.2.0 优雅停机依赖容器编排发 SIGTERM：`docker stop`/`compose down` 默认即 SIGTERM；`docker kill` 仍会硬杀（不做优雅收尾）；Windows 本机不便注入信号，进程级验证以单测 + Linux 容器 `docker stop` 为准。
- v2.2.0 `/panel/api/readyz` 与 `/metrics` 均需面板 Bearer（Prometheus 用 `authorization.credentials`、K8s probe 用 `httpHeaders` 注入 key）；readyz 的目录可写检查会**创建并删除临时文件**，只读挂载（`read_only: true`）下会报不可写——这是有意区分于 `/healthz`（只判进程存活）。
- v2.2.0 配置回滚是「恢复到 `config.json.bak`」单步操作、**没有 redo**：当前生效配置不会先另存，若回滚后又想回滚回滚前状态需手动保存或依赖 .bak 单份覆盖语义。  
- v2.2.0 `state.json.corrupt-<ts>` 隔离文件**不会自动清理**（保留现场供抢救），确认无用后可手动删除。
- v2.3.0 `data/jobs.json` / `data/account-meta.json` 与审计/任务历史一样**目前只追加/覆盖不轮转**（jobs 按 retain 上限淘汰终态条目，meta 每账号一条）；jobs 运行中不会被淘汰。
- v2.3.0 Job Runner 进度语义：`done`=已处理账号数（含失败、不含跳过），前端展示进度 = `done+skip`；取消只停后续账号、当前账号会跑完。
- v2.3.0 定时/补跑批次**不会**出现在 `/panel/api/jobs`（仅手动批量/单号任务）；定时批次记录仍在 task-runs.jsonl（批量统计随后续 scheduler 观察者增强）。
- v2.3.0 备注/标签是完整 uid 权威存储；旧 localStorage 的 uid8 键只有能解析到唯一完整 uid 时才迁移，解析不到只留本地（标记 legacy）。
- v2.3.1 「跳过当日已签」是**进程内**记录：重启/滚动升级后当天会重新打一轮 checkin（上游幂等，无副作用）；没有持久化到 state.json 是有意取舍。
- v2.3.1 重试分层：429 由 scheduler 重试 1 次；5xx/网络由 upstream 客户端既有重试；4xx 不重试。**不要在两处同时加重试**（会 3×2 放大）。
- v2.3.1 标签筛选匹配大小写不敏感；`GET meta/tags` 展示拼写=uid 序首个出现的原始拼写（稳定但非最近编辑值）；暂不支持「无标签」筛选（`tag=`=不筛选）。
- v2.3.1 CI 前端门禁脚本 `scripts/panel_frontend_check.mjs`：改 app.js/index.html 后本地先跑，失败会指出缺失 id/超预算/非法路径。
- v2.3.1 `accounts?tag=` 的 ETag = 归一化查询 + 命中行快照：新增一个「不影响当前命中集」的 tag 不会改变该查询的 ETag（预期行为；命中集变化才会变）。
- v2.3.1 `meta/tags` 展示拼写取 uid 升序首个出现的写法（如 VIP），匹配本身大小写不敏感；如需统一小写展示可后续归一。
- v2.3.1 `schedule.checkin_concurrency` 热更新只影响**下一轮**签到（在途 worker 不打断）；`notify.channel` 未知值仅 WARN 关闭通知，而 telegram 缺 chat_id 是启动 fail-fast（口径差异，见 config.normalize）。
- v2.3.0 Job 触发/取消**暂未进入面板操作审计**（依赖 jobs.json 与 task_runs 追溯）；如需审计可后续加 `jobs_run/job_cancel` action。
- v2.3.0 备注迁移逐条 PUT：旧本机备注很多时首次打开会产生 N 个请求（当前规模可接受；后续可加批量导入接口）。
- v2.3.0 Webhook 失败只重试 1 次后 WARN（不落盘待发）；data 允许 ≥7 字段（含 log_tail 摘要时更大），接收端应容忍额外字段。
- `panel.api_key` 改动**需重启**（已列入 `restart_required`）；`panel.poll_interval_sec`/`catch_up_minutes`/`balance_refresh_concurrency` 热生效。

## 跟随上游（如果上游/fork 恢复更新）

```bash
git remote add upstream https://github.com/linguo2625469/workbuddy2api-panel.git  # 已停更，仅存档
git fetch upstream main
git merge upstream/main
```

冲突预期：`internal/panel/app.js`（账号池分页/筛选段、到期分页、补偿区已删）、`index.html`（筛选栏/分页控件/补偿卡已删）、`panel.go`（packages 单号化、rescan、补偿路由已删）。
**合并后务必复查：补偿功能是否被上游重新带回来（要再次删除）；分页/筛选是否被绕过（新列表渲染路径要接入分页）；go module `github.com/lei-rr/workbuddy2api-pro` 是否被上游 import 覆盖。**

## 已回退 / 不要重复加

以下尝试过又按「最小化」要求撤掉，除非用户明确要求，不要重新引入：

- 首帧主题脚本 `theme.js`（刷新时会浅色闪一帧深色）
- 自定义分组 + 按分组/交集批量暂停选号
- 整池「积分构成」全量模式（已彻底删除，不是隐藏）
- **官方补偿**相关任何前后端（v2.0.0-pro 明确删除）

## 重建环境（本地副本丢失时）

```bash
git clone https://github.com/Lei-rr/workbuddy2api-pro.git
cd workbuddy2api-pro
git log --oneline -3     # 确认 HEAD 与本文档一致（v2.0.1-pro 起）
```

部署目录只需 `docker-compose.yml`（或 `docker run` 一条命令）+ `auths/`、`data/`、`config.json` 运行数据。
