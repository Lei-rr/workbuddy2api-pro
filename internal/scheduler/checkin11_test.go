// checkin11_test.go 钉住 v2.3.1 #11 的三组语义：
//
//	A. 签到 worker pool：账号级并发有上限（峰值 ≤ CheckinConcurrency）、当日已签
//	   跳过 checkin RPC（仍刷新余额）、瞬态错误重试 1 次而 4xx 不重试；
//	B. 按族解耦：慢签到不阻塞同批 travel（dispatchBatch 立即返回、各族独立并发）；
//	C. 定时/补跑批次进 Job 历史：trigger=schedule/catch_up、Job 可见、cancel 早停。
package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/jobs"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// checkinTestUpstream 可配置的签到/余额上游桩：记录调用次数与 /daily-checkin 并发峰值。
type checkinTestUpstream struct {
	mu            sync.Mutex
	dailyCalls    int
	resourceCalls int
	inflight      int
	peak          int
	delay         time.Duration
	// dailyStatus：/daily-checkin 的 HTTP 状态（0 = 200 成功）；firstDailyStatus
	// 非 0 时仅第一次调用使用它（用于「瞬态失败后重试成功」用例）。
	dailyStatus      int
	firstDailyStatus int
	// alreadyChecked 成功响应体改用「今日已签到」业务错误（code!=0）。
	alreadyChecked bool
}

func (f *checkinTestUpstream) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			f.mu.Lock()
			f.dailyCalls++
			n := f.dailyCalls
			f.inflight++
			if f.inflight > f.peak {
				f.peak = f.inflight
			}
			status := f.dailyStatus
			if f.firstDailyStatus != 0 && n == 1 {
				status = f.firstDailyStatus
			}
			delay := f.delay
			already := f.alreadyChecked
			f.mu.Unlock()
			if delay > 0 {
				time.Sleep(delay)
			}
			f.mu.Lock()
			f.inflight--
			f.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
				w.Write([]byte(`{"code":500,"msg":"boom"}`))
				return
			}
			if already {
				w.Write([]byte(`{"code":14001,"msg":"今日已签到"}`))
				return
			}
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			f.mu.Lock()
			f.resourceCalls++
			f.mu.Unlock()
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":600,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/activity/growth/buddy/info"):
			// 已领养：旅行状态机直接进 status 查询。
			w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1,"name":"cat"}}}`))
		case strings.HasSuffix(r.URL.Path, "/activity/growth/buddy/travel/status"):
			w.Write([]byte(`{"code":0,"data":{"state":"idle","daily_limit_reached":true}}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
}

func (f *checkinTestUpstream) counts() (daily, resource, peak int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dailyCalls, f.resourceCalls, f.peak
}

// fastRetry 把瞬态重试退避临时置 0（测试加速；恢复用 t.Cleanup）。
func fastRetry(t *testing.T) {
	t.Helper()
	old := checkinRetryBackoff
	checkinRetryBackoff = 0
	t.Cleanup(func() { checkinRetryBackoff = old })
}

// TestCheckinConcurrencyBounded 6 个账号、并发 3：/daily-checkin 峰值 ≤3 且确实并行。
func TestCheckinConcurrencyBounded(t *testing.T) {
	f := &checkinTestUpstream{delay: 15 * time.Millisecond}
	srv := f.server(t)
	defer srv.Close()

	p := newProgressPool(6)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, CheckinConcurrency: 3})

	cap := &progressCapture{}
	if err := s.RunCheckinCtx(context.Background(), cap.fn); err != nil {
		t.Fatalf("RunCheckinCtx: %v", err)
	}
	daily, _, peak := f.counts()
	if daily != 6 {
		t.Fatalf("daily-checkin calls=%d want 6", daily)
	}
	if peak > 3 {
		t.Fatalf("并发峰值=%d 超过 CheckinConcurrency=3（worker pool 失效？）", peak)
	}
	if peak < 2 {
		t.Fatalf("并发峰值=%d，未观察到并行（worker pool 退化成串行）", peak)
	}
	done, total, fail, skip, _, ok := cap.last()
	if !ok || done != 6 || total != 6 || fail != 0 || skip != 0 {
		t.Fatalf("progress=%d/%d fail=%d skip=%d want 6/6/0/0", done, total, fail, skip)
	}
}

