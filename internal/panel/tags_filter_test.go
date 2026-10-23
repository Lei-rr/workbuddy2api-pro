// tags_filter_test.go v2.3.1 #12 扩展：accounts?tag= 服务端筛选 + 标签索引端点
// GET /panel/api/meta/tags 的契约测试（存储/CRUD 见 accountmeta_test.go）。
package panel

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
)

// newTagFilterPanel 构造带 4 个账号与标签元数据的面板：
//
//	u1: cn 个人   tags=[采购A, 待补签]
//	u2: cn 个人   tags=[采购a]（大小写变体，同一标签）
//	u3: cn 企业版 tags=[采购A, 其他]
//	u4: global   无标签
func newTagFilterPanel(t *testing.T) *Panel {
	t.Helper()
	p, _ := newAccountMetaPanel(t)
	pl := p.cfg.Pool
	pl.Add(&auth.Auth{UID: "u1", Nickname: "nick-u1"})
	pl.Add(&auth.Auth{UID: "u2", Nickname: "nick-u2"})
	pl.Add(&auth.Auth{UID: "u3", Nickname: "nick-u3", EnterpriseID: "ent-1"})
	global := &auth.Auth{UID: "u4", Nickname: "nick-u4"}
	if _, err := auth.BackfillRealmFor(global, "global"); err != nil {
		t.Fatalf("backfill realm: %v", err)
	}
	pl.Add(global)

	metaPut(t, p, "u1", "", "采购A", "待补签")
	metaPut(t, p, "u2", "", "采购a")
	metaPut(t, p, "u3", "", "采购A", "其他")
	return p
}

// appendTag 拼 tag 查询参数（URL 编码，便于测空格/中文）。
func appendTag(tag string) string {
	return "tag=" + url.QueryEscape(tag)
}

