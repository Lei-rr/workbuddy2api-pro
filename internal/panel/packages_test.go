package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
)

// packages?uid= 单号模式：未知账号必须 404，且不得退化成全量整池查询。
//
// 面板「积分」按钮走单号模式（只打 1 次上游）；输错/过期的 UID 若落进全量
// 分支，就会重现 2026-10 移除的整池逐号查询（批量特征、风控风险高）。
func TestPanelPackagesSingleUIDUnknown(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/packages?uid=nope", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("packages?uid=nope status=%d want 404; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "account not found") {
		t.Fatalf("packages?uid=nope body=%s", rec.Body.String())
	}
}

// 无 uid（或只有空白）一律 400：全量模式已移除，避免整池逐号查上游的批量特征。
func TestPanelPackagesRequiresUID(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
	for _, q := range []string{"", "?uid=", "?uid=%20%20"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/panel/api/packages"+q, nil)
		req.Header.Set("Authorization", "Bearer test-key")
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("packages%s status=%d want 400; body=%s", q, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "uid is required") {
			t.Fatalf("packages%s body=%s", q, rec.Body.String())
		}
	}
}
