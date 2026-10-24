package pool

import (
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
)

func TestReenableIfCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 500, 0)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("should reenable, pick=%+v", got)
	}
}

func TestReenableZeroCreditsKeepsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 0, 0)
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatal("zero credits should stay cooling")
	}
}

func TestReenableDoesNotTouchDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.ReenableIfCredits("u1", 500, 0)
	if p.Pick() != nil {
		t.Fatal("disabled must not auto-reenable")
	}
}

func TestReenableClearsCoolingNotBreaker(t *testing.T) {
	// C5：签到解冻只清冷却（until/coolKind/reason）+ 更新 credits，不清熔断
	// （fails/retryCount/breakerUntil）。签到成功只证明余额与 billing 通道恢复，
	// 不证明 chat 通道健康——熔断仍按 breakerUntil 退避到期或 NoteSuccess 恢复。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足") // 硬冷却（喂 fails，但此时阈值默认 3，不熔断）
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // 触发熔断（fails→阈值1→fails=0, retryCount=1, breakerUntil 非零）
	p.ReenableIfCredits("u1", 500, 0)
	st, _ := p.Status("u1")
	if st.Reason != "" || st.Credits != 500 {
		t.Errorf("signin should clear reason + set credits=500: %+v", st)
	}
	if st.Until != (time.Time{}) {
		t.Errorf("signin should clear hard-cooling until: %+v", st.Until)
	}
	if st.BreakerUntil.IsZero() {
		t.Fatal("signin must NOT clear breakerUntil (chat health unresolved)")
	}
	// 熔断仍在 → 账号仍不可选（直至 breakerUntil 到期）。
	if p.internalHealthy("u1") {
		t.Fatal("account should stay unhealthy while breaker active after signin")
	}
}

func TestReenableKeepsBreaker(t *testing.T) {
	// C5 回归锁定新语义：仅熔断（无冷却）的账号，签到解冻不得清熔断。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // 触发熔断
	if bt, _ := p.breakerUntil("u1"); bt.IsZero() {
		t.Fatal("precondition: breaker should be open")
	}
	p.ReenableIfCredits("u1", 500, 0)
	if bt, _ := p.breakerUntil("u1"); bt.IsZero() {
		t.Fatal("signin must not clear breakerUntil")
	}
	if p.internalHealthy("u1") {
		t.Fatal("account should stay unhealthy while breaker active after signin")
	}
}

// ---------- 暂停选号（paused）----------
//
// paused 与 disabled 正交：两者都退出选号候选，但 paused 是「临时让位」——
// 不清冷却域、不写 reason，且**保号任务照常参与**（见 scheduler 侧测试）。

// TestPauseExitsRouting 暂停号退出选号（Pick 含全冷却兜底都不参与）。
func TestPauseExitsRouting(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if !p.Pause("u1") {
		t.Fatal("Pause 对存在的账号应返回 true")
	}
	if got := p.Pick(); got != nil {
		t.Fatalf("暂停号不应被选中, got %+v", got)
	}
	st, _ := p.Status("u1")
	if !st.Paused {
		t.Errorf("Status.Paused 应为 true: %+v", st)
	}
	if st.Disabled {
		t.Errorf("暂停不是禁用：Disabled 必须为 false: %+v", st)
	}
}

// TestPauseExcludesFromFallback 全冷却兜底同样不捞回暂停号（与 disabled 同口径）。
func TestPauseExcludesFromFallback(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Pause("u1")
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limited") // 迫使命中兜底路径
	if got := p.Pick(); got != nil {
		t.Fatalf("want nil（暂停号不入兜底）, got %+v", got)
	}
}

// TestPauseDoesNotTouchCoolingDomain 暂停**不**清冷却域——与 disable 的关键区别：
// disable 是终态故清冷却，pause 是临时态故保留观测（恢复后仍有效）。
func TestPauseDoesNotTouchCoolingDomain(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limited")
	p.Pause("u1")
	st, _ := p.Status("u1")
	if !st.Cooling || st.Reason != "429 rate limited" {
		t.Errorf("Pause 不应清冷却/原因: %+v", st)
	}
}

// TestDisableClearsPaused 禁用是比暂停更强的终态：二者不叠加。
func TestDisableClearsPaused(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Pause("u1")
	p.Disable("u1", "12153 session dead")
	st, _ := p.Status("u1")
	if st.Paused {
		t.Errorf("disable 后 paused 应被清（不叠加）: %+v", st)
	}
	if !st.Disabled {
		t.Errorf("应为 disabled: %+v", st)
	}
}

// TestReviveClearsPaused 运维「解冻」是全清：paused 一并解除。
func TestReviveClearsPaused(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Pause("u1")
	if !p.Revive("u1") {
		t.Fatal("Revive 应返回 true")
	}
	if st, _ := p.Status("u1"); st.Paused {
		t.Errorf("Revive 应清 paused: %+v", st)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Errorf("Revive 后应可选, got %+v", got)
	}
}

// TestPauseUnknownUIDReturnsFalse 不存在的 uid 返回 false（供面板区分 404）。
func TestPauseUnknownUIDReturnsFalse(t *testing.T) {
	p := New("")
	if p.Pause("nope") {
		t.Error("Pause 未知 uid 应返回 false")
	}
	if p.Resume("nope") {
		t.Error("Resume 未知 uid 应返回 false")
	}
}

// TestResumeIdempotent 对未暂停账号 Resume 是空操作（幂等，不改变状态）。
func TestResumeIdempotent(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if !p.Resume("u1") {
		t.Fatal("Resume 幂等应返回 true")
	}
	if st, _ := p.Status("u1"); st.Paused {
		t.Errorf("Paused 应仍为 false: %+v", st)
	}
}
