package notify

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// capServer 记录请求体/头，并按回调返回自定义响应（渠道业务级判定测试用）。
type capServer struct {
	mu     sync.Mutex
	bodies [][]byte
	hdrs   []http.Header
	respFn func(idx int, body []byte) (status int, respBody string)
}

func newCapServer(t *testing.T, respFn func(idx int, body []byte) (int, string)) (*capServer, *httptest.Server) {
	t.Helper()
	c := &capServer{respFn: respFn}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		idx := len(c.bodies)
		c.bodies = append(c.bodies, body)
		c.hdrs = append(c.hdrs, r.Header.Clone())
		fn := c.respFn
		c.mu.Unlock()
		status, resp := http.StatusOK, "{}"
		if fn != nil {
			status, resp = fn(idx, body)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return c, srv
}

func (c *capServer) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func (c *capServer) lastBody(t *testing.T) []byte {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatal("目标服务没有收到任何请求")
	}
	return append([]byte(nil), c.bodies[len(c.bodies)-1]...)
}

func (c *capServer) lastHeader(t *testing.T) http.Header {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.hdrs) == 0 {
		t.Fatal("目标服务没有收到任何请求")
	}
	return c.hdrs[len(c.hdrs)-1].Clone()
}

// TestNewFromConfigChannelSelection channel 选择适配器；空 channel 默认 webhook。
func TestNewFromConfigChannelSelection(t *testing.T) {
	_, srv := newCapServer(t, nil)
	cases := []struct {
		channel  string
		chatID   string
		wantChan string
	}{
		{"", "", ChannelWebhook},
		{"webhook", "", ChannelWebhook},
		{"wecom", "", ChannelWecom},
		{"telegram", "-100123", ChannelTelegram},
		{"bark", "", ChannelBark},
		{"WEBHOOK", "", ChannelWebhook}, // 大小写不敏感
		{" Telegram ", "-100123", ChannelTelegram},
	}
	for _, tc := range cases {
		n := NewFromConfig(Config{
			WebhookURL:     srv.URL,
			Timeout:        time.Second,
			Channel:        tc.channel,
			TelegramChatID: tc.chatID,
			Title:          "T",
		})
		if !n.Enabled() {
			t.Fatalf("channel=%q 应启用", tc.channel)
		}
		if got := n.adapter.channel(); got != tc.wantChan {
			t.Errorf("channel=%q adapter=%q want %q", tc.channel, got, tc.wantChan)
		}
		if n.cfg.Title != "T" && tc.channel != "" {
			t.Errorf("channel=%q title=%q want T", tc.channel, n.cfg.Title)
		}
		closeNotifier(t, n)
	}
}

// TestNewFromConfigDefensiveDisabled 防御性关闭：未知渠道 / telegram 缺 chat_id /
// 空 URL / 非法 URL 都返回关闭实例（config.normalize 已 fail fast，这里是兜底）。
func TestNewFromConfigDefensiveDisabled(t *testing.T) {
	_, srv := newCapServer(t, nil)
	cases := []struct {
		name string
		cfg  Config
	}{
		{"invalid channel", Config{WebhookURL: srv.URL, Channel: "slack"}},
		{"telegram missing chat id", Config{WebhookURL: srv.URL, Channel: "telegram"}},
		{"telegram blank chat id", Config{WebhookURL: srv.URL, Channel: "telegram", TelegramChatID: "   "}},
		{"wecom empty url", Config{WebhookURL: "", Channel: "wecom"}},
		{"webhook empty url", Config{WebhookURL: "", Channel: ""}},
		{"bark invalid url", Config{WebhookURL: "ftp://example.com/key", Channel: "bark"}},
		{"telegram invalid url", Config{WebhookURL: "/botTOKEN/sendMessage", Channel: "telegram", TelegramChatID: "1"}},
	}
	for _, tc := range cases {
		n := NewFromConfig(tc.cfg)
		if n.Enabled() {
			t.Fatalf("%s: 应关闭通知", tc.name)
		}
		n.JobDone(map[string]any{"id": "j1"}) // no-op 不 panic
		if err := n.Close(nil); err != nil {
			t.Fatalf("%s: Close=%v", tc.name, err)
		}
	}
}

