// jobs_wiring_test.go v2.3.0 #10/#19：main 侧 Job → notify job_done payload 的
// 契约（字段名与 GET /api/jobs 对齐、时间转 unix 秒、不含凭证）。
package main

import (
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/jobs"
)

// TestJobDonePayload job 终态 → notify data map 的字段/类型契约。
func TestJobDonePayload(t *testing.T) {
	started := time.Unix(1700000000, 0)
	finished := started.Add(42 * time.Second)
	j := &jobs.Job{
		ID:         "checkin-123-1",
		Kind:       jobs.KindCheckin,
		Trigger:    jobs.TriggerManual,
		State:      jobs.StateDone,
		Total:      10,
		Done:       8,
		Fail:       2,
		Skip:       0,
		StartedAt:  started,
		FinishedAt: finished,
		Error:      "partial fail",
	}
	data := jobDonePayload(j)
	wants := map[string]any{
		"id":      "checkin-123-1",
		"kind":    "checkin",
		"trigger": "manual",
		"state":   "done",
		"total":   10,
		"done":    8,
		"fail":    2,
		"skip":    0,
	}
	for k, want := range wants {
		if got, ok := data[k]; !ok || got != want {
			t.Errorf("payload[%q]=%v want %v", k, got, want)
		}
	}
	if got := data["started_at"]; got != started.Unix() {
		t.Errorf("started_at=%v want %d", got, started.Unix())
	}
	if got := data["finished_at"]; got != finished.Unix() {
		t.Errorf("finished_at=%v want %d", got, finished.Unix())
	}
	if got := data["error"]; got != "partial fail" {
		t.Errorf("error=%v", got)
	}
	// nil Job 不得 panic（防御性：OnFinish 理论上不会收到 nil）。
	if got := jobDonePayload(nil); len(got) != 0 {
		t.Errorf("nil job payload=%v want empty", got)
	}
}
