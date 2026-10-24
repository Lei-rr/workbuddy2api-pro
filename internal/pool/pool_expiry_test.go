package pool

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
)

func TestPickPrefersExpiringByVirtualWeight(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "later"})
	p.Add(&auth.Auth{UID: "soon"})
	p.Add(&auth.Auth{UID: "none"})

	now := time.Now()
	p.SetCreditsDetailed("later", 100, 100, 100, now.Add(48*time.Hour), 100)
	p.SetCreditsDetailed("soon", 100, 100, 100, now.Add(2*time.Hour), 100)
	p.SetCreditsDetailed("none", 100, 100, 0, time.Time{}, 0)

	// 3:1 虚拟实例是软偏好而非硬优先。用确定性随机源统计长期分布：两个快过期
	// 账号的合计份额应显著高于普通账号，同时普通账号仍保留少量流量。
	rng := rand.New(rand.NewPCG(1, 2))
	p.SetRandomSource(func(n int64) int64 { return rng.Int64N(n) })
	counts := map[string]int{}
	for i := 0; i < 2000; i++ {
		got := p.Pick()
		if got == nil {
			t.Fatal("pick returned nil")
		}
		counts[got.UID]++
	}
	expiring := counts["soon"] + counts["later"]
	if expiring < 1500 || counts["none"] == 0 {
		t.Fatalf("virtual weight distribution=%v, want expiring majority and regular non-zero", counts)
	}
}

func TestPickExpiringTieUsesExistingWeight(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "small"})
	p.Add(&auth.Auth{UID: "large"})
	p.SetRandomSource(func(n int64) int64 { return 0 })

	at := time.Now().Add(time.Hour)
	p.SetCreditsDetailed("small", 10, 10, 10, at, 10)
	p.SetCreditsDetailed("large", 50, 50, 50, at, 50)

	got := p.Pick()
	if got == nil || got.UID != "large" {
		t.Fatalf("pick=%v want large", got)
	}
}

func TestCreditExpirySnapshotConsumptionAndClear(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	now := time.Now()
	p.SetCreditsDetailed("u1", 100, 100, 50, now.Add(time.Hour), 40)

	st, _ := p.Status("u1")
	if st.CreditsExpiring != 50 || st.CreditsEarliestRemaining != 40 || st.CreditsEarliestExpiry.IsZero() {
		t.Fatalf("initial snapshot=%+v", st)
	}

	p.NoteModelCost("u1", "m", 10, 1000)
	st, _ = p.Status("u1")
	if st.Credits != 90 || st.CreditsExpiring != 40 || st.CreditsEarliestRemaining != 30 {
		t.Fatalf("after consume=%+v", st)
	}

	p.NoteModelCost("u1", "m", 40, 1000)
	st, _ = p.Status("u1")
	if st.Credits != 50 || st.CreditsExpiring != 0 || st.CreditsEarliestRemaining != 0 || !st.CreditsEarliestExpiry.IsZero() {
		t.Fatalf("after exhaustion=%+v", st)
	}

	p.SetCreditsDetailed("u1", 50, 50, 10, now.Add(time.Hour), 10)
	p.SetCreditsDetailed("u1", 50, 50, 0, time.Time{}, 0)
	st, _ = p.Status("u1")
	if st.CreditsExpiring != 0 || st.CreditsEarliestRemaining != 0 || !st.CreditsEarliestExpiry.IsZero() {
		t.Fatalf("zero refresh did not clear snapshot=%+v", st)
	}
}

// TestExpiryBucketsFEFOAndPersistence 钉住按日到期批次的三件事：
//  1. 最早批次以 buckets 为准（同日多包合计已在写入前完成），而不是入参单批值；
//  2. 两次余额刷新之间按 FEFO 扣减，扣穿第一桶后最早批次顺延到下一桶；
//  3. state.json / Redis 快照往返不丢批次，重启后首页快照仍与「检查」同口径。
func TestExpiryBucketsFEFOAndPersistence(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	now := time.Now()
	at1 := now.Add(24 * time.Hour)
	at2 := now.Add(48 * time.Hour)
	p.SetCreditsSnapshot("u1", CreditsSnapshot{
		Remain: 300, Total: 300, Expiring: 100,
		// 故意给一个与 buckets 不一致的最早值：buckets 必须优先。
		EarliestAt: at1, EarliestRemaining: 999,
		ExpiryBuckets: []ExpiryBucket{
			{Date: at1.Format("2006-01-02"), ExpireAt: at1, Remain: 100},
			{Date: at2.Format("2006-01-02"), ExpireAt: at2, Remain: 200},
		},
	})
	st, _ := p.Status("u1")
	if len(st.CreditsExpiryBuckets) != 2 || st.CreditsEarliestRemaining != 100 || !st.CreditsEarliestExpiry.Equal(at1) {
		t.Fatalf("initial buckets=%+v earliest=%v/%d", st.CreditsExpiryBuckets, st.CreditsEarliestExpiry, st.CreditsEarliestRemaining)
	}

	// 扣 150：第一桶扣穿（100），第二桶剩 150，最早批次顺延到 at2。
	p.NoteModelCost("u1", "m", 150, 1000)
	st, _ = p.Status("u1")
	if st.Credits != 150 || len(st.CreditsExpiryBuckets) != 1 || st.CreditsExpiryBuckets[0].Remain != 150 {
		t.Fatalf("after consume buckets=%+v credits=%d", st.CreditsExpiryBuckets, st.Credits)
	}
	if !st.CreditsEarliestExpiry.Equal(at2) || st.CreditsEarliestRemaining != 150 {
		t.Fatalf("earliest after consume=%v/%d want %v/150", st.CreditsEarliestExpiry, st.CreditsEarliestRemaining, at2)
	}

	// 持久化往返。
	p.mu.Lock()
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	if got := sf.Accounts["u1"].CreditsExpiryBuckets; len(got) != 1 || got[0].Remain != 150 {
		t.Fatalf("persisted buckets=%+v", got)
	}
	p2 := New("")
	p2.mu.Lock()
	p2.applyAccountsLocked(sf.Accounts)
	p2.mu.Unlock()
	st2, ok := p2.Status("u1")
	if !ok || len(st2.CreditsExpiryBuckets) != 1 || st2.CreditsExpiryBuckets[0].Remain != 150 {
		t.Fatalf("restored buckets=%+v", st2.CreditsExpiryBuckets)
	}
	if !st2.CreditsEarliestExpiry.Equal(at2) || st2.CreditsEarliestRemaining != 150 {
		t.Fatalf("restored earliest=%v/%d", st2.CreditsEarliestExpiry, st2.CreditsEarliestRemaining)
	}
}