// TestWebhookChannelLegacyPayloadFromConfig NewFromConfig(channel=webhook) 与旧 New
// 完全同构：payload 仍是 {"event","ts","data"} 三字段（向后兼容）。
func TestWebhookChannelLegacyPayloadFromConfig(t *testing.T) {
	rec := &webhookRecorder{}
	srv := newWebhook(t, rec)
	n := NewFromConfig(Config{WebhookURL: srv.URL, Timeout: 2 * time.Second})
	t.Cleanup(func() { closeNotifier(t, n) })

	n.JobDone(map[string]any{"id": "j1", "total": 3})
	waitFor(t, 3*time.Second, "webhook 收到事件", func() bool { return rec.count() == 1 })
	bodies, _ := rec.snapshot()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(bodies[0], &top); err != nil {
		t.Fatal(err)
	}
	if len(top) != 3 {
		t.Fatalf("webhook payload 顶层字段=%v want event/ts/data", keysOf(top))
	}
	p := decodePayload(t, bodies[0])
	if p.Event != EventJobDone || p.Data["id"] != "j1" {
		t.Fatalf("payload=%+v", p)
	}
}

// TestWecomPayloadAndSuccess 企业微信：markdown body、标题前缀、概要可读、2xx+errcode=0 成功。
func TestWecomPayloadAndSuccess(t *testing.T) {
	cap, srv := newCapServer(t, func(int, []byte) (int, string) {
		return http.StatusOK, `{"errcode":0,"errmsg":"ok"}`
	})
	n := NewFromConfig(Config{
		WebhookURL: srv.URL,
		Timeout:    2 * time.Second,
		Channel:    ChannelWecom,
		Title:      "WB Pro 告警",
	})
	t.Cleanup(func() { closeNotifier(t, n) })

	n.JobDone(map[string]any{"kind": "checkin", "total": 120, "done": 100, "fail": 3, "skip": 5})
	waitFor(t, 3*time.Second, "wecom 发送成功", func() bool { return n.Stats().Sent == 1 })
	if got := cap.count(); got != 1 {
		t.Fatalf("wecom 请求数=%d want 1", got)
	}

	var msg wecomMessage
	if err := json.Unmarshal(cap.lastBody(t), &msg); err != nil {
		t.Fatalf("wecom body 解析失败: %v", err)
	}
	if msg.MsgType != "markdown" {
		t.Errorf("msgtype=%q want markdown", msg.MsgType)
	}
	content := msg.Markdown.Content
	if !strings.HasPrefix(content, "WB Pro 告警\n") {
		t.Errorf("content 应以标题+换行开头: %q", content)
	}
	for _, want := range []string{"[job_done]", "checkin", "完成 100/120", "失败 3", "跳过 5"} {
		if !strings.Contains(content, want) {
			t.Errorf("wecom content 缺少 %q: %q", want, content)
		}
	}
	h := cap.lastHeader(t)
	if ct := h.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q", ct)
	}
	if ua := h.Get("User-Agent"); ua != userAgent {
		t.Errorf("User-Agent=%q want %q", ua, userAgent)
	}
	// 渠道消息只含概要，不含凭证痕迹。
	for _, banned := range []string{"access_token", "Authorization", "api_key", "Bearer"} {
		if strings.Contains(string(cap.lastBody(t)), banned) {
			t.Errorf("wecom body 不应含凭证痕迹 %q", banned)
		}
	}
}

