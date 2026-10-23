// tasks_run_test.go 钉住「单账号做任务」接口
// POST /panel/api/accounts/{uid}/tasks/run 的路由、门控与 summary 计数口径。
//
// 契约（前端按此实现，字段名不可改）：
//
//	{"ok":true,"uid":"...","results":[{"task_code":"...","status":"done|skipped|error","message":"..."}],
//	 "summary":{"total":N,"done":N,"skipped":N,"error":N,"claimed":N}}
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// tasksRunResp 前端契约的 Go 镜像（字段名与 JSON tag 即契约本身）。
type tasksRunResp struct {
	OK      bool   `json:"ok"`
	UID     string `json:"uid"`
	Message string `json:"message"`
	Results []struct {
		TaskCode string `json:"task_code"`
		Status   string `json:"status"`
		Message  string `json:"message"`
		// claimed 是执行器附加字段（契约之外的扩展），summary.claimed 的口径即它的计数。
		Claimed bool  `json:"claimed"`
		Credit  int64 `json:"credit"`
	} `json:"results"`
	Summary struct {
		Total   int `json:"total"`
		Done    int `json:"done"`
		Skipped int `json:"skipped"`
		Error   int `json:"error"`
		Claimed int `json:"claimed"`
	} `json:"summary"`
}

// tasksRunStub 假上游：任务列表 + 领奖 + 行为上报（默认成功）。
type tasksRunStub struct {
	// tasks 默认口径任务列表（mp 口径恒为空，避免触发小程序任务动作）。
	tasks []map[string]any

	reqs   atomic.Int32
	claims atomic.Int32
}

func (s *tasksRunStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.reqs.Add(1)
		isMP := r.Header.Get("X-Client-Platform") == "miniprogram"
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks":
			if isMP {
				w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[]}}`))
				return
			}
			out, _ := json.Marshal(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"tasks": s.tasks},
			})
			w.Write(out)
		case strings.HasSuffix(r.URL.Path, "/claim"):
			s.claims.Add(1)
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":5}}`))
		default:
			// 行为事件上报等一律成功（runAutoAll 的 done 路径）。
			w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		}
	}
}

// newTasksRunPanel 用给定假上游与账号搭一个带完整路由的面板（New 注册 mux）。
func newTasksRunPanel(t *testing.T, h http.HandlerFunc, accounts ...*auth.Auth) *Panel {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	pl := pool.New("")
	for _, a := range accounts {
		pl.Add(a)
	}
	return New(Config{
		Version: "test", APIKey: "test-key", Pool: pl,
		Upstream: &upstream.Client{
			HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
		},
	})
}

// postTasksRun 发一次「做任务」请求并解出契约响应。
func postTasksRun(t *testing.T, p *Panel, uid string) (*httptest.ResponseRecorder, tasksRunResp) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/accounts/"+uid+"/tasks/run", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	var resp tasksRunResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("响应不是合法 JSON: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, resp
}

// TestSummarizeTaskRunCounts 纯函数级计数口径：total=条目数；done/skipped/error
// 按 status 精确计数（未知 status 不归入任何一类）；claimed 只数 claimed==true。
func TestSummarizeTaskRunCounts(t *testing.T) {
	results := []map[string]any{
		{"task_code": "a", "status": "done", "claimed": true},
		{"task_code": "b", "status": "done"},
		{"task_code": "c", "status": "skipped", "claimed": true},
		{"task_code": "d", "status": "skipped", "claimed": false},
		{"task_code": "e", "status": "error", "message": "boom"},
		{"task_code": "f", "status": "unknown"},
		{}, // 空条目也计入 total，但不归入 done/skipped/error
	}
	got := summarizeTaskRun(results)
	want := taskRunSummary{Total: 7, Done: 2, Skipped: 2, Error: 1, Claimed: 2}
	if got != want {
		t.Fatalf("summarizeTaskRun = %+v, want %+v", got, want)
	}
	// 空结果（企业版/无凭证分支的 results:[]）→ 全零。
	if got := summarizeTaskRun(nil); got != (taskRunSummary{}) {
		t.Fatalf("summarizeTaskRun(nil) = %+v, want 全零", got)
	}
}

