// bodylimit_test.go v2.6.0（Audit-1 P0-1）：/v1 POST 请求体上限（max_body_mb）。
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// newBodyLimitHandler 构造最小 Handler（无账号池/上游网络调用）。
func newBodyLimitHandler(maxBodyMB int) *Handler {
	return NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New(), MaxBodyMB: maxBodyMB})
}

// TestChatBodyLimit413 超限请求体返回 413 + OpenAI 错误体（不落 500）。
func TestChatBodyLimit413(t *testing.T) {
	h := newBodyLimitHandler(1) // 1MiB
	oversize := bytes.Repeat([]byte("a"), (1<<20)+1024)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(oversize))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限 body 应返回 413，got %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("413 响应应为 JSON 错误体: %v body=%s", err, rec.Body.String())
	}
	if payload.Error.Code != "request_too_large" {
		t.Fatalf("错误 code = %q，want request_too_large", payload.Error.Code)
	}
	if !strings.Contains(payload.Error.Message, "MiB") {
		t.Fatalf("错误信息应说明上限: %q", payload.Error.Message)
	}
}

// TestChatBodyWithinLimitNot413 正常体积请求不因上限被拒（空池 → 无账号错误，非 413）。
func TestChatBodyWithinLimitNot413(t *testing.T) {
	h := newBodyLimitHandler(1)
	body := []byte(`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("正常体积 body 不应 413: %s", rec.Body.String())
	}
}

// TestMessagesBodyLimit413 messages 端点超 1MiB 返回 Anthropic 形态 413。
func TestMessagesBodyLimit413(t *testing.T) {
	h := newBodyLimitHandler(64) // chat 上限宽松；messages 仍固定 1MiB
	oversize := bytes.Repeat([]byte("a"), messagesBodyLimit+1024)
	req := httptest.NewRequest(http.MethodPost, "/v1/search/messages", bytes.NewReader(oversize))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("messages 超限应返回 413，got %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("messages 413 应为 JSON: %v body=%s", err, rec.Body.String())
	}
	if payload.Type != "error" || payload.Error.Type != "request_too_large" {
		t.Fatalf("messages 413 形态不符: %s", rec.Body.String())
	}
}

// TestMaxBodyBytesDefaultsAndClamp 裸用 Handler 时的缺省/上限夹取。
func TestMaxBodyBytesDefaultsAndClamp(t *testing.T) {
	cases := []struct {
		mb   int
		want int64
	}{
		{0, 64 << 20},
		{-1, 64 << 20},
		{8, 8 << 20},
		{4096, 1024 << 20},
	}
	for _, tc := range cases {
		h := &Handler{cfg: Config{MaxBodyMB: tc.mb}}
		if got := h.maxBodyBytes(); got != tc.want {
			t.Fatalf("MaxBodyMB=%d → %d，want %d", tc.mb, got, tc.want)
		}
	}
}
