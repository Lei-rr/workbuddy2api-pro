// channel.go 通知渠道适配器（v2.3.1 #19 扩展）：
//
//	webhook  — 通用 JSON {"event","ts","data"}（v2.3.0 兼容，不改语义）
//	wecom    — 企业微信群机器人 markdown（成功判定：2xx + body errcode==0）
//	telegram — Bot sendMessage 纯文本（成功判定：2xx + body ok==true）
//	bark     — iOS Bark 推送 JSON（成功判定：2xx）
//
// 每个适配器负责三件事：出站 body 编码、目标 URL、渠道级成功判定（checkResponse）。
// 渠道的 URL 脱敏策略也在此处（wecom/telegram/bark 的 path 常直接携带 key/token，
// 只保留 scheme://host，绝不把 key 打进日志）。
package notify

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// 渠道名（与 config.json 的 notify.channel 取值一致；大小写不敏感，NewFromConfig 归一化）。
const (
	ChannelWebhook  = "webhook"
	ChannelWecom    = "wecom"
	ChannelTelegram = "telegram"
	ChannelBark     = "bark"
)

const (
	// defaultTitle 未配置 notify.title 时的消息标题。
	defaultTitle = "WorkBuddy2API Pro"
	// barkGroup Bark 客户端里的消息分组名（固定值，便于在通知中心归类）。
	barkGroup = "workbuddy"
)

// channelAdapter 渠道适配器接口。
type channelAdapter interface {
	// channel 渠道名（日志/统计用）。
	channel() string
	// url 出站目标 URL（不做脱敏，仅供 HTTP 请求）。
	url() string
	// target 日志用脱敏目标（见 maskURL）。
	target() string
	// buildBody 把一条事件编码为该渠道的 HTTP body。
	buildBody(event string, ts int64, data map[string]any) ([]byte, error)
	// checkResponse 在 HTTP 2xx 之后做渠道业务级成功判定；nil = 成功。
	checkResponse(body []byte) error
}

// ---------------------------------------------------------------------------
// webhook：通用 JSON（v2.3.0 行为，向后兼容）
// ---------------------------------------------------------------------------

type webhookAdapter struct{ u string }

func (a webhookAdapter) channel() string { return ChannelWebhook }
func (a webhookAdapter) url() string     { return a.u }
func (a webhookAdapter) target() string  { return maskURL(ChannelWebhook, a.u) }

// buildBody 与 v2.3.0 完全一致：{"event","ts","data"}（data >64KB 降级为摘要）。
func (a webhookAdapter) buildBody(event string, ts int64, data map[string]any) ([]byte, error) {
	return buildPayload(event, ts, data)
}

// checkResponse 通用 webhook 只认 2xx（body 交给消费方，不做业务判定）。
func (a webhookAdapter) checkResponse([]byte) error { return nil }

// ---------------------------------------------------------------------------
// wecom：企业微信群机器人（markdown）
// ---------------------------------------------------------------------------

type wecomAdapter struct {
	u     string
	title string
}

func (a wecomAdapter) channel() string { return ChannelWecom }
func (a wecomAdapter) url() string     { return a.u }
func (a wecomAdapter) target() string  { return maskURL(ChannelWecom, a.u) }

// wecomMessage 企业微信机器人 markdown 消息体（只用到 msgtype=markdown）。
type wecomMessage struct {
	MsgType  string `json:"msgtype"`
	Markdown struct {
		Content string `json:"content"`
	} `json:"markdown"`
}

func (a wecomAdapter) buildBody(event string, ts int64, data map[string]any) ([]byte, error) {
	var msg wecomMessage
	msg.MsgType = "markdown"
	msg.Markdown.Content = formatChannelMessage(a.title, event, data)
	return json.Marshal(msg)
}

// wecomResponse 企业微信返回 {errcode, errmsg}；errcode==0 才算投递成功。
type wecomResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

func (a wecomAdapter) checkResponse(body []byte) error {
	var resp wecomResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("wecom 响应解析失败: %w", err)
	}
	if resp.ErrCode != 0 {
		return fmt.Errorf("wecom errcode=%d: %s", resp.ErrCode, truncateText(resp.ErrMsg, 200))
	}
	return nil
}

// ---------------------------------------------------------------------------
// telegram：Bot API sendMessage
// ---------------------------------------------------------------------------

type telegramAdapter struct {
	u      string
	chatID string
	title  string
}

func (a telegramAdapter) channel() string { return ChannelTelegram }
func (a telegramAdapter) url() string     { return a.u }
func (a telegramAdapter) target() string  { return maskURL(ChannelTelegram, a.u) }

// telegramMessage Telegram sendMessage 请求体（纯文本 + 关闭链接预览）。
type telegramMessage struct {
	ChatID                string `json:"chat_id"`
	Text                  string `json:"text"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
}

func (a telegramAdapter) buildBody(event string, ts int64, data map[string]any) ([]byte, error) {
	return json.Marshal(telegramMessage{
		ChatID:                a.chatID,
		Text:                  formatChannelMessage(a.title, event, data),
		DisableWebPagePreview: true,
	})
}

// telegramResponse Bot API 返回 {ok, description}；ok==true 才算投递成功。
type telegramResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

func (a telegramAdapter) checkResponse(body []byte) error {
	var resp telegramResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("telegram 响应解析失败: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("telegram ok=false: %s", truncateText(resp.Description, 200))
	}
	return nil
}

// ---------------------------------------------------------------------------
// bark：iOS Bark 推送
// ---------------------------------------------------------------------------

type barkAdapter struct {
	u     string
	title string
}

func (a barkAdapter) channel() string { return ChannelBark }
func (a barkAdapter) url() string     { return a.u }
func (a barkAdapter) target() string  { return maskURL(ChannelBark, a.u) }

// barkMessage Bark 推送请求体（title/body/group；group 固定为 workbuddy 便于归类）。
type barkMessage struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Group string `json:"group"`
}

func (a barkAdapter) buildBody(event string, ts int64, data map[string]any) ([]byte, error) {
	return json.Marshal(barkMessage{
		Title: a.title,
		Body:  formatSummary(event, data),
		Group: barkGroup,
	})
}

// checkResponse Bark 以 HTTP 状态码为准（200 即成功，响应体为 {code,message} 可选）。
func (a barkAdapter) checkResponse([]byte) error { return nil }

// ---------------------------------------------------------------------------
// 公共辅助
// ---------------------------------------------------------------------------

// parseHTTPURL 校验目标 URL：必须是带 host 的 http/https 地址（拒绝 file:// 等）。
func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("非 http(s) 地址或缺少 host")
	}
	return u, nil
}

// maskURL 按渠道脱敏日志目标：
//   - webhook：保持 v2.3.0 语义（scheme://host + path 前 12 字符，见 maskWebhookURL）；
//   - wecom/telegram/bark：path 里通常直接是 webhook key / bot token / 设备 key，
//     只保留 scheme://host，绝不打印 path/query。
func maskURL(channel, raw string) string {
	switch channel {
	case ChannelWecom, ChannelTelegram, ChannelBark:
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" {
			return "invalid-url"
		}
		return u.Scheme + "://" + u.Host + "/***"
	default:
		return maskWebhookURL(raw)
	}
}
