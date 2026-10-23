// taskhistory_test.go 任务执行历史（v2.2.0 #9 后端）的写入/读取/集成点测试。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/scheduler"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// taskHistoryTestPanel 构造启用任务历史落盘（StateFile 在 t.TempDir）的面板。
func taskHistoryTestPanel(t *testing.T) (*Panel, *pool.Pool, string) {
	t.Helper()
	dir := t.TempDir()
	pl := pool.New("")
	p := New(Config{
		Version: "test", APIKey: accountsTestKey, Pool: pl,
		StateFile: filepath.Join(dir, "state.json"),
	})
	return p, pl, dir
}

// readTaskRuns 调 GET /panel/api/task_runs 并解出历史（断言 200）。
func readTaskRuns(t *testing.T, p *Panel, query string) ([]TaskRun, int) {
	t.Helper()
	path := "/panel/api/task_runs"
	if query != "" {
		path += "?" + query
	}
	rec := panelRequest(p, http.MethodGet, path, accountsTestKey, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, rec.Code, rec.Body.String())
	}
	var resp struct {
		OK    bool      `json:"ok"`
		Runs  []TaskRun `json:"runs"`
		Limit int       `json:"limit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode task_runs: %v body=%s", err, rec.Body.String())
	}
	if !resp.OK {
		t.Fatalf("ok=false: %s", rec.Body.String())
	}
	return resp.Runs, resp.Limit
}

// waitTaskRuns 轮询等待异步任务收尾写入历史（最多 3s）。
func waitTaskRuns(t *testing.T, p *Panel, want int) []TaskRun {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last []TaskRun
	for time.Now().Before(deadline) {
		last, _ = readTaskRuns(t, p, "limit=200")
		if len(last) >= want {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待任务历史写入超时：got %d want %d（runs=%+v）", len(last), want, last)
	return nil
}

// TestTaskRunRecordAndRead 记录/查询：字段齐全、新的在前、limit 生效、文件不存在返回 []。
func TestTaskRunRecordAndRead(t *testing.T) {
	p, _, _ := taskHistoryTestPanel(t)

	// 尚无文件：runs:[]（不是 null），limit 回显默认值。
	rec := panelRequest(p, http.MethodGet, "/panel/api/task_runs", accountsTestKey, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("空历史 status=%d body=%s", rec.Code, rec.Body.String())
	}
	var empty struct {
		OK    bool              `json:"ok"`
		Runs  []json.RawMessage `json:"runs"`
		Limit int               `json:"limit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(empty.Runs) != 0 || empty.Limit != taskRunsDefaultLimit {
		t.Fatalf("空历史 runs=%d limit=%d want 0/%d", len(empty.Runs), empty.Limit, taskRunsDefaultLimit)
	}

	p.recordTaskRun(TaskRun{Kind: taskKindCheckin, Trigger: taskTriggerManual, UID: "u1",
		StartedAt: 100, FinishedAt: 110, Total: 1, Done: 1})
	p.recordTaskRun(TaskRun{Kind: taskKindCatchUp, Trigger: taskTriggerSchedule,
		StartedAt: 200, FinishedAt: 220, Total: 3, Done: 2, Fail: 1, Error: "部分账号领奖失败"})

	runs, limit := readTaskRuns(t, p, "limit=10")
	if len(runs) != 2 || limit != 10 {
		t.Fatalf("runs=%d limit=%d want 2/10", len(runs), limit)
	}
	if got := runs[0]; got.Kind != taskKindCatchUp || got.Trigger != taskTriggerSchedule ||
		got.Total != 3 || got.Done != 2 || got.Fail != 1 || got.Skip != 0 ||
		got.Error != "部分账号领奖失败" || got.ID == "" || got.StartedAt != 200 || got.FinishedAt != 220 {
		t.Fatalf("最新一条=%+v", got)
	}
	if got := runs[1]; got.Kind != taskKindCheckin || got.UID != "u1" || got.Done != 1 {
		t.Fatalf("较早一条=%+v", got)
	}

	// limit=1 只回最新一条。
	one, _ := readTaskRuns(t, p, "limit=1")
	if len(one) != 1 || one[0].Kind != taskKindCatchUp {
		t.Fatalf("limit=1 runs=%+v", one)
	}
}

// TestTaskRunsRequiresAuth 任务历史查询走 withAuth。
func TestTaskRunsRequiresAuth(t *testing.T) {
	p, _, _ := taskHistoryTestPanel(t)
	if rec := panelRequest(p, http.MethodGet, "/panel/api/task_runs", "", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无密钥 status=%d want 401", rec.Code)
	}
}

