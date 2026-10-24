// perf_cache_bench_test.go v2.5.0 性能专项 Perf-2：池快照/摘要/模型锁缓存的
// before/after 基准（与 perf_bench_test.go 同池同状态，可直接对比）。
//
//	before（本文件旧实现路径）：ModelLockView 空池 O(n) 全扫、面板 overview lite 的
//	    Counts+Credits+List+ModelLockView 组合成本；
//	after（实现后追加）：Snapshot（命中/构建）、Summary（命中/构建）、
//	    AvailableUIDsSorted、ModelLockView 空池 O(1)、List（重建）、Version。
package pool

import (
	"fmt"
	"testing"
	"time"
)

// sizeName 子基准命名（与 perf_bench_test.go 的 n=%d 口径一致）。
func sizeName(n int) string { return fmt.Sprintf("n=%d", n) }

// uidAt 与 newBenchPool 生成的 uid 规则一致。
func uidAt(i int) string { return fmt.Sprintf("u%06d", i) }

// BenchmarkPoolModelLockViewEmpty 无模型锁时 ModelLockView 的成本（现状：全池遍历）。
func BenchmarkPoolModelLockViewEmpty(b *testing.B) {
	for _, n := range benchPoolSizes {
		n := n
		b.Run(sizeName(n), func(b *testing.B) {
			p := newBenchPool(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = p.ModelLockView()
			}
		})
	}
}

// BenchmarkPoolModelLockViewLocked 少量真实模型锁时的成本（热路径 worst-case：
// 聚合行 + 每行再全池算 servable）。
func BenchmarkPoolModelLockViewLocked(b *testing.B) {
	for _, n := range benchPoolSizes {
		n := n
		b.Run(sizeName(n), func(b *testing.B) {
			p := newBenchPool(n)
			until := time.Now().Add(time.Hour)
			// ~5% 账号在两个模型上各有一条 6004 路由冷却（行数固定为 2，锁量随池线性）。
			for i := 0; i < n; i += 20 {
				uid := uidAt(i)
				p.CooldownSoftForModel(uid, 0, until, "claude-sonnet-4-20250514", "6004 rate limit")
				p.CooldownSoftForModel(uid, 0, until, "gpt-5", "6004 rate limit")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = p.ModelLockView()
			}
		})
	}
}

// BenchmarkPoolSummaryOldStyle 现状面板 overview lite 的聚合成本（Summary 优化前）：
// Counts + CreditsSums + List（仅为累加 InFlight）+ ModelLockView 四遍全池扫描。
func BenchmarkPoolSummaryOldStyle(b *testing.B) {
	for _, n := range benchPoolSizes {
		n := n
		b.Run(sizeName(n), func(b *testing.B) {
			p := newBenchPool(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _, _, _, _, _ = p.CountsDetailedWithPaused()
				_, _ = p.CreditsSums()
				inFlight := 0
				for _, st := range p.List() {
					inFlight += st.InFlight
				}
				locks := len(p.ModelLockView())
				if inFlight != 0 { // 防编译器优化掉
					_ = locks
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// after（v2.5.0 Perf-2 新实现）：Snapshot / Summary / AvailableUIDsSorted / Version
// ---------------------------------------------------------------------------

// BenchmarkPoolVersion 代际读取（panel ETag 失效判断的常量成本）。
func BenchmarkPoolVersion(b *testing.B) {
	p := newBenchPool(1000)
	b.ReportAllocs()
	b.ResetTimer()
	var v uint64
	for i := 0; i < b.N; i++ {
		v = p.Version()
	}
	_ = v
}

// BenchmarkPoolSnapshotHit 缓存命中：version 未变直接返回同一切片（目标 0 alloc）。
// TTL 临时拉到 1h，避免基准期间（默认 1s）触发 TTL 重建，纯测命中路径。
func BenchmarkPoolSnapshotHit(b *testing.B) {
	oldTTL := snapshotTTL
	snapshotTTL = time.Hour
	defer func() { snapshotTTL = oldTTL }()
	for _, n := range benchPoolSizes {
		n := n
		b.Run(sizeName(n), func(b *testing.B) {
			p := newBenchPool(n)
			_ = p.Snapshot() // 预热：先构建一次
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = p.Snapshot()
			}
		})
	}
}

// BenchmarkPoolSnapshotMiss 缓存失效/重建：每次清空缓存强制走完整构建（目标 ≤5ms@10k）。
func BenchmarkPoolSnapshotMiss(b *testing.B) {
	for _, n := range benchPoolSizes {
		n := n
		b.Run(sizeName(n), func(b *testing.B) {
			p := newBenchPool(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p.snapshotCache.Store(nil)
				_ = p.Snapshot()
			}
		})
	}
}

// BenchmarkPoolSummaryHit Summary 缓存命中（version 未变，目标 0 alloc）。
func BenchmarkPoolSummaryHit(b *testing.B) {
	oldTTL := summaryTTL
	summaryTTL = time.Hour
	defer func() { summaryTTL = oldTTL }()
	for _, n := range benchPoolSizes {
		n := n
		b.Run(sizeName(n), func(b *testing.B) {
			p := newBenchPool(n)
			_ = p.Summary()
			b.ReportAllocs()
			b.ResetTimer()
			var s Summary
			for i := 0; i < b.N; i++ {
				s = p.Summary()
			}
			_ = s
		})
	}
}

// BenchmarkPoolSummaryMiss Summary 重建：单次遍历聚合（目标 <0.5ms@10k）。
func BenchmarkPoolSummaryMiss(b *testing.B) {
	for _, n := range benchPoolSizes {
		n := n
		b.Run(sizeName(n), func(b *testing.B) {
			p := newBenchPool(n)
			b.ReportAllocs()
			b.ResetTimer()
			var s Summary
			for i := 0; i < b.N; i++ {
				p.summaryCache.Store(nil)
				s = p.Summary()
			}
			_ = s
		})
	}
}

// BenchmarkPoolAvailableUIDsSortedHit 可用 UID 缓存命中（0 alloc）。
func BenchmarkPoolAvailableUIDsSortedHit(b *testing.B) {
	oldTTL := availTTL
	availTTL = time.Hour
	defer func() { availTTL = oldTTL }()
	for _, n := range benchPoolSizes {
		n := n
		b.Run(sizeName(n), func(b *testing.B) {
			p := newBenchPool(n)
			_ = p.AvailableUIDsSorted()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = p.AvailableUIDsSorted()
			}
		})
	}
}

// BenchmarkPoolAvailableUIDsSortedMiss 可用 UID 重建（复用 uid 有序缓存，无需再排序）。
func BenchmarkPoolAvailableUIDsSortedMiss(b *testing.B) {
	for _, n := range benchPoolSizes {
		n := n
		b.Run(sizeName(n), func(b *testing.B) {
			p := newBenchPool(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p.availCache.Store(nil)
				_ = p.AvailableUIDsSorted()
			}
		})
	}
}