// TestCheckinSkipsAlreadySignedSameDay 同日二跑：跳过 checkin RPC（skip=total），
// 但仍刷新余额；新建 Scheduler（模拟重启）后内存标记清空 → 重新打 checkin（上游幂等）。
func TestCheckinSkipsAlreadySignedSameDay(t *testing.T) {
	f := &checkinTestUpstream{}
	srv := f.server(t)
	defer srv.Close()

	p := newProgressPool(2)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, CheckinConcurrency: 2})

	if err := s.RunCheckinCtx(context.Background(), nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	daily1, resource1, _ := f.counts()
	if daily1 != 2 || resource1 != 2 {
		t.Fatalf("first run daily=%d resource=%d want 2/2", daily1, resource1)
	}
	if !s.checkinDoneToday("a") || !s.checkinDoneToday("b") {
		t.Fatal("首轮成功应标记当日已签（进程内）")
	}

	cap := &progressCapture{}
	if err := s.RunCheckinCtx(context.Background(), cap.fn); err != nil {
		t.Fatalf("second run: %v", err)
	}
	daily2, resource2, _ := f.counts()
	if daily2 != 2 {
		t.Fatalf("同日二跑 daily-checkin calls=%d want 2（跳过已签）", daily2)
	}
	if resource2 != 4 {
		t.Fatalf("同日二跑 resource calls=%d want 4（已签仍刷新余额）", resource2)
	}
	done, total, fail, skip, _, ok := cap.last()
	if !ok || done != 0 || total != 2 || fail != 0 || skip != 2 {
		t.Fatalf("second progress=%d/%d fail=%d skip=%d want 0/2/0/2", done, total, fail, skip)
	}

	// 模拟进程重启：新 Scheduler 内存标记为空 → 重新打 checkin（上游已签幂等不影响计数）。
	s2 := New(Config{Pool: p, Upstream: up, CheckinConcurrency: 1})
	if err := s2.RunCheckinCtx(context.Background(), nil); err != nil {
		t.Fatalf("restart run: %v", err)
	}
	if daily3, _, _ := f.counts(); daily3 != 4 {
		t.Fatalf("重启后 daily-checkin calls=%d want 4（内存标记不持久化）", daily3)
	}
}

// TestCheckinUpstreamAlreadyCheckedMarksDone 上游返回「今日已签到」幂等成功：
// 计 done 并标记当日已签（同日后续轮次跳过 checkin RPC）。
func TestCheckinUpstreamAlreadyCheckedMarksDone(t *testing.T) {
	f := &checkinTestUpstream{alreadyChecked: true}
	srv := f.server(t)
	defer srv.Close()
	p := newProgressPool(1)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, CheckinConcurrency: 1})

	cap := &progressCapture{}
	if err := s.RunCheckinCtx(context.Background(), cap.fn); err != nil {
		t.Fatalf("run: %v", err)
	}
	done, _, fail, skip, _, _ := cap.last()
	if done != 1 || fail != 0 || skip != 0 {
		t.Fatalf("already-checked progress done=%d fail=%d skip=%d want 1/0/0", done, fail, skip)
	}
	if !s.checkinDoneToday("a") {
		t.Fatal("上游幂等已签应标记当日已签")
	}
}

