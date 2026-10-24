package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestConfigExampleJSONParsesAndHasV201Keys config.example.json 必须与 Default+normalize
// 同步（可直接加载），且包含 v2.0.1 新增的 7 个键（前端配置页按此渲染）。
func TestConfigExampleJSONParsesAndHasV201Keys(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("read config.example.json: %v", err)
	}
	c, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("parse config.example.json: %v", err)
	}
	// v2.6.0 审计 P0：示例文件 api_key 必须为空（不能再用 test_key 之类的可预测占位值；
	// Docker 会把它原样复制为 /app/config.json，非回环 + 空 key 由启动安全校验 fail-fast）。
	if c.APIKey != "" {
		t.Errorf("example api_key=%q want empty（P0：示例不得含默认可用密钥）", c.APIKey)
	}
	// v2.6.0：/v1 请求体上限默认 64MiB（example 必须与 Default 同步）。
	if c.Server.MaxBodyMB != 64 {
		t.Errorf("example server.max_body_mb=%d want 64", c.Server.MaxBodyMB)
	}
	if c.Panel.PollIntervalSec != 5 {
		t.Errorf("example panel.poll_interval_sec=%d want 5", c.Panel.PollIntervalSec)
	}
	if c.Security.AllowAnonymous {
		t.Error("example security.allow_anonymous should be false")
	}
	if c.Schedule.CatchUpMinutes != 120 || c.Schedule.BalanceRefreshConcurrency != 8 {
		t.Errorf("example schedule new keys = %+v", c.Schedule)
	}
	if c.BalanceRefreshStartupDelayDur != 30*time.Second || c.Schedule.BalanceRefreshStartupForce {
		t.Errorf("example startup refresh = %q/%v force=%v",
			c.Schedule.BalanceRefreshStartupDelay, c.BalanceRefreshStartupDelayDur, c.Schedule.BalanceRefreshStartupForce)
	}
	// 7 个键在原始 JSON 里必须真实存在（前端 frontend2 按这些 JSON 键名接线）。
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("re-parse example: %v", err)
	}
	var panel map[string]json.RawMessage
	_ = json.Unmarshal(probe["panel"], &panel)
	for _, k := range []string{"api_key", "poll_interval_sec"} {
		if _, ok := panel[k]; !ok {
			t.Errorf("config.example.json missing panel.%s", k)
		}
	}
	var sched map[string]json.RawMessage
	_ = json.Unmarshal(probe["schedule"], &sched)
	for _, k := range []string{"catch_up_minutes", "balance_refresh_concurrency", "balance_refresh_startup_delay", "balance_refresh_startup_force"} {
		if _, ok := sched[k]; !ok {
			t.Errorf("config.example.json missing schedule.%s", k)
		}
	}
	var sec map[string]json.RawMessage
	_ = json.Unmarshal(probe["security"], &sec)
	if _, ok := sec["allow_anonymous"]; !ok {
		t.Error("config.example.json missing security.allow_anonymous")
	}
	// v2.6.0：example 的 server 块必须含 max_body_mb 键（前端/文档按 JSON 键名接线）。
	var srv map[string]json.RawMessage
	_ = json.Unmarshal(probe["server"], &srv)
	if _, ok := srv["max_body_mb"]; !ok {
		t.Error("config.example.json missing server.max_body_mb")
	}
}
