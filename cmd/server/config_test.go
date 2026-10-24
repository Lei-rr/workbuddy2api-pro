package main

import (
	"encoding/json"
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

func TestPanelPackageDetailLimit(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Panel.PackageDetailLimit != 5 {
		t.Fatalf("default package_detail_limit=%d want 5", c.Panel.PackageDetailLimit)
	}

	configured, err := ParseConfig([]byte(`{"panel":{"package_detail_limit":8}}`))
	if err != nil {
		t.Fatalf("parse configured limit: %v", err)
	}
	if configured.Panel.PackageDetailLimit != 8 {
		t.Fatalf("configured package_detail_limit=%d want 8", configured.Panel.PackageDetailLimit)
	}

	fallback, err := ParseConfig([]byte(`{"panel":{"package_detail_limit":0}}`))
	if err != nil {
		t.Fatalf("parse fallback limit: %v", err)
	}
	if fallback.Panel.PackageDetailLimit != 5 {
		t.Fatalf("fallback package_detail_limit=%d want 5", fallback.Panel.PackageDetailLimit)
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

func TestBadDuration(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"not-a-duration"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad duration")
	}
}

func TestHardCreditKeyIgnored(t *testing.T) {
	// 退役的 hard_credit 键作为 JSON 未知字段被自然忽略，不报错。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"hard_credit":"not-a-duration","soft_rate":"30s"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("hard_credit must be ignored (not validated): %v", err)
	}
	if c.SoftRateDur.Seconds() != 30 {
		t.Errorf("soft_rate=%v want 30s", c.SoftRateDur)
	}
}

func TestNewPoolConfigDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.MaxInFlight != 3 {
		t.Errorf("max_in_flight=%d want 3", c.Pool.MaxInFlight)
	}
	if c.Pool.BreakerThreshold != 3 {
		t.Errorf("breaker_threshold=%d want 3", c.Pool.BreakerThreshold)
	}
	if c.BreakerCooldownDur.Minutes() != 30 {
		t.Errorf("breaker_cooldown=%v want 30m", c.BreakerCooldownDur)
	}
	if c.BreakerCooldownMaxD.Hours() != 6 {
		t.Errorf("breaker_cooldown_max=%v want 6h", c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.5 || c.Pool.IdleWeightMax != 5.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if !c.Pool.PreferExpiring || c.ExpiringSoonDur != 7*24*time.Hour {
		t.Errorf("expiring defaults: enabled=%v window=%v", c.Pool.PreferExpiring, c.ExpiringSoonDur)
	}
	if c.SoftRateMaxDur.Hours() != 2 {
		t.Errorf("soft_rate_max=%v want 2h", c.SoftRateMaxDur)
	}
	if !c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want true")
	}
	if c.SessionTTL.Minutes() != 30 || c.SessionGCInterval.Minutes() != 5 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		t.Errorf("upstash default should be empty: %+v", c.Upstash)
	}
}

func TestPoolConfigParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{
		"upstash":{"url":"https://foo.upstash.io","token":"tok"},
		"pool":{
			"max_in_flight":5,
			"breaker_threshold":4,
			"breaker_cooldown":"10m",
			"breaker_cooldown_max":"2h",
			"idle_weight_per_hour":0.7,
			"idle_weight_max":8.0,
			"prefer_expiring":false,
			"expiring_soon":"72h"
		},
		"session_sticky":{"enabled":false,"ttl":"1h","gc_interval":"2m"}
	}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstash.URL != "https://foo.upstash.io" || c.Upstash.Token != "tok" {
		t.Errorf("upstash=%+v", c.Upstash)
	}
	if c.Pool.MaxInFlight != 5 || c.Pool.BreakerThreshold != 4 {
		t.Errorf("pool=%+v", c.Pool)
	}
	if c.BreakerCooldownDur.Minutes() != 10 || c.BreakerCooldownMaxD.Hours() != 2 {
		t.Errorf("breaker durations=%v/%v", c.BreakerCooldownDur, c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.7 || c.Pool.IdleWeightMax != 8.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if c.Pool.PreferExpiring || c.ExpiringSoonDur != 72*time.Hour {
		t.Errorf("expiring override: enabled=%v window=%v", c.Pool.PreferExpiring, c.ExpiringSoonDur)
	}
	if c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want false from file")
	}
	if c.SessionTTL.Hours() != 1 || c.SessionGCInterval.Minutes() != 2 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
}

func TestSoftRateMaxParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"5m","soft_rate_max":"45m"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.SoftRateDur.Minutes() != 5 {
		t.Errorf("soft_rate=%v want 5m", c.SoftRateDur)
	}
	if c.SoftRateMaxDur.Minutes() != 45 {
		t.Errorf("soft_rate_max=%v want 45m", c.SoftRateMaxDur)
	}
}

func TestLegacyConfigKeepsPreferExpiringEnabled(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"idle_weight_max":3}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Pool.PreferExpiring {
		t.Fatal("missing prefer_expiring must preserve default true")
	}
}

func TestNegativeExpiringSoonClampsToDisabled(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"expiring_soon":"-1h"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.ExpiringSoonDur != 0 || c.Pool.ExpiringSoon != "0" {
		t.Fatalf("negative window=%v/%q want 0/0", c.ExpiringSoonDur, c.Pool.ExpiringSoon)
	}
}

func TestSoftRateMaxEmptyFallsBackToDefault(t *testing.T) {
	// 键缺席 → Default() 的 2h 保留（空串无法 ParseDuration）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"90s"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.SoftRateMaxDur.Hours() != 2 {
		t.Errorf("soft_rate_max=%v want 2h fallback", c.SoftRateMaxDur)
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

func TestUpstreamTimeoutDefaults(t *testing.T) {
	// 默认：header 回落 timeout，idle 回落 300。
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Upstream.TimeoutSeconds != 120 {
		t.Errorf("timeout_seconds=%d want 120", c.Upstream.TimeoutSeconds)
	}
	if c.Upstream.HeaderTimeoutSeconds != 120 {
		t.Errorf("header_timeout_seconds=%d want fallback 120", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamHeaderFallsBackToTimeout(t *testing.T) {
	// 只设 timeout_seconds：header 回落同值，idle 回落 300。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":60}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 60 {
		t.Errorf("header_timeout_seconds=%d want fallback 60", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamExplicitHeaderIdle(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":120,"header_timeout_seconds":30,"idle_timeout_seconds":600}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 30 {
		t.Errorf("header_timeout_seconds=%d want 30", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 600 {
		t.Errorf("idle_timeout_seconds=%d want 600", c.Upstream.IdleTimeoutSeconds)
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

// TestScheduleEnabledByDefault 四个任务的 enabled 开关默认均为 true：
// 老 config 不写这些键，行为必须与从前完全一致。
func TestScheduleEnabledByDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("enabled defaults want true/true, got %v/%v",
			c.Schedule.CheckinEnabled, c.Schedule.KeepaliveEnabled)
	}
	if !c.Schedule.TravelEnabled || !c.Schedule.ActivityEnabled {
		t.Errorf("travel/activity enabled defaults want true/true, got %v/%v",
			c.Schedule.TravelEnabled, c.Schedule.ActivityEnabled)
	}
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want [9,21]", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want [10]", c.Schedule.ActivityHours)
	}
}

// TestScheduleLegacyConfigKeepsRunning 老 config（只写签到/保活小时数组，无新键）加载后仍是启用态，
// 新开关缺省 true、新 hours 回落默认——对老配置零影响。
func TestScheduleLegacyConfigKeepsRunning(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_hours":[9,21],"keepalive_hours":[22]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("legacy config must stay enabled: %+v", c.Schedule)
	}
	if !c.Schedule.TravelEnabled || !c.Schedule.ActivityEnabled {
		t.Errorf("new switches must default true on legacy config: %+v", c.Schedule)
	}
	if len(c.Schedule.CheckinHours) != 2 {
		t.Errorf("checkin_hours=%v", c.Schedule.CheckinHours)
	}
	// 新 hours 缺省 → 回落默认（非空）。
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want default [9,21]", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want default [10]", c.Schedule.ActivityHours)
	}
}

// TestScheduleExplicitDisable 显式 checkin_enabled=false 即可真正关掉签到
// （issue #27 边界：此前无论怎么配小时都关不掉）。
func TestScheduleExplicitDisable(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_enabled":false,"keepalive_enabled":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CheckinEnabled || c.Schedule.KeepaliveEnabled {
		t.Errorf("want both disabled: %+v", c.Schedule)
	}
	// 小时数组仍回落默认值（禁用与默认值互不干扰：重新启用无需补配小时）。
	if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 9 || c.Schedule.CheckinHours[1] != 21 {
		t.Errorf("checkin_hours=%v want default [9 21] even when disabled", c.Schedule.CheckinHours)
	}
	if len(c.Schedule.KeepaliveHours) != 1 || c.Schedule.KeepaliveHours[0] != 22 {
		t.Errorf("keepalive_hours=%v want default [22] even when disabled", c.Schedule.KeepaliveHours)
	}
}

// TestScheduleTravelActivityExplicitDisable 显式关闭旅行/活跃上报开关。
func TestScheduleTravelActivityExplicitDisable(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_enabled":false,"activity_enabled":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.TravelEnabled || c.Schedule.ActivityEnabled {
		t.Errorf("want travel/activity disabled: %+v", c.Schedule)
	}
	// 签到/保活开关缺省 true（互不干扰）。
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("checkin/keepalive should stay enabled: %+v", c.Schedule)
	}
	// hours 仍回落默认。
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want default [9,21] even when disabled", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want default [10] even when disabled", c.Schedule.ActivityHours)
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

