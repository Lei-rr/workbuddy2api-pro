package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
)

// ---------------------------------------------------------------------------
// issue #31：429 6004 模型级限流 → 按上游重置时间收窄冷却 + 模型级豁免选号
// ---------------------------------------------------------------------------

func TestCooldownSoftForModelParsedUntil(t *testing.T) {
	// 6004 msg 带「将在 … 重置」→ 该模型的独立冷却截止精确等于解析时间（wall-clock 判断）。
	// 用未来 5 分钟的时间戳：解析后 Until ≈ now+5m，远短于固定 600s 基数的指数退避，
	// 证明"上游明说重置时间"优先于"600s 起指数退避"。
	// 新语义：只写 modelCooldowns（不写账号级 until）→ 账号不 cooling、台账单行。
	reset := time.Now().Add(5 * time.Minute)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "429 rate limit")
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("account missing")
	}
	if st.Cooling {
		t.Fatalf("6004-with-reset should NOT set account-level cooling: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("want single model ledger row for glm-5.3: %+v", st.RateLimitedModels)
	}
	until := st.RateLimitedModels[0].Until
	if d := until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("model until=%v want ~reset=%v (diff %v)", until, reset, d)
	}
}

func TestCooldownSoftForModelCappedBySoftRateMax(t *testing.T) {
	// 解析时间超出 soft_rate_max → 截断到 soft_rate_max（不无限期拉黑）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(10 * time.Minute)
	reset := time.Now().Add(2 * time.Hour) // 远超过封顶 10m
	before := time.Now()
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "429 rate limit")
	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("want model ledger row: %+v", st.RateLimitedModels)
	}
	if st.RateLimitedModels[0].Until.Sub(before) > 10*time.Minute+time.Second {
		t.Errorf("model until=%v want capped at soft_rate_max=10m", st.RateLimitedModels[0].Until)
	}
}

func TestCooldownSoftForModelNoResetFallbackBackoff(t *testing.T) {
	// 无解析时间（resetAt 零值）→ 有界退避（600s 起）。冷却中的兜底探测再撞 429
	// **不推进 streak、不延长**（吸收上游：旧「每次探测都翻倍」正是全池被推到
	// 2h 封顶的元凶）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(time.Hour)
	p.CooldownSoftForModel("u1", 600*time.Second, time.Time{}, "", "429 rate limit")
	wantCoolSec(t, p, "u1", 600, 3)
	p.CooldownSoftForModel("u1", 600*time.Second, time.Time{}, "", "429 rate limit")
	wantCoolSec(t, p, "u1", 600, 3) // 仍在冷却中：不堆加
}

// TestPickExcludingForModelSkipsSoftCoolingSameModel 冷却中账号（6004 带解析时间，
// 已记录模型）+ 同 model 请求 → 仍不可选（现状语义保持）。
func TestPickExcludingForModelSkipsSoftCoolingSameModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → 最高分 u1
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	got := p.PickExcludingForModel(nil, "glm-5.3")
	if got == nil || got.UID != "u2" {
		t.Fatalf("same-model request must skip cooling u1, got %+v", got)
	}
}

// TestPickExcludingForModelAllowsDifferentModel 6004 冷却中的账号 + 不同 model
// → 视为可用，可选到该号（真·单模型限流，切模型立即可用）。
func TestPickExcludingForModelAllowsDifferentModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1000, 0)
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → 最高分 u1
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	got := p.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u1" {
		t.Fatalf("different-model request should bypass u1 soft cooling, got %+v", got)
	}
}

// TestPickExcludingForModelBreakerStillBlocks 模型豁免只豁免软冷却维度，
// 熔断（breakerUntil）仍拦截：6004 冷却 + 熔断中的账号，切模型也不可选。
func TestPickExcludingForModelBreakerStillBlocks(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // u1 熔断
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	got := p.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u2" {
		t.Fatalf("breaker must still block, got %+v", got)
	}
}

// TestSoftRateModelClearedByPlainCooldown 回归：6004 模型冷却后，若账号又经历一次
// **非模型级**软冷却（plain Cooldown），softRateModel 必须被清空——否则上次 6004 的
// 模型豁免会泄漏到本次账号级限流上，导致"换模型请求"错误绕过本次冷却。
func TestSoftRateModelClearedByPlainCooldown(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })

	// 1) 6004 带解析时间 → 记录模型 glm-5.3。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	if got := p.PickExcludingForModel(nil, "hy3-x"); got == nil || got.UID != "u1" {
		t.Fatalf("precondition: different-model should bypass, got %+v", got)
	}
	// 2) 账号恢复后经历普通账号级软冷却（无模型语义）。
	p.NoteSuccess("u1") // 还原 fresh 状态（Cooldown 会重设 until）
	p.Cooldown("u1", CoolSoft, time.Minute, "429 rate limit")
	// 3) 换模型请求不得再豁免（softRateModel 已清空）。
	got := p.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u2" {
		t.Fatalf("plain cooldown must clear softRateModel (no bypass), got %+v", got)
	}
}

// TestSoftRateModelNotPersistedToState 模型级独立冷却（modelCooldowns）是运行态：
// 落盘不引入该字段，重启清零（退化为仅账号级 until 冷却的现状）。
// 新语义：6004 带重置时间只写 modelCooldowns、不写 until → 重载后账号不冷却、
// 台账为空。
func TestSoftRateModelPersistsToState(t *testing.T) {
	// 6004 重置墙钟可长达数小时，跨重启是常态：model_cooldowns 持久化，
	// 恢复后 healthyForModel 不失忆（吸收上游 2f4c77b）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	// resetAt 必须显著晚于 now：传 time.Now() 会走"重置时间已过"分支把冷却压到 1ms。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(10*time.Minute), "glm-5.3", "429 rate limit")
	p.Flush()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "model_cooldowns") {
		t.Errorf("state.json 应持久化 model_cooldowns: %s", raw)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatalf("account missing after reload")
	}
	if st.Cooling {
		t.Fatalf("6004-with-reset 不写账号级 until，重载后不应 cooling: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Errorf("modelCooldowns should survive reload, got %+v", st.RateLimitedModels)
	}
}
