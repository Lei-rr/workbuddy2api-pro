// rbac.go 面板多用户 / 角色 / 会话登录（v2.4.0 #6）。
//
// 兼容性原则：**无 users 配置 = 单密钥模式，行为与 v2.3 完全一致**——api_key /
// panel.api_key Bearer 通过即 admin（api_key 为空时沿用「不鉴权」的既有逃生口径），
// 其它一律 401；不启用会话，/auth/login 返回 400。配置了 users 之后：
//   - api_key / panel.api_key Bearer 仍是 root（admin）通道——脚本与应急入口兼容；
//   - 用户名密码登录换取进程内会话令牌（Authorization: Bearer <token> 或
//     wb2_session Cookie 二选一；Cookie HttpOnly/SameSite=Lax，HTTPS 或反代
//     X-Forwarded-Proto=https 时追加 Secure）；
//   - 角色门禁集中放在 withAuth 里（见 panel.go），handler 不需要各自判断：
//     viewer = 只读 GET（+ logout）；operator = GET + 业务写（账号运维/任务/Job/
//     meta 写/usage 保存）；admin = 全部。配置页（读写）/删号/添加账号/导入等仅 admin。
//
// 安全要点：密码比较常量化（明文走 sha256 摘要比较；password_hash 按
// sha256(salt+password) 比较），失败响应不区分「用户不存在」与「密码错误」；
// 会话令牌 32B crypto/rand；密码与令牌绝不写日志，审计 detail 只记用户名/角色/原因。
package panel

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/httpauth"
)

const (
	// 面板角色（由低到高；与 cmd/server panel.users[].role 同一组取值）。
	panelRoleViewer   = "viewer"
	panelRoleOperator = "operator"
	panelRoleAdmin    = "admin"

	// sessionCookieName 会话 Cookie 名（HttpOnly；前端登录也拿 token 走 Bearer）。
	sessionCookieName = "wb2_session"
	// defaultPanelSessionTTL 未配置 panel.session_ttl 时的会话有效期。
	defaultPanelSessionTTL = 12 * time.Hour
	// panelLoginBodyMax 登录请求体上限（{username,password} 很小）。
	panelLoginBodyMax = 4 << 10
	// panelAuditLogin 登录审计 action（成功/失败都记；detail 不含密码）。
	panelAuditLogin = "panel_login"
)

// PanelUser 面板多用户条目（v2.4.0 #6；cmd/server 的 panel.users 由 main 装配时转换，
// 见汇报里的 panelUsersFromCfg 补丁片段）。
//
// 密码二选一：Password 为明文（config.json 应 0600；适合家用单机）或 PasswordHash
// 为 `sha256:<salt>:<hex>`（hex = sha256(salt+password)）。角色由 cmd/server normalize
// 校验为 admin/operator/viewer；本包对未知角色按 viewer（最小权限）兜底。
type PanelUser struct {
	Name         string
	Password     string
	PasswordHash string
	Role         string
}

// panelSession 一条登录会话（进程内存储：面板重启即全部失效，符合「内存会话」设计）。
type panelSession struct {
	Name    string
	Role    string
	Expires time.Time
}

// sessionStore 进程内会话表（token → 会话）。TTL 到期由 lookup/创建时的惰性清扫
// 处理（每分钟至多一次全表扫描）；面板在线用户数很小，无需后台协程，避免测试中
// 每个 Panel 实例泄漏 goroutine。
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]panelSession
	ttl      time.Duration
	lastGC   time.Time
}

// sessionTTLFor 会话有效期：配置值 <=0 回落默认 12h（cmd/server normalize 已 clamp
// 到 5m..7d；面板侧再兜底，避免零值配置导致「登录即过期」）。
func sessionTTLFor(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return defaultPanelSessionTTL
	}
	return ttl
}

// newSessionStore 构建会话表（ttl 参数按 sessionTTLFor 归一）。
func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{
		sessions: map[string]panelSession{},
		ttl:      sessionTTLFor(ttl),
		lastGC:   time.Now(),
	}
}

// create 生成 32 字节 crypto/rand 会话令牌（64 hex），返回令牌与到期时刻。
func (s *sessionStore) create(name, role string) (string, time.Time, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, err
	}
	token := hex.EncodeToString(buf)
	expires := time.Now().Add(s.ttl)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.gcLocked(now)
	s.sessions[token] = panelSession{Name: name, Role: role, Expires: expires}
	return token, expires, nil
}

