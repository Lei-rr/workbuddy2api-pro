// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/logfmt"
)

// realmMemoEntry Realm() 记忆化缓存条目（v2.5.0 Perf-4）。
// version 与 Auth.realmVersion 对应；enabled 记录计算时的 globalEnabled，开关切换
// 时条目自然失效（无需全局清理）。
type realmMemoEntry struct {
	version uint64
	enabled bool
	value   string
}

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	// realm 账号域（"cn" / "global"），落盘于 auth.realm（嵌套形）或顶层 realm（扁平形）。
	// 空 = 缺省：Realm() 按 domain 后缀回落，最终恒非空。
	//
	// 命名注记：Go 不允许字段与方法同名，持久化字段用未导出 realm，计算访问器用
	// 导出的 Realm()（跨包调用全部走方法）。Parse/SaveAtomic/login 在包内读写字段。
	realm        string
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处

	// DeviceToken 设备风控 Token（X-Device-Token 头），来源 auth 文件的 device_token 键。
	// 缺省为空 = 不注入该头（容器内无桌面端 Turing SDK 的常见部署）。
	// 手写扁平形 auth 文件可直接写 "device_token": "..."；插件 OAuth 嵌套形
	// 顶层 device_token 也会被解析（与桌面端共用状态文件的部署方式）。
	DeviceToken string

	// Realm() 记忆化（v2.5.0 Perf-4）：realmVersion 在影响 Realm 结果的字段变更时递增
	//（BackfillRealm/BackfillRealmFor/SaveAtomic；Domain 由 upstream RefreshToken 在
	// a.mu 内改写，随后调用方 SaveAtomic 兜底失效）；realmMemo 为 atomic 快照，命中时
	// Realm() 零锁返回。旧条目靠 version 不匹配自然失效，无需清理。
	realmVersion atomic.Uint64
	realmMemo    atomic.Pointer[realmMemoEntry]
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// AccessTokenValue 加锁读取 AccessToken（出站请求头一律经此取值，勿直读字段）。
//
// 为什么必须加锁：RefreshToken 在 a.mu 内改写 AccessToken/RefreshToken/Domain/ExpiresAt
// （client.go「第 2 段（锁内）：校验快照一致后写回」），而所有出站请求头构造
// （ChatHeaders / BillingHeaders / fetchEnterpriseModels / fetchV3Models /
// global_models）与调度器的 token 检查都在锁外直读这些字段。生产上两侧真会并发：
// Scheduler.RunKeepaliveNow 定时对**每个**非禁用账号刷新（与是否有在途请求无关），
// 而 handler 正基于**同一个** *auth.Auth 指针构造请求头（Pool.AuthByUID/List 返回的
// 就是池内同一个对象）。无同步直读构成数据竞争（go test -race 实证）。
func (a *Auth) AccessTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken
}

// DomainValue 加锁读取 Domain（同 AccessTokenValue：RefreshToken 在锁内改写它）。
func (a *Auth) DomainValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Domain
}

// RefreshTokenValue 加锁读取 RefreshToken（同 AccessTokenValue：RefreshToken 在锁内
// 改写它）。调度器的「有无凭证」前置守卫（checkin/keepalive/travel 的
// `a.RefreshToken == ""`）必须经此取值，勿直读字段。
func (a *Auth) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// globalEnabled 全局开关：global realm 是否路由（D5 双保险）。
// 默认开启（与 config global.enabled 缺省 true 一致）：Realm() 正常按显式 realm/
// domain 判定 global/cn。显式 SetGlobalEnabled(false)（config "enabled": false）关闭
// → 逃生门：纯 CN 部署，即便 auth 文件写了 realm=global 或 domain 为 .workbuddy.ai
// 也恒判 cn——「关了才锁死」的单一闸口集中收敛在 Realm()/IsGlobal() 里。
var globalEnabled atomic.Bool

func init() { globalEnabled.Store(true) }

// SetGlobalEnabled 注入 global realm 路由开关（false = 锁死纯 CN，逃生门）。
func SetGlobalEnabled(enabled bool) { globalEnabled.Store(enabled) }

