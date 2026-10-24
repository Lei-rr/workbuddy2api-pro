// availability_bench_test.go v2.6.0（Audit-1 P0-2）：sticky 可用视图 before/after。
//
// before = 旧路径：session 每次 ResolveForModel 调 WeightedAvailableUIDsForModelRealm
// （全池遍历 + Realm 判定 + healthy 判定 + sort + 虚拟槽展开），再加 session 侧
// map 构造（benchmark 里单独模拟）。
// after  = 新路径：pool 缓存视图命中（O(1) Contains / 复用 UIDs）。
package pool

import (
	"fmt"
	"testing"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
)

// benchPoolAvail 构造 n 个可用账号的池。
func benchPoolAvail(n int) *Pool {
	p := New("")
	for i := 0; i < n; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%06d", i)})
	}
	return p
}

// BenchmarkPoolAvailableWeightedOld 旧路径：每请求全量构造权重槽（before）。
func BenchmarkPoolAvailableWeightedOld(b *testing.B) {
	for _, n := range []int{1000, 5000, 10000} {
		p := benchPoolAvail(n)
		b.Run(sizeName(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if got := p.WeightedAvailableUIDsForModelRealm("glm-5.2", "cn"); len(got) != n {
					b.Fatalf("权重槽数=%d want %d", len(got), n)
				}
			}
		})
		p.Close()
	}
}

// BenchmarkPoolAvailableViewHit 新路径缓存命中（after）：O(1) Contains。
func BenchmarkPoolAvailableViewHit(b *testing.B) {
	for _, n := range []int{1000, 5000, 10000} {
		p := benchPoolAvail(n)
		v := p.AvailableViewForRealm("glm-5.2", "cn")
		b.Run(sizeName(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = p.AvailableViewForRealm("glm-5.2", "cn")
			}
		})
		_ = v
		p.Close()
	}
}

// BenchmarkPoolAvailableViewContains 缓存视图上的 O(1) 绑定校验（session 快路径）。
func BenchmarkPoolAvailableViewContains(b *testing.B) {
	p := benchPoolAvail(10000)
	defer p.Close()
	v := p.AvailableViewForRealm("glm-5.2", "cn")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = v.Contains("u005000")
	}
}

// BenchmarkPoolAvailableViewMiss 缓存未命中重建成本（after 冷路径，非每请求）。
func BenchmarkPoolAvailableViewMiss(b *testing.B) {
	for _, n := range []int{1000, 5000, 10000} {
		p := benchPoolAvail(n)
		b.Run(sizeName(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				p.resetAvailViewsForTest()
				_ = p.AvailableViewForRealm("glm-5.2", "cn")
			}
		})
		p.Close()
	}
}
