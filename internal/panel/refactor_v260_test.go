// refactor_v260_test.go v2.6.0 重构批（task-47）的契约测试：
//
//   - P0 账号级有界扇出：并发峰值 ≤ 配置值、ctx 取消早停、结果统计正确；
//   - P1 writeJSON 序列化失败 → 500（不再 200 空 body）；
//   - P1 面板 API 默认 Cache-Control: no-store，accounts 的 ETag/private 语义不被破坏；
//   - P1 审计补覆盖：meta PUT/DELETE 与 jobs run/cancel；
//   - P1 POST body 上限（413）、import 空 auth_dir 拒绝（400，不写 CWD）；
//   - P1 公共 helper（parseLimit/accountForRequest/readJSONBody）行为。
package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// fanoutBackend 假上游：统计并发峰值/请求数，可配置每请求延迟。
type fanoutBackend struct {
	delay time.Duration

	inFlight atomic.Int32
	peak     atomic.Int32
	reqs     atomic.Int32
}

func (b *fanoutBackend) countPeak() {
	cur := b.inFlight.Add(1)
	for {
		old := b.peak.Load()
		if cur <= old || b.peak.CompareAndSwap(old, cur) {
			break
		}
	}
}

func (b *fanoutBackend) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b.countPeak()
		defer b.inFlight.Add(-1)
		b.reqs.Add(1)
		if b.delay > 0 {
			time.Sleep(b.delay)
		}
		// 任务列表端点返回空任务；其它端点（accept/claim）同样回空 data 即可。
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[]}}`))
	}
}

// newFanoutPanel 构造带 N 个 cn 账号的 Panel（Upstream 指向假后端）。
// direct=true 时用 &Panel{}（供直接调用内部方法的测试，不依赖路由）。
func newFanoutPanel(t *testing.T, b *fanoutBackend, n int) *Panel {
	t.Helper()
	srv := httptest.NewServer(b.handler())
	t.Cleanup(srv.Close)
	pl := pool.New("")
	for i := 0; i < n; i++ {
		pl.Add(&auth.Auth{UID: fmt.Sprintf("u%05d", i), AccessToken: "at"})
	}
	cfg := Config{
		Pool: pl, APIKey: "refactor-key", Version: "test",
		Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL},
	}
	return New(cfg)
}

// TestFanOutAccountsBounded 直接钉住 fanOutAccounts 的并发上限与完成度。
func TestFanOutAccountsBounded(t *testing.T) {
	const n, conc = 100, 4
	var inFlight, peak, done atomic.Int32
	fanOutAccounts(context.Background(), n, conc, func(ctx context.Context, i int) {
		cur := inFlight.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(200 * time.Microsecond)
		done.Add(1)
		inFlight.Add(-1)
	})
	if got := done.Load(); got != n {
		t.Fatalf("处理数=%d want %d", got, n)
	}
	if p := peak.Load(); p > conc {
		t.Fatalf("并发峰值=%d 超过上限 %d", p, conc)
	}
	if p := peak.Load(); p < 2 {
		t.Fatalf("并发峰值=%d 过低（未真正并行，测试前提不成立）", p)
	}
}

// TestFanOutAccountsCtxCancel 取消后不再派发新项（早停），已派发项收尾后返回。
func TestFanOutAccountsCtxCancel(t *testing.T) {
	const n = 1000
	ctx, cancel := context.WithCancel(context.Background())
	var started atomic.Int32
	start := time.Now()
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	fanOutAccounts(ctx, n, 4, func(ctx context.Context, i int) {
		started.Add(1)
		time.Sleep(200 * time.Microsecond)
	})
	elapsed := time.Since(start)
	if ctx.Err() == nil {
		t.Fatal("ctx 未被取消（测试前提不成立）")
	}
	if got := started.Load(); got >= n {
		t.Fatalf("取消后仍处理了全部 %d 项（应早停）", got)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("取消后返回耗时 %s（应快速返回）", elapsed)
	}
}

// TestTaskSweepBoundedConcurrency 1000 账号补领清扫：上游并发峰值 ≤ 配置（4），
// 结果统计正确（每账号 2 次列表请求：默认 + mp 口径）。
func TestTaskSweepBoundedConcurrency(t *testing.T) {
	b := &fanoutBackend{delay: 500 * time.Microsecond}
	p := newFanoutPanel(t, b, 1000)
	p.cfg.TaskSweepConcurrency = 4 // 配置生效验证
	start := time.Now()
	st := p.RunTaskSweepOnce()
	elapsed := time.Since(start)

	if st.Accounts != 1000 {
		t.Fatalf("accounts=%d want 1000", st.Accounts)
	}
	if st.Errors != 0 || st.Skipped != 0 || st.Accepted != 0 || st.Claimed != 0 {
		t.Fatalf("空任务列表下统计应为 0：%+v", st)
	}
	if peak := b.peak.Load(); peak > 4 {
		t.Fatalf("上游并发峰值=%d 超过 TaskSweepConcurrency=4", peak)
	}
	if b.reqs.Load() < 2000 {
		t.Fatalf("请求数=%d want >=2000（每账号默认+mp 两次列表）", b.reqs.Load())
	}
	if elapsed > 10*time.Second {
		t.Fatalf("1000 账号有界清扫耗时 %s（异常）", elapsed)
	}
}

// TestTaskSweepCtxCancelEarly ctx 取消后清扫早停：处理账号数 < 全量且快速返回。
func TestTaskSweepCtxCancelEarly(t *testing.T) {
	b := &fanoutBackend{delay: 2 * time.Millisecond}
	p := newFanoutPanel(t, b, 400)
	p.cfg.TaskSweepConcurrency = 4

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	start := time.Now()
	st := p.RunTaskSweepCtx(ctx)
	elapsed := time.Since(start)

	if ctx.Err() == nil {
		t.Fatal("ctx 未被取消（测试前提不成立）")
	}
	if st.Accounts >= 400 {
		t.Fatalf("取消后仍处理全部账号（accounts=%d）", st.Accounts)
	}
	if st.Accounts == 0 {
		t.Fatalf("取消过早：一个账号都未处理（应至少处理部分）")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("取消后清扫耗时 %s（应快速返回）", elapsed)
	}
}

// TestTasksScanAllBounded 扫描端点（HTTP）同样有界：并发峰值 ≤ 默认 8。
func TestTasksScanAllBounded(t *testing.T) {
	b := &fanoutBackend{delay: 300 * time.Microsecond}
	p := newFanoutPanel(t, b, 300)

	req := httptest.NewRequest(http.MethodPost, "/panel/api/tasks/scan_all", nil)
	req.Header.Set("Authorization", "Bearer refactor-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scan_all status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK           bool `json:"ok"`
		PendingCount int  `json:"pending_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.OK || resp.PendingCount != 0 {
		t.Fatalf("scan_all resp=%+v err=%v", resp, err)
	}
	if peak := b.peak.Load(); peak > panelFanOutDefaultConcurrency {
		t.Fatalf("scan_all 上游并发峰值=%d 超过默认 %d", peak, panelFanOutDefaultConcurrency)
	}
}

