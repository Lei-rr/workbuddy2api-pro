// observability_test.go /readyz 与 /metrics（v2.2.0 #8 部分）的契约测试。
package panel

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
)

// observabilityTestPanel 构造「就绪」的面板：配置文件存在、auth_dir 可写、
// state 同目录可写、池已装配。返回面板、池与临时数据目录。
func observabilityTestPanel(t *testing.T) (*Panel, *pool.Pool, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"api_key":"x"}`), 0o600); err != nil {
		t.Fatalf("写测试 config: %v", err)
	}
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatalf("建 auth_dir: %v", err)
	}
	pl := pool.New("")
	p := New(Config{
		Version: "test", APIKey: accountsTestKey, Pool: pl,
		ConfigPath: cfgPath,
		AuthDir:    authDir,
		StateFile:  filepath.Join(dir, "state.json"),
	})
	return p, pl, dir
}

// TestReadyzOK 关键项全通过 → 200 + ok:true + checks 逐项 true + redis:null。
func TestReadyzOK(t *testing.T) {
	p, _, _ := observabilityTestPanel(t)
	rec := panelRequest(p, http.MethodGet, "/panel/api/readyz", accountsTestKey, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp readyzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode readyz: %v body=%s", err, rec.Body.String())
	}
	if !resp.OK || len(resp.Errors) != 0 {
		t.Fatalf("ok=%v errors=%v want true/[]", resp.OK, resp.Errors)
	}
	c := resp.Checks
	if !c.Config || !c.AuthDirWritable || !c.StateDirWritable || !c.PoolLoaded {
		t.Fatalf("checks=%+v want 全 true", c)
	}
	if c.Redis != nil {
		t.Fatalf("RedisMode=noop 时 redis 应为 null，got %+v", c.Redis)
	}
	if c.UptimeSec < 0 {
		t.Fatalf("uptime_sec=%d 应为非负", c.UptimeSec)
	}
}

// TestReadyzFailures 逐个关键项失败 → 503 + 对应 check=false + errors 列出失败项。
func TestReadyzFailures(t *testing.T) {
	t.Run("配置缺失", func(t *testing.T) {
		p, _, dir := observabilityTestPanel(t)
		p.cfg.ConfigPath = filepath.Join(dir, "missing.json")
		assertNotReady(t, p, "config")
	})
	t.Run("auth_dir 是普通文件", func(t *testing.T) {
		p, _, dir := observabilityTestPanel(t)
		file := filepath.Join(dir, "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("写文件: %v", err)
		}
		p.cfg.AuthDir = file
		assertNotReady(t, p, "auth_dir_writable")
	})
	t.Run("state 目录不存在", func(t *testing.T) {
		p, _, dir := observabilityTestPanel(t)
		p.dataDir = filepath.Join(dir, "no-such-dir")
		p.auditLog = newJSONLLog(filepath.Join(p.dataDir, auditFileName))
		p.taskRunsLog = newJSONLLog(filepath.Join(p.dataDir, taskRunsFileName))
		assertNotReady(t, p, "state_dir_writable")
	})
	t.Run("池未装配", func(t *testing.T) {
		p, _, _ := observabilityTestPanel(t)
		p.cfg.Pool = nil
		assertNotReady(t, p, "pool_loaded")
	})
}

// assertNotReady 断言 readyz 返回 503 且失败列表含指定项名。
func assertNotReady(t *testing.T, p *Panel, wantFailure string) {
	t.Helper()
	rec := panelRequest(p, http.MethodGet, "/panel/api/readyz", accountsTestKey, "", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status=%d want 503 body=%s", rec.Code, rec.Body.String())
	}
	var resp readyzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode readyz: %v", err)
	}
	if resp.OK {
		t.Fatal("ok=true want false")
	}
	found := false
	for _, e := range resp.Errors {
		if e == wantFailure {
			found = true
		}
	}
	if !found {
		t.Fatalf("errors=%v 应含 %q", resp.Errors, wantFailure)
	}
}

// TestReadyzRedisMode RedisMode=upstash 时 checks.redis 透出模式（信息项，不影响就绪）。
func TestReadyzRedisMode(t *testing.T) {
	p, _, _ := observabilityTestPanel(t)
	p.cfg.RedisMode = "upstash"
	rec := panelRequest(p, http.MethodGet, "/panel/api/readyz", accountsTestKey, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp readyzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Checks.Redis == nil || resp.Checks.Redis.Mode != "upstash" {
		t.Fatalf("redis=%+v want mode=upstash", resp.Checks.Redis)
	}
}

// TestReadyzRequiresAuth readyz 走 withAuth。
func TestReadyzRequiresAuth(t *testing.T) {
	p, _, _ := observabilityTestPanel(t)
	if rec := panelRequest(p, http.MethodGet, "/panel/api/readyz", "", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无密钥 status=%d want 401", rec.Code)
	}
}

// TestMetricsOutput Prometheus text 格式与关键指标存在（固定低基数标签）。
func TestMetricsOutput(t *testing.T) {
	p, pl, _ := observabilityTestPanel(t)
	// 2 healthy + 1 cooling + 1 disabled + 1 paused。
	for i := 0; i < 2; i++ {
		pl.Add(&auth.Auth{UID: "h" + strconv.Itoa(i)})
		pl.SetCredits("h"+strconv.Itoa(i), 10, 0)
	}
	pl.Add(&auth.Auth{UID: "c1"})
	pl.Cooldown("c1", pool.CoolSoft, 10*time.Minute, "429 rate limit")
	pl.Add(&auth.Auth{UID: "d1"})
	pl.Disable("d1", "dead")
	pl.Add(&auth.Auth{UID: "p1"})
	pl.Pause("p1")

	rec := panelRequest(p, http.MethodGet, "/panel/api/metrics", accountsTestKey, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status=%d body=%s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") || !strings.Contains(ct, "version=0.0.4") {
		t.Fatalf("Content-Type=%q want Prometheus text v0.0.4", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE workbuddy_up gauge",
		"workbuddy_up 1",
		"workbuddy_uptime_seconds ",
		`workbuddy_accounts_total{state="healthy"} 2`,
		`workbuddy_accounts_total{state="cooling"} 1`,
		`workbuddy_accounts_total{state="disabled"} 1`,
		`workbuddy_accounts_total{state="paused"} 1`,
		"workbuddy_in_flight 0",
		"workbuddy_credits_sum 20",
		"workbuddy_credits_total_sum 0",
		"workbuddy_model_locks 0",
		"# TYPE workbuddy_panel_requests_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics 缺少 %q\n--- body ---\n%s", want, body)
		}
	}
	if !strings.HasSuffix(body, "\n") {
		t.Fatal("metrics body 应以换行结尾")
	}

	// 请求计数随抓取递增（本次请求已在 ServeHTTP 计数）。
	if got := metricValue(t, body, "workbuddy_panel_requests_total"); got < 1 {
		t.Fatalf("panel_requests_total=%d want >=1", got)
	}
	rec2 := panelRequest(p, http.MethodGet, "/panel/api/metrics", accountsTestKey, "", nil)
	if got := metricValue(t, rec2.Body.String(), "workbuddy_panel_requests_total"); got < 2 {
		t.Fatalf("第二次抓取 panel_requests_total=%d want >=2", got)
	}
}

// metricValue 从 Prometheus text 中取无标签指标的值（找不到返回 -1）。
func metricValue(t *testing.T, body, name string) int64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name+" ") {
			if v, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, name+" ")), 10, 64); err == nil {
				return v
			}
		}
	}
	t.Fatalf("metrics 里找不到指标 %q", name)
	return -1
}

// TestMetricsRequiresAuth metrics 走 withAuth。
func TestMetricsRequiresAuth(t *testing.T) {
	p, _, _ := observabilityTestPanel(t)
	if rec := panelRequest(p, http.MethodGet, "/panel/api/metrics", "", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无密钥 status=%d want 401", rec.Code)
	}
}

// TestDirWritable 可写性探测：空路径/普通文件/不存在目录 → false；可写目录 → true。
func TestDirWritable(t *testing.T) {
	dir := t.TempDir()
	if !dirWritable(dir) {
		t.Fatal("临时目录应可写")
	}
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	for _, bad := range []string{"", file, filepath.Join(dir, "missing")} {
		if dirWritable(bad) {
			t.Errorf("dirWritable(%q)=true want false", bad)
		}
	}
	// 探测不留下临时文件。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("目录应只有测试文件，got %d 项（探测残留？）", len(entries))
	}
}