// GlobalEnabled 报告 global realm 路由开关当前状态（测试/运维观测）。
func GlobalEnabled() bool { return globalEnabled.Load() }

// Realm 返回账号的归一化域：显式 Realm=="global" 或 domain 后缀 .workbuddy.ai → "global"，
// 否则 "cn"。显式 global 优先于 domain 回落（D1）。
// 全局开关 SetGlobalEnabled(false) 时恒 "cn"（逃生门：纯 CN 锁定，不影响默认行为）。
// 空 realm + 空 domain → "cn"（老 CN 凭证零回归）。
//
// v2.5.0 Perf-4：记忆化快路径——命中时只做一次 atomic 读，无锁返回；未命中（首次 /
// realm·Domain 变更后 / global 开关切换后）才在 a.mu 下重算并缓存。
// 这是 List/overview/accounts/scheduler 的高频调用点（pprof 原 ~5.6%）。
func (a *Auth) Realm() string {
	if a == nil {
		return "cn"
	}
	gen := globalEnabled.Load()
	ver := a.realmVersion.Load()
	if m := a.realmMemo.Load(); m != nil && m.version == ver && m.enabled == gen {
		return m.value
	}
	a.mu.Lock()
	v := a.realmLocked()
	a.mu.Unlock()
	// 计算期间若发生 realm/Domain 写入（version 递增），本次结果可能已过期：只返回、
	// 不入缓存，下一次调用重新计算。
	if a.realmVersion.Load() == ver {
		a.realmMemo.Store(&realmMemoEntry{version: ver, enabled: gen, value: v})
	}
	return v
}

// InvalidateRealmCache 使 Realm() 记忆化缓存失效。
// 任何绕过本包直接改写 Domain（如 upstream 刷新 token 写回）的外部代码，在写完后
// 应调用本方法；包内已知写点已自动失效（BackfillRealm/BackfillRealmFor/SaveAtomic）。
func (a *Auth) InvalidateRealmCache() {
	if a == nil {
		return
	}
	a.realmVersion.Add(1)
}

// realmLocked Realm 的无锁内部实现：仅限**已持 a.mu** 的调用方使用（sync.Mutex 不可重入，
// 锁内再调 Realm() 会自锁）。realm 由 BackfillRealm 改写、Domain 由 RefreshToken 在锁内
// 改写，故读取必须与写方同锁（理由见 AccessTokenValue 注释）。
func (a *Auth) realmLocked() string {
	if !globalEnabled.Load() {
		return "cn"
	}
	if strings.TrimSpace(a.realm) == "global" || isGlobalDomain(a.Domain) {
		return "global"
	}
	return "cn"
}

// ResolveRealm 归一化 realm（cn/global）：显式非空优先，否则按原始 domain 推断
// （isGlobalDomain）。不受逃生门影响（逃生门是路由锁，不应影响标识判定）；
// domain 也为空 → "cn"（老 CN 凭证零回归）。
func ResolveRealm(explicit, domain string) string {
	if r := strings.TrimSpace(explicit); r != "" {
		return r
	}
	if isGlobalDomain(domain) {
		return "global"
	}
	return "cn"
}

// BackfillRealm 为缺省 realm 标识的账号持久化补标识：a.realm 为空时按「原始 domain 推断」
// 写回（cn/global），返回 (是否有变更, 归一化后的 realm)。已有标识不动（幂等）。
//
// 注意用 isGlobalDomain(a.Domain) 直接推断，而非 Realm()——Realm() 在逃生门
// （SetGlobalEnabled(false)）下恒降级 cn，把 global 账号写死成 cn 会永久污染凭证
// （逃生门是纯 CN 部署的临时锁，不应改写落盘数据）。domain 也为空时写 "cn"（老 CN 凭证）。
func (a *Auth) BackfillRealm() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.realm) != "" {
		return false, a.realm
	}
	r := ResolveRealm("", a.Domain)
	a.realm = r
	a.realmVersion.Add(1) // Realm() 记忆化失效（v2.5.0）
	return true, r
}

// RealmStored 直读持久化的 realm 标识（可能为空 = 未 backfill 的旧文件，Realm() 会 fallback）。
func (a *Auth) RealmStored() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realm
}

