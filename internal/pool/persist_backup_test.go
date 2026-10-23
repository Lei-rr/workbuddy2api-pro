// persist_backup_test.go 钉住 v2.2 #13 的 state.json 可靠性语义：
//
//   - 每次主文件**写成功后**把上一版滚动为 state.json.bak（保留最近 1 份）；
//   - 主文件损坏/不可读 → 隔离坏文件（.corrupt-<ts> 保留抢救价值）→ 自动回退 .bak；
//   - .bak 缺失/同样损坏 → 空池启动（非 fatal，与引入前行为一致）且不 panic；
//   - 主文件写失败时 .bak 保持上一份可用内容，绝不被半成品覆盖。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
)

// writeStateJSON 把 stateFile 序列化写入 path（测试造 state.json / .bak）。
func writeStateJSON(t *testing.T, path string, sf stateFile) {
	t.Helper()
	raw, err := json.Marshal(sf)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// stateWithCredit 构造只含一个账号、指定余额的最小 stateFile。
func stateWithCredit(uid string, credits int64) stateFile {
	return stateFile{Accounts: map[string]stateAccount{uid: {Credits: credits}}}
}

// corruptFiles 返回主文件旁被隔离的损坏文件（state.json.corrupt-*）。
func corruptFiles(t *testing.T, statePath string) []string {
	t.Helper()
	matches, err := filepath.Glob(statePath + ".corrupt-*")
	if err != nil {
		t.Fatalf("glob corrupt: %v", err)
	}
	return matches
}

// TestStateBackupRollsPreviousVersion 首次写无旧文件 → 不产生 .bak；第二次写成功后
// .bak 恰好等于上一版主文件内容（仅 u1），主文件是最新内容（u1+u2）。
func TestStateBackupRollsPreviousVersion(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)

	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 11, 0) // 置 dirty
	p.Flush()                 // 首次写：无上一版 → 不写 .bak
	if _, err := os.Stat(fp + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("first save should not create .bak, stat err=%v", err)
	}

	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u2", 22, 0)
	p.Flush() // 第二次写：上一版（u1/11）滚动为 .bak

	bakRaw, err := os.ReadFile(fp + ".bak")
	if err != nil {
		t.Fatalf("read .bak: %v", err)
	}
	var bak stateFile
	if err := json.Unmarshal(bakRaw, &bak); err != nil {
		t.Fatalf("parse .bak: %v", err)
	}
	if len(bak.Accounts) != 1 || bak.Accounts["u1"].Credits != 11 {
		t.Fatalf(".bak should be previous version (u1=11 only), got %+v", bak.Accounts)
	}

	var cur stateFile
	curRaw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read main: %v", err)
	}
	if err := json.Unmarshal(curRaw, &cur); err != nil {
		t.Fatalf("parse main: %v", err)
	}
	if len(cur.Accounts) != 2 {
		t.Fatalf("main should have 2 accounts, got %+v", cur.Accounts)
	}
}

// TestCorruptStateFallsBackToBackup 主文件损坏 + .bak 有效 → 自动恢复 .bak 的账号，
// 坏文件被隔离保留，主文件按恢复后状态重写（再次启动走正常加载）。
func TestCorruptStateFallsBackToBackup(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	writeStateJSON(t, fp+".bak", stateWithCredit("u1", 777))
	if err := os.WriteFile(fp, []byte(`{"accounts": {"u1": {oops`), 0o600); err != nil {
		t.Fatal(err)
	}

	p := New(fp)
	st, ok := p.Status("u1")
	if !ok || st.Credits != 777 {
		t.Fatalf("should recover u1 credits=777 from .bak, got %+v ok=%v", st, ok)
	}
	if got := corruptFiles(t, fp); len(got) != 1 {
		t.Fatalf("corrupt state should be quarantined once, got %v", got)
	}
	// .bak 在恢复回写时不得被覆盖（损坏主文件已隔离 → 无「上一版」可滚动）。
	bakRaw, err := os.ReadFile(fp + ".bak")
	if err != nil {
		t.Fatalf("read .bak after recovery: %v", err)
	}
	var bak stateFile
	if err := json.Unmarshal(bakRaw, &bak); err != nil || bak.Accounts["u1"].Credits != 777 {
		t.Fatalf("recovery must keep .bak intact: err=%v accounts=%+v", err, bak.Accounts)
	}
	// 主文件已被恢复状态重写 → 再次启动能正常加载（不再走损坏路径）。
	p2 := New(fp)
	if st2, ok := p2.Status("u1"); !ok || st2.Credits != 777 {
		t.Fatalf("rewritten main should reload u1=777, got %+v ok=%v", st2, ok)
	}
	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("main state.json should be rewritten after recovery: %v", err)
	}
}

// TestCorruptStateWithoutBackupStartsEmpty 主文件损坏且无 .bak → 空池启动（不 fatal、
// 不 panic），坏文件仍被隔离（避免下一次落盘静默覆盖证据）；恢复写盘后文件合法。
func TestCorruptStateWithoutBackupStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`not-json at all`), 0o600); err != nil {
		t.Fatal(err)
	}

	p := New(fp) // 不得 panic / fatal
	if n := len(p.List()); n != 0 {
		t.Fatalf("no backup → empty pool, got %d accounts", n)
	}
	if got := corruptFiles(t, fp); len(got) != 1 {
		t.Fatalf("corrupt file should be quarantined once, got %v", got)
	}
	// 空池仍可正常写盘（新一轮状态从零开始），且不与隔离文件冲突。
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 5, 0)
	p.Flush()
	raw, err := os.ReadFile(fp)
	if err != nil || !strings.Contains(string(raw), `"credits": 5`) {
		t.Fatalf("state.json should be writable after corrupt fallback: err=%v raw=%s", err, raw)
	}
}

// TestBackupUntouchedWhenMainWriteFails 主文件写失败（tmp 路径被目录占位）时：
// 主文件与 .bak 都保持上一版内容（「备份只在写成功后更新」），并记录落盘失败。
func TestBackupUntouchedWhenMainWriteFails(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	writeStateJSON(t, fp, stateWithCredit("u1", 1))
	writeStateJSON(t, fp+".bak", stateWithCredit("u0", 9))
	prevMain, _ := os.ReadFile(fp)
	prevBak, _ := os.ReadFile(fp + ".bak")

	// 占位 state.json.tmp 为目录 → os.WriteFile(tmp) 必失败（跨平台可靠）。
	if err := os.Mkdir(fp+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u2", 2, 0)
	p.Flush()

	if p.persistFails == 0 {
		t.Fatal("failed main write should be recorded in persistFails")
	}
	curMain, err := os.ReadFile(fp)
	if err != nil || string(curMain) != string(prevMain) {
		t.Fatalf("main file must stay previous version on failed write: err=%v\n%s", err, curMain)
	}
	curBak, err := os.ReadFile(fp + ".bak")
	if err != nil || string(curBak) != string(prevBak) {
		t.Fatalf(".bak must stay previous version on failed write: err=%v\n%s", err, curBak)
	}
}

// TestBackupRecoveryIgnoresCorruptBackup 主文件损坏 + .bak 也损坏 → 空池启动（不 panic），
// 两个坏文件都不会被当作有效状态。
func TestCorruptStateAndBackupStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fp+".bak", []byte(`garbage`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	if n := len(p.List()); n != 0 {
		t.Fatalf("both corrupt → empty pool, got %d accounts", n)
	}
}
