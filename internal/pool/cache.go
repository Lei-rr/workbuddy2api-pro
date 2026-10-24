// cache.go v2.5.0 性能专项（Perf-2）：池读路径快照/摘要缓存与代际版本。
//
// 背景（基线报告 D:\dsh\tmp\perf-baseline-v241.md）：10k 账号时一次 pool.List()
// = 16.3ms/op、5.9MB/op、15k allocs/op，面板 overview/accounts/metrics 都先做它；
// overview?lite=1（344B 响应）里 48% CPU 是 ModelLockView 全池遍历。本文件提供：
//
//   - Version()：内容/状态代际。panel 用它做 ETag/缓存失效；任何会改变 Status/List
//     内容的写操作递增（Add/Remove/Sync/凭证更新/积分/冷却/禁用/暂停/签到/模型锁/
//     用量计数…）。**唯一例外**：在途请求计数（InFlight）是瞬时 gauge，且 panel 的
//     ETag 稳定投影本就把它归零，故 Acquire/Release 不递增版本——否则每一次 chat
//     请求都会打穿面板缓存。InFlight 的新鲜度由快照 TTL（500ms）兜底。
//   - Snapshot() []Status：缓存快照（按 uid 升序）。命中零分配；version 未变时直接
//     返回同一份切片。**返回切片只读**：调用方不得修改元素/排序/append，需要改动
//     请先 append([]Status(nil), snap...)。
//   - Summary()：一次遍历返回面板概况全部指标（缓存 version+TTL 500ms）。
//   - AvailableUIDsSorted()：可用账号 UID 缓存（version 变化重建，切片只读）。
//   - sortedEntriesCache：uid 排序只做一次（membership 代际，仅账号集合变化时重建）。
//
// List() 保持旧语义（每次返回新的拷贝，调用方可自由修改）；panel 热路径改走
// Snapshot()（backend-c 迁移）。
package pool

import (
	"sort"
	"time"
)

// 缓存 TTL：都取 500ms。version 变化会立即失效重建（强一致）；TTL 仅用于刷新
// 「时间自然流逝」的字段（CheckinDone 跨零点、CoolRemaining 倒计时、InFlight）。
var (
	snapshotTTL = 500 * time.Millisecond
	summaryTTL  = 500 * time.Millisecond
	availTTL    = 500 * time.Millisecond
)

// sortedEntriesCache 按 uid 升序的账号集合快照（条目指针静态、凭证可换）。
// 只在 membership 代际变化（Add/Remove/Sync/state 恢复）时重建；10k 账号排序
// 约 1.8ms，避免每次 Snapshot 都重排。
type sortedEntriesCache struct {
	version uint64
	uids    []string
	entries []*entry
}

// snapshotCacheEntry List/Snapshot 的状态快照缓存。
type snapshotCacheEntry struct {
	version uint64
	builtAt time.Time
	rows    []Status
}

// summaryCacheEntry Summary 缓存（version + TTL 500ms）。
type summaryCacheEntry struct {
	version uint64
	builtAt time.Time
	s       Summary
}

// availCacheEntry AvailableUIDsSorted 缓存。
type availCacheEntry struct {
	version uint64
	builtAt time.Time
	uids    []string
}

// Summary 面板概况的一次性聚合结果（v2.5.0 Perf-2 新 API）。
//
// 与逐个调用 CountsDetailedWithPaused/CreditsSums/List/ModelLockView 的区别：
// 一次加锁、一次遍历完成全部统计（10k 账号 <0.5ms）；ModelLocks 是「活跃的
// (realm, model) 锁行数」，无锁时零成本（不需要任何 map 分配）。
type Summary struct {
	Healthy  int
	Cooling  int
	Paused   int
	Disabled int
	Total    int
	InFlight int
	// CreditsSum 全池剩余积分合计；CreditsTotalSum 全池总额度合计
	//（未知 0/企业无限量 -1 按 0 计，与 CreditsSums 同口径）。
	CreditsSum      int64
	CreditsTotalSum int64
	// ModelLocks 当前有活跃模型级冷却的 (域, 模型) 行数（= len(ModelLockView())）。
	ModelLocks int
}