// TestWecomErrcodeFailure 企业微信 HTTP 200 但 errcode!=0：按失败处理（重试一次后 WARN）。
func TestWecomErrcodeFailure(t *testing.T) {
	cap, srv := newCapServer(t, func(int, []byte) (int, string) {
		return http.StatusOK, `{"errcode":93000,"errmsg":"invalid webhook url"}`
	})
	n := NewFromConfig(Config{WebhookURL: srv.URL, Timeout: 2 * time.Second, Channel: ChannelWecom})
	t.Cleanup(func() { closeNotifier(t, n) })

	n.JobDone(map[string]any{"id": "j1"})
	waitFor(t, 5*time.Second, "errcode 非 0 视为失败并重试", func() bool {
		return n.Stats().Failed == 1 && cap.count() == maxAttempts
	})
	if s := n.Stats(); s.Sent != 0 {
		t.Fatalf("stats=%+v want sent=0", s)
	}
}

// TestTelegramPayloadAndSuccess telegram：chat_id/text/disable_web_page_preview，2xx+ok:true 成功。
func TestTelegramPayloadAndSuccess(t *testing.T) {
	const botToken = "123456:AAHsecretTOKEN"
	cap, srv := newCapServer(t, func(int, []byte) (int, string) {
		return http.StatusOK, `{"ok":true,"result":{"message_id":1}}`
	})
	// 完整 URL 形态与真实 Bot API 一致（token 在 path，绝不能出现在日志/payload 里）。
	url := srv.URL + "/bot" + botToken + "/sendMessage"
	n := NewFromConfig(Config{
		WebhookURL:     url,
		Timeout:        2 * time.Second,
		Channel:        ChannelTelegram,
		TelegramChatID: "-100123456",
		Title:          "WB 通知",
	})
	t.Cleanup(func() { closeNotifier(t, n) })

	n.AccountDisabled("248890d9", "session_dead x3")
	waitFor(t, 3*time.Second, "telegram 发送成功", func() bool { return n.Stats().Sent == 1 })

	var msg telegramMessage
	body := cap.lastBody(t)
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("telegram body 解析失败: %v", err)
	}
	if msg.ChatID != "-100123456" {
		t.Errorf("chat_id=%q", msg.ChatID)
	}
	if !msg.DisableWebPagePreview {
		t.Error("disable_web_page_preview 应为 true")
	}
	if !strings.HasPrefix(msg.Text, "WB 通知\n") || !strings.Contains(msg.Text, "[account_disabled] 账号 248890d9 被禁用：session_dead x3") {
		t.Errorf("telegram text 不符合预期: %q", msg.Text)
	}
	// token 只在 URL 里，不得进入 body。
	if strings.Contains(string(body), botToken) {
		t.Fatalf("telegram body 泄露 bot token: %s", body)
	}
}

// TestTelegramOKFalseFails telegram 2xx 但 ok:false：按失败处理（重试一次）。
func TestTelegramOKFalseFails(t *testing.T) {
	cap, srv := newCapServer(t, func(int, []byte) (int, string) {
		return http.StatusOK, `{"ok":false,"description":"Bad Request: chat not found"}`
	})
	n := NewFromConfig(Config{
		WebhookURL:     srv.URL + "/botTOKEN/sendMessage",
		Timeout:        2 * time.Second,
		Channel:        ChannelTelegram,
		TelegramChatID: "1",
	})
	t.Cleanup(func() { closeNotifier(t, n) })

	n.CheckinFailed("uid8", "HTTP 500")
	waitFor(t, 5*time.Second, "ok=false 视为失败并重试", func() bool {
		return n.Stats().Failed == 1 && cap.count() == maxAttempts
	})
}