// BackfillRealmFor 显式写入 realm 标识（包外登录路径使用：panel login 已知用户选了
// global，直接落盘 realm=global，不依赖 domain 后缀推断）。realm 需为 cn/global，
// 非法值报错（防写脏）。返回是否发生变更。
func BackfillRealmFor(a *Auth, realm string) (bool, error) {
	if a == nil {
		return false, fmt.Errorf("nil auth")
	}
	switch strings.TrimSpace(realm) {
	case "cn", "global":
	default:
		return false, fmt.Errorf("realm must be cn/global, got %q", realm)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.realm == realm {
		return false, nil
	}
	a.realm = realm
	a.realmVersion.Add(1) // Realm() 记忆化失效（v2.5.0）
	return true, nil
}

// IsGlobal 报告账号是否属于 global realm（= Realm() == "global"）。
func (a *Auth) IsGlobal() bool { return a.Realm() == "global" }

// IsEnterprise 报告账号是否为企业版（auth 文件带非空 enterpriseId）。
//
// 企业版**没有个人成长体系**：签到 / 成长任务 / 连登管家 / 猫猫旅行 / 夜猫子
// 在上游一律被拒——实测（2026-10-07，同一时刻 A/B 对照）：
//   - POST /v2/billing/meter/daily-checkin                → 400 code 10001「企业账号不支持该操作」
//   - POST /billing/meter/claim-gift                    → 400 code 10001 同上
//   - GET  /activity/growth/{streak,buddy/info,heatmap,lottery/summary}
//     与 GET /v2/activity/growth/tasks                    → 403「growth system is only available for personal users」
//
// 因此引用处（scheduler 五类任务 + panel 成长任务/签到入口）必须与 IsGlobal 门控
// 并列跳过，避免每日对注定失败的端点空发请求、面板按钮必然报错。
//
// 不受影响的能力：选号派发、保活（token 刷新）、额度查询（企业额度改走
// /v2/billing/meter/get-enterprise-user-usage，见 upstream.enterpriseResource）。
//
// 直读字段（EnterpriseID 自 Parse 后不再改写，与 upstream 读取方式一致），
// 无需加锁——Realm() 加锁是因为 realm/Domain 会被 RefreshToken 在锁内改写。
func (a *Auth) IsEnterprise() bool { return strings.TrimSpace(a.EnterpriseID) != "" }

// IsEnterpriseSession 报告当前凭证是否处于「企业上下文」：
// 账号本身是企业版（IsEnterprise），或 AccessToken 签发自**企业切换会话**
// （JWT payload 带 token_source=enterprise_switch——浏览器在企业空间状态下
// 完成 OAuth 授权时签发）。
//
// 为什么单独一站：企业切换会话与真企业版的上游表现**不同**（实测 2026-10-12，
// 同一时刻与个人号 A/B 对照）——成长任务列表 / 额度查询 / 领养 Buddy 等**读端点
// 与直连 API 照常**，但 /v2/report 行为事件**返回 200 却被静默丢弃**（15 个成长
// 任务全部 0/x，重跑并 90s 内逐 15s 回读进度从不跳动），连登 / 签到同样无效。
// 即「读得见、记不上账」：不跳过则队列每轮白跑全部任务（含真实对话配额）且
// 无任何报错。因此行为类任务（活跃上报 / 连登 / 旅行 / 夜猫子 / 成长任务执行）
// 的门控应使用本方法；签到与任务列表扫描不在此列（签到失败是显式报错，
// 列表扫描实测可用）。额度查询 / 选号 / 保活照常（按 IsEnterprise 口径）。
//
// 每次现解码 JWT 而非 Parse 时缓存：RefreshToken 会整体换发 token，
// 企业切换状态可能随重新授权翻转，缓存会读到过期形态。解码成本仅一次
// base64 + 小 JSON（每调度轮每账号数次），可忽略。
func (a *Auth) IsEnterpriseSession() bool {
	if a.IsEnterprise() {
		return true
	}
	return jwtTokenSource(a.AccessTokenValue()) == "enterprise_switch"
}

// jwtTokenSource 解出 JWT payload 中的 token_source 声明；非 JWT / 解码失败
// 一律返回 ""（老 token 与非标准形态按无此声明处理，零回归）。先按无填充
// base64url 解（JWT 标准），失败再试带填充形态（兼容非标准签发方）。
func jwtTokenSource(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return ""
		}
	}
	var payload struct {
		TokenSource string `json:"token_source"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return payload.TokenSource
}

// isGlobalDomain 判定 domain 是否指向 www.workbuddy.ai 家族。
// 同时接受裸域 workbuddy.ai 与任意子域（HasSuffix("www.workbuddy.ai") 或裸域本身）。
func isGlobalDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
//
// v2.5.0 Perf-4：单次主 JSON 解析（auth 键用 RawMessage 捕获，嵌套形才二次解析该
// 子对象）。旧实现先解到 map[string]json.RawMessage 再二次解析整份文档，LoadDir
// 冷启动每个文件多付一次完整 JSON 解析；语义保持等价（"auth" 键存在即走嵌套形，
// 包括 "auth": null 的边角——与原 map probe 判定一致；缺 accessToken 报错）。
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var top struct {
		// Auth 键存在时捕获原始子对象（null 也会得到非 nil 的 "null"），据此保持
		// 旧实现「键存在即嵌套形」的判定。
		Auth    json.RawMessage `json:"auth"`
		Account struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		} `json:"account"`
		// 扁平形字段（auth/account 对象存在时以对象内字段为准）。
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
		Realm        string `json:"realm"`
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
		// DeviceToken 顶层 device_token（嵌套形与扁平形共用；手写时无需嵌进 auth 对象）。
		DeviceToken string `json:"device_token"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if top.Auth != nil {
		var n struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			Realm        string `json:"realm"`
		}
		if err := json.Unmarshal(top.Auth, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  n.AccessToken,
			RefreshToken: n.RefreshToken,
			ExpiresAt:    n.ExpiresAt,
			Domain:       n.Domain,
			realm:        n.Realm,
			UID:          top.Account.UID,
			EnterpriseID: top.Account.EnterpriseID,
			Nickname:     top.Account.Nickname,
			DeviceToken:  top.DeviceToken,
		}
	} else {
		a = Auth{
			AccessToken:  top.AccessToken,
			RefreshToken: top.RefreshToken,
			ExpiresAt:    top.ExpiresAt,
			Domain:       top.Domain,
			realm:        top.Realm,
			UID:          top.UID,
			EnterpriseID: top.EnterpriseID,
			Nickname:     top.Nickname,
			DeviceToken:  top.DeviceToken,
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	// 外部代码（upstream.RefreshToken）会在 a.mu 内直接改写 Domain；此处统一让 Realm()
	// 记忆化失效，确保刷新后不会继续用旧域判定（v2.5.0 Perf-4）。
	a.realmVersion.Add(1)
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"realm":        a.realm,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	// DeviceToken 非空才写回顶层 device_token：避免在无该字段的旧文件里引入空键
	// （保持与插件 OAuth 输出形状一致，插件读取忽略未知键）。
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		// Docker bind-mount 权限问题的典型现场：容器内 app 用户（uid 10001）
		// 对宿主机挂载目录无写权限。给出可操作指引而不是裸 syscall 错误。
		msg := fmt.Sprintf("写入 %s 失败: %v", tmp, err)
		if errors.Is(err, fs.ErrPermission) {
			msg += "\n（Docker 部署：容器内用户对宿主机挂载目录无写权限。解法任选：" +
				"1) 以本机 uid 运行容器：PUID=$(id -u) PGID=$(id -g) docker compose up -d；" +
				"2) sudo chown -R 10001:10001 ./auths ./data ./config.json；" +
				"3) compose 设 user: \"0:0\" 以 root 运行）"
		}
		return errors.New(msg)
	}
	return os.Rename(tmp, a.FilePath)
}

