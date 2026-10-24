// lockfree_test.go v2.6.0（Audit-1 P1）：积分历史落盘移到锁外。
package credithist

import (
	"path/filepath"
	"testing"
	"time"
)

// TestObserveWritesOutsideLock 落盘被阻塞时，Read/Observe 不被锁内的磁盘 IO 拖住；
// 且写盘完成后数据完整（重启可恢复）。
func TestObserveWritesOutsideLock(t *testing.T) {
	dir := t.TempDir()
	l := New(filepath.Join(dir, "credit-history.json"), 0)
	l.Observe("u1", 100) // 建基线（此时 hook 未装）
	l.Observe("u1", 90)  // 写一条 -10 流水（hook 未装，正常落盘）

	started := make(chan struct{})
	release := make(chan struct{})
	oldHook := writeFileHook
	writeFileHook = func() {
		close(started)
		<-release
	}
	defer func() { writeFileHook = oldHook }()

	done := make(chan struct{})
	go func() {
		l.Observe("u1", 80) // 第二条流水；写盘应在锁外发生
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("写盘未在预期时间开始")
	}

	// 写盘被卡住期间：Read 只取内存，必须立即返回（落盘若在锁内会被阻塞）。
	begin := time.Now()
	entries := l.Read(1)
	if elapsed := time.Since(begin); elapsed > 100*time.Millisecond {
		t.Fatalf("Read 被锁内的写盘阻塞了 %v（落盘应在锁外）", elapsed)
	} else {
		t.Logf("落盘被阻塞期间 Read(1) 耗时 = %v（修复前等锁内 marshal+写盘）", elapsed)
	}
	if len(entries) != 1 {
		t.Fatalf("Read 应返回 1 条流水，got %d", len(entries))
	}

	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Observe 未在释放写盘后完成")
	}

	// 落盘完整：重启后仍能恢复两条流水（-10、-10）。
	l2 := New(filepath.Join(dir, "credit-history.json"), 0)
	if got := len(l2.Read(0)); got != 2 {
		t.Fatalf("重启后应恢复 2 条流水，got %d", got)
	}
}