// TestBarkPayloadAndSuccess bark：title/body/group；2xx 即成功；设备 key 只在 URL。
func TestBarkPayloadAndSuccess(t *testing.T) {
	const deviceKey = "DEVICEKEYsecret123"
	cap, srv := newCapServer(t, func(int, []byte) (int, string) {
		return http.StatusOK, `{"code":200,"message":"success"}`
	})
	n := NewFromConfig(Config{
		WebhookURL: srv.URL + "/" + deviceKey,
		Timeout:    2 * time.Second,
		Channel:    ChannelBark,
		Title:      "WB Bark",
	})
	t.Cleanup(func() { closeNotifier(t, n) })

	n.CheckinFailed("248890d9", "upstream 502")
	waitFor(t, 3*time.Second, "bark 发送成功", func() bool { return n.Stats().Sent == 1 })

	var msg barkMessage
	body := cap.lastBody(t)
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("bark body 解析失败: %v", err)
	}
	if msg.Title != "WB Bark" {
		t.Errorf("title=%q", msg.Title)
	}
	if msg.Group != barkGroup {
		t.Errorf("group=%q want %q", msg.Group, barkGroup)
	}
	if !strings.Contains(msg.Body, "[checkin_failed] 账号 248890d9 签到失败：upstream 502") {
		t.Errorf("bark body=%q", msg.Body)
	}
	if strings.Contains(string(body), deviceKey) {
		t.Fatalf("bark body 泄露设备 key: %s", body)
	}
}

// TestChannelLogMasking 多渠道路径的日志脱敏：telegram/bark/wecom 的 path 常含
// token/key，日志只允许出现 scheme://host/***。
func TestChannelLogMasking(t *testing.T) {
	var buf bytes.Buffer
	oldOut := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(oldOut)

	_, srv := newCapServer(t, nil)
	cases := []struct {
		channel string
		chatID  string
		url     string
		secret  string
	}{
		{ChannelTelegram, "1", srv.URL + "/botSECRET-TOKEN/sendMessage", "SECRET-TOKEN"},
		{ChannelBark, "", srv.URL + "/SECRET-DEVICE-KEY", "SECRET-DEVICE-KEY"},
		{ChannelWecom, "", srv.URL + "/cgi-bin/webhook/send?key=SECRET-WECOM-KEY", "SECRET-WECOM-KEY"},
	}
	for _, tc := range cases {
		n := NewFromConfig(Config{
			WebhookURL:     tc.url,
			Timeout:        time.Second,
			Channel:        tc.channel,
			TelegramChatID: tc.chatID,
		})
		if !n.Enabled() {
			t.Fatalf("%s 应启用", tc.channel)
		}
		closeNotifier(t, n)
	}

	logs := buf.String()
	for _, banned := range []string{"SECRET-TOKEN", "SECRET-DEVICE-KEY", "SECRET-WECOM-KEY"} {
		if strings.Contains(logs, banned) {
			t.Fatalf("日志泄露渠道密钥 %q:\n%s", banned, logs)
		}
	}
	for _, want := range []string{"channel=telegram", "channel=bark", "channel=wecom", "/***"} {
		if !strings.Contains(logs, want) {
			t.Errorf("日志缺少 %q:\n%s", want, logs)
		}
	}
}

// TestMaskURLByChannel 脱敏策略：webhook 保留 path 前 12 字符（v2.3.0 兼容）；
// wecom/telegram/bark 只保留 scheme://host。
func TestMaskURLByChannel(t *testing.T) {
	cases := []struct {
		channel string
		raw     string
		want    string
	}{
		{ChannelWebhook, "https://hooks.example.com/services/T000/B000", "https://hooks.example.com/services/T0…"},
		{ChannelTelegram, "https://api.telegram.org/bot123:AAHxyz/sendMessage", "https://api.telegram.org/***"},
		{ChannelBark, "https://api.day.app/DEVICEKEY/title/body", "https://api.day.app/***"},
		{ChannelWecom, "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=SECRET", "https://qyapi.weixin.qq.com/***"},
		{ChannelBark, "not a url", "invalid-url"},
	}
	for _, tc := range cases {
		if got := maskURL(tc.channel, tc.raw); got != tc.want {
			t.Errorf("maskURL(%q, %q)=%q want %q", tc.channel, tc.raw, got, tc.want)
		}
	}
}

