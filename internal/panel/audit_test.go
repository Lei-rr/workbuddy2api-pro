// audit_test.go 面板操作审计（v2.2.0 #5）的写入/读取/埋点契约测试。
package panel

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
)

// auditTestPanel 构造启用审计落盘（StateFile 在 t.TempDir）的面板。
func auditTestPanel(t *testing.T) (*Panel, *pool.Pool, string) {
	t.Helper()
	dir := t.TempDir()
	pl := pool.New("")
	p := New(Config{
		Version: "test", APIKey: accountsTestKey, Pool: pl,
		StateFile: filepath.Join(dir, "state.json"),
	})
	return p, pl, dir
}

// readAudit 调 GET /panel/api/audit 并解出审计条目（断言 200）。
func readAudit(t *testing.T, p *Panel, query string) ([]AuditEntry, int) {
	t.Helper()
	path := "/panel/api/audit"
	if query != "" {
		path += "?" + query
	}
	rec := panelRequest(p, http.MethodGet, path, accountsTestKey, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool         `json:"ok"`
		Entries []AuditEntry `json:"entries"`
		Limit   int          `json:"limit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode audit: %v body=%s", err, rec.Body.String())
	}
	if !resp.OK {
		t.Fatalf("ok=false: %s", rec.Body.String())
	}
	return resp.Entries, resp.Limit
}

// TestAuditWriteOnOperations 写操作自动落审计：成功与失败都记，GET 只读不记；
// 字段 {ts,ip,action,uid,ok,detail} 齐全，倒序返回。
func TestAuditWriteOnOperations(t *testing.T) {
	p, pl, dir := auditTestPanel(t)
	pl.Add(&auth.Auth{UID: "u1", Nickname: "n1"})

	// 只读接口不应产生审计。
	panelRequest(p, http.MethodGet, "/panel/api/overview", accountsTestKey, "", nil)
	panelRequest(p, http.MethodGet, "/panel/api/accounts", accountsTestKey, "", nil)

	// 成功写操作。
	if rec := panelRequest(p, http.MethodPost, "/panel/api/accounts/u1/disable", accountsTestKey, "", nil); rec.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 失败写操作（账号不存在 → 404）。
	if rec := panelRequest(p, http.MethodPost, "/panel/api/accounts/nope/disable", accountsTestKey, "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("disable 未知 uid status=%d want 404", rec.Code)
	}
	// 业务拒绝（rescan 未配置 auth_dir → 501）同样留痕。
	if rec := panelRequest(p, http.MethodPost, "/panel/api/accounts/rescan", accountsTestKey, "", nil); rec.Code != http.StatusNotImplemented {
		t.Fatalf("rescan status=%d want 501", rec.Code)
	}

	// 审计文件已创建且为 JSONL。
	auditPath := filepath.Join(dir, auditFileName)
	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("审计文件未落盘: %v", err)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatalf("审计文件应为逐行 JSON 且以换行结尾: %q", raw)
	}

	entries, limit := readAudit(t, p, "limit=10")
	if n := 3; len(entries) != n {
		t.Fatalf("审计条目=%d want %d（只读 GET 不应计入）: %+v", len(entries), n, entries)
	}
	if limit != 10 {
		t.Fatalf("limit 回显=%d want 10", limit)
	}

	// 倒序：最新的是 rescan(501)，然后是未知 uid 的 disable(404)，最早是成功 disable。
	if got := entries[0].Action; got != auditActionAccountsRescan {
		t.Fatalf("entries[0].action=%q want %q", got, auditActionAccountsRescan)
	}
	if entries[0].OK {
		t.Fatal("rescan 未配置 auth_dir 应记 ok=false")
	}
	if entries[0].Detail == "" {
		t.Fatal("失败操作应带 detail 说明")
	}
	if got := entries[1].Action; got != auditActionAccountDisable || entries[1].OK || entries[1].UID != "nope" {
		t.Fatalf("entries[1]=%+v want account_disable/ok=false/uid=nope", entries[1])
	}
	if got := entries[2]; got.Action != auditActionAccountDisable || !got.OK || got.UID != "u1" {
		t.Fatalf("entries[2]=%+v want account_disable/ok=true/uid=u1", got)
	}
	if entries[2].TS <= 0 {
		t.Fatalf("ts=%d want unix 秒", entries[2].TS)
	}
	if entries[2].IP == "" {
		t.Fatal("ip 不应为空（httptest 默认 192.0.2.1）")
	}
	if entries[2].Detail != "" {
		t.Fatalf("成功操作 detail 应为空，got %q", entries[2].Detail)
	}
}

// TestAuditLimitAndTailClamp limit 默认/上限/非法值处理，且只返回最近 N 条。
func TestAuditLimitAndTailClamp(t *testing.T) {
	p, _, _ := auditTestPanel(t)
	for i := 0; i < 20; i++ {
		p.audit(nil, "test_action", "u", true, "")
	}
	entries, limit := readAudit(t, p, "")
	if len(entries) != 20 || limit != auditDefaultLimit {
		t.Fatalf("默认 limit：entries=%d limit=%d want 20/%d", len(entries), limit, auditDefaultLimit)
	}
	entries, limit = readAudit(t, p, "limit=5")
	if len(entries) != 5 || limit != 5 {
		t.Fatalf("limit=5：entries=%d limit=%d", len(entries), limit)
	}
	entries, limit = readAudit(t, p, "limit=99999")
	if len(entries) != 20 || limit != auditMaxLimit {
		t.Fatalf("limit 上限：entries=%d limit=%d want 20/%d", len(entries), limit, auditMaxLimit)
	}
}

// TestAuditEmptyWhenFileMissing 尚无审计文件：返回 200 + entries:[]（不是 404/null）。
func TestAuditEmptyWhenFileMissing(t *testing.T) {
	p, _, _ := auditTestPanel(t)
	rec := panelRequest(p, http.MethodGet, "/panel/api/audit", accountsTestKey, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if want := `"entries":[]`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("缺失文件应返回空数组: body=%s want 含 %s", rec.Body.String(), want)
	}
}

// TestAuditDisabledWithoutDataDir 未装配 state/config 路径：审计功能关闭（不落盘），
// 查询接口仍稳定返回空数组（测试/只读装配零副作用）。
func TestAuditDisabledWithoutDataDir(t *testing.T) {
	pl := pool.New("")
	p := New(Config{Version: "test", APIKey: accountsTestKey, Pool: pl})
	if p.dataDir != "" || p.auditLog.enabled() {
		t.Fatalf("未装配路径时应关闭审计：dataDir=%q auditPath=%q", p.dataDir, p.auditLog.path)
	}
	panelRequest(p, http.MethodPost, "/panel/api/accounts/nope/disable", accountsTestKey, "", nil) // 404，不 panic
	if entries, _ := readAudit(t, p, ""); len(entries) != 0 {
		t.Fatalf("关闭状态下不应有审计条目: %+v", entries)
	}
}

// TestAuditRequiresAuth 审计查询接口走 withAuth。
func TestAuditRequiresAuth(t *testing.T) {
	p, _, _ := auditTestPanel(t)
	if rec := panelRequest(p, http.MethodGet, "/panel/api/audit", "", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无密钥 status=%d want 401", rec.Code)
	}
}

// TestResponseJSONBool 登录轮询条件审计用的 JSON 布尔解析。
func TestResponseJSONBool(t *testing.T) {
	for _, tc := range []struct {
		body  string
		key   string
		value bool
		found bool
	}{
		{`{"done":true}`, "done", true, true},
		{`{"done":false}`, "done", false, true},
		{`{"other":true}`, "done", false, false},
		{`not-json`, "done", false, false},
		{``, "done", false, false},
	} {
		v, f := responseJSONBool([]byte(tc.body), tc.key)
		if v != tc.value || f != tc.found {
			t.Errorf("responseJSONBool(%q,%q)=%v,%v want %v,%v", tc.body, tc.key, v, f, tc.value, tc.found)
		}
	}
}
