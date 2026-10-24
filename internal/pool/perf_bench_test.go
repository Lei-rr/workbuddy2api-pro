// perf_bench_test.go v2.5.0 性能专项：账号池读路径微基准（只加基准文件，不改生产代码）。
//
// 覆盖 1k/5k/10k 账号下 List / CreditsSums / Counts / AuthByUID 的耗时与分配，
// 用 `go test -run '^$' -bench 'BenchmarkPool' -benchmem ./internal/pool/` 运行；
// 需要函数级热点时加 `-cpuprofile` 再 `go tool pprof -top`。
//
// 注意：基准池用 `New("")`（不落盘、无后台 flusher），状态用确定性规则注入，
// 与 tools/perfbench 生成的端到端环境同构（积分/冷却/历史计数非零），避免"全零
// 新号"低估 statusOf 的字段构造成本。
package pool

import (
	"fmt"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
)

// benchPoolSizes 三档账号规模（与 perfbench 的 1k/5k/10k 对齐）。
var benchPoolSizes = []int{1000, 5000, 10000}

// newBenchPool 构造 n 个账号的池（状态确定性注入：积分、部分软冷却、当日已签）。
func newBenchPool(n int) *Pool {
	p := New("") // 空 state 路径：不加载/不启动后台 flusher
	now := time.Now()
	exp := now.Add(72 * time.Hour).Unix()
	for i := 0; i < n; i++ {
		uid := fmt.Sprintf("u%06d", i)
		domain := "copilot.tencent.com"
		if i%5 == 0 {
			domain = "workbuddy.ai"
		}
		a := &auth.Auth{
			UID:          uid,
			AccessToken:  fmt.Sprintf("at-%06d", i),
			RefreshToken: fmt.Sprintf("rt-%06d", i),
			ExpiresAt:    exp,
			Domain:       domain,
			Nickname:     "bench-" + uid,
		}
		if i%20 == 0 {
			a.EnterpriseID = fmt.Sprintf("ent-%06d", i)
		}
		p.Add(a)
		p.SetCredits(uid, int64(100+(i*37)%9900), int64(1000+(i*101)%9000))
		if i%10 < 7 {
			p.NoteCheckinDone(uid)
		}
		if i%23 == 3 {
			// 少量软冷却账号（List/statusOf 走冷却分支，字段更多）。
			p.Cooldown(uid, CoolSoft, time.Minute, "bench soft rate")
		}
		if i%4 == 0 {
			// 实测扣费账本：List/statusOf 要构造 ModelCosts 行（与真实大池一致）。
			p.NoteModelCost(uid, "claude-sonnet-4-20250514", 0.003, 1000)
		}
		if i%8 == 0 {
			p.NoteModelCost(uid, "gpt-5", 0.01, 500)
		}
	}
	return p
}

// benchmarkPoolSizes 统一跑三档规模的子基准。
func benchmarkPoolSizes(b *testing.B, fn func(b *testing.B, p *Pool)) {
	for _, n := range benchPoolSizes {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			p := newBenchPool(n)
			b.ReportAllocs()
			b.ResetTimer()
			fn(b, p)
		})
	}
}

// BenchmarkPoolList 全池状态快照（O(n log n)：排序 + 逐号 Status 构造 + 嵌套 slice 拷贝）。
// 面板 /overview 全量、/accounts、/metrics（累加 InFlight）都走它。
func BenchmarkPoolList(b *testing.B) {
	benchmarkPoolSizes(b, func(b *testing.B, p *Pool) {
		for i := 0; i < b.N; i++ {
			_ = p.List()
		}
	})
}

// BenchmarkPoolCreditsSums 积分合计（O(n) 只读，面板 overview/metrics 每轮都调）。
func BenchmarkPoolCreditsSums(b *testing.B) {
	benchmarkPoolSizes(b, func(b *testing.B, p *Pool) {
		for i := 0; i < b.N; i++ {
			_, _ = p.CreditsSums()
		}
	})
}

// BenchmarkPoolCountsDetailedWithPaused 分类计数（O(n) 只读；overview/metrics/healthz 用）。
func BenchmarkPoolCountsDetailedWithPaused(b *testing.B) {
	benchmarkPoolSizes(b, func(b *testing.B, p *Pool) {
		for i := 0; i < b.N; i++ {
			_, _, _, _, _, _ = p.CountsDetailedWithPaused()
		}
	})
}

// BenchmarkPoolAuthByUID 随机 uid 查凭证（O(1) + RLock；高频热路径）。
func BenchmarkPoolAuthByUID(b *testing.B) {
	benchmarkPoolSizes(b, func(b *testing.B, p *Pool) {
		for i := 0; i < b.N; i++ {
			_ = p.AuthByUID(fmt.Sprintf("u%06d", i%len(benchPoolSizes))) // 命中
		}
	})
}

// BenchmarkPoolAvailableUIDs 选号可用集（O(n) 遍历 + 排序；会话粘性/选号前置调用）。
func BenchmarkPoolAvailableUIDs(b *testing.B) {
	benchmarkPoolSizes(b, func(b *testing.B, p *Pool) {
		for i := 0; i < b.N; i++ {
			_ = p.AvailableUIDs()
		}
	})
}
