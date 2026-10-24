// accounts_page_test.go — v2.1 #1 服务端分页 / 字段裁剪 / ETag 的契约测试。
//
// 覆盖（对应任务 #1 的验收项）：
//   - 分页边界：空池、默认首页、末页、超界页、page/page_size 非法值回落、
//     page_size=0「全部」的服务端 2000 行上限、极大 page 的溢出防护；
//   - 筛选：q（uid/昵称包含、忽略大小写）、state（healthy/cooling/paused/disabled，
//     cooling 含熔断/连败降权）、realm、enterprise 及其组合；
//   - 排序：uid（默认升序）/ credits（降序）/ last_success（降序、零值最后）/
//     cool_remaining（升序）；
//   - 字段投影：light 省略 credits_expiry_buckets 与 cool_remaining_sec，其余字段
//     （token_usage/model_costs/rate_limited_models）保留；full 完整；
//   - ETag：同数据同 ETag、易变字段（倒计时/在途/熔断失败/最近使用时刻）不抖动、
//     业务数据变化必变、查询参数不同 ETag 不同、If-None-Match 命中 → 304 空 body
//     且保留 ETag/Cache-Control/Vary；
//   - overview?lite=1：不含 accounts、元信息/计数保留；不带 lite 时旧语义不变；
//   - 鉴权：两个新接口都走 withAuth，无密钥 401。
package panel

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
)

const accountsTestKey = "accounts-test-key"

// newAccountsTestPanel 构造带鉴权的面板与空池。
func newAccountsTestPanel() (*Panel, *pool.Pool) {
	pl := pool.New("")
	return New(Config{Version: "test", APIKey: accountsTestKey, Pool: pl}), pl
}

// addTestAccounts 加入 n 个 uid-<i:03d> / nick-<i:03d> 账号。
func addTestAccounts(pl *pool.Pool, n int) {
	for i := 0; i < n; i++ {
		pl.Add(&auth.Auth{UID: fmt.Sprintf("uid-%03d", i), Nickname: fmt.Sprintf("nick-%03d", i)})
	}
}

