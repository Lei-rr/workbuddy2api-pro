// perf_optim_test.go v2.5.0 性能专项 / Perf-3：面板热路径优化的契约测试。
//
// 覆盖：
//   - accounts ETag 缓存：304 快速路径命中/响应头、池版本变化失效、meta 代际变化
//     失效（tag 过滤）、查询隔离、LRU 容量与 TTL、并发安全；
//   - overview：?accounts=0 轻量模式（无 accounts、默认/ lite 契约不变）、lite 汇总
//     与 pool.Summary() 口径一致；
//   - metrics：全部指标来自 Summary（不再全池 List）；
//   - gzip：缓冲池复用时多请求解压内容正确；
//   - 重构等价性：pageSliceIndexes 对齐旧 pageAccounts、sortAccountIndexes 对齐旧
//     sortAccounts（排序口径零漂移，ETag 才能稳定）。
package panel

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
)

// requestWithINM 带 If-None-Match 请求账号列表。
func requestWithINM(t *testing.T, p *Panel, query, etag string) int {
	t.Helper()
	path := "/panel/api/accounts"
	if query != "" {
		path += "?" + query
	}
	return panelRequest(p, http.MethodGet, path, accountsTestKey, "", map[string]string{
		"If-None-Match": etag,
	}).Code
}

// TestAccountsETagCacheFastPath 同查询版本未变：首次 200 建缓存，第二次带
// If-None-Match → 304 空 body（走缓存，不构造 rows），头保留。
func TestAccountsETagCacheFastPath(t *testing.T) {
	p, pl := newAccountsTestPanel()
	addTestAccounts(pl, 40)

	rec, resp := getAccounts(t, p, "page=1&page_size=10")
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("首次 accounts 响应缺少 ETag")
	}
	if resp.Total != 40 || len(resp.Rows) != 10 {
		t.Fatalf("首次响应 total=%d rows=%d", resp.Total, len(resp.Rows))
	}

	rec304 := panelRequest(p, http.MethodGet, "/panel/api/accounts?page=1&page_size=10", accountsTestKey, "", map[string]string{
		"If-None-Match":   etag,
		"Accept-Encoding": "gzip",
	})
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("缓存命中 status=%d want 304 body=%s", rec304.Code, rec304.Body.String())
	}
	if rec304.Body.Len() != 0 {
		t.Fatalf("304 body=%q want 空", rec304.Body.String())
	}
	if got := rec304.Header().Get("ETag"); got != etag {
		t.Fatalf("304 ETag=%q want %q", got, etag)
	}
	if got := rec304.Header().Get("Cache-Control"); got != accountsCacheControl {
		t.Fatalf("304 Cache-Control=%q", got)
	}

	// 未命中 304 但版本未变：200 应复用缓存 ETag（内容一致），且 now 为实时值。
	time.Sleep(5 * time.Millisecond)
	rec200, _ := getAccounts(t, p, "page=1&page_size=10")
	if got := rec200.Header().Get("ETag"); got != etag {
		t.Fatalf("200 复用 ETag 不一致：%q vs %q", got, etag)
	}

	// 缓存里确实有条目（键存在且版本一致可命中）。
	if _, ok := p.accountsETag.lookup(cacheKeyForTest(p, "page=1&page_size=10"), p.poolVersion(), accountMetaVersionNow()); !ok {
		t.Fatal("首次 200 后 ETag 缓存应有可命中条目")
	}
}

// cacheKeyForTest 解析查询串并返回归一化缓存键（测试辅助）。
func cacheKeyForTest(p *Panel, query string) string {
	req := benchRequest("/panel/api/accounts?"+query, "")
	_ = p
	return parseAccountsQuery(req).cacheKey()
}

// TestAccountsETagCachePoolVersionInvalidation 池版本变化（积分修改）→ 旧 ETag
// 不再 304，ETag 变化且内容更新；新 ETag 继续 304。
func TestAccountsETagCachePoolVersionInvalidation(t *testing.T) {
	p, pl := newAccountsTestPanel()
	addTestAccounts(pl, 10)

	rec, _ := getAccounts(t, p, "page=1&page_size=5")
	etag1 := rec.Header().Get("ETag")

	pl.SetCredits("uid-000", 99999, 99999) // 递增 pool.Version()
	oldINM := panelRequest(p, http.MethodGet, "/panel/api/accounts?page=1&page_size=5", accountsTestKey, "", map[string]string{
		"If-None-Match": etag1,
	})
	if oldINM.Code != http.StatusOK {
		t.Fatalf("池版本变化后旧 ETag status=%d want 200", oldINM.Code)
	}
	etag2 := oldINM.Header().Get("ETag")
	if etag2 == etag1 {
		t.Fatal("池内容变化后 ETag 未变（缓存失效失败）")
	}
	var resp accountsResp
	if err := json.Unmarshal(oldINM.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Rows) == 0 || resp.Rows[0].UID != "uid-000" || resp.Rows[0].Credits != 99999 {
		t.Fatalf("失效重算后内容不符：%+v", resp.Rows[:minInt(1, len(resp.Rows))])
	}
	if code := requestWithINM(t, p, "page=1&page_size=5", etag2); code != http.StatusNotModified {
		t.Fatalf("新 ETag 应命中 304，got %d", code)
	}
}

