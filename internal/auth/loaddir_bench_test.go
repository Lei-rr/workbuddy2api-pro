package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// writeBenchAuthDir 在 dir 下生成 n 个合成 auth 文件（扁平形，带 realm 键：
// 避免 LoadDir 的空 realm 回填写盘干扰「纯解析路径」的基准对比）。
func writeBenchAuthDir(tb testing.TB, dir string, n int) {
	tb.Helper()
	for i := 0; i < n; i++ {
		realm, domain := "cn", "copilot.tencent.com"
		if i%5 == 0 {
			realm, domain = "global", "www.workbuddy.ai"
		}
		doc := fmt.Sprintf(
			`{"accessToken":"at-%06d","refreshToken":"rt-%06d","expiresAt":1,"domain":"%s","realm":"%s","uid":"u%06d","nickname":"bench-%06d"}`,
			i, i, domain, realm, i, i)
		name := fmt.Sprintf("workbuddy-u%06d.json", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(doc), 0o600); err != nil {
			tb.Fatalf("写入合成 auth %d: %v", i, err)
		}
	}
}

// withLoadDirWorkers 临时覆盖并行度（测试用，cleanup 恢复）。
func withLoadDirWorkers(t *testing.T, n int) {
	t.Helper()
	old := loadDirWorkers
	loadDirWorkers = n
	t.Cleanup(func() { loadDirWorkers = old })
}

// TestLoadDirParallelMatchesSerial 并行加载与串行结果完全一致：
// 数量、uid 顺序（稳定排序）、关键字段、损坏文件跳过语义。
func TestLoadDirParallelMatchesSerial(t *testing.T) {
	dir := t.TempDir()
	writeBenchAuthDir(t, dir, 200)
	// 损坏文件：并行/串行都必须静默跳过（不阻断启动）。
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-bad.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	withLoadDirWorkers(t, 1)
	serial, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("serial LoadDir: %v", err)
	}
	if len(serial) != 200 {
		t.Fatalf("serial 加载 %d 个，want 200（损坏文件应跳过）", len(serial))
	}

	withLoadDirWorkers(t, 16)
	parallel, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("parallel LoadDir: %v", err)
	}
	if len(parallel) != len(serial) {
		t.Fatalf("并行 %d 个 vs 串行 %d 个", len(parallel), len(serial))
	}
	for i := range serial {
		if parallel[i].UID != serial[i].UID || parallel[i].AccessToken != serial[i].AccessToken ||
			parallel[i].Realm() != serial[i].Realm() {
			t.Fatalf("第 %d 项不一致: parallel=%s/%s serial=%s/%s",
				i, parallel[i].UID, parallel[i].AccessToken, serial[i].UID, serial[i].AccessToken)
		}
	}
	// 确定性：同样输入再跑一次，顺序与内容一致。
	again, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range again {
		if again[i].UID != parallel[i].UID {
			t.Fatalf("并行结果不确定：第 %d 项 %s vs %s", i, again[i].UID, parallel[i].UID)
		}
	}
}

// TestLoadDirEmpty 空目录/nil 语义：返回空、不报错（并行化不改原行为）。
func TestLoadDirEmpty(t *testing.T) {
	dir := t.TempDir()
	list, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("空目录: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("空目录加载 %d 个，want 0", len(list))
	}
}

// TestRealmMemoCachedReuse Realm 记忆化：重复调用返回一致；回归既有语义。
func TestRealmMemoCachedReuse(t *testing.T) {
	defer SetGlobalEnabled(true) // 防止同包其他测试遗留逃生门状态
	cases := []struct {
		name   string
		domain string
		realm  string
		want   string
	}{
		{"empty", "", "", "cn"},
		{"cn domain", "copilot.tencent.com", "", "cn"},
		{"global domain", "www.workbuddy.ai", "", "global"},
		{"explicit global", "", "global", "global"},
	}
	for _, tc := range cases {
		a := &Auth{Domain: tc.domain, realm: tc.realm}
		for i := 0; i < 3; i++ {
			if got := a.Realm(); got != tc.want {
				t.Fatalf("%s: Realm()=%q want %q", tc.name, got, tc.want)
			}
		}
	}
	// nil 接收者安全返回 cn（不在既有热路径崩溃）。
	var nilAuth *Auth
	if got := nilAuth.Realm(); got != "cn" {
		t.Fatalf("nil Auth.Realm()=%q want cn", got)
	}
}