// TestGrowthQueueScanBoundedAndCancel 队列扫描有界；ctx 取消回滚 running 并给出取消提示。
func TestGrowthQueueScanBoundedAndCancel(t *testing.T) {
	b := &fanoutBackend{delay: time.Millisecond}
	p := newFanoutPanel(t, b, 120)

	// 正常扫描：无待办 → started=false（不发任何自动动作）；峰值 ≤ 8。
	started, total, seq, msg := p.startGrowthQueue(1, true, taskTriggerManual)
	if started || seq != 0 || total != 0 || !strings.Contains(msg, "没有待办") {
		t.Fatalf("无待办时应 started=false: started=%v total=%d seq=%d msg=%q", started, total, seq, msg)
	}
	if peak := b.peak.Load(); peak > panelFanOutDefaultConcurrency {
		t.Fatalf("队列扫描并发峰值=%d 超过默认 %d", peak, panelFanOutDefaultConcurrency)
	}
	if q := p.queue(); q.running {
		t.Fatal("无待办返回后 q.running 应为 false")
	}

	// 取消：ctx 取消后扫描早停并回滚 running。
	b2 := &fanoutBackend{delay: 3 * time.Millisecond}
	p2 := newFanoutPanel(t, b2, 120)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(15*time.Millisecond, cancel)
	started2, _, _, msg2 := p2.startGrowthQueueCtx(ctx, 1, true, taskTriggerManual)
	if started2 || !strings.Contains(msg2, "取消") {
		t.Fatalf("取消后应 started=false + 取消提示: started=%v msg=%q", started2, msg2)
	}
	if q := p2.queue(); q.running {
		t.Fatal("取消后 q.running 必须回滚为 false（否则队列永久卡死）")
	}
}