// lookup 校验会话令牌并返回会话；过期令牌顺手删除。
func (s *sessionStore) lookup(token string) (panelSession, bool) {
	if token == "" {
		return panelSession{}, false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(now)
	sess, ok := s.sessions[token]
	if !ok {
		return panelSession{}, false
	}
	if !now.Before(sess.Expires) {
		delete(s.sessions, token)
		return panelSession{}, false
	}
	return sess, true
}

// delete 删除会话令牌（logout；对 api_key 等非会话令牌是 no-op）。
func (s *sessionStore) delete(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

// gcLocked 惰性清扫过期会话：最多每分钟一次（调用方须持 s.mu）。
func (s *sessionStore) gcLocked(now time.Time) {
	if now.Sub(s.lastGC) < time.Minute {
		return
	}
	s.lastGC = now
	for tok, sess := range s.sessions {
		if !now.Before(sess.Expires) {
			delete(s.sessions, tok)
		}
	}
}

// ---------------------------------------------------------------------------
// 身份 / 角色
// ---------------------------------------------------------------------------

// panelIdentity withAuth 解析出的调用者身份，写入 request context 供 handler 读取。
type panelIdentity struct {
	Name       string // 用户名；api_key 通道为 "api_key"
	Role       string // admin/operator/viewer
	ViaAPIKey  bool   // 经 api_key/panel.api_key Bearer 通过（root 通道）
	ViaSession bool   // 经登录会话（Bearer token 或 Cookie）通过
}

// panelIdentityCtxKey context key 类型（避免与其它包的 key 冲突）。
type panelIdentityCtxKey struct{}

// identityFrom 取当前请求身份（未经 withAuth 的路径返回零值）。
func identityFrom(r *http.Request) panelIdentity {
	if r == nil {
		return panelIdentity{}
	}
	if v, ok := r.Context().Value(panelIdentityCtxKey{}).(panelIdentity); ok {
		return v
	}
	return panelIdentity{}
}

// roleFrom 取当前请求角色（v2.4.0 #6 helper；未经鉴权返回 ""）。
func roleFrom(r *http.Request) string { return identityFrom(r).Role }

// panelRoleRank 角色等级（未知角色按 0 = 无权限）。
func panelRoleRank(role string) int {
	switch role {
	case panelRoleAdmin:
		return 3
	case panelRoleOperator:
		return 2
	case panelRoleViewer:
		return 1
	}
	return 0
}

// normalizePanelRole 未知/空角色按 viewer（最小权限）兜底；cmd/server normalize
// 已把配置里的角色校验为三种取值之一，这里只防御程序化装配/旧配置。
func normalizePanelRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case panelRoleAdmin:
		return panelRoleAdmin
	case panelRoleOperator:
		return panelRoleOperator
	case panelRoleViewer:
		return panelRoleViewer
	}
	return panelRoleViewer
}

// multiUser 是否多用户模式（配置了 users）。
func (p *Panel) multiUser() bool { return len(p.cfg.Users) > 0 }

// sessionTTL 当前生效的会话有效期（配置值；<=0 回落 12h）。
func (p *Panel) sessionTTL() time.Duration { return sessionTTLFor(p.cfg.SessionTTL) }

// panelAdminOnlyPath 管理员专属路径（集中式权限表；读写都限）。
//
// 除任务书列出的「删号/添加账号/导入/配置写」外，这里把 **配置页的 GET** 也收紧为
// admin：GET /panel/api/config 会回显 api_key、webhook 与 panel.users（含口令哈希），
// 只读角色不应看到。另把「重新扫描 auths 目录」也归入 admin：它会按目录增删账号
// （与删号同为危险操作，前端 ROLE_CAPS 也把它标为 danger）。
// 若后续前端确需 viewer 读配置/operator 重扫，可在此放开并在 README 说明。
func panelAdminOnlyPath(path string) bool {
	switch path {
	case "/panel/api/login/start", "/panel/api/import/cockpit", "/panel/api/accounts/rescan":
		return true
	}
	switch {
	case strings.HasPrefix(path, "/panel/api/config"):
		return true // GET/POST /config 与 /config/rollback
	case strings.HasPrefix(path, "/panel/api/accounts/") && strings.HasSuffix(path, "/remove"):
		return true
	}
	return false
}

