package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
)

// 重新扫描的安全阀：扫到 0 个文件而池非空（挂载丢失/路径配错）必须拒绝。
// 否则 SyncToDir 会清空整池并把结果落盘，属于不可逆误操作。
func TestPanelAccountsRescanRejectsEmptyDir(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1"})
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, AuthDir: t.TempDir()})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/accounts/rescan", nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d want 409; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := pl.Status("u1"); !ok {
		t.Fatal("拒绝执行时不得改动账号池")
	}
}

// 空目录 + 空池：等价于「还没有账号」，允许通过并返回 scanned=0。
func TestPanelAccountsRescanEmptyPool(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), AuthDir: t.TempDir()})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/accounts/rescan", nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"scanned":0`) {
		t.Fatalf("body=%s want scanned:0", rec.Body.String())
	}
}
