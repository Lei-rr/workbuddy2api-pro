// rbac_test.go v2.4.0 #6 面板多用户 / 角色 / 会话登录的契约测试。
//
// 覆盖：单密钥模式零破坏、登录成功/失败/限速/审计、Bearer 会话与 Cookie 会话、
// logout 失效、角色矩阵（viewer/operator/admin）、会话过期、密码哈希、auth/info、
// 403 响应形状、Cookie 安全属性。
package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
)

// rbacTestSalt / rbacTestHashPassword 生成 `sha256:<salt>:<hex>`（hex=sha256(salt+密码)）。
func rbacTestHashPassword(salt, password string) string {
	sum := sha256.Sum256([]byte(salt + password))
	return "sha256:" + salt + ":" + hex.EncodeToString(sum[:])
}

// rbacTestUsers 多用户测试集：明文 admin/operator/viewer + 一个 password_hash 用户。
func rbacTestUsers() []PanelUser {
	return []PanelUser{
		{Name: "alice", Password: "pw-admin", Role: "admin"},
		{Name: "bob", Password: "pw-op", Role: "operator"},
		{Name: "carol", Password: "pw-view", Role: "viewer"},
		{Name: "dave", PasswordHash: rbacTestHashPassword("salt1", "pw-hash"), Role: "operator"},
	}
}

// newRBACPanel 构造面板（默认 APIKey=root-key + 空池）；mut 可调整 Config。
func newRBACPanel(t *testing.T, mut func(*Config)) *Panel {
	t.Helper()
	cfg := Config{Version: "test", APIKey: "root-key", Pool: pool.New("")}
	if mut != nil {
		mut(&cfg)
	}
	return New(cfg)
}