// TestCheckinRetryLayering 分层重试语义（v2.3.1 #11）：
//   - 429 限流：upstream 计费路径不重试 → scheduler 补 1 次短退避重试（首败后再成功）；
//   - 5xx：upstream.retryBillingTransient 已做 1+2 次有界重试 → scheduler 不叠加
//     （这里首次 500 由 upstream 重试成功，调用 2 次、done=1，scheduler 不再补打）；
//   - 4xx 鉴权/凭证：确定性失败，哪一层都不重试（见 TestCheckinNoRetryOnAuthError）。
func TestCheckinRetryLayering(t *testing.T) {
	fastRetry(t)

	// 429 → scheduler 层重试 1 次后成功。
	f429 := &checkinTestUpstream{firstDailyStatus: http.StatusTooManyRequests}
	srv429 := f429.server(t)
	defer srv429.Close()
	p429 := newProgressPool(1)
	up429 := &upstream.Client{HTTP: srv429.Client(), ChatBaseCN: srv429.URL, BillingBaseCN: srv429.URL}
	s429 := New(Config{Pool: p429, Upstream: up429, CheckinConcurrency: 1})
	cap429 := &progressCapture{}
	if err := s429.RunCheckinCtx(context.Background(), cap429.fn); err != nil {
		t.Fatalf("run 429: %v", err)
	}
	if daily, _, _ := f429.counts(); daily != 2 {
		t.Fatalf("429 scheduler retry daily calls=%d want 2（scheduler 补 1 次）", daily)
	}
	if done, _, fail, _, _, _ := cap429.last(); done != 1 || fail != 0 {
		t.Fatalf("429 retry progress done=%d fail=%d want 1/0", done, fail)
	}

	// 5xx：由 upstream 内置重试消化（首次 500 → 第 2 次成功），scheduler 不叠加。
	f500 := &checkinTestUpstream{firstDailyStatus: http.StatusInternalServerError}
	srv500 := f500.server(t)
	defer srv500.Close()
	p500 := newProgressPool(1)
	up500 := &upstream.Client{HTTP: srv500.Client(), ChatBaseCN: srv500.URL, BillingBaseCN: srv500.URL}
	s500 := New(Config{Pool: p500, Upstream: up500, CheckinConcurrency: 1})
	cap500 := &progressCapture{}
	if err := s500.RunCheckinCtx(context.Background(), cap500.fn); err != nil {
		t.Fatalf("run 5xx: %v", err)
	}
	if daily, _, _ := f500.counts(); daily != 2 {
		t.Fatalf("5xx upstream-internal retry daily calls=%d want 2（scheduler 不叠加）", daily)
	}
	if done, _, fail, _, _, _ := cap500.last(); done != 1 || fail != 0 {
		t.Fatalf("5xx retry progress done=%d fail=%d want 1/0", done, fail)
	}
}

// TestIsTransientErrLayering scheduler 层重试分类：只接管 429（其余层已覆盖/不重试）。
func TestIsTransientErrLayering(t *testing.T) {
	ctx := context.Background()
	soft := &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, Msg: "rate limited"}
	if !isTransientErr(ctx, soft) {
		t.Error("ErrSoftRate(429) 应由 scheduler 重试")
	}
	server := &upstream.Error{Kind: upstream.ErrServer, Status: 500, Msg: "boom"}
	if isTransientErr(ctx, server) {
		t.Error("ErrServer(5xx) 已由 upstream 重试，scheduler 不应叠加")
	}
	client := &upstream.Error{Kind: upstream.ErrClient, Status: 403, Msg: "forbidden"}
	if isTransientErr(ctx, client) {
		t.Error("4xx 业务错误不应重试")
	}
	if isTransientErr(ctx, nil) {
		t.Error("nil 不应重试")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if isTransientErr(canceled, soft) {
		t.Error("ctx 已取消不应重试")
	}
}

// TestCheckinNoRetryOnAuthError 4xx 鉴权/凭证错误不重试（401 session dead 只打 1 次）。
func TestCheckinNoRetryOnAuthError(t *testing.T) {
	fastRetry(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/daily-checkin") {
			calls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/get-user-resource") {
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()
	p := newProgressPool(1)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, CheckinConcurrency: 1})
	cap := &progressCapture{}
	if err := s.RunCheckinCtx(context.Background(), cap.fn); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("auth error calls=%d want 1（4xx 不重试）", got)
	}
	if _, _, fail, _, _, _ := cap.last(); fail != 1 {
		t.Fatalf("auth error fail=%d want 1", fail)
	}
}