// TestAccountsTagFilter tag 筛选：命中/大小写/TrimSpace/未知 tag/与其它筛选 AND/分页/
// 热更新（同 Panel 实例改 meta 后立即生效）。
func TestAccountsTagFilter(t *testing.T) {
	t.Run("命中与大小写不敏感", func(t *testing.T) {
		p := newTagFilterPanel(t)
		for _, tag := range []string{"采购A", "采购a", "  采购A  "} {
			_, resp := getAccounts(t, p, appendTag(tag))
			if got := accountUIDs(resp.Rows); len(got) != 3 || got[0] != "u1" || got[1] != "u2" || got[2] != "u3" {
				t.Fatalf("tag=%q rows=%v want [u1 u2 u3]", tag, got)
			}
			if resp.Total != 4 || resp.Filtered != 3 {
				t.Fatalf("tag=%q total=%d filtered=%d want 4/3", tag, resp.Total, resp.Filtered)
			}
		}
		// 单匹配标签。
		_, resp := getAccounts(t, p, appendTag("待补签"))
		assertUIDs(t, resp.Rows, "u1")
		if resp.Filtered != 1 || resp.Total != 4 {
			t.Fatalf("待补签 filtered=%d total=%d", resp.Filtered, resp.Total)
		}
	})

	t.Run("未知标签返回空 rows", func(t *testing.T) {
		p := newTagFilterPanel(t)
		rec, resp := getAccounts(t, p, appendTag("不存在的标签"))
		if len(resp.Rows) != 0 || resp.Filtered != 0 || resp.Total != 4 || resp.Pages != 0 {
			t.Fatalf("未知 tag rows=%d filtered=%d total=%d pages=%d", len(resp.Rows), resp.Filtered, resp.Total, resp.Pages)
		}
		if !strings.Contains(rec.Body.String(), `"rows":[]`) {
			t.Fatalf("未知 tag rows 应序列化为 []: %s", rec.Body.String())
		}
	})

	t.Run("空 tag 等于不筛选", func(t *testing.T) {
		p := newTagFilterPanel(t)
		_, resp := getAccounts(t, p, "tag=")
		if len(resp.Rows) != 4 || resp.Filtered != 4 {
			t.Fatalf("空 tag rows=%d filtered=%d want 4/4", len(resp.Rows), resp.Filtered)
		}
	})

	t.Run("与 q/state/realm/enterprise AND 组合", func(t *testing.T) {
		p := newTagFilterPanel(t)
		p.cfg.Pool.Disable("u2", "dead") // u2 变 disabled，用于 state 组合
		// tag + q（q 匹配 uid 子串）。
		_, resp := getAccounts(t, p, appendTag("采购A")+"&q=u1")
		assertUIDs(t, resp.Rows, "u1")
		// tag + state=healthy：u2 被禁用，剩 u1/u3。
		_, resp = getAccounts(t, p, appendTag("采购A")+"&state=healthy")
		assertUIDs(t, resp.Rows, "u1", "u3")
		// tag + enterprise=1：仅企业版 u3。
		_, resp = getAccounts(t, p, appendTag("采购A")+"&enterprise=1")
		assertUIDs(t, resp.Rows, "u3")
		// tag + realm=global：u4 无标签 → 空。
		_, resp = getAccounts(t, p, appendTag("采购A")+"&realm=global")
		if len(resp.Rows) != 0 || resp.Filtered != 0 {
			t.Fatalf("tag+realm=global rows=%v", accountUIDs(resp.Rows))
		}
		// tag + realm=cn：u1/u2/u3。
		_, resp = getAccounts(t, p, appendTag("采购A")+"&realm=cn")
		assertUIDs(t, resp.Rows, "u1", "u2", "u3")
	})

	t.Run("分页与筛选计数", func(t *testing.T) {
		p := newTagFilterPanel(t)
		_, resp := getAccounts(t, p, appendTag("采购A")+"&page_size=1&page=2")
		if len(resp.Rows) != 1 || resp.Rows[0].UID != "u2" || resp.Pages != 3 || resp.Filtered != 3 {
			t.Fatalf("tag 分页 rows=%v pages=%d filtered=%d", accountUIDs(resp.Rows), resp.Pages, resp.Filtered)
		}
	})

	t.Run("meta 修改后索引热更新", func(t *testing.T) {
		p := newTagFilterPanel(t)
		// 新增标签到 u4（原无标签）→ 立刻命中。
		metaPut(t, p, "u4", "", "采购A")
		_, resp := getAccounts(t, p, appendTag("采购A"))
		if resp.Filtered != 4 {
			t.Fatalf("新增标记后 filtered=%d want 4", resp.Filtered)
		}
		// 删除 u3 元数据 → 立刻从筛选中消失。
		if rec := metaCall(t, p, p.accountMetaDelete, http.MethodDelete, "u3", "", false); rec.Code != http.StatusOK {
			t.Fatalf("删除 meta status=%d", rec.Code)
		}
		_, resp = getAccounts(t, p, appendTag("采购A"))
		assertUIDs(t, resp.Rows, "u1", "u2", "u4")
		// 清空 u1 tags → 从筛选中消失。
		metaPut(t, p, "u1", "只剩备注")
		_, resp = getAccounts(t, p, appendTag("采购A"))
		assertUIDs(t, resp.Rows, "u2", "u4")
	})

	t.Run("未装配元数据存储时视为无匹配", func(t *testing.T) {
		pl := pool.New("")
		pl.Add(&auth.Auth{UID: "u1"})
		p := New(Config{Version: "test", APIKey: accountsTestKey, Pool: pl}) // 无 StateFile/ConfigPath
		_, resp := getAccounts(t, p, appendTag("采购A"))
		if len(resp.Rows) != 0 || resp.Filtered != 0 || resp.Total != 1 {
			t.Fatalf("无存储 tag 筛选 rows=%d filtered=%d total=%d", len(resp.Rows), resp.Filtered, resp.Total)
		}
	})
}

