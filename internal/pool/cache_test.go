// cache_test.go v2.5.0 Perf-2：版本代际与读路径缓存（Snapshot/Summary/
// AvailableUIDsSorted/ModelLockView O(1) 快路径）的语义测试。
package pool

import (
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
)

// cacheTestPool 构造 3 个账号的小池（1 个企业、2 个 cn/global 混合、部分带积分）。
func cacheTestPool(t *testing.T) *Pool {
	t.Helper()
	p := New("")
	add := func(uid, domain string, credits int64) *auth.Auth {
		a := &auth.Auth{UID: uid, AccessToken: "at-" + uid, RefreshToken: "rt-" + uid,
			ExpiresAt: time.Now().Add(time.Hour).Unix(), Domain: domain, Nickname: "nick-" + uid}
		p.Add(a)
		p.SetCredits(uid, credits, credits*2)
		return a
	}
	add("u1", "copilot.tencent.com", 100)
	add("u2", "workbuddy.ai", 200)
	add("u3", "copilot.tencent.com", 300)
	return p
}

// TestVersionBumpsOnWrites 覆盖全部会改变 Status/List 可见内容的写操作：
// 每个操作后 Version() 必须严格递增（panel 缓存/ETag 失效依据）。
func TestVersionBumpsOnWrites(t *testing.T) {
	p := cacheTestPool(t)
	uid := "u1"
	assertBump := func(name string, op func()) {
		t.Helper()
		before := p.Version()
		op()
		if after := p.Version(); after <= before {
			t.Fatalf("%s 后 Version=%d 未递增（before=%d）", name, after, before)
		}
	}

	// 账号集合/凭证。
	assertBump("Add", func() { p.Add(&auth.Auth{UID: "u9", AccessToken: "at"}) })
	assertBump("SyncToDir", func() {
		p.SyncToDir([]*auth.Auth{
			{UID: "u1", AccessToken: "at", Domain: "copilot.tencent.com"},
			{UID: "u2", AccessToken: "at", Domain: "workbuddy.ai"},
		})
	})
	// Remove：移除刚加入的账号。
	assertBump("Remove", func() {
		p.Add(&auth.Auth{UID: "u9", AccessToken: "at"})
		if got := p.Remove("u9"); got == nil {
			t.Fatal("Remove 应返回被删凭证")
		}
	})
	// SyncToDir 会剔除不在扫描结果里的 u3，后续模型锁用例需要它 → 重新加入。
	p.Add(&auth.Auth{UID: "u3", AccessToken: "at-u3", Domain: "copilot.tencent.com"})

	// 积分/余额/签到/昵称。
	assertBump("SetCredits", func() { p.SetCredits(uid, 111, 222) })
	assertBump("SetCreditsSnapshot", func() {
		p.SetCreditsSnapshot(uid, CreditsSnapshot{Remain: 123, Total: 456, Expiring: 10,
			EarliestAt: time.Now().Add(time.Hour), EarliestRemaining: 10})
	})
	assertBump("ClearExpiringSnapshots", func() { p.ClearExpiringSnapshots() })
	assertBump("NoteCheckinDone", func() { p.NoteCheckinDone(uid) })
	assertBump("SetNickname", func() {
		a := p.AuthByUID(uid)
		tmp := filepath.Join(t.TempDir(), "a.json")
		a.FilePath = tmp
		if !p.SetNickname(uid, "renamed-"+uid) {
			t.Fatal("SetNickname 应返回 true")
		}
	})

	// 冷却/模型锁/熔断相关。
	assertBump("Cooldown", func() { p.Cooldown(uid, CoolSoft, time.Minute, "429") })
	assertBump("CooldownSoftRate", func() { p.CooldownSoftRate("u2", 0, time.Now().Add(time.Minute), "429") })
	assertBump("CooldownSoftForModel", func() {
		p.CooldownSoftForModel("u3", 0, time.Now().Add(time.Hour), "glm-5.3", "6004")
	})
	assertBump("RecordModelRateLimitAudit", func() {
		p.RecordModelRateLimitAudit("u3", "kimi-k2", "6004 no reset")
	})
	assertBump("BlockModelBackoff", func() { p.BlockModelBackoff("u3", "gpt-5", "11102 not found") })
	assertBump("BlockModelClear", func() { p.BlockModelClear("u3", "gpt-5") })

	// 状态机。
	assertBump("Disable", func() { p.Disable("u2", "bench disabled") })
	assertBump("ReviveDisabled", func() { p.ReviveDisabled("u2") })
	assertBump("Pause", func() { p.Pause("u2") })
	assertBump("Resume", func() { p.Resume("u2") })
	assertBump("Revive", func() { p.Revive("u2") })
	assertBump("ReenableIfCredits", func() { p.ReenableIfCredits("u2", 999, 999) })
	assertBump("NoteError", func() { p.NoteError(uid) })
	assertBump("NoteSuccess", func() { p.NoteSuccess(uid) })
	assertBump("NoteFailures", func() { p.NoteFailures(uid) })
	assertBump("NoteModelCost", func() { p.NoteModelCost(uid, "glm-5.3", 2, 1000) })
	assertBump("RecordTokenUsage", func() {
		p.RecordTokenUsage(uid, TokenUsageDelta{HasTotalTokens: true, TotalTokens: 42, HasPromptTokens: true, PromptTokens: 20})
	})
	assertBump("NoteSessionDead", func() { p.NoteSessionDead(uid) })
	assertBump("ClearSessionDead", func() { p.ClearSessionDead(uid) })
}

