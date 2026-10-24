// availability.go v2.6.0（Audit-1 P0-2）：会话粘性 (model, realm) 可用账号视图缓存。
//
// 背景：v2.5.0 发现 session sticky 每个 chat 请求都会走
// session.ResolveForModel → availableSet → pool.WeightedAvailableUIDsForModelRealm：
// 10k 账号下每次请求全池遍历 + 逐号 Realm()/healthyForModel + sort + 虚拟槽展开 +
// map 拷贝（ms 级 CPU + 大量分配）。本文件在 pool 侧缓存「按 (model, realm) 的可用
// uid 权重槽视图」，session 快路径只做 O(1) Contains，慢路径直接复用缓存切片做哈希分配。
//
// 失效语义（两段式；为什么不用 p.version 见下）：
//   - membership 代际：Add/Remove/SyncToDir/state 恢复（账号集合变化）时立刻失效——
//     面板增删账号后的下一个请求就生效；
//   - TTL（500ms）：兜底刷新时间自然流逝/状态迁移带来的变化（冷却到期、在途变化、
//     禁用/暂停/模型锁、积分保底漂移）。窗口内候选集可能含刚不可用的账号，但 session
//     只把它当「候选集」：最终选号仍由 pool.Pick*/PickByUIDForModel 做权威健康校验，
//     不可用号会被拒绝并回落普通轮换，不会绕过健康检查。
//   - 为什么**不**用 p.version 做失效：version 覆盖所有可见写操作，包括每个 chat 请求
//     都会递增的成功/用量计数（NoteSuccess/RecordTokenUsage）——用它做缓存键会让缓存
//     每个请求都失效，退化为修复前的 O(n)。membership + TTL 才能同时保证正确性与命中率。
//   - 容量上限（maxAvailViews）：客户端可传任意模型名，map 超过上限整体清空（最坏
//     多冷启一次，无泄漏）。
package pool

import (
	"time"
)

// availViewTTL 可用视图最长陈旧时间（包级 var 便于测试覆盖）。10k 下重建约 2-4ms，
// 500ms 摊薄到 <1% CPU；更短则命中率下降，更长则状态迁移陈旧窗口变大。
var availViewTTL = 500 * time.Millisecond

// maxAvailViews (model, realm) 缓存条目上限（模型名由客户端指定，必须防无界增长）。
const maxAvailViews = 256

// AvailabilityView 按 (model, realm) 的可用账号只读视图（session sticky 使用）。
//
// Contains O(1) 哈希命中；slots 是按 uid 升序、对快过期账号重复 expiringVirtualSlots
// 次的权重槽列表（与 WeightedAvailableUIDsForModelRealm 完全同构，保留既有双段哈希
// 分配的加权语义）。**只读**：调用方不得修改 slots。
type AvailabilityView struct {
	slots []string
	set   map[string]struct{}
}

// Contains 报告 uid 是否在可用集合内（O(1)）。
func (v *AvailabilityView) Contains(uid string) bool {
	if v == nil || v.set == nil {
		return false
	}
	_, ok := v.set[uid]
	return ok
}

// UIDs 返回权重槽列表（uid 升序；只读，不得修改/排序；可为空切片）。
func (v *AvailabilityView) UIDs() []string {
	if v == nil {
		return nil
	}
	return v.slots
}

// Len 返回权重槽数量（虚拟槽计入；session 哈希取模用）。
func (v *AvailabilityView) Len() int {
	if v == nil {
		return 0
	}
	return len(v.slots)
}

// availViewKey 缓存键：模型名与域（跨域同名模型隔离，与既有 realm 语义一致）。
type availViewKey struct {
	model string
	realm string
}

// availViewEntry 单条缓存。
type availViewEntry struct {
	gen     uint64 // 构建时的 membership 代际（账号集合变化即失效）
	builtAt time.Time
	view    *AvailabilityView
}

// AvailableViewForRealm 返回 (model, realm) 的可用账号视图（缓存；O(1) 命中）。
//
// 与 WeightedAvailableUIDsForModelRealm 的口径完全一致：healthyForModel（含 6004
// 模型豁免）+ 未占满在途；快过期账号按 expiringVirtualSlots 展开虚拟槽。
// realm=="" 退化为全池口径（与既有语义一致）。返回视图只读，生命周期由池持有。
func (p *Pool) AvailableViewForRealm(model, realm string) *AvailabilityView {
	key := availViewKey{model: model, realm: realm}
	now := time.Now()
	gen := p.membership.Load()

	p.availVMu.Lock()
	if e := p.availViews[key]; e != nil && e.gen == gen && now.Sub(e.builtAt) < availViewTTL {
		v := e.view
		p.availVMu.Unlock()
		return v
	}
	p.availVMu.Unlock()

	v := p.buildAvailabilityView(model, realm, now)

	p.availVMu.Lock()
	if len(p.availViews) >= maxAvailViews {
		// 简单整体清空：命中率短暂下降好过无界内存；清空后本次条目仍写入。
		p.availViews = make(map[availViewKey]*availViewEntry, 8)
	}
	p.availViews[key] = &availViewEntry{gen: gen, builtAt: now, view: v}
	p.availVMu.Unlock()
	return v
}

// buildAvailabilityView 构建可用视图。调用方不持锁；内部持 RLock 遍历（与
// WeightedAvailableUIDsForModelRealm 相同的只读遍历，快照式生成后即与池状态解耦）。
func (p *Pool) buildAvailabilityView(model, realm string, now time.Time) *AvailabilityView {
	p.mu.RLock()
	defer p.mu.RUnlock()

	sc := p.sortedEntriesLocked() // uid 升序，避免额外排序
	slots := make([]string, 0, len(sc.uids))
	set := make(map[string]struct{}, len(sc.uids))
	for i, e := range sc.entries {
		uid := sc.uids[i]
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		set[uid] = struct{}{}
		slots = append(slots, uid)
		if p.preferExpiring && expiringNow(e, now) {
			// 快过期账号的虚拟槽权重：与旧实现 expiringVirtualSlots 同源。
			for k := 1; k < expiringVirtualSlots; k++ {
				slots = append(slots, uid)
			}
		}
	}
	return &AvailabilityView{slots: slots, set: set}
}

// weightedAvailableUIDsFresh 返回与 WeightedAvailableUIDsForModelRealm 同口径的
// 新鲜权重槽（测试对照/诊断用，不参与生产热路径）。
func (p *Pool) weightedAvailableUIDsFresh(model, realm string) []string {
	return p.buildAvailabilityView(model, realm, time.Now()).slots
}

// resetAvailViewsForTest 清空可用视图缓存（测试隔离用；生产不调用）。
func (p *Pool) resetAvailViewsForTest() {
	p.availVMu.Lock()
	p.availViews = make(map[availViewKey]*availViewEntry, 8)
	p.availVMu.Unlock()
}