// TestRealmMemoInvalidation 缓存失效语义：
//   - global 开关切换后结果立即跟随（逃生门）；
//   - BackfillRealm / BackfillRealmFor / InvalidateRealmCache / SaveAtomic 后重算。
func TestRealmMemoInvalidation(t *testing.T) {
	defer SetGlobalEnabled(true)

	a := &Auth{Domain: "www.workbuddy.ai"}
	if got := a.Realm(); got != "global" {
		t.Fatalf("global domain Realm=%q", got)
	}
	SetGlobalEnabled(false) // 逃生门：锁死纯 CN
	if got := a.Realm(); got != "cn" {
		t.Fatalf("逃生门关闭后 Realm=%q want cn（缓存必须失效）", got)
	}
	SetGlobalEnabled(true)
	if got := a.Realm(); got != "global" {
		t.Fatalf("逃生门恢复后 Realm=%q want global", got)
	}

	// BackfillRealmFor：显式写入 realm 后必须立刻生效（不依赖 domain）。
	b := &Auth{}
	if got := b.Realm(); got != "cn" {
		t.Fatalf("b cold Realm=%q", got)
	}
	if changed, err := BackfillRealmFor(b, "global"); err != nil || !changed {
		t.Fatalf("BackfillRealmFor: changed=%v err=%v", changed, err)
	}
	if got := b.Realm(); got != "global" {
		t.Fatalf("BackfillRealmFor 后 Realm=%q want global（缓存必须失效）", got)
	}

	// 外部直接改 Domain（upstream 刷新路径形态）+ InvalidateRealmCache。
	c := &Auth{Domain: "copilot.tencent.com"}
	if got := c.Realm(); got != "cn" {
		t.Fatalf("c cold Realm=%q", got)
	}
	c.Lock()
	c.Domain = "www.workbuddy.ai"
	c.Unlock()
	c.InvalidateRealmCache()
	if got := c.Realm(); got != "global" {
		t.Fatalf("InvalidateRealmCache 后 Realm=%q want global", got)
	}

	// SaveAtomic 兜底失效：外部在锁内改 Domain 后调用 SaveAtomic（真实刷新路径）。
	d := &Auth{Domain: "copilot.tencent.com", AccessToken: "at", UID: "u-d"}
	d.FilePath = filepath.Join(t.TempDir(), "workbuddy-u-d.json")
	if got := d.Realm(); got != "cn" {
		t.Fatalf("d cold Realm=%q", got)
	}
	d.Lock()
	d.Domain = "www.workbuddy.ai"
	d.Unlock()
	if err := d.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	if got := d.Realm(); got != "global" {
		t.Fatalf("SaveAtomic 后 Realm=%q want global（缓存必须失效）", got)
	}

	// BackfillRealm（LoadDir 存量迁移路径）同样失效。
	e := &Auth{Domain: "www.workbuddy.ai"}
	if got := e.Realm(); got != "global" {
		t.Fatalf("e cold Realm=%q", got)
	}
	if changed, r := e.BackfillRealm(); !changed || r != "global" {
		t.Fatalf("BackfillRealm: changed=%v realm=%q", changed, r)
	}
	SetGlobalEnabled(false)
	if got := e.Realm(); got != "cn" {
		t.Fatalf("backfill 后开关关闭 Realm=%q want cn", got)
	}
}

// TestRealmMemoConcurrent 并发 Realm 读 + 失效/写入：无 panic、最终值正确。
// 本地无 cgo 时无法 -race，此处用多轮并发做冒烟；CI Linux 可加 -race。
func TestRealmMemoConcurrent(t *testing.T) {
	defer SetGlobalEnabled(true)
	a := &Auth{Domain: "copilot.tencent.com"}
	a.Realm() // 预热缓存

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				switch w % 4 {
				case 0:
					_ = a.Realm()
				case 1:
					a.InvalidateRealmCache()
				case 2:
					SetGlobalEnabled(i%2 == 0)
				case 3:
					_ = a.IsGlobal()
				}
			}
		}(w)
	}
	wg.Wait()
	SetGlobalEnabled(true)
	a.InvalidateRealmCache()
	// domain 仍是 cn 域；显式写 global 后必须读回 global。
	if changed, err := BackfillRealmFor(a, "global"); err != nil || !changed {
		t.Fatalf("BackfillRealmFor: changed=%v err=%v", changed, err)
	}
	if got := a.Realm(); got != "global" {
		t.Fatalf("并发后 Realm=%q want global", got)
	}
}

// BenchmarkAuthRealmCached Realm() 缓存命中（热路径：10k 账号 overview/List 高频调用）。
func BenchmarkAuthRealmCached(b *testing.B) {
	a := &Auth{Domain: "www.workbuddy.ai"}
	if got := a.Realm(); got != "global" {
		b.Fatalf("warm Realm=%q", got)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if a.Realm() != "global" {
			b.Fatal("unexpected realm")
		}
	}
}

// BenchmarkAuthRealmCold Realm() 未命中（重算+缓存写入）成本，对照旧实现。
func BenchmarkAuthRealmCold(b *testing.B) {
	a := &Auth{Domain: "www.workbuddy.ai"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.InvalidateRealmCache()
		if a.Realm() != "global" {
			b.Fatal("unexpected realm")
		}
	}
}