// loadDirWorkers 并行解析的 worker 数（包级 var 便于测试覆盖）。
// 默认 min(16, NumCPU*2)：启动是「读文件（I/O）+ JSON 解析（CPU）」混合负载，2×CPU
// 足够打满；上限 16 避免超大机器上启动反而被 goroutine 调度/磁盘寻道拖慢。
var loadDirWorkers = defaultLoadDirWorkers()

// defaultLoadDirWorkers 计算默认并行度。
func defaultLoadDirWorkers() int {
	n := runtime.NumCPU() * 2
	if n > 16 {
		n = 16
	}
	if n < 1 {
		n = 1
	}
	return n
}

// loadDirResult 单文件并行解析结果（下标与 files 对齐，保证收尾顺序确定）。
type loadDirResult struct {
	file string
	auth *Auth
	err  error
}

// readAndParseAuthFile 读取并解析单个 auth 文件（并行 worker 的工作单元）。
func readAndParseAuthFile(file string) loadDirResult {
	raw, err := os.ReadFile(file)
	if err != nil {
		return loadDirResult{file: file, err: err}
	}
	a, err := Parse(raw)
	if err != nil {
		return loadDirResult{file: file, err: err}
	}
	return loadDirResult{file: file, auth: a}
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
// 顺带做 realm 标识存量迁移：对空 realm 的 auth 自动 backfill（原始 domain 推断）并 SaveAtomic
// 落盘，一次性把旧文件补上 realm 键。单个文件写失败不阻断启动（log WARN 继续），
// 避免历史 auth 目录个别文件不可写时整个服务起不来。
//
// v2.5.0 Perf-4：文件级并行（读文件 + JSON 解析是 10k 冷启动的主要成本）。结果按 uid
// 稳定排序返回（确定性）；重复 UID 告警、空 realm 存量回填（SaveAtomic 写盘）仍在
// 串行收尾阶段按文件名顺序执行，语义与并行化前完全一致。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}

	// 1) 并行读取+解析（文件很少或 worker=1 时直接串行，避免无谓 goroutine）。
	results := make([]loadDirResult, len(files))
	workers := loadDirWorkers
	if workers > len(files) {
		workers = len(files)
	}
	if workers <= 1 {
		for i, f := range files {
			results[i] = readAndParseAuthFile(f)
		}
	} else {
		var wg sync.WaitGroup
		jobs := make(chan int)
		wg.Add(workers)
		for w := 0; w < workers; w++ {
			go func() {
				defer wg.Done()
				for i := range jobs {
					results[i] = readAndParseAuthFile(files[i])
				}
			}()
		}
		for i := range files {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
	}

	// 2) 串行收尾（与原实现同顺序/同语义）：跳过损坏文件、重复 UID 告警、realm 回填。
	// seenUID 重复 UID 检测：同 UID 出现在多个文件时（双 realm 同名 UID 概率近零）
	// 打 WARN 告警含两文件路径，由「后载入者胜出」保持现状行为（不改变加载结果）。
	seenUID := make(map[string]string, len(files))
	out := make([]*Auth, 0, len(files))
	for _, r := range results {
		if r.err != nil || r.auth == nil {
			continue // 读失败/解析失败：沿用历史"静默跳过"，不阻断启动
		}
		a := r.auth
		a.FilePath = r.file
		if prev, ok := seenUID[a.UID]; ok {
			log.Printf("WARN: uid %s duplicated across %s and %s — 后者覆盖（不同 realm 同名 UID？）",
				logfmt.Label(a.UID, a.Nickname), prev, r.file)
		}
		seenUID[a.UID] = r.file
		if a.RealmStored() == "" {
			if changed, r := a.BackfillRealm(); changed {
				if err := a.SaveAtomic(); err != nil {
					log.Printf("WARN: auth %s realm backfill save: %v", logfmt.Label(a.UID, a.Nickname), err)
				} else if r == "global" {
					log.Printf("auth %s 存量迁移: 补 realm=global（domain=%s）", logfmt.Label(a.UID, a.Nickname), a.Domain)
				}
			}
		}
		out = append(out, a)
	}

	// 3) 稳定排序：返回顺序与文件遍历顺序解耦、结果确定；UID 相同保持文件名顺序。
	sort.SliceStable(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}