// TestPanelTasksRunRouteAndSummary 路由存在（200 而非 404/405）+ 契约字段齐全，
// 且 summary 与 results 严格自洽：total=len(results)、done/skipped/error 为
// status 计数、claimed 为 claimed==true 条数（本轮 chat_5 补领一次）。
func TestPanelTasksRunRouteAndSummary(t *testing.T) {
	// 执行器里各动作的节流在测试中无意义：归零避免拖慢；回读轮询压到 1 次。
	oldReportGap := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = oldReportGap })
	oldPollAttempts := claimPollAttempts
	claimPollAttempts = 1
	t.Cleanup(func() { claimPollAttempts = oldPollAttempts })

	stub := &tasksRunStub{tasks: []map[string]any{
		// 已达标未领：执行器走「补领 → skipped」，claimed 计数 +1。
		{"task_code": "chat_5", "title": "对话 5 次", "target": 5, "current": 5,
			"accept_status": "accepted", "reward_credit": 100, "reward_energy": 5},
		// 存在但无进度：执行器跑 runBuddyApp（默认上报成功）→ done。
		{"task_code": "Buddy_App", "title": "进入应用", "target": 0, "current": 0,
			"accept_status": "accepted"},
	}}
	p := newTasksRunPanel(t, stub.handler(), &auth.Auth{UID: "u1", AccessToken: "at"})

	rec, resp := postTasksRun(t, p, "u1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200（路由注册失败？body=%s）", rec.Code, rec.Body.String())
	}
	if !resp.OK || resp.UID != "u1" {
		t.Fatalf("ok=%v uid=%q, want true/u1; body=%s", resp.OK, resp.UID, rec.Body.String())
	}
	if len(resp.Results) == 0 {
		t.Fatalf("results 为空；body=%s", rec.Body.String())
	}
	// summary 必须与 results 自洽（前端只信后端计数）。
	var done, skipped, errored, claimed int
	for _, it := range resp.Results {
		switch it.Status {
		case "done":
			done++
		case "skipped":
			skipped++
		case "error":
			errored++
		}
		if it.Claimed {
			claimed++
		}
		if strings.TrimSpace(it.TaskCode) == "" {
			t.Errorf("results 条目缺少 task_code: %+v", it)
		}
		if strings.TrimSpace(it.Message) == "" {
			t.Errorf("results 条目缺少 message: %+v", it)
		}
	}
	sum := resp.Summary
	if sum.Total != len(resp.Results) || sum.Total != len(autoActions) {
		t.Errorf("summary.total=%d, want len(results)=%d / len(autoActions)=%d",
			sum.Total, len(resp.Results), len(autoActions))
	}
	if sum.Done != done || sum.Skipped != skipped || sum.Error != errored {
		t.Errorf("summary done/skipped/error = %d/%d/%d, results 统计 = %d/%d/%d\nresults=%s",
			sum.Done, sum.Skipped, sum.Error, done, skipped, errored, rec.Body.String())
	}
	// claimed 定义 = results[].claimed==true 的条数：本用例恰好 chat_5 达标补领一次。
	if sum.Claimed != claimed {
		t.Errorf("summary.claimed=%d, results[].claimed 计数=%d", sum.Claimed, claimed)
	}
	if sum.Claimed != 1 {
		t.Errorf("summary.claimed = %d, want 1（chat_5 达标未领，本轮补领 +100 分）", sum.Claimed)
	}
	if sum.Done != 1 {
		t.Errorf("summary.done = %d, want 1（Buddy_App 行为链路成功）", sum.Done)
	}
	if sum.Error != 0 {
		t.Errorf("summary.error = %d, want 0\nresults=%s", sum.Error, rec.Body.String())
	}
	if got := stub.claims.Load(); got != 1 {
		t.Errorf("领奖端点调用 %d 次, want 1", got)
	}
}

// TestPanelTasksRunUnknownUID 未知 uid 必须 404，且不发起任何上游请求。
func TestPanelTasksRunUnknownUID(t *testing.T) {
	calls := atomic.Int32{}
	p := newTasksRunPanel(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}, &auth.Auth{UID: "u1", AccessToken: "at"})

	rec, _ := postTasksRun(t, p, "nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "account not found") {
		t.Fatalf("body=%s, want account not found", rec.Body.String())
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("未知 uid 不应发起上游请求，got %d", got)
	}
}

// TestPanelTasksRunEnterpriseRejected 企业版账号：200 + ok:false + message，
// **零上游请求**。这是刻意钉住的契约（不是 4xx）——前端 api() 只看 HTTP 状态码，
// 4xx 会弹通用错误，拿不到可读原因。
func TestPanelTasksRunEnterpriseRejected(t *testing.T) {
	calls := atomic.Int32{}
	p := newTasksRunPanel(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}, &auth.Auth{UID: "e1", AccessToken: "at", EnterpriseID: "ent-1"})

	rec, resp := postTasksRun(t, p, "e1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200（企业版门控是 ok:false 而非 4xx）; body=%s", rec.Code, rec.Body.String())
	}
	if resp.OK {
		t.Fatalf("ok=true, want false; body=%s", rec.Body.String())
	}
	if resp.UID != "e1" || !strings.Contains(resp.Message, "企业") {
		t.Fatalf("uid=%q message=%q, want uid=e1 且 message 含「企业」", resp.UID, resp.Message)
	}
	// 形状稳定：ok:false 也要有 results:[]（而非 null）与全零 summary。
	if resp.Results == nil {
		t.Error("results 必须是 [] 而非 null（前端可直接 .length）")
	}
	if len(resp.Results) != 0 || resp.Summary != (taskRunSummary{}) {
		t.Errorf("results=%v summary=%+v, want 空/全零", resp.Results, resp.Summary)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("企业版账号不应发起上游请求，got %d", got)
	}
}

// TestPanelTasksRunNoCredentials 无凭证账号：同 200 + ok:false + message，零上游请求。
func TestPanelTasksRunNoCredentials(t *testing.T) {
	calls := atomic.Int32{}
	p := newTasksRunPanel(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}, &auth.Auth{UID: "u1"})

	rec, resp := postTasksRun(t, p, "u1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if resp.OK || !strings.Contains(resp.Message, "凭证") {
		t.Fatalf("ok=%v message=%q, want false 且 message 含「凭证」", resp.OK, resp.Message)
	}
	if resp.Results == nil || len(resp.Results) != 0 {
		t.Errorf("results=%v, want []", resp.Results)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("无凭证账号不应发起上游请求，got %d", got)
	}
}

// TestPanelTasksRunBusyAccount 同账号已有任务动作在跑（per-account 锁被占）→ 409，
// 不并发跑两遍（动作虽幂等，expert 系每遍含真实对话配额）。
func TestPanelTasksRunBusyAccount(t *testing.T) {
	stub := &tasksRunStub{}
	p := newTasksRunPanel(t, stub.handler(), &auth.Auth{UID: "u1", AccessToken: "at"})
	if !p.tryLockAccount("u1") {
		t.Fatal("模拟占用账号锁失败")
	}
	defer p.unlockAccount("u1")

	rec, _ := postTasksRun(t, p, "u1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if got := stub.reqs.Load(); got != 0 {
		t.Fatalf("账号忙时不应发起上游请求，got %d", got)
	}
}
