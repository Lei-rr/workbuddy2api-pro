package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9999" || c.APIKey != "k" {
		t.Errorf("c=%+v", c)
	}
}

func TestBadDuration(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"not-a-duration"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad duration")
	}
}

func TestBadSoftRateMax(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate_max":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad soft_rate_max")
	}
}

func TestBadBreakerCooldown(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"breaker_cooldown":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad breaker_cooldown")
	}
}

// TestScheduleTravelActivityInvalidHoursRejected 旅行/活跃非法小时报错并指向正确开关。
func TestScheduleTravelActivityInvalidHoursRejected(t *testing.T) {
	cases := []struct{ body, wantSwitch string }{
		{`{"schedule":{"travel_hours":[25]}}`, "travel_enabled"},
		{`{"schedule":{"travel_hours":[-1]}}`, "travel_enabled"},
		{`{"schedule":{"activity_hours":[24]}}`, "activity_enabled"},
		{`{"schedule":{"activity_hours":[-1]}}`, "activity_enabled"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(tc.body), 0o600)
		_, err := Load(fp)
		if err == nil {
			t.Fatalf("want error for %s", tc.body)
		}
		if !strings.Contains(err.Error(), tc.wantSwitch) {
			t.Errorf("error for %s should point at schedule.%s: %v", tc.body, tc.wantSwitch, err)
		}
	}
}

// TestScheduleInvalidHourRejected 非法小时快速失败：指向正确的禁用开关，避免用户
// 猜测哨兵值（[-1] 之类）被静默当成"改到别的整点"。
func TestScheduleInvalidHourRejected(t *testing.T) {
	cases := []struct{ body, wantSwitch string }{
		{`{"schedule":{"checkin_hours":[25]}}`, "checkin_enabled"},
		{`{"schedule":{"checkin_hours":[-1]}}`, "checkin_enabled"},
		{`{"schedule":{"keepalive_hours":[-1]}}`, "keepalive_enabled"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(tc.body), 0o600)
		_, err := Load(fp)
		if err == nil {
			t.Fatalf("want error for %s", tc.body)
		}
		if !strings.Contains(err.Error(), tc.wantSwitch) {
			t.Errorf("error for %s should point at schedule.%s: %v", tc.body, tc.wantSwitch, err)
		}
	}
}

func TestBadSessionTTL(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"session_sticky":{"ttl":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad session_sticky.ttl")
	}
}

// TestPromptInvalidMode 非法 mode 启动报错。
func TestPromptInvalidMode(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"bogus"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for invalid prompt.mode")
	}
}

// TestPromptFileMissing 文件路径非空但不存在 → 启动报错（fail fast）。
func TestPromptFileMissing(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"custom","file":"/nonexistent/p.md"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for missing prompt file")
	}
}

// TestLoadConfigPathIsDirectory config 路径是目录时给出可操作提示（Docker bind mount 陷阱）。
// 复现：compose 挂载 ./config.json 但宿主机缺该文件 → Docker 创建同名目录 → 启动失败。
// 旧行为只报 "read config: ... Incorrect function" 之类晦涩错误，无从排查。
func TestLoadConfigPathIsDirectory(t *testing.T) {
	dir := t.TempDir()
	asDir := filepath.Join(dir, "config.json")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Load(asDir)
	if err == nil {
		t.Fatal("want error when config path is a directory")
	}
	msg := err.Error()
	if !strings.Contains(msg, "是目录") {
		t.Errorf("error should explain it is a directory: %v", err)
	}
	if !strings.Contains(msg, "config.example.json") {
		t.Errorf("error should suggest the fix (cp config.example.json): %v", err)
	}
}

// TestScheduleConfigV201InvalidDelay 非法/负 duration 启动即报错（不静默回落）。
func TestScheduleConfigV201InvalidDelay(t *testing.T) {
	for _, body := range []string{
		`{"schedule":{"balance_refresh_startup_delay":"oops"}}`,
		`{"schedule":{"balance_refresh_startup_delay":"-5s"}}`,
	} {
		if _, err := ParseConfig([]byte(body)); err == nil {
			t.Fatalf("%s: want error", body)
		} else if !strings.Contains(err.Error(), "balance_refresh_startup_delay") {
			t.Errorf("%s: error should name the key: %v", body, err)
		}
	}
}

