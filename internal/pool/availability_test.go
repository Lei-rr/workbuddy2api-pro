// availability_test.go v2.6.0（Audit-1 P0-2）：session 粘性可用视图缓存测试。
package pool

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
)

// newAvailTestPool 建一个带 uid 的池（无 state 文件，避免落盘干扰）。
func newAvailTestPool(t *testing.T, uids ...string) *Pool {
	t.Helper()
	p := New("")
	t.Cleanup(p.Close)
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid})
	}
	return p
}

// TestAvailableViewMatchesWeighted：新缓存视图与旧 WeightedAvailableUIDsForModelRealm
// 的权重槽必须逐项一致（保留既有加权语义）。
func TestAvailableViewMatchesWeighted(t *testing.T) {
	p := newAvailTestPool(t, "u1", "u2", "u3", "u4")
	for _, tc := range []struct{ model, realm string }{
		{"", ""}, {"glm-5.2", ""}, {"glm-5.2", "cn"}, {"glm-5.2", "global"},
	} {
		want := p.WeightedAvailableUIDsForModelRealm(tc.model, tc.realm)
		got := p.AvailableViewForRealm(tc.model, tc.realm).UIDs()
		if len(got) != len(want) {
			t.Fatalf("model=%q realm=%q 槽数不一致: got=%v want=%v", tc.model, tc.realm, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("model=%q realm=%q 槽[%d] 不一致: got=%v want=%v", tc.model, tc.realm, i, got, want)
			}
		}
	}
}

// TestAvailableViewContainsAndFilters：Contains 命中/未命中；禁用/冷却账号不在集合内。
func TestAvailableViewContainsAndFilters(t *testing.T) {
	p := newAvailTestPool(t, "healthy", "disabled", "cooled")
	p.Disable("disabled", "test")
	p.Cooldown("cooled", CoolSoft, time.Minute, "429")

	v := p.AvailableViewForRealm("", "")
	if !v.Contains("healthy") {
		t.Fatalf("healthy 应在可用视图内: %v", v.UIDs())
	}
	if v.Contains("disabled") || v.Contains("cooled") {
		t.Fatalf("禁用/冷却账号不应在可用视图内: %v", v.UIDs())
	}
	if v.Contains("not-exist") {
		t.Fatal("不存在的 uid 不应命中 Contains")
	}
}

// TestAvailableViewCacheAndInvalidation：缓存命中返回同一指针；Add（membership 变化）
// 或 TTL 到期后重建。
func TestAvailableViewCacheAndInvalidation(t *testing.T) {
	p := newAvailTestPool(t, "u1")
	v1 := p.AvailableViewForRealm("m", "cn")
	v2 := p.AvailableViewForRealm("m", "cn")
	if v1 != v2 {
		t.Fatal("未变化时缓存应返回同一视图指针")
	}
	p.Add(&auth.Auth{UID: "u2"})
	v3 := p.AvailableViewForRealm("m", "cn")
	if v3 == v1 {
		t.Fatal("Add 后（membership 代际变化）应重建视图")
	}
	if !v3.Contains("u2") {
		t.Fatalf("重建后的视图应包含新账号: %v", v3.UIDs())
	}

	oldTTL := availViewTTL
	availViewTTL = time.Millisecond
	defer func() { availViewTTL = oldTTL }()
	time.Sleep(5 * time.Millisecond)
	v4 := p.AvailableViewForRealm("m", "cn")
	if v4 == v3 {
		t.Fatal("TTL 到期后应重建视图")
	}
}

// TestAvailableViewCapacityBounded：客户端任意模型名不会让缓存无界增长。
func TestAvailableViewCapacityBounded(t *testing.T) {
	p := newAvailTestPool(t, "u1")
	for i := 0; i < maxAvailViews+50; i++ {
		p.AvailableViewForRealm(modelName(i), "")
	}
	p.availVMu.Lock()
	n := len(p.availViews)
	p.availVMu.Unlock()
	if n > maxAvailViews {
		t.Fatalf("可用视图缓存超过上限: %d > %d", n, maxAvailViews)
	}
}

// modelName 生成互不相同的模型名（测试缓存容量用）。
func modelName(i int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	return "model-" + string(alphabet[i%len(alphabet)]) + "-" + itoa(i)
}

// itoa 极简整数转字符串（避免测试引入 strconv 依赖噪音）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestAvailableViewStateFileFree 确认无 state 文件时视图构建/缓存不触发落盘。
func TestAvailableViewStateFileFree(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})
	v := p.AvailableViewForRealm("", "")
	if len(v.UIDs()) != 1 {
		t.Fatalf("空池/单账号视图异常: %v", v.UIDs())
	}
}

// TestAvailableViewExpiringVirtualSlots：快过期账号在视图里出现 expiringVirtualSlots 次
// （与旧实现同权重），普通账号 1 次。
func TestAvailableViewExpiringVirtualSlots(t *testing.T) {
	dir := t.TempDir()
	p := New(filepath.Join(dir, "state.json"))
	defer p.Close()
	p.Add(&auth.Auth{UID: "normal"})
	p.Add(&auth.Auth{UID: "expiring"})
	p.SetCreditsDetailed("expiring", 100, 100, 50, time.Now().Add(2*time.Hour), 50)
	p.SetPreferExpiring(true)

	view := p.AvailableViewForRealm("", "")
	counts := map[string]int{}
	for _, uid := range view.UIDs() {
		counts[uid]++
	}
	if counts["expiring"] != expiringVirtualSlots {
		t.Fatalf("快过期账号虚拟槽数 = %d，want %d（%v）", counts["expiring"], expiringVirtualSlots, view.UIDs())
	}
	if counts["normal"] != 1 {
		t.Fatalf("普通账号槽数 = %d，want 1（%v）", counts["normal"], view.UIDs())
	}
}
