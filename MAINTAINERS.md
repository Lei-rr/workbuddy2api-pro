# WorkBuddy2API Pro 维护手册（接手先读这页）

> 本文件替代旧 fork 的 `LOCAL_CHANGES.md`：记录 Pro 版的基线、决策、部署链路与排障要点。
> 新会话接手时先读这一页，**不要推翻已有决策**。

## 基线

- 原始项目：[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)（已删库）
- 上游最终版：[`linguo2625469/workbuddy2api-panel`](https://github.com/linguo2625469/workbuddy2api-panel)，基线 commit **`73a7560`**（2026-10-21 发布「停止维护声明」后停更）
- 本仓库起点 = 上游最终版 + 原 fork 面板定制合并（合并提交 `f9e654c`），已验证：`git diff 73a7560 f9e654c` 只差 8 个文件，**上游最终版 60 个提交零丢失**
- 本仓库：**`Lei-rr/workbuddy2api-pro`**，版本 **`2.0.0-pro`**
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

## 版本关键决策（v2.0.0-pro）

1. **更名**：Go module `github.com/lei-rr/workbuddy2api-pro`；镜像 `ghcr.io/lei-rr/workbuddy2api-pro`；容器名 `workbuddy2api-pro`；`/healthz` 的 `service` / `X-Service` 为 `workbuddy2api-pro`；面板标题 `WorkBuddy2API Pro`；版本 `2.0.0-pro`；发布产物 `wb2api-pro-v*`。
2. **官方补偿整个删除**（用户明确要求，含后台静默领取）：网关不再调用 `claim-compensation`。不要重新引入（`ClaimGift` 新手礼包属另一功能，保留）。
3. **分页/筛选为纯前端**：`overview` / `request_logs` 等接口协议未改，5s 轮询仍全量取数；服务端分页/增量推送列入 `UPGRADE_PROPOSALS.md`。
4. **单账号「做任务」**：`POST /panel/api/accounts/{uid}/tasks/run` = 复用 `auto_all` 执行器，返回 `results + summary{total,done,skipped,error,claimed}`。
5. **保留原 fork 定制**：账号池筛选/分页、三 tab（账号池/模型锁池/积分到期）、到期独立分页、单号积分弹窗、重新扫描按钮、`packages?uid=` 单号必填。

## 部署链路

- 镜像：`ghcr.io/lei-rr/workbuddy2api-pro:latest`（GitHub Actions `docker-ghcr` 自动构建，仅 linux/amd64）
- 容器名：`workbuddy2api-pro`；端口默认 `7863`（对外常见映射如 `7800:7863`）
- 运行数据（**不在仓库里，升级时必须保留**）：`auths/`、`data/`、`config.json`
- 从旧 `workbuddy2api-panel` 迁移：停旧容器 → 部署目录改 `image:` / `container_name:` → `docker pull ghcr.io/lei-rr/workbuddy2api-pro:latest` → `docker compose up -d`。配置 schema 未变，无需改 `config.json`；宿主健康探测若按旧 `service == "workbuddy2api"` 判据需同步改名。

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
```

**发布流程**：`git push origin main` → GitHub Actions 构建并推送 GHCR（约 2-3 分钟）→ `docker pull` → `docker compose up -d`。
新仓库注意：① 首次推送后到 Actions 确认 `docker-ghcr` workflow 已运行（新仓库 Actions 默认启用）；② 首次发布后需把 GHCR package 可见性设为 Public（否则拉取要 `docker login ghcr.io`）。

**打 tag 发布二进制**：`v2.0.0`（与 `appVersion` 去掉 `-pro` 后一致）→ `go-binaries` workflow 出五平台产物 + Release。

## 账号热更新

面板「重新扫描」按钮 = `auth.LoadDir` + `pool.SyncToDir`：外部脚本往 `auths/` 丢/删凭证后点一次即热对齐（只增删账号，已有账号积分/冷却状态保留）。后端对「扫到 0 个文件而池非空」返回 409 拒绝，防挂载丢失误清池。

## 实测基线（原 fork，1146 账号，供回归对照）

- `/panel/api/overview`：约 689 KB / 13 ms；前端 5s 轮询只渲染当前页
- 容器：CPU 约 0.04%，内存约 16.4 MB
- `accounts/rescan`：`scanned=1146 added=0 removed=0`，约 160 ms
- `packages?uid=<真实>` 200 / 未知 404 / 无 uid 400

## 已知注意点 / 坑

- 页面顶部「刷新」按钮 = `balance_all` → 对全池**无并发上限**打上游（1000+ 并发请求）。后台余额刷新 `balance_refresh_enabled=false` 时手动点它反而更激进，**非必要不点**。
- 签到是**串行**执行，1146 号一轮约 1 小时；新增账号较多时注意排程窗口。
- `state.json` 会落后于池（官方设计：新增账号不置 dirty，只有剔除才落盘）；重启后按零值重建，不影响使用。
- `auths/` 若出现属主混杂（如部分 `root:root`，容器以 10001 运行），可能 token 刷新失败：`chown -R 10001:10001 auths`。
- 面板只有单 `api_key` 鉴权，**公网部署必须置于 HTTPS 反代之后**并加访问限流；`auths/` 内是明文 access/refresh token，勿泄露。
- 单号积分构成弹窗为按需 1 次上游请求；整池全量查询入口已刻意封闭，不要恢复成页面自动全量查询（1000+ 账号时会触发风控）。
- 前端分页依赖 `overview`/`request_logs` 全量返回；若将来改服务端分页，需同步改 `renderAccounts`/`renderRequestTable` 等纯前端切片逻辑与测试。

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
git log --oneline -3     # 确认 HEAD 与本文档一致（v2.0.0-pro）
```

部署目录只需 `docker-compose.yml`（或 `docker run` 一条命令）+ `auths/`、`data/`、`config.json` 运行数据。