// TestWriteJSONMarshalFailure writeJSON 序列化失败 → 500 + {ok:false,error}。
func TestWriteJSONMarshalFailure(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]any{"bad": math.Inf(1)})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type=%q", ct)
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.OK || resp.Error == "" {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

// TestAPIResponsesNoStore 面板 API 默认 no-store；accounts 的 ETag/private 覆盖不被破坏。
func TestAPIResponsesNoStore(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "refactor-key", Pool: pool.New("")})

	cases := []struct {
		path string
		want string
	}{
		{"/panel/api/overview?lite=1", "no-store"},
		{"/panel/api/metrics", "no-store"},
		{"/panel/api/config", "no-store"}, // LoadConfig=nil → 501，但缓存头仍应 no-store
		{"/panel/api/accounts", accountsCacheControl},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("Authorization", "Bearer refactor-key")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if got := rec.Header().Get("Cache-Control"); got != tc.want {
			t.Errorf("%s Cache-Control=%q want %q", tc.path, got, tc.want)
		}
	}
}

// TestAuditMetaAndJobActions meta PUT/DELETE 与 jobs run/cancel 必须落审计。
func TestAuditMetaAndJobActions(t *testing.T) {
	p, _ := newAccountMetaPanel(t) // dataDir = t.TempDir()，APIKey = accountsTestKey
	do := func(method, path, body string) *httptest.ResponseRecorder {
		var rd *strings.Reader
		if body != "" {
			rd = strings.NewReader(body)
		} else {
			rd = strings.NewReader("")
		}
		req := httptest.NewRequest(method, path, rd)
		req.Header.Set("Authorization", "Bearer "+accountsTestKey)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}
	if rec := do(http.MethodPut, "/panel/api/accounts/u1/meta", `{"note":"n1","tags":["vip"]}`); rec.Code != http.StatusOK {
		t.Fatalf("meta PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodDelete, "/panel/api/accounts/u1/meta", ""); rec.Code != http.StatusOK {
		t.Fatalf("meta DELETE status=%d body=%s", rec.Code, rec.Body.String())
	}
	// cfg.Jobs=nil：run/cancel 都回 501，但审计应记录「尝试过」（ok=false）。
	if rec := do(http.MethodPost, "/panel/api/jobs/checkin/run", ""); rec.Code != http.StatusNotImplemented {
		t.Fatalf("jobs run status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPost, "/panel/api/jobs/abc/cancel", ""); rec.Code != http.StatusNotImplemented {
		t.Fatalf("jobs cancel status=%d body=%s", rec.Code, rec.Body.String())
	}

	raws, err := p.auditLog.readLast(20)
	if err != nil || len(raws) < 4 {
		t.Fatalf("审计条目=%d err=%v（want >=4）", len(raws), err)
	}
	type got struct {
		action string
		ok     bool
	}
	seen := map[string]got{}
	for _, raw := range raws {
		var e AuditEntry
		if json.Unmarshal(raw, &e) == nil {
			seen[e.Action] = got{action: e.Action, ok: e.OK}
		}
	}
	for _, want := range []struct {
		action string
		ok     bool
	}{
		{auditActionAccountMetaUpdate, true},
		{auditActionAccountMetaDelete, true},
		{auditActionJobRun, false},
		{auditActionJobCancel, false},
	} {
		g, ok := seen[want.action]
		if !ok {
			t.Fatalf("审计缺少 action=%s（已有：%v）", want.action, seen)
		}
		if g.ok != want.ok {
			t.Fatalf("审计 action=%s ok=%v want %v", want.action, g.ok, want.ok)
		}
	}
}

// TestImportRejectsEmptyAuthDir auth_dir 未配置时导入必须 400（不得写进程 CWD）。
func TestImportRejectsEmptyAuthDir(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "refactor-key", Pool: pool.New("")})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/import/cockpit", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer refactor-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "auth_dir") {
		t.Fatalf("错误信息应指出 auth_dir：%s", rec.Body.String())
	}
}