// TestDispatchBatchDecoupledFamilies 按族解耦：签到阻塞时旅行已按时启动并跑完。
func TestDispatchBatchDecoupledFamilies(t *testing.T) {
	fastTravel(t)
	checkinStarted := make(chan struct{})
	releaseCheckin := make(chan struct{})
	travelStatusHit := make(chan struct{})
	var checkinOnce, travelOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			checkinOnce.Do(func() { close(checkinStarted) })
			<-releaseCheckin // 模拟慢签到（阻塞整族）
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":600,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/activity/growth/buddy/info"):
			w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1}}}`))
		case strings.HasSuffix(r.URL.Path, "/activity/growth/buddy/travel/status"):
			travelOnce.Do(func() { close(travelStatusHit) })
			w.Write([]byte(`{"code":0,"data":{"state":"idle","daily_limit_reached":true}}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := newProgressPool(1)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, CheckinConcurrency: 1})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.dispatchBatch(ctx, []taskKind{taskCheckin, taskTravel})
	<-checkinStarted

	// 签到仍阻塞时，旅行必须已经跑起来（同批独立 goroutine，不 wg.Wait 整批）。
	select {
	case <-travelStatusHit:
	case <-time.After(3 * time.Second):
		t.Fatal("travel 未在 checkin 阻塞期间启动（按族解耦失效）")
	}
	close(releaseCheckin)
	s.batchWG.Wait()
	if s.TaskRunning(TaskCheckin) || s.TaskRunning(TaskTravel) {
		t.Fatal("批处理结束后 gate 应已释放")
	}
}

// TestScheduledBatchCreatesJob 定时批次进 Job 历史：trigger=schedule、kind 对应、
// 跑完 state=done 且 gate 释放。
func TestScheduledBatchCreatesJob(t *testing.T) {
	f := &checkinTestUpstream{}
	srv := f.server(t)
	defer srv.Close()
	p := newProgressPool(2)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	reg := jobs.New(jobs.Config{MaxConcurrency: 2})
	defer reg.Close()
	s := New(Config{Pool: p, Upstream: up, CheckinConcurrency: 2, Jobs: reg})

	s.dispatchBatch(context.Background(), []taskKind{taskCheckin})
	job := waitRegistryJob(t, reg, jobs.KindCheckin, jobs.TriggerSchedule)
	done := waitRegistryJobState(t, reg, job.ID, jobs.StateDone, 5*time.Second)
	if done.Total != 2 || done.Done != 2 || done.Trigger != jobs.TriggerSchedule {
		t.Fatalf("scheduled job=%+v want total=2 done=2 trigger=schedule", done)
	}
	if s.TaskRunning(TaskCheckin) {
		t.Fatal("Job 结束后 checkin gate 应已释放")
	}
}