// TestScheduleTravelActivityExplicitHours 显式配置旅行/活跃小时。
func TestScheduleTravelActivityExplicitHours(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_hours":[9,21],"activity_hours":[11]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want [9 21]", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 11 {
		t.Errorf("activity_hours=%v want [11]", c.Schedule.ActivityHours)
	}
}

// TestScheduleDisableKeepsExplicitHours 禁用不擦除用户配置的小时（便于原样恢复）。
func TestScheduleDisableKeepsExplicitHours(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_enabled":false,"checkin_hours":[10,14]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CheckinEnabled {
		t.Error("checkin should be disabled")
	}
	if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 10 || c.Schedule.CheckinHours[1] != 14 {
		t.Errorf("explicit hours must be preserved: %v", c.Schedule.CheckinHours)
	}
}

// TestScheduleEmptyHoursFallsBackToDefault 空数组 / null / 缺省都视同「未配置」→ 回落默认。
func TestScheduleEmptyHoursFallsBackToDefault(t *testing.T) {
	cases := map[string]string{
		"absent":   `{}`,
		"empty":    `{"schedule":{}}`,
		"null":     `{"schedule":{"checkin_hours":null,"keepalive_hours":null,"travel_hours":null,"activity_hours":null}}`,
		"emptyarr": `{"schedule":{"checkin_hours":[],"keepalive_hours":[],"travel_hours":[],"activity_hours":[]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			fp := filepath.Join(dir, "c.json")
			os.WriteFile(fp, []byte(body), 0o600)
			c, err := Load(fp)
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 9 || c.Schedule.CheckinHours[1] != 21 {
				t.Errorf("checkin_hours=%v want default [9 21]", c.Schedule.CheckinHours)
			}
			if len(c.Schedule.KeepaliveHours) != 1 || c.Schedule.KeepaliveHours[0] != 22 {
				t.Errorf("keepalive_hours=%v want default [22]", c.Schedule.KeepaliveHours)
			}
			if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
				t.Errorf("travel_hours=%v want default [9 21]", c.Schedule.TravelHours)
			}
			if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
				t.Errorf("activity_hours=%v want default [10]", c.Schedule.ActivityHours)
			}
			if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
				t.Errorf("empty hours must not imply disabled: %+v", c.Schedule)
			}
			if !c.Schedule.TravelEnabled || !c.Schedule.ActivityEnabled {
				t.Errorf("empty hours must not imply disabled: %+v", c.Schedule)
			}
		})
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

// TestPromptDefaultPassthrough 默认 prompt.mode=passthrough（对齐上游：透传客户端
// 原始 system 是更保守的缺省）；custom 由用户显式选择，此时 PromptText 为内置默认（非空）。
func TestPromptDefaultPassthrough(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("prompt.mode=%q want passthrough", c.Prompt.Mode)
	}
	// passthrough 不加载提示词文本（透传客户端 system）；切 custom 时 normalize 会加载。
}

// TestPromptExplicitPassthrough passthrough 模式不加载文本（透传客户端原始 system）。
func TestPromptExplicitPassthrough(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"passthrough"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("mode=%q want passthrough", c.Prompt.Mode)
	}
	if c.PromptText != "" {
		t.Errorf("passthrough should not load PromptText, got len=%d", len(c.PromptText))
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

// TestPromptFileOverride 自定义 file 覆盖内置默认。
func TestPromptFileOverride(t *testing.T) {
	dir := t.TempDir()
	pf := filepath.Join(dir, "my.md")
	want := "我的自定义人格入口"
	os.WriteFile(pf, []byte(want), 0o600)
	cf := filepath.Join(dir, "c.json")
	// 用 json.Marshal 拼路径：Windows 反斜杠必须转义，手工字符串拼接会产出非法 JSON。
	cfgJSON, err := json.Marshal(map[string]any{"prompt": map[string]any{"mode": "custom", "file": pf}})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(cf, cfgJSON, 0o600)
	c, err := Load(cf)
	if err != nil {
		t.Fatal(err)
	}
	if c.PromptText != want {
		t.Errorf("PromptText=%q want %q", c.PromptText, want)
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

// TestPromptLegacyConfigNoImpact 旧 config（无 prompt 段）零影响：mode 缺省 passthrough。
func TestPromptLegacyConfigNoImpact(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("legacy config should default to passthrough, got %q", c.Prompt.Mode)
	}
	if c.Listen != ":9999" {
		t.Errorf("listen=%q", c.Listen)
	}
}

// TestUpstreamUserAgentConfig 配置 upstream.user_agent 与 env WB2A_USER_AGENT 均生效，
// 缺省空串保持现状（headers 层回落到 clientUA）。
func TestUpstreamUserAgentConfig(t *testing.T) {
	// JSON 配置
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"user_agent":"WorkBuddy/1.2.3"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.UserAgent != "WorkBuddy/1.2.3" {
		t.Errorf("user_agent=%q want WorkBuddy/1.2.3", c.Upstream.UserAgent)
	}
	// 缺省为空
	if c2, err := Load(""); err != nil || c2.Upstream.UserAgent != "" {
		t.Errorf("default user_agent=%q want empty (err=%v)", c2.Upstream.UserAgent, err)
	}
	// env 覆盖
	t.Setenv("WB2A_USER_AGENT", "EnvAgent/9")
	c3, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c3.Upstream.UserAgent != "EnvAgent/9" {
		t.Errorf("env user_agent=%q want EnvAgent/9", c3.Upstream.UserAgent)
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

// ---------------------------------------------------------------------------
// v2.0.1 配置基座（#2 / #4 / #10 / #11 共用）：键名与 JSON schema 契约。
// ---------------------------------------------------------------------------

// TestPanelConfigV201 面板配置：api_key 分离、poll_interval_sec 默认 5 / clamp 2-60、
// security.allow_anonymous 默认 false。
func TestPanelConfigV201(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Panel.APIKey != "" {
		t.Errorf("default panel.api_key=%q want empty (回落网关 key)", c.Panel.APIKey)
	}
	if c.Panel.PollIntervalSec != 5 {
		t.Errorf("default panel.poll_interval_sec=%d want 5", c.Panel.PollIntervalSec)
	}
	if c.Security.AllowAnonymous {
		t.Error("default security.allow_anonymous must be false")
	}

	got, err := ParseConfig([]byte(`{"panel":{"api_key":"sk-panel","poll_interval_sec":10},"security":{"allow_anonymous":true}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Panel.APIKey != "sk-panel" || got.Panel.PollIntervalSec != 10 || !got.Security.AllowAnonymous {
		t.Fatalf("parsed panel/security = %+v / %+v", got.Panel, got.Security)
	}

	// clamp：<2 → 2；<=0（显式 0）→ 默认 5；>60 → 60；边界保留。
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"panel":{"poll_interval_sec":0}}`, 5},
		{`{"panel":{"poll_interval_sec":-3}}`, 5},
		{`{"panel":{"poll_interval_sec":1}}`, 2},
		{`{"panel":{"poll_interval_sec":2}}`, 2},
		{`{"panel":{"poll_interval_sec":60}}`, 60},
		{`{"panel":{"poll_interval_sec":61}}`, 60},
		{`{"panel":{"poll_interval_sec":10000}}`, 60},
	} {
		c, err := ParseConfig([]byte(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		if c.Panel.PollIntervalSec != tc.want {
			t.Errorf("%s: poll_interval_sec=%d want %d", tc.body, c.Panel.PollIntervalSec, tc.want)
		}
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

// TestScheduleConfigV201Defaults 调度器新键默认值（后续 #10/#11 任务复用这些字段名）。
func TestScheduleConfigV201Defaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Schedule.CatchUpMinutes != 120 {
		t.Errorf("default catch_up_minutes=%d want 120", c.Schedule.CatchUpMinutes)
	}
	if c.Schedule.BalanceRefreshConcurrency != 8 {
		t.Errorf("default balance_refresh_concurrency=%d want 8", c.Schedule.BalanceRefreshConcurrency)
	}
	if c.Schedule.BalanceRefreshStartupDelay != "30s" || c.BalanceRefreshStartupDelayDur != 30*time.Second {
		t.Errorf("default startup_delay=%q/%v want 30s", c.Schedule.BalanceRefreshStartupDelay, c.BalanceRefreshStartupDelayDur)
	}
	if c.Schedule.BalanceRefreshStartupForce {
		t.Error("default balance_refresh_startup_force must be false")
	}
}

// TestScheduleConfigV201Parsed 显式配置与 clamp 语义。
func TestScheduleConfigV201Parsed(t *testing.T) {
	got, err := ParseConfig([]byte(`{"schedule":{"catch_up_minutes":30,"balance_refresh_concurrency":16,"balance_refresh_startup_delay":"90s","balance_refresh_startup_force":true}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Schedule.CatchUpMinutes != 30 || got.Schedule.BalanceRefreshConcurrency != 16 {
		t.Errorf("parsed schedule = %+v", got.Schedule)
	}
	if got.BalanceRefreshStartupDelayDur != 90*time.Second || !got.Schedule.BalanceRefreshStartupForce {
		t.Errorf("parsed startup = %q/%v force=%v",
			got.Schedule.BalanceRefreshStartupDelay, got.BalanceRefreshStartupDelayDur, got.Schedule.BalanceRefreshStartupForce)
	}

	// "0" 是合法值：不延迟。
	zero, err := ParseConfig([]byte(`{"schedule":{"balance_refresh_startup_delay":"0"}}`))
	if err != nil {
		t.Fatalf("parse zero delay: %v", err)
	}
	if zero.BalanceRefreshStartupDelayDur != 0 {
		t.Errorf("zero delay=%v want 0", zero.BalanceRefreshStartupDelayDur)
	}

	// clamp：catch_up 负 → 0，>720 → 720；并发 0/负 → 8，>32 → 32。
	for _, tc := range []struct {
		body          string
		catchUp, conc int
	}{
		{`{"schedule":{"catch_up_minutes":-5}}`, 0, 8},
		{`{"schedule":{"catch_up_minutes":721}}`, 720, 8},
		{`{"schedule":{"catch_up_minutes":720}}`, 720, 8},
		{`{"schedule":{"balance_refresh_concurrency":0}}`, 120, 8},
		{`{"schedule":{"balance_refresh_concurrency":-2}}`, 120, 8},
		{`{"schedule":{"balance_refresh_concurrency":33}}`, 120, 32},
	} {
		c, err := ParseConfig([]byte(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		if c.Schedule.CatchUpMinutes != tc.catchUp || c.Schedule.BalanceRefreshConcurrency != tc.conc {
			t.Errorf("%s: catch_up=%d conc=%d want %d/%d", tc.body,
				c.Schedule.CatchUpMinutes, c.Schedule.BalanceRefreshConcurrency, tc.catchUp, tc.conc)
		}
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