// Version 返回池内容/状态代际（单调递增，初始 0）。
//
// 语义：任何会改变 Snapshot()/List()/Status() 可见内容的写操作递增（Add/Remove/
// SyncToDir/凭证替换/积分/冷却/禁用/暂停/签到/模型锁/成功失败与用量计数…）。
// 在途请求数（Acquire/Release）是瞬时 gauge，**不递增**（见文件头注释）。
// panel 用它做 ETag/聚合缓存失效；不保证与具体字段一一对应，只保证「内容可能变了」。
func (p *Pool) Version() uint64 { return p.version.Load() }

// bumpVersionLocked 递增内容代际。调用方应持有 p.mu 写锁（或明确只改内存态）；
// 纯 atomic，无分配。命名中的 Locked 是约定而非必需——SetNickname 在锁外写凭证
// 后也直接调 p.version.Add(1)。
func (p *Pool) bumpVersionLocked() { p.version.Add(1) }

// bumpMembershipLocked 递增账号集合代际（仅 Add/Remove/Sync/state 恢复）。
func (p *Pool) bumpMembershipLocked() { p.membership.Add(1) }

// sortedEntriesLocked 返回按 uid 升序的账号集合缓存。调用方必须持有 p.mu（读锁即可）。
// membership 代际由写锁路径维护，读锁内稳定。
func (p *Pool) sortedEntriesLocked() *sortedEntriesCache {
	v := p.membership.Load()
	if c := p.sortedCache.Load(); c != nil && c.version == v {
		return c
	}
	n := len(p.byUID)
	pairs := make([]struct {
		uid string
		e   *entry
	}, 0, n)
	for uid, e := range p.byUID {
		pairs = append(pairs, struct {
			uid string
			e   *entry
		}{uid, e})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].uid < pairs[j].uid })
	c := &sortedEntriesCache{
		version: v,
		uids:    make([]string, n),
		entries: make([]*entry, n),
	}
	for i := range pairs {
		c.uids[i] = pairs[i].uid
		c.entries[i] = pairs[i].e
	}
	p.sortedCache.Store(c)
	return c
}

// List 的旧语义实现保留在 state.go（每次返回新拷贝；panel 高频路径用 Snapshot）。

// Snapshot 返回按 UID 升序的状态快照（v2.5.0 Perf-2）。
//
// 缓存语义：version 未变且距上次构建 < snapshotTTL 时直接返回缓存切片——命中
// 零分配、O(1)。version 变化（任何内容/状态写操作）或 TTL 到期后重建一次
// （10k 约 3-5ms），并发调用用 buildMu 单飞避免缓存击穿。
//
// **返回切片只读**（与 Go 生态惯例一致）：调用方不得修改元素、不得排序/append，
// 需要可变副本时先 `append([]Status(nil), snap...)`。切片生命周期由池持有，
// 下一次重建后旧切片仍可读（GC 回收），但内容不再更新。
func (p *Pool) Snapshot() []Status {
	if rows := p.cachedSnapshot(); rows != nil {
		return rows
	}
	p.snapshotBuildMu.Lock()
	defer p.snapshotBuildMu.Unlock()
	if rows := p.cachedSnapshot(); rows != nil {
		return rows
	}
	p.mu.RLock()
	ver := p.version.Load() // 写操作持写锁，读锁内版本与状态一致
	rows := p.buildStatusesLocked()
	p.mu.RUnlock()
	p.snapshotCache.Store(&snapshotCacheEntry{version: ver, builtAt: time.Now(), rows: rows})
	return rows
}

// cachedSnapshot 返回仍有效的缓存快照；无效返回 nil（需重建）。
func (p *Pool) cachedSnapshot() []Status {
	c := p.snapshotCache.Load()
	if c == nil || c.version != p.version.Load() {
		return nil
	}
	if time.Since(c.builtAt) >= snapshotTTL {
		return nil // TTL 到期：刷新倒计时/InFlight/跨零点等时间敏感字段
	}
	return c.rows
}

// buildStatusesLocked 构建全量状态切片（按 uid 升序）。调用方必须持有 p.mu 读锁。
// now 只取一次（statusOf 旧实现每号 time.Now/Format 是热点）。
func (p *Pool) buildStatusesLocked() []Status {
	now := time.Now()
	today := now.Format("2006-01-02")
	sc := p.sortedEntriesLocked()
	out := make([]Status, len(sc.uids))
	for i := range out {
		p.fillStatus(&out[i], sc.uids[i], sc.entries[i], now, today)
	}
	return out
}

