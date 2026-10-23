// selfcheck_test.go 钉住 v2.2 #13 启动自检的纯逻辑（依赖注入，不碰真实磁盘/权限）：
// auth_dir / state 目录可写性、磁盘水位阈值；输出必须含「问题 + 修复方向」提示。
package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 测试用相对路径（filepath.Join 构造，Windows/Linux 分隔符一致）。
var (
	testAuthDir   = "auths"
	testStateDir  = filepath.Join("data")
	testStateFile = filepath.Join("data", "state.json")
)

// fakeFileInfo 实现 os.FileInfo（自检只关心 IsDir/Name）。
type fakeFileInfo struct {
	name string
	dir  bool
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Sys() any           { return nil }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Mode() fs.FileMode {
	if f.dir {
		return fs.ModeDir | 0o755
	}
	return 0o600
}

// selfCheckFixture 构造自检依赖：existing 决定 Stat 是否成功，notDir 集合表示存在
// 但不是目录；mkdirErr/probeErr/free/freeErr 注入对应阶段的失败与磁盘剩余。
type selfCheckFixture struct {
	existing map[string]bool
	notDir   map[string]bool
	mkdirErr error
	probeErr error
	free     uint64
	freeErr  error
	probed   []string
	mkdirs   []string
}

func newSelfCheckFixture() *selfCheckFixture {
	return &selfCheckFixture{
		existing: map[string]bool{testAuthDir: true, testStateDir: true, ".": true},
		notDir:   map[string]bool{},
		free:     1 << 30, // 1 GiB：充足
	}
}

func (f *selfCheckFixture) deps() selfCheckDeps {
	return selfCheckDeps{
		Stat: func(path string) (os.FileInfo, error) {
			if f.notDir[path] {
				return fakeFileInfo{name: filepath.Base(path), dir: false}, nil
			}
			if !f.existing[path] {
				return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
			}
			return fakeFileInfo{name: filepath.Base(path), dir: true}, nil
		},
		MkdirAll: func(path string, perm os.FileMode) error {
			f.mkdirs = append(f.mkdirs, path)
			if f.mkdirErr != nil {
				return f.mkdirErr
			}
			f.existing[path] = true
			return nil
		},
		ProbeWritable: func(dir string) error {
			f.probed = append(f.probed, dir)
			return f.probeErr
		},
		FreeBytes: func(path string) (uint64, error) {
			return f.free, f.freeErr
		},
	}
}

func runSelfCheck(t *testing.T, f *selfCheckFixture) []string {
	t.Helper()
	return selfCheck(testAuthDir, testStateFile, f.deps())
}

// warningsContain 是否有一条 WARN 同时包含全部子串。
func warningsContain(warns []string, substrs ...string) bool {
	for _, w := range warns {
		ok := true
		for _, s := range substrs {
			if !strings.Contains(w, s) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// TestSelfCheckAllGood 一切正常 → 无 WARN。
func TestSelfCheckAllGood(t *testing.T) {
	f := newSelfCheckFixture()
	if warns := runSelfCheck(t, f); len(warns) != 0 {
		t.Fatalf("healthy env should produce no warnings, got %v", warns)
	}
	if len(f.probed) != 1 || f.probed[0] != testStateDir {
		t.Fatalf("state dir writability should be probed: %v", f.probed)
	}
}

// TestSelfCheckMissingAuthDir auth_dir 不存在 → WARN（含目录与 mkdir 提示），
// 但不因 auth 缺失而 WARN state 侧（state 正常）。
func TestSelfCheckMissingAuthDir(t *testing.T) {
	f := newSelfCheckFixture()
	delete(f.existing, testAuthDir)
	warns := runSelfCheck(t, f)
	if !warningsContain(warns, "auth_dir", "不存在") {
		t.Fatalf("want auth_dir missing warning, got %v", warns)
	}
	if len(warns) != 1 {
		t.Fatalf("only auth_dir should warn, got %v", warns)
	}
}

// TestSelfCheckAuthDirNotDir auth_dir 是普通文件 → WARN 说明扫描不到账号。
func TestSelfCheckAuthDirNotDir(t *testing.T) {
	f := newSelfCheckFixture()
	f.notDir[testAuthDir] = true
	warns := runSelfCheck(t, f)
	if !warningsContain(warns, "auth_dir", "不是目录") {
		t.Fatalf("want auth_dir-not-dir warning, got %v", warns)
	}
}

// TestSelfCheckMissingStateDirCreates state 目录不存在但可创建 → 不 WARN（正常首次运行路径）。
func TestSelfCheckMissingStateDirCreates(t *testing.T) {
	f := newSelfCheckFixture()
	delete(f.existing, testStateDir)
	warns := runSelfCheck(t, f)
	if len(warns) != 0 {
		t.Fatalf("creatable state dir should not warn, got %v", warns)
	}
	if len(f.mkdirs) != 1 || f.mkdirs[0] != testStateDir {
		t.Fatalf("state dir should be created: %v", f.mkdirs)
	}
}

// TestSelfCheckMissingStateDirMkdirFails 创建失败（只读挂载）→ WARN 含修复提示（chown/挂载）。
func TestSelfCheckMissingStateDirMkdirFails(t *testing.T) {
	f := newSelfCheckFixture()
	delete(f.existing, testStateDir)
	f.mkdirErr = errors.New("read-only file system")
	warns := runSelfCheck(t, f)
	if !warningsContain(warns, "state 目录", "无法创建") {
		t.Fatalf("want state-dir create warning, got %v", warns)
	}
	if !warningsContain(warns, "chown") && !warningsContain(warns, "挂载") {
		t.Fatalf("warning should include actionable hint (chown/mount), got %v", warns)
	}
}

// TestSelfCheckStateDirNotWritable 目录存在但写探测失败 → WARN（含 chown/挂载提示）。
func TestSelfCheckStateDirNotWritable(t *testing.T) {
	f := newSelfCheckFixture()
	f.probeErr = errors.New("permission denied")
	warns := runSelfCheck(t, f)
	if !warningsContain(warns, "不可写") {
		t.Fatalf("want not-writable warning, got %v", warns)
	}
	if !warningsContain(warns, "chown") && !warningsContain(warns, "挂载") {
		t.Fatalf("warning should include actionable hint, got %v", warns)
	}
}

// TestSelfCheckStatePathIsFile state 目录路径是普通文件 → WARN 说明无法落盘。
func TestSelfCheckStatePathIsFile(t *testing.T) {
	f := newSelfCheckFixture()
	f.notDir[testStateDir] = true
	warns := runSelfCheck(t, f)
	if !warningsContain(warns, "不是目录") {
		t.Fatalf("want state-path-not-dir warning, got %v", warns)
	}
}

// TestSelfCheckLowDisk 剩余空间低于 100 MiB → WARN（含阈值与路径）。
func TestSelfCheckLowDisk(t *testing.T) {
	f := newSelfCheckFixture()
	f.free = 50 << 20 // 50 MiB
	warns := runSelfCheck(t, f)
	if !warningsContain(warns, "磁盘", "100 MiB") {
		t.Fatalf("want low-disk warning, got %v", warns)
	}
}

// TestSelfCheckDiskProbeError 磁盘查询失败 → WARN 但不误报低水位。
func TestSelfCheckDiskProbeError(t *testing.T) {
	f := newSelfCheckFixture()
	f.freeErr = errors.New("unsupported")
	warns := runSelfCheck(t, f)
	if !warningsContain(warns, "剩余空间") {
		t.Fatalf("want disk-probe warning, got %v", warns)
	}
	if warningsContain(warns, "低于") {
		t.Fatalf("probe error must not be reported as low disk: %v", warns)
	}
}

// TestStartupSelfCheckRealFS 真实文件系统（临时目录）下不产生误报。
func TestStartupSelfCheckRealFS(t *testing.T) {
	authDir := t.TempDir()
	stateFile := filepath.Join(t.TempDir(), "state.json")
	warns := startupSelfCheck(authDir, stateFile)
	for _, w := range warns {
		if strings.Contains(w, "auth_dir") || strings.Contains(w, "不可写") || strings.Contains(w, "无法创建") {
			t.Fatalf("real temp dirs should pass self-check, got warning: %s", w)
		}
	}
}

// TestExistingAncestor 目录不存在时向上找已存在祖先（磁盘水位探测路径）。
func TestExistingAncestor(t *testing.T) {
	existsDir := filepath.Join("exists")
	stat := func(path string) (os.FileInfo, error) {
		if path == existsDir {
			return fakeFileInfo{name: "exists", dir: true}, nil
		}
		return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
	}
	deep := filepath.Join(existsDir, "a", "b", "c")
	if got := existingAncestor(deep, stat); got != existsDir {
		t.Fatalf("existingAncestor=%q want %q", got, existsDir)
	}
	// 全部不存在 → 回落当前目录 "."
	statNone := func(string) (os.FileInfo, error) {
		return nil, &fs.PathError{Op: "stat", Path: "x", Err: fs.ErrNotExist}
	}
	if got := existingAncestor(filepath.Join("a", "b"), statNone); got != "." {
		t.Fatalf("existingAncestor fallback=%q want .", got)
	}
}
