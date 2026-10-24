// accounts.go 账号池列表服务端分页/筛选/排序/投影/ETag 与 ETag LRU 缓存
// （v2.6.1 拆分自 panel.go，纯移动；v2.1 #1 + v2.5.0 Perf-3 实现不变）。
package panel

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
)

// ---------------------------------------------------------------------------
// v2.1 #1：账号池列表服务端分页 / 筛选 / 排序 / 字段裁剪 / ETag
// ---------------------------------------------------------------------------

const (
	// accountsDefaultPageSize 默认页大小：100 行 light 约 54KB（gzip 后约 5KB）；
	// 10k 账号池全量 light 约 5-6MB，故默认分页 + ETag/304 是前端主路径。
	accountsDefaultPageSize = 100
	// accountsMaxPageSize 服务端页大小上限：page_size=0（「全部」）与 >上限 的请求
	// 都按它截断，防止单个响应无限大。
	accountsMaxPageSize = 2000
	// accountsFieldsLight / accountsFieldsFull 字段投影档位（默认 light）。
	accountsFieldsLight = "light"
	accountsFieldsFull  = "full"
	// accountsCacheControl 账号列表缓存策略：must-revalidate（每次带 ETag 回源校验，
	// 命中 304 零 body）；private 因为响应依赖面板密钥鉴权，禁止共享缓存跨用户复用。
	accountsCacheControl = "private, no-cache"
)

// accountsQuery 是 /panel/api/accounts 的归一化查询参数。
// 归一化后同时用于筛选/排序/分页与 ETag 输入：语义相同的原始写法（大小写、
// 默认值/显式值、越界值）映射到同一份参数，从而共享缓存条目。
type accountsQuery struct {
	Page       int    `json:"page"`       // >=1；默认 1
	PageSize   int    `json:"page_size"`  // 默认 100；0 = 全部（受 accountsMaxPageSize 截断）
	Q          string `json:"q"`          // 已 TrimSpace + ToLower：匹配 uid/nickname 包含
	State      string `json:"state"`      // "" | healthy | cooling | paused | disabled
	Realm      string `json:"realm"`      // "" | cn | global
	Enterprise string `json:"enterprise"` // "" | "1" | "0"
	// Tag 标签筛选（v2.3.1 #12 扩展）：已 TrimSpace + ToLower，按 data/account-meta.json
	// 中该账号的 tags 大小写不敏感精确匹配；空 = 不筛选。
	Tag    string `json:"tag"`
	Sort   string `json:"sort"`   // uid（默认）| credits | last_success | cool_remaining
	Fields string `json:"fields"` // light（默认）| full
}

// parseAccountsQuery 解析并归一化查询参数。非法值一律回落默认（不 400）：
// 列表是只读观测接口，容忍前端/脚本传脏参数比回错更容易排障。
func parseAccountsQuery(r *http.Request) accountsQuery {
	qv := r.URL.Query()
	q := accountsQuery{
		Page:       1,
		PageSize:   accountsDefaultPageSize,
		Q:          strings.ToLower(strings.TrimSpace(qv.Get("q"))),
		State:      normalizeAccountsState(qv.Get("state")),
		Realm:      normalizeAccountsRealm(qv.Get("realm")),
		Enterprise: normalizeAccountsEnterprise(qv.Get("enterprise")),
		Tag:        strings.ToLower(strings.TrimSpace(qv.Get("tag"))),
		Sort:       normalizeAccountsSort(qv.Get("sort")),
		Fields:     normalizeAccountsFields(qv.Get("fields")),
	}
	if raw := strings.TrimSpace(qv.Get("page")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			q.Page = n
		}
	}
	if vals, ok := qv["page_size"]; ok && len(vals) > 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(vals[0])); err == nil && n >= 0 {
			switch {
			case n == 0:
				q.PageSize = 0 // 全部（服务端再按 accountsMaxPageSize 截断）
			case n > accountsMaxPageSize:
				q.PageSize = accountsMaxPageSize
			default:
				q.PageSize = n
			}
		}
	}
	return q
}