// waitRegistryJob 等待某 kind/trigger 的 Job 出现（轮询；有上限）。
func waitRegistryJob(t *testing.T, reg *jobs.Registry, kind jobs.Kind, trigger jobs.Trigger) *jobs.Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, j := range reg.List(20) {
			if j.Kind == kind && j.Trigger == trigger {
				return j
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job kind=%s trigger=%s 未出现在 Registry", kind, trigger)
	return nil
}

// waitRegistryJobState 等待 Job 进入指定状态。
func waitRegistryJobState(t *testing.T, reg *jobs.Registry, id string, want jobs.State, timeout time.Duration) *jobs.Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if j, err := reg.Get(id); err == nil && j.State == want {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	j, _ := reg.Get(id)
	t.Fatalf("job %s state=%v want %v", id, j.State, want)
	return nil
}

// TestCatchUpBatchCreatesJobWithCatchUpTrigger 补跑批次进 Job 历史（trigger=catch_up）。
func TestCatchUpBatchCreatesJobWithCatchUpTrigger(t *testing.T) {
	reg := jobs.New(jobs.Config{MaxConcurrency: 2})
	defer reg.Close()
	s := New(Config{
		Pool:             pool.New(""),
		CheckinHours:     []int{9},
		TravelHours:      []int{9},
		ActivityHours:    []int{10},
		KeepaliveHours:   []int{22},
		BlackcatDisabled: true,
		GrowthDisabled:   true,
		CatchUpMinutes:   120,
		Jobs:             reg,
	})
	now := time.Date(2026, 7, 27, 10, 5, 0, 0, time.Local)
	withFixedNow(s, now)
	started := s.runCatchUps(context.Background(), now)
	if len(started) == 0 {
		t.Fatal("补跑窗口应命中至少一个任务族")
	}
	job := waitRegistryJob(t, reg, jobs.KindCheckin, jobs.TriggerCatchUp)
	waitRegistryJobState(t, reg, job.ID, jobs.StateDone, 5*time.Second)
}

// TestScheduledJobCancelStopsBatchEarly 面板 cancel 定时 Job → Job ctx 取消 →
// 签到批次提前停止（远少于全量账号数）、终态 canceled、gate 释放。
func TestScheduledJobCancelStopsBatchEarly(t *testing.T) {
	var daily atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			daily.Add(1)
			time.Sleep(15 * time.Millisecond) // 让「取消发生在批次中段」可稳定复现
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p := newProgressPool(20)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	reg := jobs.New(jobs.Config{MaxConcurrency: 2})
	defer reg.Close()
	s := New(Config{Pool: p, Upstream: up, CheckinConcurrency: 1, Jobs: reg})

	s.dispatchBatch(context.Background(), []taskKind{taskCheckin})
	job := waitRegistryJob(t, reg, jobs.KindCheckin, jobs.TriggerSchedule)

	// 等 Job 至少处理完 1 个账号（running + progress）后再取消。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		j, err := reg.Get(job.ID)
		if err == nil && j.State == jobs.StateRunning && j.Done >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if canceled, err := reg.Cancel(job.ID); err != nil || !canceled {
		t.Fatalf("cancel job = (%v,%v) want (true,nil)", canceled, err)
	}
	done := waitRegistryJobState(t, reg, job.ID, jobs.StateCanceled, 5*time.Second)
	if got := int(daily.Load()); got >= 20 {
		t.Fatalf("canceled batch processed %d accounts, want early stop (<20)", got)
	}
	if done.Done >= 20 {
		t.Fatalf("canceled job done=%d want <20（早停）", done.Done)
	}
	deadline = time.Now().Add(3 * time.Second)
	for s.TaskRunning(TaskCheckin) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.TaskRunning(TaskCheckin) {
		t.Fatal("canceled Job 结束后 checkin gate 应释放")
	}
}

// TestSetCheckinConcurrencyClamp setter/构造归一：<=0→3，>16→16，热更新下一轮读取。
func TestSetCheckinConcurrencyClamp(t *testing.T) {
	s := New(Config{})
	if got := s.CheckinConcurrency(); got != 3 {
		t.Fatalf("默认 CheckinConcurrency=%d want 3", got)
	}
	s.SetCheckinConcurrency(0)
	if got := s.CheckinConcurrency(); got != 3 {
		t.Fatalf("0 → %d want 3", got)
	}
	s.SetCheckinConcurrency(99)
	if got := s.CheckinConcurrency(); got != 16 {
		t.Fatalf("99 → %d want 16", got)
	}
	s.SetCheckinConcurrency(5)
	if got := s.CheckinConcurrency(); got != 5 {
		t.Fatalf("5 → %d want 5", got)
	}
	if got := New(Config{CheckinConcurrency: 100}).CheckinConcurrency(); got != 16 {
		t.Fatalf("构造 100 → %d want 16", got)
	}
}