// accountsResp 是 /panel/api/accounts 的响应解码结构。
type accountsResp struct {
	OK       bool          `json:"ok"`
	Now      int64         `json:"now"`
	Total    int           `json:"total"`
	Filtered int           `json:"filtered"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
	Pages    int           `json:"pages"`
	Rows     []pool.Status `json:"rows"`
}

// getAccounts 带有效密钥请求 /panel/api/accounts 并解码响应（断言 200）。
func getAccounts(t *testing.T, p *Panel, query string) (*httptest.ResponseRecorder, accountsResp) {
	t.Helper()
	path := "/panel/api/accounts"
	if query != "" {
		path += "?" + query
	}
	rec := panelRequest(p, http.MethodGet, path, accountsTestKey, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, rec.Code, rec.Body.String())
	}
	var resp accountsResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v body=%s", path, err, rec.Body.String())
	}
	return rec, resp
}

// accountUIDs 提取行 uid（保持响应顺序）。
func accountUIDs(rows []pool.Status) []string {
	out := make([]string, len(rows))
	for i, st := range rows {
		out[i] = st.UID
	}
	return out
}

// assertUIDs 断言响应行 uid 序列。
func assertUIDs(t *testing.T, got []pool.Status, want ...string) {
	t.Helper()
	if ids := accountUIDs(got); !slices.Equal(ids, want) {
		t.Fatalf("rows uid=%v want %v", ids, want)
	}
}

// ---------------------------------------------------------------------------
// 分页
// ---------------------------------------------------------------------------

func TestAccountsPagePaginationBounds(t *testing.T) {
	p, pl := newAccountsTestPanel()

	// 空池：rows 必须是 []（不是 null），pages=0，now 是当前 unix 秒。
	rec, resp := getAccounts(t, p, "")
	if !resp.OK || resp.Total != 0 || resp.Filtered != 0 || resp.Page != 1 || resp.PageSize != 100 || resp.Pages != 0 {
		t.Fatalf("空池响应=%+v", resp)
	}
	if len(resp.Rows) != 0 {
		t.Fatalf("空池 rows=%d want 0", len(resp.Rows))
	}
	if !strings.Contains(rec.Body.String(), `"rows":[]`) {
		t.Fatalf("空池 rows 必须序列化为 []，body=%s", rec.Body.String())
	}
	now := time.Now().Unix()
	if resp.Now < now-5 || resp.Now > now+5 {
		t.Fatalf("now=%d 不是当前 unix 秒（期望约 %d）", resp.Now, now)
	}
	// 空池 + 越界 page：页码规范化为 1，rows 仍为 []。
	_, emptyBeyond := getAccounts(t, p, "page=9&page_size=10")
	if emptyBeyond.Page != 1 || len(emptyBeyond.Rows) != 0 || emptyBeyond.Pages != 0 {
		t.Fatalf("空池越界 page：page=%d rows=%d pages=%d，want 1/0/0",
			emptyBeyond.Page, len(emptyBeyond.Rows), emptyBeyond.Pages)
	}

	// 250 账号：默认页大小 100 → 3 页。
	addTestAccounts(pl, 250)
	tests := []struct {
		name         string
		query        string
		wantPage     int
		wantPageSize int
		wantPages    int
		wantRows     int
		wantFirst    string
		wantLast     string
	}{
		{"默认首页", "", 1, 100, 3, 100, "uid-000", "uid-099"},
		{"末页", "page=3", 3, 100, 3, 50, "uid-200", "uid-249"},
		{"超界页钳到末页", "page=4", 3, 100, 3, 50, "uid-200", "uid-249"},
		{"page=0 回落 1", "page=0", 1, 100, 3, 100, "uid-000", "uid-099"},
		{"page 非数字回落 1", "page=abc", 1, 100, 3, 100, "uid-000", "uid-099"},
		{"page_size=0 全部", "page_size=0", 1, 0, 1, 250, "uid-000", "uid-249"},
		{"page_size=0 页参数规范化 1", "page=5&page_size=0", 1, 0, 1, 250, "uid-000", "uid-249"},
		{"page_size 超上限截断", "page_size=5000", 1, 2000, 1, 250, "uid-000", "uid-249"},
		{"page_size 负数回落默认", "page_size=-3", 1, 100, 3, 100, "uid-000", "uid-099"},
		{"page_size=1 取末行", "page_size=1&page=250", 250, 1, 250, 1, "uid-249", "uid-249"},
		{"page_size=1 超界钳末页", "page_size=1&page=251", 250, 1, 250, 1, "uid-249", "uid-249"},
		{"极大 page 防溢出", "page_size=1&page=" + strconv.Itoa(math.MaxInt64), 250, 1, 250, 1, "uid-249", "uid-249"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, resp := getAccounts(t, p, tc.query)
			if resp.Page != tc.wantPage || resp.PageSize != tc.wantPageSize ||
				resp.Pages != tc.wantPages || len(resp.Rows) != tc.wantRows {
				t.Fatalf("page=%d page_size=%d pages=%d rows=%d，want page=%d page_size=%d pages=%d rows=%d",
					resp.Page, resp.PageSize, resp.Pages, len(resp.Rows),
					tc.wantPage, tc.wantPageSize, tc.wantPages, tc.wantRows)
			}
			if resp.Total != 250 || resp.Filtered != 250 {
				t.Fatalf("total=%d filtered=%d want 250/250", resp.Total, resp.Filtered)
			}
			if tc.wantRows == 0 {
				if !strings.Contains(rec.Body.String(), `"rows":[]`) {
					t.Fatalf("空页 rows 必须序列化为 []，body=%s", rec.Body.String())
				}
				return
			}
			if got := resp.Rows[0].UID; got != tc.wantFirst {
				t.Fatalf("首行 uid=%q want %q", got, tc.wantFirst)
			}
			if got := resp.Rows[len(resp.Rows)-1].UID; got != tc.wantLast {
				t.Fatalf("末行 uid=%q want %q", got, tc.wantLast)
			}
		})
	}
}

// page_size=0「全部」模式下服务端仍受 accountsMaxPageSize=2000 上限约束。
func TestAccountsPageAllModeCappedAt2000(t *testing.T) {
	pl := pool.New("")
	addTestAccounts(pl, 2005)
	p := New(Config{Version: "test", APIKey: accountsTestKey, Pool: pl})

	_, resp := getAccounts(t, p, "page_size=0")
	if resp.Total != 2005 || resp.Filtered != 2005 {
		t.Fatalf("total=%d filtered=%d want 2005", resp.Total, resp.Filtered)
	}
	if resp.PageSize != 0 || len(resp.Rows) != 2000 || resp.Pages != 1 {
		t.Fatalf("page_size=0 全部模式：page_size=%d rows=%d pages=%d，want 0/2000/1",
			resp.PageSize, len(resp.Rows), resp.Pages)
	}
	// 截断保留 uid 升序（pool.List() 已按 uid 排序）：全部模式 = 完整排序快照的前 2000 行。
	allUIDs := make([]string, 0, 2005)
	for _, st := range pl.List() {
		allUIDs = append(allUIDs, st.UID)
	}
	if got := accountUIDs(resp.Rows); !slices.Equal(got, allUIDs[:2000]) {
		t.Fatalf("page_size=0 截断后行序与排序快照不一致：首行=%q 末行=%q",
			got[0], got[len(got)-1])
	}

	// 单页上限不限制翻页能力：2005 账号 page_size=100 → 21 页，最后一页 5 行。
	_, last := getAccounts(t, p, "page=21&page_size=100")
	if last.Pages != 21 || len(last.Rows) != 5 {
		t.Fatalf("末页 pages=%d rows=%d want 21/5", last.Pages, len(last.Rows))
	}
	if got := accountUIDs(last.Rows); !slices.Equal(got, allUIDs[2000:]) {
		t.Fatalf("末页行=%v want %v", got, allUIDs[2000:])
	}
}

// ---------------------------------------------------------------------------
// 筛选
// ---------------------------------------------------------------------------

// buildFilterAccounts 构造覆盖所有筛选维度的池：
//
//	aaa: cn / 个人 / healthy（昵称 Alpha）
//	bbb: cn / 个人 / cooling（账号级冷却）
//	ccc: cn / 企业 / healthy
//	ddd: global / 个人 / healthy
//	eee: cn / 个人 / disabled
//	fff: cn / 个人 / paused
//	ggg: cn / 个人 / cooling（连败降权期，无 until）
func buildFilterAccounts(t *testing.T) (*Panel, *pool.Pool) {
	t.Helper()
	p, pl := newAccountsTestPanel()
	pl.Add(&auth.Auth{UID: "aaa", Nickname: "Alpha"})
	pl.Add(&auth.Auth{UID: "bbb", Nickname: "Beta"})
	pl.Add(&auth.Auth{UID: "ccc", Nickname: "Gamma", EnterpriseID: "ent-1"})
	global := &auth.Auth{UID: "ddd", Nickname: "Delta"}
	if _, err := auth.BackfillRealmFor(global, "global"); err != nil {
		t.Fatalf("backfill realm: %v", err)
	}
	pl.Add(global)
	pl.Add(&auth.Auth{UID: "eee", Nickname: "Epsilon"})
	pl.Add(&auth.Auth{UID: "fff", Nickname: "Zeta"})
	pl.Add(&auth.Auth{UID: "ggg", Nickname: "Eta"})

	pl.Cooldown("bbb", pool.CoolSoft, 10*time.Minute, "429 rate limit")
	pl.Disable("eee", "session dead")
	pl.Pause("fff")
	pl.SetDegrade(1, time.Minute, time.Minute) // 阈值 1：单次 NoteFailures 即入降权期
	pl.NoteFailures("ggg")
	return p, pl
}

func TestAccountsFilters(t *testing.T) {
	p, _ := buildFilterAccounts(t)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"不加筛选", "", []string{"aaa", "bbb", "ccc", "ddd", "eee", "fff", "ggg"}},
		{"healthy 排除禁用/暂停/冷却/降权", "state=healthy", []string{"aaa", "ccc", "ddd"}},
		{"cooling 含冷却与降权", "state=cooling", []string{"bbb", "ggg"}},
		{"paused", "state=paused", []string{"fff"}},
		{"disabled", "state=disabled", []string{"eee"}},
		{"未知 state 不筛选", "state=whatever", []string{"aaa", "bbb", "ccc", "ddd", "eee", "fff", "ggg"}},
		{"realm=global", "realm=global", []string{"ddd"}},
		{"realm=cn", "realm=cn", []string{"aaa", "bbb", "ccc", "eee", "fff", "ggg"}},
		{"未知 realm 不筛选", "realm=mars", []string{"aaa", "bbb", "ccc", "ddd", "eee", "fff", "ggg"}},
		{"enterprise=1", "enterprise=1", []string{"ccc"}},
		{"enterprise=0（含 realm 组合）", "enterprise=0&realm=cn", []string{"aaa", "bbb", "eee", "fff", "ggg"}},
		{"未知 enterprise 不筛选", "enterprise=maybe", []string{"aaa", "bbb", "ccc", "ddd", "eee", "fff", "ggg"}},
		{"q 匹配昵称且忽略大小写", "q=ALP", []string{"aaa"}},
		{"q 匹配 uid", "q=dd", []string{"ddd"}},
		{"q 无命中", "q=no-such-account", nil},
		{"q+state 组合", "q=eta&state=cooling", []string{"bbb", "ggg"}},
		{"realm+state 组合", "realm=cn&state=healthy", []string{"aaa", "ccc"}},
		{"enterprise+state 组合", "enterprise=1&state=healthy", []string{"ccc"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, resp := getAccounts(t, p, tc.query)
			want := tc.want
			if want == nil {
				want = []string{}
			}
			if got := accountUIDs(resp.Rows); !slices.Equal(got, want) {
				t.Fatalf("rows=%v want %v", got, want)
			}
			if resp.Total != 7 {
				t.Fatalf("total=%d want 7（筛选不应改变池总数）", resp.Total)
			}
			if resp.Filtered != len(want) {
				t.Fatalf("filtered=%d want %d", resp.Filtered, len(want))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 排序
// ---------------------------------------------------------------------------

func TestAccountsSorting(t *testing.T) {
	p, pl := newAccountsTestPanel()
	// 故意乱序加入，验证按 uid 排序而非插入序。
	pl.Add(&auth.Auth{UID: "uid-c", Nickname: "c"})
	pl.Add(&auth.Auth{UID: "uid-a", Nickname: "a"})
	pl.Add(&auth.Auth{UID: "uid-b", Nickname: "b"})
	pl.SetCredits("uid-a", 500, 500)
	pl.SetCredits("uid-b", 300, 300)
	pl.SetCredits("uid-c", 500, 500)
	// last_success：a 先成功、b 后成功、c 从未成功（零值必须排最后）。
	pl.NoteSuccess("uid-a")
	time.Sleep(2 * time.Millisecond)
	pl.NoteSuccess("uid-b")
	time.Sleep(2 * time.Millisecond)
	// 冷却剩余：a=10m、b=30m、c 未冷却（0，升序时排最前）。
	pl.Cooldown("uid-a", pool.CoolSoft, 10*time.Minute, "429 rate limit")
	pl.Cooldown("uid-b", pool.CoolSoft, 30*time.Minute, "429 rate limit")

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"默认 uid 升序", "", []string{"uid-a", "uid-b", "uid-c"}},
		{"uid 显式升序", "sort=uid", []string{"uid-a", "uid-b", "uid-c"}},
		{"credits 降序（同额按 uid）", "sort=credits", []string{"uid-a", "uid-c", "uid-b"}},
		{"last_success 降序（零值最后）", "sort=last_success", []string{"uid-b", "uid-a", "uid-c"}},
		{"cool_remaining 升序（未冷却 0 最前）", "sort=cool_remaining", []string{"uid-c", "uid-a", "uid-b"}},
		{"未知排序键回落 uid", "sort=bogus", []string{"uid-a", "uid-b", "uid-c"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, resp := getAccounts(t, p, tc.query)
			assertUIDs(t, resp.Rows, tc.want...)
		})
	}
}

// ---------------------------------------------------------------------------
// 字段投影
// ---------------------------------------------------------------------------

func TestAccountsFieldProjection(t *testing.T) {
	p, pl := newAccountsTestPanel()
	pl.Add(&auth.Auth{UID: "u1", Nickname: "One"})
	pl.NoteSuccess("u1") // 让 success_count/last_success 非零出现（omitempty 只在零值时省略）

	now := time.Now()
	at := now.Add(24 * time.Hour)
	pl.SetCreditsSnapshot("u1", pool.CreditsSnapshot{
		Remain: 500, Total: 500, Expiring: 100,
		ExpiryBuckets: []pool.ExpiryBucket{{Date: at.Format("2006-01-02"), ExpireAt: at, Remain: 100}},
	})
	pl.Cooldown("u1", pool.CoolSoft, 10*time.Minute, "429 rate limit")
	pl.NoteModelCost("u1", "cn:glm-5.2", 0.5, 1000)
	pl.RecordTokenUsage("u1", pool.TokenUsageDelta{
		Model:           "cn:glm-5.2",
		HasPromptTokens: true, PromptTokens: 10,
		HasCompletionTokens: true, CompletionTokens: 5,
		HasTotalTokens: true, TotalTokens: 15,
		HasLatencyMs: true, LatencyMs: 120,
	})
	pl.CooldownSoftForModel("u1", time.Minute, now.Add(30*time.Minute), "cn:glm-5.2", "6004 rate limit")

	// light（默认）：两个重/易变字段缺席，其余字段保留。
	lightRec, light := getAccounts(t, p, "")
	var lightRaw struct {
		Rows []map[string]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(lightRec.Body.Bytes(), &lightRaw); err != nil {
		t.Fatalf("decode light raw: %v", err)
	}
	if len(lightRaw.Rows) != 1 {
		t.Fatalf("light rows=%d want 1", len(lightRaw.Rows))
	}
	lightRow := lightRaw.Rows[0]
	for _, forbidden := range []string{"credits_expiry_buckets", "cool_remaining_sec"} {
		if _, ok := lightRow[forbidden]; ok {
			t.Errorf("light 投影不应包含字段 %q", forbidden)
		}
	}
	for _, required := range []string{
		"uid", "nickname", "realm", "credits", "credits_total", "credits_expiring",
		"cooling", "cool_kind", "until", "reason", "success_count", "token_usage",
		"model_costs", "rate_limited_models", "in_flight", "breaker_fails", "consecutive_fails",
	} {
		if _, ok := lightRow[required]; !ok {
			t.Errorf("light 投影缺字段 %q（只允许省略 credits_expiry_buckets / cool_remaining_sec）", required)
		}
	}
	if len(light.Rows) != 1 {
		t.Fatalf("light typed rows=%d", len(light.Rows))
	}
	lr := light.Rows[0]
	if lr.CreditsExpiryBuckets != nil || lr.CoolRemaining != 0 {
		t.Fatalf("light typed 行仍带裁剪字段：buckets=%v cool_remaining=%d", lr.CreditsExpiryBuckets, lr.CoolRemaining)
	}
	if !lr.Cooling || lr.Until.IsZero() || lr.CoolKind == "" {
		t.Fatalf("light 行冷却字段错误：cooling=%v until=%v kind=%q", lr.Cooling, lr.Until, lr.CoolKind)
	}
	if len(lr.ModelCosts) == 0 || len(lr.RateLimitedModels) == 0 || lr.TokenUsage.RequestCount != 1 {
		t.Fatalf("light 投影丢了重字段：model_costs=%d rate_limited=%d token_request=%d",
			len(lr.ModelCosts), len(lr.RateLimitedModels), lr.TokenUsage.RequestCount)
	}

	// full：完整 Status。
	fullRec, full := getAccounts(t, p, "fields=full")
	var fullRaw struct {
		Rows []map[string]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(fullRec.Body.Bytes(), &fullRaw); err != nil {
		t.Fatalf("decode full raw: %v", err)
	}
	for _, required := range []string{"credits_expiry_buckets", "cool_remaining_sec"} {
		if _, ok := fullRaw.Rows[0][required]; !ok {
			t.Errorf("full 投影缺字段 %q", required)
		}
	}
	fr := full.Rows[0]
	if len(fr.CreditsExpiryBuckets) != 1 || fr.CoolRemaining <= 0 {
		t.Fatalf("full 行裁剪字段错误：buckets=%d cool_remaining=%d", len(fr.CreditsExpiryBuckets), fr.CoolRemaining)
	}

	// 未知 fields 值回落 light（默认档）。
	bogusRec, _ := getAccounts(t, p, "fields=everything")
	var bogusRaw struct {
		Rows []map[string]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(bogusRec.Body.Bytes(), &bogusRaw); err != nil {
		t.Fatalf("decode bogus raw: %v", err)
	}
	if _, ok := bogusRaw.Rows[0]["credits_expiry_buckets"]; ok {
		t.Error("未知 fields 值应回落 light 投影")
	}
}

// ---------------------------------------------------------------------------
// ETag / 304
// ---------------------------------------------------------------------------

// TestAccountsETagStableAndChanges 钉住 ETag 契约：
//   - 同数据重复请求 ETag 相同（5s 轮询可 304）；
//   - 秒级倒计时变化不改变 ETag（full 投影里 cool_remaining_sec 会变，但已规范化）；
//   - 业务数据（credits）变化 ETag 必变；
//   - If-None-Match 命中 → 304 空 body，保留 ETag/Cache-Control/Vary；
//   - 查询参数（page/sort/fields）不同 → ETag 不同。
func TestAccountsETagStableAndChanges(t *testing.T) {
	p, pl := newAccountsTestPanel()
	addTestAccounts(pl, 5)
	pl.Cooldown("uid-000", pool.CoolSoft, 10*time.Minute, "429 rate limit")
	pl.SetCredits("uid-001", 777, 777)

	rec1, _ := getAccounts(t, p, "")
	etag1 := rec1.Header().Get("ETag")
	if !strings.HasPrefix(etag1, `W/"acct-`) || !strings.HasSuffix(etag1, `"`) {
		t.Fatalf("ETag=%q 应为弱 ETag W/\"acct-<sha256>\"", etag1)
	}
	hexPart := strings.TrimSuffix(strings.TrimPrefix(etag1, `W/"acct-`), `"`)
	if len(hexPart) != 64 {
		t.Fatalf("ETag sha256 段长度=%d want 64（ETag=%q）", len(hexPart), etag1)
	}
	for _, c := range hexPart {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("ETag sha256 段含非 hex 字符 %q（ETag=%q）", c, etag1)
		}
	}
	if cc := rec1.Header().Get("Cache-Control"); cc != accountsCacheControl {
		t.Fatalf("Cache-Control=%q want %q", cc, accountsCacheControl)
	}

	// 同数据两次 → 同 ETag。
	rec2, _ := getAccounts(t, p, "")
	if got := rec2.Header().Get("ETag"); got != etag1 {
		t.Fatalf("同数据两次 ETag 不同：%s vs %s", etag1, got)
	}

	// full 投影里 cool_remaining_sec 会随秒变化，但 ETag 输入已规范化 → 跨秒仍稳定。
	fullRec1, full1 := getAccounts(t, p, "fields=full")
	etagFull := fullRec1.Header().Get("ETag")
	coolBefore := full1.Rows[0].CoolRemaining
	time.Sleep(1100 * time.Millisecond)
	fullRec2, full2 := getAccounts(t, p, "fields=full")
	coolAfter := full2.Rows[0].CoolRemaining
	if coolBefore == coolAfter {
		t.Fatalf("测试前提不成立：1.1s 后 cool_remaining_sec 未变化（%d）", coolBefore)
	}
	if got := fullRec2.Header().Get("ETag"); got != etagFull {
		t.Fatalf("倒计时变化不应改变 ETag（易变字段应已规范化）：%s vs %s", etagFull, got)
	}
	// light 投影的 ETag 也不因倒计时变化而变。
	rec3, _ := getAccounts(t, p, "")
	if got := rec3.Header().Get("ETag"); got != etag1 {
		t.Fatalf("跨越 1.1s 后 light ETag 变了：%s vs %s", etag1, got)
	}

	// If-None-Match 命中 → 304 空 body，保留 ETag/Cache-Control/Vary（含 gzip 请求头）。
	rec304 := panelRequest(p, http.MethodGet, "/panel/api/accounts", accountsTestKey, "", map[string]string{
		"If-None-Match":   etag1,
		"Accept-Encoding": "gzip",
	})
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match 命中 status=%d want 304 body=%s", rec304.Code, rec304.Body.String())
	}
	if rec304.Body.Len() != 0 {
		t.Fatalf("304 body=%q want 空", rec304.Body.String())
	}
	if got := rec304.Header().Get("ETag"); got != etag1 {
		t.Fatalf("304 必须保留 ETag：%q want %q", got, etag1)
	}
	if got := rec304.Header().Get("Cache-Control"); got != accountsCacheControl {
		t.Fatalf("304 必须保留 Cache-Control：%q", got)
	}
	if got := rec304.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("304 必须保留 Vary: Accept-Encoding：%q", got)
	}
	if got := rec304.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("304 不应有 Content-Encoding：%q", got)
	}

	// 业务数据变化 → ETag 必变；旧 If-None-Match 不再 304。
	pl.SetCredits("uid-001", 778, 778)
	rec4, _ := getAccounts(t, p, "")
	etag2 := rec4.Header().Get("ETag")
	if etag2 == etag1 {
		t.Fatal("credits 变化后 ETag 未变（缓存会一直返回旧数据）")
	}
	recOld := panelRequest(p, http.MethodGet, "/panel/api/accounts", accountsTestKey, "", map[string]string{"If-None-Match": etag1})
	if recOld.Code != http.StatusOK {
		t.Fatalf("旧 ETag 匹配失败后 status=%d want 200", recOld.Code)
	}
	recNew := panelRequest(p, http.MethodGet, "/panel/api/accounts", accountsTestKey, "", map[string]string{"If-None-Match": etag2})
	if recNew.Code != http.StatusNotModified {
		t.Fatalf("新 ETag 应命中 304，got %d", recNew.Code)
	}

	// 查询参数不同 → 不同缓存条目。
	recPage2, _ := getAccounts(t, p, "page=2&page_size=2")
	if got := recPage2.Header().Get("ETag"); got == etag2 {
		t.Error("不同 page/page_size 的 ETag 相同（缓存键缺少查询参数）")
	}
	recSort, _ := getAccounts(t, p, "sort=credits")
	if got := recSort.Header().Get("ETag"); got == etag2 {
		t.Error("不同 sort 的 ETag 相同")
	}
	recFull, _ := getAccounts(t, p, "fields=full")
	if got := recFull.Header().Get("ETag"); got == etag2 {
		t.Error("fields=full 与 light 的 ETag 相同（缓存键缺少投影档位）")
	}
	// 语义相同的写法（默认 light vs 显式 fields=light）应共享 ETag。
	recExplicitLight, _ := getAccounts(t, p, "fields=light")
	if got := recExplicitLight.Header().Get("ETag"); got != etag2 {
		t.Fatalf("显式 fields=light 的 ETag=%q 应与默认 %q 相同（参数归一化）", got, etag2)
	}
}

// TestAccountsETagVolatileNormalization 直接钉住 hash 输入的规范化集合：
// 只有易变字段（cool_remaining_sec/in_flight/breaker_fails/token_usage.last_used_at）
// 变化时 ETag 不变；任一业务字段变化时 ETag 必变。
func TestAccountsETagVolatileNormalization(t *testing.T) {
	base := pool.Status{
		UID:           "uid-1",
		Nickname:      "nick",
		Credits:       42,
		SuccessCount:  3,
		Cooling:       true,
		CoolKind:      "soft_rate",
		CoolRemaining: 3599,
		Until:         time.Now().Add(time.Hour),
		InFlight:      3,
		BreakerFails:  5,
	}
	base.TokenUsage = pool.TokenUsage{RequestCount: 7, TotalTokens: 100}

	hashFor := func(st pool.Status) string {
		etag, ok := accountsETag(accountsETagInput{
			Query:    accountsQuery{Page: 1, PageSize: 100, Sort: "uid", Fields: accountsFieldsLight},
			Total:    1,
			Filtered: 1,
			Page:     1,
			PageSize: 100,
			Pages:    1,
			Rows:     accountsStableProjection([]pool.Status{st}, accountsFieldsLight),
		})
		if !ok {
			t.Fatal("稳定投影应可 JSON 序列化")
		}
		return etag
	}
	etagBase := hashFor(base)

	volatile := base
	volatile.CoolRemaining = 1
	volatile.InFlight = 99
	volatile.BreakerFails = 0
	volatile.TokenUsage.LastUsedAt = time.Now()
	if got := hashFor(volatile); got != etagBase {
		t.Fatalf("仅易变字段变化时 ETag 应保持不变：%s vs %s", etagBase, got)
	}

	changes := map[string]func(*pool.Status){
		"credits":           func(st *pool.Status) { st.Credits++ },
		"until":             func(st *pool.Status) { st.Until = st.Until.Add(time.Minute) },
		"cooling":           func(st *pool.Status) { st.Cooling = false },
		"success_count":     func(st *pool.Status) { st.SuccessCount++ },
		"token_usage.count": func(st *pool.Status) { st.TokenUsage.RequestCount++ },
		"nickname":          func(st *pool.Status) { st.Nickname = "other" },
	}
	for name, mutate := range changes {
		changed := base
		mutate(&changed)
		if got := hashFor(changed); got == etagBase {
			t.Errorf("%s 变化后 ETag 未变（会掩盖真实数据变化）", name)
		}
	}
}

// TestAccountsETagMarshalFailureSkipsConditional 稳定投影不可序列化（极端脏数据：
// NaN 成本观测）时不得发出 ETag/304——否则恒定 ETag 会让后续轮询永久命中 304，
// 比没有缓存更糟。这里用 NoteModelCost 注入 NaN per-1k 成本构造该形态。
//
// v2.6.0：writeJSON 不再吞序列化错误——此类响应现在回 500 + {"ok":false,...}
// （而不是 200 + 空 body），HTTP 语义上让客户端/监控能直接看到编码失败。
func TestAccountsETagMarshalFailureSkipsConditional(t *testing.T) {
	p, pl := newAccountsTestPanel()
	pl.Add(&auth.Auth{UID: "u1", Nickname: "One"})
	pl.NoteModelCost("u1", "cn:dirty", math.NaN(), 100) // per1k = NaN → json.Marshal 失败

	rec := panelRequest(p, http.MethodGet, "/panel/api/accounts", accountsTestKey, "", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500（不可序列化响应由 writeJSON 统一转 500）", rec.Code)
	}
	if got := rec.Header().Get("ETag"); got != "" {
		t.Fatalf("不可序列化数据不应发 ETag（会变成恒定值导致永久 304），got %q", got)
	}
	var errResp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil || errResp.OK || errResp.Error == "" {
		t.Fatalf("500 响应应为 {ok:false,error}: %s", rec.Body.String())
	}
	// 即使客户端带任意 If-None-Match 也必须是 500（不能凭空 304）。
	rec2 := panelRequest(p, http.MethodGet, "/panel/api/accounts", accountsTestKey, "", map[string]string{
		"If-None-Match": `W/"acct-` + strings.Repeat("0", 64) + `"`,
	})
	if rec2.Code != http.StatusInternalServerError {
		t.Fatalf("无 ETag 时 If-None-Match status=%d want 500", rec2.Code)
	}
}

// ---------------------------------------------------------------------------
// overview?lite=1
// ---------------------------------------------------------------------------

// TestOverviewLite lite 只回汇总（不含 accounts）；不带 lite 时旧全量语义不变，
// 非法/0 值同样回落全量（旧脚本零破坏）。
func TestOverviewLite(t *testing.T) {
	pl := pool.New("")
	addTestAccounts(pl, 30)
	p := New(Config{Version: "test", APIKey: accountsTestKey, Pool: pl, PollIntervalSec: 7})

	// 默认 overview：保留 accounts（旧行为逐字段不变）。
	fullRec := panelRequest(p, http.MethodGet, "/panel/api/overview", accountsTestKey, "", nil)
	if fullRec.Code != http.StatusOK {
		t.Fatalf("默认 overview status=%d", fullRec.Code)
	}
	var full map[string]json.RawMessage
	if err := json.Unmarshal(fullRec.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	fullAccounts, ok := full["accounts"]
	if !ok {
		t.Fatal("默认 overview 必须保留 accounts 字段（旧脚本依赖）")
	}
	var decodedAccounts []json.RawMessage
	if err := json.Unmarshal(fullAccounts, &decodedAccounts); err != nil {
		t.Fatalf("decode accounts: %v", err)
	}
	if len(decodedAccounts) != 30 {
		t.Fatalf("默认 overview accounts=%d want 30", len(decodedAccounts))
	}

	liteKeys := []string{
		"version", "uptime_sec", "auth_required", "poll_interval_sec", "redis_mode",
		"sticky_sessions", "total", "healthy", "cooling", "disabled", "paused",
		"in_flight_full", "model_locks", "balance_refresh_sec", "balance_refreshed_at",
		"credits_sum", "credits_total_sum",
	}
	for _, param := range []string{"lite=1", "lite=true", "lite=TRUE"} {
		liteRec := panelRequest(p, http.MethodGet, "/panel/api/overview?"+param, accountsTestKey, "", nil)
		if liteRec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", param, liteRec.Code, liteRec.Body.String())
		}
		var lite map[string]json.RawMessage
		if err := json.Unmarshal(liteRec.Body.Bytes(), &lite); err != nil {
			t.Fatalf("decode %s: %v", param, err)
		}
		if _, ok := lite["accounts"]; ok {
			t.Errorf("%s：lite 响应不应包含 accounts", param)
		}
		for _, key := range liteKeys {
			if _, ok := lite[key]; !ok {
				t.Errorf("%s：lite 响应缺字段 %q", param, key)
			}
		}
		var total int
		if err := json.Unmarshal(lite["total"], &total); err != nil || total != 30 {
			t.Errorf("%s：lite total=%d err=%v want 30", param, total, err)
		}
		var poll int
		if err := json.Unmarshal(lite["poll_interval_sec"], &poll); err != nil || poll != 7 {
			t.Errorf("%s：lite poll_interval_sec=%d err=%v want 7", param, poll, err)
		}
		if liteRec.Body.Len()*2 >= fullRec.Body.Len() {
			t.Errorf("%s：lite body=%d B 未明显小于全量 %d B", param, liteRec.Body.Len(), fullRec.Body.Len())
		}
	}

	// lite=0 / 非法值 → 回落旧全量语义（含 accounts）。
	for _, param := range []string{"lite=0", "lite=false", "lite=yes"} {
		rec := panelRequest(p, http.MethodGet, "/panel/api/overview?"+param, accountsTestKey, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d", param, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"accounts":[`) {
			t.Errorf("%s：应回落默认全量（含 accounts），body=%s", param, rec.Body.String())
		}
	}
}