// TestAccountsETagCacheMetaVersionInvalidation tag 过滤依赖 meta：备注/标签写操作
// 必须让缓存失效（accountmeta 代际号自增），结果与 ETag 同步更新。
func TestAccountsETagCacheMetaVersionInvalidation(t *testing.T) {
	p, _ := newAccountMetaPanel(t)
	addTestAccounts(p.cfg.Pool, 10)

	// 初始无 vip 标签：tag=vip 为空结果。
	rec, resp := getAccounts(t, p, "tag=vip")
	if resp.Filtered != 0 {
		t.Fatalf("初始 tag=vip filtered=%d want 0", resp.Filtered)
	}
	etag1 := rec.Header().Get("ETag")

	// 直接调用 meta handler 写标签（模拟写路径；store 落盘成功后自增代际号）。
	metaPut(t, p, "uid-000", "", "vip")

	rec2 := panelRequest(p, http.MethodGet, "/panel/api/accounts?tag=vip", accountsTestKey, "", map[string]string{
		"If-None-Match": etag1,
	})
	if rec2.Code != http.StatusOK {
		t.Fatalf("meta 变化后旧 ETag status=%d want 200", rec2.Code)
	}
	etag2 := rec2.Header().Get("ETag")
	if etag2 == etag1 {
		t.Fatal("meta 变化（tag 过滤结果变化）后 ETag 未变")
	}
	var resp2 accountsResp
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp2.Filtered != 1 || len(resp2.Rows) != 1 || resp2.Rows[0].UID != "uid-000" {
		t.Fatalf("tag 过滤结果=%+v", resp2)
	}
	if code := requestWithINM(t, p, "tag=vip", etag2); code != http.StatusNotModified {
		t.Fatalf("meta 变化后新 ETag 应 304，got %d", code)
	}

	// 仅改备注不改标签：缓存失效但重算 ETag 不变（内容一致）→ 旧 ETag 仍 304。
	metaPut(t, p, "uid-001", "只是一条备注")
	if code := requestWithINM(t, p, "tag=vip", etag2); code != http.StatusNotModified {
		t.Fatalf("note-only 变化后内容未变，应 304，got %d", code)
	}
}

// TestAccountsETagCacheQueryIsolation 不同查询（分页/排序/投影）独立缓存条目。
func TestAccountsETagCacheQueryIsolation(t *testing.T) {
	p, pl := newAccountsTestPanel()
	addTestAccounts(pl, 20)

	queries := []string{
		"page=1&page_size=5",
		"page=2&page_size=5",
		"page=1&page_size=5&sort=credits",
		"page=1&page_size=5&fields=full",
	}
	etags := make(map[string]string, len(queries))
	for _, q := range queries {
		rec, _ := getAccounts(t, p, q)
		etags[q] = rec.Header().Get("ETag")
	}
	for i, qa := range queries {
		for j, qb := range queries {
			if i < j && etags[qa] == etags[qb] {
				t.Fatalf("不同查询 ETag 相同：%q vs %q（%s）", qa, qb, etags[qa])
			}
		}
	}
	for _, q := range queries {
		if code := requestWithINM(t, p, q, etags[q]); code != http.StatusNotModified {
			t.Fatalf("查询 %q 的新 ETag 应 304，got %d", q, code)
		}
	}
}

