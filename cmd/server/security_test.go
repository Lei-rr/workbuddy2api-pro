package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/livecfg"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/scheduler"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// TestValidateListenSecurity 启动安全校验（v2.0.1 #4-2「空 key 非回环阻断」）：
// 非回环监听 + 网关/面板均无密钥 + 未显式允许匿名 → 拒绝启动；回环监听与任一密钥
// 存在时放行；security.allow_anonymous=true 是显式逃生门。
func TestValidateListenSecurity(t *testing.T) {
	cases := []struct {
		name      string
		listen    string
		apiKey    string
		panelKey  string
		allowAnon bool
		wantErr   bool
	}{
		{"all interfaces no key", ":7863", "", "", false, true},
		{"0.0.0.0 no key", "0.0.0.0:7863", "", "", false, true},
		{"lan ip no key", "192.168.1.10:7863", "", "", false, true},
		{"public hostname no key", "gateway.example.com:7863", "", "", false, true},
		{"loopback v4", "127.0.0.1:7863", "", "", false, false},
		{"loopback v6", "[::1]:7863", "", "", false, false},
		{"localhost", "localhost:7863", "", "", false, false},
		{"gateway key protects", ":7863", "sk-gateway", "", false, false},
		{"panel key protects", ":7863", "", "sk-panel", false, false},
		{"both keys protect", ":7863", "sk-gateway", "sk-panel", false, false},
		{"allow anonymous escape hatch", ":7863", "", "", true, false},
		{"loopback explicit anonymous", "127.0.0.1:7863", "", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			c.Listen = tc.listen
			c.APIKey = tc.apiKey
			c.Panel.APIKey = tc.panelKey
			c.Security.AllowAnonymous = tc.allowAnon
			err := validateListenSecurity(c)
			if tc.wantErr && err == nil {
				t.Fatalf("listen=%q keys=%q/%q: want startup rejection", tc.listen, tc.apiKey, tc.panelKey)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("listen=%q keys=%q/%q: unexpected rejection: %v", tc.listen, tc.apiKey, tc.panelKey, err)
			}
			if err != nil {
				// 错误信息必须给出可操作的修复路径（生成 key / 逃生门 / 改回环）。
				for _, must := range []string{"api_key", "panel.api_key", "security.allow_anonymous", "127.0.0.1"} {
					if !strings.Contains(err.Error(), must) {
						t.Errorf("error should mention %q, got: %v", must, err)
					}
				}
			}
		})
	}
	// nil 配置不 panic、不报错（测试/裸用场景）。
	if err := validateListenSecurity(nil); err != nil {
		t.Fatalf("nil config: %v", err)
	}
}

// TestIsLoopbackListen 回环判定：":7863" 与 "0.0.0.0:7863" 都是全网卡（非回环）；
// 只有 127.0.0.1/::1/localhost 视为回环；其他主机名保守按非回环处理。
func TestIsLoopbackListen(t *testing.T) {
	loop := []string{"127.0.0.1:7863", "[::1]:7863", "localhost:7863", "LOCALHOST:7863", "127.0.0.2:80"}
	nonLoop := []string{":7863", "0.0.0.0:7863", "[::]:7863", "192.168.1.10:7863", "example.com:7863", "7863", ""}
	for _, l := range loop {
		if !isLoopbackListen(l) {
			t.Errorf("isLoopbackListen(%q)=false want true", l)
		}
	}
	for _, l := range nonLoop {
		if isLoopbackListen(l) {
			t.Errorf("isLoopbackListen(%q)=true want false", l)
		}
	}
}

// newSaveConfigFixture 搭一个 saveConfig 测试夹具（tmp config + 最小依赖）。
func newSaveConfigFixture(t *testing.T, original string) (path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	p = pool.New("")
	p.Add(&auth.Auth{UID: "a"})
	live = livecfg.New(livecfg.Snapshot{})
	up = upstream.New()
	sch = scheduler.New(scheduler.Config{})
	return
}

