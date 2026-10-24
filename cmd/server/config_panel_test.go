package main

import (
	"testing"
)

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
