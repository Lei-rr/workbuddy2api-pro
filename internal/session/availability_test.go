// availability_test.go v2.6.0（Audit-1 P0-2）：session 使用 pool 缓存可用视图后的
// O(1) 快路径 / 慢路径分配语义测试。
package session

import (
	"testing"
)

// fakeAvailability 测试用可用视图：记录调用次数以断言快路径不取 UIDs。
type fakeAvailability struct {
	uids          []string
	set           map[string]bool
	containsCalls int
	uidCalls      int
}

func newFakeAvailability(uids ...string) *fakeAvailability {
	set := make(map[string]bool, len(uids))
	for _, u := range uids {
		set[u] = true
	}
	return &fakeAvailability{uids: uids, set: set}
}

func (f *fakeAvailability) Contains(uid string) bool {
	f.containsCalls++
	return f.set[uid]
}

func (f *fakeAvailability) UIDs() []string {
	f.uidCalls++
	return f.uids
}

// TestResolveFastPathUsesContainsOnly：已绑定且可用 → 直接命中，且不取权重槽（O(1) 快路径）。
func TestResolveFastPathUsesContainsOnly(t *testing.T) {
	fv := newFakeAvailability("u1", "u2")
	r := New(Config{AvailableViewForModel: func(string) Availability { return fv }})
	r.Bind("sess", "u1")

	uid, ok := r.ResolveForModel("sess", "m")
	if !ok || uid != "u1" {
		t.Fatalf("已绑定可用账号应直接命中: uid=%q ok=%v", uid, ok)
	}
	if fv.uidCalls != 0 {
		t.Fatalf("快路径不应调用 UIDs() 取全量权重槽（调用 %d 次）", fv.uidCalls)
	}
}

// TestResolveReallocatesWhenBindingUnavailable：绑定号不在可用视图 → 慢路径重分配。
func TestResolveReallocatesWhenBindingUnavailable(t *testing.T) {
	fv := newFakeAvailability("u1", "u2")
	r := New(Config{AvailableViewForModel: func(string) Availability { return fv }})
	r.Bind("sess", "u9") // 已失效的绑定

	uid, ok := r.ResolveForModel("sess", "m")
	if !ok || uid == "u9" {
		t.Fatalf("失效绑定应重分配到可用账号: uid=%q ok=%v", uid, ok)
	}
	if uid != "u1" && uid != "u2" {
		t.Fatalf("重分配到了集合外账号: %q", uid)
	}
	if fv.uidCalls == 0 {
		t.Fatal("慢路径应读取权重槽 UIDs()")
	}
}

// TestResolveNoAvailable：视图为空 → 返回 false（不 panic）。
func TestResolveNoAvailable(t *testing.T) {
	fv := newFakeAvailability()
	r := New(Config{AvailableViewForModel: func(string) Availability { return fv }})
	if uid, ok := r.ResolveForModel("sess", "m"); ok || uid != "" {
		t.Fatalf("无可用账号应返回 false，got uid=%q ok=%v", uid, ok)
	}
}

// TestLookupForModelUsesView：Lookup 只复用可用绑定，不分配新号。
func TestLookupForModelUsesView(t *testing.T) {
	fv := newFakeAvailability("u1")
	r := New(Config{AvailableViewForModel: func(string) Availability { return fv }})
	r.Bind("sess", "u1")
	if uid, ok := r.LookupForModel("sess", "m"); !ok || uid != "u1" {
		t.Fatalf("可用绑定应被复用: uid=%q ok=%v", uid, ok)
	}
	r.Bind("sess", "u9")
	if _, ok := r.LookupForModel("sess", "m"); ok {
		t.Fatal("不可用绑定不应被复用")
	}
}

// TestLegacyAvailableForModelStillWorks：未注入新视图时旧闭包语义保持不变。
func TestLegacyAvailableForModelStillWorks(t *testing.T) {
	r := New(Config{AvailableForModel: func(model string) []string {
		if model == "m" {
			return []string{"u1", "u2"}
		}
		return nil
	}})
	r.Bind("sess", "u2")
	if uid, ok := r.ResolveForModel("sess", "m"); !ok || uid != "u2" {
		t.Fatalf("旧闭包兼容路径应命中 u2: uid=%q ok=%v", uid, ok)
	}
	if _, ok := r.ResolveForModel("sess", "other"); ok {
		t.Fatal("旧闭包对未声明模型应返回无可用")
	}
}

// TestResolveDistributionPreserved：同一 key 稳定哈希到同一账号（加权槽哈希语义不变）。
func TestResolveDistributionPreserved(t *testing.T) {
	fv := newFakeAvailability("u1", "u1", "u1", "u2") // 模拟虚拟槽权重
	r := New(Config{AvailableViewForModel: func(string) Availability { return fv }})
	first, ok := r.ResolveForModel("same-key", "m")
	if !ok {
		t.Fatal("应能分配")
	}
	r2 := New(Config{AvailableViewForModel: func(string) Availability { return fv }})
	second, ok := r2.ResolveForModel("same-key", "m")
	if !ok || second != first {
		t.Fatalf("相同 key 的哈希分配应稳定: %q vs %q", first, second)
	}
}

// BenchmarkResolveFastPathCached 新路径：10k 槽视图 + 已绑定命中（after）。
func BenchmarkResolveFastPathCached(b *testing.B) {
	uids := make([]string, 0, 10000)
	for i := 0; i < 10000; i++ {
		uids = append(uids, benchUID(i))
	}
	fv := newFakeAvailability(uids...)
	r := New(Config{AvailableViewForModel: func(string) Availability { return fv }})
	r.Bind("sess", uids[0])
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := r.ResolveForModel("sess", "m"); !ok {
			b.Fatal("应命中绑定")
		}
	}
}

// BenchmarkResolveFastPathOldStyle 旧路径等价物：每次请求把 10k 可用列表转 map 再查
// （修复前 session.availableSet 的开销；before 参照）。
func BenchmarkResolveFastPathOldStyle(b *testing.B) {
	uids := make([]string, 0, 10000)
	for i := 0; i < 10000; i++ {
		uids = append(uids, benchUID(i))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		set := make(map[string]bool, len(uids))
		for _, u := range uids {
			set[u] = true
		}
		if !set[uids[0]] {
			b.Fatal("应命中")
		}
	}
}

// benchUID 生成基准用 uid。
func benchUID(i int) string {
	const digits = "0123456789"
	buf := make([]byte, 0, 8)
	buf = append(buf, 'u')
	if i == 0 {
		return "u0"
	}
	var tmp [8]byte
	p := len(tmp)
	for i > 0 {
		p--
		tmp[p] = digits[i%10]
		i /= 10
	}
	return string(append(buf, tmp[p:]...))
}
