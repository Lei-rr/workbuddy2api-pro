package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c.Listen != ":7863" {
		t.Errorf("listen=%s", c.Listen)
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.SoftRateDur.Seconds() != 600 {
		t.Errorf("soft=%v want 600s", c.SoftRateDur)
	}
}

func TestLoggingDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if !c.Logging.RequestArchiveEnabled || c.Logging.RequestRetentionDays != 7 || c.Logging.RequestArchiveMaxMB != 100 {
		t.Fatalf("logging defaults = %+v", c.Logging)
	}
	// 来源记录（IP/UA）缺省开启：键缺席时必须保持 true，只有显式 false 才关闭。
	if !c.Logging.RequestClientInfo {
		t.Fatalf("request_client_info default = false, want true: %+v", c.Logging)
	}
	configured, err := ParseConfig([]byte(`{"logging":{"request_archive_enabled":false,"request_retention_days":30,"request_archive_max_mb":500}}`))
	if err != nil {
		t.Fatal(err)
	}
	if configured.Logging.RequestArchiveEnabled || configured.Logging.RequestRetentionDays != 30 || configured.Logging.RequestArchiveMaxMB != 500 {
		t.Fatalf("configured logging = %+v", configured.Logging)
	}
	off, err := ParseConfig([]byte(`{"logging":{"request_client_info":false}}`))
	if err != nil {
		t.Fatal(err)
	}
	if off.Logging.RequestClientInfo {
		t.Fatalf("explicit false ignored: %+v", off.Logging)
	}
	fallback, err := ParseConfig([]byte(`{"logging":{"request_retention_days":0,"request_archive_max_mb":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	if fallback.Logging.RequestRetentionDays != 7 || fallback.Logging.RequestArchiveMaxMB != 100 {
		t.Fatalf("logging fallback = %+v", fallback.Logging)
	}
}

// TestRetiredTravelIntervalKeyIgnored 退役的 travel_interval_minutes 键按未知字段忽略，不报错。
func TestRetiredTravelIntervalKeyIgnored(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_interval_minutes":15,"checkin_hours":[9]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("retired key should not fail load: %v", err)
	}
	if len(c.Schedule.CheckinHours) != 1 || c.Schedule.CheckinHours[0] != 9 {
		t.Errorf("checkin_hours=%v want [9]（同段其余键照常生效）", c.Schedule.CheckinHours)
	}
}

func TestWriteDefault(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "sub", "config.json") // 顺带验证父目录自动创建
	key, err := WriteDefault(fp)
	if err != nil {
		t.Fatal(err)
	}
	// key 形如 sk-<24字符随机串>，两次生成不重复
	if !strings.HasPrefix(key, "sk-") || len(key) < 20 {
		t.Errorf("key=%q want sk-<random>", key)
	}
	if key2, _ := WriteDefault(filepath.Join(dir, "another.json")); key2 == key {
		t.Errorf("two generated keys identical: %q", key)
	}
	// 落盘文件可被 Load 正常加载，推荐值齐备且 api_key 生效
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("load generated config: %v", err)
	}
	if c.APIKey != key {
		t.Errorf("api_key=%q want %q", c.APIKey, key)
	}
	if c.Listen != ":7863" || c.AuthDir != "./auths" || c.StateFile != "./data/state.json" {
		t.Errorf("generated defaults off: %+v", c)
	}
	if len(c.Schedule.CheckinHours) == 0 || !c.Schedule.CheckinEnabled {
		t.Errorf("generated schedule off: %+v", c.Schedule)
	}
	// 已存在的文件不覆盖：二次写入同一路径必须报错
	if _, err := WriteDefault(fp); err == nil {
		t.Error("WriteDefault must refuse to overwrite existing file")
	}
}

func TestBalanceRefreshDefaults(t *testing.T) {
	// 缺省：启用 + 30 分钟
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if !c.Schedule.BalanceRefreshEnabled || c.BalanceRefreshInterval != 5*time.Minute {
		t.Errorf("default balance refresh: enabled=%v interval=%v", c.Schedule.BalanceRefreshEnabled, c.BalanceRefreshInterval)
	}
	// 显式配置 10 分钟
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"balance_refresh_minutes":10}}`), 0o600)
	c2, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c2.BalanceRefreshInterval != 10*time.Minute {
		t.Errorf("interval=%v want 10m", c2.BalanceRefreshInterval)
	}
	// 显式关闭：interval 归零（不启动）
	os.WriteFile(fp, []byte(`{"schedule":{"balance_refresh_enabled":false}}`), 0o600)
	c3, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c3.BalanceRefreshInterval != 0 {
		t.Errorf("disabled interval=%v want 0", c3.BalanceRefreshInterval)
	}
	// 启用但 minutes<=0 → 回落默认 30
	os.WriteFile(fp, []byte(`{"schedule":{"balance_refresh_minutes":-5}}`), 0o600)
	c4, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c4.BalanceRefreshInterval != 5*time.Minute {
		t.Errorf("fallback interval=%v want 30m", c4.BalanceRefreshInterval)
	}
}

// TestServerReadTimeout 入站读取上限（issue #100）：空值回落默认 300s；
// "0" = 显式不限制（0 是合法值不回落）；负值 fail fast（静默钳 0 会把保护悄悄关掉）。
func TestServerReadTimeout(t *testing.T) {
	c := Default()
	if c.Server.ReadTimeout != "300s" {
		t.Errorf("default read_timeout=%q want 300s", c.Server.ReadTimeout)
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.ServerReadTimeoutDur != 300*time.Second {
		t.Errorf("default dur=%v want 300s", c.ServerReadTimeoutDur)
	}

	c = Default()
	c.Server.ReadTimeout = "" // 显式清空 = 未配置 → 回落默认
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize empty: %v", err)
	}
	if c.ServerReadTimeoutDur != 300*time.Second {
		t.Errorf("empty dur=%v want 300s", c.ServerReadTimeoutDur)
	}

	c = Default()
	c.Server.ReadTimeout = "0" // 显式 0 = 不限制（http.Server ReadTimeout 0 即无超时）
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize zero: %v", err)
	}
	if c.ServerReadTimeoutDur != 0 {
		t.Errorf("zero dur=%v want 0", c.ServerReadTimeoutDur)
	}

	c = Default()
	c.Server.ReadTimeout = "-5s"
	if err := c.normalize(); err == nil {
		t.Error("negative read_timeout should fail fast")
	}

	c = Default()
	c.Server.ReadTimeout = "bogus"
	if err := c.normalize(); err == nil {
		t.Error("unparsable read_timeout should fail fast")
	}
}
