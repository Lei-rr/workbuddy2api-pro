// summary.go 事件概要格式化（v2.3.1 #19）。
//
// 纯文本渠道（wecom markdown / telegram text / bark body）不发送原始 JSON，而是把
// event + data 关键字段拼成一条可读中文概要：
//
//	[job_done] checkin 完成 100/120 失败 3 跳过 5
//	[account_disabled] 账号 248890d9 被禁用：session_dead x3
//	[checkin_failed] 账号 248890d9 签到失败：HTTP 500
//
// 未知事件回退为 "[event] {JSON 摘要}"（JSON 部分 ≤500 字符）。所有概要总长 ≤2KB
// （UTF-8 安全截断），避免上游错误文案/超大 data 撑爆渠道消息上限。
package notify

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// maxSummaryBytes 渠道概要文本上限（2KB）。
	maxSummaryBytes = 2 * 1024
	// maxUnknownJSONBytes 未知事件的 JSON 摘要上限（500 字符，规格要求）。
	maxUnknownJSONBytes = 500
	// maxReasonBytes 单个原因/错误字段的展示上限。
	maxReasonBytes = 500
)

// formatChannelMessage 渠道消息正文 = 标题 + 换行 + 概要（wecom markdown / telegram
// text 共用；标题为空时用默认标题）。
func formatChannelMessage(title, event string, data map[string]any) string {
	if strings.TrimSpace(title) == "" {
		title = defaultTitle
	}
	return title + "\n" + formatSummary(event, data)
}

// formatSummary 把 event + data 格式化为可读概要（≤2KB，UTF-8 安全截断）。
func formatSummary(event string, data map[string]any) string {
	if data == nil {
		data = map[string]any{}
	}
	var s string
	switch event {
	case EventJobDone:
		s = jobDoneSummary(data)
	case EventAccountDisabled:
		s = accountDisabledSummary(data)
	case EventCheckinFailed:
		s = checkinFailedSummary(data)
	default:
		s = fmt.Sprintf("[%s] %s", event, jsonSummary(data, maxUnknownJSONBytes))
	}
	return truncateBytes(s, maxSummaryBytes)
}

// jobDoneSummary 任务批次结束概要。兼容字段：kind/task/job_kind/type、total/accounts、
// done/succeeded/success、fail/failed/errors/error_count、skip/skipped、trigger、id/job_id。
func jobDoneSummary(data map[string]any) string {
	var b strings.Builder
	b.WriteString("[job_done] ")
	if kind := firstString(data, "kind", "task", "job_kind", "type"); kind != "" {
		b.WriteString(kind)
		b.WriteString(" ")
	}
	done, hasDone := firstInt(data, "done", "succeeded", "success")
	total, hasTotal := firstInt(data, "total", "accounts")
	switch {
	case hasDone && hasTotal:
		fmt.Fprintf(&b, "完成 %d/%d", done, total)
	case hasDone:
		fmt.Fprintf(&b, "完成 %d", done)
	case hasTotal:
		fmt.Fprintf(&b, "共 %d 个账号", total)
	default:
		b.WriteString("已结束")
	}
	if fail, ok := firstInt(data, "fail", "failed", "errors", "error_count"); ok && fail > 0 {
		fmt.Fprintf(&b, " 失败 %d", fail)
	}
	if skip, ok := firstInt(data, "skip", "skipped"); ok && skip > 0 {
		fmt.Fprintf(&b, " 跳过 %d", skip)
	}
	if trigger := firstString(data, "trigger"); trigger != "" {
		fmt.Fprintf(&b, "（%s）", trigger)
	}
	if id := firstString(data, "id", "job_id"); id != "" {
		fmt.Fprintf(&b, " [%s]", id)
	}
	return b.String()
}

// accountDisabledSummary 账号被禁用概要。
func accountDisabledSummary(data map[string]any) string {
	uid := firstString(data, "uid", "account", "account_uid")
	reason := firstString(data, "reason", "message", "error")
	s := "[account_disabled] 账号 " + orPlaceholder(uid, "?") + " 被禁用"
	if reason != "" {
		s += "：" + truncateBytes(oneLine(reason), maxReasonBytes)
	}
	return s
}

// checkinFailedSummary 签到失败概要。
func checkinFailedSummary(data map[string]any) string {
	uid := firstString(data, "uid", "account", "account_uid")
	errMsg := firstString(data, "error", "message", "reason", "err")
	s := "[checkin_failed] 账号 " + orPlaceholder(uid, "?") + " 签到失败"
	if errMsg != "" {
		s += "：" + truncateBytes(oneLine(errMsg), maxReasonBytes)
	}
	return s
}

// jsonSummary 未知事件的 JSON 摘要（序列化失败时回退 %v；按 maxBytes 截断）。
func jsonSummary(data map[string]any, maxBytes int) string {
	var raw []byte
	var err error
	if data == nil {
		raw = []byte("{}")
	} else if raw, err = json.Marshal(data); err != nil {
		raw = []byte(fmt.Sprintf("%v", data))
	}
	return truncateBytes(oneLine(string(raw)), maxBytes)
}

// firstString 返回第一个非空字符串字段（按 keys 顺序）。
func firstString(data map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := data[k].(string); ok {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
	}
	return ""
}

// firstInt 返回第一个可解析为整数的字段；兼容 JSON 往返的 float64 与调用方直接
// 传入的 int/int64/json.Number/数字字符串（通知数据来自不同层，类型可能不一致）。
func firstInt(data map[string]any, keys ...string) (int64, bool) {
	for _, k := range keys {
		switch v := data[k].(type) {
		case int:
			return int64(v), true
		case int64:
			return v, true
		case int32:
			return int64(v), true
		case float64:
			return int64(v), true
		case float32:
			return int64(v), true
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return n, true
			}
		case string:
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// orPlaceholder 空串回退占位符（概要里不留空档）。
func orPlaceholder(s, placeholder string) string {
	if strings.TrimSpace(s) == "" {
		return placeholder
	}
	return s
}

// oneLine 把换行/连续空白压成单个空格（日志/单行消息展示用，防换行注入与多行错位）。
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateText 单行化 + 按字节上限截断（渠道业务错误的日志/错误串用）。
func truncateText(s string, maxBytes int) string {
	return truncateBytes(oneLine(s), maxBytes)
}

// truncateBytes 按字节上限做 UTF-8 安全截断：结果长度 ≤ maxBytes（含省略号），
// 不会切出半个 rune。
func truncateBytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	const ellipsis = "…"
	limit := maxBytes - len(ellipsis)
	if limit < 1 {
		limit = maxBytes
	}
	n := limit
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + ellipsis
}