// TestAccountsTagETag ETag/304：tag 参与归一化 hash（同 tag 稳定、不同 tag/改 meta 变化）。
func TestAccountsTagETag(t *testing.T) {
	p := newTagFilterPanel(t)

	rec1, _ := getAccounts(t, p, appendTag("采购A"))
	etag1 := rec1.Header().Get("ETag")
	if !strings.HasPrefix(etag1, `W/"acct-`) {
		t.Fatalf("ETag=%q", etag1)
	}
	// 同 tag 两次 → 同 ETag。
	rec2, _ := getAccounts(t, p, appendTag("采购A"))
	if got := rec2.Header().Get("ETag"); got != etag1 {
		t.Fatalf("同 tag 两次 ETag 不同：%s vs %s", etag1, got)
	}
	// 大小写/空格变体语义等价 → 同 ETag（归一化后同参数）。
	for _, variant := range []string{"采购a", "  采购A  "} {
		rec, _ := getAccounts(t, p, appendTag(variant))
		if got := rec.Header().Get("ETag"); got != etag1 {
			t.Fatalf("tag=%q 归一化后 ETag=%s want %s", variant, got, etag1)
		}
	}
	// 不同 tag → 不同 ETag。
	recOther, _ := getAccounts(t, p, appendTag("其他"))
	if got := recOther.Header().Get("ETag"); got == etag1 {
		t.Fatal("不同 tag 的 ETag 相同（缓存键缺少 tag 参数）")
	}
	// 带 tag 与不带 tag → 不同 ETag。
	recAll, _ := getAccounts(t, p, "")
	if got := recAll.Header().Get("ETag"); got == etag1 {
		t.Fatal("无 tag 与 tag 的 ETag 相同")
	}
	// 同 tag 不同页 → 不同 ETag。
	recPage2, _ := getAccounts(t, p, appendTag("采购A")+"&page=2&page_size=1")
	if got := recPage2.Header().Get("ETag"); got == etag1 {
		t.Fatal("不同 page 的 ETag 相同")
	}

	// If-None-Match 命中 → 304 空 body，保留 ETag/Cache-Control/Vary。
	rec304 := panelRequest(p, http.MethodGet, "/panel/api/accounts?"+appendTag("采购A"), accountsTestKey, "", map[string]string{
		"If-None-Match": etag1,
	})
	if rec304.Code != http.StatusNotModified || rec304.Body.Len() != 0 {
		t.Fatalf("INM 命中 status=%d body=%q want 304 空", rec304.Code, rec304.Body.String())
	}
	if got := rec304.Header().Get("ETag"); got != etag1 {
		t.Fatalf("304 ETag=%q want %q", got, etag1)
	}
	if got := rec304.Header().Get("Cache-Control"); got != accountsCacheControl {
		t.Fatalf("304 Cache-Control=%q", got)
	}

	// 改 meta（u4 加同一标签 → 命中集合变化）→ ETag 必须变化，旧 ETag 不再 304。
	metaPut(t, p, "u4", "", "采购A")
	rec3, resp3 := getAccounts(t, p, appendTag("采购A"))
	etag2 := rec3.Header().Get("ETag")
	if etag2 == etag1 {
		t.Fatal("修改 meta 后 ETag 未变（会返回旧的 filtered/rows）")
	}
	if resp3.Filtered != 4 {
		t.Fatalf("修改 meta 后 filtered=%d want 4", resp3.Filtered)
	}
	recOld := panelRequest(p, http.MethodGet, "/panel/api/accounts?"+appendTag("采购A"), accountsTestKey, "", map[string]string{
		"If-None-Match": etag1,
	})
	if recOld.Code != http.StatusOK {
		t.Fatalf("旧 ETag 应回 200，got %d", recOld.Code)
	}
	recNew := panelRequest(p, http.MethodGet, "/panel/api/accounts?"+appendTag("采购A"), accountsTestKey, "", map[string]string{
		"If-None-Match": etag2,
	})
	if recNew.Code != http.StatusNotModified {
		t.Fatalf("新 ETag 应命中 304，got %d", recNew.Code)
	}
}

