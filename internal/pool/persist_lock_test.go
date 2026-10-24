// persist_lock_test.go v2.6.0（Audit-1 P0-3）：验证 state.json 落盘期间不持 p.mu。
package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
)

// TestPersistWriteOutsidePoolLock 证明磁盘 IO 期间池锁空闲：
//  1. 用 persistWriteHook 把写盘卡在「已到写盘点、尚未真正写」；
//  2. 触发一次同步落盘（Remove 会同步 persistState）；
//  3. 写盘被卡的窗口内，Pick/Status 等热路径必须立即返回（不等待磁盘）。
func TestPersistWriteOutsidePoolLock(t *testing.T) {
	dir := t.TempDir()
	p := New(filepath.Join(dir, "state.json"))
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})

	started := make(chan struct{})
	release := make(chan struct{})
	oldHook := persistWriteHook
	persistWriteHook = func() {
		close(started)
		<-release
	}
	defer func() { persistWriteHook = oldHook }()

	removeDone := make(chan struct{})
	go func() {
		p.Remove("u2") // Remove 内部：锁内快照 → 锁外写盘（被 hook 卡住）
		close(removeDone)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("persistState 未在预期时间进入写盘阶段")
	}

	// 写盘被卡住的窗口内，池锁必须空闲：
	begin := time.Now()
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("写盘期间 Pick 应返回剩余账号 u1，got=%v", got)
	}
	if _, ok := p.Status("u2"); ok {
		t.Fatal("u2 已被移除，Status 不应命中")
	}
	if elapsed := time.Since(begin); elapsed > 200*time.Millisecond {
		t.Fatalf("写盘期间 Pick/Status 被阻塞了 %v（落盘应在锁外）", elapsed)
	} else {
		t.Logf("写盘被阻塞期间 Pick+Status 耗时 = %v（v2.5.0 旧实现会等整个 marshal+写盘完成）", elapsed)
	}

	close(release)
	select {
	case <-removeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Remove 未在释放写盘后完成")
	}
}

// TestPersistVersionOrdering：旧版本快照不会覆盖已写盘的新版本（并发快照乱序保护）。
func TestPersistVersionOrdering(t *testing.T) {
	dir := t.TempDir()
	p := New(filepath.Join(dir, "state.json"))
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})

	p.mu.Lock()
	oldSF, oldVer, ok := p.snapshotForPersistLocked()
	p.mu.Unlock()
	if !ok {
		t.Fatal("无 state 文件时应可再生快照")
	}

	p.SetCredits("u1", 42, 42) // 版本前进
	p.Flush()                  // 写入新版本
	if got := p.persistedVer.Load(); got <= oldVer {
		t.Fatalf("Flush 后 persistedVer=%d 应大于旧版本 %d", got, oldVer)
	}

	// 模拟「先快照后写盘」的并发乱序：旧快照晚到，必须被跳过（不覆盖新内容）。
	p.persistState(oldSF, oldVer)
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		t.Fatalf("读取 state.json 失败: %v", err)
	}
	if !strings.Contains(string(raw), `"credits": 42`) {
		t.Fatalf("旧快照覆盖了新内容，state.json 不含 credits=42: %s", string(raw))
	}
}