// TestSummaryFormatting 概要格式化：三件套可读文本、未知事件 JSON 摘要、2KB 截断。
func TestSummaryFormatting(t *testing.T) {
	// 已知事件：与提案示例同口径。
	if got, want := formatSummary(EventJobDone, map[string]any{
		"kind": "checkin", "total": 120, "done": 100, "fail": 3, "skip": 5,
	}), "[job_done] checkin 完成 100/120 失败 3 跳过 5"; got != want {
		t.Errorf("job_done summary=%q want %q", got, want)
	}
	// 数值类型兼容：JSON 往返的 float64 / 字符串数字 / int64。
	for _, data := range []map[string]any{
		{"kind": "checkin", "total": float64(120), "done": float64(100), "fail": float64(3)},
		{"kind": "checkin", "total": "120", "done": "100", "fail": "3"},
		{"kind": "checkin", "total": int64(120), "done": int64(100), "fail": int64(3)},
	} {
		if got := formatSummary(EventJobDone, data); !strings.Contains(got, "完成 100/120 失败 3") {
			t.Errorf("数值兼容 summary=%q (data=%v)", got, data)
		}
	}
	if got := formatSummary(EventJobDone, map[string]any{"id": "j1"}); got != "[job_done] 已结束 [j1]" {
		t.Errorf("minimal job_done summary=%q", got)
	}
	if got, want := formatSummary(EventAccountDisabled, map[string]any{
		"uid": "248890d9", "reason": "session_dead x3",
	}), "[account_disabled] 账号 248890d9 被禁用：session_dead x3"; got != want {
		t.Errorf("account_disabled summary=%q want %q", got, want)
	}
	if got := formatSummary(EventAccountDisabled, map[string]any{}); got != "[account_disabled] 账号 ? 被禁用" {
		t.Errorf("empty account_disabled summary=%q", got)
	}
	if got, want := formatSummary(EventCheckinFailed, map[string]any{
		"uid": "u1", "error": "HTTP 500\nstack line",
	}), "[checkin_failed] 账号 u1 签到失败：HTTP 500 stack line"; got != want {
		t.Errorf("checkin_failed summary=%q want %q", got, want)
	}
	// 未知事件：event + JSON 摘要，JSON 部分 ≤500 字符。
	got := formatSummary("custom_event", map[string]any{"a": 1, "b": "x"})
	if want := `[custom_event] {"a":1,"b":"x"}`; got != want {
		t.Errorf("unknown event summary=%q want %q", got, want)
	}
	// 超长未知数据：整体 ≤2KB 且 UTF-8 合法。
	got = formatSummary("custom_event", map[string]any{"blob": strings.Repeat("汉", 2000)})
	if len(got) > maxSummaryBytes || !utf8.ValidString(got) {
		t.Fatalf("超长 summary 截断异常：len=%d utf8=%v", len(got), utf8.ValidString(got))
	}
	// 超长错误文案：checkin_failed 整体 ≤2KB、可合法展示。
	got = formatSummary(EventCheckinFailed, map[string]any{"uid": "u1", "error": strings.Repeat("x", 5000)})
	if len(got) > maxSummaryBytes || !utf8.ValidString(got) {
		t.Fatalf("checkin_failed 截断异常：len=%d", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("截断后应带省略号: %q", got[max(0, len(got)-10):])
	}
}

// TestFormatChannelMessage 消息正文 = 标题 + 换行 + 概要；空标题回退默认标题。
func TestFormatChannelMessage(t *testing.T) {
	data := map[string]any{"uid": "u1", "reason": "r"}
	if got := formatChannelMessage("", EventAccountDisabled, data); !strings.HasPrefix(got, defaultTitle+"\n") {
		t.Errorf("空标题应回退默认标题: %q", got)
	}
	if got := formatChannelMessage("我的标题", EventAccountDisabled, data); !strings.HasPrefix(got, "我的标题\n[account_disabled]") {
		t.Errorf("标题前缀异常: %q", got)
	}
}
