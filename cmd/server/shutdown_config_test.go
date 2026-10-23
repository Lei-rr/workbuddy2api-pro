// shutdown_config_test.go v2.2 #14 新配置项 server.shutdown_timeout 的解析/normalize/env
// 契约（独立文件，避免与并行改 config_test.go 的 teammate 冲突）。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestShutdownTimeoutDefaultAndNormalize 缺省 30s；"0"/负值/非法时长 fail fast；
// 越界 clamp 到 [5s,120s]。
func TestShutdownTimeoutDefaultAndNormalize(t *testing.T) {
	c := Default()
	if c.Server.ShutdownTimeout != "30s" {
		t.Errorf("default shutdown_timeout=%q want 30s", c.Server.ShutdownTimeout)
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize default: %v", err)
	}
	if c.ShutdownTimeoutDur != 30*time.Second {
		t.Errorf("default dur=%v want 30s", c.ShutdownTimeoutDur)
	}

	cases := []struct {
		in       string
		wantDur  time.Duration
		wantErr  bool
		wantText string // normalize 后回写值（clamp 时）
	}{
		{"45s", 45 * time.Second, false, "45s"},
		{"5s", 5 * time.Second, false, "5s"},
		{"120s", 120 * time.Second, false, "120s"},
		{"1s", 5 * time.Second, false, "5s"},     // 低于下限 → clamp 5s
		{"5m", 120 * time.Second, false, "120s"}, // 高于上限 → clamp 120s
		{"bogus", 0, true, ""},                   // 非法时长 fail fast
		{"0", 0, true, ""},                       // 0 无「不限」语义，拒绝
		{"-5s", 0, true, ""},                     // 负时长无意义
	}
	for _, tc := range cases {
		cfg := Default()
		cfg.Server.ShutdownTimeout = tc.in
		err := cfg.normalize()
		if tc.wantErr {
			if err == nil {
				t.Errorf("shutdown_timeout=%q should fail fast", tc.in)
			} else if !strings.Contains(err.Error(), "shutdown_timeout") {
				t.Errorf("error should name server.shutdown_timeout: %v", err)
			}
			continue
		}
		if err != nil {
			t.Errorf("shutdown_timeout=%q normalize: %v", tc.in, err)
			continue
		}
		if cfg.ShutdownTimeoutDur != tc.wantDur {
			t.Errorf("shutdown_timeout=%q → %v want %v", tc.in, cfg.ShutdownTimeoutDur, tc.wantDur)
		}
		if cfg.Server.ShutdownTimeout != tc.wantText {
			t.Errorf("clamped field=%q want %q", cfg.Server.ShutdownTimeout, tc.wantText)
		}
	}

	// 空字符串 = 未配置 → 回落默认 30s。
	cfg := Default()
	cfg.Server.ShutdownTimeout = ""
	if err := cfg.normalize(); err != nil {
		t.Fatalf("normalize empty: %v", err)
	}
	if cfg.ShutdownTimeoutDur != 30*time.Second {
		t.Errorf("empty → %v want 30s", cfg.ShutdownTimeoutDur)
	}
}

// TestShutdownTimeoutParseAndEnv JSON 解析与 WB2A_SHUTDOWN_TIMEOUT 环境变量覆盖。
func TestShutdownTimeoutParseAndEnv(t *testing.T) {
	got, err := ParseConfig([]byte(`{"server":{"shutdown_timeout":"45s"}}`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if got.ShutdownTimeoutDur != 45*time.Second {
		t.Fatalf("JSON 45s → %v", got.ShutdownTimeoutDur)
	}

	t.Setenv("WB2A_SHUTDOWN_TIMEOUT", "10s")
	fromEnv, err := Load("")
	if err != nil {
		t.Fatalf("Load env: %v", err)
	}
	if fromEnv.ShutdownTimeoutDur != 10*time.Second {
		t.Fatalf("env 10s → %v", fromEnv.ShutdownTimeoutDur)
	}

	t.Setenv("WB2A_SHUTDOWN_TIMEOUT", "nope")
	if _, err := Load(""); err == nil {
		t.Fatal("invalid env shutdown timeout should fail load")
	}
}

// TestConfigExampleJSONHasShutdownTimeout config.example.json 必须包含 server.shutdown_timeout
// 且可直接加载（与 Default+normalize 同步）。
func TestConfigExampleJSONHasShutdownTimeout(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config.example.json: %v", err)
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("parse config.example.json: %v", err)
	}
	if cfg.ShutdownTimeoutDur != 30*time.Second {
		t.Fatalf("example shutdown timeout=%v want 30s", cfg.ShutdownTimeoutDur)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("re-parse example: %v", err)
	}
	var serverSec map[string]json.RawMessage
	if err := json.Unmarshal(probe["server"], &serverSec); err != nil {
		t.Fatalf("parse example server section: %v", err)
	}
	if _, ok := serverSec["shutdown_timeout"]; !ok {
		t.Error("config.example.json missing server.shutdown_timeout")
	}
}

// TestRestartRequiredIncludesShutdownTimeout shutdown_timeout 只在启动时读取一次，
// 面板保存后必须提示重启（否则「已保存但未生效」）。
func TestRestartRequiredIncludesShutdownTimeout(t *testing.T) {
	if !slices.Contains(restartRequiredFields(Default()), "server.shutdown_timeout") {
		t.Fatal("server.shutdown_timeout should be listed as restart-required")
	}
}