// rbacRequest 发请求（支持 JSON body / 自定义头；默认 RemoteAddr 由 httptest 决定）。
func rbacRequest(p *Panel, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// bearerHdr 构造 Bearer 鉴权头（key 可以是 api_key 或会话 token）。
func bearerHdr(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// rbacLogin 登录并返回响应（不断言状态码，便于测试失败分支）。
func rbacLogin(p *Panel, user, pass string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	return rbacRequest(p, http.MethodPost, "/panel/api/auth/login", string(body), nil)
}

// rbacLoginOK 登录成功并返回 token 与响应（断言 200/契约字段齐全）。
func rbacLoginOK(t *testing.T, p *Panel, user, pass string) (string, *httptest.ResponseRecorder) {
	t.Helper()
	rec := rbacLogin(p, user, pass)
	if rec.Code != http.StatusOK {
		t.Fatalf("登录 %s status=%d body=%s", user, rec.Code, rec.Body.String())
	}
	var resp struct {
		OK        bool   `json:"ok"`
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
		User      struct {
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("登录响应不是合法 JSON: %v body=%s", err, rec.Body.String())
	}
	if !resp.OK || resp.Token == "" || resp.ExpiresAt < time.Now().Unix() || resp.User.Name != user {
		t.Fatalf("登录响应契约不符: %+v body=%s", resp, rec.Body.String())
	}
	if _, err := hex.DecodeString(resp.Token); err != nil || len(resp.Token) != 64 {
		t.Fatalf("token 应为 32B hex（64 位），got %q err=%v", resp.Token, err)
	}
	return resp.Token, rec
}

// sessionCookieFrom 从登录响应里取 wb2_session Cookie。
func sessionCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatalf("登录响应缺少 %s Cookie: %v", sessionCookieName, rec.Header())
	return nil
}

// TestRBACSingleKeyModeUnchanged 无 users 配置：与 v2.3 行为完全一致（api_key=admin）。
func TestRBACSingleKeyModeUnchanged(t *testing.T) {
	p := newRBACPanel(t, nil) // 无 Users = 单密钥模式

	// 正确 api_key → 只读接口 200。
	if rec := panelRequest(p, http.MethodGet, "/panel/api/overview", "root-key", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("api_key overview status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 错误/缺失 api_key → 401（与旧版一致）。
	if rec := panelRequest(p, http.MethodGet, "/panel/api/overview", "wrong", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误 api_key status=%d want 401", rec.Code)
	}
	if rec := panelRequest(p, http.MethodGet, "/panel/api/overview", "", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 api_key status=%d want 401", rec.Code)
	}
	// api_key 通道 = admin：写接口过门禁（LoadConfig nil → 501，不是 403）。
	if rec := panelRequest(p, http.MethodPost, "/panel/api/config", "root-key", "", nil); rec.Code != http.StatusNotImplemented {
		t.Fatalf("api_key POST config status=%d want 501（admin 门禁通过）", rec.Code)
	}
	// auth/info：mode=single、user=null、api_key=true、session=false、默认 TTL 12h。
	rec := panelRequest(p, http.MethodGet, "/panel/api/auth/info", "root-key", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth/info status=%d body=%s", rec.Code, rec.Body.String())
	}
	var info struct {
		OK   bool   `json:"ok"`
		Mode string `json:"mode"`
		User any    `json:"user"`
		Auth struct {
			APIKey  bool `json:"api_key"`
			Session bool `json:"session"`
		} `json:"auth"`
		EventsEnabled bool `json:"events_enabled"`
		SessionTTLSec int  `json:"session_ttl_sec"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode auth/info: %v", err)
	}
	if !info.OK || info.Mode != "single" || info.User != nil || !info.Auth.APIKey || info.Auth.Session {
		t.Fatalf("auth/info(single)=%+v", info)
	}
	if info.SessionTTLSec != int(defaultPanelSessionTTL/time.Second) {
		t.Fatalf("session_ttl_sec=%d want %d", info.SessionTTLSec, int(defaultPanelSessionTTL/time.Second))
	}
	// 单密钥模式登录入口 → 400「未配置面板用户」。
	if rec := rbacLogin(p, "anyone", "any"); rec.Code != http.StatusBadRequest {
		t.Fatalf("单密钥模式 login status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	// 未配置任何密钥的面板：沿用既有「不鉴权」口径（仍可访问）。
	pNoKey := New(Config{Version: "test", Pool: pool.New("")})
	if rec := panelRequest(pNoKey, http.MethodGet, "/panel/api/overview", "", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("空 api_key 面板 overview status=%d want 200（既有不鉴权语义）", rec.Code)
	}
}

// TestRBACLoginAndBearerSession 多用户登录 + Bearer 会话身份。
func TestRBACLoginAndBearerSession(t *testing.T) {
	p := newRBACPanel(t, func(c *Config) { c.Users = rbacTestUsers() })

	token, loginRec := rbacLoginOK(t, p, "alice", "pw-admin")
	cookie := sessionCookieFrom(t, loginRec)
	if !cookie.HttpOnly || cookie.Path != "/" || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("Cookie 安全属性不符: %+v", cookie)
	}
	if strings.Contains(loginRec.Body.String(), "pw-admin") {
		t.Fatal("登录响应不得回显密码")
	}
	// password_hash 用户也能登录。
	if _, rec := rbacLoginOK(t, p, "dave", "pw-hash"); rec.Code != http.StatusOK {
		t.Fatalf("hash 用户登录 status=%d", rec.Code)
	}

	// Bearer 会话：auth/info 返回 user 与 session=true。
	rec := rbacRequest(p, http.MethodGet, "/panel/api/auth/info", "", bearerHdr(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("会话 auth/info status=%d body=%s", rec.Code, rec.Body.String())
	}
	var info struct {
		Mode string `json:"mode"`
		User *struct {
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"user"`
		Auth struct {
			APIKey  bool `json:"api_key"`
			Session bool `json:"session"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Mode != "multi" || info.User == nil || info.User.Name != "alice" || info.User.Role != "admin" ||
		info.Auth.APIKey || !info.Auth.Session {
		t.Fatalf("会话 auth/info=%+v", info)
	}
	// 会话可访问业务接口。
	if rec := rbacRequest(p, http.MethodGet, "/panel/api/accounts", "", bearerHdr(token)); rec.Code != http.StatusOK {
		t.Fatalf("会话 GET accounts status=%d", rec.Code)
	}
	// 多用户模式下 api_key 仍为 root（admin）：auth/info api_key=true、user=null。
	rec = panelRequest(p, http.MethodGet, "/panel/api/auth/info", "root-key", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("root api_key info status=%d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode root info: %v", err)
	}
	if info.Mode != "multi" || info.User != nil || !info.Auth.APIKey || info.Auth.Session {
		t.Fatalf("root api_key info=%+v", info)
	}
}

// TestRBACCookieSession Cookie 会话（不带 Authorization 也能用）与 logout 清理。
func TestRBACCookieSession(t *testing.T) {
	p := newRBACPanel(t, func(c *Config) { c.Users = rbacTestUsers() })
	_, loginRec := rbacLoginOK(t, p, "bob", "pw-op")
	cookie := sessionCookieFrom(t, loginRec)

	rec := rbacRequest(p, http.MethodGet, "/panel/api/auth/info", "", map[string]string{
		"Cookie": sessionCookieName + "=" + cookie.Value,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("Cookie 会话 info status=%d body=%s", rec.Code, rec.Body.String())
	}
	var info struct {
		User *struct {
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"user"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &info)
	if info.User == nil || info.User.Name != "bob" || info.User.Role != "operator" {
		t.Fatalf("Cookie 会话 user=%+v", info.User)
	}

	// logout：删除会话 + 清 Cookie；之后同 Cookie 失效。
	rec = rbacRequest(p, http.MethodPost, "/panel/api/auth/logout", "", map[string]string{
		"Cookie": sessionCookieName + "=" + cookie.Value,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("logout status=%d body=%s", rec.Code, rec.Body.String())
	}
	cleared := sessionCookieFrom(t, rec)
	if cleared.MaxAge >= 0 {
		t.Fatalf("logout 应清 Cookie（MaxAge<0），got %d", cleared.MaxAge)
	}
	if rec := rbacRequest(p, http.MethodGet, "/panel/api/auth/info", "", map[string]string{
		"Cookie": sessionCookieName + "=" + cookie.Value,
	}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("logout 后旧 Cookie status=%d want 401", rec.Code)
	}
}

// TestRBACLogoutInvalidatesBearerToken Bearer 令牌 logout 后失效（幂等 200 → 再 401）。
func TestRBACLogoutInvalidatesBearerToken(t *testing.T) {
	p := newRBACPanel(t, func(c *Config) { c.Users = rbacTestUsers() })
	token, _ := rbacLoginOK(t, p, "carol", "pw-view")

	rec := rbacRequest(p, http.MethodPost, "/panel/api/auth/logout", "", bearerHdr(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("logout status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := rbacRequest(p, http.MethodGet, "/panel/api/auth/info", "", bearerHdr(token)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("logout 后 token status=%d want 401", rec.Code)
	}
	// 已失效 token 再 logout → 401（鉴权层拒绝，不是 200）。
	if rec := rbacRequest(p, http.MethodPost, "/panel/api/auth/logout", "", bearerHdr(token)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("重复 logout status=%d want 401", rec.Code)
	}
}

// TestRBACRoleMatrix 角色矩阵：viewer 只读 / operator 业务写 / admin 全部。
func TestRBACRoleMatrix(t *testing.T) {
	p := newRBACPanel(t, func(c *Config) {
		c.Users = rbacTestUsers()
		c.StateFile = filepath.Join(t.TempDir(), "state.json") // 启用 meta 存储（PUT/DELETE 需要）
	})
	viewerTok, _ := rbacLoginOK(t, p, "carol", "pw-view")
	opTok, _ := rbacLoginOK(t, p, "bob", "pw-op")
	adminTok, _ := rbacLoginOK(t, p, "alice", "pw-admin")

	type reqCase struct {
		name   string
		method string
		path   string
		token  string
		status int // 期望状态码；门禁拒绝=403
	}
	cases := []reqCase{
		// viewer：GET 允许。
		{"viewer GET overview", http.MethodGet, "/panel/api/overview", viewerTok, http.StatusOK},
		{"viewer GET accounts", http.MethodGet, "/panel/api/accounts", viewerTok, http.StatusOK},
		{"viewer GET info", http.MethodGet, "/panel/api/auth/info", viewerTok, http.StatusOK},
		{"viewer GET tasks queue", http.MethodGet, "/panel/api/tasks/queue", viewerTok, http.StatusOK},
		// viewer：写操作 403。
		{"viewer POST checkin", http.MethodPost, "/panel/api/accounts/u1/checkin", viewerTok, http.StatusForbidden},
		{"viewer POST disable", http.MethodPost, "/panel/api/accounts/u1/disable", viewerTok, http.StatusForbidden},
		{"viewer PUT meta", http.MethodPut, "/panel/api/accounts/u1/meta", viewerTok, http.StatusForbidden},
		{"viewer POST usage save", http.MethodPost, "/panel/api/usage/save", viewerTok, http.StatusForbidden},
		{"viewer POST checkin_all", http.MethodPost, "/panel/api/checkin_all", viewerTok, http.StatusForbidden},
		// 配置页连读都仅 admin（含 api_key/用户口令哈希）。
		{"viewer GET config", http.MethodGet, "/panel/api/config", viewerTok, http.StatusForbidden},
		// operator：业务写放行（未知 uid → 404 证明已过门禁）+ 管理员专属 403。
		{"operator POST disable unknown", http.MethodPost, "/panel/api/accounts/nope/disable", opTok, http.StatusNotFound},
		{"operator POST checkin unknown", http.MethodPost, "/panel/api/accounts/nope/checkin", opTok, http.StatusNotFound},
		{"operator PUT meta", http.MethodPut, "/panel/api/accounts/u1/meta", opTok, http.StatusOK},
		{"operator DELETE meta", http.MethodDelete, "/panel/api/accounts/u1/meta", opTok, http.StatusOK},
		{"operator POST checkin_all (scheduler nil→501)", http.MethodPost, "/panel/api/checkin_all", opTok, http.StatusNotImplemented},
		{"operator POST remove", http.MethodPost, "/panel/api/accounts/u1/remove", opTok, http.StatusForbidden},
		{"operator POST rescan", http.MethodPost, "/panel/api/accounts/rescan", opTok, http.StatusForbidden},
		{"operator POST login start", http.MethodPost, "/panel/api/login/start", opTok, http.StatusForbidden},
		{"operator POST import", http.MethodPost, "/panel/api/import/cockpit", opTok, http.StatusForbidden},
		{"operator POST config", http.MethodPost, "/panel/api/config", opTok, http.StatusForbidden},
		{"operator POST config rollback", http.MethodPost, "/panel/api/config/rollback", opTok, http.StatusForbidden},
		{"operator GET config", http.MethodGet, "/panel/api/config", opTok, http.StatusForbidden},
		// admin：全部放行。
		{"admin GET config (LoadConfig nil→501)", http.MethodGet, "/panel/api/config", adminTok, http.StatusNotImplemented},
		{"admin POST config (SaveConfig nil→501)", http.MethodPost, "/panel/api/config", adminTok, http.StatusNotImplemented},
		{"admin POST remove unknown", http.MethodPost, "/panel/api/accounts/nope/remove", adminTok, http.StatusNotFound},
		// api_key root 通道在多用户模式同样是 admin。
		{"root key GET config", http.MethodGet, "/panel/api/config", "root-key", http.StatusNotImplemented},
		{"root key POST remove unknown", http.MethodPost, "/panel/api/accounts/nope/remove", "root-key", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// PUT meta 需要 JSON body（校验通过才 200；门禁 403 时 handler 不会执行）。
			body := ""
			if tc.method == http.MethodPut && strings.HasSuffix(tc.path, "/meta") {
				body = `{"note":"rbac"}`
			}
			rec := rbacRequest(p, tc.method, tc.path, body, bearerHdr(tc.token))
			if rec.Code != tc.status {
				t.Fatalf("%s %s status=%d want %d body=%s", tc.method, tc.path, rec.Code, tc.status, rec.Body.String())
			}
			if tc.status == http.StatusForbidden {
				var resp struct {
					OK    bool   `json:"ok"`
					Error string `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.OK || !strings.Contains(resp.Error, "权限不足") {
					t.Fatalf("403 响应形状不符: %s", rec.Body.String())
				}
			}
		})
	}
}

// TestRBACViewerCanLogout viewer 也允许 POST /auth/logout（其余写仍 403）。
func TestRBACViewerCanLogout(t *testing.T) {
	p := newRBACPanel(t, func(c *Config) { c.Users = rbacTestUsers() })
	token, _ := rbacLoginOK(t, p, "carol", "pw-view")
	if rec := rbacRequest(p, http.MethodPost, "/panel/api/auth/logout", "", bearerHdr(token)); rec.Code != http.StatusOK {
		t.Fatalf("viewer logout status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
}

// TestRBACSessionExpiry 短 TTL 会话过期后 401（惰性清理）。
func TestRBACSessionExpiry(t *testing.T) {
	p := newRBACPanel(t, func(c *Config) {
		c.Users = rbacTestUsers()
		c.SessionTTL = 40 * time.Millisecond
	})
	token, rec := rbacLoginOK(t, p, "alice", "pw-admin")
	var login struct {
		SessionTTLSec int `json:"session_ttl_sec"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	if login.SessionTTLSec != 0 { // 40ms 向下取整为 0 秒（仅契约一致性检查，不要求 0）
		t.Logf("session_ttl_sec=%d（短 TTL 测试）", login.SessionTTLSec)
	}
	if rec := rbacRequest(p, http.MethodGet, "/panel/api/auth/info", "", bearerHdr(token)); rec.Code != http.StatusOK {
		t.Fatalf("过期前 info status=%d", rec.Code)
	}
	time.Sleep(80 * time.Millisecond)
	if rec := rbacRequest(p, http.MethodGet, "/panel/api/auth/info", "", bearerHdr(token)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("过期后 info status=%d want 401", rec.Code)
	}
}

// TestRBACLoginFailureRateLimit 连续失败 20 次后第 21 次 429 + Retry-After（沿用既有 limiter）。
func TestRBACLoginFailureRateLimit(t *testing.T) {
	p := newRBACPanel(t, func(c *Config) { c.Users = rbacTestUsers() })
	for i := 0; i < 20; i++ {
		if rec := rbacLogin(p, "alice", "wrong-password"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败登录 status=%d want 401 body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := rbacLogin(p, "alice", "pw-admin") // 即使密码正确，超限也先 429
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超限后 status=%d want 429 body=%s", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("429 响应缺少 Retry-After")
	}
}

// TestRBACLoginFailureResponsesUniform 登录失败不区分「用户不存在 / 密码错误」。
func TestRBACLoginFailureResponsesUniform(t *testing.T) {
	p := newRBACPanel(t, func(c *Config) { c.Users = rbacTestUsers() })
	unknown := rbacLogin(p, "nobody", "whatever")
	wrongPw := rbacLogin(p, "alice", "wrong")
	if unknown.Code != http.StatusUnauthorized || wrongPw.Code != http.StatusUnauthorized {
		t.Fatalf("失败登录状态码 unknown=%d wrong=%d want 401", unknown.Code, wrongPw.Code)
	}
	if unknown.Body.String() != wrongPw.Body.String() {
		t.Fatalf("失败响应应一致（不泄露用户是否存在）: unknown=%s wrong=%s",
			unknown.Body.String(), wrongPw.Body.String())
	}
	if strings.Contains(unknown.Body.String(), "nobody") || strings.Contains(wrongPw.Body.String(), "alice") {
		t.Fatal("失败响应不得回显用户名")
	}
}

// TestRBACPasswordEdge 密码缺失/哈希损坏时拒绝登录且不 panic。
func TestRBACPasswordEdge(t *testing.T) {
	p := newRBACPanel(t, func(c *Config) {
		c.Users = []PanelUser{
			{Name: "nopass", Role: "admin"},                                         // 未配置密码
			{Name: "badhash", PasswordHash: "sha256:missing-colons", Role: "admin"}, // 格式错误
			{Name: "badsalt", PasswordHash: "sha256:salt:" + strings.Repeat("0", 64), Role: "admin"},
		}
	})
	for _, tc := range []struct{ user, pass string }{
		{"nopass", ""},
		{"nopass", "x"},
		{"badhash", "x"},
		{"badsalt", "x"},
	} {
		if rec := rbacLogin(p, tc.user, tc.pass); rec.Code != http.StatusUnauthorized {
			t.Fatalf("用户 %s 应拒绝登录，status=%d body=%s", tc.user, rec.Code, rec.Body.String())
		}
	}
}

// TestRBACInvalidRoleFallsBackViewer 未知/空角色按 viewer 最小权限处理（配置防御）。
func TestRBACInvalidRoleFallsBackViewer(t *testing.T) {
	p := newRBACPanel(t, func(c *Config) {
		c.Users = []PanelUser{
			{Name: "eve", Password: "pw-eve", Role: "superuser"},
			{Name: "frank", Password: "pw-frank", Role: ""},
		}
	})
	for _, tc := range []struct{ user, pass string }{
		{"eve", "pw-eve"},
		{"frank", "pw-frank"},
	} {
		token, _ := rbacLoginOK(t, p, tc.user, tc.pass)
		rec := rbacRequest(p, http.MethodGet, "/panel/api/auth/info", "", bearerHdr(token))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s info status=%d", tc.user, rec.Code)
		}
		var info struct {
			User struct {
				Role string `json:"role"`
			} `json:"user"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &info)
		if info.User.Role != panelRoleViewer {
			t.Fatalf("%s 未知角色应回落 viewer，got %q", tc.user, info.User.Role)
		}
		if rec := rbacRequest(p, http.MethodPost, "/panel/api/accounts/u1/disable", "", bearerHdr(token)); rec.Code != http.StatusForbidden {
			t.Fatalf("%s 以 viewer 处理时写操作应 403，got %d", tc.user, rec.Code)
		}
	}
}

// TestRBACLoginAudit 登录成功/失败都记 panel_login 审计，且不含密码。
func TestRBACLoginAudit(t *testing.T) {
	dir := t.TempDir()
	p := newRBACPanel(t, func(c *Config) {
		c.Users = rbacTestUsers()
		c.StateFile = dir + "/state.json"
	})
	if rec := rbacLogin(p, "alice", "pw-admin"); rec.Code != http.StatusOK {
		t.Fatalf("login status=%d", rec.Code)
	}
	if rec := rbacLogin(p, "alice", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("失败登录 status=%d", rec.Code)
	}
	rec := rbacRequest(p, http.MethodGet, "/panel/api/audit?limit=10", "", bearerHdr("root-key"))
	if rec.Code != http.StatusOK {
		t.Fatalf("audit status=%d body=%s", rec.Code, rec.Body.String())
	}
	var auditResp struct {
		OK      bool         `json:"ok"`
		Entries []AuditEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &auditResp); err != nil {
		t.Fatalf("decode audit: %v body=%s", err, rec.Body.String())
	}
	entries := auditResp.Entries
	if len(entries) != 2 {
		t.Fatalf("审计条目=%d want 2: %+v", len(entries), entries)
	}
	// 新的在前：失败在前、成功在后。
	if entries[0].Action != panelAuditLogin || entries[0].OK || !strings.Contains(entries[0].Detail, "登录失败") {
		t.Fatalf("失败审计=%+v", entries[0])
	}
	if entries[1].Action != panelAuditLogin || !entries[1].OK ||
		!strings.Contains(entries[1].Detail, "alice") || !strings.Contains(entries[1].Detail, "admin") {
		t.Fatalf("成功审计=%+v", entries[1])
	}
	for _, e := range entries {
		if strings.Contains(e.Detail, "pw-admin") || strings.Contains(e.Detail, "wrong") {
			t.Fatalf("审计 detail 泄露密码: %+v", e)
		}
	}
}

// TestRBACRoleFromContext withAuth 把身份写入 context：roleFrom 可读、403 时不进 handler。
func TestRBACRoleFromContext(t *testing.T) {
	p := newRBACPanel(t, func(c *Config) { c.Users = rbacTestUsers() })
	viewerTok, _ := rbacLoginOK(t, p, "carol", "pw-view")
	adminTok, _ := rbacLoginOK(t, p, "alice", "pw-admin")

	probe := func(token, method, path string) (*httptest.ResponseRecorder, string) {
		var gotRole string
		h := p.withAuth(func(w http.ResponseWriter, r *http.Request) {
			gotRole = roleFrom(r)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "role": gotRole})
		})
		req := httptest.NewRequest(method, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec, gotRole
	}

	if rec, role := probe(viewerTok, http.MethodGet, "/panel/api/overview"); rec.Code != http.StatusOK || role != panelRoleViewer {
		t.Fatalf("viewer context: status=%d role=%q", rec.Code, role)
	}
	if rec, role := probe(adminTok, http.MethodPost, "/panel/api/config"); rec.Code != http.StatusOK || role != panelRoleAdmin {
		t.Fatalf("admin context: status=%d role=%q", rec.Code, role)
	}
	if rec, role := probe("root-key", http.MethodGet, "/panel/api/overview"); rec.Code != http.StatusOK || role != panelRoleAdmin {
		t.Fatalf("api_key context: status=%d role=%q", rec.Code, role)
	}
	// 403 时 handler 不应执行。
	ran := false
	h := p.withAuth(func(http.ResponseWriter, *http.Request) { ran = true })
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/u1/remove", nil)
	req.Header.Set("Authorization", "Bearer "+viewerTok)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusForbidden || ran {
		t.Fatalf("viewer 写操作应 403 且不执行 handler: status=%d ran=%v", rec.Code, ran)
	}
}

// TestRBACCookieSecureFlag HTTPS/反代下 Cookie 加 Secure；HTTP 下不加。
func TestRBACCookieSecureFlag(t *testing.T) {
	p := newRBACPanel(t, func(c *Config) { c.Users = rbacTestUsers() })
	body, _ := json.Marshal(map[string]string{"username": "alice", "password": "pw-admin"})

	// 普通 HTTP：无 Secure。
	rec := rbacRequest(p, http.MethodPost, "/panel/api/auth/login", string(body), nil)
	c := sessionCookieFrom(t, rec)
	if c.Secure {
		t.Fatal("HTTP 登录 Cookie 不应带 Secure")
	}
	// 反代声明 https：带 Secure。
	rec = rbacRequest(p, http.MethodPost, "/panel/api/auth/login", string(body),
		map[string]string{"X-Forwarded-Proto": "https"})
	c = sessionCookieFrom(t, rec)
	if !c.Secure {
		t.Fatal("X-Forwarded-Proto=https 时 Cookie 应带 Secure")
	}
}

// TestRBACEventsEnabledInfo auth/info 透出 SSE 开关（sse.go 包级注入）。
func TestRBACEventsEnabledInfo(t *testing.T) {
	old := EventsEnabled()
	t.Cleanup(func() { SetEventsEnabled(old) })

	p := newRBACPanel(t, nil)
	SetEventsEnabled(true)
	rec := panelRequest(p, http.MethodGet, "/panel/api/auth/info", "root-key", "", nil)
	var info struct {
		EventsEnabled bool `json:"events_enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil || !info.EventsEnabled {
		t.Fatalf("events_enabled=true 透出失败: %+v body=%s", info, rec.Body.String())
	}
	SetEventsEnabled(false)
	rec = panelRequest(p, http.MethodGet, "/panel/api/auth/info", "root-key", "", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil || info.EventsEnabled {
		t.Fatalf("events_enabled=false 透出失败: %+v", info)
	}
}
