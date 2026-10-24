package main

import (
	"testing"
	"time"
)

func TestEnvOverride(t *testing.T) {
	t.Setenv("WB2A_LISTEN", ":7777")
	t.Setenv("WB2A_API_KEY", "envkey")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":7777" || c.APIKey != "envkey" {
		t.Errorf("c=%+v", c)
	}
}

func TestUpstreamEnvOverride(t *testing.T) {
	t.Setenv("WB2A_HEADER_TIMEOUT_SECONDS", "45")
	t.Setenv("WB2A_IDLE_TIMEOUT_SECONDS", "900")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 45 {
		t.Errorf("header_timeout_seconds=%d want env 45", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 900 {
		t.Errorf("idle_timeout_seconds=%d want env 900", c.Upstream.IdleTimeoutSeconds)
	}
}

// TestPromptEnvOverride env 覆盖 prompt.mode 与 prompt.file。
func TestPromptEnvOverride(t *testing.T) {
	t.Setenv("WB2A_PROMPT_MODE", "passthrough")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("mode=%q want passthrough", c.Prompt.Mode)
	}
}

// TestPanelConfigV201Env 面板/安全配置的环境变量覆盖（WB2A_PANEL_KEY 等）。
func TestPanelConfigV201Env(t *testing.T) {
	t.Setenv("WB2A_PANEL_KEY", "env-panel-key")
	t.Setenv("WB2A_POLL_INTERVAL_SEC", "12")
	t.Setenv("WB2A_ALLOW_ANONYMOUS", "true")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Panel.APIKey != "env-panel-key" {
		t.Errorf("panel.api_key=%q want env-panel-key", c.Panel.APIKey)
	}
	if c.Panel.PollIntervalSec != 12 {
		t.Errorf("panel.poll_interval_sec=%d want 12", c.Panel.PollIntervalSec)
	}
	if !c.Security.AllowAnonymous {
		t.Error("security.allow_anonymous env ignored")
	}
}

// TestScheduleConfigV201Env 调度器新键的环境变量覆盖。
func TestScheduleConfigV201Env(t *testing.T) {
	t.Setenv("WB2A_CATCH_UP_MINUTES", "45")
	t.Setenv("WB2A_BALANCE_REFRESH_CONCURRENCY", "12")
	t.Setenv("WB2A_BALANCE_REFRESH_STARTUP_DELAY", "45s")
	t.Setenv("WB2A_BALANCE_REFRESH_STARTUP_FORCE", "true")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CatchUpMinutes != 45 || c.Schedule.BalanceRefreshConcurrency != 12 {
		t.Errorf("env schedule = %+v", c.Schedule)
	}
	if c.BalanceRefreshStartupDelayDur != 45*time.Second || !c.Schedule.BalanceRefreshStartupForce {
		t.Errorf("env startup = %q/%v force=%v",
			c.Schedule.BalanceRefreshStartupDelay, c.BalanceRefreshStartupDelayDur, c.Schedule.BalanceRefreshStartupForce)
	}
	// 非法 env duration 同一套 fail fast。
	t.Setenv("WB2A_BALANCE_REFRESH_STARTUP_DELAY", "not-a-duration")
	if _, err := Load(""); err == nil {
		t.Fatal("invalid env startup delay should fail load")
	}
}
