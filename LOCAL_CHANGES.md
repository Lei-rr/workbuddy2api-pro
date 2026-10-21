# 本地改动说明（fork: Lei-rr/workbuddy2api-panel）

> 本文件随 fork 仓库维护（官方 `.gitignore` 忽略 `*.md`，本文件用 `git add -f` 强制跟踪），
> 删掉本地源码副本后重新 clone 仍能读到。新会话接手时先读这一页，不要推翻已有决策。

## 基线

- 上游：`linguo2625469/workbuddy2api-panel`，基线 commit `73a7560`（上游最新，含「停止维护声明」）
- 上一次基线：`6b31db6`（PR #152 合并后）；本次以 merge 方式吸收上游 60 个提交，本地 11 个提交全保留
- 本地分支：`main`，已推到 `fork`（`Lei-rr/workbuddy2api-panel`）的 `main`
- 原则：**只做增量，尽量不删官方代码**；整池「积分构成」那套代码原样保留，只断入口

## 改动清单（首创，相对 6b31db6）

| 文件 | 改动 | 原因 |
|---|---|---|
| `internal/panel/app.js` | `TRANGE_DEFAULT='today'`；账号池客户端筛选（搜索/状态/版本）+ 分页（50/100/200/全部）；三级 tab（账号池/模型锁池/积分到期，`accTab`/`switchAccTab`/`expBoxShow`）；到期列表独立分页（`expAll`/`expPage`/`expPageSize`）；账号行「积分」按钮 + `pkVeil` 单号弹窗（复用官方 `pk*` 辅助函数）；「重新扫描」按钮事件；`TITLES` 去掉 `packages`；删 `btnExp` 绑定 | 1000+ 账号时页面可用性；整池逐号查上游有风控风险 |
| `internal/panel/index.html` | 筛选栏 `#accFilter`；`#accTabs` 三个 tab；到期分页栏 `#expFilter`；`#accBox`/`#mlBox` id；`#pkVeil` 弹窗；`#btnRescan` 按钮；删导航「积分构成」项与到期卡「检查」按钮 | 同上 |
| `internal/panel/panel.go` | `packages` 收紧为 `?uid=` 必填（无 uid → 400，删除整池全量模式）；`packagesOne` 单号查询（未知 uid 404、上游失败 200+error）；`accountsRescan`（`auth.LoadDir` + `pool.SyncToDir`，扫到 0 文件而池非空时 409 拒绝） | 单号按需查；auths/ 目录热对齐 |
| `internal/panel/packages_test.go` | 新增：未知 uid 404、无 uid/空白 uid 一律 400 | 钉住单号模式 |
| `internal/panel/rescan_test.go` | 新增：0 文件保护拒绝、空池放行 | 防止挂载丢失清空整池 |
| `internal/panel/frontend_test.go` | `expBoxShow` 桩与分页状态初值（切片环境无 tab/分页）、`defaultPreset:"today"` 断言 | 测试跟随实现，防静默回退 |
| `.github/workflows/docker-ghcr.yml` | 构建平台仅 `linux/amd64`，去掉 QEMU | 部署机 x86_64，多架构每次多等 10 分钟 |

核心包 `internal/pool`、`server`、`upstream`、`scheduler`、`auth` **零改动**。

## 2026-10 同步记录（6b31db6 → 73a7560）

### 吸收的上游能力

- 联网搜索端点 `POST /v1/search/messages`
- 官方补偿卡（issue #189）：截止倒计时 + 逐号领取 + 全部领取
- 到期提醒快照与「检查」同口径：`credits_expiry_buckets` 按自然日聚合写入 state.json
- 手机端适配（`mobile.css` / `mobile.js`）：抽屉侧栏 + 宽表卡片化
- 账号备注（本机 localStorage）、到期卡「收起」折叠（`btnExpFold`）
- 按地区独立轮询/顺序耗尽调度、`models.hidden` 黑名单、裸模型名跨域回退、`cn.chat_base`、`dns_servers`、出站代理走 `ProxyFromEnvironment`、refresh 单飞防 12153、11158 / 6021 / 11-128 错误分流、`reasoning_text` 剥离
- 新包 `internal/credithist`、`cmd/credit`（积分历史）

### 冲突解决（3 处，全部按「保自定义 UI + 收上游实现」）

| 位置 | 处理 |
|---|---|
| `app.js` → `renderExpirySnapshot` 尾部 | 保留本地分页（`expAll`/`expPage`/`expPageSize`、`expCount`/`expPageInfo`/`expPrev`/`expNext`），采用上游 `snapshotNoteText()` 的数据新鲜度标注；可见性走 `expBoxShow()` |
| `app.js`「检查」绑定段 | 本地「单号积分构成弹窗」整段保留，上游「账号备注 + 到期折叠」整段追加。`btnExp` 已删，「检查」绑定不恢复 → `loadExpiry()` 依然无调用点，整池查询入口保持封闭 |
| `index.html` 到期卡 | 保留本地 `#expFilter` 分页栏与 `#expList`，吸收上游 `#btnExpFold` 折叠与 `#compBox` 官方补偿卡；不恢复「检查」按钮 |

### 让上游新路径也吃上本地分页：最小重构

上游把快照改成走 `renderExpiry()`（经 `expiryRowsHtml`），直接复用会一次性渲染全池、绕过到期分页。拆成四层：

- `expiryKeyed(list)` → `{ keyed, today }`：只排序，上游排序规则与同日 tie-break 原样保留
- `expiryRowsOf(keyed, today)`：只渲染行
- `expiryRowsHtml(list)`：原签名的组合入口，保持与上游实现/前端 harness 兼容
- `expSlice(keyed)`：按分页状态切片并回写页码；用 `typeof expAll === 'undefined'` 判定是否处于前端 harness 切片环境（那里没有模块级分页状态），无状态则原样返回全量

