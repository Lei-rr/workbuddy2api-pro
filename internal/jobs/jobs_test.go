// jobs_test.go 钉住 v2.3.0 #10 Registry 的状态机语义：
//
//   - 异步执行 + 进度累计/LogTail 环形；
//   - 同 kind 单飞（ErrRunning）、不同 kind 可并行；
//   - 并发闸门 MaxConcurrency（超限 Job 保持 pending 等槽）；
//   - Cancel（运行中 → canceled；不存在 → ErrNotFound；已结束 → false）；
//   - 失败/panic 兜底为 failed + Error；Close/根 ctx 取消拒绝新 Job；
//   - jobs.json 持久化 roundtrip、启动时 running → interrupted、retain 裁剪。
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitJob 轮询直到 Job 进入期望状态（超时 fail）。
func waitJob(t *testing.T, r *Registry, id string, want State, timeout time.Duration) *Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		j, err := r.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if j.State == want {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	j, _ := r.Get(id)
	t.Fatalf("job %s state=%v want %v (timeout %v)", id, j.State, want, timeout)
	return nil
}

// TestRunCompletesWithProgress 正常完成：进度累计值/LogTail 落到 Job 快照。
func TestRunCompletesWithProgress(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	var sawTotal int
	job, err := r.Run(KindCheckin, TriggerManual, 3, func(ctx context.Context, p Progress) error {
		for i := 1; i <= 3; i++ {
			if p != nil {
				p(i, 3, 1, 0, "account-"+string(rune('0'+i)))
			}
		}
		sawTotal = 3
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.ID == "" || job.Kind != KindCheckin || job.Trigger != TriggerManual {
		t.Fatalf("job meta = %+v", job)
	}
	done := waitJob(t, r, job.ID, StateDone, 3*time.Second)
	if done.Done != 3 || done.Total != 3 || done.Fail != 1 {
		t.Fatalf("done counters = %+v", done)
	}
	if len(done.LogTail) != 3 {
		t.Fatalf("log tail = %v", done.LogTail)
	}
	if done.FinishedAt.IsZero() || sawTotal != 3 {
		t.Fatalf("finished/ sawTotal = %v/%v", done.FinishedAt, sawTotal)
	}
}

// TestRunSingleFlightPerKind 同 kind 单飞：同类重复 → ErrRunning；不同 kind 允许并行。
func TestRunSingleFlightPerKind(t *testing.T) {
	r := New(Config{MaxConcurrency: 2})
	defer r.Close()
	release := make(chan struct{})
	started := make(chan struct{})
	blocking := func(ctx context.Context, p Progress) error {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}
	j1, err := r.Run(KindCheckin, TriggerManual, 1, blocking)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	<-started
	if _, err := r.Run(KindCheckin, TriggerManual, 1, blocking); !errors.Is(err, ErrRunning) {
		t.Fatalf("same kind should be ErrRunning, got %v", err)
	}
	j2, err := r.Run(KindTravel, TriggerManual, 1, func(ctx context.Context, p Progress) error { return nil })
	if err != nil {
		t.Fatalf("different kind should be allowed: %v", err)
	}
	close(release)
	waitJob(t, r, j1.ID, StateDone, 3*time.Second)
	waitJob(t, r, j2.ID, StateDone, 3*time.Second)
}

// TestConcurrencyGate 并发闸门：MaxConcurrency=1 时第二个 Job 保持 pending，
// 第一个释放后才 running/completed。
func TestConcurrencyGate(t *testing.T) {
	r := New(Config{MaxConcurrency: 1})
	defer r.Close()
	release := make(chan struct{})
	started := make(chan struct{})
	j1, err := r.Run(KindCheckin, TriggerManual, 1, func(ctx context.Context, p Progress) error {
		close(started)
		<-release
		return nil
	})
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	<-started
	j2, err := r.Run(KindTravel, TriggerManual, 1, func(ctx context.Context, p Progress) error { return nil })
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	// 第二个 Job 在第一个占满执行槽时应仍是 pending。
	time.Sleep(30 * time.Millisecond)
	got, _ := r.Get(j2.ID)
	if got.State != StatePending {
		t.Fatalf("second job state=%v want pending (concurrency gate)", got.State)
	}
	close(release)
	waitJob(t, r, j1.ID, StateDone, 3*time.Second)
	waitJob(t, r, j2.ID, StateDone, 3*time.Second)
}

// TestCancelRunningJob 运行中取消：Cancel=true，任务体 ctx 收到取消，终态 canceled。
func TestCancelRunningJob(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	started := make(chan struct{})
	j, err := r.Run(KindActivity, TriggerManual, 1, func(ctx context.Context, p Progress) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	<-started
	canceled, err := r.Cancel(j.ID)
	if err != nil || !canceled {
		t.Fatalf("Cancel=(%v,%v) want (true,nil)", canceled, err)
	}
	got := waitJob(t, r, j.ID, StateCanceled, 3*time.Second)
	if got.Error != "" {
		t.Fatalf("canceled job should not carry error, got %q", got.Error)
	}
	// 已结束的 Job 再取消：false, nil（不存在才 404/ErrNotFound）。
	if again, err := r.Cancel(j.ID); err != nil || again {
		t.Fatalf("cancel finished = (%v,%v) want (false,nil)", again, err)
	}
	if _, err := r.Cancel("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel unknown id should be ErrNotFound, got %v", err)
	}
}

// TestRunFailureAndPanic 任务体返回 error / panic → failed + Error（进程不崩）。
func TestRunFailureAndPanic(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	j1, err := r.Run(KindKeepalive, TriggerManual, 1, func(ctx context.Context, p Progress) error {
		return errors.New("boom")
	})
	if err != nil {
		t.Fatalf("Run err-branch: %v", err)
	}
	if got := waitJob(t, r, j1.ID, StateFailed, 3*time.Second); !strings.Contains(got.Error, "boom") {
		t.Fatalf("failed job error=%q", got.Error)
	}
	j2, err := r.Run(KindBalance, TriggerManual, 1, func(ctx context.Context, p Progress) error {
		panic("kaboom")
	})
	if err != nil {
		t.Fatalf("Run panic-branch: %v", err)
	}
	if got := waitJob(t, r, j2.ID, StateFailed, 3*time.Second); !strings.Contains(got.Error, "panic") {
		t.Fatalf("panic job error=%q", got.Error)
	}
}

// TestLogTailRing LogTail 环形裁剪到 50 条（保留最新的）。
func TestLogTailRing(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	j, err := r.Run(KindTravel, TriggerManual, 1, func(ctx context.Context, p Progress) error {
		for i := 0; i < 120; i++ {
			p(1, 1, 0, 0, "msg")
		}
		p(1, 1, 0, 0, "last")
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := waitJob(t, r, j.ID, StateDone, 3*time.Second)
	if len(got.LogTail) != LogTailMax {
		t.Fatalf("log tail len=%d want %d", len(got.LogTail), LogTailMax)
	}
	if got.LogTail[len(got.LogTail)-1] != "last" {
		t.Fatalf("log tail should keep newest, last=%q", got.LogTail[len(got.LogTail)-1])
	}
}

// TestPersistenceRoundTrip jobs.json 落盘后重新加载：历史保留、顺序/计数一致。
func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "jobs.json")
	r := New(Config{Path: path})
	j, err := r.Run(KindCheckin, TriggerManual, 2, func(ctx context.Context, p Progress) error {
		p(2, 2, 0, 0, "ok")
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	waitJob(t, r, j.ID, StateDone, 3*time.Second)
	r.Close()
	// 等后台 goroutine 真正退出（finish→persist 在状态可见后仍可能进行中），
	// 否则 Windows 上紧接着读 jobs.json 可能撞上 rename 中的文件锁。
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer waitCancel()
	if !r.Wait(waitCtx) {
		t.Fatal("registry wait timeout")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("jobs.json should exist: %v", err)
	}
	var f persisted
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Jobs) != 1 {
		t.Fatalf("persisted file = %s (err=%v)", raw, err)
	}

	r2 := New(Config{Path: path})
	defer r2.Close()
	list := r2.List(10)
	if len(list) != 1 || list[0].Kind != KindCheckin || list[0].State != StateDone || list[0].Done != 2 {
		t.Fatalf("reloaded jobs = %+v", list)
	}
}

// TestReloadMapsRunningToInterrupted 启动加载：上次 running/pending → interrupted
// （进程重启语义），终态不变。
func TestReloadMapsRunningToInterrupted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	f := persisted{Version: 1, Jobs: []*Job{
		{ID: "j1", Kind: KindCheckin, Trigger: TriggerManual, State: StateRunning, Total: 10, Done: 3},
		{ID: "j2", Kind: KindTravel, Trigger: TriggerManual, State: StateDone, Total: 1, Done: 1},
	}}
	raw, _ := json.Marshal(f)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(Config{Path: path})
	defer r.Close()
	j1, err := r.Get("j1")
	if err != nil || j1.State != StateInterrupted {
		t.Fatalf("j1=%+v err=%v want interrupted", j1, err)
	}
	if j1.Error == "" || j1.FinishedAt.IsZero() {
		t.Fatalf("interrupted job should carry error/finished_at: %+v", j1)
	}
	if j2, err := r.Get("j2"); err != nil || j2.State != StateDone {
		t.Fatalf("j2=%+v err=%v want done", j2, err)
	}
}

// TestRetainTrim retain=5：超过的历史按「最旧的已终态」裁剪，List 只留 5 条。
func TestRetainTrim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	r := New(Config{Path: path, Retain: 5})
	defer r.Close()
	for i := 0; i < 10; i++ {
		r.Record(Job{Kind: KindCheckin, Trigger: TriggerManual, State: StateDone, Total: 1, Done: 1})
	}
	list := r.List(100)
	if len(list) != 5 {
		t.Fatalf("list len=%d want 5 (retain)", len(list))
	}
	// 新的在前：最后 Record 的就是 list[0]（Record 不暴露 id，用 StartedAt 单调不可靠，
	// 只断言总条数与状态）。
	for _, j := range list {
		if j.State != StateDone {
			t.Fatalf("unexpected state: %+v", j)
		}
	}
	// reload 后仍是 5 条。
	r2 := New(Config{Path: path, Retain: 5})
	defer r2.Close()
	if got := len(r2.List(100)); got != 5 {
		t.Fatalf("reloaded list len=%d want 5", got)
	}
}

// TestRecordHistory Record 追加已完成记录（tasks_run 这类同步操作写历史用）。
func TestRecordHistory(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	j := r.Record(Job{
		Kind: KindTasksRun, Trigger: TriggerManual, State: StateDone,
		Total: 3, Done: 2, Skip: 1,
		LogTail: []string{"uid=u1"},
	})
	if j.ID == "" || j.FinishedAt.IsZero() {
		t.Fatalf("recorded job = %+v", j)
	}
	got, err := r.Get(j.ID)
	if err != nil || got.Kind != KindTasksRun || got.Done != 2 || got.Skip != 1 {
		t.Fatalf("Get recorded = %+v err=%v", got, err)
	}
	if len(r.List(10)) != 1 {
		t.Fatalf("list = %+v", r.List(10))
	}
}

// TestCloseAndCanceledBase 根 ctx 取消/Close 后：拒绝新 Job，排队中 Job 取消。
func TestCloseAndCanceledBase(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := New(Config{BaseContext: ctx})
	started := make(chan struct{})
	j, err := r.Run(KindCheckin, TriggerManual, 1, func(ctx context.Context, p Progress) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	<-started
	cancel()
	waitJob(t, r, j.ID, StateCanceled, 3*time.Second)
	if _, err := r.Run(KindTravel, TriggerManual, 1, func(ctx context.Context, p Progress) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Run after base cancel = %v want ErrClosed", err)
	}
	r.Close() // 幂等
}

// TestOnFinishHook 终态回调收到 Job 快照（通知接线用）。
func TestOnFinishHook(t *testing.T) {
	var (
		mu   sync.Mutex
		got  []*Job
		done = make(chan struct{}, 2)
	)
	r := New(Config{OnFinish: func(j *Job) {
		mu.Lock()
		got = append(got, j)
		mu.Unlock()
		done <- struct{}{}
	}})
	defer r.Close()
	j, err := r.Run(KindActivity, TriggerManual, 1, func(ctx context.Context, p Progress) error { return nil })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("OnFinish not called")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].ID != j.ID || got[0].State != StateDone {
		t.Fatalf("OnFinish jobs = %+v", got)
	}
}