// TestAccountsETagCacheLRU 容量上限与提升/淘汰语义（直接测缓存结构，不依赖池）。
func TestAccountsETagCacheLRU(t *testing.T) {
	c := newAccountsETagCache()
	total := accountsETagCacheCap + 10
	for i := 0; i < total; i++ {
		c.store(fmt.Sprintf("k%02d", i), accountsETagCacheEntry{etag: fmt.Sprintf("e%02d", i)})
	}
	if len(c.m) != accountsETagCacheCap {
		t.Fatalf("LRU 容量=%d want %d", len(c.m), accountsETagCacheCap)
	}
	if _, ok := c.lookup("k00", 0, 0); ok {
		t.Fatal("最旧条目 k00 应已被淘汰")
	}
	lastKey := fmt.Sprintf("k%02d", total-1)
	if e, ok := c.lookup(lastKey, 0, 0); !ok || e.etag != fmt.Sprintf("e%02d", total-1) {
		t.Fatalf("最新条目应命中：ok=%v e=%+v", ok, e)
	}
	// 版本不一致 → 失效。
	if _, ok := c.lookup(lastKey, 1, 0); ok {
		t.Fatal("池版本变化后条目应失效")
	}
	if _, ok := c.lookup(lastKey, 0, 0); ok {
		t.Fatal("失效条目应被移除，不应再命中")
	}
}

// TestAccountsETagCacheTTL TTL 到期后条目不可命中（兜底纯时间流逝的状态翻转）。
func TestAccountsETagCacheTTL(t *testing.T) {
	c := newAccountsETagCache()
	c.store("k", accountsETagCacheEntry{etag: "e"})
	if _, ok := c.lookup("k", 0, 0); !ok {
		t.Fatal("新写入条目应命中")
	}
	// 手工把 builtAt 拨老（同包测试可直接操作内部结构）。
	c.mu.Lock()
	c.m["k"].Value.(*accountsETagCacheItem).entry.builtAt = time.Now().Add(-accountsETagCacheTTL - time.Second)
	c.mu.Unlock()
	if _, ok := c.lookup("k", 0, 0); ok {
		t.Fatal("TTL 到期条目应失效")
	}
}

// TestAccountsETagCacheConcurrent 并发命中同一查询：只能得到合法 200/304，不 panic。
func TestAccountsETagCacheConcurrent(t *testing.T) {
	p, pl := newAccountsTestPanel()
	addTestAccounts(pl, 60)
	rec, _ := getAccounts(t, p, "page=1&page_size=20")
	etag := rec.Header().Get("ETag")

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				code := requestWithINM(t, p, "page=1&page_size=20", etag)
				if code != http.StatusOK && code != http.StatusNotModified {
					t.Errorf("并发请求 status=%d want 200/304", code)
				}
			}
		}()
	}
	wg.Wait()
}

// TestOverviewAccountsDisabled ?accounts=0（轻量模式）：响应无 accounts 字段、
// 体积显著变小、默认 overview 仍包含 accounts、lite 不受影响。
func TestOverviewAccountsDisabled(t *testing.T) {
	p, pl := newAccountsTestPanel()
	addTestAccounts(pl, 12)

	full := panelRequest(p, http.MethodGet, "/panel/api/overview", accountsTestKey, "", nil)
	if full.Code != http.StatusOK {
		t.Fatalf("默认 overview status=%d", full.Code)
	}
	var fullMap map[string]json.RawMessage
	if err := json.Unmarshal(full.Body.Bytes(), &fullMap); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	var fullAccounts []json.RawMessage
	if err := json.Unmarshal(fullMap["accounts"], &fullAccounts); err != nil || len(fullAccounts) != 12 {
		t.Fatalf("默认 overview accounts=%d err=%v want 12", len(fullAccounts), err)
	}

	for _, param := range []string{"accounts=0", "accounts=false"} {
		rec := panelRequest(p, http.MethodGet, "/panel/api/overview?"+param, accountsTestKey, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", param, rec.Code, rec.Body.String())
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("decode %s: %v", param, err)
		}
		if _, ok := m["accounts"]; ok {
			t.Errorf("%s：轻量 overview 不应包含 accounts", param)
		}
		var total int
		if err := json.Unmarshal(m["total"], &total); err != nil || total != 12 {
			t.Errorf("%s：total=%d err=%v want 12", param, total, err)
		}
		for _, key := range []string{"credits_sum", "in_flight_full", "model_locks", "poll_interval_sec"} {
			if _, ok := m[key]; !ok {
				t.Errorf("%s：轻量 overview 缺字段 %q", param, key)
			}
		}
		if rec.Body.Len()*4 >= full.Body.Len() {
			t.Errorf("%s：轻量 body=%d B 未明显小于全量 %d B", param, rec.Body.Len(), full.Body.Len())
		}
	}

	// lite=1 行为不变（仍是轻量）；lite=0 回落全量（含 accounts）。
	lite := panelRequest(p, http.MethodGet, "/panel/api/overview?lite=1", accountsTestKey, "", nil)
	if stringsContains(lite.Body.String(), `"accounts":`) {
		t.Error("lite=1 不应包含 accounts")
	}
	lite0 := panelRequest(p, http.MethodGet, "/panel/api/overview?lite=0", accountsTestKey, "", nil)
	if !stringsContains(lite0.Body.String(), `"accounts":[`) {
		t.Error("lite=0 应回落全量（含 accounts）")
	}
}