// TestSaveConfigRejectsClearingAPIKey 面板保存配置拒绝把网关 api_key 写成空
// （除非显式 security.allow_anonymous=true）：即失败也不落盘、文件保持原样。
func TestSaveConfigRejectsClearingAPIKey(t *testing.T) {
	original := `{"listen":":7863","api_key":"sk-original"}`
	path, live, p, up, sch := newSaveConfigFixture(t, original)

	_, err := saveConfig([]byte(`{"api_key":""}`), path, live, p, up, sch)
	if err == nil {
		t.Fatal("clearing api_key must be rejected")
	}
	if !strings.Contains(err.Error(), "api_key") || !strings.Contains(err.Error(), "allow_anonymous") {
		t.Fatalf("error should explain api_key / allow_anonymous: %v", err)
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != original {
		t.Fatalf("rejected save must not touch config file:\n%s", got)
	}

	// 显式打开安全逃生门后放行（用户明确接受匿名访问）。
	if _, err := saveConfig([]byte(`{"api_key":"","security":{"allow_anonymous":true}}`), path, live, p, up, sch); err != nil {
		t.Fatalf("allow_anonymous should permit empty api_key: %v", err)
	}
	saved, err := Load(path)
	if err != nil {
		t.Fatalf("load saved config: %v", err)
	}
	if saved.APIKey != "" || !saved.Security.AllowAnonymous {
		t.Fatalf("saved config = api_key %q allow_anonymous %v", saved.APIKey, saved.Security.AllowAnonymous)
	}
}

// TestSaveConfigLegacyEmptyKeyNotBlocked 老配置本来就没有 key 且本次没动 api_key 时
// 不拦截（保持既有测试环境/仅回环自用场景可用）；启动校验负责非回环场景的硬阻断。
func TestSaveConfigLegacyEmptyKeyNotBlocked(t *testing.T) {
	path, live, p, up, sch := newSaveConfigFixture(t, `{"listen":"127.0.0.1:7863"}`)
	ListRestart, err := saveConfig([]byte(`{"pool":{"max_in_flight":5}}`), path, live, p, up, sch)
	if err != nil {
		t.Fatalf("legacy empty-key save should not be blocked: %v", err)
	}
	if !slices.Contains(ListRestart, "panel.api_key") {
		t.Fatalf("restart_required should list panel.api_key, got %v", ListRestart)
	}
	saved, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Pool.MaxInFlight != 5 {
		t.Fatalf("max_in_flight=%d want 5", saved.Pool.MaxInFlight)
	}
}

// TestSaveConfigAllowsClearingPanelKey 清空 panel.api_key 是合法的（= 回落网关 key），
// 不在空 key 拦截范围内。
func TestSaveConfigAllowsClearingPanelKey(t *testing.T) {
	path, live, p, up, sch := newSaveConfigFixture(t, `{"listen":":7863","api_key":"sk-gw","panel":{"api_key":"sk-panel"}}`)
	if _, err := saveConfig([]byte(`{"panel":{"api_key":""}}`), path, live, p, up, sch); err != nil {
		t.Fatalf("clearing panel.api_key should be allowed: %v", err)
	}
	saved, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Panel.APIKey != "" || saved.APIKey != "sk-gw" {
		t.Fatalf("saved keys = panel %q gateway %q", saved.Panel.APIKey, saved.APIKey)
	}
}

// TestRestartRequiredIncludesPanelAPIKey panel.api_key 装配期生效，必须在保存响应里
// 提示重启（v2.0.1 #4-1）。
func TestRestartRequiredIncludesPanelAPIKey(t *testing.T) {
	if !slices.Contains(restartRequiredFields(Default()), "panel.api_key") {
		t.Fatal("panel.api_key missing from restart-required fields")
	}
	// panel.poll_interval_sec 走 livecfg 热生效，不应要求重启。
	if slices.Contains(restartRequiredFields(Default()), "panel.poll_interval_sec") {
		t.Fatal("panel.poll_interval_sec is hot-reloadable and must not require restart")
	}
}