`renderExpiry()` 与 `renderExpirySnapshot()` 的旧 state 分支共用这份分页；`expBoxShow()` 一律 `typeof` 保护后再调用（harness 里可能没有该桩）。

### 文案

`snapshotNoteText()` 里「点『检查』拉实时明细」→「逐包明细见账号行『积分』按钮」：本 fork 没有「检查」按钮，不能引导用户点不存在的入口。

### 验证

- `go build ./...` 通过；`go test ./...` 全绿（含 `internal/panel` 全部前端切片用例，27s）
- 手工复现上游两个关键用例：`TestAppJSExpirySnapshotRenders` / `TestAppJSExpirySnapshotMatchesLive` 逐字节一致
- 分页实测：250 账号 → 当前页 100 行、`共 250 个`、`1 / 3`

## 整池「积分构成」入口是怎么断的（三层，代码保留）

1. 导航删掉「积分构成」项
2. `TITLES` 去掉 `packages` → 直链 `#packages` 自动回落账号池，`loadPackages()` 永不被调用
3. 到期卡「检查」按钮及其绑定删除 → 第二个整池查询入口也封死

后端 `/panel/api/packages` 无 uid 直接 400，全量模式代码已删（`sort` import 一并移除）。

`renderPackages`/`loadExpiry`/`renderExpiry`/`view-packages`/`pk-expiry-*` CSS 仍在文件里，不可达。

## 部署链路

- 源码：`/guolei/workbuddy2api-panel-src`
- 部署：`/guolei/workbuddy2api-panel/docker-compose.yml`，镜像 `ghcr.io/lei-rr/workbuddy2api-panel:latest`（已从官方 linguo 镜像切过来）
- 发布：`git push fork HEAD:main` → GitHub Action（amd64，约 2.5 分钟）→ `docker pull` → `docker compose up -d`
- 注意：**fork 首次需 `PUT /repos/{owner}/{repo}/actions/workflows/{id}/enable`**，否则 push 不触发 workflow（本项目已 enable）
- 账号热更新：面板「重新扫描」按钮 = `auth.LoadDir` + `pool.SyncToDir`，1146 文件约 160ms；外部脚本往 `auths/` 丢文件后点一次即可

## 实测基线（1146 账号）

- `/panel/api/overview`：689 KB / 13 ms，前端 5s 轮询只渲染当前页 100 行
- 容器：CPU 0.04%，内存 16.4 MB
- `packages?uid=<真实>` 200 / 未知 404 / 无 uid 400
- `accounts/rescan`：`scanned=1146, added=0, removed=0`，160 ms

## 已知注意点

- 页面顶部「刷新」按钮 = `balance_all` → `RunBalanceRefreshNow()`，**对全池无并发上限地打上游**（1146 个并发请求）+ 昵称同步。后台 `balance_refresh_enabled=false`，手动点这个按钮反而更激进，建议不点。
- 积分靠 9/21 点签到更新（签到流程内含查余额 + 写回 `credits`/到期批次）；签到是**串行**执行，1146 号一轮约 1 小时。
- `state.json` 会落后于池（官方设计：新增账号不置 `dirty`，只有剔除才落盘），重启后按零值重建，不影响使用。
- `auths/` 里有 52 个文件属主是 `root:root`（其余 `10001`），容器可读不可写；若出现 token 刷新失败，先 `chown -R 10001:10001 auths`。
- 上游 73a7560 起已声明「停止维护」，后续同步频率会很低；上游 README 的增长主要来自公告，不必逐条跟。

## 重建环境（本地源码副本丢失时）

```bash
git clone https://github.com/Lei-rr/workbuddy2api-panel.git
cd workbuddy2api-panel
git log --oneline -3          # 确认 HEAD 与本文档一致
```

部署目录只需 `docker-compose.yml`（镜像 `ghcr.io/lei-rr/workbuddy2api-panel:latest`，
容器名 `workbuddy2api-panel`，`7800:7863`，挂载 `./auths`、`./data`、`./config.json`，
`user: 10001:10001`，`TZ=Asia/Shanghai`）——`auths/`、`data/`、`config.json` 是运行数据，
必须保留，不在仓库里。

## 跟随上游

```bash
git fetch upstream main
git merge upstream/main            # 冲突预期：app.js 的 renderExpirySnapshot 尾部、单号弹窗段、index.html 到期卡
git push fork HEAD:main
```

merge 比 rebase 划算：本地 11 个提交连续改同一批前端文件，rebase 会逐提交重复冲突；merge 只解一次，历史虽多一个 merge commit 但可回滚。

合并时逐项核对：

1. 到期卡是否仍有 `#expFilter` 分页栏与 `expCount`/`expPageInfo`（上游若再改 `expiryRowsHtml`，分页要重新挂到新的渲染出口）
2. `expSlice` 的 `typeof` 守卫是否还在（否则前端 harness 切片会 ReferenceError）
3. `btnExp` 是否被上游加回（若加回必须再删，否则整池查询复活）
4. 上游是否给 `snapshotNoteText` 改文案，改完要把「检查」重新换成「积分」按钮

## 已回退 / 不要重复加

这些做过又按「最小化」要求撤掉了，除非用户明确要，不要重新引入：

- 首帧主题脚本 `theme.js`（刷新时浅色会闪一帧深色）
- 自定义分组 + 按分组/交集批量暂停选号
- 本地手写的手机端窄屏自适应（上游 `mobile.css`/`mobile.js` 已是官方实现，用官方那份）
- `packages` 全量模式（已彻底删除，不是隐藏）
- 到期卡「检查」按钮（整池查询入口，删了才能防误触）