// benchLoadDir 串行/并行 LoadDir 基准（生成 10k 临时 auth 文件后只计 LoadDir 本身）。
//
// 可选环境变量 AUTH_BENCH_DIR：把 10k 临时文件放到指定目录（本机 C: 临时目录受
// Defender 实时扫描影响时可切到数据盘测得更接近生产；默认 t.TempDir()）。
func benchLoadDir(b *testing.B, accounts, workers int) {
	var dir string
	if base := os.Getenv("AUTH_BENCH_DIR"); base != "" {
		if err := os.MkdirAll(base, 0o755); err != nil {
			b.Fatalf("创建 AUTH_BENCH_DIR: %v", err)
		}
		d, err := os.MkdirTemp(base, "authbench-")
		if err != nil {
			b.Fatalf("创建基准目录: %v", err)
		}
		dir = d
		b.Cleanup(func() { _ = os.RemoveAll(d) })
	} else {
		dir = b.TempDir()
	}
	writeBenchAuthDir(b, dir, accounts)
	// 诊断用：AUTH_BENCH_WORKERS 可覆盖并行度（默认由调用方给定）。
	if v := os.Getenv("AUTH_BENCH_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			workers = n
		}
	}
	old := loadDirWorkers
	loadDirWorkers = workers
	defer func() { loadDirWorkers = old }()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		list, err := LoadDir(dir)
		if err != nil {
			b.Fatalf("LoadDir: %v", err)
		}
		if len(list) != accounts {
			b.Fatalf("加载 %d 个，want %d", len(list), accounts)
		}
	}
}

// BenchmarkLoadDir10kSerial 10k 账号串行解析（优化前基线形态）。
func BenchmarkLoadDir10kSerial(b *testing.B) { benchLoadDir(b, 10000, 1) }

// BenchmarkLoadDir10kParallel 10k 账号并行解析（默认 worker=min(16, NumCPU*2)）。
func BenchmarkLoadDir10kParallel(b *testing.B) { benchLoadDir(b, 10000, defaultLoadDirWorkers()) }

// BenchmarkParseAuth10k 纯解析成本（无文件 I/O）：10k 个合成 plain auth JSON。
func BenchmarkParseAuth10k(b *testing.B) {
	const n = 10000
	raws := make([][]byte, n)
	for i := 0; i < n; i++ {
		realm, domain := "cn", "copilot.tencent.com"
		if i%5 == 0 {
			realm, domain = "global", "www.workbuddy.ai"
		}
		raws[i] = []byte(fmt.Sprintf(
			`{"accessToken":"at-%06d","refreshToken":"rt-%06d","expiresAt":1,"domain":"%s","realm":"%s","uid":"u%06d","nickname":"bench-%06d"}`,
			i, i, domain, realm, i, i))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iter := 0; iter < b.N; iter++ {
		for _, raw := range raws {
			if _, err := Parse(raw); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkReadAuthFiles10k 纯文件读取成本（无 JSON 解析）：10k 个 auth 文件。
func BenchmarkReadAuthFiles10k(b *testing.B) {
	const n = 10000
	var dir string
	if base := os.Getenv("AUTH_BENCH_DIR"); base != "" {
		_ = os.MkdirAll(base, 0o755)
		d, err := os.MkdirTemp(base, "authread-")
		if err != nil {
			b.Fatal(err)
		}
		dir = d
		b.Cleanup(func() { _ = os.RemoveAll(d) })
	} else {
		dir = b.TempDir()
	}
	writeBenchAuthDir(b, dir, n)
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil || len(files) != n {
		b.Fatalf("glob: %v n=%d", err, len(files))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iter := 0; iter < b.N; iter++ {
		for _, f := range files {
			if _, err := os.ReadFile(f); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// TestParseAuthKeyNullEdge "auth" 键存在但为 null 时保持旧实现判定（走嵌套形 →
// 缺 accessToken 报错），不被更好的 JSON 结构误判成扁平形。无 auth 键才走扁平形。
func TestParseAuthKeyNullEdge(t *testing.T) {
	if _, err := Parse([]byte(`{"auth":null,"accessToken":"flat","uid":"u1"}`)); err == nil {
		t.Fatal(`"auth":null 应保持嵌套形判定并按缺 accessToken 报错`)
	}
	if _, err := Parse([]byte(`{"auth":"bad-type","accessToken":"flat"}`)); err == nil {
		t.Fatal("auth 非对象应报 storage_parse_error")
	}
	a, err := Parse([]byte(`{"accessToken":"flat","refreshToken":"rt","uid":"u1"}`))
	if err != nil || a.UID != "u1" || a.AccessToken != "flat" {
		t.Fatalf("扁平形 Parse: %+v err=%v", a, err)
	}
	// 嵌套形 + 顶层 device_token 仍生效。
	n, err := Parse([]byte(`{"auth":{"accessToken":"at","realm":"global"},"account":{"uid":"u2"},"device_token":"dt"}`))
	if err != nil || n.UID != "u2" || n.RealmStored() != "global" || n.DeviceToken != "dt" {
		t.Fatalf("嵌套形 Parse: %+v err=%v", n, err)
	}
}