// TestTaskRunConcurrentRecord 并发写历史：mutex 串行、无丢失、id 唯一。
func TestTaskRunConcurrentRecord(t *testing.T) {
	p, _, _ := taskHistoryTestPanel(t)
	const n = 30
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			p.recordTaskRun(TaskRun{Kind: taskKindTasksRun, Trigger: taskTriggerManual, Total: i})
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
	runs, _ := readTaskRuns(t, p, "limit=200")
	if len(runs) != n {
		t.Fatalf("并发写入后 runs=%d want %d", len(runs), n)
	}
	seen := map[string]bool{}
	for _, r := range runs {
		if r.ID == "" {
			t.Fatal("历史条目 id 为空")
		}
		if seen[r.ID] {
			t.Fatalf("历史 id 重复: %s", r.ID)
		}
		seen[r.ID] = true
	}
}

// TestWithTaskRunCaptureMapping 「做任务」中间件从响应 summary 提取计数写历史：
// 成功才记（ok:false 与非 2xx 都不记），字段映射 done=done / fail=error / skip=skipped。
func TestWithTaskRunCaptureMapping(t *testing.T) {
	p, _, _ := taskHistoryTestPanel(t)

	success := p.withTaskRunCapture(taskKindTasksRun, taskTriggerManual,
		func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "uid": "u9",
				"summary": map[string]any{"total": 7, "done": 4, "skipped": 2, "error": 1, "claimed": 2},
			})
		})
	success(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/panel/api/accounts/u9/tasks/run", nil))

	runs, _ := readTaskRuns(t, p, "limit=10")
	if len(runs) != 1 {
		t.Fatalf("成功执行应写 1 条历史，got %d", len(runs))
	}
	if got := runs[0]; got.Kind != taskKindTasksRun || got.Trigger != taskTriggerManual ||
		got.UID != "u9" || got.Total != 7 || got.Done != 4 || got.Fail != 1 || got.Skip != 2 ||
		got.StartedAt <= 0 || got.FinishedAt < got.StartedAt {
		t.Fatalf("历史映射错误: %+v", got)
	}

	// 业务拒绝（ok:false）不记。
	reject := p.withTaskRunCapture(taskKindTasksRun, taskTriggerManual,
		func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "企业版不可用", "summary": taskRunSummary{}})
		})
	reject(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/panel/api/accounts/u9/tasks/run", nil))
	// 执行级失败（503）不记。
	errored := p.withTaskRunCapture(taskKindTasksRun, taskTriggerManual,
		func(w http.ResponseWriter, r *http.Request) {
			writeErr(w, http.StatusServiceUnavailable, "boom")
		})
	errored(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/panel/api/accounts/u9/tasks/run", nil))

	if runs, _ := readTaskRuns(t, p, "limit=10"); len(runs) != 1 {
		t.Fatalf("ok:false / 非 2xx 不应写历史，got %d 条", len(runs))
	}
}

// TestTriggerBatchTaskHistory 批量任务统一收尾写历史：kind/trigger/total 正确，
// 结束时间不早于开始时间。
func TestTriggerBatchTaskHistory(t *testing.T) {
	p, pl, _ := taskHistoryTestPanel(t)
	pl.Add(&auth.Auth{UID: "u1"})
	pl.Add(&auth.Auth{UID: "u2"})
	sch := scheduler.New(scheduler.Config{Pool: pl})
	p.cfg.Scheduler = sch

	if !p.triggerBatchTask(sch, scheduler.TaskCheckin, taskKindCheckin, func() {}) {
		t.Fatal("空 gate 应能触发")
	}
	runs := waitTaskRuns(t, p, 1)
	got := runs[0]
	if got.Kind != taskKindCheckin || got.Trigger != taskTriggerManual || got.Total != 2 {
		t.Fatalf("批量历史=%+v want kind=checkin trigger=manual total=2", got)
	}
	if got.StartedAt <= 0 || got.FinishedAt < got.StartedAt {
		t.Fatalf("时间戳错误: %+v", got)
	}
	if got.Done != 0 || got.Fail != 0 || got.Skip != 0 {
		t.Fatalf("调度器内部统计不可得时应留空，got %+v", got)
	}
}

// TestBatchEndpointHistoryHTTP 走真实路由 POST /checkin_all：任务历史 + 审计双落盘。
func TestBatchEndpointHistoryHTTP(t *testing.T) {
	p, _, _ := taskHistoryTestPanel(t)
	sch := scheduler.New(scheduler.Config{Pool: p.cfg.Pool})
	p.cfg.Scheduler = sch

	rec := panelRequest(p, http.MethodPost, "/panel/api/checkin_all", accountsTestKey, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("checkin_all status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool `json:"ok"`
		Started bool `json:"started"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.OK || !resp.Started {
		t.Fatalf("checkin_all 响应=%s err=%v", rec.Body.String(), err)
	}

	runs := waitTaskRuns(t, p, 1)
	if got := runs[0]; got.Kind != taskKindCheckin || got.Trigger != taskTriggerManual {
		t.Fatalf("批量历史=%+v", got)
	}
	entries, _ := readAudit(t, p, "limit=10")
	found := false
	for _, e := range entries {
		if e.Action == auditActionCheckinAll && e.OK {
			found = true
		}
	}
	if !found {
		t.Fatalf("审计里应有 checkin_all 成功记录: %+v", entries)
	}
}

// TestBatchConflictAudit 单飞 gate 被占时重复触发：409 记审计失败、不写任务历史。
func TestBatchConflictAudit(t *testing.T) {
	p, _, _ := taskHistoryTestPanel(t)
	sch := scheduler.New(scheduler.Config{Pool: p.cfg.Pool})
	p.cfg.Scheduler = sch
	if !sch.BeginTask(scheduler.TaskCheckin) {
		t.Fatal("预占 gate 失败")
	}
	defer sch.EndTask(scheduler.TaskCheckin)

	rec := panelRequest(p, http.MethodPost, "/panel/api/checkin_all", accountsTestKey, "", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("冲突 status=%d want 409 body=%s", rec.Code, rec.Body.String())
	}
	entries, _ := readAudit(t, p, "limit=10")
	if len(entries) == 0 || entries[0].Action != auditActionCheckinAll || entries[0].OK {
		t.Fatalf("审计应记 checkin_all 失败: %+v", entries)
	}
	if runs, _ := readTaskRuns(t, p, "limit=10"); len(runs) != 0 {
		t.Fatalf("冲突不应写任务历史，got %+v", runs)
	}
}

// TestTasksRunIntegrationHistory 真实「做任务」接口（假上游）端到端：
// 响应 summary → 任务历史（kind=tasks_run/uid/计数）+ 审计（tasks_run ok=true）。
func TestTasksRunIntegrationHistory(t *testing.T) {
	// 执行器节流归零、回读轮询压到 1 次（与 tasks_run_test.go 同口径，仅为提速）。
	oldReportGap := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = oldReportGap })
	oldPollAttempts := claimPollAttempts
	claimPollAttempts = 1
	t.Cleanup(func() { claimPollAttempts = oldPollAttempts })

	stub := &tasksRunStub{tasks: []map[string]any{
		{"task_code": "Buddy_App", "title": "进入应用", "target": 0, "current": 0, "accept_status": "accepted"},
	}}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p := New(Config{
		Version: "test", APIKey: accountsTestKey, Pool: pl,
		StateFile: filepath.Join(dir, "state.json"),
		Upstream: &upstream.Client{
			HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
		},
	})

	rec := panelRequest(p, http.MethodPost, "/panel/api/accounts/u1/tasks/run", accountsTestKey, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("tasks/run status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp tasksRunResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.OK {
		t.Fatalf("tasks/run 响应解码失败: %v body=%s", err, rec.Body.String())
	}

	runs, _ := readTaskRuns(t, p, "limit=10")
	if len(runs) != 1 {
		t.Fatalf("应写 1 条任务历史，got %d（%+v）", len(runs), runs)
	}
	got := runs[0]
	if got.Kind != taskKindTasksRun || got.Trigger != taskTriggerManual || got.UID != "u1" {
		t.Fatalf("历史元信息=%+v", got)
	}
	if got.Total != resp.Summary.Total || got.Done != resp.Summary.Done ||
		got.Fail != resp.Summary.Error || got.Skip != resp.Summary.Skipped {
		t.Fatalf("历史计数与响应 summary 不一致: run=%+v summary=%+v", got, resp.Summary)
	}

	entries, _ := readAudit(t, p, "limit=10")
	if len(entries) == 0 || entries[0].Action != auditActionTasksRun || !entries[0].OK || entries[0].UID != "u1" {
		t.Fatalf("审计应记 tasks_run 成功（uid 取自路径）: %+v", entries)
	}
}