// panelRoleAllows 集中式路由级权限判定（在 withAuth 内统一执行；handler 不重复判断）。
//
// 规则：
//   - admin：全部允许；
//   - 其余角色：管理员专属路径 → 403；GET/HEAD/OPTIONS → 允许（只读）；
//   - POST /panel/api/auth/logout → 允许（viewer 也要能退出登录）；
//   - 其余写操作 → operator 及以上（viewer 403）。
func panelRoleAllows(role, method, path string) bool {
	if panelRoleRank(role) >= panelRoleRank(panelRoleAdmin) {
		return true
	}
	if panelAdminOnlyPath(path) {
		return false
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if path == "/panel/api/auth/logout" {
		return true
	}
	return panelRoleRank(role) >= panelRoleRank(panelRoleOperator)
}

// authenticatePanelRequest 解析请求凭据并返回身份：
//  1. api_key / panel.api_key Bearer → admin（单密钥与多用户模式都保留该 root 通道）；
//  2. 单密钥模式且 api_key 为空 → 既有「不鉴权」口径（本机/私网部署）；admin；
//  3. 多用户模式 → 会话令牌（Bearer 优先，其次 wb2_session Cookie）。
//
// 失败返回零值,false（调用方 401 并计入失败限速）。
func (p *Panel) authenticatePanelRequest(r *http.Request) (panelIdentity, bool) {
	key := p.apiKey()
	if key != "" {
		if httpauth.VerifyBearer(r, key) {
			return panelIdentity{Name: "api_key", Role: panelRoleAdmin, ViaAPIKey: true}, true
		}
	} else if !p.multiUser() {
		// 单密钥模式未配置密钥 = 既有逃生口径（仅应部署在本机/私网）。
		return panelIdentity{Name: "api_key", Role: panelRoleAdmin, ViaAPIKey: true}, true
	}
	if !p.multiUser() || p.sessions == nil {
		return panelIdentity{}, false
	}
	if tok := bearerTokenValue(r); tok != "" {
		if sess, ok := p.sessions.lookup(tok); ok {
			return panelIdentity{Name: sess.Name, Role: normalizePanelRole(sess.Role), ViaSession: true}, true
		}
	}
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		if sess, ok := p.sessions.lookup(c.Value); ok {
			return panelIdentity{Name: sess.Name, Role: normalizePanelRole(sess.Role), ViaSession: true}, true
		}
	}
	return panelIdentity{}, false
}

// bearerTokenValue 取 `Authorization: Bearer <token>` 的令牌部分（无则 ""）。
// 仅用于「会话令牌」提取；api_key 校验仍走 httpauth.VerifyBearer 的严格口径。
func bearerTokenValue(r *http.Request) string {
	if r == nil {
		return ""
	}
	const prefix = "Bearer "
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, prefix) {
		return ""
	}
	return strings.TrimSpace(authz[len(prefix):])
}

// panelCookieSecure Cookie 是否加 Secure：直连 TLS 或反代声明 X-Forwarded-Proto=https。
func panelCookieSecure(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

// setSessionCookie 写会话 Cookie（HttpOnly/SameSite=Lax/Path=/；HTTPS 时 Secure）。
func (p *Panel) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   panelCookieSecure(r),
		MaxAge:   int(ttl / time.Second),
	})
}

// clearSessionCookie 删除会话 Cookie（MaxAge=-1 + 过期时间，兼容旧浏览器）。
func (p *Panel) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   panelCookieSecure(r),
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

// ---------------------------------------------------------------------------
// 密码校验
// ---------------------------------------------------------------------------

// panelPasswordMatches 校验用户密码（常量时间；不区分「用户不存在/密码错误」由调用方统一）。
//   - Password 非空：明文（config.json 0600），双方取 sha256 摘要后 subtle 比较；
//   - PasswordHash 非空：`sha256:<salt>:<hex>`，hex = sha256(salt+password)；
//   - 两者都空：拒绝登录（cmd/server normalize 禁止，程序化装配兜底）。
func panelPasswordMatches(u PanelUser, password string) bool {
	if u.Password != "" {
		want := sha256.Sum256([]byte(u.Password))
		got := sha256.Sum256([]byte(password))
		return subtle.ConstantTimeCompare(want[:], got[:]) == 1
	}
	if u.PasswordHash != "" {
		salt, wantHex, ok := parsePanelPasswordHash(u.PasswordHash)
		if !ok {
			return false
		}
		got := sha256.Sum256([]byte(salt + password))
		gotHex := hex.EncodeToString(got[:])
		return subtle.ConstantTimeCompare([]byte(gotHex), []byte(wantHex)) == 1
	}
	return false
}

// parsePanelPasswordHash 解析 `sha256:<salt>:<hex>`（salt 不能含冒号；hex 必须 64 位）。
func parsePanelPasswordHash(hash string) (salt, digestHex string, ok bool) {
	parts := strings.Split(strings.TrimSpace(hash), ":")
	if len(parts) != 3 || parts[0] != "sha256" || parts[1] == "" {
		return "", "", false
	}
	digest := strings.ToLower(strings.TrimSpace(parts[2]))
	if len(digest) != sha256.Size*2 {
		return "", "", false
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", "", false
	}
	return parts[1], digest, true
}

