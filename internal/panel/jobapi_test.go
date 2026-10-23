// jobapi_test.go v2.3.0 #10 Job API / v2.3.0 #12 meta 路由注册的契约测试：
//
//   - Job API（GET /jobs、GET /jobs/{id}、POST /jobs/{kind}/run、POST /jobs/{id}/cancel）
//     的 401/404/409/200 与 job_id 契约；
//   - 旧批量端点保留 ok/started 并新增 job_id；409 语义不变；
//   - 单号 tasks/run 写入 kind=tasks_run 的 Job 历史（成功/失败都记，响应契约不变）；
//   - meta 路由已在 panel.go routes() 注册，且 accountRemove 成功后清理备注。
package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/jobs"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/scheduler"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// jobPanelFixture 装配了 Job Registry 的面板（dataDir/Jobs 落 t.TempDir，无副作用）。
type jobPanelFixture struct {
	p     *Panel
	reg   *jobs.Registry
	sch   *scheduler.Scheduler
	pl    *pool.Pool
	dir   string
	upSrv *httptest.Server // 可能为 nil（空池用例）
}

// newJobPanelFixture 构造面板：pool/upstream 可选（nil = 空池、无上游）。
func newJobPanelFixture(t *testing.T, pl *pool.Pool, up *upstream.Client, upSrv *httptest.Server) *jobPanelFixture {
	t.Helper()
	if pl == nil {
		pl = pool.New("")
	}
	dir := t.TempDir()
	reg := jobs.New(jobs.Config{
		Path:           filepath.Join(dir, "jobs.json"),
		Retain:         20,
		MaxConcurrency: 2,
	})
	sch := scheduler.New(scheduler.Config{Pool: pl, Upstream: up})
	p := New(Config{
		Version:   "test",
		APIKey:    accountsTestKey,
		Pool:      pl,
		Upstream:  up,
		Scheduler: sch,
		Jobs:      reg,
		StateFile: filepath.Join(dir, "state.json"),
	})
	f := &jobPanelFixture{p: p, reg: reg, sch: sch, pl: pl, dir: dir, upSrv: upSrv}
	t.Cleanup(func() {
		reg.Close()
		// 等后台 Job goroutine 收敛（含最后一次 jobs.json 落盘）再删临时目录，
		// 避免 Windows 上「目录已删、rename 报 Access is denied」的测试噪音。
		waitCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		reg.Wait(waitCtx)
		if upSrv != nil {
			upSrv.Close()
		}
	})
	return f
}

// newJobUpstream 最小上游桩：签到/余额/refresh 成功，其余 404（streak 等旁路失败仅日志）。
func newJobUpstream(t *testing.T) (*upstream.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":700,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return up, srv
}

// jobAddAccounts 向池里加 n 个可用 CN 账号。
func jobAddAccounts(pl *pool.Pool, n int) {
	for i := 0; i < n; i++ {
		pl.Add(&auth.Auth{UID: "u" + string(rune('0'+i)), AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
}

// doJobReq 经完整 ServeHTTP 链路发请求（覆盖路由/鉴权），auth=false 时不带 Bearer。
func doJobReq(t *testing.T, p *Panel, method, path string, body string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+accountsTestKey)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// decodeJobResp 解析 {ok, job_id, job, jobs, canceled, error} 通用响应。
type jobResp struct {
	OK       bool        `json:"ok"`
	Started  bool        `json:"started"`
	JobID    string      `json:"job_id"`
	Job      *jobs.Job   `json:"job"`
	Jobs     []*jobs.Job `json:"jobs"`
	Canceled bool        `json:"canceled"`
	Error    string      `json:"error"`
}

func decodeJobResp(t *testing.T, rec *httptest.ResponseRecorder) jobResp {
	t.Helper()
	var r jobResp
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, rec.Body.String())
	}
	return r
}

// waitJobState 轮询 Registry 直到 Job 进入期望状态。
func waitJobState(t *testing.T, reg *jobs.Registry, id string, want jobs.State, timeout time.Duration) *jobs.Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		j, err := reg.Get(id)
		if err == nil && j.State == want {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	j, _ := reg.Get(id)
	t.Fatalf("job %s state=%v want %v (timeout)", id, j.State, want)
	return nil
}

// TestJobAPIAuthRequired 四个 Job 端点未带 Bearer → 401（路由经 withAuth）。
func TestJobAPIAuthRequired(t *testing.T) {
	f := newJobPanelFixture(t, nil, nil, nil)
	cases := []struct{ method, path string }{
		{http.MethodGet, "/panel/api/jobs"},
		{http.MethodGet, "/panel/api/jobs/x"},
		{http.MethodPost, "/panel/api/jobs/checkin/run"},
		{http.MethodPost, "/panel/api/jobs/x/cancel"},
	}
	for _, tc := range cases {
		rec := doJobReq(t, f.p, tc.method, tc.path, "", false)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status=%d want 401", tc.method, tc.path, rec.Code)
		}
	}
}