// fillStatus 填充单个账号状态。now/today 由调用方统一提供，避免逐号取时/格式化。
// 调用方必须持有 p.mu 读锁（或已获取私有条目指针）；st 必须已是零值
// （buildStatusesLocked/Status 均用新分配的零值 Status，故这里不做整体清零拷贝）。
func (p *Pool) fillStatus(st *Status, uid string, e *entry, now time.Time, today string) {
	st.UID = uid
	// 限额台账（issue #36）：仅「带解析时间 6004 的模型级软冷却」仍在生效时非空。
	st.RateLimitedModels = p.rateLimitedModelsLocked(e, now)
	st.Realm = e.a.Realm()
	st.Enterprise = e.a.IsEnterprise()
	st.Nickname = e.a.Nickname
	st.Credits = e.credits
	st.CreditsTotal = e.creditsTotal
	st.CreditsExpiring = e.creditsExpiring
	st.CreditsEarliestExpiry = e.creditsEarliestExpiry
	st.CreditsEarliestRemaining = e.creditsEarliestRemaining
	// 拷贝一份：快照返回后 JSON 序列化/消费在锁外进行，不能与 NoteModelCost 的
	// 原地扣减共享底层数组（数据竞争）。
	st.CreditsExpiryBuckets = append([]ExpiryBucket(nil), e.expiryBuckets...)
	st.Cooling = now.Before(e.until) || now.Before(e.breakerUntil)
	st.Disabled = e.disabled
	st.Paused = e.paused
	st.SuccessCount = e.successCount
	st.ErrTotal = e.errTotal
	st.CheckinDone = e.lastCheckinDay == today
	st.TokenUsage = e.tokenUsage
	st.LastSuccessTime = e.lastSuccess
	st.LastErrTime = e.lastErr
	st.Until = e.until
	st.SoftStreak = e.softStreak
	st.ModelCosts = p.modelCostsStatusLocked(e, now)
	st.ConsecutiveFails = e.consecutiveFails
	st.DegradeUntil = e.degradeUntil
	st.InFlight = int(e.inFlight.Load())
	st.BreakerFails = e.fails
	st.BreakerUntil = e.breakerUntil
	// reason 仅在冷却/禁用中透出（issue #171）：避免把过期数天的历史限流原因
	// 与「可用」并排展示（判定同 v2.4.1 statusOf）。
	if st.Cooling || st.Disabled {
		st.Reason = e.reason
	}
	if st.Disabled {
		st.DisabledReason = e.reason
	}
	if st.Cooling {
		remaining := int64(0)
		if now.Before(e.until) {
			if r := int64(e.until.Sub(now).Seconds() + 0.999); r > remaining {
				remaining = r
			}
		}
		if now.Before(e.breakerUntil) {
			if r := int64(e.breakerUntil.Sub(now).Seconds() + 0.999); r > remaining {
				remaining = r
				st.CoolKind = "breaker"
			}
		}
		st.CoolRemaining = remaining
		if st.CoolKind == "" {
			st.CoolKind = e.coolKind.String()
		}
	}
}

// Summary 返回面板概况聚合（v2.5.0 Perf-2）。
//
// 缓存：version 未变且 < summaryTTL 时直接返回缓存；否则单次 RLock、单次遍历
// 完成计数/积分/在途/模型锁统计（10k <0.5ms）。InFlight 为瞬时 gauge，最多
// 500ms 陈旧（面板 5s 轮询可接受；同 List 快照口径）。
func (p *Pool) Summary() Summary {
	if s := p.cachedSummary(); s != nil {
		return *s
	}
	p.mu.RLock()
	ver := p.version.Load()
	s := p.buildSummaryLocked()
	p.mu.RUnlock()
	p.summaryCache.Store(&summaryCacheEntry{version: ver, builtAt: time.Now(), s: s})
	return s
}

// cachedSummary 返回有效缓存；nil = 需重建。
func (p *Pool) cachedSummary() *Summary {
	c := p.summaryCache.Load()
	if c == nil || c.version != p.version.Load() {
		return nil
	}
	if time.Since(c.builtAt) >= summaryTTL {
		return nil
	}
	return &c.s
}