// findPanelUser 按用户名精确匹配（TrimSpace；cmd/server 已校重）。
func (p *Panel) findPanelUser(name string) (PanelUser, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return PanelUser{}, false
	}
	for _, u := range p.cfg.Users {
		if strings.TrimSpace(u.Name) == name {
			return u, true
		}
	}
	return PanelUser{}, false
}

// ---------------------------------------------------------------------------
// 端点
// ---------------------------------------------------------------------------

// authLogin POST /panel/api/auth/login：body {username|name, password} 换会话令牌。
//
// 响应 200：{ok:true,user:{name,role},token,expires_at,session_ttl_sec} + Set-Cookie。
// 失败：未配置用户 → 400；用户名/密码错误 → 401（统一文案，不区分用户是否存在，
// 并计入既有 20/min/IP 失败限速）。成功/失败都写 panel_login 审计（不含密码）。
func (p *Panel) authLogin(w http.ResponseWriter, r *http.Request) {
	if len(p.cfg.Users) == 0 {
		writeErr(w, http.StatusBadRequest, "未配置面板用户（单密钥模式请直接使用 api_key 认证）")
		return
	}
	var body struct {
		Username string `json:"username"`
		Name     string `json:"name"` // 兼容 {name,password} 写法
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, panelLoginBodyMax)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	name := strings.TrimSpace(body.Username)
	if name == "" {
		name = strings.TrimSpace(body.Name)
	}
	ip := rateLimitIP(r)
	user, found := p.findPanelUser(name)
	if !found || !panelPasswordMatches(user, body.Password) {
		// 统一失败响应：不泄露用户名是否存在；计入失败限速（ServeHTTP 前置超限后 429）。
		if p.authFailures != nil {
			p.authFailures.fail(ip)
		}
		p.audit(r, panelAuditLogin, "", false, "登录失败 user="+truncateRunes(name, 64))
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	role := normalizePanelRole(user.Role)
	if p.sessions == nil { // 防御零值 Panel（正常经 New 构造不会走到）。
		p.sessions = newSessionStore(p.sessionTTL())
	}
	token, expires, err := p.sessions.create(user.Name, role)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "创建会话失败")
		return
	}
	if p.authFailures != nil {
		p.authFailures.success(ip) // 登录成功清失败计数（与 withAuth 成功同口径）
	}
	p.setSessionCookie(w, r, token, p.sessionTTL())
	p.audit(r, panelAuditLogin, "", true, "user="+user.Name+" role="+role)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"user":            map[string]any{"name": user.Name, "role": role},
		"token":           token,
		"expires_at":      expires.Unix(),
		"session_ttl_sec": int(p.sessionTTL().Seconds()),
	})
}

// authLogout POST /panel/api/auth/logout：删除当前会话（Bearer 令牌与 Cookie 都尝试
// 作废）并清 Cookie。api_key 通道调用时无会话可删，仍返回 200（幂等）。
func (p *Panel) authLogout(w http.ResponseWriter, r *http.Request) {
	if p.sessions != nil {
		if tok := bearerTokenValue(r); tok != "" {
			p.sessions.delete(tok)
		}
		if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
			p.sessions.delete(c.Value)
		}
	}
	p.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// authInfo GET /panel/api/auth/info：当前模式/身份/认证方式（前端据此渲染登录 UI
// 与「已登录用户」；单密钥模式沿用密钥登录，mode="single"）。
//
//	{ok:true,mode:"single"|"multi",user:{name,role}|null,
//	 auth:{api_key:bool,session:bool},events_enabled:bool,session_ttl_sec:int}
//
// user 仅在会话登录时非 null；api_key 通道用 auth.api_key=true 表达（mode=single
// 时前端走密钥流程，不显示用户）。
func (p *Panel) authInfo(w http.ResponseWriter, r *http.Request) {
	ident := identityFrom(r)
	mode := "single"
	if p.multiUser() {
		mode = "multi"
	}
	var user any
	if ident.ViaSession {
		user = map[string]any{"name": ident.Name, "role": ident.Role}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"mode":            mode,
		"user":            user,
		"auth":            map[string]any{"api_key": ident.ViaAPIKey, "session": ident.ViaSession},
		"events_enabled":  EventsEnabled(), // v2.4.0 #3 SSE 开关（sse.go 包级注入）
		"session_ttl_sec": int(p.sessionTTL().Seconds()),
	})
}
