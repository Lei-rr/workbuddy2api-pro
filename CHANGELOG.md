# Changelog

本文件记录 **WorkBuddy2API Pro**（`workbuddy2api-pro`）的版本变更。

- 原始项目：[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)（已删除）
- 上游最终版：[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) `73a7560`（2026-10-21 停止维护）
- 本仓库：在**上游最终版**基础上继续二次开发（副标题、面板定制、Pro 改动均以下文为准）

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
