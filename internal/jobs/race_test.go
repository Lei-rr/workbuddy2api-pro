// race_test.go v2.6.2：internal/jobs 数据竞争修复的回归测试。
//
// CI 新增 race job 在 v2.6.1 首跑抓到：Run() 在后台 execute() 启动后无锁 cloneJob
// 读取 Job 字段（jobs.go:261/556）vs execute() 锁内写 State/StartedAt（jobs.go:303）。
// 本地 Windows 无 cgo 跑不了 -race，用高并发压力测试覆盖同一批读写路径：
// 大量 goroutine 同时 Run/Get/List/Cancel/Record/Close，配合 -count=20 放大交错。
package jobs

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunReturnsPendingSnapshotAndIsolation：Run 返回启动前 pending 快照，且与内部
// Job 完全不共享内存（改快照不影响 Registry，Get 看到的是独立副本）。
func TestRunReturnsPendingSnapshotAndIsolation(t *testing.T) {
	r := New(Config{})
	defer r.Close()

	started := make(chan struct{})
	release := make(chan struct{})
	snap, err := r.Run(KindCheckin, TriggerManual, 2, func(ctx context.Context, p Progress) error {
		close(started)
		<-release
		p(2, 2, 0, 0, "done")
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	<-started
	if snap.State != StatePending {
		t.Fatalf("Run 返回快照应为启动前 pending，got %q", snap.State)
	}
	// 篡改快照：内部状态必须不受影响。
	snap.State = StateFailed
	snap.Error = "tampered"
	snap.LogTail = append(snap.LogTail, "tampered")
	if got, err := r.Get(snap.ID); err != nil || got.State == StateFailed || got.Error != "" || len(got.LogTail) != 0 {
		t.Fatalf("返回快照未隔离：Get=%+v err=%v", got, err)
	}
	close(release)
	waitJob(t, r, snap.ID, StateDone, 3*time.Second)
}

// TestConcurrentSnapshotAccess 并发压力：200 goroutine 同时 Run/Get/List/Cancel/
// Record，并伴随独立的持续读循环。修复前 Run 内的无锁 cloneJob 会与 execute 的
// 字段写入竞争（CI race job 正是抓到该路径）。
func TestConcurrentSnapshotAccess(t *testing.T) {
	r := New(Config{MaxConcurrency: 8, Retain: 500})
	defer r.Close()

	kinds := []Kind{KindCheckin, KindTravel, KindActivity, KindKeepalive, KindBalance, KindTasksRun}
	var wg sync.WaitGroup
	var failed atomic.Int64
	recordErr := func(err error) {
		failed.Add(1)
		t.Logf("concurrent op error: %v", err)
	}

	const workers = 200
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			kind := kinds[i%len(kinds)]
			job, err := r.Run(kind, TriggerManual, 5, func(ctx context.Context, p Progress) error {
				for n := 1; n <= 5; n++ {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					p(n, 5, 0, 0, "progress")
					runtime.Gosched()
				}
				return nil
			})
			if err != nil {
				// 同 kind 单飞/关闭：合法错误；仍并发读，覆盖 List/Get 锁路径。
				_ = r.List(20)
				return
			}
			// 立即读取 Run 返回快照（修复前这里是锁外 cloneJob）。
			_ = job.ID
			_ = job.State
			_ = job.StartedAt
			_ = len(job.LogTail)

			// 并发读内部快照 + 取消 + 记录，覆盖全部锁路径。
			if got, gerr := r.Get(job.ID); gerr == nil {
				_ = got.State
				_ = got.Done
				_ = len(got.LogTail)
			}
			_ = r.List(10)
			if _, cerr := r.Cancel(job.ID); cerr != nil && !errors.Is(cerr, ErrNotFound) {
				recordErr(cerr)
			}
			rec := r.Record(Job{Kind: KindTasksRun, Trigger: TriggerManual, State: StateDone})
			if rec == nil || rec.ID == "" {
				recordErr(errors.New("Record 返回空快照"))
			}
		}(i)
	}

	// 独立 reader：持续 List/Get 直到所有 worker 退出（与 job 状态迁移并发）。
	stopReaders := make(chan struct{})
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stopReaders:
				return
			default:
				for _, j := range r.List(30) {
					_ = j.State
					_ = j.Done
				}
				runtime.Gosched()
			}
		}
	}()

	wg.Wait()
	close(stopReaders)
	readerWG.Wait()

	// 收尾：拒绝新 Job、取消在途并等待全部 goroutine 收敛。
	r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if !r.Wait(ctx) {
		t.Fatalf("并发压力后 Job 未在 20s 内收敛（failed ops=%d）", failed.Load())
	}
	if n := failed.Load(); n > 0 {
		t.Fatalf("并发压力出现 %d 次非预期错误", n)
	}
}

// TestConcurrentCloseWithRun execute 与 Close 竞争：Close 后不得再有 Job 以 running
// 状态遗留（v2.6.2 的 closed 复检路径）。
func TestConcurrentCloseWithRun(t *testing.T) {
	for round := 0; round < 20; round++ {
		r := New(Config{MaxConcurrency: 2})
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, _ = r.Run(Kind("checkin"), TriggerManual, 1, func(ctx context.Context, p Progress) error {
					<-ctx.Done()
					return ctx.Err()
				})
			}(i)
		}
		close(start)
		r.Close()
		wg.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if !r.Wait(ctx) {
			cancel()
			t.Fatalf("round %d: Close 后 Job 未收敛", round)
		}
		for _, j := range r.List(50) {
			if j.State == StatePending || j.State == StateRunning {
				cancel()
				t.Fatalf("round %d: Close 后仍有未终态 Job: %+v", round, j)
			}
		}
		cancel()
	}
}