// TestOverviewLiteUsesPoolSummary lite 的计数/积分/在途与 pool.Summary() 一致
// （v2.5.0：overview 不再做 CountsDetailedWithPaused/CreditsSums 多次扫描）。
func TestOverviewLiteUsesPoolSummary(t *testing.T) {
	p, pl := newAccountsTestPanel()
	addTestAccounts(pl, 10)
	pl.SetCredits("uid-001", 123, 456)
	pl.Disable("uid-002", "test disable")
	fresh := pl.Summary()

	rec := panelRequest(p, http.MethodGet, "/panel/api/overview?lite=1", accountsTestKey, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("lite status=%d", rec.Code)
	}
	var got struct {
		Total        int   `json:"total"`
		Healthy      int   `json:"healthy"`
		Cooling      int   `json:"cooling"`
		Disabled     int   `json:"disabled"`
		Paused       int   `json:"paused"`
		InFlightFull int   `json:"in_flight_full"`
		CreditsSum   int64 `json:"credits_sum"`
		CreditsTotal int64 `json:"credits_total_sum"`
		ModelLocks   *int  `json:"model_locks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// model_locks 无锁时应为 null（Summary.ModelLocks=0 直接短路，不扫全池）。
	if fresh.ModelLocks == 0 {
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(rec.Body.Bytes(), &raw)
		if string(raw["model_locks"]) != "null" {
			t.Fatalf("无模型锁时 model_locks=%s want null", raw["model_locks"])
		}
	}
	if got.Total != fresh.Total || got.Healthy != fresh.Healthy || got.Cooling != fresh.Cooling ||
		got.Disabled != fresh.Disabled || got.Paused != fresh.Paused ||
		got.InFlightFull != fresh.InFlight || got.CreditsSum != fresh.CreditsSum ||
		got.CreditsTotal != fresh.CreditsTotalSum {
		t.Fatalf("lite 摘要与 Summary 不一致\n got=%+v\n want=%+v", got, fresh)
	}
}

// TestMetricsUsesPoolSummary /metrics 全量指标来自 Summary（不再 List 累加 in-flight）。
func TestMetricsUsesPoolSummary(t *testing.T) {
	p, pl := newAccountsTestPanel()
	addTestAccounts(pl, 8)
	pl.SetCredits("uid-000", 1000, 2000)
	fresh := pl.Summary()

	rec := panelRequest(p, http.MethodGet, "/panel/api/metrics", accountsTestKey, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status=%d", rec.Code)
	}
	text := rec.Body.String()
	checks := []string{
		fmt.Sprintf("workbuddy_accounts_total{state=\"healthy\"} %d", fresh.Healthy),
		fmt.Sprintf("workbuddy_accounts_total{state=\"disabled\"} %d", fresh.Disabled),
		fmt.Sprintf("workbuddy_in_flight %d", fresh.InFlight),
		fmt.Sprintf("workbuddy_credits_sum %d", fresh.CreditsSum),
		fmt.Sprintf("workbuddy_credits_total_sum %d", fresh.CreditsTotalSum),
		fmt.Sprintf("workbuddy_model_locks %d", fresh.ModelLocks),
	}
	for _, want := range checks {
		if !stringsContains(text, want) {
			t.Errorf("metrics 缺行 %q\n%s", want, text)
		}
	}
}

// TestGzipBufferPoolRoundTrip gzip 缓冲池复用下多请求内容仍正确（顺序复用不串数据）。
func TestGzipBufferPoolRoundTrip(t *testing.T) {
	p, pl := newAccountsTestPanel()
	addTestAccounts(pl, 100)
	for i := 0; i < 3; i++ {
		rec := panelRequest(p, http.MethodGet, "/panel/api/accounts?page=1&page_size=50", accountsTestKey, "", map[string]string{
			"Accept-Encoding": "gzip",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status=%d", i, rec.Code)
		}
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("第 %d 次 Content-Encoding=%q want gzip（body=%d B）", i, got, rec.Body.Len())
		}
		zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("第 %d 次 gzip.NewReader: %v", i, err)
		}
		raw, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("第 %d 次 gzip 解压: %v", i, err)
		}
		_ = zr.Close()
		var resp accountsResp
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("第 %d 次解压后 JSON: %v", i, err)
		}
		if resp.Total != 100 || len(resp.Rows) != 50 {
			t.Fatalf("第 %d 次解压后 total=%d rows=%d", i, resp.Total, len(resp.Rows))
		}
	}
}

// TestPageSliceIndexesEquivalent 重构后的 pageSliceIndexes 与旧 pageAccounts 语义一致。
func TestPageSliceIndexesEquivalent(t *testing.T) {
	for _, n := range []int{0, 1, 5, 37, 100, 2000, 2001, 10000} {
		rows := make([]pool.Status, n)
		for _, pageSize := range []int{0, 1, 10, 100, 2000, 5000} {
			for _, page := range []int{1, 2, 3, 50, 999, 0, -1} {
				wantRows, wantPage, wantPages := pageAccounts(rows, page, pageSize)
				start, end, effPage, pages := pageSliceIndexes(n, page, pageSize)
				if len(wantRows) != end-start || wantPage != effPage || wantPages != pages {
					t.Fatalf("n=%d page=%d pageSize=%d: old(len=%d,page=%d,pages=%d) new(start=%d,end=%d,page=%d,pages=%d)",
						n, page, pageSize, len(wantRows), wantPage, wantPages, start, end, effPage, pages)
				}
			}
		}
	}
}

// TestSortAccountIndexesEquivalent 重构后的索引排序与旧 sortAccounts 排序口径一致
// （credits/last_success/cool_remaining/uid 四档，含同值 uid 兜底与零值时间）。
func TestSortAccountIndexesEquivalent(t *testing.T) {
	now := time.Now()
	rows := []pool.Status{
		{UID: "u1", Credits: 10, LastSuccessTime: now.Add(-time.Hour), CoolRemaining: 30},
		{UID: "u2", Credits: 30, LastSuccessTime: time.Time{}, CoolRemaining: 0},
		{UID: "u3", Credits: 10, LastSuccessTime: now, CoolRemaining: 30},
		{UID: "u0", Credits: 10, LastSuccessTime: time.Time{}, CoolRemaining: 5},
		{UID: "u4", Credits: 30, LastSuccessTime: now.Add(-2 * time.Hour), CoolRemaining: 0},
	}
	for _, sortBy := range []string{"uid", "credits", "last_success", "cool_remaining"} {
		want := append([]pool.Status(nil), rows...)
		sortAccounts(want, sortBy)
		idxs := make([]int, len(rows))
		for i := range idxs {
			idxs[i] = i
		}
		sortAccountIndexes(idxs, rows, sortBy)
		for i, idx := range idxs {
			if rows[idx].UID != want[i].UID {
				t.Fatalf("sort=%s 第 %d 位 UID=%s want %s", sortBy, i, rows[idx].UID, want[i].UID)
			}
		}
	}
}

// minInt 小工具（Go 1.22 无内置 min for int? 有内置 min，但保底手写避免版本差异）。
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// stringsContains 薄封装（避免与现有测试 helper 命名冲突）。
func stringsContains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }

// ---------------------------------------------------------------------------
// v2.5.0 Perf-3 目标数字基准（10k 账号；与 perf_bench_test.go 同口径）
// ---------------------------------------------------------------------------

// BenchmarkPerfOptimOverviewAccountsOff 全量 overview 的 ?accounts=0 轻量模式
// （目标 <2ms：Summary O(1) + 不序列化 accounts）。
func BenchmarkPerfOptimOverviewAccountsOff(b *testing.B) {
	p := newBenchPanel(b, 10000)
	benchPanelTarget(b, p, "/panel/api/overview?accounts=0", false)
}

// BenchmarkPerfOptimSSESnapshot SSE snapshot 构建（目标 <1ms：Summary 替代全池扫）。
func BenchmarkPerfOptimSSESnapshot(b *testing.B) {
	p := newBenchPanel(b, 10000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := p.buildSSESnapshot()
		if s.Counts.Total != 10000 {
			b.Fatalf("snapshot total=%d", s.Counts.Total)
		}
	}
}

// BenchmarkPerfOptimAccountsETagCache304 缓存命中的 304 快速路径（目标 <1ms）。
func BenchmarkPerfOptimAccountsETagCache304(b *testing.B) {
	p := newBenchPanel(b, 10000)
	benchPanelTarget(b, p, "/panel/api/accounts?page=1&page_size=100&fields=light", true)
}