// TestWeakAPIKeyGuard 弱/示例密钥启动守卫（v2.6.0 审计 P0）：
//   - 非回环监听 + 命中弱名单（api_key 或 panel.api_key）→ 启动/保存失败；
//   - 回环监听 + 弱密钥 → 仅 WARN 放行；
//   - 强密钥 / 空密钥 → 放行（空 = 不鉴权，由 validateListenSecurity 在启动时处理）。
func TestWeakAPIKeyGuard(t *testing.T) {
	weak := []string{"test_key", "TEST_KEY", "test-key", "changeme", "ChangeMe", "password", "123456", "example", "admin", "  test_key  "}
	for _, key := range weak {
		c := Default() // listen :7863 = 非回环
		c.APIKey = key
		if err := c.normalize(); err == nil {
			t.Fatalf("api_key=%q 非回环监听应被拒绝启动", key)
		}
		c = Default()
		c.Panel.APIKey = key
		if err := c.normalize(); err == nil {
			t.Fatalf("panel.api_key=%q 非回环监听应被拒绝启动", key)
		}
	}
	// 回环监听：弱密钥仅 WARN 放行（本机自用）。
	for _, key := range []string{"test_key", "password"} {
		c := Default()
		c.Listen = "127.0.0.1:7863"
		c.APIKey = key
		if err := c.normalize(); err != nil {
			t.Fatalf("回环 + 弱密钥 %q 应放行，got %v", key, err)
		}
	}
	// 强随机密钥 / 空密钥：非回环也放行（空由启动安全校验决定）。
	for _, key := range []string{"sk-3f9c2b1d8a7e6f504142434445464748", ""} {
		c := Default()
		c.APIKey = key
		if err := c.normalize(); err != nil {
			t.Fatalf("api_key=%q 应放行，got %v", key, err)
		}
	}
}

// TestServerMaxBodyMB /v1 请求体上限（v2.6.0）：默认 64；<=0 回落默认；clamp [1,1024]；
// env WB2A_MAX_BODY_MB 覆盖（非整数忽略，与其它 int env 同风格）。
func TestServerMaxBodyMB(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, 64},      // 未设置/0 → 默认
		{-5, 64},     // 负值 → 默认
		{1, 1},       // 下边界
		{64, 64},     // 默认值
		{128, 128},   // 正常配置
		{2000, 1024}, // 上界 clamp
	}
	for _, tc := range cases {
		c := Default()
		c.Server.MaxBodyMB = tc.in
		if err := c.normalize(); err != nil {
			t.Fatalf("max_body_mb=%d normalize: %v", tc.in, err)
		}
		if c.Server.MaxBodyMB != tc.want {
			t.Errorf("max_body_mb=%d → %d want %d", tc.in, c.Server.MaxBodyMB, tc.want)
		}
	}
	// Default() 本身就是 64。
	if got := Default().Server.MaxBodyMB; got != 64 {
		t.Errorf("Default server.max_body_mb=%d want 64", got)
	}
	// 环境变量覆盖。
	t.Setenv("WB2A_MAX_BODY_MB", "256")
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Server.MaxBodyMB != 256 {
		t.Errorf("WB2A_MAX_BODY_MB=256 → %d", c.Server.MaxBodyMB)
	}
	// 非法环境变量忽略（回落默认），不阻断启动。
	t.Setenv("WB2A_MAX_BODY_MB", "not-a-number")
	c, err = Load("")
	if err != nil {
		t.Fatalf("Load with invalid env: %v", err)
	}
	if c.Server.MaxBodyMB != 64 {
		t.Errorf("invalid env → %d want 64", c.Server.MaxBodyMB)
	}
}