// normalizeAccountsState 归一化状态筛选；未知值 = 不筛选（全部）。
func normalizeAccountsState(v string) string {
	switch s := strings.ToLower(strings.TrimSpace(v)); s {
	case "healthy", "cooling", "paused", "disabled":
		return s
	}
	return ""
}

// normalizeAccountsRealm 归一化域筛选；未知值 = 不筛选。
func normalizeAccountsRealm(v string) string {
	switch s := strings.ToLower(strings.TrimSpace(v)); s {
	case "cn", "global":
		return s
	}
	return ""
}

// normalizeAccountsEnterprise 归一化企业版筛选（1/0，兼容 true/false 写法）；
// 空/未知值 = 不筛选。
func normalizeAccountsEnterprise(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "" {
		return ""
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return ""
	}
	if b {
		return "1"
	}
	return "0"
}

// normalizeAccountsSort 归一化排序键；未知值回落默认 uid 升序。
func normalizeAccountsSort(v string) string {
	switch s := strings.ToLower(strings.TrimSpace(v)); s {
	case "credits", "last_success", "cool_remaining":
		return s
	}
	return "uid"
}

// normalizeAccountsFields 归一化字段投影；只有显式 full 才返回全量，其余一律 light。
func normalizeAccountsFields(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), accountsFieldsFull) {
		return accountsFieldsFull
	}
	return accountsFieldsLight
}

// accountsMatch 报告账号是否满足筛选条件。
func accountsMatch(st pool.Status, q accountsQuery, now time.Time) bool {
	if q.Q != "" &&
		!strings.Contains(strings.ToLower(st.UID), q.Q) &&
		!strings.Contains(strings.ToLower(st.Nickname), q.Q) {
		return false
	}
	if q.Realm != "" && st.Realm != q.Realm {
		return false
	}
	if q.Enterprise != "" && (q.Enterprise == "1") != st.Enterprise {
		return false
	}
	if q.State != "" && accountsState(st, now) != q.State {
		return false
	}
	return true
}

// accountsState 单值状态归类，优先级 disabled > paused > cooling > healthy，
// 与 pool.CountsDetailedWithPaused 的四个计数桶口径一致：
//   - cooling = 未禁用未暂停且不可选：账号级冷却（until）/熔断期（breaker_until）/
//     连败降权期（degrade_until），即 pool.healthy 判 false 的全部原因；
//   - healthy = pool.healthy 判 true（注意 in_flight 维度不算，与 CountsDetailed 一致）。
func accountsState(st pool.Status, now time.Time) string {
	switch {
	case st.Disabled:
		return "disabled"
	case st.Paused:
		return "paused"
	case st.Cooling || (!st.DegradeUntil.IsZero() && now.Before(st.DegradeUntil)):
		// Status.Cooling 只覆盖 until/breakerUntil；pool.healthy 还看 degradeUntil，
		// 这里补上降权期，保证 state=healthy 不会把降权号算进来。
		return "cooling"
	default:
		return "healthy"
	}
}

