package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/redisstore"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// TestMain 默认关闭聊天表格日志（chatLogEnabled=false），消除 go test 期间的 stdout 噪音。
// 断言表格行输出的测试（logging_test.go 中的 ChatLogs/LogChatRow 系列）用 withChatLog 临时开启。
func TestMain(m *testing.M) {
	chatLogEnabled = false
	os.Exit(m.Run())
}

const sseOK = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

// newFakeUpstream 返回一个 ChatStream 走 fake 的 upstream.Client。
// fake 依据 Authorization 头决定行为。
func newFakeUpstream(t *testing.T, behavior func(auth string) (status int, body string, isStream bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			status, body, isStream := behavior(authz)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// bindStore 记录粘性绑定镜像调用（不联网），供 D4 端到端断言绑定收敛到最终成功号。
type bindStore struct {
	redisstore.Noop
	mu     sync.Mutex
	binds  map[string]string
	delCnt int
}

func newBindStore() *bindStore { return &bindStore{binds: map[string]string{}} }

func (b *bindStore) SetBind(key, uid string, ttl time.Duration) {
	b.mu.Lock()
	b.binds[key] = uid
	b.mu.Unlock()
}

func (b *bindStore) DelBind(key string) {
	b.mu.Lock()
	b.delCnt++
	delete(b.binds, key)
	b.mu.Unlock()
}

func (b *bindStore) LoadBinds() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]string{}
	for k, v := range b.binds {
		out[k] = v
	}
	return out
}

func (b *bindStore) lastUID(key string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.binds[key]
	return u, ok
}

// testPoolWith 构建一个所有账号 credits=1000 的池，并注入确定性随机源：
// randInt64N 恒返回 0 → pickWeighted 必选候选集中积分最高者（第一个）。
// 这让依赖"bad 先被选中"的轮转测试（如 TestChatRotatesOnHardCredit）完全确定，
// 不再受加权随机影响而 flake。
func testPoolWith(auths ...*auth.Auth) *pool.Pool {
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	for _, a := range auths {
		p.Add(a)
		p.SetCredits(a.UID, 1000, 0)
	}
	return p
}

// --- 模型原生工具调用标记修复（端到端，见 upstream/dsml.go）-----------------

// markupFixture 拼一段「正文 + 原生工具调用标记」，模拟上游在没有 tools 的请求里
// 把工具调用吐成文本的形态。分隔符按码点构造，避免测试源码里出现裸全角字面量。
func markupFixture(prose, cmd string) string {
	return markupFixtureNamed(prose, "exec_command", cmd)
}

// markupFixtureNamed 同 markupFixture，但可指定工具名（用于弱判定的形态把关测试）。
func markupFixtureNamed(prose, name, cmd string) string {
	const pipe = "\uFF5C\uFF5C"
	tag := func(rest string) string { return "<" + pipe + "DSML" + pipe + " " + rest + ">" }
	ctag := func(rest string) string { return "</" + pipe + "DSML" + pipe + " " + rest + ">" }
	return prose + tag("calls") + "\n" +
		tag(`invoke name="`+name+`"`) + "\n" +
		tag(`parameter name="cmd" string="true"`) + cmd + ctag("parameter") + "\n" +
		ctag("invoke") + "\n" + ctag("calls")
}

// markupUpstreamSSE 用一段正文拼上游 SSE（role 首帧 + 正文帧 + stop 收尾帧 + [DONE]）。
func markupUpstreamSSE(content string) string {
	chunk := func(delta map[string]any, finish any) string {
		b, _ := json.Marshal(map[string]any{
			"id": "resp-1", "object": "chat.completion.chunk", "created": 1, "model": "glm-5.2",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})
		return "data: " + string(b) + "\n\n"
	}
	return chunk(map[string]any{"role": "assistant"}, nil) +
		chunk(map[string]any{"content": content}, nil) +
		chunk(map[string]any{}, "stop") +
		"data: [DONE]\n\n"
}

// declaredToolsBody 是带 tools 声明的请求体（标记修复的启用前提）。
const declaredToolsBody = `"tools":[{"type":"function","function":{"name":"exec_command",` +
	`"parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}}]`