// TestMetaTagsIndex GET /panel/api/meta/tags：count 降序、同 count 字典序、大小写合并、
// meta 热更新、空态/无存储、401。
func TestMetaTagsIndex(t *testing.T) {
	readTags := func(t *testing.T, p *Panel) []accountMetaTagCount {
		t.Helper()
		rec := panelRequest(p, http.MethodGet, "/panel/api/meta/tags", accountsTestKey, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("tags status=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			OK   bool                  `json:"ok"`
			Tags []accountMetaTagCount `json:"tags"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode tags: %v body=%s", err, rec.Body.String())
		}
		if !resp.OK {
			t.Fatalf("ok=false: %s", rec.Body.String())
		}
		return resp.Tags
	}

	p := newTagFilterPanel(t)
	// u1:[采购A,待补签] u2:[采购a→合并采购A] u3:[采购A,其他] u4:无
	// 期望：采购A=3（u1,u2,u3 大小写合并）、其他=1、待补签=1；
	// count 降序，同 count 字典序（「其他」<「待补签」）。
	tags := readTags(t, p)
	if len(tags) != 3 {
		t.Fatalf("tags=%+v want 3 个", tags)
	}
	if tags[0].Tag != "采购A" || tags[0].Count != 3 {
		t.Fatalf("tags[0]=%+v want 采购A/3", tags[0])
	}
	if tags[1].Tag != "其他" || tags[1].Count != 1 {
		t.Fatalf("tags[1]=%+v want 其他/1", tags[1])
	}
	if tags[2].Tag != "待补签" || tags[2].Count != 1 {
		t.Fatalf("tags[2]=%+v want 待补签/1", tags[2])
	}
	// 与 accounts?tag= 的 filtered 计数一致。
	_, resp := getAccounts(t, p, appendTag("采购A"))
	if resp.Filtered != tags[0].Count {
		t.Fatalf("accounts?tag filtered=%d 与 tags count=%d 不一致", resp.Filtered, tags[0].Count)
	}

	// meta 热更新：u2 改到「其他」→ 其他=2、采购A=2（u1,u3）。
	metaPut(t, p, "u2", "", "其他")
	tags = readTags(t, p)
	// count 降序、同 count 字典序：其他(2) vs 采购A(2) → 「其他」字节序更小排前。
	wantTags := []accountMetaTagCount{
		{Tag: "其他", Count: 2},
		{Tag: "采购A", Count: 2},
		{Tag: "待补签", Count: 1},
	}
	if len(tags) != len(wantTags) {
		t.Fatalf("热更新后 tags=%+v want %+v", tags, wantTags)
	}
	for i := range wantTags {
		if tags[i] != wantTags[i] {
			t.Fatalf("热更新后 tags[%d]=%+v want %+v（全部=%+v）", i, tags[i], wantTags[i], tags)
		}
	}

	// 删除 u3 元数据 → 采购A 只剩 u1。
	if rec := metaCall(t, p, p.accountMetaDelete, http.MethodDelete, "u3", "", false); rec.Code != http.StatusOK {
		t.Fatalf("删除 meta status=%d", rec.Code)
	}
	tags = readTags(t, p)
	for _, tc := range tags {
		if tc.Tag == "采购A" && tc.Count != 1 {
			t.Fatalf("删除后 采购A count=%d want 1（%+v）", tc.Count, tags)
		}
	}
}

// TestMetaTagsEmptyAndAuth 空态：无标签=[]、未装配存储=[]；无 key → 401。
func TestMetaTagsEmptyAndAuth(t *testing.T) {
	t.Run("无标签返回空数组", func(t *testing.T) {
		p, _ := newAccountMetaPanel(t) // 空池、无 meta
		rec := panelRequest(p, http.MethodGet, "/panel/api/meta/tags", accountsTestKey, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"tags":[]`) {
			t.Fatalf("空标签应序列化为 []: %s", rec.Body.String())
		}
	})
	t.Run("未装配存储返回空数组", func(t *testing.T) {
		p := New(Config{Version: "test", APIKey: accountsTestKey, Pool: pool.New("")})
		rec := panelRequest(p, http.MethodGet, "/panel/api/meta/tags", accountsTestKey, "", nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"tags":[]`) {
			t.Fatalf("无存储 status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("401", func(t *testing.T) {
		p := newTagFilterPanel(t)
		rec := panelRequest(p, http.MethodGet, "/panel/api/meta/tags", "", "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("无 key status=%d want 401", rec.Code)
		}
	})
}

// TestAccountMetaTagHelpers 索引查询 helper 直测：大小写/TrimSpace/未知 uid/空标签，
// 以及 UID 集合与 MatchesTag 一致。
func TestAccountMetaTagHelpers(t *testing.T) {
	p := newTagFilterPanel(t)

	cases := []struct {
		uid  string
		tag  string
		want bool
	}{
		{"u1", "采购A", true},
		{"u1", "采购a", true},
		{"u1", "  采购A  ", true},
		{"u2", "采购A", true}, // u2 存的是 采购a
		{"u3", "其他", true},
		{"u1", "其他", false},
		{"u2", "其他", false},
		{"u4", "采购A", false},
		{"not-exist", "采购A", false},
		{"u1", "   ", false},
		{"", "采购A", false},
	}
	for _, tc := range cases {
		if got := p.accountMetaMatchesTag(tc.uid, tc.tag); got != tc.want {
			t.Errorf("accountMetaMatchesTag(%q,%q)=%v want %v", tc.uid, tc.tag, got, tc.want)
		}
	}

	// UID 集合：大小写变体返回同一集合；未知标签空集合（非 nil）。
	set := p.accountMetaUIDsForTag("采购A")
	if len(set) != 3 {
		t.Fatalf("UIDs(采购A)=%v want 3", set)
	}
	if set2 := p.accountMetaUIDsForTag("采购a"); len(set2) != len(set) {
		t.Fatalf("大小写变体集合不同：%v vs %v", set, set2)
	}
	for uid := range set {
		if !p.accountMetaMatchesTag(uid, "采购A") {
			t.Fatalf("索引与 MatchesTag 不一致：uid=%s", uid)
		}
	}
	if got := p.accountMetaUIDsForTag("未知"); got == nil || len(got) != 0 {
		t.Fatalf("未知标签应返回空非 nil 集合，got %v", got)
	}
	if got := p.accountMetaUIDsForTag("  "); got != nil {
		t.Fatalf("空标签应返回 nil，got %v", got)
	}

	// 未装配存储：MatchesTag=false、UID 集合 nil、TagCounts 空。
	noStore := New(Config{Version: "test", APIKey: accountsTestKey, Pool: pool.New("")})
	if noStore.accountMetaMatchesTag("u1", "采购A") {
		t.Fatal("无存储时 MatchesTag 应 false")
	}
	if got := noStore.accountMetaUIDsForTag("采购A"); got != nil {
		t.Fatalf("无存储 UID 集合应为 nil，got %v", got)
	}
	if got := noStore.accountMetaTagCounts(); len(got) != 0 {
		t.Fatalf("无存储 TagCounts 应为空，got %+v", got)
	}
}