// sortAccounts 按归一化排序键就地排序（SliceStable + uid 兜底，保证同值输出确定，
// ETag 才能稳定）：
//   - uid：升序（默认，与 pool.List() 的自然顺序一致）；
//   - credits：降序（余额多的在前），同额按 uid 升序；
//   - last_success：降序（最近成功在前），从未成功的零值恒排最后，同刻按 uid 升序；
//   - cool_remaining：升序（剩余冷却秒数少的在前；未冷却为 0 排最前），同值按 uid 升序。
//
// v2.5.0 Perf-3 起生产 accounts handler 改用 sortAccountIndexes（只排索引，不再
// 拷贝/移动 527B 的 Status）；本函数**仅测试用**（作为等价性 oracle，见
// perf_optim_test.go TestSortAccountIndexesEquivalent），勿在 handler 新调用。
func sortAccounts(rows []pool.Status, sortBy string) {
	switch sortBy {
	case "credits":
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Credits != rows[j].Credits {
				return rows[i].Credits > rows[j].Credits
			}
			return rows[i].UID < rows[j].UID
		})
	case "last_success":
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := rows[i].LastSuccessTime, rows[j].LastSuccessTime
			if !a.Equal(b) {
				if a.IsZero() {
					return false
				}
				if b.IsZero() {
					return true
				}
				return a.After(b)
			}
			return rows[i].UID < rows[j].UID
		})
	case "cool_remaining":
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].CoolRemaining != rows[j].CoolRemaining {
				return rows[i].CoolRemaining < rows[j].CoolRemaining
			}
			return rows[i].UID < rows[j].UID
		})
	default: // uid
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].UID < rows[j].UID })
	}
}

// pageAccounts 按 page/page_size 切页，返回当前页、生效页码与总页数。
//
// page_size<=0 表示「全部」模式：返回前 min(len, accountsMaxPageSize) 行，
// page_size 语义保持 0（响应回显 0，前端据此知道不是普通分页），page 规范化为 1，
// pages 为 0（空结果）/1。
//
// 普通模式下超界 page 自动钳到最后一页（响应 page 为钳后值，前端以响应为准）：
// 数据随状态变化减少页数时，轮询不会停在一个永远空白的页码上；空结果 pages=0、
// page=1。空切片保持非 nil，JSON 输出 [] 而不是 null。
//
// v2.5.0 Perf-3 起生产 accounts handler 改用 pageSliceIndexes（对索引切片分页，
// 不再整页拷贝 Status）；本函数**仅测试用**（等价性 oracle，见
// perf_optim_test.go TestPageSliceIndexesEquivalent）。
func pageAccounts(rows []pool.Status, page, pageSize int) (pageRows []pool.Status, effPage, pages int) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		if len(rows) == 0 {
			return rows[:0], 1, 0
		}
		if len(rows) > accountsMaxPageSize {
			return rows[:accountsMaxPageSize], 1, 1
		}
		return rows, 1, 1
	}
	pages = (len(rows) + pageSize - 1) / pageSize
	if pages == 0 {
		return rows[:0], 1, 0
	}
	if page > pages {
		// 钳到最后一页：此后 (page-1)*pageSize < len(rows)，无溢出风险
		// （page 可能来自任意大的整数，先钳再乘）。
		page = pages
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if end > len(rows) {
		end = len(rows)
	}
	return rows[start:end], page, pages
}

// lightStatus 返回 light 字段投影：把两个「重/易变、列表页用不到」的字段置零
// （两者都是 omitempty，置零即从 JSON 省略）：
//   - credits_expiry_buckets：按日到期批次，可变长数组，只属「积分到期」快照；
//   - cool_remaining_sec：秒级倒计时，前端改用 until / breaker_until / degrade_until
//     绝对时间 + 响应里的 now 在本地倒计时（否则 304 会让倒计时冻结）。
//
// 其余字段与 /overview 的 accounts 逐字段同构（含 token_usage / model_costs /
// rate_limited_models），前端渲染列表无需字段回退。
func lightStatus(st pool.Status) pool.Status {
	st.CreditsExpiryBuckets = nil
	st.CoolRemaining = 0
	return st
}

// stableStatusForETag 返回 ETag 输入用的稳定副本：把「随秒/随请求瞬变，但不足以
// 让列表内容发生有意义变化」的字段规范化为零值，避免 5s 轮询在无运维状态变化时
// ETag 抖动（这些字段变化时若换 ETag，缓存永远命不中）：
//   - cool_remaining_sec：按 now 计算的秒级倒计时；
//   - in_flight：在途并发瞬时值；
//   - breaker_fails：熔断器运行期失败计数（每次请求都可能 +1/清零）；
//   - token_usage.last_used_at：最近一次请求时刻。
//
// 其它字段（credits/until/token 计数/model_costs/...）是有意义的内容变化，照常
// 参与 hash。代价：命中 304 时浏览器复用旧 body，其中倒计时/在途可能是旧值——
// 倒计时本就应由 until 本地重算，在途是瞬时观测，口径可接受（契约见任务 #1）。
func stableStatusForETag(st pool.Status) pool.Status {
	st.CoolRemaining = 0
	st.InFlight = 0
	st.BreakerFails = 0
	st.TokenUsage.LastUsedAt = time.Time{}
	return st
}