// TestVersionNotBumpedByInFlight 在途计数是瞬时 gauge：Acquire/Release 不递增版本
// （否则每个 chat 请求都会打穿面板缓存），新鲜度由快照 TTL 兜底。
func TestVersionNotBumpedByInFlight(t *testing.T) {
	p := cacheTestPool(t)
	v := p.Version()
	if !p.Acquire("u1") {
		t.Fatal("Acquire 应成功")
	}
	p.Release("u1")
	if got := p.Version(); got != v {
		t.Fatalf("Acquire/Release 不应递增 Version：%d → %d", v, got)
	}
}

// TestSnapshotCacheAndReadOnly Snapshot 命中零分配（同一底层数组）、version 变化即失效、
// 只读契约下内容正确、TTL 到期重建。
func TestSnapshotCacheAndReadOnly(t *testing.T) {
	oldTTL := snapshotTTL
	snapshotTTL = 500 * time.Millisecond
	t.Cleanup(func() { snapshotTTL = oldTTL })

	p := cacheTestPool(t)
	s1 := p.Snapshot()
	if len(s1) != 3 || s1[0].UID != "u1" || s1[2].UID != "u3" {
		t.Fatalf("Snapshot 必须按 uid 升序：%v", statusUIDs(s1))
	}
	s2 := p.Snapshot()
	if len(s2) == 0 || &s1[0] != &s2[0] {
		t.Fatal("version 未变时应返回同一份缓存切片（零分配）")
	}
	if got := p.Snapshot(); got[0].Nickname != "nick-u1" {
		t.Fatalf("nickname=%q", got[0].Nickname)
	}

	// version 变化 → 立即重建。
	p.SetCredits("u1", 987, 1000)
	s3 := p.Snapshot()
	if s3[0].Credits != 987 {
		t.Fatalf("SetCredits 后 Snapshot credits=%d want 987", s3[0].Credits)
	}
	if &s3[0] == &s1[0] {
		t.Fatal("version 变化后必须重建快照")
	}

	// TTL 到期（version 不变）→ 重建以刷新时间敏感字段。
	snapshotTTL = 5 * time.Millisecond
	time.Sleep(15 * time.Millisecond)
	s4 := p.Snapshot()
	if &s4[0] == &s3[0] {
		t.Fatal("TTL 到期后应重建快照（刷新 InFlight/倒计时/跨零点）")
	}
}

