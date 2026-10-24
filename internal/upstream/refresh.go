package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
)

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。全程持 a 锁，防止并发 SaveAtomic 读半更新 token。
// refreshIOTimeout 刷新端点网络 I/O 上限（两段式锁外执行，防上游 hang 长占锁）。
const refreshIOTimeout = 30 * time.Second

// refreshTokenExpiresInMax refresh 响应 expiresIn 的量级上限（10 年，纯防御值：
// 实测 R-D 响应恒 5184000=60d）。超限视为上游脏数据，不写 ExpiresAt（保留旧值），
// 防止 NeedsRefresh 永假导致 token 永不刷新反而真过期失效。
const refreshTokenExpiresInMax = 10 * 365 * 24 * time.Hour

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。
//
// 并发安全模型（单飞 + 两段式）：
//   - **单飞**：同一账号（UID）同一时刻只允许一个 refresh 在途，其余调用等待并
//     原样复用首个结果。上游对 refreshToken 有反重放：RT 被消费后再用会撞 12153
//     （refresh token is invalid）并把整条 RT 链作废，账号从此恒 401、只能人工
//     重新登录（issue #215：scheduler keepalive / handler 临近过期刷新 / signin
//     会并发触发同一账号）。所以单飞在这里是保号手段，而不只是省流量。
//   - 首个调用内部仍是两段式：锁内读快照 → 锁外 I/O（30s）→ 锁内校验快照一致后
//     写回；写回前若发现另一刷新已完成（两 token 同时变化）则直接采用。
//   - 12153 兜底：若磁盘 auth 文件已被其他进程（server + signin 双进程部署）刷新
//     过，则采纳磁盘 token 重试一次——进程内单飞挡不住跨进程竞态。
func (c *Client) RefreshToken(a *auth.Auth) error {
	key := a.UID
	if key == "" {
		// 无 UID 的临时 auth：退化为按对象指针去重，仍保证同一对象串行。
		key = fmt.Sprintf("%p", a)
	}
	c.refreshMu.Lock()
	if c.refreshInFlight == nil {
		c.refreshInFlight = make(map[string]*refreshFlight)
	}
	if f, ok := c.refreshInFlight[key]; ok {
		c.refreshMu.Unlock()
		<-f.done
		return f.err
	}
	f := &refreshFlight{done: make(chan struct{})}
	c.refreshInFlight[key] = f
	c.refreshMu.Unlock()

	err := c.refreshTokenOnce(a)
	if err != nil && isSessionDead(err) && adoptNewerTokensFromDisk(a) {
		// 跨进程竞态：磁盘 RT 已被别的进程 rotate 过，用新 RT 再试一次。
		err = c.refreshTokenOnce(a)
	}

	// 先写结果再关 channel：等待者 <-f.done 之后读到的一定是最终 err。
	f.err = err
	close(f.done)
	c.refreshMu.Lock()
	delete(c.refreshInFlight, key)
	c.refreshMu.Unlock()
	return err
}

// refreshFlight 是同一账号一次 refresh 的单飞票据：done 关闭即结果就绪。
type refreshFlight struct {
	done chan struct{}
	err  error
}

// isSessionDead 判定错误是否为上游 12153（会话 / RT 失效）。
func isSessionDead(err error) bool {
	var ue *Error
	return errors.As(err, &ue) && ue.Kind == ErrSessionDead
}

// adoptNewerTokensFromDisk 在 refresh 失败后读回 auth 文件：若磁盘 RT 与内存不同
// （另一进程已刷新并写回），采纳磁盘 token 并返回 true，供调用方重试一次。
// 读文件失败、解析失败、RT 一致均返回 false（不改内存状态），保持失败语义。
func adoptNewerTokensFromDisk(a *auth.Auth) bool {
	if a.FilePath == "" {
		return false
	}
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		return false
	}
	disk, err := auth.Parse(raw)
	if err != nil || strings.TrimSpace(disk.RefreshToken) == "" {
		return false
	}
	a.Lock()
	defer a.Unlock()
	if disk.RefreshToken == a.RefreshToken {
		return false
	}
	a.AccessToken = disk.AccessToken
	a.RefreshToken = disk.RefreshToken
	if disk.ExpiresAt > 0 {
		a.ExpiresAt = disk.ExpiresAt
	}
	return true
}

// refreshTokenOnce 是单次刷新本体（调用方 RefreshToken 已保证同账号串行）。
func (c *Client) refreshTokenOnce(a *auth.Auth) error {
	// 第 1 段（锁内）：读快照。
	a.Lock()
	rtSnapshot := a.RefreshToken
	atBefore := a.AccessToken
	a.Unlock()
	if strings.TrimSpace(rtSnapshot) == "" {
		return fmt.Errorf("no refreshToken")
	}

	endpoint := c.chatBase(a) + "/v2/plugin/auth/token/refresh"
	ctx, cancel := context.WithTimeout(context.Background(), refreshIOTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	// RefreshHeaders 读取 a 的字段（domain/uid 等）注入请求头——需在锁内取快照值，
	// 用一个显式逐字段拷贝的临时 auth 构造头（不拷贝 sync.Mutex，避免 vet copies-lock）。
	a.Lock()
	hdrSnapshot := auth.Auth{
		AccessToken:  a.AccessToken,
		RefreshToken: rtSnapshot,
		ExpiresAt:    a.ExpiresAt,
		Domain:       a.Domain,
		UID:          a.UID,
		EnterpriseID: a.EnterpriseID,
		Nickname:     a.Nickname,
		DeviceToken:  a.DeviceToken,
	}
	a.Unlock()
	c.RefreshHeaders(req, &hdrSnapshot)

	// 网络 I/O（锁外，30s 上限）。
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}

	// 第 2 段（锁内）：校验快照一致后写回。
	a.Lock()
	defer a.Unlock()
	// 写回守卫是 AND 语义：锁外期间另一刷新已完成 → 两 token 必同时变化（实测 R-D：
	// refresh 响应 accessToken/refreshToken 总是一起 rotate，写回也同时写两个），AND
	// 即「并发刷新已完成」判据；AND 与 OR 在真实形态下等价。唯 OR 会额外放弃的
	// 「只有单 token 变化」（如手工只改 auth 文件一个字段）不构成放弃条件——本次
	// 结果覆盖手工编辑。
	if a.AccessToken != atBefore && a.RefreshToken != rtSnapshot {
		// 锁外期间另一 goroutine 已完成刷新：新 token 已生效，本次结果不必再写
		// （实测 R-E：服务端无 rotation 撤销，并发双刷新拿到的两个新 token 都有效，
		// 后写覆盖先写二者等价可用；提前返回避免无意义覆盖与 ExpiresAt 抖动）。
		return nil
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	// 实测 R-D 响应恒带 expiresIn=5184000（60d）——缺省分支仅为防御，保留旧值
	// 避免过期判定漂移。同理，超过 10 年的 expiresIn 按脏值处理保留旧值：
	// 实测 JWT exp-iat 与 expiresIn 严格自洽（R-F），超量级值只会是上游脏数据，
	// 照写会把 ExpiresAt 推到荒谬未来 → NeedsRefresh 永假 → token 永不刷新
	// 反而真过期失效。
	if tok.ExpiresIn > 0 && time.Duration(tok.ExpiresIn)*time.Second < refreshTokenExpiresInMax {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}