// accountsStableProjection 返回 ETag hash 输入行：在响应投影基础上再把易变字段
// 归零（见 stableStatusForETag）。
func accountsStableProjection(pageRows []pool.Status, fields string) []pool.Status {
	rows := make([]pool.Status, len(pageRows))
	for i, st := range pageRows {
		if fields != accountsFieldsFull {
			st = lightStatus(st)
		}
		rows[i] = stableStatusForETag(st)
	}
	return rows
}

// accountsETagInput 是 ETag 的完整 hash 输入：归一化查询参数 + 计数/分页元信息 +
// 稳定投影行。rows 参与 hash 保证「内容变了 ETag 必变」；查询参数参与保证不同
// 页/筛选/排序/投影各有独立缓存条目。
type accountsETagInput struct {
	Query    accountsQuery `json:"query"`
	Total    int           `json:"total"`
	Filtered int           `json:"filtered"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
	Pages    int           `json:"pages"`
	Rows     []pool.Status `json:"rows"`
}

// accountsETag 计算弱 ETag `W/"acct-<sha256 hex>"`。
// 弱校验适合「语义等价即可复用」的列表响应；hex 输入是无 key 的 JSON
// （encoding/json 对结构体字段顺序确定，无 map 乱序问题）。
//
// ok=false 表示稳定投影无法 JSON 序列化（例如极端脏数据里的 NaN/Inf 浮点）：
// 此时调用方必须跳过 ETag/304——绝不能返回一个恒定 ETag，否则后续所有轮询都会
// 命中 304 拿到过期数据，比没有缓存更糟。
func accountsETag(in accountsETagInput) (etag string, ok bool) {
	raw, err := json.Marshal(in)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(raw)
	return `W/"acct-` + hex.EncodeToString(sum[:]) + `"`, true
}

// ---------------------------------------------------------------------------
// v2.5.0 Perf-3：accounts ETag/304 缓存（进程内 LRU，容量 32）
// ---------------------------------------------------------------------------
//
// 背景（基线）：10k 账号下 accounts 每次请求 ~31ms，其中 ~29ms 用在「全池快照 +
// 过滤 + 排序 + 稳定投影 hash」，而面板 5s 轮询在无运维变化时几乎全是 304。
// 缓存语义：
//   - key = 归一化 accountsQuery（与 ETag 输入同源；语义等价写法共享条目）；
//   - value = {etag, poolVersion, metaVersion, builtAt}；
//   - 请求先查缓存：pool.Version()/accountMeta 代际号都未变且未超 TTL 时，
//     If-None-Match 命中缓存 etag → 直接 304（不读快照、不构 rows，<0.1ms）；
//   - 未命中 304 但版本一致 → 慢路径仍要构造响应 body（200 必须给最新 now/
//     in_flight/cool_remaining 等瞬态值，不能复用旧 body——v2.1 契约与
//     TestAccountsETagVolatileNormalization 钉住了该行为），但可复用缓存 etag，
//     省掉稳定投影 JSON+sha256；
//   - 版本变化/TTL 到期 → 淘汰条目重算（内容变化 ETag 必变）。
//   - TTL=10s 只兜底「纯时间流逝」的状态翻转（冷却到期等没有写操作可感知）。
const (
	// accountsETagCacheCap ETag 缓存容量：前端只轮询少量查询组合（默认页/翻页/
	// 字段档位/筛选；多标签页 + 脚本），32 条足够，LRU 淘汰。
	accountsETagCacheCap = 32
	// accountsETagCacheTTL 条目最长可信时间（见上）。
	accountsETagCacheTTL = 10 * time.Second
)

// accountsETagCacheEntry 一条 ETag 缓存（只存 ETag 与失效依据，不缓存 body：
// 200 响应必须反映当前 now/瞬态字段，旧 body 复用会违反 v2.1 轮询契约）。
type accountsETagCacheEntry struct {
	etag        string
	poolVersion uint64
	metaVersion uint64
	builtAt     time.Time
}

// accountsETagCacheItem list 元素（带 key 便于淘汰时同步删 map）。
type accountsETagCacheItem struct {
	key   string
	entry accountsETagCacheEntry
}

// accountsETagCache 固定容量 LRU（container/list + map，并发安全）。
type accountsETagCache struct {
	mu sync.Mutex
	l  *list.List
	m  map[string]*list.Element
}

// newAccountsETagCache 构建空缓存。
func newAccountsETagCache() *accountsETagCache {
	return &accountsETagCache{
		l: list.New(),
		m: make(map[string]*list.Element, accountsETagCacheCap),
	}
}

// lookup 版本一致且未超 TTL 时返回条目（提升为最近使用）；版本变化/TTL 到期
// 淘汰条目并报告未命中（调用方走慢路径重算并覆盖）。
func (c *accountsETagCache) lookup(key string, poolVersion, metaVersion uint64) (accountsETagCacheEntry, bool) {
	if c == nil {
		return accountsETagCacheEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[key]
	if !ok {
		return accountsETagCacheEntry{}, false
	}
	item := el.Value.(*accountsETagCacheItem)
	if item.entry.poolVersion != poolVersion ||
		item.entry.metaVersion != metaVersion ||
		time.Since(item.entry.builtAt) >= accountsETagCacheTTL {
		c.l.Remove(el)
		delete(c.m, key)
		return accountsETagCacheEntry{}, false
	}
	c.l.MoveToFront(el)
	return item.entry, true
}

// store 写入/覆盖条目并维护容量（超限淘汰最旧）。
func (c *accountsETagCache) store(key string, entry accountsETagCacheEntry) {
	if c == nil {
		return
	}
	entry.builtAt = time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[key]; ok {
		el.Value.(*accountsETagCacheItem).entry = entry
		c.l.MoveToFront(el)
		return
	}
	c.m[key] = c.l.PushFront(&accountsETagCacheItem{key: key, entry: entry})
	for c.l.Len() > accountsETagCacheCap {
		oldest := c.l.Back()
		if oldest == nil {
			break
		}
		item := oldest.Value.(*accountsETagCacheItem)
		c.l.Remove(oldest)
		delete(c.m, item.key)
	}
}

// cacheKey 归一化查询的稳定缓存键（\x00 分隔，避免字段内容互相拼歧义）。
func (q accountsQuery) cacheKey() string {
	return strings.Join([]string{
		strconv.Itoa(q.Page), strconv.Itoa(q.PageSize), q.Q, q.State, q.Realm,
		q.Enterprise, q.Tag, q.Sort, q.Fields,
	}, "\x00")
}

// accountsProjectionRow 单行投影（light 省略两个重/易变字段；full 原样返回副本）。
func accountsProjectionRow(st pool.Status, fields string) pool.Status {
	if fields == accountsFieldsFull {
		return st
	}
	return lightStatus(st)
}

// sortAccountIndexes 按归一化排序键就地排序「快照索引」切片（v2.5.0 Perf-3：
// 只排索引（8B/项），避免对 10k 个 527B 的 Status 全量拷贝/移动）。
// 排序口径与旧 sortAccounts 完全一致（SliceStable + uid 兜底，保证同值输出确定、
// ETag 稳定）：credits 降序、last_success 降序（零值最后）、cool_remaining 升序。
func sortAccountIndexes(idxs []int, snapshot []pool.Status, sortBy string) {
	switch sortBy {
	case "credits":
		sort.SliceStable(idxs, func(i, j int) bool {
			a, b := &snapshot[idxs[i]], &snapshot[idxs[j]]
			if a.Credits != b.Credits {
				return a.Credits > b.Credits
			}
			return a.UID < b.UID
		})
	case "last_success":
		sort.SliceStable(idxs, func(i, j int) bool {
			a, b := &snapshot[idxs[i]], &snapshot[idxs[j]]
			if !a.LastSuccessTime.Equal(b.LastSuccessTime) {
				if a.LastSuccessTime.IsZero() {
					return false
				}
				if b.LastSuccessTime.IsZero() {
					return true
				}
				return a.LastSuccessTime.After(b.LastSuccessTime)
			}
			return a.UID < b.UID
		})
	case "cool_remaining":
		sort.SliceStable(idxs, func(i, j int) bool {
			a, b := &snapshot[idxs[i]], &snapshot[idxs[j]]
			if a.CoolRemaining != b.CoolRemaining {
				return a.CoolRemaining < b.CoolRemaining
			}
			return a.UID < b.UID
		})
	default: // uid
		sort.SliceStable(idxs, func(i, j int) bool {
			return snapshot[idxs[i]].UID < snapshot[idxs[j]].UID
		})
	}
}

// pageSliceIndexes 按 page/page_size 计算索引切片范围（语义与 pageAccounts 一致：
// page_size<=0 = 全部（上限 accountsMaxPageSize）、超界 page 钳到末页、空结果 pages=0）。
func pageSliceIndexes(n, page, pageSize int) (start, end, effPage, pages int) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		if n == 0 {
			return 0, 0, 1, 0
		}
		if n > accountsMaxPageSize {
			return 0, accountsMaxPageSize, 1, 1
		}
		return 0, n, 1, 1
	}
	pages = (n + pageSize - 1) / pageSize
	if pages == 0 {
		return 0, 0, 1, 0
	}
	if page > pages {
		page = pages
	}
	start = (page - 1) * pageSize
	end = start + pageSize
	if end > n {
		end = n
	}
	return start, end, page, pages
}

// accounts 账号池列表（v2.1 #1）：服务端筛选/排序/分页/字段裁剪 + ETag/304。
//
// 数据源与 /overview 的 accounts 相同（pool.Snapshot() 缓存只读快照，锁外筛选/
// 排序/序列化），但只序列化当前页——旧接口默认行为完全不变，本接口供账号页按需拉取：
//
//	GET /panel/api/accounts?page=1&page_size=100&q=&state=&realm=&enterprise=&tag=&sort=&fields=
//
// tag（v2.3.1 #12 扩展）：按 account-meta.json 中该账号 tags 大小写不敏感精确匹配，
// 与 q/state/realm/enterprise AND；未知标签返回空 rows（200），未装配元数据存储时
// 同样视为无匹配。ETag 输入是归一化 accountsQuery（含 tag），因此同 tag 稳定 304、
// 不同 tag/改 meta 后 ETag 变化。
//
// v2.5.0 Perf-3：10k 账号下从 ~31ms 降到个位数毫秒——
//   - 数据源用 pool.Snapshot()（缓存、uid 升序、只读）：默认 sort=uid 零排序；
//   - 过滤只收集「快照索引」，不拷贝 527B/个的 Status；
//   - 非 uid 排序只排 []int（8B/项），仍保持旧排序口径（SliceStable + uid 兜底）；
//   - 当前页才做字段投影；ETag/304 走 LRU 缓存 + 池/元数据代际号校验，
//     5s 轮询在无变化时 <1ms 直接 304（不构造 rows）。
//
// 响应：{"ok":true,"now":<unix秒>,"total":N,"filtered":M,"page":1,"page_size":100,
// "pages":P,"rows":[Status...]}。now 供前端校正时钟后在本地按 until 倒计时。
// 超界 page 钳到最后一页、page_size=0 时 page 规范化 1（均以响应 page 为准）。
// 带 If-None-Match 且命中稳定投影 ETag 时返回 304 空 body（保留 ETag/
// Cache-Control/Vary），5s 轮询在无状态变化时零 body。
func (p *Panel) accounts(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	q := parseAccountsQuery(r)
	h := w.Header()
	h.Set("Cache-Control", accountsCacheControl)

	poolVersion := p.poolVersion()
	metaVersion := accountMetaVersionNow()
	cacheKey := q.cacheKey()

	// ---- 快速路径：池/元数据代际未变且条目未超 TTL ----
	// If-None-Match 命中缓存 ETag → 304（不读快照、不构 rows，5s 轮询常态）。
	// 未命中 304 时记录 cachedEtag：慢路径构造 body 后可直接复用该 ETag，
	// 省掉稳定投影 JSON+sha256（200 的 now/瞬态字段仍取本次请求的实时值）。
	cachedEtag := ""
	if cached, ok := p.accountsETag.lookup(cacheKey, poolVersion, metaVersion); ok {
		h.Set("ETag", cached.etag)
		if ifNoneMatch(r, cached.etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		cachedEtag = cached.etag
	}

	// ---- 慢路径：快照 → 索引过滤 → 索引排序 → 分页 → 当前页投影 ----
	// 标签筛选：一次查内存反向索引拿 uid 集合（不逐号加锁、不读盘）。
	// q.Tag 为空时 tagUIDs 为 nil = 不筛选；有 tag 但无存储/无匹配时为空/nil map
	// → 后续 map 取值为零值 false → 空 rows（符合「未知标签返回空」契约）。
	var tagUIDs map[string]struct{}
	if q.Tag != "" {
		tagUIDs = p.accountMetaUIDsForTag(q.Tag)
	}
	snapshot := p.poolSnapshot()
	total := len(snapshot)
	idxs := make([]int, 0, len(snapshot))
	for i := range snapshot {
		st := &snapshot[i]
		if q.Tag != "" {
			if _, ok := tagUIDs[st.UID]; !ok {
				continue
			}
		}
		if accountsMatch(*st, q, now) {
			idxs = append(idxs, i)
		}
	}
	if q.Sort != "uid" { // 默认 uid：Snapshot 已按 uid 升序，无需排序
		sortAccountIndexes(idxs, snapshot, q.Sort)
	}
	filtered := len(idxs)
	start, end, effPage, pages := pageSliceIndexes(filtered, q.Page, q.PageSize)
	q.Page = effPage // 生效页码（越界钳到末页 / 全部模式规范化 1）

	rows := make([]pool.Status, 0, end-start)
	for _, idx := range idxs[start:end] {
		rows = append(rows, accountsProjectionRow(snapshot[idx], q.Fields))
	}

	// ETag：命中未超 TTL 的缓存条目时直接复用（版本一致 → 内容必不变）；
	// 否则序列化当前页稳定投影后 sha256。
	etag := cachedEtag
	if etag == "" {
		var etagOK bool
		etag, etagOK = accountsETag(accountsETagInput{
			Query:    q,
			Total:    total,
			Filtered: filtered,
			Page:     q.Page,
			PageSize: q.PageSize,
			Pages:    pages,
			Rows:     accountsStableProjection(rows, q.Fields),
		})
		if !etagOK {
			etag = ""
		}
	}
	if etag != "" {
		h.Set("ETag", etag)
		p.accountsETag.store(cacheKey, accountsETagCacheEntry{
			etag:        etag,
			poolVersion: poolVersion,
			metaVersion: metaVersion,
		})
		if ifNoneMatch(r, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"now":       now.Unix(),
		"total":     total,
		"filtered":  filtered,
		"page":      q.Page,
		"page_size": q.PageSize,
		"pages":     pages,
		"rows":      rows,
	})
}
