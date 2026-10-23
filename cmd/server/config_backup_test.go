package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lei-rr/workbuddy2api-pro/internal/livecfg"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/scheduler"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// newRollbackDeps 构造回滚测试依赖（与 main 装配同型；空池即可，回滚只做热应用）。
func newRollbackDeps() (*livecfg.Holder, *pool.Pool, *upstream.Client, *scheduler.Scheduler) {
	return livecfg.New(livecfg.Snapshot{}), pool.New(""), upstream.New(), scheduler.New(scheduler.Config{})
}

// TestBackupConfigFile 备份：原子复制当前配置为 <path>.bak、只保留最近 1 份；
// 主配置不存在时不报错也不创建 .bak。
func TestBackupConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"api_key":"v1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bak, err := backupConfigFile(path)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if bak != path+".bak" {
		t.Fatalf("backup path=%q want %q", bak, path+".bak")
	}
	if data, err := os.ReadFile(bak); err != nil || string(data) != `{"api_key":"v1"}` {
		t.Fatalf("bak content=%q err=%v", data, err)
	}

	// 覆盖旧备份：只保留最近 1 份（第二次备份内容是「当前生效版本」v2）。
	if err := os.WriteFile(path, []byte(`{"api_key":"v2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := backupConfigFile(path); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(bak); string(data) != `{"api_key":"v2"}` {
		t.Fatalf("bak after second backup=%q want v2", data)
	}
	// 无临时文件残留。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("残留临时文件: %s", e.Name())
		}
	}

	// 主配置不存在（首次生成场景）：返回空路径、不报错、不创建 .bak。
	missing := filepath.Join(t.TempDir(), "missing.json")
	if got, err := backupConfigFile(missing); err != nil || got != "" {
		t.Fatalf("missing config backup=%q err=%v want empty/nil", got, err)
	}
	if _, err := os.Stat(missing + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing config must not create .bak: %v", err)
	}
}

// TestWriteConfigAtomic 原子写：先 tmp 再 rename（覆盖旧内容、无 tmp 残留）；
// 目标目录不存在时报错。
func TestWriteConfigAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := writeConfigAtomic(path, []byte(`{"v":1}`)); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	if err := writeConfigAtomic(path, []byte(`{"v":2,"longer":true}`)); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != `{"v":2,"longer":true}` {
		t.Fatalf("content=%q", data)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tmp must be removed after success: %v", err)
	}
	if err := writeConfigAtomic(filepath.Join(dir, "no-such-dir", "c.json"), []byte("x")); err == nil {
		t.Fatal("write into missing dir should fail")
	}
}

// TestRollbackConfigSuccess 回滚成功：.bak 整份写回主配置（不与当前配置合并）、
// 热应用到 livecfg/scheduler、返回需重启字段清单；备份保留可重复回滚。
func TestRollbackConfigSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	current := `{"listen":"127.0.0.1:7863","api_key":"sk-current","auth_dir":"./auths","state_file":"./data/state.json","panel":{"poll_interval_sec":11},"schedule":{"balance_refresh_concurrency":12}}`
	backup := `{"listen":"127.0.0.1:7864","api_key":"sk-backup","auth_dir":"./auths","state_file":"./data/state.json","panel":{"poll_interval_sec":9},"schedule":{"balance_refresh_concurrency":5}}`
	if err := os.WriteFile(path, []byte(current), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configBackupPath(path), []byte(backup), 0o600); err != nil {
		t.Fatal(err)
	}

	live, p, up, sch := newRollbackDeps()
	restart, err := rollbackConfig(path, live, p, up, sch)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != backup {
		t.Fatalf("回滚后主配置必须等于 .bak 原文：\n got=%s\nwant=%s", data, backup)
	}
	if got := live.Load(); got.APIKey != "sk-backup" || got.PollIntervalSec != 9 {
		t.Fatalf("回滚未热应用 livecfg: api_key=%q poll=%d", got.APIKey, got.PollIntervalSec)
	}
	if got := sch.BalanceRefreshConcurrency(); got != 5 {
		t.Fatalf("回滚未热应用 scheduler 并发: %d want 5", got)
	}
	wantCfg, err := ParseConfig([]byte(backup))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(restartRequiredFields(wantCfg), ",")
	if got := strings.Join(restart, ","); got != want {
		t.Fatalf("restart_required=%q want %q", got, want)
	}
	// 备份保留：可对同一版重复回滚。
	if _, err := os.Stat(configBackupPath(path)); err != nil {
		t.Fatalf(".bak 应保留：%v", err)
	}
}

// TestRollbackConfigNoBackup 无备份：返回 errNoConfigBackup（HTTP 层 400），主配置不动。
func TestRollbackConfigNoBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	current := `{"listen":"127.0.0.1:7863","api_key":"sk-current"}`
	if err := os.WriteFile(path, []byte(current), 0o600); err != nil {
		t.Fatal(err)
	}
	live, p, up, sch := newRollbackDeps()
	_, err := rollbackConfig(path, live, p, up, sch)
	if !errors.Is(err, errNoConfigBackup) {
		t.Fatalf("err=%v want errNoConfigBackup", err)
	}
	if !strings.Contains(err.Error(), "没有可回滚") {
		t.Fatalf("error should explain missing backup: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != current {
		t.Fatalf("无备份时主配置不得变化: %q", data)
	}
}

// TestRollbackConfigRejectsInvalidBackup 非法/危险备份：报错、不写盘（主配置保持原样）。
func TestRollbackConfigRejectsInvalidBackup(t *testing.T) {
	cases := []struct {
		name         string
		backup       string
		wantContains string
	}{
		{"invalid json", `{not json`, "备份配置校验失败"},
		{"empty auth_dir", `{"listen":"127.0.0.1:7863","api_key":"k","auth_dir":"","state_file":"./data/state.json"}`, "auth_dir"},
		{"empty state_file", `{"listen":"127.0.0.1:7863","api_key":"k","auth_dir":"./auths","state_file":" "}`, "state_file"},
		{"startup unsafe", `{"listen":"0.0.0.0:7863","api_key":"","auth_dir":"./auths","state_file":"./data/state.json"}`, "启动安全校验"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			current := `{"listen":"127.0.0.1:7863","api_key":"sk-current","auth_dir":"./auths","state_file":"./data/state.json"}`
			if err := os.WriteFile(path, []byte(current), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configBackupPath(path), []byte(tc.backup), 0o600); err != nil {
				t.Fatal(err)
			}
			live, p, up, sch := newRollbackDeps()
			_, err := rollbackConfig(path, live, p, up, sch)
			if err == nil {
				t.Fatalf("want error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantContains) {
				t.Fatalf("%s: error should contain %q, got: %v", tc.name, tc.wantContains, err)
			}
			if data, _ := os.ReadFile(path); string(data) != current {
				t.Fatalf("%s: 失败回滚不得改动主配置: %q", tc.name, data)
			}
		})
	}
}

// TestSaveBackupSemantics 模拟 patched saveConfig 的「校验 → 备份 → 写盘」顺序：
//   - 保存成功：.bak 是保存前的回滚点（旧版）；
//   - 校验失败：提前返回，不备份不写盘，.bak 保持上一版（不被覆盖）；
//   - 多次成功：.bak 只保留最近 1 份（上次成功保存的那一版）。
func TestSaveBackupSemantics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	v1 := `{"listen":"127.0.0.1:7863","api_key":"sk-a","pool":{"breaker_threshold":4}}`
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}

	// 第一次保存：校验通过 → 备份 v1 → 写盘 v2。
	v2 := `{"listen":"127.0.0.1:7863","api_key":"sk-a","pool":{"breaker_threshold":6}}`
	if _, err := ParseConfig([]byte(v2)); err != nil {
		t.Fatalf("v2 should be valid: %v", err)
	}
	if _, err := backupConfigFile(path); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigAtomic(path, []byte(v2)); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(configBackupPath(path)); string(data) != v1 {
		t.Fatalf("保存后 .bak 应为旧版 v1，got=%q", data)
	}

	// 第二次保存失败（校验不过）：saveConfig 在备份/写盘之前返回，.bak 与主配置都不动。
	if _, err := ParseConfig([]byte(`{"pool":{"strategy":{"domestic":"bogus"}}}`)); err == nil {
		t.Fatal("bogus strategy should fail validation")
	}
	if data, _ := os.ReadFile(configBackupPath(path)); string(data) != v1 {
		t.Fatalf("校验失败不得覆盖 .bak，got=%q", data)
	}
	if data, _ := os.ReadFile(path); string(data) != v2 {
		t.Fatalf("校验失败不得写盘，got=%q", data)
	}

	// 第三次保存成功：.bak 更新为 v2（只保留最近 1 份）。
	v3 := `{"listen":"127.0.0.1:7863","api_key":"sk-a","pool":{"breaker_threshold":8}}`
	if _, err := backupConfigFile(path); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigAtomic(path, []byte(v3)); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(configBackupPath(path)); string(data) != v2 {
		t.Fatalf(".bak 应更新为最近一次保存前的版本 v2，got=%q", data)
	}
}

// TestVerifyDangerousConfigValues 危险值拦截：auth_dir / state_file 不允许保存为空。
func TestVerifyDangerousConfigValues(t *testing.T) {
	if err := verifyDangerousConfigValues(Default()); err != nil {
		t.Fatalf("default config should pass: %v", err)
	}
	c := Default()
	c.AuthDir = ""
	if err := verifyDangerousConfigValues(c); err == nil || !strings.Contains(err.Error(), "auth_dir") {
		t.Fatalf("empty auth_dir err=%v", err)
	}
	c = Default()
	c.StateFile = "   "
	if err := verifyDangerousConfigValues(c); err == nil || !strings.Contains(err.Error(), "state_file") {
		t.Fatalf("blank state_file err=%v", err)
	}
}

// TestListenChangeAuditNote 监听面变化审计：回环 ↔ 非回环允许保存但留痕；
// 同类变化（端口/回环内地址）不产生审计噪音；老配置未写 listen 时按默认 :7863 判。
// oldRaw 必须是保存前原文（merge 会原地改写解析后的 map，这条测试钉住该口径）。
func TestListenChangeAuditNote(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		new  string
		want string // "" = 无审计；否则包含的子串
	}{
		{"loopback to all-interfaces", `{"listen":"127.0.0.1:7863"}`, ":7863", "回环 → 非回环"},
		{"all-interfaces to loopback", `{"listen":":7863"}`, "127.0.0.1:7863", "非回环 → 回环"},
		{"v6 loopback to lan", `{"listen":"[::1]:7863"}`, "0.0.0.0:7863", "回环 → 非回环"},
		{"same class port change", `{"listen":":7863"}`, ":9999", ""},
		{"same loopback addr change", `{"listen":"127.0.0.1:7863"}`, "localhost:9999", ""},
		{"legacy missing defaults to all-interfaces", `{}`, "127.0.0.1:7863", "非回环 → 回环"},
		{"legacy missing to another all-interfaces port", `{}`, ":9999", ""},
		{"unparsable old raw falls back to default", `not json`, "127.0.0.1:7863", "非回环 → 回环"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := listenChangeAuditNote([]byte(tc.raw), tc.new)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("want no audit note, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("note=%q should contain %q", got, tc.want)
			}
		})
	}
}