// buildSummaryLocked 单次遍历构建 Summary。调用方必须持有 p.mu 读锁。
func (p *Pool) buildSummaryLocked() Summary {
	now := time.Now()
	var s Summary
	// 锁行去重只在真的有活跃模型锁时分配（无锁池零分配）。
	type lockKey struct{ realm, model string }
	var seen map[lockKey]struct{}
	for _, e := range p.byUID {
		s.Total++
		switch {
		case e.disabled:
			s.Disabled++
		case e.paused:
			s.Paused++
		case !e.healthy(now):
			s.Cooling++
		default:
			s.Healthy++
		}
		s.InFlight += int(e.inFlight.Load())
		s.CreditsSum += e.credits
		if e.creditsTotal > 0 {
			s.CreditsTotalSum += e.creditsTotal
		}
		if len(e.modelCooldowns) == 0 || e.disabled || e.paused {
			continue
		}
		for m, mc := range e.modelCooldowns {
			if mc.AuditOnly || mc.Until.IsZero() || !now.Before(mc.Until) {
				continue
			}
			if seen == nil {
				seen = make(map[lockKey]struct{})
			}
			seen[lockKey{e.a.Realm(), m}] = struct{}{}
		}
	}
	s.ModelLocks = len(seen)
	return s
}

// AvailableUIDsSorted 返回当前 healthy 且未占满在途名额的账号 UID（按 UID 升序），
// 供 scheduler/jobs 等消费方避免每次 O(n log n) 过滤+排序。
//
// 缓存：version 未变且 < availTTL 时直接返回缓存切片（命中零分配）；**只读**。
// 与 AvailableUIDs 的区别仅在于缓存与「不保证反映最新在途计数」（最多 500ms）。
func (p *Pool) AvailableUIDsSorted() []string {
	if uids := p.cachedAvailableUIDs(); uids != nil {
		return uids
	}
	p.mu.RLock()
	ver := p.version.Load()
	now := time.Now()
	sc := p.sortedEntriesLocked() // 已按 uid 升序，无需再排序
	uids := make([]string, 0, len(sc.uids))
	for i, e := range sc.entries {
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, sc.uids[i])
	}
	p.mu.RUnlock()
	p.availCache.Store(&availCacheEntry{version: ver, builtAt: time.Now(), uids: uids})
	return uids
}

// cachedAvailableUIDs 返回有效缓存；nil = 需重建。
func (p *Pool) cachedAvailableUIDs() []string {
	c := p.availCache.Load()
	if c == nil || c.version != p.version.Load() {
		return nil
	}
	if time.Since(c.builtAt) >= availTTL {
		return nil
	}
	return c.uids
}

// ---------------------------------------------------------------------------
// 模型锁条目计数（ModelLockView O(1) 空池快路径）
// ---------------------------------------------------------------------------

// addModelLockEntriesLocked 新增 n 条模型冷却条目（调用方持 p.mu 写锁）。
func (p *Pool) addModelLockEntriesLocked(n int) {
	if n > 0 {
		p.modelLockEntries.Add(int64(n))
	}
}

// delModelLockEntriesLocked 删除 n 条模型冷却条目（调用方持 p.mu 写锁）。
// 计数只作快路径提示：CAS 钳到 >=0，宁可多扫一次也不让计数为负而漏报。
func (p *Pool) delModelLockEntriesLocked(n int) {
	if n <= 0 {
		return
	}
	for {
		cur := p.modelLockEntries.Load()
		if cur <= 0 {
			return
		}
		next := cur - int64(n)
		if next < 0 {
			next = 0
		}
		if p.modelLockEntries.CompareAndSwap(cur, next) {
			return
		}
	}
}

// dropModelLockEntriesLocked 清空账号模型冷却表并同步计数（调用方持写锁）。
func (p *Pool) dropModelLockEntriesLocked(e *entry) {
	if n := len(e.modelCooldowns); n > 0 {
		p.delModelLockEntriesLocked(n)
		e.modelCooldowns = nil
	}
}

// modelLockEntriesTotalLocked 重算条目总数（自愈/测试注入后调用；O(n)）。
func (p *Pool) modelLockEntriesTotalLocked() int {
	total := 0
	for _, e := range p.byUID {
		total += len(e.modelCooldowns)
	}
	return total
}

// refreshModelLockEntryCountLocked 用当前各账号 modelCooldowns 重算计数器。
// 供 state 恢复与白盒测试直接注入条目后自愈（调用方持写锁或独占池）。
func (p *Pool) refreshModelLockEntryCountLocked() {
	p.modelLockEntries.Store(int64(p.modelLockEntriesTotalLocked()))
}
