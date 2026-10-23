// batch_task_test.go 钉住 v2.0.1 #10 面板批量端点的契约：
//
//   - checkin_all / travel_all / activity_all / keepalive_all / balance_all 命中
//     同类任务正在执行（单飞 gate 被占）→ 409 + {"ok":false,"error":"…正在执行中"}；
//   - balance_all 异步：先占 gate 立即返回 {ok:true,started:true}，不再同步等全池
//     刷完（后台还在跑时响应已返回），也不再同步带回 accounts；重复点击 409。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/scheduler"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// newBatchPanel 构造批量端点测试用面板：空池、无上游依赖（任务体遍历 0 账号）。
func newBatchPanel(t *testing.T) (*Panel, *scheduler.Scheduler) {
	t.Helper()
	pl := pool.New("")
	sch := scheduler.New(scheduler.Config{Pool: pl})
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, Scheduler: sch})
	return p, sch
}

// doPanelPost 带 Bearer 发起面板 POST（走 ServeHTTP，覆盖鉴权/压缩包装）。
func doPanelPost(t *testing.T, p *Panel, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	return rec
}

// TestBatchEndpointsSingleFlight409 五个 *_all 端点：同类 gate 被占（定时批次或
// 重复点击）时返回 409，error 文案说明「正在执行」，旧字段 ok=false 保留。
func TestBatchEndpointsSingleFlight409(t *testing.T) {
	cases := []struct {
		name string
		path string
		kind scheduler.TaskKind
	}{
		{"checkin_all", "/panel/api/checkin_all", scheduler.TaskCheckin},
		{"travel_all", "/panel/api/travel_all", scheduler.TaskTravel},
		{"activity_all", "/panel/api/activity_all", scheduler.TaskActivity},
		{"keepalive_all", "/panel/api/keepalive_all", scheduler.TaskKeepalive},
		{"balance_all", "/panel/api/balance_all", scheduler.TaskBalance},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, sch := newBatchPanel(t)
			if !sch.BeginTask(tc.kind) {
				t.Fatalf("前置：占用 %s gate 失败", tc.kind)
			}
			defer sch.EndTask(tc.kind)

			rec := doPanelPost(t, p, tc.path)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status=%d want 409; body=%s", rec.Code, rec.Body.String())
			}
			var body struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode 409 body: %v; body=%s", err, rec.Body.String())
			}
			if body.OK {
				t.Errorf("409 响应 ok 应为 false; body=%s", rec.Body.String())
			}
			if !strings.Contains(body.Error, "正在执行") {
				t.Errorf("409 error 应说明正在执行，got %q", body.Error)
			}
		})
	}
}

// TestBatchEndpointsStartOK 未在跑时五个端点都返回 200 + {ok:true,started:true}
// （旧脚本依赖的字段不得丢）；gate 在后台任务结束后自动释放。
func TestBatchEndpointsStartOK(t *testing.T) {
	paths := []string{
		"/panel/api/checkin_all",
		"/panel/api/travel_all",
		"/panel/api/activity_all",
		"/panel/api/keepalive_all",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			p, _ := newBatchPanel(t)
			rec := doPanelPost(t, p, path)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["ok"] != true || body["started"] != true {
				t.Fatalf("body=%s want {ok:true,started:true}", rec.Body.String())
			}
		})
	}
}

// TestBalanceAllAsync 核心契约：balance_all 在后台任务仍阻塞于上游时就已经返回
// 200 + {ok:true,started:true}（异步，不再等全池刷完触发反代 504）；随后重复点击
// 命中单飞 gate → 409。后台完成后 gate 释放。
func TestBalanceAllAsync(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // 模拟慢上游：后台任务阻塞在这里
		w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`))
	}))
	defer srv.Close()

	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	sch := scheduler.New(scheduler.Config{Pool: pl, Upstream: up})
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, Scheduler: sch, Upstream: up})

	start := time.Now()
	rec := doPanelPost(t, p, "/panel/api/balance_all")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("balance_all 仍同步阻塞：%v", elapsed)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["ok"] != true || body["started"] != true {
		t.Fatalf("body=%s want {ok:true,started:true}", rec.Body.String())
	}
	if _, has := body["accounts"]; has {
		t.Error("异步 balance_all 不应再同步返回 accounts（前端改为拉 overview）")
	}
	if !sch.TaskRunning(scheduler.TaskBalance) {
		t.Fatal("响应返回时后台余额刷新应仍在执行（gate 已占）")
	}

	// 重复点击：同类正在执行 → 409（与其余 *_all 同口径）。
	rec2 := doPanelPost(t, p, "/panel/api/balance_all")
	if rec2.Code != http.StatusConflict {
		t.Fatalf("重复点击 status=%d want 409; body=%s", rec2.Code, rec2.Body.String())
	}

	// 放行上游：后台跑完 → gate 释放。
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for sch.TaskRunning(scheduler.TaskBalance) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if sch.TaskRunning(scheduler.TaskBalance) {
		t.Fatal("后台任务结束后单飞 gate 未释放")
	}
}