// TestReadJSONBodyLimit 超过 1MB 的 JSON body → 413（不进入上游）。
func TestReadJSONBodyLimit(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "refactor-key", Pool: pool.New("")})
	p.cfg.Pool.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	huge := `{"task_codes":["` + strings.Repeat("x", panelJSONBodyMax+1024) + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/u1/tasks/accept", strings.NewReader(huge))
	req.Header.Set("Authorization", "Bearer refactor-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want 413 body=%s", rec.Code, rec.Body.String())
	}
}

// TestParseLimitHelper parseLimit 契约（默认/上限/容错/trim）。
func TestParseLimitHelper(t *testing.T) {
	cases := []struct {
		query string
		def   int
		max   int
		want  int
	}{
		{"", 20, 500, 20},
		{"?limit=5", 20, 500, 5},
		{"?limit=9999", 20, 500, 500},
		{"?limit=0", 20, 500, 20},
		{"?limit=abc", 20, 500, 20},
		{"?limit=%205%20", 20, 500, 5},
		{"?limit=-3", 20, 500, 20},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/x"+c.query, nil)
		if got := parseLimit(req, c.def, c.max); got != c.want {
			t.Errorf("parseLimit(%q) = %d want %d", c.query, got, c.want)
		}
	}
}

// TestAccountForRequestHelper 账号查找 helper：命中/404/nil-pool 501。
func TestAccountForRequestHelper(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "refactor-key", Pool: pool.New("")})
	p.cfg.Pool.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	req := httptest.NewRequest(http.MethodGet, "/panel/api/accounts/u1/tasks", nil)
	req.SetPathValue("uid", "u1")
	rec := httptest.NewRecorder()
	a, ok := p.accountForRequest(rec, req)
	if !ok || a == nil || a.UID != "u1" {
		t.Fatalf("命中失败：ok=%v a=%+v", ok, a)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/panel/api/accounts/nope/tasks", nil)
	req2.SetPathValue("uid", "nope")
	rec2 := httptest.NewRecorder()
	if _, ok := p.accountForRequest(rec2, req2); ok || rec2.Code != http.StatusNotFound {
		t.Fatalf("未命中应 404：ok=%v code=%d", ok, rec2.Code)
	}

	pNil := New(Config{Version: "test", APIKey: "refactor-key"})
	req3 := httptest.NewRequest(http.MethodGet, "/panel/api/accounts/u1/tasks", nil)
	req3.SetPathValue("uid", "u1")
	rec3 := httptest.NewRecorder()
	if _, ok := pNil.accountForRequest(rec3, req3); ok || rec3.Code != http.StatusNotImplemented {
		t.Fatalf("nil pool 应 501：ok=%v code=%d", ok, rec3.Code)
	}
}

// TestFanOutHelpersDoNotLeak 确保 fanOutAccounts 对 n<=0 / nil fn 安全（边界）。
func TestFanOutHelpersDoNotLeak(t *testing.T) {
	fanOutAccounts(context.Background(), 0, 4, func(context.Context, int) { t.Fatal("不应调用") })
	fanOutAccounts(context.Background(), 5, 0, nil)
	// 并发数 > n 时退化为 n（不 panic、全部执行）。
	var n atomic.Int32
	fanOutAccounts(context.Background(), 3, 99, func(context.Context, int) { n.Add(1) })
	if n.Load() != 3 {
		t.Fatalf("执行数=%d want 3", n.Load())
	}
}

// TestRefactorNoDataRaceOnCounters 有界扇出结果聚合在 -race 下应无竞态（CI 跑 race）。
func TestRefactorNoDataRaceOnCounters(t *testing.T) {
	var mu sync.Mutex
	seen := map[int]bool{}
	fanOutAccounts(context.Background(), 200, 8, func(_ context.Context, i int) {
		mu.Lock()
		seen[i] = true
		mu.Unlock()
	})
	if len(seen) != 200 {
		t.Fatalf("seen=%d want 200", len(seen))
	}
}

// TestTasksRunQueueInvalidJSONBody 脏 body → 400（不再静默忽略）。
func TestTasksRunQueueInvalidJSONBody(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "refactor-key", Pool: pool.New("")})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/tasks/run_queue", bytes.NewReader([]byte("{bad json")))
	req.Header.Set("Authorization", "Bearer refactor-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
}