// TestOverviewCreditsSums 钉住 overview 的积分合计字段（v2.1 #1 补充）：
//   - credits_sum = 全池 credits 之和（含禁用/暂停号，与旧前端从 accounts 数组求和同口径）；
//   - credits_total_sum = 全池 credits_total 之和，credits_total<=0（0=未知、-1=企业版
//     无限量）按 0 计，不得把总额拉低；
//   - lite 与全量 overview 都带这两个字段（前端改 lite 轮询后统计条仍能更新）；
//   - 全量 overview 的合计与同响应 accounts 数组求和逐值一致（向后兼容口径），空池 0/0。
func TestOverviewCreditsSums(t *testing.T) {
	readSums := func(t *testing.T, p *Panel, path string) (creditsSum, creditsTotalSum int64) {
		t.Helper()
		rec := panelRequest(p, http.MethodGet, path, accountsTestKey, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		for _, key := range []string{"credits_sum", "credits_total_sum"} {
			if _, ok := m[key]; !ok {
				t.Fatalf("%s 缺字段 %q（lite 与全量 overview 都必须透出积分合计）", path, key)
			}
		}
		var sums struct {
			CreditsSum      int64 `json:"credits_sum"`
			CreditsTotalSum int64 `json:"credits_total_sum"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &sums); err != nil {
			t.Fatalf("decode sums %s: %v", path, err)
		}
		return sums.CreditsSum, sums.CreditsTotalSum
	}

	// 空池：0/0（字段存在性由 readSums 断言）。
	empty := New(Config{Version: "test", APIKey: accountsTestKey, Pool: pool.New("")})
	for _, path := range []string{"/panel/api/overview?lite=1", "/panel/api/overview"} {
		if cs, cts := readSums(t, empty, path); cs != 0 || cts != 0 {
			t.Fatalf("空池 %s credits_sum=%d credits_total_sum=%d want 0/0", path, cs, cts)
		}
	}

	// 混合池：a=10/100、b=20/0（未知）、c=30/-1（企业无限量）、d=5/50、e=7/25（禁用）。
	pl := pool.New("")
	for _, c := range []struct {
		uid     string
		credits int64
		total   int64
	}{{"a", 10, 100}, {"b", 20, 0}, {"c", 30, -1}, {"d", 5, 50}, {"e", 7, 25}} {
		pl.Add(&auth.Auth{UID: c.uid})
		pl.SetCredits(c.uid, c.credits, c.total)
	}
	pl.Disable("e", "session dead")
	p := New(Config{Version: "test", APIKey: accountsTestKey, Pool: pl})

	// credits_sum = 10+20+30+5+7 = 72（禁用号也是池内余额，旧前端同样求和）。
	// credits_total_sum = 100+50+25 = 175：b 的 0 与 c 的 -1 按 0 计。
	const wantCreditsSum, wantCreditsTotalSum = 72, 175
	if cs, cts := readSums(t, p, "/panel/api/overview?lite=1"); cs != wantCreditsSum || cts != wantCreditsTotalSum {
		t.Fatalf("lite credits_sum=%d credits_total_sum=%d want %d/%d",
			cs, cts, wantCreditsSum, wantCreditsTotalSum)
	}
	if cs, cts := readSums(t, p, "/panel/api/overview"); cs != wantCreditsSum || cts != wantCreditsTotalSum {
		t.Fatalf("全量 credits_sum=%d credits_total_sum=%d want %d/%d",
			cs, cts, wantCreditsSum, wantCreditsTotalSum)
	}

	// 全量 overview 的合计必须与同响应 accounts 数组按旧口径求和逐值一致。
	rec := panelRequest(p, http.MethodGet, "/panel/api/overview", accountsTestKey, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview status=%d", rec.Code)
	}
	var full struct {
		CreditsSum      int64         `json:"credits_sum"`
		CreditsTotalSum int64         `json:"credits_total_sum"`
		Accounts        []pool.Status `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	var sumCredits, sumTotal int64
	for _, st := range full.Accounts {
		sumCredits += st.Credits
		if st.CreditsTotal > 0 { // 未知 0 / 企业 -1 按 0 计
			sumTotal += st.CreditsTotal
		}
	}
	if full.CreditsSum != sumCredits || full.CreditsTotalSum != sumTotal {
		t.Fatalf("合计与 accounts 求和不一致：credits %d vs %d，total %d vs %d",
			full.CreditsSum, sumCredits, full.CreditsTotalSum, sumTotal)
	}
}

// ---------------------------------------------------------------------------
// 鉴权
// ---------------------------------------------------------------------------

func TestAccountsEndpointsRequireAuth(t *testing.T) {
	p, pl := newAccountsTestPanel()
	addTestAccounts(pl, 3)

	for _, path := range []string{"/panel/api/accounts", "/panel/api/overview?lite=1"} {
		rec := panelRequest(p, http.MethodGet, path, "", "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("无密钥 %s status=%d want 401", path, rec.Code)
		}
		okRec := panelRequest(p, http.MethodGet, path, accountsTestKey, "", nil)
		if okRec.Code != http.StatusOK {
			t.Fatalf("有效密钥 %s status=%d want 200 body=%s", path, okRec.Code, okRec.Body.String())
		}
	}
}
