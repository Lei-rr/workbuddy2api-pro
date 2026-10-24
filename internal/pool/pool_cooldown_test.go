package pool

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
)

func TestNextDay4AMBoundaries(t *testing.T) {
	cases := []struct {
		name string
		now  string // RFC3339 (UTC 表示)
		want string // 下一个 04:00（同一时区，UTC 表示）
	}{
		{"普通日", "2026-08-28T17:00:00+08:00", "2026-08-29T04:00:00+08:00"},
		// 凌晨 00:00~04:00 触发硬冷却：当天 04:00 尚未到，冷却应落在当天（而非次日），
		// 否则多冷约一天（原 bug）。
		{"凌晨02:30", "2026-08-28T02:30:00+08:00", "2026-08-28T04:00:00+08:00"},
		{"凌晨00:00", "2026-08-28T00:00:00+08:00", "2026-08-28T04:00:00+08:00"},
		{"凌晨03:59:59", "2026-08-28T03:59:59+08:00", "2026-08-28T04:00:00+08:00"},
		{"正好4点", "2026-08-28T04:00:00+08:00", "2026-08-29T04:00:00+08:00"},
		{"4点刚过", "2026-08-28T04:00:01+08:00", "2026-08-29T04:00:00+08:00"},
		{"月末(31天月)", "2026-01-31T12:00:00+08:00", "2026-02-01T04:00:00+08:00"},
		{"月末(28天月)", "2026-02-28T12:00:00+08:00", "2026-03-01T04:00:00+08:00"},
		{"闰年月末", "2028-02-29T12:00:00+08:00", "2028-03-01T04:00:00+08:00"},
		{"年末", "2026-12-31T23:59:59+08:00", "2027-01-01T04:00:00+08:00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, c.now)
			if err != nil {
				t.Fatal(err)
			}
			want, err := time.Parse(time.RFC3339, c.want)
			if err != nil {
				t.Fatal(err)
			}
			if got := nextDay4AM(now); !got.Equal(want) {
				t.Errorf("nextDay4AM(%v)=%v want %v", c.now, got, want)
			}
		})
	}
}

func TestCooldownUntilTomorrow4AM(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	before := time.Now()
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	after := time.Now()
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if !st.Cooling {
		t.Fatalf("should be cooling: %+v", st)
	}
	if st.Reason != "余额不足" {
		t.Errorf("reason=%q", st.Reason)
	}
	// 冷却截止必须是"此刻之后的最近一个 04:00"：晚于 now、距今不超过 24h
	//（凌晨 00:00~04:00 触发时落在当天 04:00，其余时段落在次日 04:00，跨度恒 < 24h）。
	if st.Until.Before(after) {
		t.Errorf("until %v is in the past (call span %v..%v)", st.Until, before, after)
	}
	if st.Until.Hour() != 4 {
		t.Errorf("until hour=%d want 4", st.Until.Hour())
	}
	if d := st.Until.Sub(after); d > 24*time.Hour {
		t.Errorf("until %v is more than 24h out: %v", st.Until, d)
	}
	// 全冷却时余额耗尽（hard）号不参与兜底 → 返回 nil（等签到恢复）。
	if got := p.Pick(); got != nil {
		t.Fatalf("all-hard-cooling should return nil (hard excluded from fallback), got %+v", got)
	}
}

func TestCooldownSoftExponentialBackoff(t *testing.T) {
	// 同一账号**跨冷却期**连续软限流 → 时长按 2 倍指数增长（吸收上游：冷却中的
	// 兜底探测不再堆加，堆加只发生在到期后的新一轮限流）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(time.Hour) // 封顶 1h：本用例三步（600/1200/2400）都不触及

	for i, want := range []int64{600, 1200, 2400} {
		p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit")
		wantCoolSec(t, p, "u1", want, 3)
		if st, _ := p.Status("u1"); st.SoftStreak != i+1 {
			t.Errorf("after call %d: soft_streak=%d want %d", i+1, st.SoftStreak, i+1)
		}
		expireCooldown(p, "u1") // 模拟冷却到期后再撞新一轮限流
	}
}

func TestCooldownSoftCappedBySoftRateMax(t *testing.T) {
	// 注入封顶：streak 3 的 400s 被压到 250s（跨冷却期堆加）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(250 * time.Second)

	p.CooldownSoftRate("u1", 100*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 100, 3)
	expireCooldown(p, "u1")
	p.CooldownSoftRate("u1", 100*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 200, 3)
	expireCooldown(p, "u1")
	p.CooldownSoftRate("u1", 100*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 250, 3)
}