// TestJobAPINilRegistryNotImplemented 未装配 Registry：Job API 返回 501。
func TestJobAPINilRegistryNotImplemented(t *testing.T) {
	pl := pool.New("")
	sch := scheduler.New(scheduler.Config{Pool: pl})
	p := New(Config{Version: "test", APIKey: accountsTestKey, Pool: pl, Scheduler: sch})
	cases := []struct{ method, path string }{
		{http.MethodGet, "/panel/api/jobs"},
		{http.MethodGet, "/panel/api/jobs/x"},
		{http.MethodPost, "/panel/api/jobs/checkin/run"},
		{http.MethodPost, "/panel/api/jobs/x/cancel"},
	}
	for _, tc := range cases {
		rec := doJobReq(t, p, tc.method, tc.path, "", true)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s status=%d want 501; body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// TestJobRunGetList 核心链路：POST run → 200 job_id → 轮询 GET 进度 → done；
// GET /jobs 列表包含该 Job；未知 id 404；未知 kind 400。
func TestJobRunGetList(t *testing.T) {
	pl := pool.New("")
	jobAddAccounts(pl, 2)
	up, srv := newJobUpstream(t)
	f := newJobPanelFixture(t, pl, up, srv)

	rec := doJobReq(t, f.p, http.MethodPost, "/panel/api/jobs/checkin/run", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("run status=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeJobResp(t, rec)
	if !resp.OK || !resp.Started || resp.JobID == "" {
		t.Fatalf("run resp=%+v body=%s", resp, rec.Body.String())
	}

	done := waitJobState(t, f.reg, resp.JobID, jobs.StateDone, 5*time.Second)
	if done.Kind != jobs.KindCheckin || done.Trigger != jobs.TriggerManual {
		t.Fatalf("job meta=%+v", done)
	}
	if done.Total != 2 || done.Done != 2 || done.Fail != 0 || done.Skip != 0 {
		t.Fatalf("job counters=%+v want total=2 done=2", done)
	}

	// GET /jobs/{id}
	rec = doJobReq(t, f.p, http.MethodGet, "/panel/api/jobs/"+resp.JobID, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("get job status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeJobResp(t, rec)
	if got.Job == nil || got.Job.ID != resp.JobID || got.Job.State != jobs.StateDone {
		t.Fatalf("get job resp=%+v", got)
	}

	// GET /jobs?limit=1
	rec = doJobReq(t, f.p, http.MethodGet, "/panel/api/jobs?limit=1", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	list := decodeJobResp(t, rec)
	if len(list.Jobs) != 1 || list.Jobs[0].ID != resp.JobID {
		t.Fatalf("list resp=%+v", list)
	}

	// 未知 id / 未知 kind
	if rec := doJobReq(t, f.p, http.MethodGet, "/panel/api/jobs/nope", "", true); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id status=%d want 404", rec.Code)
	}
	if rec := doJobReq(t, f.p, http.MethodPost, "/panel/api/jobs/bogus/run", "", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown kind status=%d want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestJobRunConflict409 同类 gate 被占（定时批次/上一轮手动任务仍在跑）→ 409。
func TestJobRunConflict409(t *testing.T) {
	pl := pool.New("")
	up, srv := newJobUpstream(t)
	f := newJobPanelFixture(t, pl, up, srv)
	if !f.sch.BeginTask(scheduler.TaskCheckin) {
		t.Fatal("前置：占用 checkin gate 失败")
	}
	defer f.sch.EndTask(scheduler.TaskCheckin)

	rec := doJobReq(t, f.p, http.MethodPost, "/panel/api/jobs/checkin/run", "", true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d want 409; body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeJobResp(t, rec)
	if resp.OK || !strings.Contains(resp.Error, "正在执行") {
		t.Fatalf("409 resp=%+v want ok:false + 正在执行", resp)
	}
}

// TestJobCancel 取消运行中 Job：200 {ok,canceled:true} → 终态 canceled；
// 未知 id 404。（Job 由 Registry 直接创建、挂起等待 ctx，模拟长任务。）
func TestJobCancel(t *testing.T) {
	pl := pool.New("")
	up, srv := newJobUpstream(t)
	f := newJobPanelFixture(t, pl, up, srv)

	started := make(chan struct{})
	job, err := f.reg.Run(jobs.KindActivity, jobs.TriggerManual, 1,
		func(ctx context.Context, p jobs.Progress) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
	<-started

	rec := doJobReq(t, f.p, http.MethodPost, "/panel/api/jobs/"+job.ID+"/cancel", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeJobResp(t, rec)
	if !resp.OK || !resp.Canceled {
		t.Fatalf("cancel resp=%+v", resp)
	}
	waitJobState(t, f.reg, job.ID, jobs.StateCanceled, 3*time.Second)

	if rec := doJobReq(t, f.p, http.MethodPost, "/panel/api/jobs/nope/cancel", "", true); rec.Code != http.StatusNotFound {
		t.Fatalf("cancel unknown status=%d want 404", rec.Code)
	}
}

// TestOldBatchEndpointsReturnJobID 旧批量端点：保留 ok/started，新增 job_id；
// Job 落 Registry 且能跑到 done。409 语义由既有测试覆盖（此处只验契约字段）。
func TestOldBatchEndpointsReturnJobID(t *testing.T) {
	pl := pool.New("") // 空池：任务体快速完成
	f := newJobPanelFixture(t, pl, nil, nil)

	cases := []struct {
		path string
		kind jobs.Kind
	}{
		{"/panel/api/checkin_all", jobs.KindCheckin},
		{"/panel/api/travel_all", jobs.KindTravel},
		{"/panel/api/activity_all", jobs.KindActivity},
		{"/panel/api/keepalive_all", jobs.KindKeepalive},
		{"/panel/api/balance_all", jobs.KindBalance},
	}
	for _, tc := range cases {
		rec := doJobReq(t, f.p, http.MethodPost, tc.path, "", true)
		if rec.Code != http.StatusOK {
			t.Errorf("%s status=%d body=%s", tc.path, rec.Code, rec.Body.String())
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("%s decode: %v", tc.path, err)
			continue
		}
		if body["ok"] != true || body["started"] != true {
			t.Errorf("%s body=%s（旧字段 ok/started 必须保留）", tc.path, rec.Body.String())
		}
		jobID, _ := body["job_id"].(string)
		if jobID == "" {
			t.Errorf("%s body=%s（装配 Registry 时应返回 job_id）", tc.path, rec.Body.String())
			continue
		}
		done := waitJobState(t, f.reg, jobID, jobs.StateDone, 5*time.Second)
		if done.Kind != tc.kind {
			t.Errorf("%s job kind=%s want %s", tc.path, done.Kind, tc.kind)
		}
	}
}

// TestTasksRunRecordsJobHistory 单号 tasks/run 失败（未知 uid → 404）也写一条
// kind=tasks_run 的 Job 历史（成功/失败都记），且同步响应契约不变。
func TestTasksRunRecordsJobHistory(t *testing.T) {
	pl := pool.New("")
	f := newJobPanelFixture(t, pl, nil, nil)

	before := len(f.reg.List(50))
	rec := doJobReq(t, f.p, http.MethodPost, "/panel/api/accounts/ghost-uid/tasks/run", "", true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown uid status=%d want 404; body=%s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		list := f.reg.List(50)
		for _, j := range list {
			if j.Kind == jobs.KindTasksRun {
				if j.State != jobs.StateFailed || j.Error == "" {
					t.Fatalf("tasks_run history job=%+v want failed+error", j)
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("tasks_run job history not recorded (before=%d, now=%d)", before, len(f.reg.List(50)))
}

// TestMetaRoutesRegistered meta 路由已注册（GET/PUT/DELETE 全链路），且
// accountRemove 成功后调用 accountMetaForget 清理备注。
func TestMetaRoutesRegistered(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1"})
	f := newJobPanelFixture(t, pl, nil, nil)

	// GET 列表（初始空）
	rec := doJobReq(t, f.p, http.MethodGet, "/panel/api/meta", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("meta list status=%d body=%s", rec.Code, rec.Body.String())
	}
	// PUT 备注
	rec = doJobReq(t, f.p, http.MethodPut, "/panel/api/accounts/u1/meta", `{"note":"主号","tags":["A"]}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("meta put status=%d body=%s", rec.Code, rec.Body.String())
	}
	// GET 单号
	rec = doJobReq(t, f.p, http.MethodGet, "/panel/api/accounts/u1/meta", "", true)
	var got struct {
		OK   bool   `json:"ok"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || !got.OK || got.Note != "主号" {
		t.Fatalf("meta get resp=%+v err=%v body=%s", got, err, rec.Body.String())
	}
	// DELETE 幂等
	if rec := doJobReq(t, f.p, http.MethodDelete, "/panel/api/accounts/u1/meta", "", true); rec.Code != http.StatusOK {
		t.Fatalf("meta delete status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 重新写入 → 删除账号 → 备注应被 accountRemove 清理
	doJobReq(t, f.p, http.MethodPut, "/panel/api/accounts/u1/meta", `{"note":"待清理"}`, true)
	rec = doJobReq(t, f.p, http.MethodPost, "/panel/api/accounts/u1/remove", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := pl.Status("u1"); ok {
		t.Fatal("u1 should be removed from pool")
	}
	rec = doJobReq(t, f.p, http.MethodGet, "/panel/api/accounts/u1/meta", "", true)
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Note != "" {
		t.Fatalf("removed account meta should be cleared, resp=%+v body=%s", got, rec.Body.String())
	}
}
