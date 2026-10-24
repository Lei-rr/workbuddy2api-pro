package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
)

// TestChatNonStreamRepairsMarkupToolCalls 非流式：上游把工具调用吐成正文标记时，
// 网关还原成 message.tool_calls，正文不再残留标记，finish_reason 收敛为 tool_calls。
func TestChatNonStreamRepairsMarkupToolCalls(t *testing.T) {
	content := markupFixture("先读一遍控制器。\n\n", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	reqBody := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],` + declaredToolsBody + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "\uFF5C") {
		t.Fatalf("响应里仍残留标记: %s", rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "先读一遍控制器。\n\n" {
		t.Errorf("content=%q", msg["content"])
	}
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls=%#v want 1 个", msg["tool_calls"])
	}
	fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn == nil || fn["name"] != "exec_command" {
		t.Fatalf("function=%#v", tcs[0])
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatalf("arguments 不是合法 JSON: %v", fn["arguments"])
	}
	if args["cmd"] != "ls -la" {
		t.Errorf("cmd=%v", args["cmd"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v want tool_calls", choice["finish_reason"])
	}
}

// TestChatStreamRepairsMarkupToolCalls 流式：同样还原成 delta.tool_calls，且收尾帧
// finish_reason 为 tool_calls、排在调用帧之后。
func TestChatStreamRepairsMarkupToolCalls(t *testing.T) {
	content := markupFixture("先读一遍控制器。\n\n", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	reqBody := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}],` + declaredToolsBody + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, "\uFF5C") {
		t.Fatalf("流式响应里仍残留标记: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("缺 [DONE] 收尾: %s", body)
	}
	var text strings.Builder
	var calls int
	frameNo, lastCallFrame, finishFrame := 0, -1, -1
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		frameNo++
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			t.Fatalf("帧不是合法 JSON: %q (%v)", payload, err)
		}
		chs, _ := obj["choices"].([]any)
		if len(chs) == 0 {
			continue
		}
		ch, _ := chs[0].(map[string]any)
		d, _ := ch["delta"].(map[string]any)
		if v, ok := d["content"].(string); ok {
			text.WriteString(v)
		}
		if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
			calls += len(tcs)
			lastCallFrame = frameNo
			fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
			if fn == nil || fn["name"] != "exec_command" {
				t.Fatalf("调用帧 function=%#v", tcs[0])
			}
		}
		if v, ok := ch["finish_reason"].(string); ok && v != "" {
			finishFrame = frameNo
			if v != "tool_calls" {
				t.Errorf("finish_reason=%q want tool_calls", v)
			}
		}
	}
	if text.String() != "先读一遍控制器。\n\n" {
		t.Errorf("正文错误: %q", text.String())
	}
	if calls != 1 {
		t.Errorf("tool_call 帧数=%d want 1", calls)
	}
	if lastCallFrame < 0 || finishFrame < 0 {
		t.Fatalf("缺调用帧或收尾帧: lastCall=%d finish=%d", lastCallFrame, finishFrame)
	}
	if finishFrame < lastCallFrame {
		t.Errorf("收尾帧（第 %d 帧）必须排在调用帧（第 %d 帧）之后", finishFrame, lastCallFrame)
	}
}

// --- 弱判定（名单为空）端到端：这是实测最常见的泄漏形态 ---------------------

// TestChatRepairsMarkupWithoutDeclaredTools 请求体里**没有** tools 声明时（tools 在
// 中转环节丢失是实测常态），弱判定必须照样把正文标记还原成 tool_calls——否则修复层
// 恰好对主场景不设防。夹具用生产环境真实泄漏的正文。
func TestChatRepairsMarkupWithoutDeclaredTools(t *testing.T) {
	content := markupFixture("", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "\uFF5C") {
		t.Fatalf("响应里仍残留标记: %s", rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls=%#v want 1 个", msg["tool_calls"])
	}
	fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn == nil || fn["name"] != "exec_command" {
		t.Fatalf("function=%#v", tcs[0])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v want tool_calls", choice["finish_reason"])
	}
}

// TestChatMarkupRepairRejectsNonIdentifierToolName 弱判定的把关：名字不像工具名
// （模型在正文里「讨论」这段语法时的占位符）→ 原文透传，绝不改写成工具调用。
func TestChatMarkupRepairRejectsNonIdentifierToolName(t *testing.T) {
	content := markupFixtureNamed("", "<tool_name>", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, ok := msg["tool_calls"]; ok {
		t.Fatalf("占位符工具名不得转换: %#v", msg["tool_calls"])
	}
	if got, _ := msg["content"].(string); !strings.Contains(got, "\uFF5C") {
		t.Fatalf("应原文透传，实际 content=%q", got)
	}
}

// TestChatMarkupRepairStrictModeRejectsUndeclaredTool 名单非空时走严格判定：
// 块内工具名不在名单里 → 不转换（即使名字形态合法）。
func TestChatMarkupRepairStrictModeRejectsUndeclaredTool(t *testing.T) {
	content := markupFixtureNamed("", "rm_rf_everything", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	reqBody := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],` + declaredToolsBody + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, ok := msg["tool_calls"]; ok {
		t.Fatalf("未声明的工具名不得转换: %#v", msg["tool_calls"])
	}
}
