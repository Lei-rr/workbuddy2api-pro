// shutdown_wait_extra_test.go v2.6.0（Audit-1 P0-4）：停机顺序「停新任务 → 等后台
// 循环退出 → Flush」。
package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestShutdownWaitsExtraBeforeFlush 顺序断言：Cancel → WaitExtra → Flush。
func TestShutdownWaitsExtraBeforeFlush(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}
	res := gracefulShutdown(shutdownDeps{
		Cancel:     func() { record("cancel") },
		Background: &sync.WaitGroup{},
		WaitExtra: func(ctx context.Context) bool {
			record("wait_extra")
			return true
		},
		Flush:   func() { record("flush") },
		Timeout: 2 * time.Second,
		Logf:    func(string, ...any) {},
	})
	if !res.BackgroundDone {
		t.Fatal("空 WaitGroup 应判定后台已收敛")
	}
	want := []string{"cancel", "wait_extra", "flush"}
	if len(order) != len(want) {
		t.Fatalf("停机顺序 = %v，want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("停机顺序[%d] = %q，want %q（完整 %v）", i, order[i], want[i], order)
		}
	}
}

// TestShutdownWaitExtraTimeoutStillFlushes WaitExtra 超时只 WARN，Flush 仍执行（顺序不变）。
func TestShutdownWaitExtraTimeoutStillFlushes(t *testing.T) {
	var mu sync.Mutex
	var order []string
	var logs []string
	record := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}
	gracefulShutdown(shutdownDeps{
		Cancel:     func() { record("cancel") },
		Background: &sync.WaitGroup{},
		WaitExtra: func(ctx context.Context) bool {
			record("wait_extra")
			return false // 模拟预算到期未收敛
		},
		Flush:   func() { record("flush") },
		Timeout: 2 * time.Second,
		Logf: func(format string, args ...any) {
			mu.Lock()
			logs = append(logs, format)
			mu.Unlock()
		},
	})
	mu.Lock()
	defer mu.Unlock()
	want := []string{"cancel", "wait_extra", "flush"}
	if len(order) != len(want) {
		t.Fatalf("停机顺序 = %v，want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("停机顺序[%d] = %q，want %q（完整 %v）", i, order[i], want[i], order)
		}
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "调度器后台循环未在时限内收敛") {
			found = true
		}
	}
	if !found {
		t.Fatalf("超时未收敛应打 WARN，logs=%v", logs)
	}
}
