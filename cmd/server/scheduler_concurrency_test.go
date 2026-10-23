// scheduler_concurrency_test.go v2.3.1 #11：schedule.checkin_concurrency 的
// 装配/热更新接线（main 传入 scheduler.New + applyConfigRuntime 的 setter）。
package main

import "testing"

// TestSaveConfigHotAppliesCheckinConcurrency 面板保存配置后签到并发度热生效
// （applyConfigRuntime → scheduler.SetCheckinConcurrency；无需重启）。
func TestSaveConfigHotAppliesCheckinConcurrency(t *testing.T) {
	path, live, p, up, sch := newSaveConfigFixture(t, `{"listen":"127.0.0.1:7863","api_key":"k"}`)
	if _, err := saveConfig([]byte(`{"schedule":{"checkin_concurrency":7}}`), path, live, p, up, sch); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	if got := sch.CheckinConcurrency(); got != 7 {
		t.Fatalf("热更新后 CheckinConcurrency=%d want 7", got)
	}

	// 越界值经 config normalize + scheduler 归一：>16 clamp 16。
	if _, err := saveConfig([]byte(`{"schedule":{"checkin_concurrency":99}}`), path, live, p, up, sch); err != nil {
		t.Fatalf("saveConfig clamp: %v", err)
	}
	if got := sch.CheckinConcurrency(); got != 16 {
		t.Fatalf("clamp 后 CheckinConcurrency=%d want 16", got)
	}
	// 热生效字段不应出现在 restart_required 清单。
	restart, err := saveConfig([]byte(`{"schedule":{"checkin_concurrency":5}}`), path, live, p, up, sch)
	if err != nil {
		t.Fatalf("saveConfig again: %v", err)
	}
	for _, f := range restart {
		if f == "schedule.checkin_concurrency" {
			t.Fatal("schedule.checkin_concurrency 应热生效，不应要求重启")
		}
	}
}