// TestSnapshotMatchesList Snapshot 与 List 内容/顺序一致（List 保持旧语义）。
func TestSnapshotMatchesList(t *testing.T) {
	p := cacheTestPool(t)
	p.Disable("u2", "x")
	p.Cooldown("u3", CoolSoft, time.Minute, "429")
	p.CooldownSoftForModel("u1", 0, time.Now().Add(time.Hour), "glm-5.3", "6004")
	p.NoteModelCost("u1", "glm-5.3", 1, 1000)

	list := p.List()
	snap := p.Snapshot()
	if len(list) != len(snap) {
		t.Fatalf("len List=%d Snapshot=%d", len(list), len(snap))
	}
	for i := range list {
		// 时间敏感字段独立于两个时间点：比较稳定投影（UID/积分/状态/台账）。
		if list[i].UID != snap[i].UID || list[i].Credits != snap[i].Credits ||
			list[i].Cooling != snap[i].Cooling || list[i].Disabled != snap[i].Disabled ||
			len(list[i].RateLimitedModels) != len(snap[i].RateLimitedModels) ||
			len(list[i].ModelCosts) != len(snap[i].ModelCosts) {
			t.Fatalf("第 %d 行 List=%+v Snapshot=%+v 不一致", i, list[i], snap[i])
		}
	}
}

// TestSummaryMatchesLegacy Summary 一次聚合与旧的多次调用口径一致。
func TestSummaryMatchesLegacy(t *testing.T) {
	oldTTL := summaryTTL
	summaryTTL = 500 * time.Millisecond
	t.Cleanup(func() { summaryTTL = oldTTL })

	p := cacheTestPool(t)
	p.Disable("u1", "dead")
	p.Pause("u2")
	p.CooldownSoftForModel("u3", 0, time.Now().Add(time.Hour), "glm-5.3", "6004")
	p.Cooldown("u3", CoolSoft, time.Minute, "429") // 账号级冷却：u3 cooling（同时清路由锁）

	s := p.Summary()
	total, healthy, cooling, disabled, paused, _ := p.CountsDetailedWithPaused()
	credits, creditsTotal := p.CreditsSums()
	inFlight := 0
	for _, st := range p.List() {
		inFlight += st.InFlight
	}
	modelLocks := len(p.ModelLockView())

	if s.Total != total || s.Healthy != healthy || s.Cooling != cooling ||
		s.Disabled != disabled || s.Paused != paused {
		t.Fatalf("Summary counts=%+v vs legacy total=%d healthy=%d cooling=%d disabled=%d paused=%d",
			s, total, healthy, cooling, disabled, paused)
	}
	if s.CreditsSum != credits || s.CreditsTotalSum != creditsTotal {
		t.Fatalf("Summary credits=(%d,%d) vs legacy (%d,%d)", s.CreditsSum, s.CreditsTotalSum, credits, creditsTotal)
	}
	if s.InFlight != inFlight || s.ModelLocks != modelLocks {
		t.Fatalf("Summary inFlight=%d locks=%d vs legacy %d/%d", s.InFlight, s.ModelLocks, inFlight, modelLocks)
	}

	// 缓存命中（version 未变）返回同一值；写入后 version 变化 → 立即重新聚合。
	if again := p.Summary(); again != s {
		t.Fatalf("缓存命中应返回相同 Summary：%+v vs %+v", again, s)
	}
	p.SetCredits("u2", 5000, 6000)
	if got := p.Summary(); got.CreditsSum != s.CreditsSum+4800 {
		t.Fatalf("SetCredits 后 Summary.CreditsSum=%d want %d", got.CreditsSum, s.CreditsSum+4800)
	}
}