func TestCooldownSoftDefaultCapWhenUnset(t *testing.T) {
	// 未注入 softRateMax → 按 2h 封顶（避免裸用池时退避无上限）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i, want := range []int64{600, 1200, 2400, 4800, 7200} {
		p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
		wantCoolSec(t, p, "u1", want, 3)
		if st, _ := p.Status("u1"); st.SoftStreak != i+1 {
			t.Errorf("soft_streak=%d want %d", st.SoftStreak, i+1)
		}
		expireCooldown(p, "u1")
	}
}

func TestSetSoftRateMaxIgnoresNonPositive(t *testing.T) {
	// 非正值保留原值（风格同 SetBreaker）：0 不应把封顶清零导致无上限。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(0)
	p.SetSoftRateMax(-time.Second)
	for i := 0; i < 6; i++ {
		p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
		if i < 5 {
			expireCooldown(p, "u1") // 最后一轮不回拨：断言时须仍在冷却中
		}
	}
	wantCoolSec(t, p, "u1", 7200, 3) // 仍是 2h 封顶（第 6 步 19200s → 7200s）
}

func TestCooldownSoftStreakResetBySuccess(t *testing.T) {
	// 成功即证明账号恢复 → streak 归零，下次软冷却回到基数。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	expireCooldown(p, "u1")
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 1200, 3)

	p.NoteSuccess("u1")
	if st, _ := p.Status("u1"); st.SoftStreak != 0 {
		t.Fatalf("success should reset soft_streak, got %d", st.SoftStreak)
	}
	expireCooldown(p, "u1") // 新一轮限流（上一轮冷却已过/已被成功重置）
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 600, 3)
}

func TestCooldownSoftKeptByReenable(t *testing.T) {
	// 签到/余额刷新解冻（reviveCoolingLocked）只解冻余额耗尽冷却（CoolHard）；
	// 软限流冷却（CoolSoft）与 softStreak 保留——限流恢复证据是重置墙钟/退避到期，
	// 不是余额恢复（余额刷新每 5 分钟一次，若在此清冷却域，限流保护实际寿命被压到
	// 一个刷新周期内）。熔断域（fails/retryCount/breakerUntil）不动，与既有 C5 语义一致。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	expireCooldown(p, "u1") // 跨冷却期第二次限流，streak 累计到 2（冷却中重复触发不堆叠，#152）
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	failsBefore := p.breakerFails("u1")

	p.ReenableIfCredits("u1", 500, 0)
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Errorf("reenable 不得解除软限流冷却: %+v", st)
	}
	if st.SoftStreak != 2 {
		t.Errorf("reenable 不得清零软限流退避计数 soft_streak, got %d want 2", st.SoftStreak)
	}
	if failsAfter := p.breakerFails("u1"); failsAfter != failsBefore {
		t.Errorf("reenable must not touch breaker: fails %d → %d", failsBefore, failsAfter)
	}

	// 退避延续：第 3 次触发从既有 streak=2 继续 → 600s<<2 = 2400s（而非归零后的 600s）。
	expireCooldown(p, "u1")
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 2400, 3)
}

func TestCooldownHardDoesNotAdvanceSoftStreak(t *testing.T) {
	// 硬冷却（余额耗尽）时长由签到时点决定，不参与软退避指数。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	if st, _ := p.Status("u1"); st.SoftStreak != 0 {
		t.Fatalf("hard cooldown must not touch soft_streak, got %d", st.SoftStreak)
	}
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 600, 3)
}

func TestSoftStreakMissingInLegacyStateFile(t *testing.T) {
	// 旧 state.json 无 soft_streak → 零值兼容，退避从基数重新开始。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":100}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	if st, _ := p.Status("u1"); st.SoftStreak != 0 {
		t.Fatalf("legacy file should load soft_streak=0, got %d", st.SoftStreak)
	}
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 600, 3)
}

// TestCooldownSoftWithoutModelRecordsNone 非 6004 的普通软冷却（resetAt 零值，
// 不记录 softRateModel）→ 不因模型切换而豁免（现状语义）。
func TestCooldownSoftWithoutModelRecordsNone(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.CooldownSoftForModel("u1", time.Minute, time.Time{}, "", "429 rate limit")
	// 冷却中 + 不同 model 请求仍跳过 u1（无 softRateModel，不豁免）。
	got := p.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u2" {
		t.Fatalf("no model recorded → must not bypass, got %+v", got)
	}
}