// TestAvailableUIDsSorted 新排序缓存与旧 AvailableUIDs 口径一致、只读缓存随 version 失效。
func TestAvailableUIDsSorted(t *testing.T) {
	oldTTL := availTTL
	availTTL = 500 * time.Millisecond
	t.Cleanup(func() { availTTL = oldTTL })

	p := cacheTestPool(t)
	legacy := p.AvailableUIDs()
	sort.Strings(legacy)
	cached := p.AvailableUIDsSorted()
	if !reflect.DeepEqual(cached, legacy) {
		t.Fatalf("AvailableUIDsSorted=%v vs AvailableUIDs=%v", cached, legacy)
	}
	if again := p.AvailableUIDsSorted(); len(again) > 0 && &again[0] != &cached[0] {
		t.Fatal("version 未变时应命中缓存（同一底层数组）")
	}
	p.Cooldown("u2", CoolSoft, time.Minute, "429")
	got := p.AvailableUIDsSorted()
	want := []string{"u1", "u3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("冷却后 AvailableUIDsSorted=%v want %v", got, want)
	}
}

// TestModelLockViewFastPath ModelLockView 空池 O(1) 快路径正确性 + 计数维护。
func TestModelLockViewFastPath(t *testing.T) {
	p := cacheTestPool(t)
	if rows := p.ModelLockView(); rows != nil {
		t.Fatalf("无锁应返回 nil：%+v", rows)
	}
	if n := p.modelLockEntries.Load(); n != 0 {
		t.Fatalf("无锁时条目计数=%d want 0", n)
	}

	// 写入真实模型锁 → 计数 +2，视图按 realm 分行（u1=cn、u2=global 各一行）。
	p.CooldownSoftForModel("u1", 0, time.Now().Add(time.Hour), "glm-5.3", "6004")
	p.CooldownSoftForModel("u2", 0, time.Now().Add(time.Hour), "glm-5.3", "6004")
	if n := p.modelLockEntries.Load(); n != 2 {
		t.Fatalf("两个模型锁后条目计数=%d want 2", n)
	}
	rows := p.ModelLockView()
	if len(rows) != 2 {
		t.Fatalf("ModelLockView=%+v want 2 行（cn/global 各一）", rows)
	}
	locked := 0
	for _, r := range rows {
		if r.Model != "glm-5.3" {
			t.Fatalf("行 model=%q", r.Model)
		}
		locked += r.Locked
	}
	if locked != 2 {
		t.Fatalf("两行 Locked 合计=%d want 2", locked)
	}

	// 账号级冷却清掉路由豁免（保留 AuditOnly）→ 计数归零、视图回到 nil。
	p.Cooldown("u1", CoolSoft, time.Minute, "429")
	p.Cooldown("u2", CoolSoft, time.Minute, "429")
	if n := p.modelLockEntries.Load(); n != 0 {
		t.Fatalf("路由锁清除后条目计数=%d want 0", n)
	}
	if rows := p.ModelLockView(); rows != nil {
		t.Fatalf("锁清除后应返回 nil：%+v", rows)
	}
}

// TestStatusSingleAccount Status(uid) 仍为 O(1) 单号查询且字段与 Snapshot 对应行一致。
func TestStatusSingleAccount(t *testing.T) {
	p := cacheTestPool(t)
	p.SetCreditsSnapshot("u2", CreditsSnapshot{Remain: 321, Total: 654, Expiring: 21,
		EarliestAt: time.Now().Add(48 * time.Hour), EarliestRemaining: 21})
	st, ok := p.Status("u2")
	if !ok {
		t.Fatal("Status(u2) 应存在")
	}
	if st.UID != "u2" || st.Credits != 321 || st.CreditsTotal != 654 {
		t.Fatalf("Status(u2)=%+v", st)
	}
	if st.Realm != "global" {
		t.Fatalf("workbuddy.ai 域应为 global，得到 %q", st.Realm)
	}
}

// statusUIDs 提取 uid 列表（测试断言用）。
func statusUIDs(rows []Status) []string {
	out := make([]string, len(rows))
	for i := range rows {
		out[i] = rows[i].UID
	}
	return out
}
